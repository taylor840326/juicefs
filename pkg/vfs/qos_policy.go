/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package vfs

import (
	"context"
	"errors"
	"sync"
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/qos"
)

// scopeCacheSize caps the inode -> scope cache. Rename and policy changes bump
// the epoch and clear the cache, so entries only live while the tree is stable.
const scopeCacheSize = 1 << 16

type scopeEntry struct {
	epoch uint64
	scope *qos.Scope
}

type scopeCache struct {
	mu      sync.Mutex
	entries map[meta.Ino]scopeEntry
}

func (c *scopeCache) get(ino meta.Ino, epoch uint64) *qos.Scope {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[ino]; ok && e.epoch == epoch {
		return e.scope
	}
	return nil
}

func (c *scopeCache) put(ino meta.Ino, epoch uint64, s *qos.Scope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= scopeCacheSize {
		c.entries = make(map[meta.Ino]scopeEntry, 64)
	}
	c.entries[ino] = scopeEntry{epoch: epoch, scope: s}
}

func (c *scopeCache) reset() {
	c.mu.Lock()
	c.entries = make(map[meta.Ino]scopeEntry, 64)
	c.mu.Unlock()
}

// SetQoSPolicy installs a multi-tenant policy snapshot. It replaces the
// previous snapshot and invalidates every cached scope, so the next operation
// resolves the new tree. Passing nil disables policy based QoS.
func (v *VFS) SetQoSPolicy(p *qos.Policy) {
	v.qosPolicy.Store(p)
	v.qosEpoch.Add(1)
	v.qosScopeCache.reset()
	if v.qosMgr != nil {
		v.qosMgr.Reset()
	}
	if p != nil {
		logger.Infof("QoS policy loaded: tenant=%s version=%d epoch=%d rules=%d",
			p.Tenant, p.Version, p.Epoch, len(p.Rules))
	}
}

// SetQoSManager installs the limiter used for policy based QoS. Without a
// manager a policy snapshot is still visible (GetQoSPolicy) but not enforced.
func (v *VFS) SetQoSManager(m *qos.Manager) {
	v.qosMgr = m
}

// GetQoSPolicy returns the current policy snapshot, or nil.
func (v *VFS) GetQoSPolicy() *qos.Policy {
	return v.qosPolicy.Load()
}

func (v *VFS) invalidateQoSScopes() {
	v.qosEpoch.Add(1)
	v.qosScopeCache.reset()
}

// qosScope resolves the rule chain of ino: every ancestor with a rule, from
// the inode itself up to the tenant cap. Rules of descendants never apply to
// their ancestors, and a missing rule means "inherit the next one up".
func (v *VFS) qosScope(ctx Context, ino meta.Ino) *qos.Scope {
	p := v.qosPolicy.Load()
	if p == nil || v.qosMgr == nil {
		return nil
	}
	if len(p.Rules) == 0 && !p.Cap.Enabled() {
		return nil
	}
	epoch := v.qosEpoch.Load()
	if s := v.qosScopeCache.get(ino, epoch); s != nil {
		return s
	}

	chain := make([]qos.Level, 0, 4)
	node := ino
	for depth := 0; depth < 4096; depth++ {
		if r := p.Rule(uint64(node)); r != nil {
			chain = append(chain, qos.Level{Ino: uint64(node), Limits: r.Limits})
		}
		if node == meta.RootInode {
			break
		}
		parents := v.Meta.GetParents(ctx, node)
		if len(parents) == 0 {
			break
		}
		// A hardlinked directory is not possible; for a file moved while
		// being opened the parent can be ambiguous, pick the smallest ino
		// so all clients resolve the same chain.
		var next meta.Ino = 0
		for p := range parents {
			if next == 0 || p < next {
				next = p
			}
		}
		if next == 0 {
			break
		}
		node = next
	}
	chain = append(chain, qos.Level{Ino: 0, Limits: p.Cap})
	scope := &qos.Scope{Ino: uint64(ino), Chain: chain}
	v.qosScopeCache.put(ino, epoch, scope)
	return scope
}

// qosWaitMeta charges one metadata operation against the rule of ino. It also
// honors the legacy client wide limits (--qos-*), so both configurations can
// coexist while users migrate to a policy file.
// scopeForHandle returns the scope bound to an open handle. A handle stores the
// epoch it was resolved at; when a policy reload bumps the epoch, the next I/O
// of that handle resolves its rule chain again instead of using stale rules.
func (v *VFS) scopeForHandle(ctx Context, h *handle) *qos.Scope {
	if h == nil || v.qosMgr == nil {
		return nil
	}
	epoch := v.qosEpoch.Load()
	if h.scopeEpoch.Load() == epoch {
		if s := h.scope.Load(); s != nil {
			return s
		}
	}
	s := v.qosScope(ctx, h.inode)
	h.scope.Store(s)
	h.scopeEpoch.Store(epoch)
	return s
}

func errnoOf(err error) syscall.Errno {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return syscall.EINTR
	}
	return syscall.EIO
}

// qosCharge charges op under the newest policy. If a reload happens while the
// charge is blocked, the manager aborts it with ErrStale; the scope is resolved
// again and the operation retries under the new rules.
func (v *VFS) qosCharge(ctx Context, op qos.Op, resolve func() (*qos.Scope, uint64)) syscall.Errno {
	if v.qosMgr == nil {
		return 0
	}
	for i := 0; i < 1000; i++ {
		s, epoch := resolve()
		if s == nil {
			return 0
		}
		err := v.qosMgr.ChargeWatch(ctx, s, op, func() bool { return v.qosEpoch.Load() != epoch })
		if err == qos.ErrStale {
			continue
		}
		if err != nil {
			return errnoOf(err)
		}
		return 0
	}
	return syscall.EINTR
}

func (v *VFS) qosWaitMeta(ctx Context, ino meta.Ino, class string) syscall.Errno {
	if v.qosMgr != nil {
		if err := v.qosCharge(ctx, qos.Op{Meta: class}, func() (*qos.Scope, uint64) {
			epoch := v.qosEpoch.Load()
			return v.qosScope(ctx, ino), epoch
		}); err != 0 {
			return err
		}
	}
	return v.waitMeta(ctx)
}

func (v *VFS) qosWaitData(ctx Context, h *handle, read bool, n int64) syscall.Errno {
	if n <= 0 {
		return 0
	}
	if err := v.qosCharge(ctx, qos.Op{Read: read, Bytes: n}, func() (*qos.Scope, uint64) {
		epoch := v.qosEpoch.Load()
		return v.scopeForHandle(ctx, h), epoch
	}); err != 0 {
		return err
	}
	return v.waitData(ctx, read, n)
}

func (v *VFS) qosWaitOp(ctx Context, ino meta.Ino) syscall.Errno {
	if v.qosMgr != nil {
		if err := v.qosCharge(ctx, qos.Op{}, func() (*qos.Scope, uint64) {
			epoch := v.qosEpoch.Load()
			return v.qosScope(ctx, ino), epoch
		}); err != 0 {
			return err
		}
	}
	return v.waitOp(ctx)
}

func (v *VFS) qosWaitCopy(ctx Context, h *handle, n int64) syscall.Errno {
	if n > 0 {
		if err := v.qosCharge(ctx, qos.Op{Bytes: n}, func() (*qos.Scope, uint64) {
			epoch := v.qosEpoch.Load()
			return v.scopeForHandle(ctx, h), epoch
		}); err != 0 {
			return err
		}
	}
	return v.waitOp(ctx)
}

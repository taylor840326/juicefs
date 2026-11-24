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

package qos

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/juicedata/juicefs/pkg/utils"
)

var logger = utils.GetLogger("juicefs")

// Level is one node of a scope chain: the directory inode that owns a rule
// and the limits of that rule. The tenant cap is the last level with inode 0.
type Level struct {
	Ino    uint64
	Limits Limits
}

// Scope is the resolved rule chain of one inode, ordered from the nearest
// rule to the tenant cap.
type Scope struct {
	Ino   uint64
	Chain []Level
}

// Manager enforces the QoS limits of one mount. It either runs the buckets
// locally (single mount, useful for tests and standalone deployments) or
// leases tokens from a shared counter service, which is what makes several
// mounts of the same volume share one budget.
type Manager struct {
	tenant   string
	lease    LeaseClient
	failOpen bool

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// NewManager creates a manager. A nil lease means the buckets are local to
// this process; failOpen controls whether a failing counter service lets
// operations through (true) or fails them (false).
func NewManager(tenant string, lease LeaseClient, failOpen bool) *Manager {
	return &Manager{
		tenant:   tenant,
		lease:    lease,
		failOpen: failOpen,
		buckets:  make(map[string]*tokenBucket),
	}
}

// Reset drops the cached buckets. It must be called when the policy snapshot
// changes, because chain levels and rates may be different.
func (m *Manager) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.buckets = make(map[string]*tokenBucket)
	m.mu.Unlock()
}

// burstFor returns the tokenBucket capacity for a rate. A tenth of the rate keeps
// shaping smooth; one second of tokens would make short bursts much larger
// than the configured rate.
func burstFor(rate uint64) uint64 {
	b := rate / 10
	if b < 1 {
		b = 1
	}
	if b > rate {
		b = rate
	}
	return b
}

func (m *Manager) tokenBucket(key string, rate, burst uint64) *tokenBucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[key]
	if b == nil || b.rate != float64(rate) || b.burst != float64(burst) {
		b = newTokenBucket(rate, burst, rate > 0)
		m.buckets[key] = b
	}
	return b
}

func (m *Manager) warnf(format string, args ...interface{}) {
	logger.Warnf(format, args...)
}

// canceled reports whether ctx was canceled. The FUSE context of JuiceFS
// reports EINTR from Err() by design and exposes cancellation through
// Canceled() instead, so Err() alone must not be used here.
func canceled(ctx context.Context) bool {
	if c, ok := ctx.(interface{ Canceled() bool }); ok {
		return c.Canceled()
	}
	return ctx.Err() != nil
}

// Charge blocks until op is allowed by the policy and consumes its tokens.
// A nil scope or an empty chain means "no policy", i.e. unlimited.
func (m *Manager) Charge(ctx context.Context, s *Scope, op Op) error {
	if m == nil || s == nil || len(s.Chain) == 0 {
		return nil
	}
	if m.lease != nil {
		return m.chargeLeased(ctx, s, op)
	}
	return m.chargeLocal(ctx, s, op)
}

// chargeLocal charges every level of the chain independently. Sibling
// directories therefore share their parent level's tokenBucket, which gives
// hierarchical enforcement without a counter service.
func (m *Manager) chargeLocal(ctx context.Context, s *Scope, op Op) error {
	for _, lv := range s.Chain {
		for _, dr := range lv.Limits.dimensionReqs(op) {
			rate := lv.Limits.Rate(dr.Dim)
			if rate == 0 {
				continue
			}
			if err := m.waitLocal(ctx, keyOf(lv.Ino, dr.Dim), rate, dr.N); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) waitLocal(ctx context.Context, key string, rate uint64, n int64) error {
	burst := burstFor(rate)
	b := m.tokenBucket(key, rate, burst)
	for n > 0 {
		if canceled(ctx) {
			return context.Canceled
		}
		chunk := n
		if chunk > int64(burst) {
			chunk = int64(burst)
		}
		if got := b.take(chunk); got > 0 {
			n -= got
			continue
		}
		wait := b.waitFor(chunk)
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		if wait <= 0 {
			wait = time.Millisecond
		}
		// A plain sleep keeps this safe for contexts that do not implement
		// Done() (the FUSE context does not); cancellation is checked at the
		// top of the loop, at most one wait slice later.
		time.Sleep(wait)
	}
	return nil
}

// chargeLeased charges the nearest level that defines each dimension. The
// grant request carries the whole chain, so the counter deducts every
// ancestor tokenBucket at once; charging ancestors again here would double count.
func (m *Manager) chargeLeased(ctx context.Context, s *Scope, op Op) error {
	handled := make(map[string]bool, 4)
	for i, lv := range s.Chain {
		for _, dr := range lv.Limits.dimensionReqs(op) {
			if handled[dr.Dim] {
				continue
			}
			handled[dr.Dim] = true
			if err := m.waitLeased(ctx, s.Chain, i, dr.Dim, dr.N); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) waitLeased(ctx context.Context, chain []Level, idx int, dim string, n int64) error {
	rate := chain[idx].Limits.Rate(dim)
	if rate == 0 {
		return nil
	}
	burst := burstFor(rate)
	key := keyOf(chain[idx].Ino, dim)
	b := m.tokenBucket(key, 0, burst) // leased buckets never refill on their own
	for n > 0 {
		if canceled(ctx) {
			return context.Canceled
		}
		chunk := n
		if chunk > int64(burst) {
			chunk = int64(burst)
		}
		if got := b.take(chunk); got > 0 {
			n -= got
			continue
		}
		levels := make([]LeaseLevel, 0, len(chain)-idx)
		for j := idx; j < len(chain); j++ {
			if r := chain[j].Limits.Rate(dim); r > 0 {
				levels = append(levels, LeaseLevel{Ino: chain[j].Ino, Rate: r})
			}
		}
		if len(levels) == 0 {
			return nil
		}
		granted, err := m.lease.Grant(ctx, m.tenant, levels, dim, int64(burst))
		if err != nil {
			if m.failOpen {
				m.warnf("QoS lease service unavailable (%s), letting %s through: %d", dim, m.tenant, n)
				return nil
			}
			return err
		}
		if granted > 0 {
			b.add(granted)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func keyOf(ino uint64, dim string) string {
	return fmt.Sprintf("%d|%s", ino, dim)
}

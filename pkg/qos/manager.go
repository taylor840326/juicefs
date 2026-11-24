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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/juicedata/juicefs/pkg/utils"
)

var logger = utils.GetLogger("juicefs")

// ErrStale is returned by ChargeWatch when the policy changed while the caller
// was blocked. The caller is expected to resolve its scope again and retry, so
// that a rule update takes effect for operations that are already waiting
// instead of letting them drain the old buckets.
var ErrStale = errors.New("qos policy changed while waiting")

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

	// flights coalesces concurrent lease requests for the same
	// (inode, dimension) pair: one mount talking to the counter service
	// must not turn every blocked operation into an HTTP round trip.
	flights sync.Map // key -> *leaseFlight
}

// leaseFlight is one in-flight grant request shared by all goroutines that
// are waiting for tokens of the same bucket.
type leaseFlight struct {
	done    chan struct{}
	granted int64
	err     error
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

// leaseQuantum is the amount of tokens one grant request asks for and the
// capacity of both the local lease bucket and the counter service bucket.
// Half a second of the configured rate keeps the request rate near 2/s while
// still bounding how much a single mount can hold back from its siblings.
func leaseQuantum(rate uint64, dim string) uint64 {
	q := rate / 2
	if q < 1 {
		q = 1
	}
	switch dim {
	case DimBandwidth, DimReadBandwidth, DimWriteBandwidth:
		if q > 16<<20 {
			q = 16 << 20
		}
	default:
		if q > 2000 {
			q = 2000
		}
	}
	if q > rate {
		q = rate
	}
	return q
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
	return m.ChargeWatch(ctx, s, op, nil)
}

// ChargeWatch is Charge with a staleness probe. stale is called at every wait
// iteration; when it reports true, ChargeWatch gives up with ErrStale so the
// caller can re-resolve the scope and retry under the new policy.
func (m *Manager) ChargeWatch(ctx context.Context, s *Scope, op Op, stale func() bool) error {
	if m == nil || s == nil || len(s.Chain) == 0 {
		return nil
	}
	if m.lease != nil {
		return m.chargeLeased(ctx, s, op, stale)
	}
	return m.chargeLocal(ctx, s, op, stale)
}

// chargeLocal charges every level of the chain independently. Sibling
// directories therefore share their parent level's tokenBucket, which gives
// hierarchical enforcement without a counter service.
func (m *Manager) chargeLocal(ctx context.Context, s *Scope, op Op, stale func() bool) error {
	for _, lv := range s.Chain {
		for _, dr := range lv.Limits.dimensionReqs(op) {
			rate := lv.Limits.Rate(dr.Dim)
			if rate == 0 {
				continue
			}
			if err := m.waitLocal(ctx, keyOf(lv.Ino, dr.Dim), rate, dr.N, stale); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) waitLocal(ctx context.Context, key string, rate uint64, n int64, stale func() bool) error {
	burst := burstFor(rate)
	b := m.tokenBucket(key, rate, burst)
	for n > 0 {
		if canceled(ctx) {
			return context.Canceled
		}
		if stale != nil && stale() {
			return ErrStale
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
func (m *Manager) chargeLeased(ctx context.Context, s *Scope, op Op, stale func() bool) error {
	handled := make(map[string]bool, 4)
	for i, lv := range s.Chain {
		for _, dr := range lv.Limits.dimensionReqs(op) {
			if handled[dr.Dim] {
				continue
			}
			handled[dr.Dim] = true
			if err := m.waitLeased(ctx, s.Chain, i, dr.Dim, dr.N, stale); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) waitLeased(ctx context.Context, chain []Level, idx int, dim string, n int64, stale func() bool) error {
	rate := chain[idx].Limits.Rate(dim)
	if rate == 0 {
		return nil
	}
	quantum := leaseQuantum(rate, dim)
	key := keyOf(chain[idx].Ino, dim)
	b := m.tokenBucket(key, 0, quantum) // leased buckets never refill on their own
	levels := make([]LeaseLevel, 0, len(chain)-idx)
	for j := idx; j < len(chain); j++ {
		if r := chain[j].Limits.Rate(dim); r > 0 {
			levels = append(levels, LeaseLevel{Ino: chain[j].Ino, Rate: r})
		}
	}
	if len(levels) == 0 {
		return nil
	}
	backoff := 50 * time.Millisecond
	for n > 0 {
		if canceled(ctx) {
			return context.Canceled
		}
		if stale != nil && stale() {
			return ErrStale
		}
		chunk := n
		if chunk > int64(quantum) {
			chunk = int64(quantum)
		}
		if got := b.take(chunk); got > 0 {
			n -= got
			backoff = 50 * time.Millisecond
			continue
		}
		want := int64(quantum)
		granted, err := m.grantLease(ctx, key, b, func(ctx context.Context) (int64, error) {
			return m.lease.Grant(ctx, m.tenant, levels, dim, want)
		})
		if err != nil {
			if m.failOpen {
				m.warnf("QoS lease service unavailable (%s), letting %s through: %d", dim, m.tenant, n)
				return nil
			}
			return err
		}
		if granted > 0 {
			// One request delivers at most one quantum; if the counter could
			// only hand out part of it, pace the next request by the missing
			// amount instead of hammering the counter service.
			if granted < int64(quantum) {
				wait := time.Duration(float64(quantum-uint64(granted)) / float64(rate) * float64(time.Second))
				if wait < 2*time.Millisecond {
					wait = 2 * time.Millisecond
				}
				if wait > 200*time.Millisecond {
					wait = 200 * time.Millisecond
				}
				time.Sleep(wait)
			}
			continue
		}
		time.Sleep(backoff)
		if backoff < 250*time.Millisecond {
			backoff *= 2
		}
	}
	return nil
}

// grantLease performs one grant request for key, sharing the result with all
// goroutines that ask for the same bucket while it is in flight.
func (m *Manager) grantLease(ctx context.Context, key string, b *tokenBucket,
	fn func(context.Context) (int64, error)) (int64, error) {
	for {
		v, loaded := m.flights.LoadOrStore(key, &leaseFlight{done: make(chan struct{})})
		f := v.(*leaseFlight)
		if !loaded {
			f.granted, f.err = fn(ctx)
			if f.err == nil && f.granted > 0 {
				b.add(f.granted)
			}
			m.flights.Delete(key)
			close(f.done)
			return f.granted, f.err
		}
		select {
		case <-f.done:
			return f.granted, f.err
		case <-time.After(25 * time.Millisecond):
			if canceled(ctx) {
				return 0, context.Canceled
			}
		}
	}
}

func keyOf(ino uint64, dim string) string {
	return fmt.Sprintf("%d|%s", ino, dim)
}

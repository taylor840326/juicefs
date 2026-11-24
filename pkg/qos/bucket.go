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
	"math"
	"sync"
	"time"
)

// tokenBucket is a token bucket that can be shared between the local limiter and
// the reference counter server. A rate of zero disables refill, which is how
// leased buckets work: tokens only arrive through Grant.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate, burst uint64, full bool) *tokenBucket {
	b := &tokenBucket{rate: float64(rate), burst: float64(burst), last: time.Now()}
	if full {
		b.tokens = b.burst
	}
	return b
}

func (b *tokenBucket) refillLocked(now time.Time) {
	if b.rate <= 0 || b.last.IsZero() {
		b.last = now
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
	b.last = now
}

// take removes up to max tokens and returns how many were available.
func (b *tokenBucket) take(max int64) int64 {
	if max <= 0 {
		return 0
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	n := math.Min(float64(max), math.Floor(b.tokens))
	if n <= 0 {
		return 0
	}
	b.tokens -= n
	return int64(n)
}

// add puts tokens back into the tokenBucket, capped at burst.
func (b *tokenBucket) add(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = math.Min(b.burst, b.tokens+float64(n))
}

// available returns the tokens that can be taken right now.
func (b *tokenBucket) available() int64 {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	return int64(math.Floor(b.tokens))
}

// deduct removes n tokens without waiting. It returns the number actually
// deducted (n when the tokenBucket had enough tokens).
func (b *tokenBucket) deduct(n int64) int64 {
	if n <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	got := math.Min(float64(n), math.Floor(b.tokens))
	if got <= 0 {
		return 0
	}
	b.tokens -= got
	return int64(got)
}

// waitFor returns how long until n tokens are available, assuming refill
// continues at rate. It returns 0 when the tokenBucket is already full enough.
func (b *tokenBucket) waitFor(n int64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	miss := float64(n) - b.tokens
	if miss <= 0 {
		return 0
	}
	if b.rate <= 0 {
		return time.Hour // leased buckets: caller must ask the counter
	}
	return time.Duration(miss / b.rate * float64(time.Second))
}

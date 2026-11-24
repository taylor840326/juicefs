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

// Package qos implements client-side quality-of-service (QoS) controls for a
// JuiceFS mount. Limits are enforced with token buckets for data bandwidth,
// data operations (IOPS) and metadata operations.
package qos

import (
	"syscall"
	"time"

	"github.com/juju/ratelimit"
)

// Config is the QoS configuration of one JuiceFS client (one mount process).
// A zero value means "unlimited".
type Config struct {
	// Bandwidth limits the total data bandwidth (readers and writers) in
	// bytes per second.
	Bandwidth uint64 `json:"bandwidth,omitempty"`
	// ReadBandwidth and WriteBandwidth override Bandwidth for one direction.
	// When set for a direction, that direction stops consuming from the
	// shared Bandwidth bucket.
	ReadBandwidth  uint64 `json:"readBandwidth,omitempty"`
	WriteBandwidth uint64 `json:"writeBandwidth,omitempty"`
	// IOPS limits the number of data operations (read/write/copy) per second.
	IOPS uint64 `json:"iops,omitempty"`
	// MetaIOPS limits the number of metadata operations per second.
	MetaIOPS uint64 `json:"metaIOPS,omitempty"`
}

// Enabled reports whether at least one limit is configured.
func (c *Config) Enabled() bool {
	return c != nil && (c.Bandwidth > 0 || c.ReadBandwidth > 0 || c.WriteBandwidth > 0 || c.IOPS > 0 || c.MetaIOPS > 0)
}

// Update is a partial update of Config. A nil field keeps its current value,
// while a pointer to zero explicitly clears that limit. It is the wire format
// used by the `.control` message and the `juicefs qos` command.
type Update struct {
	// Bandwidth is the total data bandwidth in bytes per second.
	Bandwidth *uint64 `json:"bandwidth,omitempty"`
	// ReadBandwidth and WriteBandwidth override Bandwidth for one direction.
	ReadBandwidth  *uint64 `json:"readBandwidth,omitempty"`
	WriteBandwidth *uint64 `json:"writeBandwidth,omitempty"`
	// IOPS is the number of data operations per second.
	IOPS *uint64 `json:"iops,omitempty"`
	// MetaIOPS is the number of metadata operations per second.
	MetaIOPS *uint64 `json:"metaIOPS,omitempty"`
}

// Apply returns a copy of c with the limits present in u replaced.
func (c Config) Apply(u Update) Config {
	if u.Bandwidth != nil {
		c.Bandwidth = *u.Bandwidth
	}
	if u.ReadBandwidth != nil {
		c.ReadBandwidth = *u.ReadBandwidth
	}
	if u.WriteBandwidth != nil {
		c.WriteBandwidth = *u.WriteBandwidth
	}
	if u.IOPS != nil {
		c.IOPS = *u.IOPS
	}
	if u.MetaIOPS != nil {
		c.MetaIOPS = *u.MetaIOPS
	}
	return c
}

// Empty reports whether u does not change any limit.
func (u Update) Empty() bool {
	return u.Bandwidth == nil && u.ReadBandwidth == nil && u.WriteBandwidth == nil && u.IOPS == nil && u.MetaIOPS == nil
}

// Cancelable is the subset of meta.Context needed to abort a blocking wait
// when the caller (or the kernel) gives up on an operation.
type Cancelable interface {
	Canceled() bool
}

// Controller enforces the limits of a Config. A nil *Controller is valid and
// means "unlimited".
type Controller struct {
	conf *Config
	bw   *ratelimit.Bucket // shared bandwidth, used when a direction is not overridden
	rbw  *ratelimit.Bucket
	wbw  *ratelimit.Bucket
	iops *ratelimit.Bucket
	meta *ratelimit.Bucket
}

// New creates a Controller from conf. It returns nil when nothing is limited.
func New(conf *Config) *Controller {
	if !conf.Enabled() {
		return nil
	}
	c := &Controller{conf: conf}
	if conf.Bandwidth > 0 {
		c.bw = newBucket(conf.Bandwidth, conf.Bandwidth/10)
	}
	if conf.ReadBandwidth > 0 {
		c.rbw = newBucket(conf.ReadBandwidth, conf.ReadBandwidth/10)
	}
	if conf.WriteBandwidth > 0 {
		c.wbw = newBucket(conf.WriteBandwidth, conf.WriteBandwidth/10)
	}
	if conf.IOPS > 0 {
		c.iops = newBucket(conf.IOPS, conf.IOPS/10)
	}
	if conf.MetaIOPS > 0 {
		c.meta = newBucket(conf.MetaIOPS, conf.MetaIOPS/10)
	}
	return c
}

// Conf returns a copy of the configuration the controller was created with.
func (c *Controller) Conf() Config {
	if c == nil || c.conf == nil {
		return Config{}
	}
	return *c.conf
}

// newBucket creates a token bucket with the given rate (per second) and burst
// capacity. The burst is one tenth of the rate, clamped to a sane range: at
// least one token, and never more than the rate itself, so that shaping stays
// smooth for both very low and very high limits.
func newBucket(rate, burst uint64) *ratelimit.Bucket {
	if burst < 1 {
		burst = 1
	}
	if burst > rate {
		burst = rate
	}
	return ratelimit.NewBucketWithRate(float64(rate), int64(burst))
}

// waitSlice bounds how long a single blocking wait lasts before the controller
// re-checks whether the caller has given up, so that a canceled operation is
// not stuck waiting for its tokens.
const waitSlice = 100 * time.Millisecond

// WaitData blocks until one data operation of the given size is allowed,
// then consumes its tokens. read selects the read or the write direction.
// It returns EINTR if ctx was canceled while waiting.
func (c *Controller) WaitData(ctx Cancelable, read bool, bytes int64) syscall.Errno {
	if c == nil {
		return 0
	}
	if c.iops != nil {
		if err := c.wait(ctx, c.iops, 1); err != 0 {
			return err
		}
	}
	b := c.bw
	if read && c.rbw != nil {
		b = c.rbw
	} else if !read && c.wbw != nil {
		b = c.wbw
	}
	return c.wait(ctx, b, bytes)
}

// WaitMeta blocks until one metadata operation is allowed.
func (c *Controller) WaitMeta(ctx Cancelable) syscall.Errno {
	if c == nil {
		return 0
	}
	return c.wait(ctx, c.meta, 1)
}

// WaitOp waits for one data operation without charging any bandwidth. It is
// used by operations that move data on the server side (copy_file_range), so
// that they still consume the IOPS budget.
func (c *Controller) WaitOp(ctx Cancelable) syscall.Errno {
	if c == nil {
		return 0
	}
	return c.wait(ctx, c.iops, 1)
}

// wait charges n tokens against the bucket, waiting as long as needed. A
// request larger than the bucket capacity is split into capacity-sized
// chunks, because a token bucket can never hold more than its capacity.
func (c *Controller) wait(ctx Cancelable, b *ratelimit.Bucket, n int64) syscall.Errno {
	if b == nil || n <= 0 {
		return 0
	}
	capacity := b.Capacity()
	if capacity < 1 {
		capacity = 1
	}
	for n > 0 {
		if ctx.Canceled() {
			return syscall.EINTR
		}
		chunk := n
		if chunk > capacity {
			chunk = capacity
		}
		n -= chunk
		if d := b.Take(chunk); d > 0 {
			if err := sleep(ctx, d); err != 0 {
				return err
			}
		}
	}
	return 0
}

// sleep waits for d, checking ctx every waitSlice so that a canceled operation
// does not keep the caller waiting longer than one slice.
func sleep(ctx Cancelable, d time.Duration) syscall.Errno {
	for d > 0 {
		if ctx.Canceled() {
			return syscall.EINTR
		}
		step := d
		if step > waitSlice {
			step = waitSlice
		}
		time.Sleep(step)
		d -= step
	}
	return 0
}

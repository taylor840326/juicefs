/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
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
	"syscall"
	"testing"
	"time"
)

type cancelFlag bool

func (c cancelFlag) Canceled() bool { return bool(c) }

func TestDisabled(t *testing.T) {
	for _, conf := range []*Config{nil, {}, {Bandwidth: 0, ReadBandwidth: 0, WriteBandwidth: 0}} {
		c := New(conf)
		if c != nil {
			t.Fatalf("expected nil controller for %+v", conf)
		}
		start := time.Now()
		if err := c.WaitData(cancelFlag(false), true, 1<<20); err != 0 {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := c.WaitMeta(cancelFlag(false)); err != 0 {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := c.WaitOp(cancelFlag(false)); err != 0 {
			t.Fatalf("unexpected error: %v", err)
		}
		if d := time.Since(start); d > 10*time.Millisecond {
			t.Fatalf("unlimited controller should not block, took %s", d)
		}
	}
}

func TestBandwidth(t *testing.T) {
	c := New(&Config{Bandwidth: 1 << 20}) // 1MiB/s, burst 100KiB
	start := time.Now()
	for i := 0; i < 3; i++ { // 600KiB in total: 100KiB of burst + 500KiB of waiting
		if err := c.WaitData(cancelFlag(false), true, 200<<10); err != 0 {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 350*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("600KiB at 1MiB/s should take about 500ms, took %s", elapsed)
	}
}

func TestDirectionSplit(t *testing.T) {
	c := New(&Config{ReadBandwidth: 1 << 20, WriteBandwidth: 1 << 20})
	if err := c.WaitData(cancelFlag(false), true, 200<<10); err != 0 { // wait ~100ms for read tokens
		t.Fatalf("unexpected error: %v", err)
	}
	start := time.Now()
	if err := c.WaitData(cancelFlag(false), false, 100<<10); err != 0 { // fits in the write burst
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("read limit should not slow down writes, wrote after %s", d)
	}
}

func TestIOPS(t *testing.T) {
	c := New(&Config{IOPS: 50}) // burst 5
	start := time.Now()
	for i := 0; i < 10; i++ { // 5 free + 5 * 20ms
		if err := c.WaitOp(cancelFlag(false)); err != 0 {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 60*time.Millisecond || elapsed > time.Second {
		t.Fatalf("10 ops at 50 IOPS should take about 100ms, took %s", elapsed)
	}
}

func TestMetaIOPS(t *testing.T) {
	c := New(&Config{MetaIOPS: 100}) // burst 10
	start := time.Now()
	for i := 0; i < 30; i++ { // 10 free + 20 * 10ms
		if err := c.WaitMeta(cancelFlag(false)); err != 0 {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 120*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("30 meta ops at 100 ops/s should take about 200ms, took %s", elapsed)
	}
}

func TestLargeRequest(t *testing.T) {
	c := New(&Config{Bandwidth: 1 << 20}) // burst 100KiB
	start := time.Now()
	if err := c.WaitData(cancelFlag(false), true, 1<<20); err != 0 { // 1MiB, chunked by burst
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("1MiB at 1MiB/s should take about 1s, took %s", elapsed)
	}
}

func TestCancel(t *testing.T) {
	c := New(&Config{Bandwidth: 1 << 20})
	if err := c.WaitData(cancelFlag(false), true, 100<<10); err != 0 { // exhaust the burst
		t.Fatalf("unexpected error: %v", err)
	}
	start := time.Now()
	if err := c.WaitData(cancelFlag(true), true, 10<<20); err != syscall.EINTR {
		t.Fatalf("expected EINTR, got %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("canceled wait should return promptly, took %s", d)
	}
}

func TestCancelMeta(t *testing.T) {
	c := New(&Config{MetaIOPS: 10}) // burst 1
	if err := c.WaitMeta(cancelFlag(false)); err != 0 {
		t.Fatalf("unexpected error: %v", err)
	}
	start := time.Now()
	if err := c.WaitMeta(cancelFlag(true)); err != syscall.EINTR {
		t.Fatalf("expected EINTR, got %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("canceled meta wait should return promptly, took %s", d)
	}
}

func TestNonPositiveSize(t *testing.T) {
	c := New(&Config{Bandwidth: 1 << 20})
	for _, n := range []int64{0, -1} {
		if err := c.WaitData(cancelFlag(false), false, n); err != 0 {
			t.Fatalf("unexpected error for size %d: %v", n, err)
		}
	}
}

func TestUpdateApply(t *testing.T) {
	cur := Config{Bandwidth: 100, ReadBandwidth: 200, IOPS: 50}
	if got := cur.Apply(Update{}); got != cur {
		t.Fatalf("empty update should keep the config, got %+v", got)
	}
	bw := uint64(300)
	got := cur.Apply(Update{Bandwidth: &bw})
	if got.Bandwidth != 300 || got.ReadBandwidth != 200 || got.IOPS != 50 {
		t.Fatalf("unexpected merged config: %+v", got)
	}
	zero := uint64(0)
	got = got.Apply(Update{ReadBandwidth: &zero})
	if got.ReadBandwidth != 0 || got.Bandwidth != 300 || got.IOPS != 50 {
		t.Fatalf("zero should clear only the given limit: %+v", got)
	}
	if !(Update{}).Empty() || (Update{Bandwidth: &bw}).Empty() {
		t.Fatalf("unexpected Empty result")
	}
}

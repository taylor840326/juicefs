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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const policyJSON = `{
  "version": 3,
  "tenant": "t1",
  "cap": {"bandwidth": 10485760, "iops": 5000, "metaIOPS": 3000},
  "rules": [
    {"id": "a", "path": "/a", "ino": 100, "limits": {"bandwidth": 5242880, "iops": 1000}},
    {"id": "b", "path": "/b", "ino": 200, "limits": {"bandwidth": 2097152, "readBandwidth": 1048576, "metaOps": {"lookup": 200}}}
  ]
}`

func mustPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(policyJSON))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParsePolicy(t *testing.T) {
	p := mustPolicy(t)
	if p.Tenant != "t1" || p.Version != 3 {
		t.Fatalf("unexpected header: %+v", p)
	}
	if r := p.Rule(100); r == nil || r.Limits.Bandwidth != 5242880 {
		t.Fatalf("rule 100 not parsed: %+v", r)
	}
	if got := p.Rule(200).Limits.Rate(DimReadBandwidth); got != 1048576 {
		t.Fatalf("read bandwidth: %d", got)
	}
	if got := p.Rule(200).Limits.Rate(MetaDim(MetaLookup)); got != 200 {
		t.Fatalf("meta lookup rate: %d", got)
	}
	if p.Rule(999) != nil {
		t.Fatal("unexpected rule")
	}
}

func TestParsePolicyErrors(t *testing.T) {
	cases := []string{
		`{"tenant":"","cap":{}}`,
		`{"tenant":"t","rules":[{"ino":0,"limits":{"iops":1}}]}`,
		`{"tenant":"t","rules":[{"ino":1,"limits":{"iops":1}},{"ino":1,"limits":{"iops":2}}]}`,
		`{"tenant":"t","rules":[{"ino":1,"limits":{}}]}`,
	}
	for _, c := range cases {
		if _, err := ParsePolicy([]byte(c)); err == nil {
			t.Fatalf("expected error for %s", c)
		}
	}
}

func TestDimensionReqs(t *testing.T) {
	l := Limits{Bandwidth: 1000, WriteBandwidth: 2000, IOPS: 10}
	if got := l.dimensionReqs(Op{Read: true, Bytes: 100}); len(got) != 2 ||
		got[0].Dim != DimIOPS || got[1].Dim != DimBandwidth || got[1].N != 100 {
		t.Fatalf("read dims: %+v", got)
	}
	if got := l.dimensionReqs(Op{Bytes: 100}); got[1].Dim != DimWriteBandwidth {
		t.Fatalf("write dims: %+v", got)
	}
	if got := (Limits{IOPS: 5}).dimensionReqs(Op{Bytes: 10}); len(got) != 1 || got[0].Dim != DimIOPS {
		t.Fatalf("iops only: %+v", got)
	}
	if got := (Limits{}).dimensionReqs(Op{Bytes: 10}); len(got) != 0 {
		t.Fatalf("unlimited: %+v", got)
	}
	m := Limits{MetaIOPS: 3, MetaOps: map[string]uint64{MetaLookup: 7}}
	if got := m.dimensionReqs(Op{Meta: MetaLookup}); len(got) != 2 ||
		got[0].Dim != MetaDim(MetaLookup) || got[1].Dim != DimMetaIOPS {
		t.Fatalf("meta dims: %+v", got)
	}
}

func TestResolvePaths(t *testing.T) {
	p, err := ParsePolicy([]byte(`{"tenant":"t1","rules":[
		{"id":"a","path":"proj-a","limits":{"bandwidth":1000}},
		{"id":"b","ino":200,"limits":{"iops":5}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Rule(100) != nil {
		t.Fatal("unresolved rule must not be indexed")
	}
	if err := p.ResolvePaths(func(path string) (uint64, error) {
		if path == "proj-a" {
			return 100, nil
		}
		return 0, fmt.Errorf("not found")
	}, t.Logf); err != nil {
		t.Fatal(err)
	}
	if r := p.Rule(100); r == nil || r.Limits.Bandwidth != 1000 {
		t.Fatalf("path was not bound: %+v", p.Rule(100))
	}
	if r := p.Rule(200); r == nil {
		t.Fatal("inode rule lost")
	}
}

func TestValidateBudget(t *testing.T) {
	p := mustPolicy(t)
	parent := func(ino uint64) uint64 {
		if ino == 100 || ino == 200 {
			return 0
		}
		return 0
	}
	if err := p.ValidateBudget(parent, DimBandwidth); err != nil {
		t.Fatalf("5+2 <= 10 MiB should fit: %v", err)
	}
	// raise rule b above the remaining budget
	p.Rules[1].Limits.Bandwidth = 6 << 20
	if err := p.ValidateBudget(parent, DimBandwidth); err == nil {
		t.Fatal("expected budget violation")
	}
}

func TestLocalCharge(t *testing.T) {
	m := NewManager("t1", nil, true)
	s := &Scope{Chain: []Level{
		{Ino: 100, Limits: Limits{Bandwidth: 2 << 20}}, // 2 MiB/s
		{Ino: 0, Limits: Limits{Bandwidth: 10 << 20}},
	}}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := m.Charge(context.Background(), s, Op{Bytes: 100 << 10}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("charge returned too fast: %s", d)
	}
}

func TestLocalStrictPartition(t *testing.T) {
	m := NewManager("t1", nil, true)
	// two siblings share a 4 MiB/s parent; each is capped at 2 MiB/s, and
	// the second one being idle must not raise the first one's ceiling.
	sibling := &Scope{Chain: []Level{
		{Ino: 100, Limits: Limits{Bandwidth: 2 << 20}},
		{Ino: 0, Limits: Limits{Bandwidth: 4 << 20}},
	}}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := m.Charge(context.Background(), sibling, Op{Bytes: 100 << 10}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("sibling overran its own quota: %s", d)
	}
}

func TestCanceledCharge(t *testing.T) {
	m := NewManager("t1", nil, true)
	s := &Scope{Chain: []Level{{Ino: 1, Limits: Limits{Bandwidth: 1 << 20}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Charge(ctx, s, Op{Bytes: 10 << 20}); err == nil {
		t.Fatal("expected context error")
	}
}

func TestCounterGrant(t *testing.T) {
	c := NewCounter()
	levels := []LeaseLevel{{Ino: 100, Rate: 1000}, {Ino: 0, Rate: 1000}}
	// The bucket capacity is one lease quantum (rate/2 = 500).
	if got := c.Grant("t1", levels, DimBandwidth, 480); got != 480 {
		t.Fatalf("first grant: %d", got)
	}
	// Both levels were deducted; 20 tokens are left (plus a negligible refill).
	got := c.Grant("t1", levels, DimBandwidth, 480)
	if got < 18 || got > 30 {
		t.Fatalf("second grant: %d", got)
	}
	if got := c.Grant("t2", levels, DimBandwidth, 480); got != 480 {
		t.Fatalf("another tenant must have its own buckets: %d", got)
	}
}

type countingLease struct {
	mu      sync.Mutex
	calls   int
	latency time.Duration
	perCall int64
}

func (c *countingLease) Grant(ctx context.Context, tenant string, levels []LeaseLevel, dim string, want int64) (int64, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	time.Sleep(c.latency)
	if want > c.perCall {
		want = c.perCall
	}
	return want, nil
}

func (c *countingLease) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestLeaseRequestCoalescing makes sure a burst of blocked operations does not
// turn into one HTTP request per operation.
func TestLeaseRequestCoalescing(t *testing.T) {
	lease := &countingLease{latency: 5 * time.Millisecond, perCall: 10}
	m := NewManager("t1", lease, false)
	scope := &Scope{Chain: []Level{{Ino: 100, Limits: Limits{Bandwidth: 40}}}}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Charge(context.Background(), scope, Op{Bytes: 1}); err != nil {
				t.Errorf("charge: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls := lease.Calls(); calls >= 20 {
		t.Fatalf("40 charges caused %d lease requests; coalescing/pacing is not working", calls)
	}
}

func TestLeaseSharedBudgets(t *testing.T) {
	counter := NewCounter()
	srv := httptest.NewServer(counter.Handler("secret"))
	defer srv.Close()

	levels := []LeaseLevel{{Ino: 0, Rate: 1000}}
	client := NewHTTPLeaseClient(srv.URL, "token-mismatch", 2)
	m := NewManager("t1", client, false)
	s := &Scope{Chain: []Level{{Ino: 0, Limits: Limits{Bandwidth: 1000}}}}
	if err := m.Charge(context.Background(), s, Op{Bytes: 10}); err == nil {
		t.Fatal("expected auth error")
	}

	client.Token = "secret"
	if err := m.Charge(context.Background(), s, Op{Bytes: 10}); err != nil {
		t.Fatalf("charge with token: %v", err)
	}
	_ = levels
}

func TestLeaseFailOpen(t *testing.T) {
	s := &Scope{Chain: []Level{{Ino: 0, Limits: Limits{Bandwidth: 1 << 20}}}}
	bad := NewHTTPLeaseClient("http://127.0.0.1:1", "", 2)
	if err := NewManager("t1", bad, true).Charge(context.Background(), s, Op{Bytes: 1 << 20}); err != nil {
		t.Fatalf("fail-open must not error: %v", err)
	}
	if err := NewManager("t1", bad, false).Charge(context.Background(), s, Op{Bytes: 1 << 20}); err == nil {
		t.Fatal("fail-closed must error")
	}
}

func TestCounterHandler(t *testing.T) {
	counter := NewCounter()
	h := counter.Handler("")
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"tenant":"t1","dim":"bandwidth","want":10,"levels":[{"ino":0,"rate":100000}]}`
	resp, err := http.Post(srv.URL+"/v1/grant", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out grantResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if out.Granted != 10 {
		t.Fatalf("granted: %d", out.Granted)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/grant", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", resp2.StatusCode)
	}
}

func TestCounterAuth(t *testing.T) {
	srv := httptest.NewServer(NewCounter().Handler("tok"))
	defer srv.Close()
	body := `{"tenant":"t","dim":"bandwidth","want":1,"levels":[{"ino":0,"rate":10}]}`
	resp, err := http.Post(srv.URL+"/v1/grant", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestChargeStaleAbortsWait checks that a blocked charge gives up as soon as
// the policy changes, so the caller can re-resolve its scope.
func TestChargeStaleAbortsWait(t *testing.T) {
	lease := &countingLease{latency: 5 * time.Millisecond, perCall: 0} // never grants
	m := NewManager("t1", lease, false)
	scope := &Scope{Chain: []Level{{Ino: 100, Limits: Limits{MetaIOPS: 2}}}}
	start := time.Now()
	err := m.ChargeWatch(context.Background(), scope, Op{Meta: MetaLookup}, func() bool {
		return time.Since(start) > 30*time.Millisecond
	})
	if err != ErrStale {
		t.Fatalf("expected ErrStale, got %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("aborted too slowly: %v", d)
	}
}

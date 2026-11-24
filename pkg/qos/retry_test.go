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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		try  int
		want time.Duration
	}{
		{0, time.Millisecond},
		{1, time.Millisecond},
		{2, 301 * time.Millisecond},
		{3, 601 * time.Millisecond},
		{29, 8401 * time.Millisecond},
		{30, 10 * time.Second},
		{100, 10 * time.Second},
	}
	for _, c := range cases {
		if got := RetryDelay(c.try); got != c.want {
			t.Fatalf("RetryDelay(%d) = %s, want %s", c.try, got, c.want)
		}
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&HTTPError{StatusCode: http.StatusServiceUnavailable}, true},
		{&HTTPError{StatusCode: http.StatusTooManyRequests}, true},
		{&HTTPError{StatusCode: http.StatusGatewayTimeout}, true},
		{&HTTPError{StatusCode: http.StatusBadRequest}, false},
		{&HTTPError{StatusCode: http.StatusUnauthorized}, false},
		{&HTTPError{StatusCode: http.StatusNotFound}, false},
		{fmt.Errorf("connection reset by peer"), true},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Fatalf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestGrantRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"warming up"}`))
			return
		}
		_, _ = w.Write([]byte(`{"granted":42}`))
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "", 5)
	n, err := c.Grant(context.Background(), "t1", []LeaseLevel{{Ino: 0, Rate: 1000}}, DimBandwidth, 100)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if n != 42 {
		t.Fatalf("granted = %d, want 42", n)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestGrantNonRetryable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "", 5)
	_, err := c.Grant(context.Background(), "t1", []LeaseLevel{{Ino: 0, Rate: 1000}}, DimBandwidth, 100)
	if err == nil {
		t.Fatal("expected an error")
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not be retried)", got)
	}
}

func TestGrantGivesUpAfterRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "", 2)
	_, err := c.Grant(context.Background(), "t1", []LeaseLevel{{Ino: 0, Rate: 1000}}, DimBandwidth, 100)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3 (1 attempt + 2 retries)", got)
	}
}

type blockingLease struct{}

func (blockingLease) Grant(ctx context.Context, tenant string, levels []LeaseLevel, dim string, want int64) (int64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestLeaseFailOpenBudgetAndCooldown(t *testing.T) {
	oldBudget, oldCooldown := leaseGrantBudget, leaseCooldown
	leaseGrantBudget, leaseCooldown = 60*time.Millisecond, 300*time.Millisecond
	defer func() {
		leaseGrantBudget, leaseCooldown = oldBudget, oldCooldown
	}()

	m := NewManager("t1", blockingLease{}, true)
	s := &Scope{Chain: []Level{{Ino: 1, Limits: Limits{Bandwidth: 1 << 20}}}}

	start := time.Now()
	if err := m.Charge(context.Background(), s, Op{Bytes: 64 << 10}); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("first charge returned after %s, expected it to wait for the grant budget", d)
	}

	start = time.Now()
	if err := m.Charge(context.Background(), s, Op{Bytes: 64 << 10}); err != nil {
		t.Fatalf("second charge: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("second charge took %s, expected it to be let through during cooldown", d)
	}
}

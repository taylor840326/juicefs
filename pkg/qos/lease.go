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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LeaseLevel is one tokenBucket of a grant request. The client sends the chain
// from the level being charged up to the tenant cap, with the rate of each
// level, so the counter does not need a copy of the policy tree.
type LeaseLevel struct {
	Ino  uint64 `json:"ino"`
	Rate uint64 `json:"rate"`
}

// LeaseClient asks a shared counter service for tokens. A counter guarantees
// that the sum of all grants never exceeds the tenant cap, which is what makes
// several mounts share one budget.
type LeaseClient interface {
	Grant(ctx context.Context, tenant string, levels []LeaseLevel, dim string, want int64) (int64, error)
}

// HTTPLeaseClient is the default LeaseClient: it talks to juicefs qosd.
type HTTPLeaseClient struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// NewHTTPLeaseClient creates a client for the counter service at url. An
// empty token means the service does not require authentication.
func NewHTTPLeaseClient(url, token string) *HTTPLeaseClient {
	return &HTTPLeaseClient{
		URL:   strings.TrimRight(url, "/"),
		Token: token,
		HTTP:  &http.Client{Timeout: 5 * time.Second},
	}
}

type grantRequest struct {
	Tenant string       `json:"tenant"`
	Dim    string       `json:"dim"`
	Want   int64        `json:"want"`
	Levels []LeaseLevel `json:"levels"`
}

type grantResponse struct {
	Granted int64  `json:"granted"`
	Error   string `json:"error,omitempty"`
}

func (c *HTTPLeaseClient) Grant(ctx context.Context, tenant string, levels []LeaseLevel, dim string, want int64) (int64, error) {
	if want <= 0 || len(levels) == 0 {
		return 0, nil
	}
	body, err := json.Marshal(grantRequest{Tenant: tenant, Dim: dim, Want: want, Levels: levels})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/grant", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("lease grant: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out grantResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("lease grant: invalid response: %w", err)
	}
	if out.Error != "" {
		return 0, fmt.Errorf("lease grant: %s", out.Error)
	}
	return out.Granted, nil
}

// Counter is the reference hierarchical token counter. One Counter serves any
// number of mounts; it is safe for concurrent use.
type Counter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// NewCounter creates an empty counter.
func NewCounter() *Counter {
	return &Counter{buckets: make(map[string]*tokenBucket)}
}

func counterKey(tenant string, ino uint64, dim string) string {
	return fmt.Sprintf("%s|%d|%s", tenant, ino, dim)
}

func (c *Counter) bucketLocked(tenant string, l LeaseLevel, dim string) *tokenBucket {
	key := counterKey(tenant, l.Ino, dim)
	burst := burstFor(l.Rate)
	b := c.buckets[key]
	if b == nil || b.rate != float64(l.Rate) || b.burst != float64(burst) {
		b = newTokenBucket(l.Rate, burst, true)
		c.buckets[key] = b
	}
	return b
}

// Grant deducts want tokens from every level of the chain, stopping at the
// level with the fewest tokens, and returns the granted amount. Because the
// levels are charged together, the sum of grants for a child can never exceed
// the tokens its ancestors had. Unused capacity of siblings is never lent out:
// each chain only sees its own buckets.
func (c *Counter) Grant(tenant string, levels []LeaseLevel, dim string, want int64) int64 {
	if want <= 0 || len(levels) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	buckets := make([]*tokenBucket, 0, len(levels))
	for _, l := range levels {
		if l.Rate == 0 {
			continue
		}
		buckets = append(buckets, c.bucketLocked(tenant, l, dim))
	}
	if len(buckets) == 0 {
		return want
	}
	granted := want
	for _, b := range buckets {
		if a := b.available(); a < granted {
			granted = a
		}
	}
	if granted <= 0 {
		return 0
	}
	for _, b := range buckets {
		if got := b.deduct(granted); got < granted {
			// available() and deduct() run under c.mu, so this can only be
			// a rounding artifact; clamp to the smallest deduction.
			granted = got
		}
	}
	return granted
}

// Handler returns the HTTP API of the counter: POST /v1/grant and
// GET /v1/healthz. An empty token disables authentication.
func (c *Counter) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/grant", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" && !checkToken(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req grantRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Tenant == "" || req.Dim == "" || len(req.Levels) == 0 {
			http.Error(w, "tenant, dim and levels are required", http.StatusBadRequest)
			return
		}
		out := grantResponse{Granted: c.Grant(req.Tenant, req.Levels, req.Dim, req.Want)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

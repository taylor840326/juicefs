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
	"net"
	"net/http"
	"time"
)

// HTTPError is returned when the QoS service answers with an unexpected HTTP
// status. Transport failures are returned as their original error.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// Retryable reports whether a failed call to the QoS service is worth
// retrying. Authentication and other client errors are final; timeouts,
// rate limiting, server errors and transport failures are retried.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusRequestTimeout, // 408
			http.StatusTooEarly,            // 425
			http.StatusTooManyRequests,     // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		}
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	// Anything else that is not a context error comes from the transport
	// (connection reset, refused, EOF, ...) or from a truncated response.
	return true
}

// RetryDelay returns the wait before retry number try (1-based). It follows
// the backoff used by the io-retries implementation in pkg/vfs: 300ms steps
// for the first 30 retries, then 10s.
func RetryDelay(try int) time.Duration {
	if try < 1 {
		try = 1
	}
	if try < 30 {
		return time.Duration((try-1)*300+1) * time.Millisecond
	}
	return 10 * time.Second
}

// sleepWithCancel sleeps for d, returning false when ctx is canceled first.
// It polls in 100ms slices because FUSE contexts do not implement Done().
func sleepWithCancel(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if canceled(ctx) {
			return false
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return true
		}
		if remain > 100*time.Millisecond {
			remain = 100 * time.Millisecond
		}
		time.Sleep(remain)
	}
}

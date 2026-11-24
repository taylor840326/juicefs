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
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Updater is implemented by the client that owns the effective QoS limits. It
// is used by the HTTP handler to read and change them at runtime.
type Updater interface {
	GetQoS() Config
	UpdateQoS(Update) Config
}

// HTTPHandler returns the handler of the client QoS HTTP API. get must return
// the owner of the limits, or nil while the client is not ready yet.
//
// Requests must carry the token in either "Authorization: Bearer <token>" or
// "X-JuiceFS-Token: <token>". An empty token disables the API: every request
// is answered with 403, so an unconfigured mount never exposes a control
// surface. Comparing the token is constant time.
//
//	GET  /api/qos  - return the effective limits as JSON
//	PUT  /api/qos  - apply a partial update (same JSON as Config, omitted
//	                 fields keep their value, 0 clears that limit)
func HTTPHandler(get func() Updater, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(status int, v interface{}) {
			w.Header().Set("Content-Type", "application/json")
			buf, err := json.Marshal(v)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write(buf)
		}

		if token == "" {
			writeJSON(http.StatusForbidden, map[string]string{
				"error": "QoS HTTP API is disabled: no token configured",
			})
			return
		}
		if !checkToken(r, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="juicefs-qos"`)
			writeJSON(http.StatusUnauthorized, map[string]string{
				"error": "invalid or missing token",
			})
			return
		}

		updater := get()
		if updater == nil {
			writeJSON(http.StatusServiceUnavailable, map[string]string{
				"error": "JuiceFS client is not ready",
			})
			return
		}

		switch r.Method {
		case http.MethodGet:
			writeJSON(http.StatusOK, updater.GetQoS())
		case http.MethodPut, http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
			var update Update
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&update); err != nil {
				writeJSON(http.StatusBadRequest, map[string]string{
					"error": "invalid JSON body: " + err.Error(),
				})
				return
			}
			writeJSON(http.StatusOK, updater.UpdateQoS(update))
		default:
			w.Header().Set("Allow", "GET, PUT, POST")
			writeJSON(http.StatusMethodNotAllowed, map[string]string{
				"error": "method not allowed",
			})
		}
	})
}

// checkToken extracts the token from the request and compares it in constant
// time.
func checkToken(r *http.Request, token string) bool {
	got := r.Header.Get("X-JuiceFS-Token")
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		got = strings.TrimSpace(auth[len("Bearer "):])
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

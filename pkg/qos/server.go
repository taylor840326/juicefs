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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/juicedata/juicefs/pkg/version"
)

// Server is the standalone qosd HTTP API:
//
//	GET    /v1/healthz                          service and counter status
//	GET    /v1/policies[?tenant=NAME]           list policies
//	GET    /v1/policies/{volume}                policy of one volume
//	PUT    /v1/policies/{volume}                create or replace a policy
//	DELETE /v1/policies/{volume}                delete a policy
//	POST   /v1/policies/{volume}/rules          add one rule
//	PUT    /v1/policies/{volume}/rules/{id}     replace one rule
//	DELETE /v1/policies/{volume}/rules/{id}     delete one rule
//	POST   /v1/grant                            token-bucket lease (mounts)
//
// Every request except /v1/healthz is authenticated with the bearer token when
// one is set. The policy endpoints return the same JSON document the mount used
// to read from --qos-policy-file, so a mount can poll /v1/policies/{volume} and
// apply it unchanged (it resolves rule paths to inodes itself).
type Server struct {
	store   PolicyStore
	counter *Counter
	token   string
}

// NewServer builds the HTTP API on top of a policy store and the shared
// counter. An empty token disables authentication.
func NewServer(store PolicyStore, counter *Counter, token string) *Server {
	return &Server{store: store, counter: counter, token: token}
}

// ServeHTTP implements the whole API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Liveness probes are unauthenticated so orchestrators can check the
	// service without holding the tenant token.
	if strings.TrimSuffix(r.URL.Path, "/") == "/v1/healthz" {
		s.health(w, r)
		return
	}
	if s.token != "" && !checkToken(r, s.token) {
		logger.Warnf("qosd: rejected %s %s from %s: bad token", r.Method, r.URL.Path, r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Bearer realm="qosd"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/v1/healthz":
		s.health(w, r)
	case path == "/v1/grant":
		s.grant(w, r)
	case path == "/v1/policies":
		s.listPolicies(w, r)
	case strings.HasPrefix(path, "/v1/policies/"):
		s.policyRoute(w, r, strings.TrimPrefix(path, "/v1/policies/"))
	default:
		logger.Warnf("qosd: rejected %s %s from %s: not found", r.Method, r.URL.Path, r.RemoteAddr)
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"service": "qosd",
		"version": version.Version(),
		"counter": s.counter.Stats(),
	})
}

func (s *Server) policyRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "volume is required")
		return
	}
	volume := parts[0]
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			s.getPolicy(w, r, volume)
		case http.MethodPut:
			s.putPolicy(w, r, volume)
		case http.MethodDelete:
			s.deletePolicy(w, r, volume)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case len(parts) == 2 && parts[1] == "rules":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.addRule(w, r, volume)
	case len(parts) == 3 && parts[1] == "rules":
		switch r.Method {
		case http.MethodPut:
			s.updateRule(w, r, volume, parts[2])
		case http.MethodDelete:
			s.deleteRule(w, r, volume, parts[2])
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := r.URL.Query().Get("tenant")
	policies, err := s.store.List(r.Context(), tenant)
	if err != nil {
		logger.Warnf("qosd: list policies tenant=%q: %s", tenant, err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Debugf("qosd: list policies tenant=%q -> %d", tenant, len(policies))
	writeJSON(w, http.StatusOK, map[string]interface{}{"policies": policies, "count": len(policies)})
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request, volume string) {
	p, err := s.store.Get(r.Context(), volume)
	if err != nil {
		if errors.Is(err, ErrPolicyNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no QoS policy for volume %q", volume))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request, volume string) {
	p, err := decodePolicy(r)
	if err != nil {
		logger.Warnf("qosd: invalid policy for %s: %s", volume, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.Volume = volume
	if err := p.init(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for i := range p.Rules {
		if err := normalizeRule(&p.Rules[i]); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := p.Reindex(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, err := s.store.Put(r.Context(), volume, p)
	if err != nil {
		logger.Warnf("qosd: store policy %s: %s", volume, err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Infof("qosd: policy %s stored version=%d tenant=%s rules=%d", volume, stored.Version, stored.Tenant, len(stored.Rules))
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request, volume string) {
	if err := s.store.Delete(r.Context(), volume); err != nil {
		if errors.Is(err, ErrPolicyNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no QoS policy for volume %q", volume))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Infof("qosd: policy %s deleted", volume)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addRule(w http.ResponseWriter, r *http.Request, volume string) {
	var rule Rule
	if err := decodeJSON(r, &rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := normalizeRule(&rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.store.Get(r.Context(), volume)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no QoS policy for volume %q", volume))
		return
	}
	for _, r0 := range p.Rules {
		if r0.ID == rule.ID {
			writeError(w, http.StatusConflict, fmt.Sprintf("rule %q already exists", rule.ID))
			return
		}
		if r0.Path != "" && r0.Path == rule.Path {
			writeError(w, http.StatusConflict, fmt.Sprintf("rule for path %q already exists", rule.Path))
			return
		}
	}
	p.Rules = append(p.Rules, rule)
	stored, err := s.store.Put(r.Context(), volume, p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Infof("qosd: policy %s rule %s added (version=%d)", volume, rule.ID, stored.Version)
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) updateRule(w http.ResponseWriter, r *http.Request, volume, id string) {
	var rule Rule
	if err := decodeJSON(r, &rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rule.ID = id
	if err := normalizeRule(&rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.store.Get(r.Context(), volume)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no QoS policy for volume %q", volume))
		return
	}
	found := false
	for i := range p.Rules {
		if p.Rules[i].ID == id {
			p.Rules[i] = rule
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("rule %q not found", id))
		return
	}
	stored, err := s.store.Put(r.Context(), volume, p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Infof("qosd: policy %s rule %s updated (version=%d)", volume, id, stored.Version)
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request, volume, id string) {
	p, err := s.store.Get(r.Context(), volume)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no QoS policy for volume %q", volume))
		return
	}
	out := p.Rules[:0]
	for _, rule := range p.Rules {
		if rule.ID != id {
			out = append(out, rule)
		}
	}
	if len(out) == len(p.Rules) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("rule %q not found", id))
		return
	}
	p.Rules = out
	stored, err := s.store.Put(r.Context(), volume, p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logger.Infof("qosd: policy %s rule %s deleted (version=%d)", volume, id, stored.Version)
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) grant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req grantRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Tenant == "" || req.Dim == "" || len(req.Levels) == 0 {
		writeError(w, http.StatusBadRequest, "tenant, dim and levels are required")
		return
	}
	out := grantResponse{Granted: s.counter.Grant(req.Tenant, req.Levels, req.Dim, req.Want)}
	if out.Granted == 0 {
		logger.Debugf("qosd: request throttled tenant=%s dim=%s want=%d levels=%s",
			req.Tenant, req.Dim, req.Want, levelsText(req.Levels))
	}
	writeJSON(w, http.StatusOK, out)
}

// normalizeRule fills an id derived from the path and validates the rule.
func normalizeRule(rule *Rule) error {
	if rule.Path == "" && rule.Ino == 0 {
		return errors.New("rule requires a path or an inode")
	}
	if !rule.Limits.Enabled() {
		return fmt.Errorf("rule %q has no limits", rule.ID)
	}
	if rule.ID == "" {
		base := strings.Trim(strings.ReplaceAll(rule.Path, "/", "-"), "-")
		if base == "" {
			base = fmt.Sprintf("ino-%d", rule.Ino)
		}
		rule.ID = base
	}
	return nil
}

func decodePolicy(r *http.Request) (*Policy, error) {
	var p Policy
	if err := decodeJSON(r, &p); err != nil {
		return nil, err
	}
	if p.Tenant == "" {
		return nil, errors.New("tenant is required")
	}
	return &p, nil
}

func decodeJSON(r *http.Request, v interface{}) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type stubUpdater struct {
	conf Config
	last Update
	got  bool
}

func (s *stubUpdater) GetQoS() Config { return s.conf }

func (s *stubUpdater) UpdateQoS(u Update) Config {
	s.got = true
	s.last = u
	s.conf = s.conf.Apply(u)
	return s.conf
}

func doRequest(t *testing.T, h http.Handler, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/qos", nil)
	} else {
		r = httptest.NewRequest(method, "/api/qos", bytes.NewReader([]byte(body)))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTPDisabledWithoutToken(t *testing.T) {
	h := HTTPHandler(func() Updater { return nil }, "")
	w := doRequest(t, h, http.MethodGet, "", nil)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestHTTPAuth(t *testing.T) {
	h := HTTPHandler(func() Updater { return &stubUpdater{} }, "secret")
	require.Equal(t, http.StatusUnauthorized, doRequest(t, h, http.MethodGet, "", nil).Code)
	require.Equal(t, http.StatusUnauthorized,
		doRequest(t, h, http.MethodGet, "", map[string]string{"Authorization": "Bearer nope"}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(t, h, http.MethodGet, "", map[string]string{"X-JuiceFS-Token": "secret"}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(t, h, http.MethodGet, "", map[string]string{"Authorization": "Bearer secret"}).Code)
}

func TestHTTPGet(t *testing.T) {
	u := &stubUpdater{conf: Config{Bandwidth: 100 << 20, MetaIOPS: 1000}}
	h := HTTPHandler(func() Updater { return u }, "secret")
	w := doRequest(t, h, http.MethodGet, "", map[string]string{"X-JuiceFS-Token": "secret"})
	require.Equal(t, http.StatusOK, w.Code)
	var got Config
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, uint64(100<<20), got.Bandwidth)
	require.Equal(t, uint64(1000), got.MetaIOPS)
}

func TestHTTPUpdate(t *testing.T) {
	u := &stubUpdater{conf: Config{MetaIOPS: 50}}
	h := HTTPHandler(func() Updater { return u }, "secret")
	w := doRequest(t, h, http.MethodPut, `{"bandwidth":"100M","iops":200}`,
		map[string]string{"Authorization": "Bearer secret"})
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, u.got)
	require.NotNil(t, u.last.Bandwidth)
	require.Equal(t, uint64(100<<20), *u.last.Bandwidth)
	require.NotNil(t, u.last.IOPS)
	require.Equal(t, uint64(200), *u.last.IOPS)

	var got Config
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, uint64(100<<20), got.Bandwidth)
	require.Equal(t, uint64(200), got.IOPS)
	require.Equal(t, uint64(50), got.MetaIOPS, "untouched limits keep their value")
}

func TestHTTPBadBody(t *testing.T) {
	h := HTTPHandler(func() Updater { return &stubUpdater{} }, "secret")
	w := doRequest(t, h, http.MethodPut, `{"unknown":1}`, map[string]string{"Authorization": "Bearer secret"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	h := HTTPHandler(func() Updater { return &stubUpdater{} }, "secret")
	w := doRequest(t, h, http.MethodDelete, "", map[string]string{"X-JuiceFS-Token": "secret"})
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	require.Equal(t, "GET, PUT, POST", w.Header().Get("Allow"))
}

func TestHTTPNotReady(t *testing.T) {
	h := HTTPHandler(func() Updater { return nil }, "secret")
	w := doRequest(t, h, http.MethodGet, "", map[string]string{"X-JuiceFS-Token": "secret"})
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"100M", 100 << 20, true},
		{"1G", 1 << 30, true},
		{"1.5G", 3 << 29, true},
		{"104857600", 100 << 20, true}, // no suffix is bytes/s
		{"100", 100, true},
		{"1K", 1 << 10, true},
		{"0", 0, true},
		{"10X", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := ParseRate(c.in)
		if c.ok {
			require.NoError(t, err, c.in)
			require.Equal(t, c.want, got, c.in)
		} else {
			require.Error(t, err, c.in)
		}
	}
}

func TestUpdateUnmarshal(t *testing.T) {
	var u Update
	require.NoError(t, json.Unmarshal([]byte(`{"bandwidth":"100M","metaIOPS":1000}`), &u))
	require.NotNil(t, u.Bandwidth)
	require.Equal(t, uint64(100<<20), *u.Bandwidth)
	require.NotNil(t, u.MetaIOPS)
	require.Equal(t, uint64(1000), *u.MetaIOPS)
	require.Nil(t, u.IOPS)

	require.Error(t, json.Unmarshal([]byte(`{"bogus":1}`), &u))
}

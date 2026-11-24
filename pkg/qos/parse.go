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
	"fmt"
	"strconv"
	"strings"
)

// ParseRate converts a human readable rate such as "100M" or "1.5G" to bytes
// per second. A value without a unit suffix is interpreted as bytes per
// second, matching a bare JSON number. Fractions are accepted ("1.5G").
func ParseRate(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty rate")
	}
	unit := byte('B')
	num := s
	if last := s[len(s)-1]; last < '0' || last > '9' {
		unit = last
		num = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid rate %q", s)
	}
	var shift uint
	switch unit {
	case 'B', 'b':
		shift = 0
	case 'k', 'K':
		shift = 10
	case 'm', 'M':
		shift = 20
	case 'g', 'G':
		shift = 30
	case 't', 'T':
		shift = 40
	case 'p', 'P':
		shift = 50
	case 'e', 'E':
		shift = 60
	default:
		return 0, fmt.Errorf("invalid unit %q in rate %q", string(unit), s)
	}
	return uint64(f * float64(uint64(1)<<shift)), nil
}

// parseRateJSON accepts either a number of bytes per second or a string with
// a unit suffix.
func parseRateJSON(raw json.RawMessage) (uint64, error) {
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("expect a number of bytes or a string like %q", "100M")
	}
	return ParseRate(s)
}

// UnmarshalJSON accepts the wire format of Update: every field is optional, a
// bandwidth may be a number of bytes per second or a string with a unit
// suffix ("100M"), and unknown fields are rejected. It is used by both the
// `.control` message and the HTTP API.
func (u *Update) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var upd Update
	for key, val := range raw {
		switch key {
		case "bandwidth", "readBandwidth", "writeBandwidth":
			rate, err := parseRateJSON(val)
			if err != nil {
				return fmt.Errorf("%s: %s", key, err)
			}
			switch key {
			case "bandwidth":
				upd.Bandwidth = &rate
			case "readBandwidth":
				upd.ReadBandwidth = &rate
			default:
				upd.WriteBandwidth = &rate
			}
		case "iops", "metaIOPS":
			var ops uint64
			if err := json.Unmarshal(val, &ops); err != nil {
				return fmt.Errorf("%s: %s", key, err)
			}
			if key == "iops" {
				upd.IOPS = &ops
			} else {
				upd.MetaIOPS = &ops
			}
		default:
			return fmt.Errorf("unknown field %q", key)
		}
	}
	*u = upd
	return nil
}

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
	"os"
	"sort"
	"strings"
)

// Dimension names used by limits, the lease protocol and the reference
// counter server. They are stable strings so that a client and a counter
// service built from different revisions can still talk to each other.
const (
	DimBandwidth      = "bandwidth"
	DimReadBandwidth  = "readBandwidth"
	DimWriteBandwidth = "writeBandwidth"
	DimIOPS           = "iops"
	DimReadIOPS       = "readIOPS"
	DimWriteIOPS      = "writeIOPS"
	DimMetaIOPS       = "metaIOPS"
)

// Meta operation classes. A class is charged in addition to the aggregate
// metaIOPS bucket when both are configured.
const (
	MetaLookup  = "lookup"
	MetaGetattr = "getattr"
	MetaCreate  = "create"
	MetaUnlink  = "unlink"
	MetaReaddir = "readdir"
	MetaStatfs  = "statfs"
)

// MetaDim returns the limit dimension of a metadata class.
func MetaDim(class string) string {
	if class == "" {
		return ""
	}
	return "meta." + class
}

// Limits is one set of QoS limits. Zero means "not configured".
type Limits struct {
	Bandwidth      uint64            `json:"bandwidth,omitempty"`
	ReadBandwidth  uint64            `json:"readBandwidth,omitempty"`
	WriteBandwidth uint64            `json:"writeBandwidth,omitempty"`
	IOPS           uint64            `json:"iops,omitempty"`
	ReadIOPS       uint64            `json:"readIOPS,omitempty"`
	WriteIOPS      uint64            `json:"writeIOPS,omitempty"`
	MetaIOPS       uint64            `json:"metaIOPS,omitempty"`
	MetaOps        map[string]uint64 `json:"metaOps,omitempty"`
}

// Enabled reports whether at least one dimension is limited.
func (l Limits) Enabled() bool {
	if l.Bandwidth > 0 || l.ReadBandwidth > 0 || l.WriteBandwidth > 0 ||
		l.IOPS > 0 || l.ReadIOPS > 0 || l.WriteIOPS > 0 || l.MetaIOPS > 0 {
		return true
	}
	for _, v := range l.MetaOps {
		if v > 0 {
			return true
		}
	}
	return false
}

// Rate returns the configured rate of dim, in units per second. Unknown or
// unconfigured dimensions return 0.
func (l Limits) Rate(dim string) uint64 {
	switch dim {
	case DimBandwidth:
		return l.Bandwidth
	case DimReadBandwidth:
		return l.ReadBandwidth
	case DimWriteBandwidth:
		return l.WriteBandwidth
	case DimIOPS:
		return l.IOPS
	case DimReadIOPS:
		return l.ReadIOPS
	case DimWriteIOPS:
		return l.WriteIOPS
	case DimMetaIOPS:
		return l.MetaIOPS
	}
	if strings.HasPrefix(dim, "meta.") {
		return l.MetaOps[strings.TrimPrefix(dim, "meta.")]
	}
	return 0
}

// Rule is a QoS rule attached to one directory inode.
type Rule struct {
	ID     string `json:"id,omitempty"`
	Path   string `json:"path,omitempty"`
	Ino    uint64 `json:"ino"`
	Limits Limits `json:"limits"`
}

// Policy is the policy snapshot of one tenant volume. It is produced by the
// control plane and consumed by every mount of the volume.
type Policy struct {
	Version uint64 `json:"version"`
	Tenant  string `json:"tenant"`
	Volume  string `json:"volume,omitempty"`
	// Epoch is bumped by the control plane when rules are re-bound to new
	// inodes (for example after a directory was recreated).
	Epoch uint64 `json:"epoch,omitempty"`
	Cap   Limits `json:"cap"`
	Rules []Rule `json:"rules"`

	byIno map[uint64]*Rule
}

// ParsePolicy decodes and validates a policy snapshot.
func ParsePolicy(data []byte) (*Policy, error) {
	p := new(Policy)
	if err := json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("parse QoS policy: %w", err)
	}
	if err := p.init(); err != nil {
		return nil, err
	}
	return p, nil
}

// LoadPolicyFile reads and parses a policy snapshot from path.
func LoadPolicyFile(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePolicy(data)
}

func (p *Policy) init() error {
	if p.Tenant == "" {
		return fmt.Errorf("QoS policy: tenant is required")
	}
	return p.Reindex()
}

// Reindex rebuilds the inode lookup table. A rule with path set and ino 0 is
// kept unresolved until the mount resolves it against its own namespace.
func (p *Policy) Reindex() error {
	if p.Tenant == "" {
		return fmt.Errorf("QoS policy: tenant is required")
	}
	p.byIno = make(map[uint64]*Rule, len(p.Rules))
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Ino == 0 {
			if r.Path == "" {
				return fmt.Errorf("QoS policy: rule %q has neither path nor inode", r.ID)
			}
			continue
		}
		if _, ok := p.byIno[r.Ino]; ok {
			return fmt.Errorf("QoS policy: duplicate rule for inode %d", r.Ino)
		}
		if !r.Limits.Enabled() {
			return fmt.Errorf("QoS policy: rule for inode %d has no limits", r.Ino)
		}
		p.byIno[r.Ino] = r
	}
	return nil
}

// ResolvePaths binds every rule that only carries a path to the inode the
// mount resolves it to. Rules whose path cannot be resolved are skipped with a
// warning: the directory may not exist yet, and the next reload will pick it up.
func (p *Policy) ResolvePaths(resolve func(path string) (uint64, error), warn func(format string, args ...interface{})) error {
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Ino != 0 || r.Path == "" {
			continue
		}
		ino, err := resolve(r.Path)
		if err != nil {
			if warn != nil {
				warn("QoS rule %s: cannot resolve path %s: %s", r.ID, r.Path, err)
			}
			continue
		}
		r.Ino = ino
	}
	return p.Reindex()
}

// Rule returns the rule attached to ino, or nil.
func (p *Policy) Rule(ino uint64) *Rule {
	if p == nil || p.byIno == nil {
		return nil
	}
	return p.byIno[ino]
}

// LimitsOf returns the limits of the rule attached to ino.
func (p *Policy) LimitsOf(ino uint64) *Limits {
	if r := p.Rule(ino); r != nil {
		return &r.Limits
	}
	return nil
}

// Rules returns the rules sorted by inode, which makes snapshots and test
// output stable.
func (p *Policy) SortedRules() []Rule {
	out := append([]Rule(nil), p.Rules...)
	sort.Slice(out, func(i, j int) bool { return out[i].Ino < out[j].Ino })
	return out
}

// ValidateBudget checks the admission rule of the policy tree for one
// dimension: direct children reservations must not exceed their parent.
// Because a snapshot does not carry parent pointers, callers must pass the
// parent of every rule; rules without a known parent are checked against the
// tenant cap. It is used by the control plane and by tests; a mount does not
// need it to enforce limits.
func (p *Policy) ValidateBudget(parentOf func(ino uint64) uint64, dim string) error {
	if p == nil {
		return nil
	}
	used := make(map[uint64]uint64, len(p.Rules))
	for i := range p.Rules {
		r := &p.Rules[i]
		rate := r.Limits.Rate(dim)
		if rate == 0 {
			continue
		}
		parent := parentOf(r.Ino)
		used[parent] += rate
	}
	for parent, sum := range used {
		limit := p.Cap.Rate(dim)
		if parent != 0 {
			if r := p.Rule(parent); r != nil {
				limit = r.Limits.Rate(dim)
			}
		}
		if limit > 0 && sum > limit {
			return fmt.Errorf("QoS policy: children of inode %d reserve %d %s, which exceeds the parent limit %d", parent, sum, dim, limit)
		}
	}
	return nil
}

// Op describes one operation that needs QoS tokens.
type Op struct {
	// Read selects the read direction of a data operation.
	Read bool
	// Bytes is the data size of a read or write. Zero with Meta == "" means
	// a server-side data operation (copy_file_range), which only consumes
	// IOPS.
	Bytes int64
	// Meta is the metadata class (MetaLookup, ...), empty for data.
	Meta string
}

// dimReq is one dimension that must be charged.
type dimReq struct {
	Dim string
	N   int64
}

// dimensionReqs returns the dimensions of op under l. A direction-specific
// limit replaces the shared one for that direction; metadata classes are
// charged in addition to metaIOPS.
func (l Limits) dimensionReqs(op Op) []dimReq {
	if op.Meta != "" {
		out := make([]dimReq, 0, 2)
		if r := l.Rate(MetaDim(op.Meta)); r > 0 {
			out = append(out, dimReq{MetaDim(op.Meta), 1})
		}
		if l.MetaIOPS > 0 {
			out = append(out, dimReq{DimMetaIOPS, 1})
		}
		return out
	}
	out := make([]dimReq, 0, 2)
	if op.Bytes <= 0 {
		// server-side data operation: only IOPS
		if op.Read && l.ReadIOPS > 0 {
			out = append(out, dimReq{DimReadIOPS, 1})
		} else if !op.Read && l.WriteIOPS > 0 {
			out = append(out, dimReq{DimWriteIOPS, 1})
		} else if l.IOPS > 0 {
			out = append(out, dimReq{DimIOPS, 1})
		}
		return out
	}
	if op.Read {
		if l.ReadIOPS > 0 {
			out = append(out, dimReq{DimReadIOPS, 1})
		} else if l.IOPS > 0 {
			out = append(out, dimReq{DimIOPS, 1})
		}
		if l.ReadBandwidth > 0 {
			out = append(out, dimReq{DimReadBandwidth, op.Bytes})
		} else if l.Bandwidth > 0 {
			out = append(out, dimReq{DimBandwidth, op.Bytes})
		}
	} else {
		if l.WriteIOPS > 0 {
			out = append(out, dimReq{DimWriteIOPS, 1})
		} else if l.IOPS > 0 {
			out = append(out, dimReq{DimIOPS, 1})
		}
		if l.WriteBandwidth > 0 {
			out = append(out, dimReq{DimWriteBandwidth, op.Bytes})
		} else if l.Bandwidth > 0 {
			out = append(out, dimReq{DimBandwidth, op.Bytes})
		}
	}
	return out
}

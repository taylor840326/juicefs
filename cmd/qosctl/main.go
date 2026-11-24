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

// Command qosctl manages the multi-tenant QoS policies stored in qosd.
//
// It is the Go equivalent of juicefs-multitenant-qos-test/qos.sh: every
// command talks to the qosd HTTP API, changes the policy document of one
// volume and then asks the local mounts of that volume to fetch it
// immediately (SIGUSR1). Mounts also poll the service every 3 seconds, so a
// missed signal still converges.
//
//	qosctl show                       print the policy document of the volume
//	qosctl list                       print the tenant cap and directory rules
//	qosctl cap --bandwidth 100M ...   update the tenant cap
//	qosctl set /proj-a --bandwidth 20M --meta-iops 50
//	qosctl del /proj-a
//	qosctl reload                     make every local mount fetch the policy
//	qosctl check [--script PATH]      run the acceptance test script
//
// Global flags (accepted by every command):
//
//	--server URL    qosd base URL (env JFS_QOSD_URL, default http://127.0.0.1:19567)
//	--token TOKEN   bearer token (env JFS_QOSD_TOKEN)
//	--volume NAME   volume name (env JFS_QOS_VOLUME); when empty the service
//	                must hold exactly one policy
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/qos"
	"github.com/juicedata/juicefs/pkg/version"
)

const (
	defaultServer  = "http://127.0.0.1:19567"
	requestTimeout = 10 * time.Second
)

var (
	errUsage = errors.New("usage")
)

// globals holds the flags shared by every subcommand.
type globals struct {
	server string
	token  string
	volume string
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.server, "server", envOr("JFS_QOSD_URL", defaultServer), "qosd base URL")
	fs.StringVar(&g.token, "token", os.Getenv("JFS_QOSD_TOKEN"), "qosd bearer token")
	fs.StringVar(&g.volume, "volume", os.Getenv("JFS_QOS_VOLUME"), "volume name (default: the only policy in the service)")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// api is the small qosd client used by every command.
type api struct {
	base  string
	token string
	hc    *http.Client
}

func newAPI(g *globals) *api {
	return &api{
		base:  strings.TrimRight(g.server, "/"),
		token: g.token,
		hc:    &http.Client{Timeout: requestTimeout},
	}
}

func (a *api) do(method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, fmt.Errorf("qosd returned %s: %s", resp.Status, msg)
	}
	return data, nil
}

func (a *api) getPolicy(volume string) (*qos.Policy, error) {
	data, err := a.do(http.MethodGet, "/v1/policies/"+volume, nil)
	if err != nil {
		return nil, err
	}
	return qos.ParsePolicy(data)
}

func (a *api) putPolicy(volume string, p *qos.Policy) (*qos.Policy, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	data, err := a.do(http.MethodPut, "/v1/policies/"+volume, body)
	if err != nil {
		return nil, err
	}
	return qos.ParsePolicy(data)
}

func (a *api) listPolicies() ([]*qos.Policy, error) {
	data, err := a.do(http.MethodGet, "/v1/policies", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Policies []json.RawMessage `json:"policies"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]*qos.Policy, 0, len(resp.Policies))
	for _, raw := range resp.Policies {
		p, err := qos.ParsePolicy(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// resolveVolume returns the volume to operate on. Without --volume the service
// must manage exactly one policy, which keeps the command convenient in the
// single-tenant test setups while staying unambiguous in production.
func resolveVolume(a *api, g *globals) (string, error) {
	if g.volume != "" {
		return g.volume, nil
	}
	policies, err := a.listPolicies()
	if err != nil {
		return "", err
	}
	if len(policies) != 1 {
		names := make([]string, 0, len(policies))
		for _, p := range policies {
			names = append(names, p.Volume)
		}
		sort.Strings(names)
		return "", fmt.Errorf("cannot choose a volume (%d policies: %s); pass --volume", len(policies), strings.Join(names, ", "))
	}
	return policies[0].Volume, nil
}

// --- limit flags -----------------------------------------------------------

// limitFlags binds the nine bandwidth/IOPS options and the six metadata class
// options. All values are parsed with qos.ParseRate so "20M" and "500" are
// both accepted; 0 clears a limit explicitly.
type limitFlags struct {
	fs   *flag.FlagSet
	vals map[string]*string
	meta map[string]*string
}

var limNames = []string{
	"bandwidth", "read-bandwidth", "write-bandwidth",
	"iops", "read-iops", "write-iops", "meta-iops",
}

var metaNames = []string{
	"meta-lookup", "meta-getattr", "meta-create",
	"meta-unlink", "meta-readdir", "meta-statfs",
}

var limFields = map[string]string{
	"bandwidth":       qos.DimBandwidth,
	"read-bandwidth":  qos.DimReadBandwidth,
	"write-bandwidth": qos.DimWriteBandwidth,
	"iops":            qos.DimIOPS,
	"read-iops":       qos.DimReadIOPS,
	"write-iops":      qos.DimWriteIOPS,
	"meta-iops":       qos.DimMetaIOPS,
}

var metaFields = map[string]string{
	"meta-lookup":  qos.MetaLookup,
	"meta-getattr": qos.MetaGetattr,
	"meta-create":  qos.MetaCreate,
	"meta-unlink":  qos.MetaUnlink,
	"meta-readdir": qos.MetaReaddir,
	"meta-statfs":  qos.MetaStatfs,
}

func newLimitFlags(fs *flag.FlagSet) *limitFlags {
	l := &limitFlags{fs: fs, vals: map[string]*string{}, meta: map[string]*string{}}
	for _, name := range limNames {
		v := new(string)
		l.vals[name] = v
		fs.StringVar(v, name, "", "limit, e.g. 20M or 0 to clear")
	}
	for _, name := range metaNames {
		v := new(string)
		l.meta[name] = v
		fs.StringVar(v, name, "", "per-class metadata ops/s, e.g. 100")
	}
	return l
}

// apply merges the flags that were actually set into lim. Metadata classes are
// merged individually so that "set dir --meta-lookup 100" does not drop the
// other classes of the same rule.
func (l *limitFlags) apply(lim *qos.Limits) error {
	set := map[string]bool{}
	l.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for name, dim := range limFields {
		if !set[name] {
			continue
		}
		v, err := qos.ParseRate(*l.vals[name])
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		switch dim {
		case qos.DimBandwidth:
			lim.Bandwidth = v
		case qos.DimReadBandwidth:
			lim.ReadBandwidth = v
		case qos.DimWriteBandwidth:
			lim.WriteBandwidth = v
		case qos.DimIOPS:
			lim.IOPS = v
		case qos.DimReadIOPS:
			lim.ReadIOPS = v
		case qos.DimWriteIOPS:
			lim.WriteIOPS = v
		case qos.DimMetaIOPS:
			lim.MetaIOPS = v
		}
	}
	for name, class := range metaFields {
		if !set[name] {
			continue
		}
		v, err := qos.ParseRate(*l.meta[name])
		if err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
		if lim.MetaOps == nil {
			lim.MetaOps = map[string]uint64{}
		}
		lim.MetaOps[class] = v
	}
	return nil
}

// --- output ---------------------------------------------------------------

func fmtRate(v uint64) string {
	if v == 0 {
		return "-"
	}
	for _, u := range []struct {
		name string
		v    uint64
	}{{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if v >= u.v && v%u.v == 0 {
			return fmt.Sprintf("%d%s", v/u.v, u.name)
		}
	}
	return strconv.FormatUint(v, 10)
}

func printSummary(p *qos.Policy) {
	fmt.Printf("tenant %s  volume %s  version %d\n", p.Tenant, p.Volume, p.Version)
	fmt.Printf("  %-24s bandwidth=%s iops=%s metaIOPS=%s\n", "cap", fmtRate(p.Cap.Bandwidth), dash(p.Cap.IOPS), dash(p.Cap.MetaIOPS))
	rules := append([]qos.Rule(nil), p.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].Path < rules[j].Path })
	for _, r := range rules {
		l := r.Limits
		extra := ""
		if l.ReadBandwidth > 0 {
			extra += " read=" + fmtRate(l.ReadBandwidth)
		}
		if l.WriteBandwidth > 0 {
			extra += " write=" + fmtRate(l.WriteBandwidth)
		}
		if l.ReadIOPS > 0 {
			extra += fmt.Sprintf(" readIOPS=%d", l.ReadIOPS)
		}
		if l.WriteIOPS > 0 {
			extra += fmt.Sprintf(" writeIOPS=%d", l.WriteIOPS)
		}
		if len(l.MetaOps) > 0 {
			keys := make([]string, 0, len(l.MetaOps))
			for k := range l.MetaOps {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s:%d", k, l.MetaOps[k]))
			}
			extra += " metaOps=" + strings.Join(parts, ",")
		}
		fmt.Printf("  %-24s bandwidth=%s iops=%s metaIOPS=%s%s\n",
			"/"+strings.TrimPrefix(r.Path, "/"), fmtRate(l.Bandwidth), dash(l.IOPS), dash(l.MetaIOPS), extra)
	}
}

func dash(v uint64) string {
	if v == 0 {
		return "-"
	}
	return strconv.FormatUint(v, 10)
}

// --- local mounts ---------------------------------------------------------

// localMounts returns the mount point -> volume name of every JuiceFS mount of
// this host, by parsing the mount table ("JuiceFS:name on /path ...").
func localMounts() map[string]string {
	out := map[string]string{}
	data, err := exec.Command("mount").Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "JuiceFS:") {
			continue
		}
		rest := strings.TrimSpace(line)
		if i := strings.Index(rest, " on "); i >= 0 {
			name := strings.TrimPrefix(rest[:i], "JuiceFS:")
			tail := strings.Fields(rest[i+4:])
			if len(tail) > 0 {
				out[tail[0]] = name
			}
		}
	}
	return out
}

// mountDaemonPids returns the pids of the FUSE daemon processes serving mp.
// The supervisor and the daemon share a command line, so the daemon is the
// process whose parent is also a matching mount process (same rule qos.sh
// uses); sending SIGUSR1 to the supervisor would terminate it.
func mountDaemonPids(mp string) []int {
	type proc struct {
		pid, ppid int
		args      []string
	}
	procs := make([]proc, 0, 512)
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil
		}
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			if err != nil || len(raw) == 0 {
				continue
			}
			args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
			stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			ppid := 0
			if fields := strings.Fields(string(stat)); len(fields) > 3 {
				ppid, _ = strconv.Atoi(fields[3])
			}
			procs = append(procs, proc{pid: pid, ppid: ppid, args: args})
		}
	} else {
		out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			pid, err1 := strconv.Atoi(fields[0])
			ppid, err2 := strconv.Atoi(fields[1])
			if err1 != nil || err2 != nil {
				continue
			}
			procs = append(procs, proc{pid: pid, ppid: ppid, args: fields[2:]})
		}
	}

	match := func(p proc) bool {
		if len(p.args) < 2 {
			return false
		}
		// The binary may live anywhere; only its base name is stable. The
		// mount point is one of the arguments but not necessarily the last
		// one: "juicefs mount -d META /mnt --qos-server ... --log ...".
		base := filepath.Base(p.args[0])
		if !strings.Contains(strings.ToLower(base), "juicefs") {
			return false
		}
		hasMount := false
		for _, a := range p.args[1:minInt(len(p.args), 8)] {
			if a == "mount" {
				hasMount = true
				break
			}
		}
		if !hasMount {
			return false
		}
		for _, a := range p.args {
			if a == mp {
				return true
			}
		}
		return false
	}

	matched := map[int]bool{}
	for _, p := range procs {
		if match(p) {
			matched[p.pid] = true
		}
	}
	pids := make([]int, 0, len(matched))
	for _, p := range procs {
		if matched[p.pid] && matched[p.ppid] {
			pids = append(pids, p.pid)
		}
	}
	sort.Ints(pids)
	return pids
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// reloadMounts asks every local mount to fetch the policy immediately. The 3s
// poll of the mount is the fallback, so reloading is an optimization and its
// failures are reported but not fatal.
func reloadMounts(volume string) {
	mounts := localMounts()
	if len(mounts) == 0 {
		fmt.Println("  [INFO] no JuiceFS mount on this host")
		return
	}
	sent := 0
	for mp, vol := range mounts {
		if volume != "" && vol != volume {
			continue
		}
		pids := mountDaemonPids(mp)
		for _, pid := range pids {
			if err := syscall.Kill(pid, syscall.SIGUSR1); err == nil {
				fmt.Printf("  [INFO] SIGUSR1 -> %d (%s)\n", pid, mp)
				sent++
			}
		}
	}
	if sent == 0 {
		fmt.Printf("  [INFO] no running mount process for volume %q\n", volume)
	} else {
		fmt.Printf("  [INFO] policy reload signalled to %d mount process(es); mounts also poll every 3s\n", sent)
	}
	time.Sleep(300 * time.Millisecond)
}

// --- commands -------------------------------------------------------------

func usage() {
	fmt.Fprintf(os.Stderr, `usage: qosctl [global flags] COMMAND [ARGS]

Commands:
  show                  print the policy document of the volume
  list                  print the tenant cap and directory rules
  cap [limits...]       update the tenant cap
  set PATH [limits...]  create or update a directory rule
  del PATH              delete a directory rule
  reload                ask every local mount to fetch the policy now
  check [--script P]    run the acceptance test script (default ./test.sh)
  version               print the qosctl version

Global flags:
  --server URL    qosd base URL (env JFS_QOSD_URL, default %s)
  --token TOKEN   bearer token (env JFS_QOSD_TOKEN)
  --volume NAME   volume name (env JFS_QOS_VOLUME)

Limits (bytes/s; suffixes K/M/G/T/P are powers of 1024; 0 clears):
  --bandwidth --read-bandwidth --write-bandwidth
  --iops --read-iops --write-iops --meta-iops
  --meta-lookup --meta-getattr --meta-create --meta-unlink
  --meta-readdir --meta-statfs

Examples:
  qosctl list
  qosctl cap --bandwidth 100M --iops 5000 --meta-iops 2000
  qosctl set /proj-a --bandwidth 20M --meta-iops 50
  qosctl del /proj-a
`, defaultServer)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "help", "-h", "--help":
		usage()
	case "version", "--version", "-v":
		fmt.Printf("qosctl version %s\n", version.Version())
	case "show":
		cmdShow(args)
	case "list", "get":
		cmdList(args)
	case "cap":
		cmdEdit(args, "cap")
	case "set":
		cmdEdit(args, "set")
	case "del", "delete":
		cmdEdit(args, "del")
	case "reload":
		cmdReload(args)
	case "check", "test":
		cmdCheck(args)
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func newFlagSet(name string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	return fs
}

func cmdShow(args []string) {
	var g globals
	fs := newFlagSet("show", &g)
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	a := newAPI(&g)
	vol, err := resolveVolume(a, &g)
	if err != nil {
		fail(err)
	}
	p, err := a.getPolicy(vol)
	if err != nil {
		fail(err)
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	fmt.Printf("policy: %s/v1/policies/%s\n", a.base, vol)
	fmt.Println(string(data))
}

func cmdList(args []string) {
	var g globals
	fs := newFlagSet("list", &g)
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	a := newAPI(&g)
	vol, err := resolveVolume(a, &g)
	if err != nil {
		fail(err)
	}
	p, err := a.getPolicy(vol)
	if err != nil {
		fail(err)
	}
	printSummary(p)
}

func cmdEdit(args []string, op string) {
	var g globals
	fs := newFlagSet(op, &g)
	limits := newLimitFlags(fs)
	script := ""
	if op == "check" {
		_ = script
	}
	var target string
	rest := args
	if op == "set" || op == "del" {
		// PATH is the first positional argument; flags may come before it.
		pos := -1
		for i, a := range rest {
			if !strings.HasPrefix(a, "-") {
				pos = i
				break
			}
			// Skip the value of a flag when it is spelled as "--flag value".
			if !strings.Contains(a, "=") && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
				pos = -1
			}
		}
		if pos < 0 {
			fail(fmt.Errorf("usage: qosctl %s PATH [limits...]", op))
		}
		target = rest[pos]
		rest = append(append([]string{}, rest[:pos]...), rest[pos+1:]...)
	}
	if err := fs.Parse(rest); err != nil {
		fail(err)
	}
	if fs.NArg() > 0 {
		fail(fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	if op == "del" && target == "" {
		fail(errors.New("usage: qosctl del PATH"))
	}

	a := newAPI(&g)
	vol, err := resolveVolume(a, &g)
	if err != nil {
		fail(err)
	}
	p, err := a.getPolicy(vol)
	if err != nil {
		fail(err)
	}
	path := normPath(target)
	switch op {
	case "cap":
		if err := limits.apply(&p.Cap); err != nil {
			fail(err)
		}
	case "set":
		rule := findRule(p, path)
		if rule == nil {
			p.Rules = append(p.Rules, qos.Rule{ID: ruleID(path), Path: path})
			rule = &p.Rules[len(p.Rules)-1]
		}
		if err := limits.apply(&rule.Limits); err != nil {
			fail(err)
		}
	case "del":
		if findRule(p, path) == nil {
			fail(fmt.Errorf("no rule for path %q", path))
		}
		kept := p.Rules[:0]
		for _, r := range p.Rules {
			if normPath(r.Path) != path {
				kept = append(kept, r)
			}
		}
		p.Rules = kept
	}

	stored, err := a.putPolicy(vol, p)
	if err != nil {
		fail(err)
	}
	fmt.Printf("  [INFO] policy for %s saved as version %d\n", vol, stored.Version)
	reloadMounts(vol)
	printSummary(stored)
}

func cmdReload(args []string) {
	var g globals
	fs := newFlagSet("reload", &g)
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	a := newAPI(&g)
	vol := g.volume
	if vol == "" {
		v, err := resolveVolume(a, &g)
		if err != nil {
			fail(err)
		}
		vol = v
	}
	reloadMounts(vol)
}

func cmdCheck(args []string) {
	var g globals
	fs := newFlagSet("check", &g)
	script := fs.String("script", "test.sh", "acceptance test script to run")
	if err := fs.Parse(args); err != nil {
		fail(err)
	}
	cmd := exec.Command(*script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s failed: %v\n", *script, err)
		os.Exit(1)
	}
}

func normPath(p string) string {
	return strings.Trim(strings.TrimSpace(p), "/")
}

func ruleID(path string) string {
	if path == "" {
		return "rule-root"
	}
	return "rule-" + strings.ReplaceAll(path, "/", "-")
}

func findRule(p *qos.Policy, path string) *qos.Rule {
	for i := range p.Rules {
		if normPath(p.Rules[i].Path) == path {
			return &p.Rules[i]
		}
	}
	return nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	os.Exit(1)
}

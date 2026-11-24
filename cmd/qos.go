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

package cmd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/qos"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/urfave/cli/v2"
)

func cmdQoS() *cli.Command {
	return &cli.Command{
		Name:      "qos",
		Action:    qosAction,
		Category:  "TOOL",
		Usage:     "Inspect or update the client-side QoS limits of a mounted filesystem",
		ArgsUsage: "MOUNTPOINT",
		Description: `
Inspect or update the client-side QoS limits (bandwidth, data IOPS and metadata
IOPS) of a mounted JuiceFS filesystem without unmounting it. The change takes
effect immediately and only affects the mount process serving MOUNTPOINT.

The limits of a mount process can also be changed by sending SIGUSR1 to it when
it was mounted with --qos-config: the file is re-read and applied.

Examples:
# Show the current limits
$ juicefs qos /mnt/jfs

# Limit the total bandwidth to 100 MiB/s and metadata operations to 1000/s
$ juicefs qos /mnt/jfs --bandwidth 100M --meta-iops 1000

# Limit reads and writes separately
$ juicefs qos /mnt/jfs --read-bandwidth 200M --write-bandwidth 50M

# Remove some limits (0 clears a limit, omitted options keep their value)
$ juicefs qos /mnt/jfs --bandwidth 0 --iops 0

# Print the effective limits in JSON
$ juicefs qos /mnt/jfs --json`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "bandwidth",
				Usage: "total bandwidth limit, e.g. 100M, 1G; 0 removes the limit",
			},
			&cli.StringFlag{
				Name:  "read-bandwidth",
				Usage: "bandwidth limit for reading, overrides --bandwidth in the read direction; 0 removes the limit",
			},
			&cli.StringFlag{
				Name:  "write-bandwidth",
				Usage: "bandwidth limit for writing, overrides --bandwidth in the write direction; 0 removes the limit",
			},
			&cli.Int64Flag{
				Name:  "iops",
				Usage: "maximum number of data operations (read/write/copy) per second; 0 removes the limit",
			},
			&cli.Int64Flag{
				Name:  "meta-iops",
				Usage: "maximum number of metadata operations per second; 0 removes the limit",
			},
			&cli.BoolFlag{
				Name:  "json",
				Usage: "print the effective limits in JSON",
			},
		},
	}
}

// qosUpdate builds a partial update from the command line flags. Only the flags
// explicitly set are included, so that the other limits keep their value.
func qosUpdate(c *cli.Context) qos.Update {
	var u qos.Update
	if c.IsSet("bandwidth") {
		v := utils.ParseBytesStr("bandwidth", c.String("bandwidth"), 'M')
		u.Bandwidth = &v
	}
	if c.IsSet("read-bandwidth") {
		v := utils.ParseBytesStr("read-bandwidth", c.String("read-bandwidth"), 'M')
		u.ReadBandwidth = &v
	}
	if c.IsSet("write-bandwidth") {
		v := utils.ParseBytesStr("write-bandwidth", c.String("write-bandwidth"), 'M')
		u.WriteBandwidth = &v
	}
	if c.IsSet("iops") {
		v := c.Int64("iops")
		if v < 0 {
			logger.Fatalf("iops should not be negative: %d", v)
		}
		uv := uint64(v)
		u.IOPS = &uv
	}
	if c.IsSet("meta-iops") {
		v := c.Int64("meta-iops")
		if v < 0 {
			logger.Fatalf("meta-iops should not be negative: %d", v)
		}
		uv := uint64(v)
		u.MetaIOPS = &uv
	}
	return u
}

func qosAction(c *cli.Context) error {
	setup0(c, 1, 0)
	mp := c.Args().First()
	if mp == "" {
		logger.Fatalf("MOUNTPOINT is required")
	}
	cf, err := openController(mp)
	if err != nil {
		logger.Fatalf("open control file of %s: %s", mp, err)
	}
	defer cf.Close()

	body, _ := json.Marshal(qosUpdate(c)) // "{}" means "just show the current limits"
	wb := utils.NewBuffer(uint32(4 + 4 + len(body)))
	wb.Put32(meta.QoS)
	wb.Put32(uint32(len(body)))
	wb.Put(body)
	if _, err = cf.Write(wb.Bytes()); err != nil {
		logger.Fatalf("send QoS message: %s", err)
	}

	data, errno := readQoSResponse(cf)
	if errno != 0 {
		logger.Fatalf("update QoS: %s", errno)
	}
	var conf qos.Config
	if err = json.Unmarshal(data, &conf); err != nil {
		logger.Fatalf("parse QoS response: %s", err)
	}
	if c.Bool("json") {
		out, _ := json.MarshalIndent(&conf, "", "  ")
		fmt.Println(string(out))
	} else {
		printQoS(&conf)
	}
	return nil
}

// readQoSResponse reads one CDATA frame followed by the trailing errno byte from
// the control file. It tolerates the response arriving in several reads.
func readQoSResponse(cf *os.File) ([]byte, syscall.Errno) {
	var resp = make([]byte, 2<<16)
	var buf []byte
	for {
		n := readControl(cf, resp)
		buf = append(buf, resp[:n]...)
		if len(buf) >= 5 && buf[0] == meta.CDATA {
			size := int(binary.BigEndian.Uint32(buf[1:5]))
			if len(buf) >= 5+size+1 {
				return buf[5 : 5+size], syscall.Errno(buf[5+size])
			}
		}
	}
}

// printQoS prints the limits in a human readable form.
func printQoS(conf *qos.Config) {
	fmt.Printf("       bandwidth: %s/s\n", utils.FormatBytes(conf.Bandwidth))
	fmt.Printf("  read bandwidth: %s/s\n", utils.FormatBytes(conf.ReadBandwidth))
	fmt.Printf(" write bandwidth: %s/s\n", utils.FormatBytes(conf.WriteBandwidth))
	fmt.Printf("            iops: %d\n", conf.IOPS)
	fmt.Printf("       meta iops: %d\n", conf.MetaIOPS)
}

// reloadQoS re-reads the --qos-config file and applies it to a running mount.
// It is triggered by SIGUSR1, which lets operators tune the limits of a mount
// without restarting it (for example after a ConfigMap update).
func reloadQoS(v *vfs.VFS) {
	if qosConfigFile == "" && qosPolicyFile == "" {
		logger.Warnf("received SIGUSR1 but neither --qos-config nor --qos-policy-file is set, ignore")
		return
	}
	if qosConfigFile != "" {
		applyQoSConfig(v, qosConfigFile)
	}
	if qosPolicyFile != "" {
		applyQoSPolicy(v, qosPolicyFile)
	}
}

// applyQoSConfig reloads the client wide limits (--qos-config). It keeps the
// running limits untouched when the file is missing or malformed.
func applyQoSConfig(v *vfs.VFS, path string) bool {
	u, err := loadQoSFile(path)
	if err != nil {
		logger.Warnf("reload QoS config %s: %s", path, err)
		return false
	}
	if u == nil {
		return false
	}
	v.UpdateQoS(*u)
	logger.Infof("QoS config reloaded from %s", path)
	return true
}

// applyQoSPolicy reloads a multi-tenant policy file (--qos-policy-file) and
// resolves the path based rules against the mounted volume.
func applyQoSPolicy(v *vfs.VFS, path string) bool {
	p, err := qos.LoadPolicyFile(path)
	if err != nil {
		logger.Warnf("reload QoS policy %s: %s", path, err)
		return false
	}
	if err := p.ResolvePaths(func(path string) (uint64, error) {
		return lookupMetaPath(v.Meta, path)
	}, logger.Warnf); err != nil {
		logger.Warnf("resolve QoS policy paths: %s", err)
	}
	v.SetQoSPolicy(p)
	qosPolicy = p
	logger.Infof("QoS policy reloaded from %s (version=%d rules=%d)", path, p.Version, len(p.Rules))
	return true
}

// qosWatchInterval controls how often the mount process checks the QoS files
// for changes. A 3s poll keeps the client within a few seconds of the control
// plane without requiring an operator to send SIGUSR1 after every edit.
const qosWatchInterval = 3 * time.Second

// watchQoS polls --qos-config and --qos-policy-file, and reloads whichever one
// changed. Files are compared by content hash, so a touched but unchanged file
// does not bump the policy epoch and does not disturb running I/O.
func watchQoS(v *vfs.VFS) {
	files := []string{qosConfigFile, qosPolicyFile}
	last := make(map[string][sha256.Size]byte, 2)
	for _, p := range files {
		if p == "" {
			continue
		}
		if h, ok := qosFileHash(p); ok {
			last[p] = h
		}
	}
	ticker := time.NewTicker(qosWatchInterval)
	defer ticker.Stop()
	for range ticker.C {
		for _, p := range files {
			if p == "" {
				continue
			}
			h, ok := qosFileHash(p)
			if !ok || h == last[p] {
				continue
			}
			last[p] = h
			logger.Debugf("QoS file %s changed, reloading", p)
			if p == qosConfigFile {
				applyQoSConfig(v, p)
			} else {
				applyQoSPolicy(v, p)
			}
		}
	}
}

func qosFileHash(path string) ([sha256.Size]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(data), true
}

// loadQoSFile reads a JSON file with QoS limits. The bandwidth fields accept
// either a number of bytes per second or a human readable value such as "100M".
// It returns nil when path is empty, so the file is optional.
func loadQoSFile(path string) (*qos.Update, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	u := &qos.Update{}
	if err = json.Unmarshal(data, u); err != nil {
		return nil, err
	}
	return u, nil
}

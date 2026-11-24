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
	"net/http"
	"os"
	"time"

	"github.com/juicedata/juicefs/pkg/qos"
	"github.com/urfave/cli/v2"
)

func cmdQoSD() *cli.Command {
	return &cli.Command{
		Name:      "qosd",
		Category:  "ADMIN",
		Usage:     "Run the QoS counter service used to share a tenant budget between mounts",
		ArgsUsage: "",
		Description: `
The counter service keeps one bucket per (tenant, inode, dimension) and only
grants tokens that all ancestor buckets can still afford. Every mount points at
the same service (--qos-lease-url), so the sum of the limits of all subdirectory
rules can never exceed the tenant cap, even when several clients are mounted.

Example:
  $ juicefs qosd --listen 0.0.0.0:19567 --token "$JFS_QOSD_TOKEN"
  $ juicefs mount ... --qos-policy-file policy.json \
      --qos-lease-url http://qosd.example.com:19567 --qos-lease-token "$JFS_QOSD_TOKEN"
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "listen",
				Value: "127.0.0.1:19567",
				Usage: "address to listen on",
			},
			&cli.StringFlag{
				Name:  "token",
				Usage: "bearer token required by mounts (can also be set with JFS_QOSD_TOKEN)",
			},
		},
		Action: qosdAction,
	}
}

func qosdAction(c *cli.Context) error {
	setup(c, 0)

	listen := c.String("listen")
	token := c.String("token")
	if token == "" {
		token = os.Getenv("JFS_QOSD_TOKEN")
	}
	counter := qos.NewCounter()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			logger.Infof("qosd: %s", counter.Stats())
		}
	}()
	srv := &http.Server{
		Addr:    listen,
		Handler: counter.Handler(token),
	}
	if token == "" {
		logger.Warnf("QoS counter service is running without a token; restrict the listen address to a trusted network")
	} else {
		logger.Infof("qosd: bearer token authentication enabled")
	}
	logger.Infof("QoS counter service is listening on %s", listen)
	return srv.ListenAndServe()
}

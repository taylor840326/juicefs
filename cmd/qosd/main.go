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

// Command qosd is the standalone QoS counter service.
//
// It is the same service as `juicefs qosd`, but built as its own small
// executable: it only depends on the token-bucket counter, so it can be
// deployed as a sidecar or a shared service next to the mounts without
// pulling in FUSE, the metadata engines or any object storage client.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/juicedata/juicefs/pkg/qos"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/version"
)

var logger = utils.GetLogger("qosd")

func setLogLevel(level string) error {
	var l logrus.Level
	switch level {
	case "info":
		l = logrus.InfoLevel
	case "warn":
		l = logrus.WarnLevel
	case "debug":
		l = logrus.DebugLevel
	default:
		return fmt.Errorf("invalid log level %q: must be one of info, warn, debug", level)
	}
	utils.SetLogLevel(l)
	return nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:19567", "address to listen on")
	token := flag.String("token", "", "bearer token required by mounts (can also be set with JFS_QOSD_TOKEN)")
	logLevel := flag.String("log-level", "info", "log level: info, warn, debug")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("juicefs-qosd version %s\n", version.Version())
		return
	}
	if err := setLogLevel(*logLevel); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}
	if *token == "" {
		*token = os.Getenv("JFS_QOSD_TOKEN")
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Fatalf("listen on %s: %s", *listen, err)
	}
	if *token == "" {
		logger.Warnf("QoS counter service is running without a token; restrict the listen address to a trusted network")
	}
	srv := &http.Server{
		Handler:           qos.NewCounter().Handler(*token),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Infof("juicefs-qosd %s listening on %s (log-level=%s)", version.Version(), *listen, *logLevel)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("serve: %s", err)
	}
}

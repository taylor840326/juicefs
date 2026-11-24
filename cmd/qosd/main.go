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

// Command qosd is the standalone multi-tenant QoS control service.
//
// It stores the policy of every tenant volume in Redis (create, read, update
// and delete through the HTTP API), serves the policy snapshots to the mounts,
// and keeps the shared token buckets used to enforce the tenant budget across
// all of them. It only depends on pkg/qos, so it can be deployed next to the
// mounts without pulling in FUSE, the metadata engines or object storage.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	redisAddr := flag.String("redis", "", "Redis address of the policy store, e.g. redis://127.0.0.1:6379/0 or 127.0.0.1:6379")
	token := flag.String("token", "", "bearer token required by clients (can also be set with JFS_QOSD_TOKEN)")
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
	if *redisAddr == "" {
		*redisAddr = os.Getenv("JFS_QOSD_REDIS")
	}
	if *redisAddr == "" {
		fmt.Fprintf(os.Stderr, "ERROR: --redis is required (or set JFS_QOSD_REDIS); qosd stores policies in Redis\n")
		os.Exit(2)
	}

	store, err := qos.NewRedisStore(*redisAddr)
	if err != nil {
		logger.Fatalf("redis: %s", err)
	}
	defer store.Close()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Fatalf("listen on %s: %s", *listen, err)
	}
	if *token == "" {
		logger.Warnf("qosd is running without a token; restrict the listen address to a trusted network")
	} else {
		logger.Infof("qosd: bearer token authentication enabled")
	}

	counter := qos.NewCounter()
	go heartbeat(counter, 10*time.Second)

	server := qos.NewServer(store, counter, *token)
	srv := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Infof("juicefs-qosd %s listening on %s, redis=%s (log-level=%s)",
		version.Version(), *listen, *redisAddr, *logLevel)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		logger.Infof("qosd: received %s, shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("serve: %s", err)
	}
	logger.Infof("qosd: stopped")
}

// heartbeat logs a periodic counter summary, so a quiet service still makes
// its state visible at the default log level.
func heartbeat(c *qos.Counter, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		logger.Infof("qosd: %s", c.Stats())
	}
}

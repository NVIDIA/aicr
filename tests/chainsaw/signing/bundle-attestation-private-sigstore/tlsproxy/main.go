// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// tlsproxy is a minimal HTTPS-termination reverse proxy used only by the
// private-Sigstore e2e suite. `aicr bundle` requires absolute https:// signing
// endpoints, but the local Fulcio/Rekor services are reached over plain HTTP
// through `kubectl port-forward`. This proxy fronts those HTTP targets with TLS
// using a caller-supplied (mkcert) certificate trusted by the host store, so
// `aicr` connects over https to localhost while the proxy forwards cleartext to
// the port-forward. Stdlib only; run.sh / the workflow `go build` it on demand.
//
// It runs until it receives SIGINT or SIGTERM (sent by run.sh / the workflow on
// cleanup), then drains in-flight requests with a bounded graceful shutdown.
//
// Usage:
//
//	tlsproxy CERT KEY LISTEN=TARGET [LISTEN=TARGET ...]
//
// e.g. tlsproxy cert.pem key.pem 8443=http://localhost:8080 8444=http://localhost:8081
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// shutdownTimeout bounds the graceful drain of in-flight requests on signal.
const shutdownTimeout = 5 * time.Second

func main() {
	if len(os.Args) < 4 {
		slog.Error("usage: tlsproxy CERT KEY LISTEN=TARGET [LISTEN=TARGET ...]")
		os.Exit(2)
	}
	certFile, keyFile, pairs := os.Args[1], os.Args[2], os.Args[3:]

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		slog.Error("failed to load TLS keypair", "error", err)
		os.Exit(1)
	}

	// Stop the proxy on the signals run.sh / the workflow use to tear it down.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	servers := make([]*http.Server, 0, len(pairs))
	for _, pair := range pairs {
		servers = append(servers, startProxy(pair, cert))
	}

	<-ctx.Done()
	slog.Info("signal received, shutting down TLS proxy")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			slog.Warn("proxy shutdown error", "addr", srv.Addr, "error", shutdownErr)
		}
	}
}

// startProxy launches one TLS listener that reverse-proxies to an HTTP target
// in its own goroutine and returns the server so the caller can shut it down.
// pair is "LISTEN_PORT=TARGET_URL". A setup or non-graceful serve failure is
// fatal (this is a single-purpose test helper).
func startProxy(pair string, cert tls.Certificate) *http.Server {
	listen, target, found := strings.Cut(pair, "=")
	if !found {
		slog.Error("malformed proxy spec, expected LISTEN=TARGET", "pair", pair)
		os.Exit(2)
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		slog.Error("invalid target URL", "target", target, "error", err)
		os.Exit(2)
	}

	srv := &http.Server{
		// Loopback only: this proxy bridges localhost to the in-cluster
		// Fulcio/Rekor port-forwards; it must not be reachable off-host.
		Addr: "127.0.0.1:" + listen,
		// G704: the target is an operator-supplied local port-forward address
		// (e.g. http://localhost:8080), not attacker-controlled input. This is
		// a single-purpose local test helper that exists to proxy to it.
		Handler:           httputil.NewSingleHostReverseProxy(targetURL), //nolint:gosec // G704: trusted local target, test helper
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		slog.Info("TLS proxy listening", "listen", "https://localhost:"+listen, "target", target)
		// ErrServerClosed is the expected result of a graceful Shutdown.
		if serveErr := srv.ListenAndServeTLS("", ""); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("TLS proxy stopped", "listen", listen, "error", serveErr)
			os.Exit(1)
		}
	}()
	return srv
}

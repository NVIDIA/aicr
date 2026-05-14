// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package testdns provides shared test utilities for spinning up an in-process
// DNS server. It lives in internal/ so it is only importable from this module
// and can be reused across pkg/discovery, pkg/api, and any future consumers
// without each package keeping its own copy.
package testdns

import (
	"context"
	"net"
	"testing"

	"github.com/miekg/dns"
)

// Start launches a UDP DNS server on 127.0.0.1 with an ephemeral port that
// dispatches to the supplied handler. It returns the host:port the caller
// should configure their DNS client with. The server is shut down via
// t.Cleanup when the test finishes.
func Start(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()

	// noctx lint requires the context-aware ListenConfig variant rather
	// than the bare net.ListenPacket. Background() is fine here — test
	// servers don't need a real deadline.
	lc := &net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testdns: listen: %v", err)
	}

	server := &dns.Server{
		PacketConn: pc,
		Handler:    handler,
	}

	go func() {
		_ = server.ActivateAndServe()
	}()

	t.Cleanup(func() {
		_ = server.Shutdown()
	})

	return pc.LocalAddr().String()
}

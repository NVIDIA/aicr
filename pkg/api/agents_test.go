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

package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/discovery"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startMockDNS starts a mock DNS server for testing.
// Duplicated from pkg/discovery/testhelper_test.go because Go test helpers
// cannot be shared across packages without a non-test dependency.
func startMockDNS(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)

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

func TestHandleAgents(t *testing.T) {
	handler := func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)

		qname := r.Question[0].Name
		switch r.Question[0].Qtype {
		case dns.TypeTXT:
			m.Answer = []dns.RR{
				&dns.TXT{
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 300},
					Txt: []string{"aicrd:mcp"},
				},
			}
		case dns.TypeSVCB:
			m.Answer = []dns.RR{
				&dns.SVCB{
					Hdr:      dns.RR_Header{Name: qname, Rrtype: dns.TypeSVCB, Class: dns.ClassINET, Ttl: 300},
					Priority: 1,
					Target:   fmt.Sprintf("aicrd.default.svc.cluster.local."),
					Value: []dns.SVCBKeyValue{
						&dns.SVCBPort{Port: 8080},
					},
				},
			}
		}
		_ = w.WriteMsg(m)
	}

	addr := startMockDNS(t, handler)
	disc := discovery.NewDiscoverer(
		discovery.WithDNSServer(addr),
		discovery.WithDNSTimeout(2*time.Second),
	)

	h := handleAgents(disc, nil, "default.svc.cluster.local.")

	t.Run("GET returns agents", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
		w := httptest.NewRecorder()

		h(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, "default.svc.cluster.local.", resp["domain"])
		assert.Equal(t, float64(1), resp["count"])

		agents, ok := resp["agents"].([]any)
		require.True(t, ok)
		require.Len(t, agents, 1)

		agent := agents[0].(map[string]any)
		assert.Equal(t, "aicrd", agent["name"])
		assert.Equal(t, "mcp", agent["protocol"])
		assert.Equal(t, float64(8080), agent["port"])
	})

	t.Run("custom domain via query param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/agents?domain=custom.svc.cluster.local.", nil)
		w := httptest.NewRecorder()

		h(w, req)

		var resp map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)
		assert.Equal(t, "custom.svc.cluster.local.", resp["domain"])
	})

	t.Run("POST returns method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/agents", nil)
		w := httptest.NewRecorder()

		h(w, req)

		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})
}

func TestHandleAgentsEmptyIndex(t *testing.T) {
	handler := func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(m)
	}

	addr := startMockDNS(t, handler)
	disc := discovery.NewDiscoverer(
		discovery.WithDNSServer(addr),
		discovery.WithDNSTimeout(2*time.Second),
	)

	h := handleAgents(disc, nil, "empty.svc.cluster.local.")

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	w := httptest.NewRecorder()

	h(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, float64(0), resp["count"])
}

func TestHandleAgentsInvalidDomain(t *testing.T) {
	// Handler shouldn't even hit DNS for invalid domains
	disc := discovery.NewDiscoverer(
		discovery.WithDNSServer("127.0.0.1:1"),
		discovery.WithDNSTimeout(100*time.Millisecond),
	)

	h := handleAgents(disc, nil, "default.svc.cluster.local.")

	tests := []struct {
		name   string
		domain string
	}{
		{"path traversal", "../etc/passwd"},
		{"uppercase", "Default.SVC.cluster.local"},
		{"empty label", "foo..bar.com"},
		{"starts with hyphen", "-bad.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/agents?domain="+tt.domain, nil)
			w := httptest.NewRecorder()

			h(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestDiscoveryDomain(t *testing.T) {
	// Without any env vars or service account, should fall back to default
	t.Setenv("AICR_DISCOVERY_DOMAIN", "")
	domain := discoveryDomain()
	// When no SA namespace file exists, expect default
	assert.Contains(t, domain, "svc.cluster.local.")

	// With env var set
	t.Setenv("AICR_DISCOVERY_DOMAIN", "custom.example.com.")
	domain = discoveryDomain()
	assert.Equal(t, "custom.example.com.", domain)
}

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

package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverer_Discover(t *testing.T) {
	tests := []struct {
		name     string
		handler  dns.HandlerFunc
		agentNm  string
		protocol Protocol
		domain   string
		want     *AgentRecord
		wantErr  bool
		errMsg   string
	}{
		{
			name: "successful discovery",
			handler: func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(r)
				m.Answer = []dns.RR{
					&dns.SVCB{
						Hdr:      dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeSVCB, Class: dns.ClassINET, Ttl: 300},
						Priority: 1,
						Target:   "aicrd.default.svc.cluster.local.",
						Value: []dns.SVCBKeyValue{
							&dns.SVCBPort{Port: 8080},
							&dns.SVCBLocal{KeyCode: dns.SVCBKey(SvcParamKeyRealm), Data: []byte("prod")},
						},
					},
				}
				_ = w.WriteMsg(m)
			},
			agentNm:  "aicrd",
			protocol: ProtocolMCP,
			domain:   "default.svc.cluster.local.",
			want: &AgentRecord{
				Name:     "aicrd",
				Protocol: ProtocolMCP,
				Domain:   "default.svc.cluster.local.",
				Target:   "aicrd.default.svc.cluster.local.",
				Port:     8080,
				Priority: 1,
				Endpoint: "https://aicrd.default.svc.cluster.local:8080",
				Params:   SvcParams{Realm: "prod"},
			},
		},
		{
			name: "NXDOMAIN returns not found",
			handler: func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetRcode(r, dns.RcodeNameError)
				_ = w.WriteMsg(m)
			},
			agentNm:  "missing",
			protocol: ProtocolMCP,
			domain:   "default.svc.cluster.local.",
			wantErr:  true,
			errMsg:   "not found",
		},
		{
			name: "empty answer returns not found",
			handler: func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(r)
				_ = w.WriteMsg(m)
			},
			agentNm:  "empty",
			protocol: ProtocolMCP,
			domain:   "default.svc.cluster.local.",
			wantErr:  true,
			errMsg:   "no SVCB records",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := startMockDNS(t, tt.handler)

			d := NewDiscoverer(
				WithDNSServer(addr),
				WithDNSTimeout(2*time.Second),
			)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := d.Discover(ctx, tt.agentNm, tt.protocol, tt.domain)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.want.Name, got.Name)
			assert.Equal(t, tt.want.Protocol, got.Protocol)
			assert.Equal(t, tt.want.Port, got.Port)
			assert.Equal(t, tt.want.Endpoint, got.Endpoint)
			assert.Equal(t, tt.want.Params.Realm, got.Params.Realm)
		})
	}
}

func TestDiscoverer_DiscoverUnreachableServer(t *testing.T) {
	d := NewDiscoverer(
		WithDNSServer("127.0.0.1:1"), // port 1 — nothing listening
		WithDNSTimeout(500*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := d.Discover(ctx, "test", ProtocolMCP, "example.com.")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SVCB query failed")
}

func TestDiscoverer_ListIndex(t *testing.T) {
	tests := []struct {
		name    string
		handler dns.HandlerFunc
		want    []IndexEntry
		wantErr bool
	}{
		{
			name: "successful index query",
			handler: func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(r)
				m.Answer = []dns.RR{
					&dns.TXT{
						Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 300},
						Txt: []string{"aicrd:mcp", "chat:a2a"},
					},
				}
				_ = w.WriteMsg(m)
			},
			want: []IndexEntry{
				{Name: "aicrd", Protocol: ProtocolMCP},
				{Name: "chat", Protocol: ProtocolA2A},
			},
		},
		{
			name: "NXDOMAIN returns nil",
			handler: func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetRcode(r, dns.RcodeNameError)
				_ = w.WriteMsg(m)
			},
			want: nil,
		},
		{
			name: "malformed entries are skipped",
			handler: func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(r)
				m.Answer = []dns.RR{
					&dns.TXT{
						Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 300},
						Txt: []string{"badentry", "good:mcp", "", "also-bad"},
					},
				}
				_ = w.WriteMsg(m)
			},
			want: []IndexEntry{
				{Name: "good", Protocol: ProtocolMCP},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := startMockDNS(t, tt.handler)

			d := NewDiscoverer(
				WithDNSServer(addr),
				WithDNSTimeout(2*time.Second),
			)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := d.ListIndex(ctx, "default.svc.cluster.local.")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDiscoverer_DiscoverAll(t *testing.T) {
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
					Target:   "aicrd.default.svc.cluster.local.",
					Value: []dns.SVCBKeyValue{
						&dns.SVCBPort{Port: 8080},
					},
				},
			}
		}
		_ = w.WriteMsg(m)
	}

	addr := startMockDNS(t, handler)

	d := NewDiscoverer(
		WithDNSServer(addr),
		WithDNSTimeout(2*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	records, err := d.DiscoverAll(ctx, "default.svc.cluster.local.")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "aicrd", records[0].Name)
	assert.Equal(t, ProtocolMCP, records[0].Protocol)
	assert.Equal(t, uint16(8080), records[0].Port)
}

func TestDiscoverer_DiscoverAllEmptyIndex(t *testing.T) {
	handler := func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(m)
	}

	addr := startMockDNS(t, handler)
	d := NewDiscoverer(WithDNSServer(addr), WithDNSTimeout(2*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	records, err := d.DiscoverAll(ctx, "empty.svc.cluster.local.")
	require.NoError(t, err)
	assert.Nil(t, records)
}

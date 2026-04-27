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
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSVCBAnswer(t *testing.T) {
	tests := []struct {
		name     string
		msg      *dns.Msg
		agentNm  string
		protocol Protocol
		domain   string
		want     *AgentRecord
	}{
		{
			name:     "no SVCB records returns nil",
			msg:      &dns.Msg{},
			agentNm:  "aicrd",
			protocol: ProtocolMCP,
			domain:   "default.svc.cluster.local.",
			want:     nil,
		},
		{
			name: "basic SVCB record with port",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.SVCB{
						Priority: 1,
						Target:   "aicrd.default.svc.cluster.local.",
						Value: []dns.SVCBKeyValue{
							&dns.SVCBPort{Port: 8080},
						},
					},
				},
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
			},
		},
		{
			name: "SVCB with ALPN",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.SVCB{
						Priority: 1,
						Target:   "agent.example.com.",
						Value: []dns.SVCBKeyValue{
							&dns.SVCBPort{Port: 443},
							&dns.SVCBAlpn{Alpn: []string{"h2", "http/1.1"}},
						},
					},
				},
			},
			agentNm:  "chat",
			protocol: ProtocolA2A,
			domain:   "example.com.",
			want: &AgentRecord{
				Name:     "chat",
				Protocol: ProtocolA2A,
				Domain:   "example.com.",
				Target:   "agent.example.com.",
				Port:     443,
				Priority: 1,
				ALPN:     []string{"h2", "http/1.1"},
				Endpoint: "https://agent.example.com",
			},
		},
		{
			name: "SVCB with all DNS-AID params",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.SVCB{
						Priority: 1,
						Target:   "aicrd.prod.svc.cluster.local.",
						Value: []dns.SVCBKeyValue{
							&dns.SVCBPort{Port: 8080},
							makeSVCBLocal(t, SvcParamKeyCap, "https://example.com/cap.json"),
							makeSVCBLocal(t, SvcParamKeyCapSHA256, "abc123"),
							makeSVCBLocal(t, SvcParamKeyBAP, "mcp/1,a2a/1"),
							makeSVCBLocal(t, SvcParamKeyPolicy, "https://example.com/policy.json"),
							makeSVCBLocal(t, SvcParamKeyRealm, "production"),
							makeSVCBLocal(t, SvcParamKeySig, "eyJhbGciOiJSUzI1NiJ9.test"),
							makeSVCBLocal(t, SvcParamKeyConnectClass, "lattice"),
							makeSVCBLocal(t, SvcParamKeyConnectMeta, "arn:aws:vpc-lattice:us-east-1:123:service/svc-1"),
							makeSVCBLocal(t, SvcParamKeyEnrollURI, "https://example.com/enroll"),
						},
					},
				},
			},
			agentNm:  "aicrd",
			protocol: ProtocolMCP,
			domain:   "prod.svc.cluster.local.",
			want: &AgentRecord{
				Name:     "aicrd",
				Protocol: ProtocolMCP,
				Domain:   "prod.svc.cluster.local.",
				Target:   "aicrd.prod.svc.cluster.local.",
				Port:     8080,
				Priority: 1,
				Endpoint: "https://aicrd.prod.svc.cluster.local:8080",
				Params: SvcParams{
					Cap:          "https://example.com/cap.json",
					CapSHA256:    "abc123",
					BAP:          []string{"mcp/1", "a2a/1"},
					Policy:       "https://example.com/policy.json",
					Realm:        "production",
					Sig:          "eyJhbGciOiJSUzI1NiJ9.test",
					ConnectClass: "lattice",
					ConnectMeta:  "arn:aws:vpc-lattice:us-east-1:123:service/svc-1",
					EnrollURI:    "https://example.com/enroll",
				},
			},
		},
		{
			name: "non-SVCB records are skipped",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.A{A: []byte{10, 0, 0, 1}},
					&dns.SVCB{
						Priority: 1,
						Target:   "agent.test.",
						Value: []dns.SVCBKeyValue{
							&dns.SVCBPort{Port: 9090},
						},
					},
				},
			},
			agentNm:  "test",
			protocol: ProtocolMCP,
			domain:   "test.",
			want: &AgentRecord{
				Name:     "test",
				Protocol: ProtocolMCP,
				Domain:   "test.",
				Target:   "agent.test.",
				Port:     9090,
				Priority: 1,
				Endpoint: "https://agent.test:9090",
			},
		},
		{
			name: "selects lowest priority from multiple SVCB records",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.SVCB{
						Priority: 10,
						Target:   "backup.example.com.",
						Value:    []dns.SVCBKeyValue{&dns.SVCBPort{Port: 9090}},
					},
					&dns.SVCB{
						Priority: 1,
						Target:   "primary.example.com.",
						Value:    []dns.SVCBKeyValue{&dns.SVCBPort{Port: 8080}},
					},
					&dns.SVCB{
						Priority: 5,
						Target:   "secondary.example.com.",
						Value:    []dns.SVCBKeyValue{&dns.SVCBPort{Port: 8081}},
					},
				},
			},
			agentNm:  "multi",
			protocol: ProtocolMCP,
			domain:   "example.com.",
			want: &AgentRecord{
				Name:     "multi",
				Protocol: ProtocolMCP,
				Domain:   "example.com.",
				Target:   "primary.example.com.",
				Port:     8080,
				Priority: 1,
				Endpoint: "https://primary.example.com:8080",
			},
		},
		{
			name: "alias mode priority 0 is skipped",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.SVCB{
						Priority: 0, // alias mode
						Target:   "alias.example.com.",
					},
					&dns.SVCB{
						Priority: 1,
						Target:   "real.example.com.",
						Value:    []dns.SVCBKeyValue{&dns.SVCBPort{Port: 443}},
					},
				},
			},
			agentNm:  "alias-test",
			protocol: ProtocolMCP,
			domain:   "example.com.",
			want: &AgentRecord{
				Name:     "alias-test",
				Protocol: ProtocolMCP,
				Domain:   "example.com.",
				Target:   "real.example.com.",
				Port:     443,
				Priority: 1,
				Endpoint: "https://real.example.com",
			},
		},
		{
			name: "only alias mode returns nil",
			msg: &dns.Msg{
				Answer: []dns.RR{
					&dns.SVCB{
						Priority: 0,
						Target:   "alias.example.com.",
					},
				},
			},
			agentNm:  "alias-only",
			protocol: ProtocolMCP,
			domain:   "example.com.",
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSVCBAnswer(tt.msg, tt.agentNm, tt.protocol, tt.domain)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want.Name, got.Name)
			assert.Equal(t, tt.want.Protocol, got.Protocol)
			assert.Equal(t, tt.want.Domain, got.Domain)
			assert.Equal(t, tt.want.Target, got.Target)
			assert.Equal(t, tt.want.Port, got.Port)
			assert.Equal(t, tt.want.Priority, got.Priority)
			assert.Equal(t, tt.want.ALPN, got.ALPN)
			assert.Equal(t, tt.want.Endpoint, got.Endpoint)
			assert.Equal(t, tt.want.Params, got.Params)
		})
	}
}

func TestBuildEndpoint(t *testing.T) {
	tests := []struct {
		name   string
		target string
		port   uint16
		want   string
	}{
		{"standard port", "agent.example.com.", 8080, "https://agent.example.com:8080"},
		{"default HTTPS port", "agent.example.com.", 443, "https://agent.example.com"},
		{"zero port", "agent.example.com.", 0, "https://agent.example.com"},
		{"empty target", "", 8080, ""},
		{"dot-only target", ".", 8080, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildEndpoint(tt.target, tt.port)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFQDN(t *testing.T) {
	tests := []struct {
		name     string
		agent    string
		protocol Protocol
		domain   string
		want     string
	}{
		{"with trailing dot", "aicrd", ProtocolMCP, "default.svc.cluster.local.", "_aicrd._mcp._agents.default.svc.cluster.local."},
		{"without trailing dot", "aicrd", ProtocolMCP, "default.svc.cluster.local", "_aicrd._mcp._agents.default.svc.cluster.local."},
		{"a2a agent", "chat", ProtocolA2A, "example.com.", "_chat._a2a._agents.example.com."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FQDN(tt.agent, tt.protocol, tt.domain)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIndexFQDN(t *testing.T) {
	assert.Equal(t, "_index._agents.default.svc.cluster.local.", IndexFQDN("default.svc.cluster.local."))
	assert.Equal(t, "_index._agents.default.svc.cluster.local.", IndexFQDN("default.svc.cluster.local"))
}

func TestBapVersions(t *testing.T) {
	tests := []struct {
		name string
		reg  AgentRegistration
		want []string
	}{
		{"explicit BAP", AgentRegistration{BAP: []string{"mcp/2", "a2a/1"}}, []string{"mcp/2", "a2a/1"}},
		{"derived from version", AgentRegistration{Protocol: ProtocolMCP, Version: "2.1.0"}, []string{"mcp/2"}},
		{"no version defaults to 1", AgentRegistration{Protocol: ProtocolA2A}, []string{"a2a/1"}},
		{"simple version", AgentRegistration{Protocol: ProtocolMCP, Version: "3"}, []string{"mcp/3"}},
		{"semver v-prefix", AgentRegistration{Protocol: ProtocolMCP, Version: "v2.1.0"}, []string{"mcp/2"}},
		{"semver V-prefix", AgentRegistration{Protocol: ProtocolMCP, Version: "V3.0.0"}, []string{"mcp/3"}},
		{"v-prefix simple", AgentRegistration{Protocol: ProtocolMCP, Version: "v4"}, []string{"mcp/4"}},
		{"non-semver leading v", AgentRegistration{Protocol: ProtocolMCP, Version: "vendor1"}, []string{"mcp/vendor1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.reg.bapVersions()
			assert.Equal(t, tt.want, got)
		})
	}
}

// makeSVCBLocal creates an SVCBLocal key-value pair for testing.
func makeSVCBLocal(t *testing.T, key uint16, value string) *dns.SVCBLocal {
	t.Helper()
	kv := new(dns.SVCBLocal)
	kv.KeyCode = dns.SVCBKey(key)
	kv.Data = []byte(value)
	return kv
}

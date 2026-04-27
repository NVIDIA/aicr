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
	"fmt"
	"strings"
)

// Protocol represents the agent communication protocol.
type Protocol string

const (
	// ProtocolMCP is the Model Context Protocol.
	ProtocolMCP Protocol = "mcp"
	// ProtocolA2A is the Agent-to-Agent protocol.
	ProtocolA2A Protocol = "a2a"
)

// SvcParams holds DNS-AID private-use SvcParam values parsed from SVCB records.
// Keys 65400-65408 per draft-mozleywilliams-dnsop-dnsaid-01.
type SvcParams struct {
	Cap          string   // 65400: URI to capability document
	CapSHA256    string   // 65401: base64url SHA-256 of cap document
	BAP          []string // 65402: bulk agent protocols (e.g., "mcp/1", "a2a/1")
	Policy       string   // 65403: URI to policy document
	Realm        string   // 65404: multi-tenant scope identifier
	Sig          string   // 65405: JWS compact signature over SVCB record
	ConnectClass string   // 65406: service mesh connector class
	ConnectMeta  string   // 65407: service ARN or metadata
	EnrollURI    string   // 65408: enrollment endpoint URI
}

// AgentRecord represents a discovered agent's DNS-AID metadata.
type AgentRecord struct {
	Name     string   // Agent name (e.g., "aicrd")
	Protocol Protocol // Communication protocol
	Domain   string   // Discovery domain
	Target   string   // SVCB target hostname
	Port     uint16   // SVCB port
	Priority uint16   // SVCB priority (0 = alias mode)
	ALPN     []string // ALPN protocol negotiation
	Params   SvcParams
	Endpoint string // Constructed endpoint URL (https://target:port)
}

// AgentRegistration holds the information needed to publish
// this agent as discoverable via DNS-AID.
type AgentRegistration struct {
	Name         string
	Protocol     Protocol
	Namespace    string
	ServiceName  string // K8s Service name backing this agent
	Port         uint16
	Capabilities []string
	Version      string
	PolicyURI    string
	Realm        string
	BAP          []string // Explicit BAP versions (e.g., "mcp/1"). If nil, derived from Protocol.
}

// bapVersions returns the BAP values for this registration.
// Uses explicit BAP if set, otherwise derives from Protocol and Version.
// A leading semver "v" prefix (e.g. "v2.1.0") is stripped so the result is
// "mcp/2" rather than "mcp/v2".
func (r AgentRegistration) bapVersions() []string {
	if len(r.BAP) > 0 {
		return r.BAP
	}
	ver := "1"
	if r.Version != "" {
		v := r.Version
		// Strip a single leading "v" or "V" only when followed by a digit
		// (semver style); leave non-semver strings untouched.
		if len(v) >= 2 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
			v = v[1:]
		}
		// Use major version only (e.g., "2.1.0" -> "2")
		if idx := strings.IndexByte(v, '.'); idx > 0 {
			ver = v[:idx]
		} else {
			ver = v
		}
	}
	return []string{string(r.Protocol) + "/" + ver}
}

// IndexEntry represents a single agent in a domain's index record.
// Serialized as "name:protocol" in TXT records, not JSON.
type IndexEntry struct {
	Name     string
	Protocol Protocol
}

// FQDN returns the fully qualified DNS name for an agent record.
// Format: _{name}._{protocol}._agents.{domain}
// The domain is normalized to include a trailing dot.
func FQDN(name string, protocol Protocol, domain string) string {
	return fmt.Sprintf("_%s._%s._agents.%s", name, protocol, ensureTrailingDot(domain))
}

// IndexFQDN returns the fully qualified DNS name for a domain's agent index.
// Format: _index._agents.{domain}
// The domain is normalized to include a trailing dot.
func IndexFQDN(domain string) string {
	return fmt.Sprintf("_index._agents.%s", ensureTrailingDot(domain))
}

// ensureTrailingDot ensures a domain name ends with a trailing dot (FQDN form).
func ensureTrailingDot(domain string) string {
	if !strings.HasSuffix(domain, ".") {
		return domain + "."
	}
	return domain
}

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

	"github.com/miekg/dns"
)

// DNS-AID SvcParamKey constants (private-use range).
// Per draft-mozleywilliams-dnsop-dnsaid-01.
const (
	SvcParamKeyCap          uint16 = 65400
	SvcParamKeyCapSHA256    uint16 = 65401
	SvcParamKeyBAP          uint16 = 65402
	SvcParamKeyPolicy       uint16 = 65403
	SvcParamKeyRealm        uint16 = 65404
	SvcParamKeySig          uint16 = 65405
	SvcParamKeyConnectClass uint16 = 65406
	SvcParamKeyConnectMeta  uint16 = 65407
	SvcParamKeyEnrollURI    uint16 = 65408
)

// ParseSVCBAnswer extracts the highest-priority AgentRecord from a DNS SVCB answer section.
// Per RFC 9460, lower numeric priority = higher precedence. Priority 0 (alias mode) is skipped.
// Returns nil if no ServiceMode SVCB records are found.
func ParseSVCBAnswer(msg *dns.Msg, name string, protocol Protocol, domain string) *AgentRecord {
	var best *dns.SVCB
	for _, rr := range msg.Answer {
		svcb, ok := rr.(*dns.SVCB)
		if !ok {
			continue
		}
		// Skip alias mode (priority 0) — it has no SvcParams
		if svcb.Priority == 0 {
			continue
		}
		// Strict less-than keeps the first record at the lowest priority. RFC
		// 9460 §2.4.3 says equal-priority records are eligible for load
		// balancing; we currently rely on resolver answer order rather than
		// implementing randomized selection, since the typical AICR deployment
		// publishes one SVCB per agent.
		if best == nil || svcb.Priority < best.Priority {
			best = svcb
		}
	}
	if best == nil {
		return nil
	}
	return parseSVCBRecord(best, name, protocol, domain)
}

// parseSVCBRecord extracts AgentRecord fields from a single dns.SVCB RR.
func parseSVCBRecord(svcb *dns.SVCB, name string, protocol Protocol, domain string) *AgentRecord {
	record := &AgentRecord{
		Name:     name,
		Protocol: protocol,
		Domain:   domain,
		Target:   svcb.Target,
		Priority: svcb.Priority,
	}

	for _, kv := range svcb.Value {
		switch v := kv.(type) {
		case *dns.SVCBPort:
			record.Port = v.Port
		case *dns.SVCBAlpn:
			record.ALPN = v.Alpn
		case *dns.SVCBLocal:
			parseDNSAIDParam(v, &record.Params)
		}
	}

	record.Endpoint = buildEndpoint(record.Target, record.Port)
	return record
}

// parseDNSAIDParam extracts a DNS-AID private-use SvcParam value.
func parseDNSAIDParam(local *dns.SVCBLocal, params *SvcParams) {
	key := uint16(local.Key())
	val := string(local.Data)

	switch key {
	case SvcParamKeyCap:
		params.Cap = val
	case SvcParamKeyCapSHA256:
		params.CapSHA256 = val
	case SvcParamKeyBAP:
		// strings.Split("", ",") returns [""], which would make
		// AgentRegistration.bapVersions() think BAP is explicitly set and
		// skip protocol-derived defaults. Treat empty as unset.
		if val == "" {
			params.BAP = nil
		} else {
			params.BAP = strings.Split(val, ",")
		}
	case SvcParamKeyPolicy:
		params.Policy = val
	case SvcParamKeyRealm:
		params.Realm = val
	case SvcParamKeySig:
		params.Sig = val
	case SvcParamKeyConnectClass:
		params.ConnectClass = val
	case SvcParamKeyConnectMeta:
		params.ConnectMeta = val
	case SvcParamKeyEnrollURI:
		params.EnrollURI = val
	}
}

// buildEndpoint constructs an endpoint URL from target and port.
// Assumes HTTPS scheme. Future: derive scheme from ALPN (h2 -> https, h2c -> http).
func buildEndpoint(target string, port uint16) string {
	// Strip trailing dot from DNS name.
	host := strings.TrimSuffix(target, ".")
	if host == "" {
		return ""
	}
	if port == 0 || port == 443 {
		return fmt.Sprintf("https://%s", host)
	}
	return fmt.Sprintf("https://%s:%d", host, port)
}

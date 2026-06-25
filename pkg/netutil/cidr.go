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

// Package netutil holds small, dependency-free networking helpers shared across
// packages that have no other reason to depend on one another (e.g. the bundler
// and the conformance validators).
package netutil

import (
	"net"
	"net/netip"
	"strings"
)

// IsAnySourceCIDR reports whether cidr parses to a /0 prefix (covers every
// address), e.g. 0.0.0.0/0 or ::/0, which leaves a LoadBalancer open to the
// entire internet despite a non-empty source-range list. Unparseable entries
// return false: an invalid CIDR cannot widen exposure because the cloud LB
// would reject it before the source-range list takes effect.
//
// Limitation: this matches only a literal /0 prefix. It does not detect a union
// of narrower subnets that together cover the whole address space (e.g.
// 0.0.0.0/1 + 128.0.0.0/1) — a deliberate-evasion case nobody hits by accident,
// left as a documented limitation rather than expanded into range-union math.
func IsAnySourceCIDR(cidr string) bool {
	_, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return false
	}
	ones, _ := ipNet.Mask.Size()
	return ones == 0
}

// IsValidCIDR reports whether cidr is a canonical CIDR prefix that Kubernetes
// will accept in a Service loadBalancerSourceRanges entry (e.g. 10.0.0.0/8 or
// ::/0). It accepts surrounding whitespace.
//
// Canonical means: the address has no bits set outside the prefix length (so
// 1.2.3.4/24 is rejected — the network address is 1.2.3.0/24), and the prefix
// length has no leading zeros (so 1.2.3.0/024 is rejected). net.ParseCIDR is
// too lax here — it accepts both forms — but Kubernetes 1.36+ enables strict
// CIDR validation by default and rejects them at apply time. Because the
// bundler renders the operator's original strings verbatim, validating the
// canonical form here keeps a bundle that generates from failing when the
// Service is applied. netip.ParsePrefix gives us the strict parse; comparing
// against Masked() confirms there are no host bits.
//
// IPv4-mapped IPv6 prefixes (e.g. ::ffff:192.12.2.0/120) are also rejected:
// netip parses them and they can be canonical, but Kubernetes 1.36+ strict
// validation rejects the 4-in-6 form outright, so accepting them here would be
// another generate-pass/apply-fail trap.
func IsValidCIDR(cidr string) bool {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return false
	}
	if p.Addr().Is4In6() {
		return false
	}
	return p == p.Masked()
}

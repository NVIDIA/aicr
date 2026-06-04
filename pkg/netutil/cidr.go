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

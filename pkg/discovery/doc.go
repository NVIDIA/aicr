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

// Package discovery provides DNS-AID agent discovery for AICR.
//
// DNS-AID uses SVCB records (RFC 9460) with private-use SvcParam keys
// (65400-65408) per draft-mozleywilliams-dnsop-dnsaid-01 to enable
// agent-to-agent discovery via DNS.
//
// Agents are discovered at:
//
//	_{agent-name}._{protocol}._agents.{domain}
//
// An index of all agents in a domain is available at:
//
//	_index._agents.{domain}
//
// The Discoverer queries DNS for agent SVCB records. The Publisher
// creates Kubernetes resources (ConfigMaps) that CoreDNS can serve
// as SVCB records.
package discovery

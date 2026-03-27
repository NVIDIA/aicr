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

import "time"

// DiscovererOption configures a Discoverer.
type DiscovererOption func(*Discoverer)

// WithDNSServer sets the DNS server address for queries (e.g., "10.96.0.10:53").
func WithDNSServer(addr string) DiscovererOption {
	return func(d *Discoverer) {
		d.dnsServer = addr
	}
}

// WithDNSTimeout sets the per-query DNS timeout.
func WithDNSTimeout(timeout time.Duration) DiscovererOption {
	return func(d *Discoverer) {
		d.timeout = timeout
	}
}

// PublisherOption configures a Publisher.
type PublisherOption func(*Publisher)

// WithNamespace sets the Kubernetes namespace for publishing agent records.
func WithNamespace(ns string) PublisherOption {
	return func(p *Publisher) {
		p.namespace = ns
	}
}

// WithLabels sets additional labels on created Kubernetes resources.
func WithLabels(labels map[string]string) PublisherOption {
	return func(p *Publisher) {
		for k, v := range labels {
			p.labels[k] = v
		}
	}
}

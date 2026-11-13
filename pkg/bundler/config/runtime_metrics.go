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

package config

import "slices"

// RuntimeMetrics is what a bundle adds to the runtime Prometheus server's
// scrape scope (ADR-026), on top of the registry's telemetry.promql
// declarations. It is shared by the --runtime-metrics-* CLI flags,
// spec.bundle.runtimeMetrics in a config file, the runtime-metrics-* REST
// query parameters, and BundleOptions.RuntimeMetrics in the Go SDK.
type RuntimeMetrics struct {
	// Series are RE2 patterns matched against the whole metric name as
	// scraped; the server keeps a matching series.
	Series []string

	// Namespaces are namespaces whose ServiceMonitors and PodMonitors the
	// server loads.
	Namespaces []string
}

// IsZero reports whether m adds nothing.
func (m RuntimeMetrics) IsZero() bool {
	return len(m.Series) == 0 && len(m.Namespaces) == 0
}

// Clone returns a copy of m that shares no slice with it.
func (m RuntimeMetrics) Clone() RuntimeMetrics {
	return RuntimeMetrics{Series: slices.Clone(m.Series), Namespaces: slices.Clone(m.Namespaces)}
}

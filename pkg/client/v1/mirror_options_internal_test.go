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

package aicr

import (
	"testing"

	bundlerconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
)

// Option forwarding is tested here, inside the package, because the honest
// alternative is not available: asserting it end-to-end would require running
// discovery, which renders every component's Helm chart and needs the network.
//
// This matters more than typical option plumbing. Both options change the
// discovered set — overrides can disable a sub-component and drop its images,
// and the Kubernetes version changes how charts branch — so an option that
// silently did nothing would produce a plausible inventory that mirrors the
// wrong artifacts. The failure is silent by construction, which is exactly the
// case worth pinning.

func TestMirrorInventoryOptions_AreApplied(t *testing.T) {
	overrides := []bundlerconfig.ComponentPath{{Component: "gpuoperator"}}

	settings := &mirrorInventoryOptions{}
	for _, opt := range []MirrorInventoryOption{
		WithMirrorKubeVersion("1.31.0"),
		WithMirrorValueOverrides(overrides),
	} {
		opt(settings)
	}

	if settings.kubeVersion != "1.31.0" {
		t.Errorf("kubeVersion = %q, want %q; charts branch on it, so an ignored "+
			"value discovers a different image set than the cluster pulls",
			settings.kubeVersion, "1.31.0")
	}
	if len(settings.valueOverrides) != 1 ||
		settings.valueOverrides[0].Component != "gpuoperator" {

		t.Errorf("valueOverrides = %+v, want the supplied override; an ignored "+
			"override mirrors images for components the caller disabled",
			settings.valueOverrides)
	}
}

// TestMirrorInventoryOptions_NilOptionIsSkipped pins the nil-entry guard.
func TestMirrorInventoryOptions_NilOptionIsSkipped(t *testing.T) {
	settings := &mirrorInventoryOptions{}
	for _, opt := range []MirrorInventoryOption{nil, WithMirrorKubeVersion("1.30.0"), nil} {
		if opt != nil {
			opt(settings)
		}
	}
	if settings.kubeVersion != "1.30.0" {
		t.Errorf("kubeVersion = %q, want the non-nil option to still apply",
			settings.kubeVersion)
	}
}

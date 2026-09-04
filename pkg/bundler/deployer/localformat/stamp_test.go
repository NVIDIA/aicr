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

package localformat

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestStampFor(t *testing.T) {
	tests := []struct {
		name        string
		component   Component
		aicrVersion string
		wantAICR    string
		wantPayload string
	}{
		{
			name:        "helm component reports its chart pin verbatim",
			component:   Component{Name: "gpu-operator", Version: "v25.3.0"},
			aicrVersion: "v1.4.0",
			wantAICR:    "1.4.0",
			// Not normalized: the annotation is free-form and matching the
			// recipe pin exactly is what makes it comparable to one.
			wantPayload: "v25.3.0",
		},
		{
			name:        "kustomize component reports its git ref",
			component:   Component{Name: "kai-scheduler", Tag: "release-1.4", Path: "deploy"},
			aicrVersion: "1.4.0",
			wantAICR:    "1.4.0",
			wantPayload: "release-1.4",
		},
		{
			name:        "version wins over tag when both are set",
			component:   Component{Name: "mixed", Version: "2.0.0", Tag: "release-1.4"},
			aicrVersion: "1.4.0",
			wantAICR:    "1.4.0",
			wantPayload: "2.0.0",
		},
		{
			name:        "manifest-only component falls back to the AICR version",
			component:   Component{Name: "skyhook-customizations"},
			aicrVersion: "v1.4.0",
			wantAICR:    "1.4.0",
			wantPayload: "1.4.0",
		},
		{
			name:        "dev build folds to a Helm-valid version",
			component:   Component{Name: "gpu-operator", Version: "v25.3.0"},
			aicrVersion: defaults.DevVersion,
			wantAICR:    defaults.DevChartVersion,
			wantPayload: "v25.3.0",
		},
		{
			name:        "dev build with no payload reports the dev chart version twice",
			component:   Component{Name: "skyhook-customizations"},
			aicrVersion: defaults.DevVersion,
			wantAICR:    defaults.DevChartVersion,
			wantPayload: defaults.DevChartVersion,
		},
		{
			name:        "unset AICR version is treated as a dev build",
			component:   Component{Name: "skyhook-customizations"},
			aicrVersion: "",
			wantAICR:    defaults.DevChartVersion,
			wantPayload: defaults.DevChartVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stampFor(tt.component, tt.aicrVersion)
			if got.AICRVersion != tt.wantAICR {
				t.Errorf("AICRVersion = %q, want %q", got.AICRVersion, tt.wantAICR)
			}
			if got.PayloadVersion != tt.wantPayload {
				t.Errorf("PayloadVersion = %q, want %q", got.PayloadVersion, tt.wantPayload)
			}
		})
	}
}

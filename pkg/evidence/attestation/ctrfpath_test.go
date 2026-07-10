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

package attestation

import "testing"

func TestCTRFRelPath(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		want  string
	}{
		{"deployment", PhaseDeployment, "ctrf/deployment.json"},
		{"performance", PhasePerformance, "ctrf/performance.json"},
		{"conformance", PhaseConformance, "ctrf/conformance.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CTRFRelPath(tt.phase); got != tt.want {
				t.Errorf("CTRFRelPath(%q) = %q, want %q", tt.phase, got, tt.want)
			}
		})
	}
}

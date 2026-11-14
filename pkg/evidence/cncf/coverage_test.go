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

package cncf

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// everyMustCheck declares one complete proof path for every MUST of v1.37.
var everyMustCheck = []string{
	"gang-scheduling", "accelerator-metrics", "ai-service-metrics", "inference-gateway",
	"pod-autoscaling", "cluster-autoscaling", "robust-controller", "secure-accelerator-access",
}

func without(checks []string, drop ...string) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		keep := true
		for _, d := range drop {
			if c == d {
				keep = false
			}
		}
		if keep {
			out = append(out, c)
		}
	}
	return out
}

func TestRecipeCoverageGaps(t *testing.T) {
	tests := []struct {
		name          string
		version       Version
		declared      []string
		notApplicable map[string]string
		wantGaps      []string // CoverageGap.String() in checklist order
	}{
		{"every MUST covered", Version137, everyMustCheck, nil, nil},
		{"a missing single-path check is a gap", Version137, without(everyMustCheck, "gang-scheduling"), nil,
			[]string{"gang_scheduling: missing gang-scheduling"}},
		{"the Slinky alternative covers secure accelerator access", Version137,
			append(without(everyMustCheck, "secure-accelerator-access"), "slinky-slurm-gpu-access"), nil, nil},
		{"neither alternative names both", Version137, without(everyMustCheck, "secure-accelerator-access"), nil,
			[]string{"secure_accelerator_access: missing secure-accelerator-access or slinky-slurm-gpu-access"}},
		{"notApplicable removes the requirement from the gate", Version137, without(everyMustCheck, "cluster-autoscaling"),
			map[string]string{"cluster_autoscaling": "fixed-size cluster"}, nil},
		{"a SHOULD is never required", Version137, everyMustCheck, nil, nil}, // workload-sandboxing and driver-runtime-management absent
		{"dra_support is required at v1.35 only", Version135, everyMustCheck, nil,
			[]string{"dra_support: missing dra-support"}},
		{"an empty recipe names every MUST", Version136, nil, nil, []string{
			"ai_inference: missing inference-gateway", "gang_scheduling: missing gang-scheduling",
			"cluster_autoscaling: missing cluster-autoscaling", "pod_autoscaling: missing pod-autoscaling",
			"accelerator_metrics: missing accelerator-metrics", "ai_service_metrics: missing ai-service-metrics",
			"secure_accelerator_access: missing secure-accelerator-access or slinky-slurm-gpu-access",
			"robust_controller: missing robust-controller",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gaps, err := RecipeCoverageGaps(tt.version, tt.declared, tt.notApplicable)
			if err != nil {
				t.Fatalf("RecipeCoverageGaps: %v", err)
			}
			got := make([]string, 0, len(gaps))
			for _, g := range gaps {
				got = append(got, g.String())
			}
			if len(got) != len(tt.wantGaps) {
				t.Fatalf("gaps = %v, want %v", got, tt.wantGaps)
			}
			for i := range got {
				if got[i] != tt.wantGaps[i] {
					t.Errorf("gap[%d] = %q, want %q", i, got[i], tt.wantGaps[i])
				}
			}
		})
	}

	t.Run("unsupported version", func(t *testing.T) {
		_, err := RecipeCoverageGaps("v1.30", everyMustCheck, nil)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
}

func TestValidateRecipeCoverage(t *testing.T) {
	if err := ValidateRecipeCoverage(Version137, everyMustCheck, nil); err != nil {
		t.Fatalf("ValidateRecipeCoverage(complete) = %v, want nil", err)
	}
	err := ValidateRecipeCoverage(Version137, without(everyMustCheck, "gang-scheduling", "robust-controller"), nil)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	for _, want := range []string{"2 applicable MUST", "v1.37", "gang_scheduling: missing gang-scheduling",
		"robust_controller: missing robust-controller", "spec.validation.conformance.checks", "notApplicable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want substring %q", err, want)
		}
	}
}

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

package recipe

import (
	"context"
	"testing"
)

// TestVR200TrainingUsesNVLSNCCLCheck pins the VR200 training leaf to the NVLS
// NCCL check, guarding a failure mode that is silent rather than loud.
//
// The check NAME selects the transport variant and its template: the
// performance validator dispatches nccl-all-reduce-bw{,-net,-nvls} to
// different entrypoints, and constraintNameForVariant looks the threshold up
// under the matching name. VR200 appears only in
// supportedNCCLCombinations[variantNVLS], so the bare nccl-all-reduce-bw would
// dispatch the default variant, miss the matrix, and return validators.Skip —
// an unsupported combination is treated as "genuinely inapplicable", not as a
// failure. The gate would report SKIPPED and read as a clean run forever.
//
// A mismatched threshold constraint fails the same way: the check looks for
// its own variant-suffixed name, so a stray nccl-all-reduce-bw constraint
// would leave the NVLS check with no floor.
//
// Also asserts nccl-benchmark-runtime-ref is gone. That constraint made the
// validator skip ComputeDomain/IMEX provisioning ("the supplied runtime owns
// its fabric wiring") while the benchmark moved to a per-run namespace, so the
// operator-precreated ComputeDomain became unreachable and the launcher
// failed. VR200 now uses the compiled matrix entry and the embedded
// runtime-nvls.yaml instead, so the validator provisions IMEX itself.
func TestVR200TrainingUsesNVLSNCCLCheck(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), adequateBuildTimeout)
	defer cancel()

	criteria := NewCriteria()
	criteria.Service = CriteriaServiceRKE2
	criteria.Accelerator = CriteriaAcceleratorVR200
	criteria.OS = CriteriaOSUbuntu
	criteria.Intent = CriteriaIntentTraining

	result, err := NewBuilder().BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria(rke2/vr200/ubuntu/training) failed: %v", err)
	}
	if result == nil || result.Validation == nil || result.Validation.Performance == nil {
		t.Fatal("resolved recipe has no performance validation block")
	}
	perf := result.Validation.Performance

	const (
		nvlsCheck    = "nccl-all-reduce-bw-nvls"
		genericCheck = "nccl-all-reduce-bw"
		runtimeRef   = "nccl-benchmark-runtime-ref"
	)

	var haveNVLS, haveGeneric bool
	for _, c := range perf.Checks {
		switch c {
		case nvlsCheck:
			haveNVLS = true
		case genericCheck:
			haveGeneric = true
		}
	}
	if !haveNVLS {
		t.Errorf("performance checks %v do not include %q — the NVLS variant is what "+
			"selects testdata/vr200/rke2/runtime-nvls.yaml", perf.Checks, nvlsCheck)
	}
	if haveGeneric {
		t.Errorf("performance checks %v include the generic %q, which dispatches the "+
			"default variant; rke2/vr200 is not in that matrix, so the gate would be "+
			"SKIPPED rather than run", perf.Checks, genericCheck)
	}

	var haveNVLSThreshold bool
	for _, c := range perf.Constraints {
		switch c.Name {
		case nvlsCheck:
			haveNVLSThreshold = true
			if c.Value != ">= 600" {
				t.Errorf("%s threshold = %q, want %q", nvlsCheck, c.Value, ">= 600")
			}
		case genericCheck:
			t.Errorf("constraint %q is named for the default variant; the NVLS check "+
				"looks its threshold up under %q and would run with no floor",
				genericCheck, nvlsCheck)
		case runtimeRef:
			t.Errorf("constraint %q is still present: it makes the validator skip "+
				"ComputeDomain/IMEX provisioning, which no longer works now that the "+
				"benchmark runs in a per-run namespace", runtimeRef)
		}
	}
	if !haveNVLSThreshold {
		t.Errorf("no %q threshold constraint found in %v", nvlsCheck, perf.Constraints)
	}
}

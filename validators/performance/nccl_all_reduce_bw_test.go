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

package main

import (
	"encoding/json"
	stderrors "errors"
	"maps"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/validators"
)

// TestClassifyNCCLAllReduceBWResult pins the #2122 applicability contract at the
// dispatch boundary: once the recipe declares the nccl-all-reduce-bw* constraint
// (the only way this function is reached), a DECLARED-but-unavailable
// prerequisite must BLOCK, while a genuinely-inapplicable outcome that depends
// only on the recipe criteria must remain a Skip.
func TestClassifyNCCLAllReduceBWResult(t *testing.T) {
	const name = "nccl-all-reduce-bw"
	constraint := recipe.Constraint{Name: name, Value: ">= 300 GB/s"}

	// verdict classifies the returned error into one of three buckets so each
	// case can assert exactly which arm of the contract it exercises.
	const (
		wantPass  = "pass"
		wantSkip  = "skip"
		wantBlock = "block"
	)

	tests := []struct {
		name string
		// inner validateNcclAllReduceBw outputs
		actual   string
		passed   bool
		innerErr error

		want     string           // wantPass | wantSkip | wantBlock
		wantCode errors.ErrorCode // asserted only when want == wantBlock
	}{
		{
			// #2122 core case: recipe declared the benchmark but the cluster
			// lacks the >=2 GPU nodes it requires. Must fail closed, not Skip.
			name:     "declared prerequisite absent (few nodes) blocks with NotFound",
			actual:   skipMsgNCCLFewNodes,
			passed:   true, // inner folds skips as passed=true; must not leak through
			want:     wantBlock,
			wantCode: errors.ErrCodeNotFound,
		},
		{
			// Deterministic from recipe criteria: this service+accelerator has no
			// compiled template and no profile/runtime override. Genuinely
			// inapplicable — remains a Skip.
			name:   "unsupported combination stays a skip",
			actual: skipMsgNCCLNotImplemented,
			passed: true,
			want:   wantSkip,
		},
		{
			// A valid profile that does not implement this variant is also a
			// function of the recipe alone — genuinely inapplicable, stays Skip.
			name:   "profile does not implement variant stays a skip",
			actual: skipMsgNCCLNoProfile(ncclBenchmarkTarget{accelerator: recipe.CriteriaAcceleratorGB200, service: recipe.CriteriaServiceEKS}, variantDefault),
			passed: true,
			want:   wantSkip,
		},
		{
			// The #1327 standalone shape (no ValidationInput). It cannot occur via
			// this dispatch (the caller Skips when the constraint is absent), but
			// the classifier must still treat it as a genuine Skip if it appears.
			name:   "no-input skip string stays a skip",
			actual: skipMsgNCCLNoInput,
			passed: true,
			want:   wantSkip,
		},
		{
			// A specific inner code (RBAC denial during a preflight, etc.) must
			// survive the dispatch rather than be flattened to Internal.
			name:     "inner Unauthorized preserved (not flattened, not skipped)",
			innerErr: errors.New(errors.ErrCodeUnauthorized, "preflight RBAC denied"),
			want:     wantBlock,
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			// The inner TrainJob-admission timeout classification must survive too.
			name:     "inner Timeout preserved",
			innerErr: errors.New(errors.ErrCodeTimeout, "admission retry budget exhausted"),
			want:     wantBlock,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			// An uncoded error falls back to Internal.
			name:     "uncoded inner error falls back to Internal",
			innerErr: stderrors.New("some transport glitch"),
			want:     wantBlock,
			wantCode: errors.ErrCodeInternal,
		},
		{
			name:   "measured bandwidth over threshold passes",
			actual: "330.00 GB/s",
			passed: true,
			want:   wantPass,
		},
		{
			// A real bandwidth shortfall is a fail, never a skip.
			name:     "measured bandwidth under threshold blocks",
			actual:   "120.00 GB/s",
			passed:   false,
			want:     wantBlock,
			wantCode: errors.ErrCodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyNCCLAllReduceBWResult(name, constraint, tt.actual, tt.passed, tt.innerErr)

			switch tt.want {
			case wantPass:
				if err != nil {
					t.Fatalf("expected pass (nil error), got %v", err)
				}
			case wantSkip:
				if !validators.IsSkip(err) {
					t.Fatalf("expected a Skip, got %v", err)
				}
			case wantBlock:
				if err == nil {
					t.Fatalf("expected a blocking error, got nil")
				}
				if validators.IsSkip(err) {
					t.Fatalf("expected a blocking failure, got a Skip: %v", err)
				}
				if errorstest.ReportedCode(err) != tt.wantCode {
					t.Fatalf("expected error code %q, got %v", tt.wantCode, err)
				}
			}
		})
	}
}

// extraFromStdout returns the Extra sentinels in out, merged as the
// orchestrator merges them.
func extraFromStdout(t *testing.T, out string) map[string]string {
	t.Helper()
	merged := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		payload, ok := strings.CutPrefix(line, ctrf.ExtraLinePrefix)
		if !ok {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("malformed extra sentinel %q: %v", line, err)
		}
		maps.Copy(merged, m)
	}
	return merged
}

func TestNCCLScopeExtra(t *testing.T) {
	tests := []struct {
		name    string
		variant ncclVariant
		fabric  ncclFabricType
		want    map[string]string
	}{
		{
			name:    "nvls with no published fabric",
			variant: variantNVLS,
			want: map[string]string{
				"transport": "nvls", "thresholdGbps": "300", "tolerancePct": "10",
				"workerNodes": "2", "gpusInvolved": "16",
			},
		},
		{
			name:    "net variant on the EKS RoCE path",
			variant: variantNET,
			fabric:  fabricRoCE,
			want: map[string]string{
				"transport": "net", "fabric": "roce", "thresholdGbps": "300", "tolerancePct": "10",
				"workerNodes": "2", "gpusInvolved": "16",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ncclScopeExtra(tt.variant, tt.fabric, 300, 2, 16)
			if !maps.Equal(got, tt.want) {
				t.Errorf("ncclScopeExtra() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNCCLPublishedFabric(t *testing.T) {
	tests := []struct {
		name          string
		variant       ncclVariant
		customRuntime string
		service       recipe.CriteriaServiceType
		want          ncclFabricType
	}{
		{"baked-in EKS NET template uses the selector", variantNET, "", recipe.CriteriaServiceEKS, fabricEFA},
		{"default variant never receives the selector", variantDefault, "", recipe.CriteriaServiceEKS, ""},
		{"nvls variant never receives the selector", variantNVLS, "", recipe.CriteriaServiceEKS, ""},
		{"GKE template ignores the selector", variantNET, "", recipe.CriteriaServiceGKE, ""},
		{"recipe-supplied runtime owns its fabric", variantNET, "runtime.yaml", recipe.CriteriaServiceEKS, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ncclPublishedFabric(tt.variant, fabricEFA, tt.customRuntime, tt.service); got != tt.want {
				t.Errorf("ncclPublishedFabric() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNCCLBandwidthExtra(t *testing.T) {
	tests := []struct {
		name      string
		bandwidth float64
		threshold float64
		want      map[string]string
	}{
		{"barely over the threshold", 305.44, 300, map[string]string{"achievedGbps": "305.4", "passMargin": "+1.8"}},
		{"inside the tolerance band", 285, 300, map[string]string{"achievedGbps": "285", "passMargin": "-5"}},
		{"far above", 850, 300, map[string]string{"achievedGbps": "850", "passMargin": "+183.3"}},
		{"zero threshold omits the margin", 120, 0, map[string]string{"achievedGbps": "120"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ncclBandwidthExtra(tt.bandwidth, tt.threshold); !maps.Equal(got, tt.want) {
				t.Errorf("ncclBandwidthExtra() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNCCLSkipsPublishReason(t *testing.T) {
	t.Run("no constraint in recipe", func(t *testing.T) {
		var err error
		out := captureStdout(t, func() {
			err = checkNCCLAllReduceBWVariant(&validators.Context{Ctx: t.Context()}, variantNVLS)
		})
		if !validators.IsSkip(err) {
			t.Fatalf("want a Skip, got %v", err)
		}
		if got := extraFromStdout(t, out)["skipReason"]; got != validators.SkipReasonNoConstraint {
			t.Errorf("skipReason = %q, want %q", got, validators.SkipReasonNoConstraint)
		}
	})
	t.Run("no validation input", func(t *testing.T) {
		var actual string
		out := captureStdout(t, func() {
			actual, _, _ = validateNcclAllReduceBw(&validators.Context{Ctx: t.Context()}, recipe.Constraint{}, variantNET)
		})
		if actual != skipMsgNCCLNoInput {
			t.Fatalf("actual = %q, want %q", actual, skipMsgNCCLNoInput)
		}
		if got := extraFromStdout(t, out)["skipReason"]; got != validators.SkipReasonNoValidationInput {
			t.Errorf("skipReason = %q, want %q", got, validators.SkipReasonNoValidationInput)
		}
	})
}

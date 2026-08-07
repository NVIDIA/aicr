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

package verifier

import (
	"net/http"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestClassifyFailure_RegistryStatusTakesPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		step       int
		wantClass  string
		wantStatus int
		wantHint   bool
	}{
		{"forbidden", http.StatusForbidden, stepMaterialize, CauseRegistryForbidden, 403, true},
		{"unauthorized", http.StatusUnauthorized, stepMaterialize, CauseRegistryForbidden, 401, true},
		{"not found", http.StatusNotFound, stepMaterialize, CauseNotFound, 404, true},
		// A 4xx the classifier has no specific arm for. 5xx and 429 are no
		// longer "other": they are server-side, so they classify as transient
		// and carry a retry hint (see
		// TestClassifyRegistryError_ServerFaultsAreNotVerdicts).
		{"other client status", http.StatusBadRequest, stepMaterialize, CauseRegistry, 400, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Wrap through pkg/errors to mimic the real chain MaterializeBundle
			// produces (errors.Wrap over the oras response error).
			err := errors.Wrap(errors.ErrCodeUnavailable, "OCI pull failed",
				&errcode.ErrorResponse{StatusCode: tt.status})
			c := classifyFailure(tt.step, err)
			if c.Class != tt.wantClass {
				t.Errorf("Class = %q, want %q", c.Class, tt.wantClass)
			}
			if c.HTTPStatus != tt.wantStatus {
				t.Errorf("HTTPStatus = %d, want %d", c.HTTPStatus, tt.wantStatus)
			}
			switch {
			case tt.wantHint && c.Hint == "":
				t.Errorf("expected an actionable hint for status %d", tt.status)
			case !tt.wantHint && c.Hint != "":
				t.Errorf("status %d should carry no remediation hint; got %q", tt.status, c.Hint)
			}
		})
	}
}

func TestClassifyFailure_StepWhenNoRegistryStatus(t *testing.T) {
	tests := []struct {
		name      string
		step      int
		wantClass string
	}{
		{"signature", stepSignature, CauseSignature},
		{"inventory", stepInventory, CauseIntegrity},
		{"predicate", stepPredicate, CauseSchema},
		{"materialize (no registry status) is unknown, not registry", stepMaterialize, CauseUnknown},
		{"unknown step", 99, CauseUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := classifyFailure(tt.step, errors.New(errors.ErrCodeInvalidRequest, "boom"))
			if c.Class != tt.wantClass {
				t.Errorf("Class = %q, want %q", c.Class, tt.wantClass)
			}
			if !strings.Contains(c.Detail, "boom") {
				t.Errorf("Detail = %q, want it to contain %q", c.Detail, "boom")
			}
			if c.HTTPStatus != 0 {
				t.Errorf("HTTPStatus should be 0 without a registry status; got %d", c.HTTPStatus)
			}
		})
	}
}

// TestRecordFailure_FirstVerdictWins keeps the original first-wins rule for two
// real verdicts: verification runs in order, so the earliest genuine failure is
// the root cause and later ones are usually fallout.
func TestRecordFailure_FirstVerdictWins(t *testing.T) {
	r := &VerifyResult{}
	recordFailure(r, stepSignature, errors.New(errors.ErrCodeUnauthorized, "first"))
	recordFailure(r, stepInventory, errors.New(errors.ErrCodeInvalidRequest, "second"))
	if r.FailureCause == nil || !strings.Contains(r.FailureCause.Detail, "first") {
		t.Fatalf("expected first failure to win; got %+v", r.FailureCause)
	}
	if r.Exit != ExitInvalid {
		t.Errorf("exit = %d, want %d", r.Exit, ExitInvalid)
	}
}

// TestRecordFailure_HardVerdictSupersedesNonVerdict is the exception, and the
// reason exit and cause are set together. An earlier transient must not leave a
// tampered bundle reported as "exit 2, class transient, hint: retry" — that
// tells a CI gate to re-run a bundle it should be rejecting.
func TestRecordFailure_HardVerdictSupersedesNonVerdict(t *testing.T) {
	r := &VerifyResult{}
	recordFailure(r, stepSignature, errors.New(errors.ErrCodeTimeout, "trusted root unavailable"))
	if r.FailureCause == nil || r.FailureCause.Class != CauseTransient {
		t.Fatalf("expected a transient cause first; got %+v", r.FailureCause)
	}

	recordFailure(r, stepInventory, errors.New(errors.ErrCodeInvalidRequest, "manifest digest mismatch"))
	if r.Exit != ExitInvalid {
		t.Errorf("exit = %d, want %d", r.Exit, ExitInvalid)
	}
	if r.FailureCause == nil || r.FailureCause.Class != CauseIntegrity {
		t.Fatalf("cause = %+v, want class %q — an invalid verdict must not carry a transient cause",
			r.FailureCause, CauseIntegrity)
	}
	if strings.Contains(r.FailureCause.Hint, "retry") {
		t.Errorf("invalid bundle still carries a retry hint: %q", r.FailureCause.Hint)
	}
}

func TestRecordFailure_IgnoresNil(t *testing.T) {
	r := &VerifyResult{}
	recordFailure(r, stepMaterialize, nil)
	if r.FailureCause != nil {
		t.Errorf("nil error should not set a cause; got %+v", r.FailureCause)
	}
	if r.Exit != ExitValidPassed {
		t.Errorf("nil error should not change exit; got %d", r.Exit)
	}
}

func TestWriteVerdict_PendingAndCause(t *testing.T) {
	tests := []struct {
		name     string
		result   *VerifyResult
		wantText []string
	}{
		{
			name:     "pending signature verdict",
			result:   &VerifyResult{Exit: ExitValidPassed, Pending: true},
			wantText: []string{"pending signature"},
		},
		{
			name: "invalid verdict renders classified cause",
			result: &VerifyResult{
				Exit: ExitInvalid,
				FailureCause: &FailureCause{
					Class:      CauseRegistryForbidden,
					HTTPStatus: 403,
					Hint:       "make the fork's aicr-evidence package public",
					Detail:     "OCI pull failed",
				},
			},
			wantText: []string{CauseRegistryForbidden, "HTTP 403", "package public", "OCI pull failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderMarkdown(tt.result)
			for _, want := range tt.wantText {
				if !strings.Contains(got, want) {
					t.Errorf("verdict missing %q:\n%s", want, got)
				}
			}
		})
	}
}

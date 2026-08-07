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
	"context"
	"net/url"
	"syscall"
	"testing"

	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestClassifyFailure_TransientVsIntegrity is the diagnostic split #2083 asks
// for: a dead mount and a tampered bundle must not produce the same verdict.
// Before this, a timeout at any step fell through to the step's own class —
// so a wedged NFS read during the manifest hash check was reported as
// `integrity`, i.e. "this bundle has been tampered with".
func TestClassifyFailure_TransientVsIntegrity(t *testing.T) {
	tests := []struct {
		name      string
		step      int
		err       error
		wantClass string
	}{
		{
			name:      "timeout during inventory is transient, not integrity",
			step:      stepInventory,
			err:       errors.New(errors.ErrCodeTimeout, "timed out reading bundle file"),
			wantClass: CauseTransient,
		},
		{
			name:      "timeout during materialize is transient, not unknown",
			step:      stepMaterialize,
			err:       errors.New(errors.ErrCodeTimeout, "timed out reading bundle marker"),
			wantClass: CauseTransient,
		},
		{
			name:      "genuine hash mismatch stays integrity",
			step:      stepInventory,
			err:       errors.New(errors.ErrCodeInvalidRequest, "sha256 mismatch"),
			wantClass: CauseIntegrity,
		},
		{
			name:      "genuine signature failure stays signature",
			step:      stepSignature,
			err:       errors.New(errors.ErrCodeUnauthorized, "sigstore verification failed"),
			wantClass: CauseSignature,
		},
		{
			// An operator abort is neither an environmental fault nor a
			// verdict. Classifying a Ctrl-C as transient would tell a CI gate
			// to retry a run the operator deliberately stopped; letting it
			// fall through to the step class would report "integrity", i.e.
			// "this bundle has been tampered with", for pressing Ctrl-C.
			name:      "operator cancellation is its own class",
			step:      stepInventory,
			err:       errors.New(errors.ErrCodeCanceled, "canceled while reading bundle file"),
			wantClass: CauseCanceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFailure(tt.step, tt.err)
			if got == nil {
				t.Fatal("classifyFailure returned nil")
			}
			if got.Class != tt.wantClass {
				t.Errorf("class = %q, want %q", got.Class, tt.wantClass)
			}
		})
	}
}

// TestExitForFailure_EarlyTransientDoesNotMaskLaterInvalid guards the
// fail-open regression an early version of this change introduced.
//
// stepSignatureCheck does not return early from Verify, and setFailureCause is
// first-wins. So a transient signature-step failure (an unreachable Sigstore
// TUF cache, say) records CauseTransient, and every later step recomputed its
// exit from that sticky cause. A tampered bundle caught at the inventory step
// then reported "not verifiable — retry" with process exit 5, and the CI gate
// rendered it as an infrastructure blip instead of ":x: invalid".
func TestExitForFailure_EarlyTransientDoesNotMaskLaterInvalid(t *testing.T) {
	r := &VerifyResult{}

	// Step 2: transient fault. Not a verdict, so exit 3.
	transient := errors.New(errors.ErrCodeTimeout, "trusted root unavailable")
	recordFailure(r, stepSignature, transient)
	if r.Exit != ExitIncomplete {
		t.Fatalf("transient signature failure = exit %d, want %d", r.Exit, ExitIncomplete)
	}

	// Step 4: the manifest hash chain does not match — the bundle is tampered.
	tampered := errors.New(errors.ErrCodeInvalidRequest, "manifest.json digest does not match")
	recordFailure(r, stepInventory, tampered)

	if r.Exit != ExitInvalid {
		t.Errorf("tampered bundle after an earlier transient = exit %d, want %d (ExitInvalid) — "+
			"a hard verdict must not be downgraded to \"retry\"", r.Exit, ExitInvalid)
	}
	// The cause must move too: exit 2 paired with class "transient" would tell
	// the gate the bundle is invalid AND that it should retry.
	if r.FailureCause == nil || r.FailureCause.Class != CauseIntegrity {
		t.Errorf("cause = %+v, want class %q", r.FailureCause, CauseIntegrity)
	}
}

// TestExitForFailure_NeverDowngradesRecordedInvalid covers the reverse order:
// once a hard verdict is on the result, a later transient (fallout from the
// same broken state) must not relabel it.
func TestExitForFailure_NeverDowngradesRecordedInvalid(t *testing.T) {
	r := &VerifyResult{Exit: ExitInvalid, FailureCause: &FailureCause{Class: CauseIntegrity}}
	recordFailure(r, stepInventory, errors.New(errors.ErrCodeTimeout, "late timeout"))
	if r.Exit != ExitInvalid {
		t.Errorf("exit = %d, want %d", r.Exit, ExitInvalid)
	}
	if r.FailureCause.Class != CauseIntegrity {
		t.Errorf("cause = %q, want %q — late fallout must not relabel the verdict",
			r.FailureCause.Class, CauseIntegrity)
	}
}

// TestStorageFaultChain_NeverBecomesIntegrity walks the full classification
// chain a soft-mounted NFS produces: boundedio wraps EIO/EACCES/ESTALE as
// ErrCodeUnavailable, classifyFailure must call that transient rather than
// letting it inherit the step's class, and the exit must be ExitIncomplete.
//
// The inventory step is the dangerous one: its class is "integrity", so a
// storage fault that fell through to the step switch would tell the operator
// their bundle's hash chain is broken.
func TestStorageFaultChain_NeverBecomesIntegrity(t *testing.T) {
	steps := map[string]int{
		"materialize": stepMaterialize,
		"signature":   stepSignature,
		"predicate":   stepPredicate,
		"inventory":   stepInventory,
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			r := &VerifyResult{}
			fault := errors.Wrap(errors.ErrCodeUnavailable,
				"failed to read bundle recipe.yaml", syscall.EIO)

			recordFailure(r, step, fault)

			if r.Exit != ExitIncomplete {
				t.Errorf("exit = %d, want %d — a storage fault is not a verdict", r.Exit, ExitIncomplete)
			}
			if r.FailureCause == nil || r.FailureCause.Class != CauseTransient {
				t.Fatalf("cause = %+v, want class %q", r.FailureCause, CauseTransient)
			}
			if r.FailureCause.Class == CauseIntegrity {
				t.Error("storage fault reported as an integrity failure")
			}
		})
	}
}

// TestCancellationChain_IsNeverTransientOrInvalid is the same walk for a
// deliberate abort, which must reach neither the retryable bucket nor a
// verdict.
func TestCancellationChain_IsNeverTransientOrInvalid(t *testing.T) {
	r := &VerifyResult{}
	abort := errors.Wrap(errors.ErrCodeCanceled, "canceled while reading bundle file", context.Canceled)

	recordFailure(r, stepInventory, abort)

	if r.Exit != ExitIncomplete {
		t.Errorf("exit = %d, want %d", r.Exit, ExitIncomplete)
	}
	if r.FailureCause == nil || r.FailureCause.Class != CauseCanceled {
		t.Fatalf("cause = %+v, want class %q", r.FailureCause, CauseCanceled)
	}
	if isTransientFailure(abort) {
		t.Error("operator abort classified as transient — a CI gate would retry a deliberate stop")
	}
	if errors.IsTransient(abort) {
		t.Error("operator abort is in the global retryable bucket")
	}
}

// TestClassifyRegistryError_ServerFaultsAreNotVerdicts covers the path that
// runs *before* the transient check: classifyRegistryError returns early, so a
// registry status was landing on CauseRegistry and, through exitForFailure, on
// ExitInvalid. A registry outage or rate limit means we never obtained the
// bundle — reporting "your evidence is invalid" to a contributor for someone
// else's 503 is the same misdiagnosis this PR exists to remove.
func TestClassifyRegistryError_ServerFaultsAreNotVerdicts(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantClass string
		wantExit  int
	}{
		{"service unavailable", 503, CauseTransient, ExitIncomplete},
		{"internal server error", 500, CauseTransient, ExitIncomplete},
		{"bad gateway", 502, CauseTransient, ExitIncomplete},
		{"rate limited", 429, CauseTransient, ExitIncomplete},
		// Client-side statuses keep their existing, actionable classes.
		{"forbidden", 403, CauseRegistryForbidden, ExitInvalid},
		{"not found", 404, CauseNotFound, ExitInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &errcode.ErrorResponse{
				Method:     "GET",
				URL:        &url.URL{Scheme: "https", Host: "ghcr.io", Path: "/v2/x/manifests/y"},
				StatusCode: tt.status,
			}

			c := classifyFailure(stepMaterialize, err)
			if c == nil || c.Class != tt.wantClass {
				t.Fatalf("class = %+v, want %q", c, tt.wantClass)
			}
			if c.HTTPStatus != tt.status {
				t.Errorf("httpStatus = %d, want %d", c.HTTPStatus, tt.status)
			}

			r := &VerifyResult{}
			recordFailure(r, stepMaterialize, err)
			if r.Exit != tt.wantExit {
				t.Errorf("exit = %d, want %d", r.Exit, tt.wantExit)
			}
		})
	}
}

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

package cli

import (
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

// TestVerdictError_ProcessExitMatrix pins the whole verdict-to-process-exit
// contract in one place, because the load-bearing property of issue #2083 is a
// negative one: a run that never reached a verdict must not share a process
// exit code with a bundle that was checked and rejected. Asserting a set of
// acceptable codes would let that collapse back without failing.
func TestVerdictError_ProcessExitMatrix(t *testing.T) {
	tests := []struct {
		name     string
		result   *verifier.VerifyResult
		wantCode errors.ErrorCode
		wantExit int
	}{
		{
			name:     "valid bundle",
			result:   &verifier.VerifyResult{Exit: verifier.ExitValidPassed},
			wantExit: errors.ExitSuccess,
		},
		{
			name:     "valid with recorded phase failures",
			result:   &verifier.VerifyResult{Exit: verifier.ExitValidPhaseFailures},
			wantCode: errors.ErrCodeConflict,
			wantExit: errors.ExitInvalidInput,
		},
		{
			name:     "invalid bundle",
			result:   &verifier.VerifyResult{Exit: verifier.ExitInvalid},
			wantCode: errors.ErrCodeInvalidRequest,
			wantExit: errors.ExitInvalidInput,
		},
		{
			name: "storage fault — no verdict reached",
			result: &verifier.VerifyResult{
				Exit:         verifier.ExitIncomplete,
				FailureCause: &verifier.FailureCause{Class: verifier.CauseTransient},
			},
			wantCode: errors.ErrCodeTimeout,
			wantExit: errors.ExitTimeout,
		},
		{
			name: "operator abort — no verdict reached",
			result: &verifier.VerifyResult{
				Exit:         verifier.ExitIncomplete,
				FailureCause: &verifier.FailureCause{Class: verifier.CauseCanceled},
			},
			wantCode: errors.ErrCodeCanceled,
			wantExit: errors.ExitCanceled,
		},
		{
			// Defensive: an incomplete result with no recorded cause still must
			// not be reported as an invalid bundle.
			name:     "incomplete with no cause",
			result:   &verifier.VerifyResult{Exit: verifier.ExitIncomplete},
			wantCode: errors.ErrCodeTimeout,
			wantExit: errors.ExitTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verdictError(tt.result)
			if got := errors.ExitCodeFromError(err); got != tt.wantExit {
				t.Errorf("process exit = %d, want %d (err %v)", got, tt.wantExit, err)
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("code = %v, want %s", err, tt.wantCode)
			}
		})
	}
}

// TestVerdictError_NoVerdictNeverSharesInvalidExit is the negative property
// stated directly: neither way of failing to reach a verdict may collide with
// the exit code a rejected bundle produces, or a CI gate cannot tell "retry
// this" from "reject this contribution".
func TestVerdictError_NoVerdictNeverSharesInvalidExit(t *testing.T) {
	invalid := errors.ExitCodeFromError(verdictError(
		&verifier.VerifyResult{Exit: verifier.ExitInvalid}))

	for _, class := range []string{verifier.CauseTransient, verifier.CauseCanceled} {
		got := errors.ExitCodeFromError(verdictError(&verifier.VerifyResult{
			Exit:         verifier.ExitIncomplete,
			FailureCause: &verifier.FailureCause{Class: class},
		}))
		if got == invalid {
			t.Errorf("class %q exits %d, colliding with the invalid-bundle exit", class, got)
		}
	}
}

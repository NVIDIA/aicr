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
	stderrors "errors"

	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/internal/boundedio"
)

// recordFailure sets the exit code and the failure cause together, so the two
// can never disagree.
//
// The cause is first-wins — verification runs steps in order, so the earliest
// failure is the root cause and later errors are usually fallout — with one
// exception: a non-verdict cause (transient/canceled) recorded by an earlier
// step is REPLACED once a later step returns a real verdict. Otherwise a
// timeout during signature verification would leave a tampered bundle reported
// as `exit 2` with `class: transient` and the hint "retry", which tells a CI
// gate to re-run a bundle it should be rejecting.
func recordFailure(r *VerifyResult, step int, err error) {
	if r == nil || err == nil {
		return
	}
	cause := classifyFailure(step, err)
	exit := exitForClass(cause.Class)
	// Never downgrade a hard verdict an earlier step already recorded: later
	// errors are usually fallout from the same broken state.
	if r.Exit == ExitInvalid {
		exit = ExitInvalid
	}
	if r.FailureCause == nil ||
		(exit == ExitInvalid && isNonVerdictClass(r.FailureCause.Class)) {

		r.FailureCause = cause
	}
	r.Exit = exit
}

// exitForClass derives the exit code from the classified cause, so the two can
// never disagree. Deriving it from the raw error instead would miss causes the
// classifier assigns without a matching error code — a registry 503, for
// instance, carries no ErrCodeTimeout yet is plainly not a verdict about the
// bundle.
func exitForClass(class string) int {
	if isNonVerdictClass(class) {
		return ExitIncomplete
	}
	return ExitInvalid
}

// isNonVerdictClass reports whether a recorded cause means "no verdict was
// reached" rather than "the bundle is bad".
func isNonVerdictClass(class string) bool {
	return class == CauseTransient || class == CauseCanceled
}

// classifyFailure turns a step error into a structured, actionable cause.
// A registry HTTP status (extracted from the oras error chain) takes
// precedence over the step identity because a 403/404 pulling the bundle is
// the same actionable problem regardless of which step surfaced it; absent a
// status, the step that failed selects the class.
func classifyFailure(step int, err error) *FailureCause {
	if c := classifyRegistryError(err); c != nil {
		return c
	}
	// A read we could not complete says nothing about the bundle's contents,
	// so it must not inherit the step's verdict (a timeout at stepInventory is
	// not "integrity"). Keyed on deadlines and ErrCodeTimeout only, NOT on
	// errors.IsTransient: that also reports true for context.Canceled, which
	// would classify a deliberate operator abort as a retryable fault.
	if stderrors.Is(err, errors.New(errors.ErrCodeCanceled, "")) {
		return &FailureCause{
			Class:  CauseCanceled,
			Detail: err.Error(),
			Hint:   "verification was aborted by the operator — no verdict was reached",
		}
	}
	if isTransientFailure(err) {
		return &FailureCause{
			Class:  CauseTransient,
			Detail: err.Error(),
			Hint:   "the bundle could not be read (storage or registry fault) — this is not a verdict on its contents; retry",
		}
	}
	c := &FailureCause{Detail: err.Error()}
	switch step {
	case stepSignature:
		c.Class = CauseSignature
	case stepInventory:
		c.Class = CauseIntegrity
	case stepPredicate:
		c.Class = CauseSchema
	default:
		// Includes stepMaterialize with no registry status: Verify also
		// accepts unpacked local directories, so a status-less materialize
		// failure (missing dir, malformed reference) is not necessarily a
		// registry problem. Without registry evidence, leave it unknown rather
		// than asserting a registry cause that would drive the wrong remediation.
		c.Class = CauseUnknown
	}
	return c
}

// isTransientFailure reports whether err is an environmental fault worth
// retrying. Deliberately narrower than errors.IsTransient, which also counts
// context.Canceled: an operator who pressed Ctrl-C has not hit a transient
// fault and must not have their abort reported as a retryable condition.
func isTransientFailure(err error) bool {
	if boundedio.IsCanceled(err) {
		return false
	}
	return stderrors.Is(err, context.DeadlineExceeded) ||
		stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) ||
		stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, ""))
}

// classifyRegistryError walks the error chain for an oras registry response
// and maps its HTTP status to an actionable cause. Returns nil when no
// registry status is present.
func classifyRegistryError(err error) *FailureCause {
	var respErr *errcode.ErrorResponse
	if !stderrors.As(err, &respErr) {
		return nil
	}
	c := &FailureCause{Detail: err.Error(), HTTPStatus: respErr.StatusCode}
	switch respErr.StatusCode {
	case 401, 403:
		c.Class = CauseRegistryForbidden
		c.Hint = "registry not accessible (make the fork's aicr-evidence package public, or provide registry credentials)"
	case 404:
		c.Class = CauseNotFound
		c.Hint = "bundle not found at the referenced digest (was it pushed to this registry?)"
	case 429:
		// Rate limiting and server-side errors say nothing about the bundle —
		// we never got it. Leaving these as CauseRegistry made exitForFailure
		// return ExitInvalid, so a registry outage was reported to a
		// contributor as "your evidence is invalid".
		c.Class = CauseTransient
		c.Hint = "registry rate-limited the request — retry"
	default:
		if respErr.StatusCode >= 500 {
			c.Class = CauseTransient
			c.Hint = "registry returned a server error — retry"
			break
		}
		c.Class = CauseRegistry
	}
	return c
}

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
	stderrors "errors"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// setFailureCause records the classified reason for a non-zero Exit, but
// only the first one — verification runs steps in order, so the earliest
// failure is the root cause and later steps' errors are usually fallout.
func setFailureCause(r *VerifyResult, step int, err error) {
	if r == nil || err == nil || r.FailureCause != nil {
		return
	}
	r.FailureCause = classifyFailure(step, err)
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
	default:
		c.Class = CauseRegistry
	}
	return c
}

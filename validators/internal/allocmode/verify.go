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

package allocmode

import (
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// Verify is the VERIFY step of the #1327 contract ("configuration selects
// the allocation policy, cluster inspection verifies it, mismatch fails
// closed"): it compares the recipe-configured whole-GPU allocation policy
// against the facts Detect inspected, and returns ErrCodeInvalidRequest when
// the cluster cannot serve the configured mechanism — no silent fallback to
// the other mechanism. A cluster drifted from its recipe is a validation
// failure by design.
//
// GPUAllocationPolicyUnspecified (and "") verifies nothing: standalone runs
// keep capability-driven automatic selection. Any policy this build does not
// know how to verify — including the reserved dra-extended-resource — fails
// closed rather than slipping through unverified.
func Verify(policy string, mode *Mode) error {
	switch policy {
	case v1.GPUAllocationPolicyUnspecified, "":
		return nil
	case v1.GPUAllocationPolicyDRAResourceClaim:
		if mode == nil || !mode.DRAUsable {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"GPU allocation policy mismatch: the recipe configures %q but the cluster has no usable full-GPU DRA (%s DeviceClass + validated ResourceSlices).\nObserved state:\n%s\nLikely cause: the cluster has drifted from the recipe (DRA driver not deployed with resources.gpus.enabled, or its ResourceSlices are unhealthy) — redeploy the bundle or fix the recipe (issue #1327)",
				policy, draDriverGPU, modeSummaryOrAbsent(mode)))
		}
		return nil
	case v1.GPUAllocationPolicyDevicePluginExtendedResource:
		if mode == nil || !mode.DevicePluginUsable {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"GPU allocation policy mismatch: the recipe configures %q but no Ready, schedulable node advertises allocatable %s.\nObserved state:\n%s\nLikely cause: the cluster has drifted from the recipe (device plugin disabled or unhealthy on the GPU nodes) — redeploy the bundle or fix the recipe (issue #1327)",
				policy, resourceNVIDIAGPU, modeSummaryOrAbsent(mode)))
		}
		return nil
	case v1.GPUAllocationPolicyDRAExtendedResource:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"GPU allocation policy %q is reserved (KEP-5004 mapped extended resource) and not yet validated by AICR (issue #1327)", policy))
	default:
		// Allowlist, fail closed: an unknown policy (typo, or a newer
		// producer than this validator build) must not run unverified.
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"unknown GPU allocation policy %q (valid: %s, %s, %s)", policy,
			v1.GPUAllocationPolicyUnspecified,
			v1.GPUAllocationPolicyDevicePluginExtendedResource,
			v1.GPUAllocationPolicyDRAResourceClaim))
	}
}

// modeSummaryOrAbsent renders the inspection facts for mismatch messages,
// tolerating a nil Mode (verification invoked without a completed probe).
func modeSummaryOrAbsent(mode *Mode) string {
	if mode == nil {
		return "(no inspection facts available)"
	}
	return mode.Summary()
}

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

package validators

import (
	"log/slog"
	"math"
	"strconv"

	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// Extra keys a check may publish with EmitExtra. Each must have a value
// validator in pkg/evidence/redact's ctrfExtraAllowlist, or minimal redaction
// drops it.
const (
	ExtraKeyNodesValidated = "nodesValidated"
	ExtraKeyNodesTotal     = "nodesTotal"
	ExtraKeySkipReason     = "skipReason"
	ExtraKeyRuntimeSource  = "runtimeSource"

	ExtraKeyAchievedGbps  = "achievedGbps"
	ExtraKeyThresholdGbps = "thresholdGbps"
	ExtraKeyTolerancePct  = "tolerancePct"
	ExtraKeyPassMargin    = "passMargin"
	ExtraKeyWorkerNodes   = "workerNodes"
	ExtraKeyGPUsInvolved  = "gpusInvolved"
	ExtraKeyTransport     = "transport"
	ExtraKeyFabric        = "fabric"

	ExtraKeyDRAReady            = "draReady"
	ExtraKeyDRADesired          = "draDesired"
	ExtraKeyNodewrightComplete  = "nodewrightComplete"
	ExtraKeyNodewrightExpected  = "nodewrightExpected"
	ExtraKeyNamespacesActive    = "namespacesActive"
	ExtraKeyNamespacesExpected  = "namespacesExpected"
	ExtraKeyThroughputTokPerSec = "throughputTokPerSec"
	ExtraKeyTTFTP99Ms           = "ttftP99Ms"
	ExtraKeyThroughputThreshold = "throughputThreshold"
	ExtraKeyTTFTThresholdMs     = "ttftThresholdMs"
	ExtraKeyScaledThreshold     = "scaledThreshold"
	ExtraKeyGPUsBenchmarked     = "gpusBenchmarked"
	ExtraKeyGPUsPerNode         = "gpusPerNode"

	ExtraKeyConfiguredPolicy = "configuredPolicy"
	ExtraKeyResolvedMode     = "resolvedMode"
	ExtraKeyNodeLocal        = "nodeLocal"

	ExtraKeyDetectedVersion  = "detectedVersion"
	ExtraKeyVersionSatisfied = "versionSatisfied"
)

// Skip codes published under ExtraKeySkipReason. Each must also be listed in
// pkg/evidence/redact's ctrfSkipReasons.
const (
	SkipReasonNoGPUNodes            = "no-gpu-nodes"
	SkipReasonNoSchedulableGPUNodes = "no-schedulable-gpu-nodes"
	SkipReasonNodesBusy             = "nodes-busy"
	SkipReasonNoConstraint          = "no-constraint"
	SkipReasonComponentNotInRecipe  = "component-not-in-recipe"
	SkipReasonNoValidationInput     = "no-validation-input"
	SkipReasonUnsupportedTarget     = "unsupported-target"
	SkipReasonProfileLacksVariant   = "profile-lacks-variant"
	SkipReasonUnsupportedService    = "unsupported-service"
	SkipReasonUnrecognizedPlatform  = "unrecognized-platform"
	SkipReasonNoGPUBackedNodeSet    = "no-gpu-backed-nodeset"
	SkipReasonNoVerifiableNodes     = "no-verifiable-nodes"
	SkipReasonDRADriverNotInstalled = "dra-driver-not-installed"
	SkipReasonSlurmManagedGPUs      = "slurm-managed-gpus"
	SkipReasonSimulatedNodes        = "simulated-nodes"
)

// SkipWithReason publishes code as the skip reason and returns Skip(reason).
// Only code survives minimal redaction.
func SkipWithReason(code, reason string) error {
	EmitExtraOrWarn(map[string]string{ExtraKeySkipReason: code})
	return Skip(reason)
}

// EmitExtraOrWarn calls EmitExtra and logs, rather than returns, any error.
func EmitExtraOrWarn(extra map[string]string) {
	if err := EmitExtra(extra); err != nil {
		slog.Warn("failed to emit validator extra", "error", err)
	}
}

// FormatDecimal formats v rounded to one decimal place, without trailing
// zeros ("305.4", "300").
//
// Special cases, all rejected by the redaction allowlist, are:
//   - a v that rounds below zero keeps its minus sign
//   - NaN and ±Inf format as "NaN", "+Inf", and "-Inf"
func FormatDecimal(v float64) string {
	return strconv.FormatFloat(roundTenth(v), 'f', -1, 64)
}

// FormatSignedDecimal is FormatDecimal with an explicit sign ("+1.8", "-12.3",
// "+0").
func FormatSignedDecimal(v float64) string {
	r := roundTenth(v)
	if r >= 0 {
		return "+" + strconv.FormatFloat(r, 'f', -1, 64)
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}

// FormatBool formats b as "true" or "false".
func FormatBool(b bool) string { return strconv.FormatBool(b) }

// GPUAllocationPolicyExtra returns the Extra recording policy as the configured
// allocation policy. Emit it before verifying policy, so a mismatch still
// records it.
func GPUAllocationPolicyExtra(policy string) map[string]string {
	return map[string]string{ExtraKeyConfiguredPolicy: policy}
}

// GPUAllocationResolvedExtra returns the Extra recording the allocation mode a
// check exercised, DRA resource claims if dra is set and device-plugin
// extended resources otherwise. nodeLocal records whether the mode was chosen
// for one node rather than from cluster-wide capability.
func GPUAllocationResolvedExtra(dra, nodeLocal bool) map[string]string {
	mode := v1.GPUAllocationPolicyDevicePluginExtendedResource
	if dra {
		mode = v1.GPUAllocationPolicyDRAResourceClaim
	}
	return map[string]string{
		ExtraKeyResolvedMode: mode,
		ExtraKeyNodeLocal:    FormatBool(nodeLocal),
	}
}

func roundTenth(v float64) float64 {
	r := math.Round(v*10) / 10
	if r == 0 {
		return 0 // normalizes -0
	}
	return r
}

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
	"fmt"
	"os"
	"testing"
)

// driverRootLockstepStrictEnvVar promotes "lockstep unverifiable"
// findings (one side explicitly set, the other unset and falling
// through to the upstream chart default) from warnings to test
// failures. Used by CI jobs that want the stronger contract.
const driverRootLockstepStrictEnvVar = "AICR_DRIVER_ROOT_LOCKSTEP_STRICT"

// TestDriverRootLockstep enforces that, for every leaf overlay that ships
// both nvidia-dra-driver-gpu and gpu-operator, the resolved values of
// nvidia-dra-driver-gpu.nvidiaDriverRoot and
// gpu-operator.hostPaths.driverInstallDir do not silently diverge.
//
// Why this lockstep matters (see issue #1087):
// The DRA kubelet plugin loads the NVIDIA driver userspace
// (libnvidia-ml.so, nvidia-smi, nvidia-ctk) from nvidiaDriverRoot. The
// GPU operator mounts the operator-managed driver container rootfs onto
// the host at driverInstallDir. The two paths are independently
// configurable across overlays, but if they drift the DRA driver fails
// CDI spec generation ("Driver/library version mismatch" or missing
// libnvidia-ml.so), DRA-allocated pods stall in ContainerCreating, and
// `aicr validate` deployment phase fails. There is no schema link
// between the two fields, so an overlay editor can change one without
// the other and CI won't notice — this test is the only guard.
//
// The test walks every leaf overlay (same discovery pattern used by
// TestBothBuildPathsProduceIdenticalContent), resolves it through the
// production builder, and inspects both resolved values:
//
//   - Both explicitly set and differ: HARD FAIL. This is the core
//     drift case the issue is guarding against.
//   - Exactly one explicitly set: warn-only by default; promoted to
//     fail when AICR_DRIVER_ROOT_LOCKSTEP_STRICT=1. The other side
//     falls through to the upstream chart default which this test
//     cannot read, so the lockstep is unverifiable rather than
//     definitively broken. The strict mode is for the eventual
//     cleanup PR that makes every overlay explicit.
//   - Both unset / both set and match: pass.
func TestDriverRootLockstep(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	strict := os.Getenv(driverRootLockstepStrictEnvVar) == "1"
	t.Logf("strict mode (%s=1): %t", driverRootLockstepStrictEnvVar, strict)

	// Discover leaf overlays: overlays not referenced as spec.base by any
	// other overlay. Mirrors the discovery in
	// TestBothBuildPathsProduceIdenticalContent.
	referencedAsBases := make(map[string]bool, len(store.Overlays))
	for _, overlay := range store.Overlays {
		if overlay.Spec.Base != "" {
			referencedAsBases[overlay.Spec.Base] = true
		}
	}

	leafCount := 0
	checked := 0
	for name, overlay := range store.Overlays {
		if referencedAsBases[name] {
			continue
		}
		if overlay.Spec.Criteria == nil {
			continue
		}
		leafCount++

		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult: %v", err)
			}

			dra := result.GetComponentRef("nvidia-dra-driver-gpu")
			op := result.GetComponentRef("gpu-operator")
			if dra == nil || op == nil {
				t.Skipf("lockstep N/A: nvidia-dra-driver-gpu=%v gpu-operator=%v",
					dra != nil, op != nil)
			}
			if !dra.IsEnabled() || !op.IsEnabled() {
				t.Skipf("lockstep N/A: one or both components disabled (dra enabled=%v, gpu-operator enabled=%v)",
					dra.IsEnabled(), op.IsEnabled())
			}
			checked++

			draValues, err := result.GetValuesForComponent("nvidia-dra-driver-gpu")
			if err != nil {
				t.Fatalf("GetValuesForComponent(nvidia-dra-driver-gpu): %v", err)
			}
			opValues, err := result.GetValuesForComponent("gpu-operator")
			if err != nil {
				t.Fatalf("GetValuesForComponent(gpu-operator): %v", err)
			}

			draRoot, _ := draValues["nvidiaDriverRoot"].(string)
			opInstallDir := stringAtPath(opValues, "hostPaths", "driverInstallDir")

			// Hard fail (the core lockstep break): both sides are
			// explicitly set, but they disagree. This is the silent-
			// drift case the issue is guarding against.
			if draRoot != "" && opInstallDir != "" && draRoot != opInstallDir {
				t.Errorf(
					"overlay %q: driver path mismatch — these MUST be identical:\n"+
						"  nvidia-dra-driver-gpu.nvidiaDriverRoot         = %q\n"+
						"  gpu-operator.hostPaths.driverInstallDir        = %q\n"+
						"  The DRA kubelet plugin loads the driver userspace from nvidiaDriverRoot;\n"+
						"  gpu-operator mounts the driver container rootfs at driverInstallDir.\n"+
						"  Divergence breaks CDI spec generation and stalls DRA-allocated pods.\n"+
						"  See issue #1087.",
					name, draRoot, opInstallDir)
				return
			}

			// Soft finding (warn by default, fail under strict mode):
			// exactly one side is explicitly set. The unset side falls
			// through to the upstream chart default, which this test
			// cannot read — so the lockstep is unverifiable rather than
			// definitively broken. Strict mode is for the eventual
			// cleanup PR that makes every overlay's pair explicit.
			report := func(msg string) {
				if strict {
					t.Error(msg)
				} else {
					t.Logf("UNVERIFIED LOCKSTEP: %s", msg)
				}
			}
			switch {
			case draRoot == "" && opInstallDir != "":
				report(fmt.Sprintf(
					"overlay %q: nvidia-dra-driver-gpu.nvidiaDriverRoot is unset (chart default in effect) but gpu-operator.hostPaths.driverInstallDir = %q. "+
						"Set both explicitly so the lockstep is verifiable.",
					name, opInstallDir))
			case opInstallDir == "" && draRoot != "":
				report(fmt.Sprintf(
					"overlay %q: gpu-operator.hostPaths.driverInstallDir is unset (chart default in effect) but nvidia-dra-driver-gpu.nvidiaDriverRoot = %q. "+
						"Set both explicitly so the lockstep is verifiable.",
					name, draRoot))
			}
		})
	}

	if leafCount == 0 {
		t.Fatal("no leaf overlays discovered — the lockstep check would be vacuous; " +
			"verify the recipes/overlays/ directory and the leaf-discovery filter")
	}
	t.Logf("verified driver-root lockstep across %d leaf overlays (%d carried both components)",
		leafCount, checked)
}

// TestStringAtPath covers the helper used to dig
// gpu-operator.hostPaths.driverInstallDir out of the resolved Helm
// values map.
func TestStringAtPath(t *testing.T) {
	tree := map[string]any{
		"hostPaths": map[string]any{
			"driverInstallDir": "/run/nvidia/driver",
		},
		"scalar":      "leaf",
		"wrongType":   42,
		"nestedWrong": map[string]any{"x": 7},
	}
	tests := []struct {
		name string
		keys []string
		want string
	}{
		{"hits nested string", []string{"hostPaths", "driverInstallDir"}, "/run/nvidia/driver"},
		{"hits scalar", []string{"scalar"}, "leaf"},
		{"missing top key", []string{"absent"}, ""},
		{"missing nested key", []string{"hostPaths", "absent"}, ""},
		{"intermediate not a map", []string{"scalar", "leaf"}, ""},
		{"leaf wrong type", []string{"wrongType"}, ""},
		{"nested wrong-type leaf", []string{"nestedWrong", "x"}, ""},
		{"empty path", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringAtPath(tree, tt.keys...); got != tt.want {
				t.Errorf("stringAtPath(%v) = %q, want %q", tt.keys, got, tt.want)
			}
		})
	}
}

// stringAtPath walks a nested map[string]any along the given keys and
// returns the leaf value as a string, or "" if any key is missing or any
// intermediate is not a map. Used to extract gpu-operator's
// hostPaths.driverInstallDir from the resolved Helm values tree.
func stringAtPath(m map[string]any, keys ...string) string {
	current := m
	for i, k := range keys {
		v, ok := current[k]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			s, _ := v.(string)
			return s
		}
		next, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

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

// TestDriverRootLockstep enforces that, for every overlay carrying both
// nvidia-dra-driver-gpu and gpu-operator, the resolved values of
// nvidia-dra-driver-gpu.nvidiaDriverRoot and
// gpu-operator.hostPaths.driverInstallDir are explicitly set and identical.
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
// **Discovery.** The test iterates every overlay with non-nil
// Spec.Criteria. The earlier draft restricted to "leaf" overlays
// (overlays not referenced as spec.base by any other overlay), but
// production resolution is per-query: FindMatchingOverlays →
// filterToMaximalLeaves drops an overlay only when a matching descendant
// exists for that query. E.g., h100-gke-cos-training is a base for
// -kubeflow/-slurm leaves (which require platform=kubeflow/slurm), so a
// {h100, gke-cos, training} query without platform resolves to it
// directly in production. The earlier filter would miss that.
//
// **Assertion.** Both resolved values must be explicitly set (non-empty
// in the resolved Helm values map) AND identical. An empty value falls
// through to the upstream chart's bundled default, which the test cannot
// read — and per-component defaults differ (GPU Operator chart 26.3.1
// defaults driverInstallDir to /run/nvidia/driver, but DRA chart 25.12.0
// defaults nvidiaDriverRoot to /). Relying on chart defaults is itself
// drift waiting to happen on the next chart bump, so the test treats
// "not explicitly set on both" as a failure.
func TestDriverRootLockstep(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	overlayCount := 0
	checked := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		overlayCount++

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

			switch {
			case draRoot == "" && opInstallDir == "":
				t.Errorf(
					"overlay %q: both nvidia-dra-driver-gpu.nvidiaDriverRoot and gpu-operator.hostPaths.driverInstallDir are unset.\n"+
						"  Both must be set explicitly to the same path. Chart defaults differ across components\n"+
						"  (gpu-operator chart 26.3.1: /run/nvidia/driver; dra chart 25.12.0: /), so an unset value\n"+
						"  is drift waiting to happen on the next chart bump.\n"+
						"  See issue #1087.",
					name)
			case draRoot == "":
				t.Errorf(
					"overlay %q: nvidia-dra-driver-gpu.nvidiaDriverRoot is unset (chart default in effect)\n"+
						"  but gpu-operator.hostPaths.driverInstallDir = %q.\n"+
						"  Set nvidiaDriverRoot in the dra-driver values (or via the overlay's componentRefs.overrides)\n"+
						"  to %q so the lockstep is verifiable.\n"+
						"  See issue #1087.",
					name, opInstallDir, opInstallDir)
			case opInstallDir == "":
				t.Errorf(
					"overlay %q: gpu-operator.hostPaths.driverInstallDir is unset (chart default in effect)\n"+
						"  but nvidia-dra-driver-gpu.nvidiaDriverRoot = %q.\n"+
						"  Set hostPaths.driverInstallDir in the gpu-operator values (or via the overlay's componentRefs.overrides)\n"+
						"  to %q so the lockstep is verifiable.\n"+
						"  See issue #1087.",
					name, draRoot, draRoot)
			case draRoot != opInstallDir:
				t.Errorf(
					"overlay %q: driver path mismatch — these MUST be identical:\n"+
						"  nvidia-dra-driver-gpu.nvidiaDriverRoot         = %q\n"+
						"  gpu-operator.hostPaths.driverInstallDir        = %q\n"+
						"  The DRA kubelet plugin loads the driver userspace from nvidiaDriverRoot;\n"+
						"  gpu-operator mounts the driver container rootfs at driverInstallDir.\n"+
						"  Divergence breaks CDI spec generation and stalls DRA-allocated pods.\n"+
						"  See issue #1087.",
					name, draRoot, opInstallDir)
			}
		})
	}

	if overlayCount == 0 {
		t.Fatal("no overlays with criteria discovered — the lockstep check would be vacuous; " +
			"verify the recipes/overlays/ directory")
	}
	t.Logf("verified driver-root lockstep across %d overlays (%d carried both components)",
		overlayCount, checked)
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

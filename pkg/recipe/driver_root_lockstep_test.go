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

// TestDriverRootLockstep guards the relationship between
// nvidia-dra-driver-gpu.nvidiaDriverRoot and
// gpu-operator.hostPaths.driverInstallDir across every overlay carrying
// both components.
//
// Why this matters (see issue #1087):
// The DRA kubelet plugin loads the NVIDIA driver userspace
// (libnvidia-ml.so, nvidia-smi, nvidia-ctk) from nvidiaDriverRoot. When
// the GPU operator MANAGES the driver (driver.enabled true), it installs
// the driver container rootfs onto the host at driverInstallDir, and DRA
// must read from that same path or CDI spec generation fails
// ("Driver/library version mismatch" / missing libnvidia-ml.so),
// DRA-allocated pods stall in ContainerCreating, and `aicr validate`
// deployment phase fails. There is no schema link between the two fields,
// so an overlay editor can drift one without the other and CI won't
// notice — this test is the only guard.
//
// **Two distinct invariants.** The original draft of this test (PR #1106)
// asserted the two values are always identical. That is wrong, and forcing
// it caused a regression: to make nvidiaDriverRoot ("/") and
// driverInstallDir match on the kind and oke overlays, #1106 set
// driverInstallDir: "/". But driverInstallDir is the host path the
// operator-validator bind-mounts as the driver-validation container's
// rootfs target, and runc rejects a mount whose destination is "/"
// ("mountpoint is on the top of rootfs") — crash-looping
// nvidia-operator-validator, stalling ClusterPolicy, and hanging every
// GPU-on-kind run (and oke, had it been in CI). The corrected model:
//
//  1. driverInstallDir is NEVER "/", for any overlay that includes
//     gpu-operator — it is a mount destination, illegal regardless of
//     driver.enabled or whether nvidia-dra-driver-gpu is present. (Hard
//     invariant; this is the regression guard, checked before the
//     DRA-dependent skips so a gpu-operator-only overlay can't slip past.)
//  2. nvidiaDriverRoot == driverInstallDir is required ONLY when the
//     operator manages the driver (gpu-operator driver.enabled true / unset
//     → chart default true). Then both must be explicitly set and equal.
//     When driver.enabled is false (host-installed drivers, e.g. kind/oke),
//     the two are independent: nvidiaDriverRoot is the host driver-userspace
//     location (legitimately "/"), while driverInstallDir is only the
//     validator's mount target (the base default /run/nvidia/driver).
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
// **Why "explicitly set" matters for the lockstep case.** An empty value
// falls through to the upstream chart's bundled default, which the test
// cannot read — and per-component defaults differ (GPU Operator chart
// 26.3.2 defaults driverInstallDir to /run/nvidia/driver, but DRA chart
// 25.12.0 defaults nvidiaDriverRoot to /). Relying on chart defaults is
// itself drift waiting to happen on the next chart bump, so when the
// lockstep applies the test treats "not explicitly set on both" as a
// failure.
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

			// Resolve gpu-operator values once (when present) so the hard
			// invariant below and the lockstep checks share them.
			var opValues map[string]any
			if op != nil {
				opValues, err = result.GetValuesForComponentWithContext(ctx, "gpu-operator")
				if err != nil {
					t.Fatalf("GetValuesForComponent(gpu-operator): %v", err)
				}
			}

			// Hard invariant, enforced for every overlay that includes
			// gpu-operator — independent of nvidia-dra-driver-gpu and of the
			// operator's own driver.enabled setting. driverInstallDir is the
			// operator-validator's bind-mount destination, so "/" is illegal:
			// runc rejects mounting over the container rootfs and
			// nvidia-operator-validator crash-loops. This is the guard for the
			// #1106 regression, so it must run BEFORE the DRA-dependent skips
			// below — otherwise a gpu-operator-only overlay (DRA absent or
			// disabled) could set driverInstallDir: "/" and slip past unchecked.
			if op != nil {
				if opInstallDir := stringAtPath(opValues, "hostPaths", "driverInstallDir"); opInstallDir == "/" {
					t.Errorf(
						"overlay %q: gpu-operator.hostPaths.driverInstallDir = %q is invalid.\n"+
							"  driverInstallDir is bind-mounted as the driver-validation container's rootfs target;\n"+
							"  runc rejects a mount whose destination is \"/\" (\"mountpoint is on the top of rootfs\"),\n"+
							"  crash-looping nvidia-operator-validator and stalling ClusterPolicy.\n"+
							"  Use a real subdirectory (the base default /run/nvidia/driver). For host-installed\n"+
							"  drivers at the host root, set nvidia-dra-driver-gpu.nvidiaDriverRoot: / instead —\n"+
							"  that field may be \"/\"; driverInstallDir may not.\n"+
							"  See issue #1087.",
						name, opInstallDir)
					return
				}
			}

			// The nvidiaDriverRoot == driverInstallDir lockstep additionally
			// requires both components present and enabled.
			if dra == nil || op == nil {
				t.Skipf("lockstep N/A: nvidia-dra-driver-gpu=%v gpu-operator=%v",
					dra != nil, op != nil)
			}
			if !dra.IsEnabled() || !op.IsEnabled() {
				t.Skipf("lockstep N/A: one or both components disabled (dra enabled=%v, gpu-operator enabled=%v)",
					dra.IsEnabled(), op.IsEnabled())
			}
			checked++

			draValues, err := result.GetValuesForComponentWithContext(ctx, "nvidia-dra-driver-gpu")
			if err != nil {
				t.Fatalf("GetValuesForComponent(nvidia-dra-driver-gpu): %v", err)
			}

			draRoot, _ := draValues["nvidiaDriverRoot"].(string)
			opInstallDir := stringAtPath(opValues, "hostPaths", "driverInstallDir")

			// The nvidiaDriverRoot == driverInstallDir lockstep only applies
			// when the operator installs the driver. With driver.enabled false
			// (host-installed drivers), the operator manages no driver, so the
			// DRA userspace root and the validator mount target are independent
			// and may legitimately differ (e.g. kind/oke: "/" vs
			// /run/nvidia/driver).
			if !boolAtPath(opValues, true, "driver", "enabled") {
				t.Skipf("lockstep N/A: gpu-operator manages no driver (driver.enabled=false); "+
					"nvidiaDriverRoot=%q and driverInstallDir=%q are independent", draRoot, opInstallDir)
			}

			switch {
			case draRoot == "" && opInstallDir == "":
				t.Errorf(
					"overlay %q: both nvidia-dra-driver-gpu.nvidiaDriverRoot and gpu-operator.hostPaths.driverInstallDir are unset.\n"+
						"  Both must be set explicitly to the same path. Chart defaults differ across components\n"+
						"  (gpu-operator chart 26.3.2: /run/nvidia/driver; dra chart 25.12.0: /), so an unset value\n"+
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

// TestBoolAtPath covers the helper used to read gpu-operator's
// driver.enabled out of the resolved Helm values map, including the
// fallback to the caller-supplied default when the path is absent or the
// leaf is not a bool (e.g. unset → chart default true).
func TestBoolAtPath(t *testing.T) {
	tree := map[string]any{
		"driver": map[string]any{
			"enabled": false,
		},
		"truthy":    true,
		"wrongType": "nope",
		"nested":    map[string]any{"x": 7},
	}
	tests := []struct {
		name string
		def  bool
		keys []string
		want bool
	}{
		{"hits nested false", true, []string{"driver", "enabled"}, false},
		{"hits scalar true", false, []string{"truthy"}, true},
		{"missing top key → default", true, []string{"absent"}, true},
		{"missing nested key → default", true, []string{"driver", "absent"}, true},
		{"intermediate not a map → default", true, []string{"truthy", "x"}, true},
		{"leaf wrong type → default", true, []string{"wrongType"}, true},
		{"nested wrong-type leaf → default", false, []string{"nested", "x"}, false},
		{"empty path → default", true, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boolAtPath(tree, tt.def, tt.keys...); got != tt.want {
				t.Errorf("boolAtPath(%v, def=%v) = %v, want %v", tt.keys, tt.def, got, tt.want)
			}
		})
	}
}

// boolAtPath walks a nested map[string]any along the given keys and returns
// the leaf bool, or def if any key is missing, any intermediate is not a
// map, or the leaf is not a bool. Used to read gpu-operator's
// driver.enabled (which falls back to the chart default — true — when an
// overlay does not set it).
func boolAtPath(m map[string]any, def bool, keys ...string) bool {
	current := m
	for i, k := range keys {
		v, ok := current[k]
		if !ok {
			return def
		}
		if i == len(keys)-1 {
			b, isBool := v.(bool)
			if !isBool {
				return def
			}
			return b
		}
		next, ok := v.(map[string]any)
		if !ok {
			return def
		}
		current = next
	}
	return def
}

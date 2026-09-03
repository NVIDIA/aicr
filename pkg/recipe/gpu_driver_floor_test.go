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

// gpu_driver_floor_test.go gates the host GPU driver floors recipes declare
// via the `Deployment.gpu-driver.version` constraint (issue #2438, consuming
// the validator hook shipped in #1995).
//
// Why an effective-value test rather than a presence test
//
// Deployment-phase constraints merge by NAME with the later overlay winning
// and no version comparison — see mergeValidationPhase in validation.go:
// "Non-empty overlay unions by Name; overlay value wins on same-name."
// There is no max(). So asserting that a floor exists somewhere in the chain
// is not enough: a same-name constraint applied later silently downgrades it,
// and a presence test still passes. These tests assert the FINAL effective
// value produced by the production resolver for every affected query.
//
// The placement rule the floors follow
//
// FindMatchingOverlays sorts candidates by criteria specificity ASCENDING,
// then each candidate's full inheritance chain is merged root-to-leaf. The
// consequence, verified by TestGPUDriverFloorWildcardIsWeakestPosition below,
// is that an accelerator wildcard (`*-any`) is applied BEFORE the service
// overlays it appears alongside. The wildcard is therefore the WEAKEST
// position for a floor, not the broadest — the opposite of the intuition.
//
// Rule: declare a product-level fallback on an accelerator wildcard only when
// the documented minimum applies to every hardware variant normalized to that
// accelerator. Repeat such a fallback on each maximal service x accelerator x
// intent leaf so later overlays cannot weaken it. Provider-specific leaves may
// carry a narrower edition-specific floor when the provider documents that
// edition. Never declare a floor on a shared accelerator-unbound service
// overlay (eks, gke-cos, aks, ...).
//
// The tests below enforce that rule for the current RTX PRO 6000 family and
// constrain where future floors may be declared.

package recipe

import (
	"context"
	"testing"
)

// gpuDriverFloorConstraint is the deployment-phase constraint name the
// check-nvidia-smi validator reads (validators/deployment/nvidia_smi.go).
const gpuDriverFloorConstraint = "Deployment.gpu-driver.version"

// rtxProDriverFloor is the RTX PRO 6000 Blackwell Server Edition host driver
// minimum. Source: NVIDIA GPU Operator platform support, "NVIDIA RTX PRO 6000
// Blackwell Server Edition notes": "Driver versions 575.57.08 or later is
// required."
// https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/platform-support.html
const rtxProDriverFloor = ">= 575.57.08"

// resolvedDriverFloor returns the effective Deployment.gpu-driver.version
// value for criteria under the given profile selection, and whether one was
// declared at all. It goes through the production resolver so the assertion
// covers wildcard contributions, mixins, and profile application.
// resolvedDeployment resolves a recipe once and returns its deployment
// validation phase, so a caller asserting both the floor and the check that
// evaluates it does not build the same recipe twice.
//
// selection is threaded through even though every current call passes "":
// no overlay in the RTX PRO 6000 EKS or LKE chains declares a profile (only
// aks.yaml and gke-cos.yaml do anywhere in the catalog), and selecting a
// profile against a composition that declares none is rejected at resolution.
// There is therefore no alternate-profile dimension to exercise for these
// leaves rather than an untested one.
//
// Note also that a profile could not downgrade this floor even where one does
// exist: ProfileValue.constraints are validated as measurement paths at catalog
// load (constraint_paths.go), and Deployment is not a measurement Type, so a
// profile value cannot carry a Deployment.gpu-driver.version constraint at all.
func resolvedDeployment(
	ctx context.Context, t *testing.T, criteria *Criteria, selection string,
) *ValidationPhase {

	t.Helper()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(ctx, criteria, selection)
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile(%s, %q): %v", criteria.String(), selection, err)
	}
	if result.Validation == nil {
		return nil
	}
	return result.Validation.Deployment
}

// driverFloorOf returns the declared host driver floor in a resolved deployment
// phase, if any.
func driverFloorOf(deployment *ValidationPhase) (string, bool) {
	if deployment == nil {
		return "", false
	}
	for _, c := range deployment.Constraints {
		if c.Name == gpuDriverFloorConstraint {
			return c.Value, true
		}
	}
	return "", false
}

// deploymentHasCheck reports whether a resolved deployment phase declares the
// named check. A floor with no check to evaluate it is inert.
func deploymentHasCheck(deployment *ValidationPhase, name string) bool {
	if deployment == nil {
		return false
	}
	for _, c := range deployment.Checks {
		if c == name {
			return true
		}
	}
	return false
}

// TestGPUDriverFloorEffectiveValue asserts the final effective host driver
// floor for every resolved query affected by a declared floor.
//
// Coverage lists the 11 current RTX PRO 6000 Server Edition resolutions: the
// four service x intent leaves that carry the constraint and every deeper OS
// and platform leaf that must inherit it.
func TestGPUDriverFloorEffectiveValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		criteria *Criteria
		want     string
	}{
		// EKS: the two overlays that declare the floor.
		{
			name: "rtx-pro-6000 eks training declares the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				Intent: CriteriaIntentTraining,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 eks inference declares the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				Intent: CriteriaIntentInference,
			},
			want: rtxProDriverFloor,
		},
		// EKS: deeper OS / platform leaves must inherit it unchanged.
		{
			name: "rtx-pro-6000 eks ubuntu training inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 eks ubuntu training kubeflow inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining,
				Platform: CriteriaPlatformKubeflow,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 eks ubuntu inference inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentInference,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 eks ubuntu inference dynamo inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentInference,
				Platform: CriteriaPlatformDynamo,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 eks ubuntu inference nim inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentInference,
				Platform: CriteriaPlatformNIM,
			},
			want: rtxProDriverFloor,
		},
		// LKE: the two overlays that declare the floor, plus their leaves.
		{
			name: "rtx-pro-6000 lke training declares the floor",
			criteria: &Criteria{
				Service: CriteriaServiceLKE, Accelerator: CriteriaAcceleratorRTXPro6000,
				Intent: CriteriaIntentTraining,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 lke inference declares the floor",
			criteria: &Criteria{
				Service: CriteriaServiceLKE, Accelerator: CriteriaAcceleratorRTXPro6000,
				Intent: CriteriaIntentInference,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 lke ubuntu training inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceLKE, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining,
			},
			want: rtxProDriverFloor,
		},
		{
			name: "rtx-pro-6000 lke ubuntu inference inherits the floor",
			criteria: &Criteria{
				Service: CriteriaServiceLKE, Accelerator: CriteriaAcceleratorRTXPro6000,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentInference,
			},
			want: rtxProDriverFloor,
		},
	}

	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deployment := resolvedDeployment(ctx, t, tt.criteria, "")
			got, found := driverFloorOf(deployment)
			if !found {
				t.Fatalf("%s resolved with no %s constraint; want %q.\n"+
					"A floor declared upstream was dropped, or the leaf no longer "+
					"inherits the deployment phase that carries it.",
					tt.criteria.String(), gpuDriverFloorConstraint, tt.want)
			}
			if got != tt.want {
				t.Errorf("%s effective %s = %q, want %q.\n"+
					"Constraints merge last-wins by name with no max comparison, so an "+
					"overlay applied later in this chain has overwritten the intended "+
					"floor. See the placement rule in this file's header.",
					tt.criteria.String(), gpuDriverFloorConstraint, got, tt.want)
			}

			// A floor with no check to evaluate it is inert: check-nvidia-smi
			// is the only consumer of this constraint.
			if !deploymentHasCheck(deployment, "check-nvidia-smi") {
				t.Errorf("%s declares %s but the resolved deployment phase has no "+
					"check-nvidia-smi check, so nothing evaluates the floor",
					tt.criteria.String(), gpuDriverFloorConstraint)
			}
		})
	}
}

// TestGPUDriverFloorEditionAmbiguousQueryHasNoFloor prevents the Server
// Edition minimum from leaking through the edition-collapsed accelerator
// wildcard. The generic check remains present and validates the nvidia-smi
// banner without imposing an edition-specific version floor.
func TestGPUDriverFloorEditionAmbiguousQueryHasNoFloor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	criteria := &Criteria{Accelerator: CriteriaAcceleratorRTXPro6000}
	deployment := resolvedDeployment(ctx, t, criteria, "")

	if got, found := driverFloorOf(deployment); found {
		t.Errorf("%s resolves with %s = %q; the accelerator criterion represents "+
			"Server, Workstation, and Max-Q Workstation editions, so a Server-only "+
			"floor must remain on provider-specific leaves",
			criteria.String(), gpuDriverFloorConstraint, got)
	}
	if !deploymentHasCheck(deployment, "check-nvidia-smi") {
		t.Errorf("%s resolved without check-nvidia-smi; removing the edition-specific "+
			"floor must not remove the generic driver-presence check", criteria.String())
	}
}

// TestGPUDriverFloorWildcardIsWeakestPosition pins the ordering fact the
// placement rule rests on: for a query that matches both an accelerator
// wildcard and a service overlay chain, the wildcard is applied FIRST.
//
// This is why any valid product-level fallback on an `*-any` overlay must be
// repeated on maximal leaves. If this test ever fails because the resolver's
// ordering changed, the placement rule and the comments beside every declared
// floor need rereading.
func TestGPUDriverFloorWildcardIsWeakestPosition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	criteria := &Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorRTXPro6000,
		Intent: CriteriaIntentTraining,
	}

	result, err := NewBuilder().BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}

	applied := result.Metadata.AppliedOverlays
	posWildcard, posService, posLeaf := -1, -1, -1
	for i, name := range applied {
		switch name {
		case "rtx-pro-6000-any":
			posWildcard = i
		case "eks-training":
			posService = i
		case "rtx-pro-6000-eks-training":
			posLeaf = i
		}
	}
	if posWildcard < 0 || posService < 0 || posLeaf < 0 {
		t.Fatalf("expected rtx-pro-6000-any, eks-training and rtx-pro-6000-eks-training "+
			"in the applied chain, got %v", applied)
	}

	if posWildcard > posService {
		t.Errorf("accelerator wildcard rtx-pro-6000-any applied at %d, AFTER service "+
			"overlay eks-training at %d (chain %v).\n"+
			"The placement rule in this file's header assumes the wildcard is applied "+
			"FIRST and is therefore the weakest position for a floor. That assumption "+
			"no longer holds; re-derive the rule before trusting existing floors.",
			posWildcard, posService, applied)
	}
	if posLeaf < posService {
		t.Errorf("maximal leaf rtx-pro-6000-eks-training applied at %d, BEFORE service "+
			"overlay eks-training at %d (chain %v); the leaf must be applied last for "+
			"its floor to survive last-wins merging", posLeaf, posService, applied)
	}
}

// TestGPUDriverFloorPlacementInvariant enforces the placement rule against
// every overlay in the catalog, so a floor added in future lands somewhere it
// cannot be silently downgraded.
//
// Structurally permitted positions are either an accelerator-bound product
// fallback (both service and intent wildcarded) or a concrete service x
// accelerator x intent leaf. A product fallback is semantically valid only
// when its floor applies to every hardware variant normalized to its
// accelerator criterion. Forbidden positions are:
//
//   - base.yaml, for the same reason, plus option B's lowest-common-
//     denominator problem discussed in issue #2438;
//   - a partially qualified overlay where only service or intent is wildcarded;
//   - an accelerator-unbound overlay (no `accelerator:` criterion, e.g. eks,
//     eks-training, gke-cos), since a host driver minimum is a property of the
//     GPU product and such an overlay cannot state one meaningfully — and a
//     floor there outranks accelerator-bound wildcards applied earlier.
func TestGPUDriverFloorPlacementInvariant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	declares := func(spec *RecipeMetadataSpec) bool {
		if spec == nil || spec.Validation == nil || spec.Validation.Deployment == nil {
			return false
		}
		for _, c := range spec.Validation.Deployment.Constraints {
			if c.Name == gpuDriverFloorConstraint {
				return true
			}
		}
		return false
	}

	if store.Base != nil && declares(&store.Base.Spec) {
		t.Errorf("base recipe declares %s. A global floor is the lowest common "+
			"denominator across every accelerator and provider, so it is weakest "+
			"exactly where it matters; declare floors on maximal leaves instead "+
			"(issue #2438, option A over option B).", gpuDriverFloorConstraint)
	}

	found := 0
	for name, overlay := range store.Overlays {
		if !declares(&overlay.Spec) {
			continue
		}
		found++

		criteria := overlay.Spec.Criteria
		if criteria == nil {
			t.Errorf("overlay %q declares %s but has no criteria", name, gpuDriverFloorConstraint)
			continue
		}

		serviceWildcard := criteria.Service == "" || criteria.Service == CriteriaServiceAny
		intentWildcard := criteria.Intent == "" || criteria.Intent == CriteriaIntentAny
		if serviceWildcard != intentWildcard {
			t.Errorf("overlay %q declares %s with only one of service or intent "+
				"wildcarded; declare a product-level accelerator fallback or use a "+
				"concrete service x accelerator x intent leaf",
				name, gpuDriverFloorConstraint)
		}

		if criteria.Accelerator == "" || criteria.Accelerator == CriteriaAcceleratorAny {
			t.Errorf("overlay %q declares %s but is accelerator-unbound (accelerator=%q).\n"+
				"A host driver minimum is a property of the GPU product, and a floor on "+
				"a shared service overlay outranks accelerator-bound wildcards applied "+
				"earlier — the silent-downgrade path issue #2438 describes.",
				name, gpuDriverFloorConstraint, criteria.Accelerator)
		}
	}

	// Guard against the gate going vacuous if every floor is removed: the
	// invariant would then pass while asserting nothing.
	if found == 0 {
		t.Fatalf("no overlay declares %s; this placement gate is vacuous. "+
			"If the last floor was intentionally removed, remove this guard too.",
			gpuDriverFloorConstraint)
	}
	t.Logf("overlays declaring %s: %d", gpuDriverFloorConstraint, found)
}

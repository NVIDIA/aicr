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

package recipe_test

import (
	"context"
	stderrors "errors"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	stockAdoptionComponent = "k8s-aibom"
	stockAdoptionVersion   = "k8s-aibom-stock-adoption-test"
)

// TestK8sAIBOMStockAdoption pins the stock-adoption contract from ADR-019's
// amendment: exactly one stock recipe installs k8s-aibom, its only descendant
// declines it, and the generation-time flag declines it too.
//
// Why this test rather than the parity goldens: both goldens key on *leaf*
// recipes, and `h100-gke-cos-inference` is not a leaf — `-dynamo` bases on it.
// So the target recipe of this whole amendment has no golden coverage, and the
// only golden that moves is the collateral one on the descendant. Without this
// test, flipping the target ref to `install: false` would leave every existing
// test green while silently un-shipping the component.
//
// DeploymentOrder is asserted alongside IsEnabled because the order is what
// bundlers walk. A ref that is declared-but-declined must be absent from it,
// which is the emission-level half of the claim.
func TestK8sAIBOMStockAdoption(t *testing.T) {
	target := func() *recipe.Criteria {
		return &recipe.Criteria{
			Service:     recipe.CriteriaServiceGKE,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSCOS,
			Intent:      recipe.CriteriaIntentInference,
		}
	}

	tests := []struct {
		name     string
		criteria *recipe.Criteria
		opts     []recipe.BuildOption
		// wantErr means the build must be rejected outright.
		wantErr      bool
		wantDeclared bool
		wantEnabled  bool
		// wantRecorded and wantMode are asserted separately, against
		// RuntimeInventoryMode()'s two return values. Comparing the mode
		// alone would collapse two distinct states into "": no configuration
		// recorded at all, which is correct for a build that does not pass
		// the flag, and a configuration that is present but carries an empty
		// mode, which is invalid. A regression producing the latter must not
		// pass as the former.
		//
		// Asserted at all because ADR-019 section E requires the recipe to
		// carry the decision, not just its effect, so a build that declines
		// the component while dropping the record is still a regression.
		wantRecorded bool
		wantMode     recipe.RuntimeInventoryMode
	}{
		{
			name:         "target stock recipe declares and enables the component",
			criteria:     target(),
			wantDeclared: true,
			wantEnabled:  true,
		},
		{
			name: "dynamo descendant declares but declines it",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Platform = recipe.CriteriaPlatformDynamo
				return c
			}(),
			wantDeclared: true,
			wantEnabled:  false,
		},
		{
			name:         "generation-time opt-out declines it on the target",
			criteria:     target(),
			opts:         []recipe.BuildOption{recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryDisabled)},
			wantDeclared: true,
			wantEnabled:  false,
			wantRecorded: true,
			wantMode:     recipe.RuntimeInventoryDisabled,
		},
		{
			// Regression: --runtime-inventory enabled must not override a
			// recipe that deliberately declines the component. The write path
			// uses the same `install` key an overlay declines with, so without
			// a pre-write guard the override clobbers the decline and the
			// post-write check reads back only what it just wrote. That
			// silently produced k8s-aibom alongside grove and dynamo-platform,
			// a combination nothing has qualified.
			name: "enabling on the declining descendant is rejected",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Platform = recipe.CriteriaPlatformDynamo
				return c
			}(),
			opts:    []recipe.BuildOption{recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled)},
			wantErr: true,
		},
		{
			name: "a sibling stock recipe does not declare it at all",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Intent = recipe.CriteriaIntentTraining
				return c
			}(),
			wantDeclared: false,
			wantEnabled:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion))

			result, err := builder.BuildFromCriteria(context.Background(), tt.criteria, tt.opts...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("BuildFromCriteria() error = nil, want rejection: a recipe-level decline must not be overridable from the command line")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("BuildFromCriteria() error = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}

			gotMode, gotRecorded := result.RuntimeInventoryMode()
			if gotRecorded != tt.wantRecorded {
				t.Errorf("RuntimeInventoryMode() recorded = %v, want %v: an absent configuration and a present-but-empty one must not be conflated",
					gotRecorded, tt.wantRecorded)
			}
			if gotMode != tt.wantMode {
				t.Errorf("configuration.runtimeInventory.mode = %q, want %q: the recipe must record the decision, not just its effect",
					gotMode, tt.wantMode)
			}

			var ref *recipe.ComponentRef
			for i := range result.ComponentRefs {
				if result.ComponentRefs[i].Name == stockAdoptionComponent {
					ref = &result.ComponentRefs[i]
					break
				}
			}

			if !tt.wantDeclared {
				if ref != nil {
					t.Fatalf("component %q is declared, want absent: adoption leaked beyond the target recipe",
						stockAdoptionComponent)
				}
				if slices.Contains(result.DeploymentOrder, stockAdoptionComponent) {
					t.Errorf("component %q is in DeploymentOrder despite not being declared",
						stockAdoptionComponent)
				}
				return
			}

			if ref == nil {
				t.Fatalf("component %q is absent, want declared: the target recipe no longer ships it",
					stockAdoptionComponent)
			}
			if got := ref.IsEnabled(); got != tt.wantEnabled {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.wantEnabled)
			}
			if got := slices.Contains(result.DeploymentOrder, stockAdoptionComponent); got != tt.wantEnabled {
				t.Errorf("in DeploymentOrder = %v, want %v: enabled state and emission disagree",
					got, tt.wantEnabled)
			}
		})
	}
}

// This test does not pass a BuildOption, so cfg.runtimeInventoryMode stays
// nil and applyRuntimeInventoryMode is never invoked (see accounting.go
// around the runtimeInventoryMode branch) -- it does not exercise the grant
// function at all. Coverage for the grant itself (including the reject
// paths) lives in the applyRuntimeInventoryMode tests in
// runtimeinventory_test.go.
//
// What this test does pin: that no stock GKE recipe declares k8s-aibom by
// default beyond the one that already does, and that the grant is not wired
// into ordinary resolution through some path other than
// applyRuntimeInventoryMode. The h100/inference and h100/training rows are
// intentionally omitted here because TestK8sAIBOMStockAdoption in this file
// already pins both ("target stock recipe declares and enables the
// component" and "a sibling stock recipe does not declare it at all"); the
// a100/training and b200/inference rows below add accelerator sampling
// breadth that nothing else covers.
func TestGKECriteriaUnchangedWithoutOptIn(t *testing.T) {
	cases := []struct {
		name        string
		criteria    *recipe.Criteria
		wantEnabled bool
	}{
		{"a100 training does not gain it", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorA100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining}, false},
		{"b200 inference does not gain it", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorB200,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentInference}, false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			builder := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion))
			result, err := builder.BuildFromCriteria(context.Background(), tt.criteria)
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}
			ref := result.GetComponentRef("k8s-aibom")
			enabled := ref != nil && ref.IsEnabled()
			if enabled != tt.wantEnabled {
				t.Errorf("k8s-aibom enabled = %v, want %v (no flag was passed)", enabled, tt.wantEnabled)
			}
		})
	}
}

// GKE criteria accept the opt-in regardless of workload shape -- that is the
// #2962 contract: one mechanism, no per-recipe special cases. The dynamo
// platform is the deliberate exception and must still be rejected.
func TestGKECriteriaAcceptOptIn(t *testing.T) {
	optIn := []recipe.BuildOption{
		recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled),
	}

	cases := []struct {
		name     string
		criteria *recipe.Criteria
		wantErr  bool
	}{
		{"training", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining}, false},
		// a100, not h100: h100-gke-cos-training-kubeflow additionally requires
		// --gke-tcpxo-interfaces (torch-distributed-tcpxo), a build gate
		// unrelated to k8s-aibom. a100-gke-cos-training-kubeflow carries no
		// such requirement and still exercises platform=kubeflow under the
		// opt-in.
		{"training on kubeflow", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorA100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			Platform: recipe.CriteriaPlatformKubeflow}, false},
		{"training on slurm", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			Platform: recipe.CriteriaPlatformSlurm}, false},
		{"a100 training", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorA100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining}, false},
		// Declines the component because k8s-aibom alongside grove and
		// dynamo-platform is unqualified. Widening adoption does not qualify it.
		// Every GKE dynamo accelerator is covered structurally by
		// TestEveryGKEDynamoRecipeRejectsTheOptIn below.
		{"dynamo still declines", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentInference,
			Platform: recipe.CriteriaPlatformDynamo}, true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			builder := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion))
			result, err := builder.BuildFromCriteria(context.Background(), tt.criteria, optIn...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("BuildFromCriteria() error = nil, want rejection: a recipe-level decline must not be overridable")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("BuildFromCriteria() error = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v, want the opt-in accepted", err)
			}
			ref := result.GetComponentRef("k8s-aibom")
			if ref == nil || !ref.IsEnabled() {
				t.Fatal("k8s-aibom was not granted")
			}
			var ordered bool
			for _, n := range result.DeploymentOrder {
				if n == "k8s-aibom" {
					ordered = true
				}
			}
			if !ordered {
				t.Errorf("k8s-aibom missing from deploymentOrder %v", result.DeploymentOrder)
			}
		})
	}
}

// The opt-in has to move the component and its health check together, which is
// what ADR-019 means by adopting a component rather than a chart.
//
// This fails closed in the dangerous direction: the deployment validator skips
// a component carrying no asserts rather than failing it, so a grant that
// loses them reports a passing check against a controller that never came up.
// Comparing against the declaring recipe rather than asserting non-empty also
// catches a grant that hydrates from the wrong source.
func TestGrantedRefCarriesSameHealthCheckAsDeclaring(t *testing.T) {
	refFor := func(t *testing.T, c *recipe.Criteria, opts ...recipe.BuildOption) *recipe.ComponentRef {
		t.Helper()
		builder := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion))
		result, err := builder.BuildFromCriteria(context.Background(), c, opts...)
		if err != nil {
			t.Fatalf("BuildFromCriteria() error = %v", err)
		}
		ref := result.GetComponentRef("k8s-aibom")
		if ref == nil {
			t.Fatal("k8s-aibom absent from the resolved recipe")
		}
		return ref
	}

	declared := refFor(t, &recipe.Criteria{
		Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
		OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentInference})

	granted := refFor(t, &recipe.Criteria{
		Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorA100,
		OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining},
		recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled))

	if declared.HealthCheckAsserts == "" {
		t.Fatal("declaring recipe carries no healthCheckAsserts; the comparison below would be vacuous")
	}
	if granted.HealthCheckAsserts != declared.HealthCheckAsserts {
		t.Errorf("granted healthCheckAsserts != declaring (%d vs %d bytes); deployment validation would skip the granted component",
			len(granted.HealthCheckAsserts), len(declared.HealthCheckAsserts))
	}
}

// The decline is a property of the k8s-aibom + grove + dynamo-platform
// pairing, not of any one accelerator, so it has to hold on every GKE dynamo
// recipe. Written as a sweep over accelerators rather than a fixed list
// because the h100-only version of this test passed while b200 and gb200
// silently assembled the trio the h100 overlay refuses.
//
// Each accelerator is first resolved WITHOUT the opt-in. That is what makes
// the sweep self-maintaining: an accelerator with no GKE dynamo recipe is
// skipped rather than asserted about, and a newly added one is picked up with
// no edit here. Only recipes that actually carry both grove and
// dynamo-platform are held to the rejection.
func TestEveryGKEDynamoRecipeRejectsTheOptIn(t *testing.T) {
	accelerators := []recipe.CriteriaAcceleratorType{
		recipe.CriteriaAcceleratorH100, recipe.CriteriaAcceleratorH200,
		recipe.CriteriaAcceleratorGB200, recipe.CriteriaAcceleratorGB300,
		recipe.CriteriaAcceleratorB200, recipe.CriteriaAcceleratorA100,
		recipe.CriteriaAcceleratorL40, recipe.CriteriaAcceleratorL40S,
		recipe.CriteriaAcceleratorRTXPro6000, recipe.CriteriaAcceleratorVR200,
	}

	var covered []string
	for _, acc := range accelerators {
		criteria := func() *recipe.Criteria {
			return &recipe.Criteria{
				Service: recipe.CriteriaServiceGKE, Accelerator: acc,
				OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentInference,
				Platform: recipe.CriteriaPlatformDynamo,
			}
		}

		base, err := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion)).
			BuildFromCriteria(context.Background(), criteria())
		if err != nil {
			continue // no GKE dynamo recipe for this accelerator
		}
		if base.GetComponentRef("grove") == nil || base.GetComponentRef("dynamo-platform") == nil {
			continue // not the unqualified pairing
		}
		covered = append(covered, string(acc))

		t.Run(string(acc), func(t *testing.T) {
			result, err := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion)).
				BuildFromCriteria(context.Background(), criteria(),
					recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled))
			if err == nil {
				// Name the component set, because the failure people need to
				// see is the trio being assembled, not merely a missing error.
				var got []string
				for _, n := range []string{"k8s-aibom", "grove", "dynamo-platform"} {
					if ref := result.GetComponentRef(n); ref != nil && ref.IsEnabled() {
						got = append(got, n)
					}
				}
				t.Fatalf("opt-in accepted on a GKE dynamo recipe; enabled components = %v, "+
					"want rejection (this overlay needs the k8s-aibom install:false decline)", got)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}

	// Guards the sweep itself: if resolution changes so that nothing matches,
	// every t.Run above would vanish and the test would pass vacuously.
	if len(covered) < 3 {
		t.Errorf("only %d GKE dynamo recipe(s) exercised (%v); expected at least h100, b200 and gb200 "+
			"-- the sweep is not reaching the recipes it is meant to protect", len(covered), covered)
	}
}

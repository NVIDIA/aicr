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
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	stockAdoptionComponent = "k8s-aibom"
	stockAdoptionVersion   = "k8s-aibom-stock-adoption-test"
)

// TestK8sAIBOMStockAdoption pins the stock-adoption contract: the target stock
// recipe installs k8s-aibom, its Dynamo descendant inherits it enabled (the
// pairing was qualified against k8s-aibom v1.6.1), and the generation-time
// flag declines it.
//
// Why this test rather than the parity goldens: both goldens key on *leaf*
// recipes, and `h100-gke-cos-inference` is not a leaf — `-dynamo` bases on it.
// So the target recipe has no golden coverage of its own. Without this test,
// flipping the target ref to `install: false` would leave every existing test
// green while silently un-shipping the component.
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
		name         string
		criteria     *recipe.Criteria
		opts         []recipe.BuildOption
		wantDeclared bool
		wantEnabled  bool
		// wantAfter names a component k8s-aibom must follow in
		// DeploymentOrder because it installs CRDs the controller watches.
		wantAfter string
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
			name: "dynamo descendant inherits and enables it",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Platform = recipe.CriteriaPlatformDynamo
				return c
			}(),
			wantDeclared: true,
			wantEnabled:  true,
			wantAfter:    "dynamo-platform",
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
			// The enable-over-decline guard itself is pinned on a synthetic
			// recipe by TestApplyRuntimeInventoryRespectsDeclineOnGKE, since no
			// stock recipe declines the component any more.
			name: "enabling on the dynamo descendant confirms it",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Platform = recipe.CriteriaPlatformDynamo
				return c
			}(),
			opts:         []recipe.BuildOption{recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled)},
			wantDeclared: true,
			wantEnabled:  true,
			wantAfter:    "dynamo-platform",
			wantRecorded: true,
			wantMode:     recipe.RuntimeInventoryEnabled,
		},
		{
			name: "generation-time opt-out declines it on the dynamo descendant",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Platform = recipe.CriteriaPlatformDynamo
				return c
			}(),
			opts:         []recipe.BuildOption{recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryDisabled)},
			wantDeclared: true,
			wantEnabled:  false,
			wantRecorded: true,
			wantMode:     recipe.RuntimeInventoryDisabled,
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
			// The dynamo overlay's ref is ordering-only, so the values file
			// must still come from the declaring recipe; losing it would ship
			// chart defaults instead of AICR's secure values.
			if ref.ValuesFile != "components/k8s-aibom/values.yaml" {
				t.Errorf("valuesFile = %q, want components/k8s-aibom/values.yaml", ref.ValuesFile)
			}
			if got := slices.Contains(result.DeploymentOrder, stockAdoptionComponent); got != tt.wantEnabled {
				t.Errorf("in DeploymentOrder = %v, want %v: enabled state and emission disagree",
					got, tt.wantEnabled)
			}
			if tt.wantAfter != "" {
				assertDeploysAfter(t, result.DeploymentOrder, stockAdoptionComponent, tt.wantAfter)
			}
		})
	}
}

// assertDeploysAfter fails unless component follows provider in order. The
// k8s-aibom controller discovers the kinds it watches when it starts, so
// deploying it before the component that installs their CRDs leaves those
// kinds unwatched until the controller restarts.
func assertDeploysAfter(t *testing.T, order []string, component, provider string) {
	t.Helper()
	ci, pi := slices.Index(order, component), slices.Index(order, provider)
	if ci < 0 || pi < 0 {
		t.Fatalf("deploymentOrder %v is missing %q or %q", order, component, provider)
	}
	if ci < pi {
		t.Errorf("%s deploys at %d, before %s at %d; it must follow the component that installs the CRDs it watches",
			component, ci, provider, pi)
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
// default beyond the ones that already do, and that the grant is not wired
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
		// Only h100's dynamo recipe inherits the component; the other GKE
		// dynamo recipes take it by opt-in alone.
		{"b200 inference on dynamo does not gain it", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorB200,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentInference,
			Platform: recipe.CriteriaPlatformDynamo}, false},
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
// #2962 contract: one mechanism, no per-recipe special cases.
func TestGKECriteriaAcceptOptIn(t *testing.T) {
	optIn := []recipe.BuildOption{
		recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled),
	}

	cases := []struct {
		name     string
		criteria *recipe.Criteria
	}{
		{"training", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining}},
		// a100, not h100: h100-gke-cos-training-kubeflow additionally requires
		// --gke-tcpxo-interfaces (torch-distributed-tcpxo), a build gate
		// unrelated to k8s-aibom. a100-gke-cos-training-kubeflow carries no
		// such requirement and still exercises platform=kubeflow under the
		// opt-in.
		{"training on kubeflow", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorA100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			Platform: recipe.CriteriaPlatformKubeflow}},
		{"training on slurm", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			Platform: recipe.CriteriaPlatformSlurm}},
		{"a100 training", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorA100,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining}},
		// b200, not h100: h100-gke-cos-inference-dynamo already enables the
		// component by inheritance, so only a dynamo recipe that does not
		// declare it exercises the grant. Every GKE dynamo accelerator is
		// covered structurally by TestEveryGKEDynamoRecipeAcceptsTheOptIn.
		{"b200 inference on dynamo", &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorB200,
			OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentInference,
			Platform: recipe.CriteriaPlatformDynamo}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			builder := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion))
			result, err := builder.BuildFromCriteria(context.Background(), tt.criteria, optIn...)
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

// The k8s-aibom + grove + dynamo-platform pairing was qualified as a pairing,
// not per accelerator, so every GKE dynamo recipe must accept the opt-in and
// none may carry a leftover decline. Written as a sweep over accelerators
// rather than a fixed list because an earlier h100-only version passed while
// b200 and gb200 behaved differently from h100.
//
// Each accelerator is first resolved WITHOUT the opt-in. That is what makes
// the sweep self-maintaining: an accelerator with no GKE dynamo recipe is
// skipped rather than asserted about, and a newly added one is picked up with
// no edit here. Only recipes that actually carry both grove and
// dynamo-platform are held to the contract.
func TestEveryGKEDynamoRecipeAcceptsTheOptIn(t *testing.T) {
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
			continue // not the dynamo pairing
		}
		covered = append(covered, string(acc))

		t.Run(string(acc), func(t *testing.T) {
			if ref := base.GetComponentRef("k8s-aibom"); ref != nil {
				if !ref.IsEnabled() {
					t.Fatal("k8s-aibom is declared but declined on a GKE dynamo recipe; " +
						"the pairing is qualified, so the overlay's decline is stale")
				}
				assertDeploysAfter(t, base.DeploymentOrder, "k8s-aibom", "dynamo-platform")
			}

			result, err := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion)).
				BuildFromCriteria(context.Background(), criteria(),
					recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryEnabled))
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v, want the opt-in accepted", err)
			}
			for _, n := range []string{"k8s-aibom", "grove", "dynamo-platform"} {
				if ref := result.GetComponentRef(n); ref == nil || !ref.IsEnabled() {
					t.Errorf("%s is not enabled after the opt-in", n)
				}
			}
			assertDeploysAfter(t, result.DeploymentOrder, "k8s-aibom", "dynamo-platform")
		})
	}

	// Guards the sweep itself: if resolution changes so that nothing matches,
	// every t.Run above would vanish and the test would pass vacuously.
	if len(covered) < 3 {
		t.Errorf("only %d GKE dynamo recipe(s) exercised (%v); expected at least h100, b200 and gb200 "+
			"-- the sweep is not reaching the recipes it is meant to protect", len(covered), covered)
	}
}

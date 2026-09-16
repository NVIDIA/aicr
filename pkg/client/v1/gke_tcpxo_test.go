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

package aicr_test

import (
	"context"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func gkeTCPXOTestRequest(mapping string) aicr.RecipeRequest {
	return aicr.RecipeRequest{
		Service:            "gke",
		Accelerator:        "h100",
		OS:                 "cos",
		Intent:             "training",
		Platform:           "kubeflow",
		GKETCPXOInterfaces: mapping,
	}
}

func gkeTCPXOTestMappingString() string {
	return recipe.FormatGKETCPXOInterfaces(recipe.GKETCPXOIntrospectionInterfaces())
}

// TestResolveRecipeGKETCPXOInterfaces covers the full facade path for the
// required mapping: request field → recorded configuration → query
// projection → adoption → values-only bundle.
func TestResolveRecipeGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	want := gkeTCPXOTestMappingString()
	result, err := client.ResolveRecipe(t.Context(), gkeTCPXOTestRequest(want))
	if err != nil {
		t.Fatalf("ResolveRecipe() error = %v", err)
	}

	got, present := result.Resolved().GKETCPXOInterfaces()
	if !present {
		t.Fatal("resolved recipe records no configuration.gke.tcpxoInterfaces")
	}
	if got[0].InterfaceName != "eth1" || got[0].Network != "gpu-nic-0" {
		t.Errorf("recorded first entry = %v", got[0])
	}

	// The query surface must see the recorded section.
	selected, err := aicr.SelectFromRecipeWithContext(t.Context(), result, "configuration.gke.tcpxoInterfaces")
	if err != nil {
		t.Fatalf("SelectFromRecipeWithContext() error = %v", err)
	}
	if entries, ok := selected.([]any); !ok || len(entries) != 8 {
		t.Errorf("selector returned %v, want 8 entries", selected)
	}

	// Adoption (the REST/POST boundary) must preserve the mapping, and the
	// values-only bundle must accept the adopted recipe.
	adopted, err := client.AdoptRecipe(t.Context(), result.Resolved())
	if err != nil {
		t.Fatalf("AdoptRecipe() error = %v", err)
	}
	if _, present := adopted.Resolved().GKETCPXOInterfaces(); !present {
		t.Fatal("AdoptRecipe dropped configuration.gke.tcpxoInterfaces")
	}
	if _, err := client.BundleComponents(t.Context(), adopted); err != nil {
		t.Fatalf("BundleComponents() after adoption error = %v", err)
	}
}

// TestResolveRecipeGKETCPXOInterfacesRequired covers fail-closed: the
// fingerprint family cannot resolve without the mapping, and the mapping is
// rejected on a family that does not ship the runtime.
func TestResolveRecipeGKETCPXOInterfacesRequired(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	missing := gkeTCPXOTestRequest("")
	missing.GKETCPXOInterfaces = ""
	if _, err := client.ResolveRecipe(t.Context(), missing); err == nil ||
		!strings.Contains(err.Error(), "--gke-tcpxo-interfaces") {

		t.Fatalf("ResolveRecipe() without the mapping error = %v, want fail-closed naming the flag", err)
	}

	notApplicable := aicr.RecipeRequest{
		Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training",
		Platform: "kubeflow", GKETCPXOInterfaces: gkeTCPXOTestMappingString(),
	}
	if _, err := client.ResolveRecipe(t.Context(), notApplicable); err == nil {
		t.Fatal("ResolveRecipe() on a non-GKE family accepted the mapping, want rejection")
	}
}

// TestConfig_GKETCPXOInterfaces covers the AICRConfig document path: the raw
// accessor and the RecipeResolveOptions projection.
func TestConfig_GKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	cfg, err := aicr.LoadConfig(context.Background(), writeConfig(t, `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    configuration:
      gke:
        tcpxoInterfaces:
          - {interfaceName: eth1, network: gpu-nic-0}
          - {interfaceName: eth2, network: gpu-nic-1}
          - {interfaceName: eth3, network: gpu-nic-2}
          - {interfaceName: eth4, network: gpu-nic-3}
          - {interfaceName: eth5, network: gpu-nic-4}
          - {interfaceName: eth6, network: gpu-nic-5}
          - {interfaceName: eth7, network: gpu-nic-6}
          - {interfaceName: eth8, network: gpu-nic-7}
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	mapping, set, err := cfg.RecipeGKETCPXOInterfaces()
	if err != nil {
		t.Fatalf("RecipeGKETCPXOInterfaces: %v", err)
	}
	if !set {
		t.Fatal("RecipeGKETCPXOInterfaces reported unset, but the document configures one")
	}
	if mapping != gkeTCPXOTestMappingString() {
		t.Errorf("RecipeGKETCPXOInterfaces = %q", mapping)
	}

	opts, err := cfg.RecipeResolveOptions()
	if err != nil {
		t.Fatalf("RecipeResolveOptions: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("RecipeResolveOptions returned no options; the GKE mapping was dropped")
	}

	var nilCfg *aicr.Config
	if _, set, err := nilCfg.RecipeGKETCPXOInterfaces(); err != nil || set {
		t.Errorf("nil Config: set=%v err=%v, want false/nil", set, err)
	}
}

// TestConfig_GKETCPXOInterfacesEmptyListRejected covers the explicitly-empty
// list: it must fail validation at load, not read as absent.
func TestConfig_GKETCPXOInterfacesEmptyListRejected(t *testing.T) {
	t.Parallel()

	_, err := aicr.LoadConfig(context.Background(), writeConfig(t, `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    configuration:
      gke:
        tcpxoInterfaces: []
`))
	if err == nil || !strings.Contains(err.Error(), "exactly 8 entries") {
		t.Fatalf("LoadConfig() error = %v, want rejection of the empty mapping", err)
	}
}

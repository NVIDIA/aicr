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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// TestCheckRecipeIdentity pins the content binding: pointer/predicate
// identity claims must derive from the manifest-verified recipe bytes --
// suffix heuristics are name-spoofable (a recipe named ...-ubuntu-training
// "satisfies" a fabricated profile ubuntu=training).
func TestCheckRecipeIdentity(t *testing.T) {
	const recipeYAML = `kind: RecipeResult
apiVersion: aicr.run/v1alpha2
criteria:
  service: eks
  accelerator: gb200
  os: ubuntu
  intent: training
`
	digest, err := attestation.SubjectDigest([]byte(recipeYAML))
	if err != nil {
		t.Fatal(err)
	}
	pred := func() *attestation.Predicate {
		p := &attestation.Predicate{}
		p.Recipe.Name = "gb200-eks-ubuntu-training"
		p.Recipe.Digest = digest
		return p
	}
	ptr := func(profile string) *attestation.Pointer {
		return &attestation.Pointer{Recipe: "gb200-eks-ubuntu-training", Profile: profile}
	}

	tests := []struct {
		name    string
		profile string                         // pointer Profile field
		mutate  func(p *attestation.Predicate) // optional predicate mutation
		wantErr string                         // "" = must pass; otherwise required error substring
	}{
		{
			name: "clean unprofiled",
		},
		{
			// The exact spoof: the name naturally ends -ubuntu-training,
			// so a suffix heuristic accepts profile ubuntu=training; the
			// content binding must not.
			name:    "fabricated-suffix profile",
			profile: "ubuntu=training",
			wantErr: "selection",
		},
		{
			name: "digest mismatch",
			mutate: func(p *attestation.Predicate) {
				p.Recipe.Digest = "sha256:" + strings.Repeat("0", 64)
			},
			wantErr: "digest",
		},
		{
			name: "predicate name mismatch",
			mutate: func(p *attestation.Predicate) {
				p.Recipe.Name = "h100-aks-ubuntu-training"
			},
			wantErr: "name",
		},
		{
			// A v2 profile block over an UNPROFILED recipe is a presence
			// mismatch.
			name: "profile block over unprofiled recipe",
			mutate: func(p *attestation.Predicate) {
				p.Profile = &attestation.ProfilePredicate{
					Selection:                "gpuStack=gke-default",
					PolicyDescriptorIdentity: allocpolicy.IdentityFor(nil),
				}
			},
			wantErr: "unprofiled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := pred()
			if tt.mutate != nil {
				tt.mutate(p)
			}
			err := checkRecipeIdentity([]byte(recipeYAML), ptr(tt.profile), p)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkRecipeIdentity() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkRecipeIdentity() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestCheckRecipeIdentity_ProfileBinding pins the predicate-profile
// binding on a PROFILED recipe: presence both ways, exact selection and
// advertiser, and the recipe-scoped descriptor-identity currentness — on
// this path regardless of whether the predicate came from the unsigned
// statement or the Sigstore-verified DSSE payload (both flow through
// checkRecipeIdentity).
func TestCheckRecipeIdentity_ProfileBinding(t *testing.T) {
	const recipeYAML = `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: gpuStack
    value: gke-default
    advertiser: external
    ownedPaths:
      gpu-operator:
        - devicePlugin.enabled
        - enabled
criteria:
  service: gke
  accelerator: h100
  os: cos
  intent: training
componentRefs:
  - name: gpu-operator
`
	digest, err := attestation.SubjectDigest([]byte(recipeYAML))
	if err != nil {
		t.Fatal(err)
	}
	// The recipe's closure contributes only the enabled gpu-operator entry.
	wantIdentity := allocpolicy.IdentityFor([]allocpolicy.Entry{{
		Component:     allocpolicy.ComponentGPUOperator,
		SelectorPaths: allocpolicy.SelectorPaths(allocpolicy.ComponentGPUOperator),
	}})
	const name = "h100-gke-cos-training-gpustack-gke-default"
	pred := func() *attestation.Predicate {
		p := &attestation.Predicate{}
		p.Recipe.Name = name
		p.Recipe.Digest = digest
		p.Profile = &attestation.ProfilePredicate{
			Selection:                "gpuStack=gke-default",
			Advertiser:               allocpolicy.AdvertiserExternal,
			PolicyDescriptorIdentity: wantIdentity,
		}
		return p
	}
	ptr := &attestation.Pointer{Recipe: name, Profile: "gpuStack=gke-default"}

	tests := []struct {
		name    string
		mutate  func(p *attestation.Predicate)
		wantErr string // "" means the coherent binding must verify
	}{
		{"coherent profiled bundle verifies",
			func(*attestation.Predicate) {}, ""},
		// A pre-cut-over v1 predicate (no profile block) over the
		// profiled recipe must fail the presence binding.
		{"v1 predicate over profiled recipe rejected",
			func(p *attestation.Predicate) { p.Profile = nil },
			"no profile block"},
		{"selection mismatch rejected",
			func(p *attestation.Predicate) {
				p.Profile.Selection = "gpuStack=bundle-installer"
			}, "selection"},
		{"advertiser mismatch rejected",
			func(p *attestation.Predicate) { p.Profile.Advertiser = "" },
			"advertiser"},
		// Stale identity: recording the FULL global descriptor (or any
		// other value) instead of the recipe-scoped identity is
		// historical-only.
		{"stale descriptor identity rejected",
			func(p *attestation.Predicate) {
				p.Profile.PolicyDescriptorIdentity = allocpolicy.Identity()
			}, "historical-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := pred()
			tt.mutate(p)
			err := checkRecipeIdentity([]byte(recipeYAML), ptr, p)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkRecipeIdentity() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkRecipeIdentity() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestCheckRecipeIdentity_RecipeSwapAfterInventoryRejected pins the
// TOCTOU closure (CWE-367): identity binding must consume the exact bytes
// the inventory pass hashed, never a path-based reread of the caller-owned
// bundle directory. The regression is deterministic: inventory verifies and
// captures recipe A, the on-disk recipe.yaml is then replaced with recipe B,
// and a predicate matching B is presented. With the pre-fix reread, identity
// read the swapped file and accepted B against predicate B (false PASS,
// proven by the counterfactual subcheck below); with capture, identity binds
// the inventory-verified A bytes and rejects predicate B.
func TestCheckRecipeIdentity_RecipeSwapAfterInventoryRejected(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	captured, rows, err := checkInventoryCaptureRecipe(context.Background(),
		&MaterializedBundle{BundleDir: summary}, readManifestDigest(t, summary))
	if err != nil || len(rows) > 0 {
		t.Fatalf("checkInventoryCaptureRecipe() = rows %v, err %v; want clean pass", rows, err)
	}
	recipeA, err := os.ReadFile(filepath.Join(summary, attestation.RecipeFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(captured, recipeA) {
		t.Fatalf("captured recipe bytes differ from the manifest-verified recipe.yaml")
	}

	// Swap: replace the caller-owned recipe.yaml with recipe B and present
	// a predicate that matches B exactly.
	const recipeB = `kind: RecipeResult
apiVersion: aicr.run/v1alpha2
criteria:
  service: gke
  accelerator: h100
  os: cos
  intent: training
`
	if werr := os.WriteFile(filepath.Join(summary, attestation.RecipeFilename), []byte(recipeB), 0o600); werr != nil {
		t.Fatal(werr)
	}
	digestB, err := attestation.SubjectDigest([]byte(recipeB))
	if err != nil {
		t.Fatal(err)
	}
	predB := &attestation.Predicate{}
	predB.Recipe.Name = "h100-gke-cos-training"
	predB.Recipe.Digest = digestB

	// Counterfactual: the swapped on-disk bytes DO satisfy predicate B —
	// exactly what the pre-fix path-based reread accepted. This subcheck
	// keeps the regression honest: if it ever fails, the rejection below
	// no longer proves anything about the swap.
	if cErr := checkRecipeIdentity([]byte(recipeB), nil, predB); cErr != nil {
		t.Fatalf("swapped bytes should satisfy predicate B (the false-PASS shape): %v", cErr)
	}

	// The fix: identity binds the inventory-verified bytes, so predicate B
	// is rejected against captured recipe A.
	if idErr := checkRecipeIdentity(captured, nil, predB); idErr == nil {
		t.Fatal("checkRecipeIdentity(captured A bytes, predicate B) = nil, want rejection: identity accepted bytes the manifest never covered")
	}

	// And a nil capture (no manifest-verified recipe) must fail closed,
	// never fall back to a path read of the swapped file.
	if nilErr := checkRecipeIdentity(nil, nil, predB); nilErr == nil {
		t.Fatal("checkRecipeIdentity(nil recipe bytes) = nil, want fail-closed rejection")
	}
}

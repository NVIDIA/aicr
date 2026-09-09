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

package attestation

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// aksProfiledOverlayPath is the embedded catalog's profile-bearing AKS
// leaf overlay (the gpuStack declaration lives on its aks.yaml ancestor),
// addressed relative to this package directory.
const aksProfiledOverlayPath = "../../../recipes/overlays/h100-aks-ubuntu-training.yaml"

func TestComputeRecipeDigestWithProfile_SelectionChangesDigest(t *testing.T) {
	ctx := context.Background()
	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")

	defaultDigest, err := ComputeRecipeDigest(ctx, dp, aksProfiledOverlayPath, "", "vtest")
	if err != nil {
		t.Fatalf("ComputeRecipeDigest(default): %v", err)
	}
	operatorDigest, err := ComputeRecipeDigestWithProfile(
		ctx, dp, aksProfiledOverlayPath, "", "vtest", "gpuStack=operator-managed")
	if err != nil {
		t.Fatalf("ComputeRecipeDigestWithProfile(gpuStack=operator-managed): %v", err)
	}
	if defaultDigest == operatorDigest {
		t.Errorf("default and gpuStack=operator-managed digests are equal (%q); the selection must be a digest input", defaultDigest)
	}

	// The explicit default selection must equal the implicit default: the
	// digest is a function of the hydrated selection, not of how it was
	// requested.
	explicitDefault, err := ComputeRecipeDigestWithProfile(
		ctx, dp, aksProfiledOverlayPath, "", "vtest", "gpuStack=azure-managed")
	if err != nil {
		t.Fatalf("ComputeRecipeDigestWithProfile(gpuStack=azure-managed): %v", err)
	}
	if explicitDefault != defaultDigest {
		t.Errorf("explicit default selection digest %q != implicit default digest %q",
			explicitDefault, defaultDigest)
	}
}

func TestComputeRecipeDigest_MatchesGeneratedRecipe(t *testing.T) {
	ctx := context.Background()
	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")

	overlayDigest, err := ComputeRecipeDigest(ctx, dp, "../../../recipes/overlays/gb200-eks-ubuntu-training.yaml", "", "vtest")
	if err != nil {
		t.Fatalf("ComputeRecipeDigest(overlay): %v", err)
	}

	criteria := recipe.NewCriteria()
	criteria.Service = recipe.CriteriaServiceEKS
	criteria.Accelerator = recipe.CriteriaAcceleratorGB200
	criteria.OS = recipe.CriteriaOSUbuntu
	criteria.Intent = recipe.CriteriaIntentTraining
	builder := recipe.NewBuilder(recipe.WithVersion("vtest"), recipe.WithDataProvider(dp))
	generated, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "generated.yaml")
	generatedYAML, err := serializer.MarshalYAMLDeterministic(generated)
	if err != nil {
		t.Fatalf("MarshalYAMLDeterministic: %v", err)
	}
	err = os.WriteFile(path, generatedYAML, 0o600)
	if err != nil {
		t.Fatalf("write generated recipe: %v", err)
	}
	generatedDigest, err := ComputeRecipeDigest(ctx, dp, path, "", "vtest")
	if err != nil {
		t.Fatalf("ComputeRecipeDigest(generated): %v", err)
	}

	if overlayDigest != generatedDigest {
		t.Errorf("overlay digest %q != generated recipe digest %q", overlayDigest, generatedDigest)
	}
}

func TestComputeRecipeDigestWithProfile_MatchesBuilderHydration(t *testing.T) {
	ctx := context.Background()
	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")

	overlayDigest, err := ComputeRecipeDigestWithProfile(
		ctx, dp, aksProfiledOverlayPath, "", "vtest", "gpuStack=operator-managed")
	if err != nil {
		t.Fatalf("ComputeRecipeDigestWithProfile: %v", err)
	}

	criteria := recipe.NewCriteria()
	criteria.Service = recipe.CriteriaServiceAKS
	criteria.Accelerator = recipe.CriteriaAcceleratorH100
	criteria.OS = recipe.CriteriaOSUbuntu
	criteria.Intent = recipe.CriteriaIntentTraining
	builder := recipe.NewBuilder(recipe.WithVersion("vtest"), recipe.WithDataProvider(dp))
	rec, err := builder.BuildFromCriteriaWithProfile(ctx, criteria, "gpuStack=operator-managed")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile: %v", err)
	}
	recipeYAML, err := serializer.MarshalYAMLDeterministic(rec)
	if err != nil {
		t.Fatalf("MarshalYAMLDeterministic: %v", err)
	}
	builderDigest, err := SubjectDigest(recipeYAML)
	if err != nil {
		t.Fatalf("SubjectDigest: %v", err)
	}
	if overlayDigest != builderDigest {
		t.Errorf("overlay-path digest %q != builder-hydrated digest %q", overlayDigest, builderDigest)
	}
}

func TestComputeRecipeDigestWithProfile_RejectsFullResultInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recipe.yaml")
	body := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\ncriteria:\n  service: aks\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	_, err := ComputeRecipeDigestWithProfile(context.Background(), dp, path, "", "vtest", "gpuStack=operator-managed")
	if err == nil || !strings.Contains(err.Error(), "already baked") {
		t.Fatalf("error = %v, want baked-in selection rejection", err)
	}
	if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
}

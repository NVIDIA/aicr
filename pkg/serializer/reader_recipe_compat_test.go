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

package serializer_test

import (
	"os"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func writeLegacyRecipeFile(t *testing.T, pattern, content string) string {
	t.Helper()

	tmpfile, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpfile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	return tmpfile.Name()
}

func assertLegacyRecipeLoads(t *testing.T, path string) {
	t.Helper()
	defer os.Remove(path)

	result, err := serializer.FromFile[recipe.RecipeResult](path)
	if err != nil {
		t.Fatalf("FromFile failed for legacy recipe: %v", err)
	}

	if len(result.Metadata.ExcludedOverlays) != 1 {
		t.Fatalf("expected 1 excluded overlay, got %d", len(result.Metadata.ExcludedOverlays))
	}
	if result.Metadata.ExcludedOverlays[0].Name != "h100-eks-ubuntu-training" {
		t.Fatalf("unexpected excluded overlay name: %+v", result.Metadata.ExcludedOverlays[0])
	}
	if result.Metadata.ExcludedOverlays[0].Reason != "" {
		t.Fatalf("legacy excluded overlay reason should be empty, got %q", result.Metadata.ExcludedOverlays[0].Reason)
	}
}

func TestFromFile_LegacyRecipeExcludedOverlaysYAML(t *testing.T) {
	path := writeLegacyRecipeFile(t, "recipe-legacy*.yaml", `kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  excludedOverlays:
    - h100-eks-ubuntu-training
componentRefs: []
deploymentOrder: []
`)

	assertLegacyRecipeLoads(t, path)
}

func TestFromFile_LegacyRecipeExcludedOverlaysJSON(t *testing.T) {
	path := writeLegacyRecipeFile(t, "recipe-legacy*.json", `{
  "kind": "RecipeResult",
  "apiVersion": "aicr.nvidia.com/v1alpha1",
  "metadata": {
    "excludedOverlays": [
      "h100-eks-ubuntu-training"
    ]
  },
  "componentRefs": [],
  "deploymentOrder": []
}`)

	assertLegacyRecipeLoads(t, path)
}

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
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), []byte(recipeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
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

	if err := checkRecipeIdentity(dir, ptr(""), pred()); err != nil {
		t.Fatalf("clean unprofiled: %v", err)
	}

	// The exact spoof: the name naturally ends -ubuntu-training, so a
	// suffix heuristic accepts profile ubuntu=training; the content
	// binding must not.
	if err := checkRecipeIdentity(dir, ptr("ubuntu=training"), pred()); err == nil ||
		!strings.Contains(err.Error(), "selection") {

		t.Fatalf("fabricated-suffix profile accepted: %v", err)
	}

	badDigest := pred()
	badDigest.Recipe.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := checkRecipeIdentity(dir, ptr(""), badDigest); err == nil ||
		!strings.Contains(err.Error(), "digest") {

		t.Fatalf("digest mismatch accepted: %v", err)
	}

	badName := pred()
	badName.Recipe.Name = "h100-aks-ubuntu-training"
	if err := checkRecipeIdentity(dir, ptr(""), badName); err == nil ||
		!strings.Contains(err.Error(), "name") {

		t.Fatalf("predicate name mismatch accepted: %v", err)
	}
}

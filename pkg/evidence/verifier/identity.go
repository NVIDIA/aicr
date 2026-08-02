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
	"io"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// checkRecipeIdentity binds the pointer's and predicate's identity claims to
// the manifest-verified recipe.yaml CONTENT. Runs only after CheckInventory
// has proven the bytes are the ones the predicate (and any signature) binds.
// Suffix heuristics alone are spoofable — a recipe name that naturally ends
// in "-<x>-<y>" (e.g. ...-ubuntu-training) satisfies a fabricated
// "profile: x=y" — so identity is DERIVED from the decoded recipe and
// compared exactly:
//
//	pointer.profile  == ProfileSelectionString(recipe)  (incl. absence, case)
//	pointer.recipe   == RecipeNameFor(recipe)
//	predicate name   == RecipeNameFor(recipe)
//	predicate digest == canonical digest of recipe.yaml
func checkRecipeIdentity(bundleDir string, pointer *attestation.Pointer, pred *attestation.Predicate) error {
	path := filepath.Join(bundleDir, attestation.RecipeFilename)
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // manifest-verified bundle dir
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open bundle recipe.yaml for identity binding", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxRecipePOSTBytes+1))
	_ = f.Close() // read-only handle
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read bundle recipe.yaml for identity binding", err)
	}
	if int64(len(data)) > defaults.MaxRecipePOSTBytes {
		return errors.New(errors.ErrCodeInvalidRequest, "bundle recipe.yaml exceeds the identity-binding size cap")
	}

	rec, err := recipe.DecodeRecipeResult(data, serializer.FormatYAML)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "failed to decode bundle recipe.yaml for identity binding")
	}

	wantName := attestation.RecipeNameFor(rec)
	wantSel := attestation.ProfileSelectionString(rec)

	// A criteria-less legacy recipe derives no name ("" from RecipeNameFor);
	// name equality is only enforceable when derivable. The profile and
	// digest bindings below never depend on the name and always run — a
	// criteria-less recipe also has no selectedProfile, so a fabricated
	// pointer profile still fails the wantSel comparison.
	if wantName != "" && pred.Recipe.Name != wantName {
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate recipe name "+pred.Recipe.Name+" does not match the name derived from the bundle recipe ("+wantName+")")
	}
	// SubjectDigest canonicalizes the raw recipe.yaml bytes — the same
	// input the producer digested at emit time (never a decode/re-marshal
	// round trip, which would not be byte-stable across writers).
	wantDigest, err := attestation.SubjectDigest(data)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to digest bundle recipe", err)
	}
	if pred.Recipe.Digest != wantDigest {
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate recipe digest does not match the canonical digest of the bundle recipe")
	}
	if pointer != nil {
		if wantName != "" && pointer.Recipe != wantName {
			return errors.New(errors.ErrCodeInvalidRequest,
				"pointer.recipe "+pointer.Recipe+" does not match the name derived from the bundle recipe ("+wantName+")")
		}
		if pointer.Profile != wantSel {
			return errors.New(errors.ErrCodeInvalidRequest,
				"pointer.profile "+pointer.Profile+" does not match the bundle recipe's selection ("+wantSel+")")
		}
	}
	return nil
}

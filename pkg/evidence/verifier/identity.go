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
	"strconv"

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// checkRecipeIdentity binds the pointer's and predicate's identity claims to
// the manifest-verified recipe.yaml CONTENT: recipeYAML must be the exact
// bytes the inventory pass read and hashed (checkInventoryCaptureRecipe),
// never a path-based reread — for InputFormDir the bundle directory is
// caller-owned, so reopening recipe.yaml after the inventory check is a
// TOCTOU window (CWE-367) in which a writer can swap the file and have
// identity accept bytes the manifest never covered. Runs only after the
// inventory pass has proven the bytes are the ones the predicate (and any
// signature) binds.
// Suffix heuristics alone are spoofable — a recipe name that naturally ends
// in "-<x>-<y>" (e.g. ...-ubuntu-training) satisfies a fabricated
// "profile: x=y" — so identity is DERIVED from the decoded recipe and
// compared exactly:
//
//	pointer.profile  == ProfileSelectionString(recipe)  (incl. absence, case)
//	pointer.recipe   == RecipeNameFor(recipe)
//	predicate name   == RecipeNameFor(recipe)
//	predicate digest == canonical digest of recipe.yaml
//	predicate profile block == recipe's metadata.selectedProfile
//	  (presence both ways, exact name=value selection, advertiser) and its
//	  policyDescriptorIdentity == the recipe-scoped descriptor identity
//	  recomputed from the decoded recipe (ADR-015 descriptor-currentness).
//	  Running the currentness check here — not on the predicate-parse path —
//	  covers both the unsigned-statement and the Sigstore-verified predicate
//	  sources, and compares like-for-like against the recipe that was
//	  actually attested.
func checkRecipeIdentity(recipeYAML []byte, pointer *attestation.Pointer, pred *attestation.Predicate) error {
	if len(recipeYAML) == 0 {
		// The inventory pass captured nothing: the manifest carries no
		// recipe.yaml entry. Identity cannot bind without manifest-verified
		// recipe bytes — fail closed rather than fall back to a path read.
		return errors.New(errors.ErrCodeInvalidRequest,
			"bundle manifest carries no verified recipe.yaml — identity binding requires the manifest-covered recipe bytes")
	}

	rec, err := recipe.DecodeRecipeResult(recipeYAML, serializer.FormatYAML)
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
	wantDigest, err := attestation.SubjectDigest(recipeYAML)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to digest bundle recipe", err)
	}
	if pred.Recipe.Digest != wantDigest {
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate recipe digest does not match the canonical digest of the bundle recipe")
	}
	if profErr := checkPredicateProfileBinding(rec, pred, wantSel); profErr != nil {
		return profErr
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

// checkPredicateProfileBinding binds the predicate's profile claims to the
// manifest-verified recipe's metadata.selectedProfile: presence must agree
// in both directions, the selection and advertiser must match exactly, and
// the recorded policy-descriptor identity must equal the recipe-scoped
// identity recomputed from the decoded recipe's closure-contributing
// descriptor entries (ADR-015 descriptor-currentness). Without this, a
// pre-cut-over v1 statement over a profiled recipe — or a v2 payload
// claiming another profile value or advertiser — would verify.
func checkPredicateProfileBinding(rec *recipe.RecipeResult, pred *attestation.Predicate, wantSel string) error {
	selected := rec.Metadata.SelectedProfile
	switch {
	case selected == nil && pred.Profile == nil:
		return nil
	case selected == nil:
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate carries a profile block ("+pred.Profile.Selection+
				") but the bundle recipe is unprofiled")
	case pred.Profile == nil:
		return errors.New(errors.ErrCodeInvalidRequest,
			"bundle recipe carries selectedProfile "+wantSel+
				" but the predicate has no profile block — profiled evidence requires the "+
				attestation.PredicateTypeV2+" predicate; regenerate and re-sign it (ADR-015)")
	}
	if pred.Profile.Selection != wantSel {
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate profile selection "+pred.Profile.Selection+
				" does not match the bundle recipe's selection ("+wantSel+")")
	}
	if pred.Profile.Advertiser != selected.Advertiser {
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate profile advertiser "+strconv.Quote(pred.Profile.Advertiser)+
				" does not match the bundle recipe's advertiser "+strconv.Quote(selected.Advertiser))
	}
	wantIdentity := allocpolicy.IdentityFor(rec.ClosureDescriptorEntries())
	if pred.Profile.PolicyDescriptorIdentity != wantIdentity {
		return errors.New(errors.ErrCodeInvalidRequest,
			"profiled evidence records policy-descriptor identity "+pred.Profile.PolicyDescriptorIdentity+
				", which differs from the recipe-scoped identity recomputed from the bundle recipe ("+
				wantIdentity+") — the evidence is historical-only; regenerate and re-sign it "+
				"(ADR-015 descriptor-currentness)")
	}
	return nil
}

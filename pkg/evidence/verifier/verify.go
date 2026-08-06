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
	"context"
	"encoding/json"
	stderrors "errors"
	"path/filepath"
	"strconv"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// Step numbers recorded in StepResult.Step. Signature verification
// runs between materialize and predicate-parse so the predicate body
// downstream steps consume is the cryptographically anchored one when
// available. Constraint replay is reserved for a follow-up slice.
const (
	stepMaterialize = 1
	stepSignature   = 2
	stepPredicate   = 3
	stepInventory   = 4
	stepRender      = 5
)

var stepNames = map[int]string{
	stepMaterialize: "materialize-bundle",
	stepSignature:   "signature-verify",
	stepPredicate:   "predicate-parse",
	stepInventory:   "manifest-hash-check",
	stepRender:      "render-summary",
}

// Verify runs the verification pipeline. Returns a non-nil error only
// when verification could not begin (bad input, etc.); step-level
// failures are recorded in VerifyResult.Steps and reflected in Exit.
func Verify(ctx context.Context, opts VerifyOptions) (*VerifyResult, error) {
	if opts.Input == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "Input is required")
	}
	form, err := DetectInputFormContext(ctx, opts.Input)
	if err != nil {
		return nil, err
	}

	r := &VerifyResult{Input: form, Steps: make([]StepResult, 0, len(stepNames))}

	// Pointer file is loaded up front for the pointer input form so
	// materialization knows the OCI ref to pull from.
	var pointer *attestation.Pointer
	if form == InputFormPointer {
		p, perr := LoadAndValidatePointerContext(ctx, opts.Input)
		if perr != nil {
			return nil, perr
		}
		pointer = p
		r.Pointer = p
	}

	// Step 1 — materialize.
	mat, err := MaterializeBundle(ctx, opts, form, pointer)
	if err != nil {
		record(r, stepMaterialize, StepFailed, err.Error(), nil)
		recordFailure(r, stepMaterialize, err)
		return r, nil
	}
	defer mat.Cleanup()
	if mat.Digest != "" {
		r.BundleDigest = mat.Digest
	}
	record(r, stepMaterialize, StepPassed, "bundle at "+mat.BundleDir, nil)

	// Step 2 — signature verify. When attestation.intoto.jsonl is
	// present, sigstore-go anchors the DSSE-wrapped Statement to a
	// Fulcio cert + optional Rekor entry. The predicate inside that
	// verified Statement is the cryptographically authoritative value.
	verifiedPredicate, pendingSignature := stepSignatureCheck(ctx, r, mat, opts)

	// Step 3 — predicate parse. Prefer the predicate the signature
	// step produced (cryptographically anchored); fall back to the
	// bundle's unsigned statement.intoto.json when no signature was
	// attached. Either way the manifest digest comes from this value.
	pred, perr := resolvePredicate(ctx, verifiedPredicate, mat)
	if perr != nil {
		record(r, stepPredicate, StepFailed, perr.Error(), nil)
		recordFailure(r, stepPredicate, perr)
		return r, nil
	}
	r.Predicate = pred
	r.RecipeName = pred.Recipe.Name
	// Early pointer-to-predicate name check (fast fail with clear step
	// attribution). The AUTHORITATIVE identity binding — including the
	// profile claim, which suffix rules alone cannot secure against
	// name-collision spoofs — is checkRecipeIdentity after CheckInventory,
	// which derives name/selection/digest from the manifest-verified
	// recipe bytes.
	if pointer != nil && pointer.Recipe != pred.Recipe.Name {
		berr := errors.New(errors.ErrCodeInvalidRequest,
			"pointer.recipe "+pointer.Recipe+" does not match the predicate's recipe name "+
				pred.Recipe.Name+" — the pointer's identity (including any profile claim) must "+
				"be the signed bundle's identity")
		record(r, stepPredicate, StepFailed, berr.Error(), nil)
		recordFailure(r, stepPredicate, berr)
		return r, nil
	}
	source := "unsigned statement.intoto.json"
	if verifiedPredicate != nil {
		source = "verified DSSE payload"
	}
	record(r, stepPredicate, StepPassed,
		"predicate "+pred.SchemaVersion+" for recipe "+pred.Recipe.Name+
			" (from "+source+")", nil)

	// Step 4 — manifest hash check. Binds manifest.json to
	// predicate.Manifest.Digest, then every file in the manifest to its
	// recorded sha256.
	// The capture variant retains the exact recipe.yaml bytes the inventory
	// pass hashed, so the identity binding below cannot be raced by a
	// swap of the caller-owned file between the two steps (CWE-367).
	recipeYAML, mismatches, invErr := checkInventoryCaptureRecipe(ctx, mat, pred.Manifest.Digest)
	if invErr != nil {
		record(r, stepInventory, StepFailed, invErr.Error(), mismatches)
		recordFailure(r, stepInventory, invErr)
	} else if phaseRows, phaseErr := CheckPhaseDigestsContext(ctx, mat, pred); phaseErr != nil {
		// Manifest chain is intact, but the predicate's per-phase CTRFDigest
		// claim disagrees with the committed report — fail closed.
		record(r, stepInventory, StepFailed, phaseErr.Error(), phaseRows)
		recordFailure(r, stepInventory, phaseErr)
	} else if idErr := checkRecipeIdentity(recipeYAML, pointer, pred); idErr != nil {
		// Content-bound identity: the pointer/predicate recipe name,
		// profile claim (presence, selection, advertiser, descriptor
		// currentness), and digest must all derive from the
		// manifest-verified recipe bytes (suffix heuristics are
		// name-spoofable). Runs on both the unsigned-statement and the
		// Sigstore-verified predicate paths.
		record(r, stepInventory, StepFailed, idErr.Error(), nil)
		recordFailure(r, stepInventory, idErr)
	} else {
		record(r, stepInventory, StepPassed,
			"manifest digest matches predicate; bundle files, phase digests, and recipe identity (name/profile/digest) verified", nil)
	}

	// Surface recorded phase failures as the informational exit-1 signal.
	if r.Exit == ExitValidPassed && hasPhaseFailures(pred) {
		r.Exit = ExitValidPhaseFailures
	}

	// "Pending signature" is strictly an exit-0 state. Set it only once the
	// final exit is known, so an unsigned bundle that later failed predicate
	// parsing, the manifest hash check, or carries phase failures is reported
	// as that failure — never as a misleading pending result with a non-zero
	// exit.
	if r.Exit == ExitValidPassed && pendingSignature {
		r.Pending = true
	}

	record(r, stepRender, StepInformational, "report assembled", nil)
	return r, nil
}

// stepSignatureCheck runs step 2 and returns the cryptographically
// anchored predicate when the bundle is signed (nil otherwise) plus whether
// the bundle is unsigned (a candidate "pending signature" state). Side
// effects: records the step row, sets r.Signer, may update r.Exit. The
// caller decides whether to surface Pending, since that is only valid once
// the final exit is known.
//
// When the input is a pointer file with a signer claim, this step also
// cross-checks the pointer's claim against the actual cert. A
// malicious pointer that names a different signer than the bundle
// fails here.
func stepSignatureCheck(ctx context.Context, r *VerifyResult, mat *MaterializedBundle, opts VerifyOptions) (*attestation.Predicate, bool) {
	sig, sigErr := VerifySignature(ctx, mat, opts)

	var claimedSigner *attestation.PointerSigner
	if r.Pointer != nil && len(r.Pointer.Attestations) > 0 {
		claimedSigner = r.Pointer.Attestations[0].Signer
	}

	switch {
	case stderrors.Is(sigErr, ErrUnsignedBundle):
		// Pointer claims a signer but the bundle is unsigned → fail.
		if claimedSigner != nil {
			if ccErr := CrossCheckPointerSigner(claimedSigner, nil); ccErr != nil {
				record(r, stepSignature, StepFailed, ccErr.Error(), nil)
				recordFailure(r, stepSignature, ccErr)
				return nil, false
			}
		}
		// Unsigned with no signer claim: a candidate "pending signature"
		// state, not a failure. The bundle was pushed (often via --no-sign)
		// and awaits the signing leg; verification of the rest of the bundle
		// continues. The caller sets r.Pending only if the final exit is 0.
		record(r, stepSignature, StepSkipped, "no signature attached (unsigned bundle — pending signature)", nil)
		return nil, true
	case sigErr != nil:
		record(r, stepSignature, StepFailed, sigErr.Error(), nil)
		recordFailure(r, stepSignature, sigErr)
		return nil, false
	default:
		r.Signer = sig.Signer
		if ccErr := CrossCheckPointerSigner(claimedSigner, sig.Signer); ccErr != nil {
			record(r, stepSignature, StepFailed, ccErr.Error(), nil)
			recordFailure(r, stepSignature, ccErr)
			return nil, false
		}
		detail := "signer " + sig.Signer.Identity + " (issuer " + sig.Signer.Issuer + ")"
		var sub []KV
		if sig.Signer.RekorLogIndex != nil {
			sub = []KV{{Key: "rekorLogIndex",
				Value: strconv.FormatInt(*sig.Signer.RekorLogIndex, 10)}}
		}
		record(r, stepSignature, StepPassed, detail, sub)
		return sig.Predicate, false
	}
}

// resolvePredicate picks the predicate to use for downstream steps.
// Verified payload takes precedence; otherwise we fall back to the
// unsigned statement.intoto.json. Both shapes go through the same
// PredicateTypeV1 check.
func resolvePredicate(
	ctx context.Context,
	verified *attestation.Predicate,
	mat *MaterializedBundle,
) (*attestation.Predicate, error) {

	if verified != nil {
		return verified, nil
	}
	return loadUnsignedPredicate(ctx, mat)
}

func record(r *VerifyResult, step int, status StepStatus, detail string, sub []KV) {
	r.Steps = append(r.Steps, StepResult{
		Step: step, Name: stepNames[step], Status: status, Detail: detail, SubRows: sub,
	})
}

// loadUnsignedPredicate reads the bundle's unsigned in-toto Statement
// and returns the predicate body. Used when no Sigstore Bundle was
// emitted; the predicate is trusted as-is (self-consistency only).
func loadUnsignedPredicate(ctx context.Context, mat *MaterializedBundle) (*attestation.Predicate, error) {
	path := filepath.Join(mat.BundleDir, attestation.StatementFilename)
	body, err := readBoundedFile(ctx, path, "in-toto Statement", defaults.MaxAttestationFileBytes)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		PredicateType string                `json:"predicateType"`
		Predicate     attestation.Predicate `json:"predicate"`
	}
	if uErr := json.Unmarshal(body, &envelope); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "Statement is not valid JSON", uErr)
	}
	if cErr := attestation.ValidatePredicateTypeCoherence(envelope.PredicateType, &envelope.Predicate); cErr != nil {
		return nil, cErr
	}
	return &envelope.Predicate, nil
}

func hasPhaseFailures(pred *attestation.Predicate) bool {
	if pred == nil {
		return false
	}
	for _, p := range pred.Phases {
		if p.Failed > 0 {
			return true
		}
	}
	return false
}

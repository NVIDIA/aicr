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
	"os"
	"path/filepath"
	"strconv"

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
	form, err := DetectInputForm(opts.Input)
	if err != nil {
		return nil, err
	}

	r := &VerifyResult{Input: form, Steps: make([]StepResult, 0, len(stepNames))}

	// Pointer file is loaded up front for the pointer input form so
	// materialization knows the OCI ref to pull from.
	var pointer *attestation.Pointer
	if form == InputFormPointer {
		p, perr := LoadAndValidatePointer(opts.Input)
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
		r.Exit = ExitInvalid
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
	verifiedPredicate := stepSignatureCheck(ctx, r, mat, opts)

	// Step 3 — predicate parse. Prefer the predicate the signature
	// step produced (cryptographically anchored); fall back to the
	// bundle's unsigned statement.intoto.json when no signature was
	// attached. Either way the manifest digest comes from this value.
	pred, perr := resolvePredicate(verifiedPredicate, mat)
	if perr != nil {
		record(r, stepPredicate, StepFailed, perr.Error(), nil)
		r.Exit = ExitInvalid
		return r, nil
	}
	r.Predicate = pred
	r.RecipeName = pred.Recipe.Name
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
	mismatches, invErr := CheckInventory(ctx, mat, pred.Manifest.Digest)
	if invErr != nil {
		record(r, stepInventory, StepFailed, invErr.Error(), mismatches)
		r.Exit = ExitInvalid
	} else {
		record(r, stepInventory, StepPassed,
			"manifest digest matches predicate; all bundle files verified", nil)
	}

	// Surface recorded phase failures as the informational exit-1 signal.
	if r.Exit == ExitValidPassed && hasPhaseFailures(pred) {
		r.Exit = ExitValidPhaseFailures
	}

	record(r, stepRender, StepInformational, "report assembled", nil)
	return r, nil
}

// stepSignatureCheck runs step 2 and returns the cryptographically
// anchored predicate when the bundle is signed (nil otherwise). Side
// effects: records the step row, sets r.Signer, may update r.Exit.
//
// When the input is a pointer file with a signer claim, this step also
// cross-checks the pointer's claim against the actual cert. A
// malicious pointer that names a different signer than the bundle
// fails here.
func stepSignatureCheck(ctx context.Context, r *VerifyResult, mat *MaterializedBundle, opts VerifyOptions) *attestation.Predicate {
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
				r.Exit = ExitInvalid
				return nil
			}
		}
		record(r, stepSignature, StepSkipped, "no signature attached (unsigned bundle)", nil)
		return nil
	case sigErr != nil:
		record(r, stepSignature, StepFailed, sigErr.Error(), nil)
		r.Exit = ExitInvalid
		return nil
	default:
		r.Signer = sig.Signer
		if ccErr := CrossCheckPointerSigner(claimedSigner, sig.Signer); ccErr != nil {
			record(r, stepSignature, StepFailed, ccErr.Error(), nil)
			r.Exit = ExitInvalid
			return nil
		}
		detail := "signer " + sig.Signer.Identity + " (issuer " + sig.Signer.Issuer + ")"
		var sub []KV
		if sig.Signer.RekorLogIndex != nil {
			sub = []KV{{Key: "rekorLogIndex",
				Value: strconv.FormatInt(*sig.Signer.RekorLogIndex, 10)}}
		}
		record(r, stepSignature, StepPassed, detail, sub)
		return sig.Predicate
	}
}

// resolvePredicate picks the predicate to use for downstream steps.
// Verified payload takes precedence; otherwise we fall back to the
// unsigned statement.intoto.json. Both shapes go through the same
// PredicateTypeV1 check.
func resolvePredicate(verified *attestation.Predicate, mat *MaterializedBundle) (*attestation.Predicate, error) {
	if verified != nil {
		return verified, nil
	}
	return loadUnsignedPredicate(mat)
}

func record(r *VerifyResult, step int, status StepStatus, detail string, sub []KV) {
	r.Steps = append(r.Steps, StepResult{
		Step: step, Name: stepNames[step], Status: status, Detail: detail, SubRows: sub,
	})
}

// loadUnsignedPredicate reads the bundle's unsigned in-toto Statement
// and returns the predicate body. Used when no Sigstore Bundle was
// emitted; the predicate is trusted as-is (self-consistency only).
func loadUnsignedPredicate(mat *MaterializedBundle) (*attestation.Predicate, error) {
	path := filepath.Join(mat.BundleDir, attestation.StatementFilename)
	body, err := os.ReadFile(path) //nolint:gosec // bundle-local path
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read in-toto Statement", err)
	}
	var envelope struct {
		PredicateType string                `json:"predicateType"`
		Predicate     attestation.Predicate `json:"predicate"`
	}
	if uErr := json.Unmarshal(body, &envelope); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "Statement is not valid JSON", uErr)
	}
	if envelope.PredicateType != attestation.PredicateTypeV1 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"unexpected predicateType "+envelope.PredicateType)
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

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
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// Step numbers — first slice runs three steps. Subsequent PRs insert
// signature verification (between materialize and inventory) and
// inline constraint replay (after inventory); the render step stays
// last. ADR-007 enumerates twelve steps; see doc.go for the mapping.
const (
	stepMaterialize = 1
	stepInventory   = 2
	stepRender      = 3
)

var stepNames = map[int]string{
	stepMaterialize: "materialize-bundle",
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

	// Step 1 — materialize.
	mat, err := MaterializeBundle(ctx, opts, form)
	if err != nil {
		record(r, stepMaterialize, StepFailed, err.Error(), nil)
		r.Exit = ExitInvalid
		return r, nil
	}
	defer mat.Cleanup()
	record(r, stepMaterialize, StepPassed, "bundle at "+mat.BundleDir, nil)

	// Parse the predicate so display steps can surface signed fields.
	// Signature verification ships in a later slice; for now the
	// predicate is read but not cryptographically verified — callers
	// see this via the empty Signer field in the rendered report.
	pred, perr := loadPredicate(mat)
	if perr != nil {
		record(r, stepInventory, StepFailed, "could not parse predicate: "+perr.Error(), nil)
		r.Exit = ExitInvalid
		return r, nil
	}
	r.Predicate = pred
	r.RecipeName = pred.Recipe.Name

	// Step 2 — manifest hash check (covers every file in the bundle).
	mismatches, invErr := CheckInventory(mat)
	if invErr != nil {
		record(r, stepInventory, StepFailed, invErr.Error(), mismatches)
		r.Exit = ExitInvalid
	} else {
		record(r, stepInventory, StepPassed,
			"all bundle files verified against manifest.json", nil)
	}

	// Surface recorded phase failures as the informational exit-1 signal.
	if r.Exit == ExitValidPassed && hasPhaseFailures(pred) {
		r.Exit = ExitValidPhaseFailures
	}

	record(r, stepRender, StepInformational, "report assembled", nil)
	return r, nil
}

func record(r *VerifyResult, step int, status StepStatus, detail string, sub []KV) {
	r.Steps = append(r.Steps, StepResult{
		Step: step, Name: stepNames[step], Status: status, Detail: detail, SubRows: sub,
	})
}

// loadPredicate reads the bundle's unsigned in-toto Statement and
// returns the predicate body. The signature slice (PR 3) will
// cryptographically bind this content; for now we trust the file.
func loadPredicate(mat *MaterializedBundle) (*attestation.Predicate, error) {
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

// WriteMarkdown is a convenience for the CLI's --output-markdown flag.
func WriteMarkdown(path string, r *VerifyResult) error {
	if err := os.WriteFile(path, []byte(RenderMarkdown(r)), 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write markdown summary", err)
	}
	return nil
}

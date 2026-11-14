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
	"path/filepath"
	"slices"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// checkValidatorImages verifies that pred's validatorImages equal
// attestation.ValidatorImagesForPredicate over the reports of pred's phases,
// and returns a row per image only one side has. Each report must still match
// its CTRFDigest. A bundle whose reports record no images may list images by
// tag alone, the form older releases emit.
func checkValidatorImages(ctx context.Context, mat *MaterializedBundle, pred *attestation.Predicate) ([]KV, error) {
	var reports []*ctrf.Report
	for _, p := range attestation.AllPhases {
		ps, ok := pred.Phases[p]
		if !ok {
			continue
		}
		rel := attestation.CTRFRelPath(p)
		body, err := readBoundedFile(ctx,
			filepath.Join(mat.BundleDir, filepath.FromSlash(rel)),
			rel, defaults.MaxAttestationFileBytes)
		if err != nil {
			return nil, err
		}
		if got := attestation.HashBytesSHA256(body); got != ps.CTRFDigest {
			return []KV{{Key: rel, Value: "ctrfDigest mismatch (got " + got + ", want " + ps.CTRFDigest + ")"}},
				errors.New(errors.ErrCodeInvalidRequest, "phase report changed after its digest was checked")
		}
		var report ctrf.Report
		if uErr := json.Unmarshal(body, &report); uErr != nil {
			return []KV{{Key: rel, Value: "not a CTRF report"}},
				errors.Wrap(errors.ErrCodeInvalidRequest, "phase report "+rel+" is not valid CTRF JSON", uErr)
		}
		reports = append(reports, &report)
	}

	derived := attestation.ValidatorImagesForPredicate(reports)
	if slices.Equal(derived, pred.ValidatorImages) {
		return nil, nil
	}
	fromRecord := func(img attestation.ValidatorImage) bool { return img.Digest != "" || img.Platform != "" }
	if len(derived) == 0 && !slices.ContainsFunc(pred.ValidatorImages, fromRecord) {
		return nil, nil
	}

	var rows []KV
	recorded := make(map[attestation.ValidatorImage]bool, len(derived))
	for _, img := range derived {
		recorded[img] = true
	}
	listed := make(map[attestation.ValidatorImage]bool, len(pred.ValidatorImages))
	for _, img := range pred.ValidatorImages {
		listed[img] = true
		if !recorded[img] {
			rows = append(rows, KV{Key: validatorImageKey(img), Value: "listed by the predicate, recorded by no phase report"})
		}
	}
	for _, img := range derived {
		if !listed[img] {
			rows = append(rows, KV{Key: validatorImageKey(img), Value: "recorded by a phase report, not listed by the predicate"})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, KV{Key: "validatorImages", Value: "not sorted or not distinct"})
	}
	return rows, errors.New(errors.ErrCodeInvalidRequest,
		"predicate validatorImages do not match the images the phase reports recorded")
}

func validatorImageKey(img attestation.ValidatorImage) string {
	key := img.Image
	if img.Digest != "" {
		key += " " + img.Digest
	}
	if img.Platform != "" {
		key += " " + img.Platform
	}
	return key
}

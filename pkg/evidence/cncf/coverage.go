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

package cncf

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// CoverageGap is an applicable MUST requirement none of whose proof paths
// the recipe declares in full: a submission run of that recipe cannot
// establish it, whatever the checks find.
type CoverageGap struct {
	// RequirementID is the upstream requirement identifier.
	RequirementID string

	// Missing lists, per proof path, the checks of that path the recipe does
	// not declare, in page order; a requirement with one path has one entry.
	// Declaring every check of any one entry closes the gap.
	Missing [][]string
}

// String renders the gap as the requirement and the checks that would close
// it, alternatives joined with "or".
func (g CoverageGap) String() string {
	paths := make([]string, 0, len(g.Missing))
	for _, missing := range g.Missing {
		paths = append(paths, strings.Join(missing, ", "))
	}
	return g.RequirementID + ": missing " + strings.Join(paths, " or ")
}

// RecipeCoverageGaps compares the conformance checks a recipe declares with
// the checks the checklist at version v expects for every applicable MUST
// (notApplicable names the requirements declared not applicable, which are
// not required). A requirement is covered when every check of one of its
// proof paths is declared (alternativeProofs); a MUST AICR has no check for
// is reported by the aggregation as NOT TESTED and is not a gap here. The
// gaps are in checklist order; none means the recipe can establish every
// applicable MUST.
//
// Errors:
//   - ErrCodeInvalidRequest when v is unsupported.
func RecipeCoverageGaps(v Version, declared []string, notApplicable map[string]string) ([]CoverageGap, error) {
	cl, err := ChecklistFor(v)
	if err != nil {
		return nil, err
	}
	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}
	var gaps []CoverageGap
	for _, req := range cl.MustRequirements() {
		if _, ok := notApplicable[req.ID]; ok {
			continue
		}
		page := pageForRequirement(req.ID)
		if page == nil || len(page.Checks) == 0 {
			continue
		}
		var missing [][]string
		covered := false
		for _, path := range proofPaths(req.ID, page.Checks) {
			var absent []string
			for _, name := range path {
				if !declaredSet[name] {
					absent = append(absent, name)
				}
			}
			if len(absent) == 0 {
				covered = true
				break
			}
			missing = append(missing, absent)
		}
		if !covered {
			gaps = append(gaps, CoverageGap{RequirementID: req.ID, Missing: missing})
		}
	}
	return gaps, nil
}

// ValidateRecipeCoverage is RecipeCoverageGaps as a pre-flight verdict: nil
// when the recipe declares a complete proof path for every applicable MUST
// of the checklist at version v.
//
// Errors:
//   - ErrCodeInvalidRequest when v is unsupported, or when the recipe
//     cannot establish one or more applicable MUSTs; the message names each
//     requirement and the checks that would close its gap.
func ValidateRecipeCoverage(v Version, declared []string, notApplicable map[string]string) error {
	gaps, err := RecipeCoverageGaps(v, declared, notApplicable)
	if err != nil {
		return err
	}
	if len(gaps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(gaps))
	for _, g := range gaps {
		parts = append(parts, g.String())
	}
	return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
		"the recipe's conformance checks cannot establish %d applicable MUST requirement(s) of the %s checklist: %s; "+
			"add the checks to spec.validation.conformance.checks, or declare a conditional MUST not applicable under spec.validate.evidence.cncf.notApplicable",
		len(gaps), v, strings.Join(parts, "; ")))
}

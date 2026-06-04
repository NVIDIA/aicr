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

package validator

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// Phase re-exports pkg/validator/v1.Phase so callers that work in the
// pkg/validator orchestration layer do not have to import the wire
// package directly.
type Phase = v1.Phase

// Re-exported phase constants from pkg/validator/v1.
const (
	PhaseDeployment  = v1.PhaseDeployment
	PhasePerformance = v1.PhasePerformance
	PhaseConformance = v1.PhaseConformance
)

// PhaseOrder defines the execution order for ValidatePhases.
// All phases run by default; set Validator.FailFast to stop after the
// first phase that reports StatusFailed.
//
// Note: Readiness phase is NOT included. It remains in pkg/validator
// and uses inline constraint evaluation (no containers).
var PhaseOrder = []Phase{PhaseDeployment, PhasePerformance, PhaseConformance}

// PhaseAll is the wildcard string accepted by both the `aicr validate
// --phase` CLI flag and the spec.validate.execution.phases config field
// to mean "run every phase." It is not a Phase value — the CLI parser
// collapses it into a nil selection that ValidatePhases interprets as
// "run all phases."
const PhaseAll = "all"

// PhaseNames is the canonical user-facing vocabulary accepted by the
// --phase flag and spec.validate.execution.phases. The typed Phase
// constants in PhaseOrder plus the PhaseAll wildcard. Single source of
// truth so the CLI parser and the config-load validator stay in sync
// when a phase is added or removed.
var PhaseNames = []string{
	string(PhaseDeployment),
	string(PhasePerformance),
	string(PhaseConformance),
	PhaseAll,
}

// ParsePhase converts a user-facing phase name to its typed Phase value.
// Returns false for PhaseAll (the wildcard, which has no Phase value)
// and for unrecognized inputs. Callers that want to accept the wildcard
// handle it separately, typically by collapsing the whole selection to
// nil (= run every phase).
func ParsePhase(s string) (Phase, bool) {
	for _, p := range PhaseOrder {
		if string(p) == s {
			return p, true
		}
	}
	return "", false
}

// ParsePhaseSelection parses a list of user-facing phase names (from the
// `--phase` CLI flag or the spec.validate.execution.phases config field)
// into typed Phase values. The PhaseAll wildcard collapses the whole
// selection to nil (= run every phase), matching the documented "Default:
// all phases" behavior. PhaseAll is exclusive: combining it with any
// specific phase is a hard error rather than silently treating the
// selection as wildcard, so a typo like `--phase deployment --phase all`
// does not mask the user's mistake.
//
// Every entry is parsed before the wildcard collapse, so an invalid
// phase name surfaces an error even when "all" is also present.
func ParsePhaseSelection(phaseStrs []string) ([]Phase, error) {
	if len(phaseStrs) == 0 {
		return nil, nil
	}

	var (
		sawAll bool
		phases []Phase
		seen   = make(map[Phase]bool)
	)
	for _, s := range phaseStrs {
		if s == PhaseAll {
			sawAll = true
			continue
		}
		p, ok := ParsePhase(s)
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid phase %q: must be one of: %s",
					s, strings.Join(PhaseNames, ", ")))
		}
		if !seen[p] {
			phases = append(phases, p)
			seen[p] = true
		}
	}

	if sawAll && len(phases) > 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("phase %q cannot be combined with other phases", PhaseAll))
	}
	if sawAll {
		return nil, nil
	}
	return phases, nil
}

// WarnPhasesAgainstRecipe warns when a requested phase has no checks
// defined in the recipe. The phase will still run but produce 0 tests
// in the CTRF report. This is purely advisory — it emits slog warnings
// and never fails the run.
func WarnPhasesAgainstRecipe(phases []Phase, rec *recipe.RecipeResult) {
	if rec.Validation == nil {
		if len(phases) > 0 {
			slog.Warn("recipe has no validation section; requested phases will have no checks",
				"phases", phases)
		}
		return
	}

	if len(phases) == 0 {
		return
	}

	defined := make(map[Phase]bool)
	if rec.Validation.Deployment != nil && len(rec.Validation.Deployment.Checks) > 0 {
		defined[PhaseDeployment] = true
	}
	if rec.Validation.Performance != nil && len(rec.Validation.Performance.Checks) > 0 {
		defined[PhasePerformance] = true
	}
	if rec.Validation.Conformance != nil && len(rec.Validation.Conformance.Checks) > 0 {
		defined[PhaseConformance] = true
	}

	for _, p := range phases {
		if !defined[p] {
			slog.Warn("phase requested but no checks defined in recipe; phase will be empty",
				"phase", p)
		}
	}
}

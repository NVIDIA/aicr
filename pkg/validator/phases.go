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

import v1 "github.com/NVIDIA/aicr/pkg/api/validator/v1"

// Re-exported type from pkg/api/validator/v1 for backward compatibility.
type Phase = v1.Phase

// Re-exported constants from pkg/api/validator/v1 for backward compatibility.
const (
	PhaseDeployment  = v1.PhaseDeployment
	PhasePerformance = v1.PhasePerformance
	PhaseConformance = v1.PhaseConformance
)

// PhaseOrder defines the mandatory execution order.
// If a phase fails, subsequent phases are skipped.
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

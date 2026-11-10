// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package mirror

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/constraints/expr"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Recipe constraint names that pin the node OS.
const (
	osIDConstraintName      = "OS.release.ID"
	osVersionConstraintName = "OS.release.VERSION_ID"
)

// DriverTargetForRecipe derives the node OS and architecture an operator
// completes driver tags for. The OS comes from the recipe's exact
// OS.release.ID and OS.release.VERSION_ID constraints; a missing, ranged or
// compound constraint leaves its field empty and adds an explanatory note. The
// architecture follows from the accelerator criterion and stays empty when the
// accelerator does not determine it.
func DriverTargetForRecipe(rec *recipe.RecipeResult) (bom.DriverTarget, []string) {
	var target bom.DriverTarget
	var notes []string
	var note string
	target.OSID, note = exactConstraint(rec.Constraints, osIDConstraintName)
	notes = appendNonEmpty(notes, note)
	target.OSVersion, note = exactConstraint(rec.Constraints, osVersionConstraintName)
	notes = appendNonEmpty(notes, note)
	if rec.Criteria != nil {
		target.Arch = architectureForAccelerator(rec.Criteria.Accelerator)
	}
	return target, notes
}

// exactConstraint returns the value of the named constraint when it is a
// single exact match (no operator, or ==). Otherwise it returns an empty value
// and a note naming what the recipe has instead.
func exactConstraint(constraints []recipe.Constraint, name string) (value, note string) {
	for _, c := range constraints {
		if c.Name != name {
			continue
		}
		parsed, err := expr.ParseCompoundConstraint(c.Value)
		if err == nil && len(parsed.Alternatives) == 1 && len(parsed.Alternatives[0]) == 1 {
			term := parsed.Alternatives[0][0]
			if term.Operator == expr.OperatorExact || term.Operator == expr.OperatorEQ {
				return term.Value, ""
			}
		}
		return "", fmt.Sprintf("%s is %q", name, c.Value)
	}
	return "", name + " is not set"
}

func appendNonEmpty(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	return append(notes, note)
}

// architectureForAccelerator maps an accelerator criterion to the CPU
// architecture of its nodes: Grace-based platforms are arm64, the rest are
// amd64. It returns "" for any or an unrecognized accelerator.
func architectureForAccelerator(a recipe.CriteriaAcceleratorType) string {
	switch a {
	case recipe.CriteriaAcceleratorGB200, recipe.CriteriaAcceleratorGB300, recipe.CriteriaAcceleratorVR200:
		return bom.ArchARM64
	case recipe.CriteriaAcceleratorH100, recipe.CriteriaAcceleratorH200,
		recipe.CriteriaAcceleratorB200, recipe.CriteriaAcceleratorB300,
		recipe.CriteriaAcceleratorA100, recipe.CriteriaAcceleratorL40,
		recipe.CriteriaAcceleratorL40S, recipe.CriteriaAcceleratorRTXPro6000:
		return bom.ArchAMD64
	case recipe.CriteriaAcceleratorAny:
		return ""
	}
	return ""
}

// qualifyDrivers returns the published references for drivers. A driver that
// is switched off is skipped. One whose tag cannot be listed is omitted and
// named in ci.Warnings, so the air-gap relocation does not silently lack it.
func qualifyDrivers(
	drivers []bom.DriverImage,
	target bom.DriverTarget,
	notes []string,
	compName string,
	ci *ComponentImages,
) []string {

	var refs []string
	for _, d := range drivers {
		if d.Disabled {
			continue
		}
		ref, reason, ok := d.Qualify(target)
		if ok {
			refs = append(refs, ref)
			continue
		}
		slog.Warn("driver image tag cannot be resolved",
			logKeyComponent, compName, "image", d.Ref, "reason", reason)
		if !d.Precompiled && len(notes) > 0 {
			reason += " (" + strings.Join(notes, ", ") + ")"
		}
		ci.Warnings = append(ci.Warnings, fmt.Sprintf(
			"driver image %s not listed: the operator completes its tag as %s on each node, and %s; mirror the tag for your nodes",
			d.Ref, d.TagPattern(), reason))
	}
	return refs
}

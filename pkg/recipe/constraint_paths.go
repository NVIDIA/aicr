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

package recipe

import (
	stderrors "errors"
	"fmt"
	"maps"
	"sort"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Structured-error context keys added by constraint-path annotation.
const (
	ctxKeyFile     = "file"
	ctxKeyLocation = "location"
	ctxKeyIndex    = "index"
)

// Field locations reported when a constraint path is rejected.
const (
	locSpecConstraints      = "spec.constraints"
	locReadinessConstraints = "spec.validation.readiness.constraints"
	locProfileConstraints   = "spec.profile.values"
	locResultConstraints    = "constraints"
	locResultReadiness      = "validation.readiness.constraints"
)

// validateConstraintPaths rejects any constraint whose name is not an
// addressable measurement path (issue #1783).
//
// Without this gate a typo such as "K8s.server.versionn" parses cleanly and
// evaluates as ErrCodeNotFound, which evaluateOverlayConstraints treats as
// "reading absent from this snapshot — exclude gracefully". The overlay is
// silently dropped and resolution reports success with a partial recipe: a
// recipe passing because it skipped a gate.
//
// source is the file the constraints came from, empty when the caller has no
// originating file (an SDK-supplied RecipeResult). location names the field.
func validateConstraintPaths(cs []Constraint, source, location string) error {
	for i, c := range cs {
		if err := measurement.ValidatePath(c.Name); err != nil {
			return annotateConstraintPathErr(err, source, location, i)
		}
	}
	return nil
}

// validateSpecConstraintPaths validates every snapshot-evaluated constraint
// set a recipe spec can carry.
//
// Deliberately excludes validation.{deployment,performance,conformance}
// constraints: those are a different namespace, evaluated against a live
// cluster or a benchmark result rather than snapshot readings. Their names —
// "Deployment.gpu-operator.version", "nccl-all-reduce-bw", "WITH_WORKLOAD" —
// are not measurement paths and the catalog would reject every one.
func validateSpecConstraintPaths(spec *RecipeMetadataSpec, source string) error {
	if spec == nil {
		return nil
	}

	if err := validateConstraintPaths(spec.Constraints, source, locSpecConstraints); err != nil {
		return err
	}

	if spec.Validation != nil && spec.Validation.Readiness != nil {
		if err := validateConstraintPaths(
			spec.Validation.Readiness.Constraints, source, locReadinessConstraints); err != nil {
			return err
		}
	}

	if spec.Profile != nil {
		// Values is a map; iterate in sorted order so the reported failure is
		// deterministic across runs.
		names := make([]string, 0, len(spec.Profile.Values))
		for name := range spec.Profile.Values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			location := fmt.Sprintf("%s.%s.constraints", locProfileConstraints, name)
			if err := validateConstraintPaths(spec.Profile.Values[name].Constraints, source, location); err != nil {
				return err
			}
			location = fmt.Sprintf("%s.%s.readinessConstraints", locProfileConstraints, name)
			if err := validateConstraintPaths(spec.Profile.Values[name].ReadinessConstraints, source, location); err != nil {
				return err
			}
		}
	}

	return nil
}

// annotateConstraintPathErr adds file, field, and index to a ValidatePath
// error without losing anything the inner error carried.
//
// Neither stock helper works here. errors.PropagateOrWrap returns an already
// structured error unchanged, discarding the annotation entirely.
// errors.WrapWithContext stores only the context handed to it, and
// server.WriteErrorFromErr reads the OUTERMOST structured error's context —
// so a plain wrap would leave path, subtype, key, and suggestion invisible to
// every machine consumer. Merge explicitly.
//
// The inner code is preserved rather than forced to ErrCodeInvalidRequest: a
// catalog gap surfaces as ErrCodeInternal and must stay Internal, since the
// code drives HTTP status and the retryable flag.
func annotateConstraintPathErr(err error, source, location string, index int) error {
	label := fmt.Sprintf("%s[%d]", location, index)
	if source != "" {
		label = source + ": " + label
	}

	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) {
		return aicrerrors.WrapWithContext(aicrerrors.ErrCodeInternal, label, err, nil)
	}

	// Clone: se.Context belongs to the inner error and must not be mutated.
	ctx := make(map[string]any, len(se.Context)+3)
	maps.Copy(ctx, se.Context)
	if source != "" {
		ctx[ctxKeyFile] = source
	}
	ctx[ctxKeyLocation] = location
	ctx[ctxKeyIndex] = index

	return aicrerrors.WrapWithContext(se.Code, label, err, ctx)
}

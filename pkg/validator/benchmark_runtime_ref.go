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
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// benchmarkRuntimeRefTestdataDir is the --data-tree location a recipe's NCCL
// benchmark runtime template lives at, relative to the --data root. It mirrors
// the embedded validators/performance/testdata layout so an external recipe's
// runtime can be upstreamed by copying the file into the repo unchanged.
const benchmarkRuntimeRefTestdataDir = "validators/performance/testdata"

// resolveBenchmarkRuntimeRef lowers a recipe's nccl-benchmark-runtime-ref
// performance constraint into the nccl-benchmark-runtime carrier the in-pod
// validator reads. The ref is a bare "{accelerator}/{service}" value naming a
// runtime template the recipe ships in its --data tree at
// validators/performance/testdata/{accelerator}/{service}/runtime.yaml; its
// content is read through the recipe DataProvider and injected as
// nccl-benchmark-runtime, so the pod renders and runs it exactly as if it had
// been supplied inline (NVIDIA/aicr#1792). No ref, or a blank ref → no-op.
//
// Fails closed (ErrCodeInvalidRequest) on a malformed ref, a missing/unreadable/
// empty file, an absent DataProvider, or a ref set alongside an inline
// nccl-benchmark-runtime — a mis-typed ref must abort, never silently skip the
// benchmark, which is the exact failure mode this feature exists to eliminate.
func (v *Validator) resolveBenchmarkRuntimeRef(ctx context.Context, vi *v1.ValidationInput) error {
	if vi == nil || vi.Config.Performance == nil {
		return nil
	}
	cs := vi.Config.Performance.Constraints

	// Reject duplicate ref / carrier constraints before any first-match lookup —
	// a blank earlier entry must not hide a later non-blank duplicate (and the
	// overlay merge already collapses same-named constraints, so a duplicate here
	// is a malformed input). Exactly one of each is allowed.
	if n := v1.CountConstraint(cs, v1.PerfConstraintNCCLBenchmarkRuntimeRef); n > 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("declare at most one %s constraint (found %d)", v1.PerfConstraintNCCLBenchmarkRuntimeRef, n))
	}
	if n := v1.CountConstraint(cs, v1.PerfConstraintNCCLBenchmarkRuntime); n > 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("declare at most one %s constraint (found %d)", v1.PerfConstraintNCCLBenchmarkRuntime, n))
	}

	ref, ok := v1.FindConstraint(cs, v1.PerfConstraintNCCLBenchmarkRuntimeRef)
	if !ok {
		return nil
	}
	raw := strings.TrimSpace(ref.Value)
	if raw == "" {
		return nil
	}

	// A ref is mutually exclusive with an inline runtime and with a benchmark
	// profile — supply your own runtime OR borrow an embedded one, never both.
	// Both conflicts are pure recipe-authoring errors, so reject them here (before
	// any Job is deployed) rather than leaving the ref+profile case to fail late
	// in the pod.
	if inline, ok := v1.FindConstraint(cs, v1.PerfConstraintNCCLBenchmarkRuntime); ok && strings.TrimSpace(inline.Value) != "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s and an inline %s are both set — supply one",
				v1.PerfConstraintNCCLBenchmarkRuntimeRef, v1.PerfConstraintNCCLBenchmarkRuntime))
	}
	if profile, ok := v1.FindConstraint(cs, v1.PerfConstraintNCCLBenchmarkProfile); ok && strings.TrimSpace(profile.Value) != "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s and %s are mutually exclusive — supply a runtime or borrow an embedded profile, not both",
				v1.PerfConstraintNCCLBenchmarkRuntimeRef, v1.PerfConstraintNCCLBenchmarkProfile))
	}

	relPath, err := benchmarkRuntimeRefPath(raw)
	if err != nil {
		return err
	}

	if v.dataProvider == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s=%q is set but no --data source is available to resolve it",
				v1.PerfConstraintNCCLBenchmarkRuntimeRef, raw))
	}

	readCtx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()
	content, err := v.dataProvider.ReadFile(readCtx, relPath)
	if err != nil {
		// A canceled/deadline context surfaces as ErrCodeTimeout from the
		// provider — propagate it as a timeout rather than mislabeling a
		// transient fault as a bad recipe. Any other read failure (not-found,
		// internal) is a ref the recipe cannot resolve: a recipe-authoring
		// error the caller must fix, normalized to ErrCodeInvalidRequest.
		if errors.IsTransient(err) {
			// PropagateOrWrap keeps an already-coded provider error (e.g. the
			// ErrCodeTimeout the DataProvider returns on ctx cancellation) intact;
			// it only wraps a bare context error as ErrCodeTimeout with context.
			return errors.PropagateOrWrap(err, errors.ErrCodeTimeout,
				fmt.Sprintf("timed out resolving %s=%q (%q)",
					v1.PerfConstraintNCCLBenchmarkRuntimeRef, raw, relPath))
		}
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to resolve %s=%q: expected %q in the --data tree",
				v1.PerfConstraintNCCLBenchmarkRuntimeRef, raw, relPath), err)
	}
	if strings.TrimSpace(string(content)) == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s=%q resolved to an empty file (%q)",
				v1.PerfConstraintNCCLBenchmarkRuntimeRef, raw, relPath))
	}

	// Shape-check the resolved file against the same contract the pod enforces,
	// so a runtime that isn't a valid TrainingRuntime fails fast here (offline,
	// before any Job) instead of only in the pod after the Trainer install. The
	// check parses the raw pre-substitution content; ${VAR} placeholders live in
	// value positions and don't affect the identity / replicatedJob shape.
	if err := v1.ValidateBenchmarkRuntime(string(content)); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s=%q (%q) resolved to an invalid runtime", v1.PerfConstraintNCCLBenchmarkRuntimeRef, raw, relPath), err)
	}

	// Consume the ref: rebuild the constraint slice without it (and without any
	// pre-existing carrier — the non-blank case is already rejected above, so
	// only a blank one can reach here, and leaving it would shadow the resolved
	// value for first-match consumers like the pod) and append the resolved
	// carrier. Dropping the ref makes a second resolution pass a no-op
	// (idempotent — a re-invocation on the same input pointer no longer sees a
	// ref) and keeps the raw ref out of the pod's ConfigMap; the fresh slice also
	// avoids mutating a backing array shared via convertValidationPhase's
	// by-reference copy of Constraints. The result holds exactly one carrier.
	rebuilt := make([]recipe.Constraint, 0, len(vi.Config.Performance.Constraints))
	for _, c := range vi.Config.Performance.Constraints {
		if c.Name == v1.PerfConstraintNCCLBenchmarkRuntimeRef || c.Name == v1.PerfConstraintNCCLBenchmarkRuntime {
			continue
		}
		rebuilt = append(rebuilt, c)
	}
	rebuilt = append(rebuilt, recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntime, Value: string(content)})
	vi.Config.Performance.Constraints = rebuilt

	slog.Info("resolved NCCL benchmark runtime from --data",
		"ref", raw, "path", relPath, "bytes", len(content))
	return nil
}

// benchmarkRuntimeRefPath validates a "{accelerator}/{service}" ref and returns
// the --data-relative path of the runtime template it names. Rejects malformed
// or non-local refs fail-closed so a typo (or a traversal attempt) cannot
// resolve to an unexpected file.
func benchmarkRuntimeRefPath(ref string) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 {
		return "", invalidRuntimeRef(ref)
	}
	accelerator, service := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if !isSafeRefSegment(accelerator) || !isSafeRefSegment(service) {
		return "", invalidRuntimeRef(ref)
	}
	relPath := filepath.Join(benchmarkRuntimeRefTestdataDir, accelerator, service, "runtime.yaml")
	// Defense in depth against a segment that survives isSafeRefSegment but still
	// escapes the tree once joined/cleaned.
	if !filepath.IsLocal(relPath) || !strings.HasPrefix(relPath, benchmarkRuntimeRefTestdataDir+string(filepath.Separator)) {
		return "", invalidRuntimeRef(ref)
	}
	return relPath, nil
}

func invalidRuntimeRef(ref string) error {
	return errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("invalid %s=%q: must be \"{accelerator}/{service}\" (e.g. \"gb200/mycloud\")",
			v1.PerfConstraintNCCLBenchmarkRuntimeRef, ref))
}

// isSafeRefSegment reports whether a single ref path segment is a plain,
// non-empty name — no path separators, no "."/".." traversal.
func isSafeRefSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

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

package main

import (
	"fmt"
	"strings"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// perfConstraintNCCLBenchmarkRuntime is the resolved carrier constraint the pod
// reads: a complete Kubeflow TrainingRuntime (inline YAML) that the NCCL
// all-reduce benchmark renders in place of a baked-in testdata template.
//
// It is the terminal escape hatch of the data-driven validation vision
// (NVIDIA/aicr#1703, #1792): a private service+accelerator introduced entirely
// via `--data` has no entry in the compiled supportedNCCLCombinations matrix
// and — the gap nccl-benchmark-profile could not close — no embedded
// testdata/{accelerator}/{service} template to borrow either. Rather than
// requiring the validator binary to know the service, the recipe brings its own
// runtime, so `aicr validate` gates the benchmark keyed on the recipe's OWN
// criteria without any compiled applicability entry.
//
// Authors do not write this constraint directly. They ship the runtime as a
// file in the --data tree and reference it with nccl-benchmark-runtime-ref (a
// bare "{accelerator}/{service}" value); the orchestrator reads
// validators/performance/testdata/{accelerator}/{service}/runtime.yaml and
// lowers its content into this carrier before the pod runs (see
// pkg/validator/benchmark_runtime_ref.go). The pod is oblivious to that
// resolution — it just renders whatever content the carrier holds.
//
// When set, the runtime is rendered (with the generic ${NAMESPACE},
// ${WORKER_COUNT}, ${GPU_COUNT_PER_NODE}, ${GPU_COUNT}, ${TEST_TYPE},
// ${MIN_MESSAGE_SIZE}, ${MAX_MESSAGE_SIZE} substitutions) in place of any
// baked-in testdata template; the compiled applicability gate is bypassed (the
// recipe opted in explicitly); and the service-specific fabric plumbing
// (EFA/TCPXO/RDMA NIC discovery, the GB200-NVreg / GKE-TCPXO preflights, and
// NVLS IMEX provisioning) is skipped — the supplied runtime owns its own fabric
// wiring end to end. The name is shared with the orchestrator via
// pkg/validator/v1 so the write side and read side cannot drift. See
// nccl_all_reduce_bw_constraint.go (validateNcclAllReduceBw / applyNCCLResources)
// for the custom-path branches, and nccl_benchmark_profile.go for the
// borrow-an-embedded-template sibling.
const perfConstraintNCCLBenchmarkRuntime = v1.PerfConstraintNCCLBenchmarkRuntime

// perfConstraintNCCLBenchmarkRuntimeRef is the author-facing --data reference the
// orchestrator resolves into the carrier above. The pod only reads it to fail
// closed if it arrives unresolved (see resolveNCCLBenchmarkRuntime).
const perfConstraintNCCLBenchmarkRuntimeRef = v1.PerfConstraintNCCLBenchmarkRuntimeRef

// resolveNCCLBenchmarkRuntime reads the optional nccl-benchmark-runtime
// performance constraint. Returns "" when the constraint is absent or blank —
// callers fall back to the criteria/profile-selected embedded template. A
// non-empty value that is not a well-formed Kubeflow TrainingRuntime fails
// closed with ErrCodeInvalidRequest: a malformed runtime must abort the check,
// never silently skip it, which is the exact failure mode this feature exists
// to eliminate.
func resolveNCCLBenchmarkRuntime(ctx *validators.Context) (string, error) {
	// Reject duplicates before any first-match lookup — a blank earlier entry
	// must not hide a later non-blank one. The orchestrator already dedups, but
	// the pod is the last line of defense (a hand-built ValidationInput, or the
	// Plan()/EnsureDataConfigMaps path, may not have gone through it).
	if n := countPerformanceConstraint(ctx, perfConstraintNCCLBenchmarkRuntime); n > 1 {
		return "", aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("declare at most one %s constraint (found %d)", perfConstraintNCCLBenchmarkRuntime, n))
	}
	if n := countPerformanceConstraint(ctx, perfConstraintNCCLBenchmarkRuntimeRef); n > 1 {
		return "", aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("declare at most one %s constraint (found %d)", perfConstraintNCCLBenchmarkRuntimeRef, n))
	}

	carrier := ""
	if c, ok := findPerformanceConstraint(ctx, perfConstraintNCCLBenchmarkRuntime); ok {
		carrier = strings.TrimSpace(c.Value)
	}
	if carrier == "" {
		// Defense in depth: the orchestrator resolves nccl-benchmark-runtime-ref
		// into the carrier and drops the ref before the pod runs. If a non-blank
		// ref still reaches the pod with no carrier, resolution was skipped (e.g.
		// an external controller that built the validation ConfigMap via
		// EnsureDataConfigMaps/Plan without ValidatePhases). Fail closed rather
		// than silently skipping the benchmark the recipe explicitly opted into —
		// the exact failure mode this feature exists to eliminate.
		if ref, ok := findPerformanceConstraint(ctx, perfConstraintNCCLBenchmarkRuntimeRef); ok && strings.TrimSpace(ref.Value) != "" {
			return "", aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s=%q reached the validator unresolved: the orchestrator must lower it into %s before the pod runs",
					perfConstraintNCCLBenchmarkRuntimeRef, strings.TrimSpace(ref.Value), perfConstraintNCCLBenchmarkRuntime))
		}
		return "", nil
	}
	// Shape-check the carrier via the contract shared with the orchestrator
	// (pkg/validator/v1) — a wrong kind/apiVersion, unparseable YAML, or a
	// missing "node" replicatedJob fails closed with ErrCodeInvalidRequest,
	// turning what would otherwise be a late ErrCodeInternal deep inside
	// applyNCCLWorkerScheduling into an early, recipe-actionable error.
	if err := v1.ValidateBenchmarkRuntime(carrier); err != nil {
		return "", err
	}
	return carrier, nil
}

// customRuntimeNodeSelector extracts the "node" replicatedJob's worker
// nodeSelector from a recipe-supplied runtime (raw, pre-substitution). Returns
// nil when the runtime pins no worker nodeSelector. The caller sizes the worker
// cohort against the same nodes the runtime will place workers on, so
// WorkerCount and placement stay consistent. The content was already
// shape-validated by v1.ValidateBenchmarkRuntime, so a parse failure here is
// unexpected but still surfaced fail-closed.
func customRuntimeNodeSelector(content string) (map[string]string, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(content), obj); err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s: not parseable as YAML", perfConstraintNCCLBenchmarkRuntime), err)
	}
	jobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		return nil, nil //nolint:nilnil // no replicatedJobs → nothing to size against
	}
	for _, raw := range jobs {
		jobMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(jobMap, "name"); name != nodeJobName {
			continue
		}
		sel, found, err := unstructured.NestedStringMap(jobMap, "template", "spec", "template", "spec", "nodeSelector")
		if err != nil {
			// A present-but-malformed nodeSelector (non-string values) must fail
			// closed, not be treated as "no selector" — otherwise sizing would
			// fall back to the whole cohort, the exact mismatch this guards.
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid %s: %q replicatedJob has a malformed nodeSelector", perfConstraintNCCLBenchmarkRuntime, nodeJobName), err)
		}
		if !found {
			return nil, nil //nolint:nilnil // node job pins no nodeSelector
		}
		return sel, nil
	}
	return nil, nil //nolint:nilnil // no "node" job → nothing to size against
}

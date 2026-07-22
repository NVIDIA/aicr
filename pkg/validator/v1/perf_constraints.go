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

package v1

import (
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Performance-phase constraint names and helpers shared between the validator
// orchestrator and the in-pod performance validator. Defining them here — the
// one package both ends import — keeps the write side (the orchestrator, which
// resolves NCCLBenchmarkRuntimeRef against the recipe DataProvider and lowers it
// into NCCLBenchmarkRuntime) and the read side (the pod, which renders and runs
// NCCLBenchmarkRuntime) in lockstep, rather than duplicating literals and lookup
// semantics on both ends. See NVIDIA/aicr#1792.
const (
	// PerfConstraintNCCLBenchmarkRuntime carries a complete Kubeflow
	// TrainingRuntime (inline YAML) that the performance validator renders in
	// place of a baked-in testdata template, keyed on the recipe's own criteria
	// with no compiled applicability entry required. It is the resolved carrier:
	// the orchestrator normally populates it from PerfConstraintNCCLBenchmarkRuntimeRef.
	PerfConstraintNCCLBenchmarkRuntime = "nccl-benchmark-runtime"

	// PerfConstraintNCCLBenchmarkRuntimeRef is the author-facing surface: a bare
	// "{accelerator}/{service}" value naming a runtime template the recipe ships
	// in its --data tree at
	// validators/performance/testdata/{accelerator}/{service}/runtime.yaml — the
	// same layout the embedded templates use, so an external recipe's runtime can
	// be upstreamed by copying the file into the repo unchanged. The orchestrator
	// reads that file through the recipe DataProvider and lowers its content into
	// PerfConstraintNCCLBenchmarkRuntime before the validator pod runs.
	PerfConstraintNCCLBenchmarkRuntimeRef = "nccl-benchmark-runtime-ref"

	// PerfConstraintNCCLBenchmarkProfile opts a recipe into an embedded NCCL
	// benchmark template by naming a compiled "{accelerator}/{service}" pair. It
	// is mutually exclusive with the runtime/ref pair (borrow an embedded
	// template OR supply your own, never both); the name lives here so the
	// orchestrator can reject that conflict at resolve time.
	PerfConstraintNCCLBenchmarkProfile = "nccl-benchmark-profile"

	// BenchmarkRuntimeAPIVersion and KindTrainingRuntime are the Kubeflow Trainer
	// identity a recipe-supplied runtime must declare. The validator only ever
	// applies it through a trainingruntimes GVR at this apiVersion with a
	// force-set name/namespace, so a recipe can supply a TrainingRuntime — and
	// nothing else.
	BenchmarkRuntimeAPIVersion = "trainer.kubeflow.org/v1alpha1"
	KindTrainingRuntime        = "TrainingRuntime"

	// BenchmarkRuntimeNodeJob is the replicatedJob a recipe-supplied runtime must
	// declare: the worker cohort the shared TrainJob sizes and the validator
	// injects scheduling into / reads launcher logs from.
	BenchmarkRuntimeNodeJob = "node"
)

// FindConstraint returns the first constraint with the given name.
func FindConstraint(cs []recipe.Constraint, name string) (recipe.Constraint, bool) {
	for _, c := range cs {
		if c.Name == name {
			return c, true
		}
	}
	return recipe.Constraint{}, false
}

// CountConstraint counts constraints with the given name.
func CountConstraint(cs []recipe.Constraint, name string) int {
	n := 0
	for _, c := range cs {
		if c.Name == name {
			n++
		}
	}
	return n
}

// ValidateBenchmarkRuntime confirms a recipe-supplied runtime template is a
// Kubeflow TrainingRuntime of the expected apiVersion that declares the "node"
// replicatedJob (with its worker pod spec at template.spec.template.spec — the
// path scheduling is injected into). It is the single shape contract shared by
// the orchestrator (which runs it at resolve time so a malformed runtime fails
// fast offline, before any Job) and the pod (which renders and applies it).
//
// The identity fields (apiVersion, kind) and the replicatedJob names never carry
// ${VAR} placeholders, so the raw content parses for this shape check even
// before substitution. A wrong kind/apiVersion, content that does not parse as
// YAML, or a missing/ill-formed "node" replicatedJob is rejected fail-closed
// (ErrCodeInvalidRequest) so a typo cannot masquerade as a passing — or silently
// skipped — benchmark.
func ValidateBenchmarkRuntime(content string) error {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(content), obj); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s: not parseable as YAML", PerfConstraintNCCLBenchmarkRuntime), err)
	}
	if obj.GetAPIVersion() != BenchmarkRuntimeAPIVersion || obj.GetKind() != KindTrainingRuntime {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s: must be a %s %s (got apiVersion=%q kind=%q)",
				PerfConstraintNCCLBenchmarkRuntime, BenchmarkRuntimeAPIVersion, KindTrainingRuntime,
				obj.GetAPIVersion(), obj.GetKind()))
	}
	return validateBenchmarkNodeJob(obj)
}

// validateBenchmarkNodeJob confirms the runtime declares the "node" replicatedJob
// and that it carries the worker pod spec at template.spec.template.spec.
func validateBenchmarkNodeJob(obj *unstructured.Unstructured) error {
	jobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err == nil && found {
		for _, raw := range jobs {
			jobMap, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _, _ := unstructured.NestedString(jobMap, "name"); name != BenchmarkRuntimeNodeJob {
				continue
			}
			if _, ok, err := unstructured.NestedMap(jobMap, "template", "spec", "template", "spec"); err != nil || !ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("invalid %s: %q replicatedJob must define the worker pod spec at template.spec.template.spec",
						PerfConstraintNCCLBenchmarkRuntime, BenchmarkRuntimeNodeJob))
			}
			return nil
		}
	}
	return errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("invalid %s: must declare a %q replicatedJob (spec.template.spec.replicatedJobs) — the NCCL worker cohort",
			PerfConstraintNCCLBenchmarkRuntime, BenchmarkRuntimeNodeJob))
}

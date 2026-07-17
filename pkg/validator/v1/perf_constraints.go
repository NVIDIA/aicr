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

// Performance-phase constraint names shared between the validator orchestrator
// and the in-pod performance validator. Defining them here — the one package
// both ends import — keeps the write side (the orchestrator, which resolves
// NCCLBenchmarkRuntimeRef against the recipe DataProvider and lowers it into
// NCCLBenchmarkRuntime) and the read side (the pod, which renders and runs
// NCCLBenchmarkRuntime) in lockstep, rather than duplicating the literal on both
// ends with a lock test. See NVIDIA/aicr#1792.
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
)

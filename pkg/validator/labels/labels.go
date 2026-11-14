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

// Package labels provides shared label constants for validation resources.
package labels

import (
	"github.com/NVIDIA/aicr/pkg/header"
	k8slabels "github.com/NVIDIA/aicr/pkg/k8s/labels"
)

// Standard Kubernetes label keys. These are aliases of pkg/k8s/labels, which
// is the single source of truth shared with the snapshot agent
// (pkg/k8s/agent) — validator code should keep referring to them as
// labels.Name etc. rather than importing pkg/k8s/labels directly.
const (
	Name      = k8slabels.Name
	Component = k8slabels.Component
	ManagedBy = k8slabels.ManagedBy
	RunID     = k8slabels.RunID
)

// AICR-specific label keys. All are keyed on the canonical AICR API domain
// but GangSchedulingPhase, which keeps its frozen pre-ADR-013 domain.
const (
	JobType    = header.Domain + "/job-type"
	Validator  = header.Domain + "/validator"
	Phase      = header.Domain + "/phase"
	ReportType = header.Domain + "/report-type"

	// GPUTestRun is the ownership label stamped on every per-run test
	// namespace (and on the gang-scheduling pods/PodGroups a run places in a
	// Run:ai Project namespace it does not own); its value is the run's
	// random token. Cleanup uses it to verify that an object found at the
	// expected name really belongs to the run before deleting anything, and
	// `aicr sweep` uses it to find leaked test resources — the residual
	// paths no in-process budget can cover (SIGKILL, or scheduling/image-pull
	// skew beyond the margin the work budgets reserve).
	GPUTestRun = header.Domain + "/gpu-test-run"

	// GangSchedulingPhase is stamped by the gang-scheduling conformance
	// check on every test pod and PodGroup, marking which phase they belong
	// to. Every run shares it, so together with GPUTestRun it selects the
	// residue a run leaves in a Run:ai Project namespace. The domain
	// predates the ADR-013 migration to header.Domain; the key is frozen
	// because Run:ai-side tooling and documentation reference it.
	GangSchedulingPhase = "gang-scheduling.aicr.nvidia.com/phase"
)

// Third-party label keys the validator reads to recognize the environment a
// run deployed into. They are not AICR's to stamp; the constants exist so the
// conformance checks and the sweeper agree on them.
const (
	// RunAIQueue is the Run:ai Project namespace label whose value is the
	// Project's queue (the queue label of Run:ai's KAI). A namespace carrying
	// it is a user-owned Project, which `aicr sweep` must never treat as a
	// validator test namespace.
	RunAIQueue = "runai/queue"
)

// Common label values.
const (
	ValueAICR       = k8slabels.ValueAICR
	ValueValidation = "validation"
	ValueValidator  = "aicr-validator"

	// ValueSnapshotAgent and ValueAgentRBAC are aliases of pkg/k8s/labels,
	// like the keys above: ValueSnapshotAgent selects snapshot-agent run
	// objects, and ValueAgentRBAC marks the operator-owned RBAC that must be
	// excluded from any run-scoped selection.
	ValueSnapshotAgent = k8slabels.ValueSnapshotAgent
	ValueAgentRBAC     = k8slabels.ValueAgentRBAC
)

// Component label values for per-run benchmark namespaces, stamped
// alongside ManagedBy so a stale-namespace prune can scope its List to
// exactly one benchmark server-side, instead of matching a hand-maintained
// name prefix.
const (
	ValueNCCLPerf      = "nccl-perf"
	ValueInferencePerf = "inference-perf"
)

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

// Package labels provides shared Kubernetes label constants used by both
// the validator (pkg/validator) and the snapshot agent (pkg/k8s/agent), so
// neither has to import the other to agree on label keys and values.
package labels

import "github.com/NVIDIA/aicr/pkg/header"

// Standard Kubernetes label keys.
const (
	Name      = "app.kubernetes.io/name"
	Component = "app.kubernetes.io/component"
	ManagedBy = "app.kubernetes.io/managed-by"
)

// RunID scopes every resource to the run that created it.
const RunID = header.Domain + "/run-id"

// InvocationID identifies the single in-process invocation — one
// snapshot-agent Deployer — that created an object, as distinct from the run
// it belongs to.
//
// It exists because RunID cannot answer "did I create this?". RunID is public,
// caller-settable SDK surface, and sharing one across invocations is a
// designed scenario: `aicr validate` hands the same ID to its live-capture
// agent and to its validator Jobs, and e2e/chainsaw runs pin a value on
// purpose. Two invocations sharing a RunID therefore stamp byte-identical
// Name/ManagedBy/Component/RunID labels, so that set proves membership in a
// run but never authorship by one invocation.
//
// The value is generated inside the Deployer with runid.Generate() and is
// reachable through no configuration field, so a second invocation cannot
// reproduce it even when it reuses the RunID verbatim. Cleanup requires it
// before adopting an object whose creation it never had confirmed.
const InvocationID = header.Domain + "/invocation-id"

// Common label values.
const (
	// ValueAICR is the shared app name.
	ValueAICR = "aicr"

	// ValueSnapshotAgent identifies snapshot-agent-owned resources.
	ValueSnapshotAgent = "snapshot-agent"

	// ValueAgentRBAC identifies the NON-run-scoped Role, RoleBinding,
	// ClusterRole and ClusterRoleBinding that
	// `aicr snapshot --add-roles-to-service-account` renders as manifests
	// for an operator-supplied ServiceAccount. aicr applies none of them;
	// the operator does. Objects carrying this value are deliberately
	// outside every run's lifecycle: they carry no RunID label, never
	// enter a run's created-set, and are never deleted by run cleanup.
	// Teardown is the operator's `kubectl delete -f`.
	ValueAgentRBAC = "agent-rbac"
)

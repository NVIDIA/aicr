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

// Package sweep finds and deletes the per-run validation resources an
// interrupted `aicr validate` leaves behind: conformance test namespaces
// stamped with the labels.GPUTestRun ownership label, the gang-scheduling
// pods and PodGroups a run places in a Run:ai Project namespace it does not
// own, and the run-scoped RBAC and ConfigMaps of the validation namespace —
// including the cluster-admin ClusterRoleBinding, whose orphaning is the
// highest-impact leak a SIGKILLed run produces.
//
// A run cleans all of this up after itself on the success and SIGTERM paths;
// the sweeper exists for the paths that cannot run deferred cleanup (SIGKILL,
// OOM, node loss) and for `--no-cleanup` runs once their retained validator
// Jobs have expired (the Job TTL controller reaps them after
// defaults.JobTTLAfterFinished) and the remaining objects are older than the
// floor — a retained Job keeps its run guarded while it exists, and the
// sweeper never deletes the Jobs themselves.
//
// Safety is layered, and every layer fails closed:
//
//   - Selection is by label AND shape, never by label alone: a labeled
//     namespace is a candidate only when its name is a known test-namespace
//     prefix followed by the hex token the label carries, a
//     ClusterRoleBinding only when its name, roleRef and single ServiceAccount
//     subject match what pkg/validator/job writes for the run, a data or CTRF
//     ConfigMap only when its name is exactly one the validator writes for
//     the label's run ID, and a snapshot-agent object only when its name
//     carries the label's run ID the way pkg/k8s/agent writes it (exactly
//     defaults.AgentClusterRoleName + "-" + runID for the cluster-scoped
//     pair).
//   - An object must be older than Options.OlderThan (default
//     defaults.ValidationSweepMinAge, which outlasts any single check's
//     wait envelope — the bound on a test namespace's life) AND fail a
//     liveness guard: a test namespace is kept while any non-terminal pod
//     runs in it, and run-scoped RBAC/ConfigMaps are kept while a
//     validator Job carrying their run's aicr.run/run-id label exists in
//     any namespace (a run in another namespace still owns cluster-scoped
//     agent RBAC). The floor does not bound a whole uncapped `aicr
//     validate`, and a live run with cleanup on shows no Job between its
//     checks, so Execute additionally re-plans against current cluster
//     state, then lists the run-labeled Jobs and watches from the list's
//     ResourceVersion for defaults.ValidationSweepSettleWindow for a Job
//     to appear, and re-plans once more before deleting anything
//     run-scoped. A watch the apiserver ends before the window elapses is
//     an anomaly (apiserver watch timeouts are far longer): the apply
//     aborts with nothing deleted rather than treating quiet as observed.
//     The window is a conservative margin over the usual between-checks
//     gap, not a proven bound: a run stalled between checks for longer
//     cannot be told from a dead one.
//   - Every delete is pinned with metav1.Preconditions{UID}, so an object
//     recreated between planning and deletion is left alone. Execute
//     deletes only the intersection of the caller's plan and a fresh
//     Plan, so a tampered or stale report cannot widen the deletion set.
//
// The `aicr-validation` namespace itself, anything unlabeled, and the
// benchmark namespaces (aicr-nccl-perf-*, aicr-inference-perf-*) are never
// selected. A namespace carrying labels.RunAIQueue is a user-owned Run:ai
// Project and is never a candidate, and neither is anything in it deleted at
// namespace scope — only the labeled pods and PodGroups inside it.
package sweep

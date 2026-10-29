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

package v1alpha1

// SingletonName is the only allowed ClusterRuntime name.
const SingletonName = "cluster"

// Label and annotation keys (docs/operator/api.md#engine-object-metadata). Labels
// select objects for watches; annotations plus the recorded runtime UID
// authorize mutation.
const (
	// LabelManagedBy marks every object the operator owns.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the LabelManagedBy value for operator objects.
	ManagedByValue = "aicr-operator"

	// LabelRuntime names the runtime an object belongs to.
	LabelRuntime = "operator.aicr.run/runtime"
	// LabelRunID carries an AICR Job's run ID (Jobs and per-run Secrets).
	LabelRunID = "operator.aicr.run/run-id"
	// LabelRole classifies operator ConfigMaps: plan, recipe, values,
	// snapshot or assessment.
	LabelRole = "operator.aicr.run/role"
	// LabelSchemaVersion carries the plan document schema version.
	LabelSchemaVersion = "operator.aicr.run/schema-version"
	// LabelToPlan carries the first 16 hex characters of a rollout's target.
	LabelToPlan = "operator.aicr.run/to-plan"
	// LabelTransition carries a rollout's transition hash.
	LabelTransition = "operator.aicr.run/transition"
	// LabelOutcome records a rollout's terminal outcome, set once.
	LabelOutcome = "operator.aicr.run/outcome"
	// LabelStage names the AICR stage an operator Job runs.
	LabelStage = "operator.aicr.run/stage"
	// LabelInput carries the first 16 hex characters of the resolution
	// inputDigest an AICR Job or resolver ConfigMap was produced for, so a
	// restarted resolver finds its earlier work without status.
	LabelInput = "operator.aicr.run/input"

	// AnnotationRuntimeUID records the owning runtime's UID.
	AnnotationRuntimeUID = "operator.aicr.run/runtime-uid"
	// AnnotationPlanDigest records the desiredStateDigest that last wrote an object.
	AnnotationPlanDigest = "operator.aicr.run/plan-digest"
	// AnnotationDigest records the content digest of a recipe or values ConfigMap.
	AnnotationDigest = "operator.aicr.run/digest"
	// AnnotationAttempt numbers an AICR Job among the runs of its stage and
	// input; creation timestamps have one-second resolution and cannot order
	// runs.
	AnnotationAttempt = "operator.aicr.run/attempt"
	// AnnotationWaiting records on an AICR Job why its pod was last not
	// running, so a run that hits its deadline keeps the reason.
	AnnotationWaiting = "operator.aicr.run/waiting"
	// AnnotationRecorded marks an AICR Job whose terminal result was
	// counted and, when it failed, reported, so each run is recorded once.
	AnnotationRecorded = "operator.aicr.run/recorded"
	// AnnotationTokenExpiresAt records when the registry token in the engine's
	// repo-creds Secret expires (aws provider).
	AnnotationTokenExpiresAt = "operator.aicr.run/token-expires-at" //nolint:gosec // an annotation key, not a credential
	// AnnotationReleaseID records the plan release ID of an engine object.
	AnnotationReleaseID = "operator.aicr.run/release-id"
	// AnnotationOrphanedFrom records the runtime UID an object was orphaned from.
	AnnotationOrphanedFrom = "operator.aicr.run/orphaned-from"
	// AnnotationEvidence records a succeeded rollout's evidence reference, set once.
	AnnotationEvidence = "operator.aicr.run/evidence"
	// AnnotationPhase records an engine object's plan phase.
	AnnotationPhase = "operator.aicr.run/phase"
	// AnnotationDependsOn records an engine object's sorted, comma-separated dependencies.
	AnnotationDependsOn = "operator.aicr.run/depends-on"
	// AnnotationReadinessFor records a readiness release's parent release ID.
	AnnotationReadinessFor = "operator.aicr.run/readiness-for"
	// AnnotationOwnsCRDs records whether a release owns CRDs.
	AnnotationOwnsCRDs = "operator.aicr.run/owns-crds"
	// AnnotationReleaseDigest records the digest of the release identity an
	// engine object was written from (plan.ReleaseDigest), so a release
	// edited out of band is named on rebuild.
	AnnotationReleaseDigest = "operator.aicr.run/release-digest"
	// AnnotationAdoptedFrom records, on an Adopt rollout, the earlier
	// runtime UID whose engine objects it adopts.
	AnnotationAdoptedFrom = "operator.aicr.run/adopted-from"
	// AnnotationDeleteRollout names the rollout whose approved Remove step is
	// deleting an Application; present only with the resources finalizer.
	AnnotationDeleteRollout = "operator.aicr.run/delete-rollout"

	// AnnotationRetry requests a retry of a failed, expired, revoked or
	// stalled rollout; each retry needs a new token.
	AnnotationRetry = "operator.aicr.run/retry"
	// AnnotationRetryRequested records on a stalled rollout the retry token
	// that asked to supersede it, consuming the token.
	AnnotationRetryRequested = "operator.aicr.run/retry-requested"
	// AnnotationResnapshot requests an AICR snapshot outside the schedule.
	AnnotationResnapshot = "operator.aicr.run/resnapshot"
)

// FinalizerCleanup governs runtime deletion: Orphan or Delete runs before
// it is removed. It is the operator's only finalizer.
const FinalizerCleanup = "operator.aicr.run/cleanup"

// Role values for LabelRole.
const (
	RolePlan   = "plan"
	RoleRecipe = "recipe"
	RoleValues = "values"
	// RoleSnapshot is a discovery snapshot, labeled with the snapshot input
	// it answers (LabelInput).
	RoleSnapshot = "snapshot"
	// RoleAssessment is an AICR upgrade-check report between two recipes,
	// labeled with the transition it assessed (LabelInput).
	RoleAssessment = "assessment"
)

// ClusterRuntime condition types (docs/operator/api.md). They follow kstatus,
// so other tools can health-check the runtime. Ready concerns the candidate
// for the current generation; Available concerns the active plan, so a
// rollout in progress can be Ready=False while the stack stays Available.
const (
	// ConditionReady: the candidate is fully rolled out, current-revision
	// ready and validated when required. Owner: rollout controller.
	ConditionReady = "Ready"
	// ConditionAvailable: the active plan's releases are healthy. Owner:
	// rollout controller.
	ConditionAvailable = "Available"
	// ConditionReconciling: work is in progress for the current generation,
	// including resolution. Owner: rollout controller.
	ConditionReconciling = "Reconciling"
	// ConditionStalled: progress needs intervention, including a resolver
	// stall reported through Resolved. Owner: rollout controller.
	ConditionStalled = "Stalled"
	// ConditionDegraded is reserved: this version does not set it, and reports
	// unhealthy releases through Available.
	ConditionDegraded = "Degraded"
	// ConditionResolved: a plan was produced for the current generation and
	// snapshot. Owner: resolver, its only condition.
	ConditionResolved = "Resolved"
	// ConditionEngineAvailable: engine CRDs, project and customizations are
	// present. Owner: rollout controller.
	ConditionEngineAvailable = "EngineAvailable"
)

// ResolverConditionTypes are the condition types the resolver applies; the
// rollout controller applies every other type. Disjoint ownership keeps
// server-side apply from transferring entries between field managers.
var ResolverConditionTypes = []string{ConditionResolved}

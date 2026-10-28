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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutSchemaVersion is the RuntimeRollout spec schema this operator writes.
const RolloutSchemaVersion = 1

// SupportedRolloutSchemaVersions are the rollout schemas this operator reads
// (N and N-1); a rollout with any other stops it.
var SupportedRolloutSchemaVersions = map[int32]bool{RolloutSchemaVersion: true}

// RuntimeRollout is one transition between plans: its target, assessment,
// steps, deadline and approval. Its spec is immutable except
// the set-once approval; its outcome label is set once by the operator.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=aicrro
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Approval",type=string,JSONPath=`.status.approval`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.metadata.labels.operator\.aicr\.run/to-plan`
// +kubebuilder:printcolumn:name="Deadline",type=date,JSONPath=`.spec.deadline`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RuntimeRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RuntimeRolloutSpec   `json:"spec"`
	Status RuntimeRolloutStatus `json:"status,omitempty"`
}

// RuntimeRolloutSpec is immutable except Approval, which is set once and may
// then gain a revocation, also set once.
//
// +kubebuilder:validation:XValidation:rule="self.schemaVersion == oldSelf.schemaVersion && self.runtimeUID == oldSelf.runtimeUID && self.toPlanDigest == oldSelf.toPlanDigest && self.startingInventoryDigest == oldSelf.startingInventoryDigest && self.deadline == oldSelf.deadline && self.actionTimeout == oldSelf.actionTimeout && self.approvalRequired == oldSelf.approvalRequired && self.steps == oldSelf.steps",message="rollout spec is immutable except spec.approval"
// +kubebuilder:validation:XValidation:rule="has(self.fromPlanDigest) == has(oldSelf.fromPlanDigest) && (!has(self.fromPlanDigest) || self.fromPlanDigest == oldSelf.fromPlanDigest) && has(self.assessmentRef) == has(oldSelf.assessmentRef) && (!has(self.assessmentRef) || self.assessmentRef == oldSelf.assessmentRef)",message="rollout spec is immutable except spec.approval"
// +kubebuilder:validation:XValidation:rule="has(self.startingInventory) == has(oldSelf.startingInventory) && (!has(self.startingInventory) || self.startingInventory == oldSelf.startingInventory)",message="rollout spec is immutable except spec.approval"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approval) || (has(self.approval) && self.approval.approvedBy == oldSelf.approval.approvedBy && self.approval.expiresAt == oldSelf.approval.expiresAt && self.approval.reason == oldSelf.approval.reason)",message="an approval can be neither cleared nor changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approval) || !has(oldSelf.approval.revokedBy) || (has(self.approval.revokedBy) && self.approval.revokedBy == oldSelf.approval.revokedBy && self.approval.revokedAt == oldSelf.approval.revokedAt && self.approval.revokeReason == oldSelf.approval.revokeReason)",message="a revocation can be neither cleared nor changed"
// +kubebuilder:validation:XValidation:rule="!has(self.approval) || !has(self.approval.revokedBy) || has(oldSelf.approval)",message="a revocation needs an existing approval"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.approval) || !has(oldSelf.approval.acknowledgements) || (has(self.approval.acknowledgements) && oldSelf.approval.acknowledgements.all(o, self.approval.acknowledgements.exists(n, n == o)))",message="acknowledgements are append-only"
// +kubebuilder:validation:XValidation:rule="!has(self.approval) || !has(self.approval.acknowledgements) || has(oldSelf.approval)",message="an acknowledgement needs an existing approval"
type RuntimeRolloutSpec struct {
	// SchemaVersion is the spec schema; an unknown version stalls.
	// +kubebuilder:validation:Minimum=1
	SchemaVersion int32 `json:"schemaVersion"`

	// RuntimeUID is the UID of the ClusterRuntime the rollout belongs to.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	RuntimeUID string `json:"runtimeUID"`

	// FromPlanDigest is the active plan at creation; empty for an initial install.
	// +optional
	// +kubebuilder:validation:Pattern=`^(sha256:[a-f0-9]{64})?$`
	FromPlanDigest string `json:"fromPlanDigest,omitempty"`

	// ToPlanDigest is the target plan.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ToPlanDigest string `json:"toPlanDigest"`

	// StartingInventoryDigest digests the owned objects at creation.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	StartingInventoryDigest string `json:"startingInventoryDigest"`

	// StartingInventory lists the owned engine objects at creation, which
	// StartingInventoryDigest digests. Before each step the controller
	// compares the live inventory with it and the completed steps.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=256
	StartingInventory []InventoryEntry `json:"startingInventory,omitempty"`

	// AssessmentRef points at the AICR upgrade-check report.
	// +optional
	AssessmentRef *DigestedConfigMapRef `json:"assessmentRef,omitempty"`

	// Deadline bounds the rollout; computed once at creation.
	Deadline metav1.Time `json:"deadline"`

	// ActionTimeout bounds each step.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	ActionTimeout metav1.Duration `json:"actionTimeout"`

	// ApprovalRequired is true when any step requires approval.
	ApprovalRequired bool `json:"approvalRequired"`

	// Steps run in order; 128 target releases plus removals from the active plan.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=256
	Steps []RolloutStep `json:"steps"`

	// Approval is written by an approver, never by the operator.
	// +optional
	Approval *Approval `json:"approval,omitempty"`
}

// InventoryEntry is one owned engine object.
type InventoryEntry struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=53
	ReleaseID string `json:"releaseID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=36
	UID string `json:"uid"`
	// PlanDigest is the object's plan-digest annotation.
	// +optional
	// +kubebuilder:validation:MaxLength=71
	PlanDigest string `json:"planDigest,omitempty"`
	// Revision is the engine's observed revision.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Revision string `json:"revision,omitempty"`
}

// DigestedConfigMapRef names a ConfigMap and the digest of its content.
type DigestedConfigMapRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`
}

// StepOperation is what a step does to one release.
// +kubebuilder:validation:Enum=Install;Upgrade;Verify;Remove;Adopt
type StepOperation string

// Step operations.
const (
	OperationInstall StepOperation = "Install"
	OperationUpgrade StepOperation = "Upgrade"
	OperationVerify  StepOperation = "Verify"
	OperationRemove  StepOperation = "Remove"
	OperationAdopt   StepOperation = "Adopt"
)

// Assessment is AICR's upgrade-check verdict for a step.
// +kubebuilder:validation:Enum=Safe;Manual;Blocked;Unknown;Unversioned;NotApplicable
type Assessment string

// Assessments.
const (
	AssessmentSafe          Assessment = "Safe"
	AssessmentManual        Assessment = "Manual"
	AssessmentBlocked       Assessment = "Blocked"
	AssessmentUnknown       Assessment = "Unknown"
	AssessmentUnversioned   Assessment = "Unversioned"
	AssessmentNotApplicable Assessment = "NotApplicable"
)

// RolloutStep is one engine mutation for one release.
// +kubebuilder:validation:XValidation:rule="(self.operation == 'Remove') == has(self.uid)",message="uid is set exactly on Remove steps"
type RolloutStep struct {
	// Release is the plan release ID (the Helm release-name limit).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=53
	Release string `json:"release"`

	Operation StepOperation `json:"operation"`

	Assessment Assessment `json:"assessment"`

	// CRDChange is true when the release owns CRDs whose chart changes.
	// +optional
	CRDChange bool `json:"crdChange,omitempty"`

	// UID is the engine object a Remove step deletes, fixed when the rollout
	// is created: an object recreated under the same name is never deleted.
	// +optional
	// +kubebuilder:validation:MaxLength=36
	UID string `json:"uid,omitempty"`

	// PostApplySteps are the IDs, in order, of a manual transition record's
	// steps after the apply. Once the release is ready the rollout waits for
	// the approver to acknowledge them before the plan becomes active.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=63
	PostApplySteps []string `json:"postApplySteps,omitempty"`
}

// Approval authorizes a rollout; it is set once and may gain a revocation.
// +kubebuilder:validation:XValidation:rule="has(self.revokedBy) == has(self.revokedAt) && has(self.revokedBy) == has(self.revokeReason)",message="revokedBy, revokedAt and revokeReason are set together"
type Approval struct {
	// ApprovedBy must equal the requester's username (admission policy).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ApprovedBy string `json:"approvedBy"`

	// ExpiresAt ends the approval; expiry ends the rollout as Expired.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason"`

	// RevokedBy must equal the requester's username (admission policy).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	RevokedBy string `json:"revokedBy,omitempty"`

	// RevokedAt is informational, written by the client.
	// +optional
	RevokedAt *metav1.Time `json:"revokedAt,omitempty"`

	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	RevokeReason string `json:"revokeReason,omitempty"`

	// Acknowledgements record, per release, that the approver completed its
	// post-apply steps. Append-only; each names its requester
	// (admission policy).
	// +optional
	// +listType=map
	// +listMapKey=release
	// +kubebuilder:validation:MaxItems=64
	Acknowledgements []Acknowledgement `json:"acknowledgements,omitempty"`
}

// Acknowledgement is an approver's statement that a release's post-apply
// steps are done.
type Acknowledgement struct {
	// +kubebuilder:validation:MaxLength=53
	Release string `json:"release"`
	// AcknowledgedBy must equal the requester's username (admission policy).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	AcknowledgedBy string `json:"acknowledgedBy"`
	// AcknowledgedAt is informational, written by the client.
	// +optional
	AcknowledgedAt *metav1.Time `json:"acknowledgedAt,omitempty"`
}

// RolloutPhase is the rollout state machine phase.
// +kubebuilder:validation:Enum=Pending;AwaitingApproval;Progressing;AwaitingAcknowledgement;Validating;Stalled;Succeeded;Failed;Expired;Revoked;Superseded;Orphaned
type RolloutPhase string

// Rollout phases.
const (
	PhasePending          RolloutPhase = "Pending"
	PhaseAwaitingApproval RolloutPhase = "AwaitingApproval"
	PhaseProgressing      RolloutPhase = "Progressing"
	// PhaseAwaitingAcknowledgement waits for the approver to acknowledge a
	// ready release's post-apply steps.
	PhaseAwaitingAcknowledgement RolloutPhase = "AwaitingAcknowledgement"
	PhaseValidating              RolloutPhase = "Validating"
	PhaseStalled                 RolloutPhase = "Stalled"
	PhaseSucceeded               RolloutPhase = "Succeeded"
	PhaseFailed                  RolloutPhase = "Failed"
	PhaseExpired                 RolloutPhase = "Expired"
	PhaseRevoked                 RolloutPhase = "Revoked"
	PhaseSuperseded              RolloutPhase = "Superseded"
	PhaseOrphaned                RolloutPhase = "Orphaned"
)

// IsTerminal reports whether a rollout in this phase never resumes.
func (p RolloutPhase) IsTerminal() bool {
	switch p {
	case PhaseSucceeded, PhaseFailed, PhaseExpired, PhaseRevoked, PhaseSuperseded, PhaseOrphaned:
		return true
	case PhasePending, PhaseAwaitingApproval, PhaseProgressing, PhaseAwaitingAcknowledgement, PhaseValidating, PhaseStalled:
		return false
	default:
		return false
	}
}

// ApprovalState summarizes a rollout's approval in status.
// +kubebuilder:validation:Enum=NotRequired;Pending;Approved;Bypassed;Revoked;Expired
type ApprovalState string

// Approval states.
const (
	ApprovalNotRequired ApprovalState = "NotRequired"
	ApprovalPending     ApprovalState = "Pending"
	ApprovalApproved    ApprovalState = "Approved"
	ApprovalBypassed    ApprovalState = "Bypassed"
	ApprovalRevoked     ApprovalState = "Revoked"
	ApprovalExpired     ApprovalState = "Expired"
)

// RuntimeRolloutStatus holds observations rebuilt from the spec, the outcome
// label and live objects.
type RuntimeRolloutStatus struct {
	// +optional
	Phase RolloutPhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Approval ApprovalState `json:"approval,omitempty"`

	// Current is the step in progress.
	// +optional
	Current *CurrentStep `json:"current,omitempty"`

	// Releases are per-release observations.
	// +optional
	// +listType=map
	// +listMapKey=id
	// +kubebuilder:validation:MaxItems=256
	Releases []ReleaseStatus `json:"releases,omitempty"`

	// Validation is the deployment validation run.
	// +optional
	Validation *ValidationStatus `json:"validation,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// CurrentStep is the step in progress.
type CurrentStep struct {
	// +kubebuilder:validation:MaxLength=53
	Release   string        `json:"release"`
	Operation StepOperation `json:"operation"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// TimeoutFrom, when set, is when the action timeout last started again:
	// the end of a halt. Time halted does not count against the step.
	// +optional
	TimeoutFrom *metav1.Time `json:"timeoutFrom,omitempty"`
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
}

// ReleaseState is a release's observed state.
// +kubebuilder:validation:Enum=Pending;Progressing;Ready;Failed;Suspended;Deleting;Removed;Unknown
type ReleaseState string

// Release states (the engine's Observe results plus Pending).
const (
	ReleasePending     ReleaseState = "Pending"
	ReleaseProgressing ReleaseState = "Progressing"
	ReleaseReady       ReleaseState = "Ready"
	ReleaseFailed      ReleaseState = "Failed"
	ReleaseSuspended   ReleaseState = "Suspended"
	ReleaseDeleting    ReleaseState = "Deleting"
	ReleaseRemoved     ReleaseState = "Removed"
	ReleaseUnknown     ReleaseState = "Unknown"
)

// ReleaseStatus is one release's observation.
type ReleaseStatus struct {
	// +kubebuilder:validation:MaxLength=53
	ID    string       `json:"id"`
	State ReleaseState `json:"state"`
	// +optional
	// +kubebuilder:validation:MaxLength=71
	Revision string `json:"revision,omitempty"`
}

// ValidationStatus is the deployment validation run.
type ValidationStatus struct {
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Job string `json:"job,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	RunID string `json:"runID,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Pending;Passed;Failed
	Result string `json:"result,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Evidence string `json:"evidence,omitempty"`
}

// RuntimeRolloutList is a list of RuntimeRollout.
//
// +kubebuilder:object:root=true
type RuntimeRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeRollout `json:"items"`
}

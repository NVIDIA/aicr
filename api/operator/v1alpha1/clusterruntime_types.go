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
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterRuntime is the administrator's desired GPU runtime for the cluster.
// It is a singleton named "cluster" and holds intent and policy only;
// infrastructure is installation configuration.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=aicrrt
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intent`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activePlanDigest`,priority=1
// +kubebuilder:printcolumn:name="Rollout",type=string,JSONPath=`.status.rollout`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="ClusterRuntime is a singleton named 'cluster'"
type ClusterRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterRuntimeSpec   `json:"spec"`
	Status ClusterRuntimeStatus `json:"status,omitempty"`
}

// ClusterRuntimeSpec is the desired runtime intent and policy.
type ClusterRuntimeSpec struct {
	// Intent is the AICR criteria intent, for example "training".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Intent string `json:"intent"`

	// Platform is the optional AICR criteria platform, for example "kubeflow".
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Platform string `json:"platform,omitempty"`

	// Profile is the optional AICR configuration profile.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Profile string `json:"profile,omitempty"`

	// Criteria are administrator-declared facts that must agree with discovery.
	// +optional
	Criteria *DeclaredCriteria `json:"criteria,omitempty"`

	// Placement maps to AICR's system and accelerated scheduling flags.
	Placement Placement `json:"placement"`

	// Configuration is cluster-specific desired state AICR records in the
	// recipe without a criteria axis, such as the GKE TCPXO interface mapping.
	// +optional
	Configuration *RecipeConfiguration `json:"configuration,omitempty"`

	// Selection chooses between the newest resolved plan and a pinned one.
	// +optional
	// +kubebuilder:default={policy: "Automatic"}
	Selection Selection `json:"selection,omitzero"`

	// Overrides are typed values passed to AICR's render. Each
	// (component, path) appears at most once.
	// +optional
	// +listType=map
	// +listMapKey=component
	// +listMapKey=path
	// +kubebuilder:validation:MaxItems=64
	Overrides []Override `json:"overrides,omitempty"`

	// Rollout bounds stabilization, each step and the whole rollout.
	// +optional
	// +kubebuilder:default={stabilizationWindow: "10m", actionTimeout: "30m", deadline: "6h"}
	Rollout RolloutPolicy `json:"rollout,omitzero"`

	// Validation selects whether AICR deployment validation gates promotion.
	// +optional
	// +kubebuilder:default={deployment: "Required"}
	Validation ValidationPolicy `json:"validation,omitzero"`

	// DeletionPolicy decides what deleting the runtime does to the stack.
	// +optional
	// +kubebuilder:default=Orphan
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Adoption names an earlier runtime whose engine objects to adopt.
	// +optional
	Adoption *Adoption `json:"adoption,omitempty"`

	// SuspendEngine stops automated engine syncs and drift repair; it never deletes.
	// +optional
	SuspendEngine bool `json:"suspendEngine,omitempty"`

	// Paused stops new rollout steps; observation continues.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// DeclaredCriteria are criteria the administrator asserts.
type DeclaredCriteria struct {
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Service string `json:"service,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Accelerator string `json:"accelerator,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	OS string `json:"os,omitempty"`
}

// Placement holds the two fixed node groups.
// +kubebuilder:validation:XValidation:rule="self.system.nodeSelector != self.accelerated.nodeSelector",message="system and accelerated node selectors must differ"
type Placement struct {
	System      NodeGroupPlacement `json:"system"`
	Accelerated NodeGroupPlacement `json:"accelerated"`
}

// LabelValue is a Kubernetes label value.
// +kubebuilder:validation:MaxLength=63
type LabelValue string

// NodeGroupPlacement selects and tolerates one node group.
type NodeGroupPlacement struct {
	// NodeSelector selects the group's nodes.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=16
	NodeSelector map[string]LabelValue `json:"nodeSelector"`

	// Tolerations let the group's workloads run on tainted nodes.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// RecipeConfiguration mirrors AICR's recipe configuration. AICR validates it
// during resolution; the CRD bounds only its shape.
type RecipeConfiguration struct {
	// GKE is configuration for GKE clusters.
	// +optional
	GKE *GKEConfiguration `json:"gke,omitempty"`
}

// GKEConfiguration is AICR's configuration.gke.
type GKEConfiguration struct {
	// TCPXOInterfaces binds each GPU NIC to the GKE Device Network
	// provisioning attached to it. AICR requires it for recipes that ship
	// the GPUDirect-TCPXO training runtime: on a3-megagpu-8g, eth1 to eth8.
	// +listType=map
	// +listMapKey=interfaceName
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	TCPXOInterfaces []NetworkInterfaceMapping `json:"tcpxoInterfaces"`
}

// NetworkInterfaceMapping binds a guest network interface to a GKE Network.
type NetworkInterfaceMapping struct {
	// InterfaceName is the guest interface, for example eth1.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	InterfaceName string `json:"interfaceName"`

	// Network is the GKE Device Network object name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Network string `json:"network"`
}

// SelectionPolicy chooses the target plan.
// +kubebuilder:validation:Enum=Automatic;Pinned
type SelectionPolicy string

// Selection policies.
const (
	SelectionAutomatic SelectionPolicy = "Automatic"
	SelectionPinned    SelectionPolicy = "Pinned"
)

// Selection chooses the target plan.
// +kubebuilder:validation:XValidation:rule="(self.policy == 'Pinned') == (has(self.pinnedPlanDigest) && size(self.pinnedPlanDigest) > 0)",message="pinnedPlanDigest is required when, and only when, policy is Pinned"
type Selection struct {
	// +optional
	// +kubebuilder:default=Automatic
	Policy SelectionPolicy `json:"policy,omitempty"`

	// PinnedPlanDigest is the desiredStateDigest of the plan to hold.
	// +optional
	// +kubebuilder:validation:Pattern=`^(sha256:[a-f0-9]{64})?$`
	PinnedPlanDigest string `json:"pinnedPlanDigest,omitempty"`
}

// Override is one typed value passed to AICR's render.
type Override struct {
	// Component is the AICR component the value belongs to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Component string `json:"component"`

	// Path is the dotted values path within the component.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Path string `json:"path"`

	// Value is any JSON value; its encoded size is bounded at reconcile time.
	Value apiextensionsv1.JSON `json:"value"`

	// Additive allows a path absent from both the base render and the chart
	// defaults (typo protection opt-out).
	// +optional
	Additive bool `json:"additive,omitempty"`
}

// RolloutPolicy bounds rollouts. Durations are positive, and a single step
// never outlasts the whole rollout.
// +kubebuilder:validation:XValidation:rule="duration(self.stabilizationWindow) >= duration('0s')",message="stabilizationWindow must not be negative"
// +kubebuilder:validation:XValidation:rule="duration(self.actionTimeout) > duration('0s') && duration(self.deadline) > duration('0s')",message="actionTimeout and deadline must be positive"
// +kubebuilder:validation:XValidation:rule="duration(self.actionTimeout) <= duration(self.deadline)",message="actionTimeout must not exceed deadline"
type RolloutPolicy struct {
	// StabilizationWindow is how long a new plan must exist before a rollout.
	// +optional
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	StabilizationWindow *metav1.Duration `json:"stabilizationWindow,omitempty"`

	// ActionTimeout bounds each step.
	// +optional
	// +kubebuilder:default="30m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	ActionTimeout *metav1.Duration `json:"actionTimeout,omitempty"`

	// Deadline bounds the whole rollout.
	// +optional
	// +kubebuilder:default="6h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	Deadline *metav1.Duration `json:"deadline,omitempty"`
}

// DeploymentValidation selects AICR deployment validation.
// +kubebuilder:validation:Enum=Required;Disabled
type DeploymentValidation string

// Deployment validation modes.
const (
	DeploymentValidationRequired DeploymentValidation = "Required"
	DeploymentValidationDisabled DeploymentValidation = "Disabled"
)

// ValidationPolicy selects validation gates.
type ValidationPolicy struct {
	// +optional
	// +kubebuilder:default=Required
	Deployment DeploymentValidation `json:"deployment,omitempty"`
}

// DeletionPolicy decides what deleting the runtime does.
// +kubebuilder:validation:Enum=Orphan;Delete
type DeletionPolicy string

// Deletion policies.
const (
	DeletionOrphan DeletionPolicy = "Orphan"
	DeletionDelete DeletionPolicy = "Delete"
)

// Adoption requests re-adoption of an earlier runtime's engine objects.
type Adoption struct {
	// RuntimeUID is the UID of the runtime whose objects to adopt.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?$`
	RuntimeUID string `json:"runtimeUID,omitempty"`
}

// ClusterRuntimeStatus holds observations rebuilt from specs, plans and live
// objects. It never holds a snapshot, values or credentials.
type ClusterRuntimeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow kstatus. The resolver owns only Resolved; the rollout
	// controller owns every other type. Each controller applies only its own.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	AICR *AICRStatus `json:"aicr,omitempty"`

	// +optional
	Discovery *DiscoveryStatus `json:"discovery,omitempty"`

	// ActivePlanDigest is the desiredStateDigest of the active plan.
	// +optional
	// +kubebuilder:validation:MaxLength=71
	ActivePlanDigest string `json:"activePlanDigest,omitempty"`

	// CandidatePlanDigest is the desiredStateDigest of the newest resolved plan.
	// +optional
	// +kubebuilder:validation:MaxLength=71
	CandidatePlanDigest string `json:"candidatePlanDigest,omitempty"`

	// InputDigest is the resolution inputDigest the candidate plan was
	// resolved for. It is a cache: after its loss the resolver resolves
	// again and arrives at the same plan. Owner: resolver.
	// +optional
	// +kubebuilder:validation:MaxLength=71
	InputDigest string `json:"inputDigest,omitempty"`

	// Rollout names the rollout in progress, if any.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Rollout string `json:"rollout,omitempty"`
}

// AICRStatus is the AICR release the image reported.
type AICRStatus struct {
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`
}

// DiscoveryStatus summarizes the latest discovery.
type DiscoveryStatus struct {
	// +optional
	Snapshot *SnapshotRef `json:"snapshot,omitempty"`
	// +optional
	Criteria *ResolvedCriteria `json:"criteria,omitempty"`
	// +optional
	AcceleratedNodes int32 `json:"acceleratedNodes,omitempty"`
	// +optional
	SystemNodes int32 `json:"systemNodes,omitempty"`
}

// SnapshotRef points at the AICR snapshot used for resolution.
type SnapshotRef struct {
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=71
	Digest string `json:"digest,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ResolvedCriteria are the criteria the recipe was resolved with.
type ResolvedCriteria struct {
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Service string `json:"service,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Accelerator string `json:"accelerator,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	OS string `json:"os,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Intent string `json:"intent,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Platform string `json:"platform,omitempty"`
}

// ClusterRuntimeList is a list of ClusterRuntime.
//
// +kubebuilder:object:root=true
type ClusterRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterRuntime `json:"items"`
}

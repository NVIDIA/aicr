/*
Copyright 2026 NVIDIA CORPORATION & AFFILIATES

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// NicNodePolicyCRDName is used for the CRD Kind.
	NicNodePolicyCRDName = "NicNodePolicy"
	// NicNodePolicyShortName is the abbreviated form used in labels and requestorIDs
	// to keep values within Kubernetes length limits.
	NicNodePolicyShortName = "nnp"
)

// NicNodePolicySpec defines the desired state of NIC drivers and device plugin
type NicNodePolicySpec struct {
	// OFEDDriver is a specialized driver for NVIDIA NICs which can replace the inbox driver that comes with an OS.
	// See https://network.nvidia.com/support/mlnx-ofed-matrix/
	OFEDDriver *OFEDDriverSpec `json:"ofedDriver,omitempty"`
	// RdmaSharedDevicePlugin manages support IB and RoCE HCAs through the Kubernetes device plugin framework.
	// The config field is a json representation of the RDMA shared device plugin configuration.
	// See https://github.com/Mellanox/k8s-rdma-shared-dev-plugin
	RdmaSharedDevicePlugin *DevicePluginSpec `json:"rdmaSharedDevicePlugin,omitempty"`
	// SriovDevicePlugin manages SRIOV through the Kubernetes device plugin framework.
	// The config field is a json representation of the RDMA shared device plugin configuration.
	// See https://github.com/k8snetworkplumbingwg/sriov-network-device-plugin
	SriovDevicePlugin *DevicePluginSpec `json:"sriovDevicePlugin,omitempty"`
	// +kubebuilder:validation:Optional
	// NodeSelector specifies a selector for the nodes this policy applies to
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +kubebuilder:validation:Optional
	// Optional: Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	Labels map[string]string `json:"labels,omitempty"`

	// +kubebuilder:validation:Optional
	// Optional: Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	Annotations map[string]string `json:"annotations,omitempty"`

	// +kubebuilder:validation:Optional
	// Optional: Set tolerations
	// +operator-sdk:gen-csv:customresourcedefinitions.specDescriptors=true
	// +operator-sdk:gen-csv:customresourcedefinitions.specDescriptors.displayName="Tolerations"
	// +operator-sdk:gen-csv:customresourcedefinitions.specDescriptors.x-descriptors="urn:alm:descriptor:com.tectonic.ui:advanced,urn:alm:descriptor:io.kubernetes:Tolerations"
	Tolerations []v1.Toleration `json:"tolerations,omitempty"`
}

// NicNodePolicyStatus defines the observed state of NicNodePolicy
type NicNodePolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Reflects the current state of the cluster policy
	// +kubebuilder:validation:Enum={"ignore", "notReady", "ready", "error"}
	State State `json:"state"`
	// Informative string in case the observed state is error
	Reason string `json:"reason,omitempty"`
	// AppliedStates provide a finer view of the observed state
	AppliedStates []AppliedState `json:"appliedStates,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName={nicnode,nnp}
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.state`,priority=0
// +kubebuilder:printcolumn:name="Age",type=string,JSONPath=`.metadata.creationTimestamp`,priority=0

// NicNodePolicy is the Schema for the NicNodePolicies API
type NicNodePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Defines the desired state of NicNodePolicy
	Spec NicNodePolicySpec `json:"spec,omitempty"`
	// Defines the observed state of NicNodePolicy
	Status NicNodePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true

// NicNodePolicyList contains a list of NicNodePolicy
type NicNodePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicNodePolicy `json:"items"`
}

// GetOFEDDriverSpec implements NicPolicy.
func (n *NicNodePolicy) GetOFEDDriverSpec() *OFEDDriverSpec {
	return n.Spec.OFEDDriver
}

// GetRdmaSharedDevicePluginSpec implements NicPolicy.
func (n *NicNodePolicy) GetRdmaSharedDevicePluginSpec() *DevicePluginSpec {
	return n.Spec.RdmaSharedDevicePlugin
}

// GetSriovDevicePluginSpec implements NicPolicy.
func (n *NicNodePolicy) GetSriovDevicePluginSpec() *DevicePluginSpec {
	return n.Spec.SriovDevicePlugin
}

// GetTolerations implements NicPolicy.
func (n *NicNodePolicy) GetTolerations() []v1.Toleration {
	return n.Spec.Tolerations
}

// GetNodeAffinity implements NicPolicy.
// NicNodePolicy does not have a NodeAffinity field; returns nil.
func (n *NicNodePolicy) GetNodeAffinity() *v1.NodeAffinity {
	return nil
}

// GetNodeSelector implements NicPolicy.
func (n *NicNodePolicy) GetNodeSelector() map[string]string {
	return n.Spec.NodeSelector
}

// GetCRDName implements NicPolicy.
func (n *NicNodePolicy) GetCRDName() string {
	return NicNodePolicyCRDName
}

// GetGlobalConfig implements NicPolicy.
// NicNodePolicy does not have a Global config field; returns nil.
func (n *NicNodePolicy) GetGlobalConfig() *GlobalConfig {
	return nil
}

// GetAppliedStates implements NicPolicyCR.
func (n *NicNodePolicy) GetAppliedStates() []AppliedState {
	return n.Status.AppliedStates
}

// SetAppliedStates implements NicPolicyCR.
func (n *NicNodePolicy) SetAppliedStates(states []AppliedState) {
	n.Status.AppliedStates = states
}

// GetPolicyState implements NicPolicyCR.
func (n *NicNodePolicy) GetPolicyState() State {
	return n.Status.State
}

// SetPolicyState implements NicPolicyCR.
func (n *NicNodePolicy) SetPolicyState(state State) {
	n.Status.State = state
}

// SetReason implements NicPolicyCR.
func (n *NicNodePolicy) SetReason(reason string) {
	n.Status.Reason = reason
}

func init() {
	SchemeBuilder.Register(&NicNodePolicy{}, &NicNodePolicyList{})
}

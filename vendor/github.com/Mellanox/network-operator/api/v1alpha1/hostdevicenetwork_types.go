/*
Copyright 2021 NVIDIA

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// HostDeviceNetworkCRDName is used for the CRD Kind.
	HostDeviceNetworkCRDName = "HostDeviceNetwork"
)

// HostDeviceNetworkSpec defines the desired state of HostDeviceNetwork
type HostDeviceNetworkSpec struct {
	// Namespace of the NetworkAttachmentDefinition custom resource
	NetworkNamespace string `json:"networkNamespace,omitempty"`
	// Host device resource pool name
	ResourceName string `json:"resourceName,omitempty"`
	// IPAM configuration to be used for this network
	IPAM string `json:"ipam,omitempty"`
}

// HostDeviceNetworkStatus defines the observed state of HostDeviceNetwork
type HostDeviceNetworkStatus struct {
	// Reflects the state of the HostDeviceNetwork
	// +kubebuilder:validation:Enum={"notReady", "ready", "error"}
	State State `json:"state"`
	// Network attachment definition generated from HostDeviceNetworkSpec
	HostDeviceNetworkAttachmentDef string `json:"hostDeviceNetworkAttachmentDef,omitempty"`
	// Informative string in case the observed state is error
	Reason string `json:"reason,omitempty"`
	// AppliedStates provide a finer view of the observed state
	AppliedStates []AppliedState `json:"appliedStates,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.state`,priority=0
// +kubebuilder:printcolumn:name="Age",type=string,JSONPath=`.metadata.creationTimestamp`,priority=0

// HostDeviceNetwork is the Schema for the hostdevicenetworks API
type HostDeviceNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Defines the desired state of HostDeviceNetwork
	Spec HostDeviceNetworkSpec `json:"spec,omitempty"`
	// Defines the observed state of HostDeviceNetwork
	Status HostDeviceNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true

// HostDeviceNetworkList contains a list of HostDeviceNetwork
type HostDeviceNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostDeviceNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostDeviceNetwork{}, &HostDeviceNetworkList{})
}

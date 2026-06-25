/*
2022 NVIDIA CORPORATION & AFFILIATES

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
	// IPoIBNetworkCRDName is used for the CRD Kind.
	IPoIBNetworkCRDName = "IPoIBNetwork"
)

// IPoIBNetworkSpec defines the desired state of IPoIBNetwork
type IPoIBNetworkSpec struct {
	// Namespace of the NetworkAttachmentDefinition custom resource
	NetworkNamespace string `json:"networkNamespace,omitempty"`
	// Name of the host interface to enslave. Defaults to default route interface
	Master string `json:"master,omitempty"`
	// IPAM configuration to be used for this network.
	IPAM string `json:"ipam,omitempty"`
}

// IPoIBNetworkStatus defines the observed state of IPoIBNetwork
type IPoIBNetworkStatus struct {
	// Reflects the state of the IPoIBNetwork
	// +kubebuilder:validation:Enum={"notReady", "ready", "error"}
	State State `json:"state"`
	// Network attachment definition generated from IPoIBNetworkSpec
	IPoIBNetworkAttachmentDef string `json:"ipoibNetworkAttachmentDef,omitempty"`
	// Informative string in case the observed state is error
	Reason string `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.state`,priority=0
// +kubebuilder:printcolumn:name="Age",type=string,JSONPath=`.metadata.creationTimestamp`,priority=0

// IPoIBNetwork is the Schema for the ipoibnetworks API
type IPoIBNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Defines the desired state of IPoIBNetwork
	Spec IPoIBNetworkSpec `json:"spec,omitempty"`
	// Defines the observed state of IPoIBNetwork
	Status IPoIBNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true

// IPoIBNetworkList contains a list of IPoIBNetwork
type IPoIBNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPoIBNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IPoIBNetwork{}, &IPoIBNetworkList{})
}

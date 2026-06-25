/*
2026 NVIDIA CORPORATION & AFFILIATES
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

// NicInterfaceNameTemplateSpec defines the desired state of NicInterfaceNameTemplate
type NicInterfaceNameTemplateSpec struct {
	// NodeSelector contains labels required on the node. When empty, the template will be applied to matching devices on all nodes.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// PfsPerNic specifies the number of PFs per NIC
	// Used to calculate the number of planes per NIC
	// +required
	PfsPerNic int `json:"pfsPerNic"`
	// RdmaDevicePrefix specifies the prefix for the rdma device name
	// %nic_id%, %plane_id% and %rail_id% placeholders can be used to construct the device name
	// %nic_id% is the index of the NIC in the flattened list of NICs
	// %plane_id% is the index of the plane of the specific NIC
	// %rail_id% is the index of the rail where the given NIC belongs to
	// +required
	RdmaDevicePrefix string `json:"rdmaDevicePrefix"`
	// NetDevicePrefix specifies the prefix for the net device name
	// %nic_id%, %plane_id% and %rail_id% placeholders can be used to construct the device name
	// %nic_id% is the index of the NIC in the flattened list of NICs
	// %plane_id% is the index of the plane of the specific NIC
	// %rail_id% is the index of the rail where the given NIC belongs to
	// +required
	NetDevicePrefix string `json:"netDevicePrefix"`
	// RailPciAddresses defines the PCI address to rail mapping and order
	// The first dimension is the rail index, the second dimension is the PCI addresses of the NICs in the rail.
	// The PCI addresses must be sorted in the order of the rails.
	// Example: [["0000:1a:00.0", "0000:2a:00.0"], ["0000:3a:00.0", "0000:4a:00.0"]] specifies 2 rails with 2 NICs each.
	// +required
	RailPciAddresses [][]string `json:"railPciAddresses"`
}

// NicInterfaceNameTemplateStatus defines the observed state of NicInterfaceNameTemplate
type NicInterfaceNameTemplateStatus struct {
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NicInterfaceNameTemplate is the Schema for the nicinterfacenametemplates API
type NicInterfaceNameTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NicInterfaceNameTemplateSpec   `json:"spec,omitempty"`
	Status NicInterfaceNameTemplateStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NicInterfaceNameTemplateList contains a list of NicInterfaceNameTemplate
type NicInterfaceNameTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicInterfaceNameTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NicInterfaceNameTemplate{}, &NicInterfaceNameTemplateList{})
}

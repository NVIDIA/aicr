/*
2025 NVIDIA CORPORATION & AFFILIATES
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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// FirmwareTemplateSpec specifies a FW update policy for a given FW source ref
type FirmwareTemplateSpec struct {
	// NicFirmwareSourceRef refers to existing NicFirmwareSource CR on where to get the FW from
	// +required
	NicFirmwareSourceRef string `json:"nicFirmwareSourceRef"`
	// UpdatePolicy indicates whether the operator needs to validate installed FW or upgrade it
	// +kubebuilder:validation:Enum=Validate;Update
	// +required
	UpdatePolicy string `json:"updatePolicy"`
}

// NicFirmwareTemplateSpec defines the FW templates and node/nic selectors for it
type NicFirmwareTemplateSpec struct {
	// NodeSelector contains labels required on the node. When empty, the template will be applied to matching devices on all nodes.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// NIC selector configuration
	// +required
	NicSelector *NicSelectorSpec `json:"nicSelector"`
	// Firmware update template
	// +required
	Template *FirmwareTemplateSpec `json:"template"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NicFirmwareTemplate is the Schema for the nicfirmwaretemplates API
type NicFirmwareTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NicFirmwareTemplateSpec `json:"spec,omitempty"`
	Status NicTemplateStatus       `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NicFirmwareTemplateList contains a list of NicFirmwareTemplate
type NicFirmwareTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicFirmwareTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NicFirmwareTemplate{}, &NicFirmwareTemplateList{})
}

/*
2024 NVIDIA CORPORATION & AFFILIATES
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

// NicSelectorSpec is a desired configuration for NICs
type NicSelectorSpec struct {
	// Type of the NIC to be selected, e.g. 101d,1015,a2d6 etc.
	NicType string `json:"nicType"`
	// Array of PCI addresses to be selected, e.g. "0000:03:00.0"
	// +kubebuilder:validation:items:Pattern=`^0000:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`
	PciAddresses []string `json:"pciAddresses,omitempty"`
	// Serial numbers of the NICs to be selected, e.g. MT2116X09299
	SerialNumbers []string `json:"serialNumbers,omitempty"`
}

// LinkTypeEnum described the link type (Ethernet / Infiniband)
// +enum
type LinkTypeEnum string

// PciPerformanceOptimizedSpec specifies PCI performance optimization settings
type PciPerformanceOptimizedSpec struct {
	// Specifies whether to enable PCI performance optimization
	Enabled bool `json:"enabled"`
	// Specifies the PCIe Max Accumulative Outstanding read bytes
	MaxAccOutRead int `json:"maxAccOutRead,omitempty"`
	// Specifies the size of a single PCI read request in bytes
	// +kubebuilder:validation:Enum=128;256;512;1024;2048;4096
	MaxReadRequest int `json:"maxReadRequest,omitempty"`
}

// QosSpec specifies Quality of Service settings
type QosSpec struct {
	// Trust mode for QoS settings, e.g. trust-dscp
	Trust string `json:"trust"`
	// Priority-based Flow Control configuration, e.g. "0,0,0,1,0,0,0,0"
	// +kubebuilder:validation:Pattern=`^([01],){7}[01]$`
	PFC string `json:"pfc"`
	// 8-bit value for type of service
	ToS int `json:"tos,omitempty"`
}

// RoceOptimizedSpec specifies RoCE optimization settings
type RoceOptimizedSpec struct {
	// Optimize RoCE
	Enabled bool `json:"enabled"`
	// Quality of Service settings
	Qos *QosSpec `json:"qos,omitempty"`
}

// GpuDirectOptimizedSpec specifies GPU Direct optimization settings
type GpuDirectOptimizedSpec struct {
	// Optimize GPU Direct
	Enabled bool `json:"enabled"`
	// GPU direct environment, e.g. Baremetal
	Env string `json:"env"`
}

// SpectrumXOptimizedSpec enables Spectrum-X specific optimizations
// +kubebuilder:validation:XValidation:rule="!has(self.multiplaneMode) || !has(self.numberOfPlanes) || self.multiplaneMode != 'none' || self.numberOfPlanes == 1",message="when MultiplaneMode is none, numberOfPlanes must be 1"
// +kubebuilder:validation:XValidation:rule="!has(self.multiplaneMode) || !has(self.numberOfPlanes) || self.multiplaneMode == 'none' || self.numberOfPlanes != 1",message="when MultiplaneMode is not none, numberOfPlanes must not be 1"
// +kubebuilder:validation:XValidation:rule="!has(self.version) || !has(self.multiplaneMode) || !has(self.numberOfPlanes) || !(self.version == 'RA1.3' || self.version == 'RA2.0') || (self.multiplaneMode == 'none' && self.numberOfPlanes == 1)",message="when Version is RA1.3 or RA2.0, MultiplaneMode must be none and numberOfPlanes must be 1"
type SpectrumXOptimizedSpec struct {
	// Optimize Spectrum X
	Enabled bool `json:"enabled"`
	// Version of the Spectrum-X architecture to optimize for
	// +kubebuilder:validation:Enum=RA1.3;RA2.0;RA2.1
	// +required
	Version string `json:"version"`
	// Overlay mode to be configured
	// Can be "l3" or "none"
	// +kubebuilder:validation:Enum=l3;none
	// +kubebuilder:default:=none
	// +optional
	Overlay string `json:"overlay,omitempty"`
	// Multiplane mode to be configured
	// Can be "none", "swplb", "hwplb", or "uniplane"
	// +kubebuilder:validation:Enum=none;swplb;hwplb;uniplane
	// +kubebuilder:default:=none
	// +optional
	MultiplaneMode string `json:"multiplaneMode,omitempty"`
	// Number of planes to be configured
	// +kubebuilder:validation:Enum=1;2;4
	// +kubebuilder:default:=1
	// +optional
	NumberOfPlanes int `json:"numberOfPlanes,omitempty"`
}

type NvConfigParam struct {
	// Name of the arbitrary nvconfig parameter
	Name string `json:"name"`
	// Value of the arbitrary nvconfig parameter
	Value string `json:"value"`
}

// ConfigurationTemplateSpec is a set of configurations for the NICs
// +kubebuilder:validation:XValidation:rule="!(has(self.spectrumXOptimized) && self.spectrumXOptimized.enabled) || (self.linkType == 'Ethernet' && self.numVfs == 1)",message="spectrumXOptimized can be enabled only when linkType=='Ethernet' and numVfs==1"
// +kubebuilder:validation:XValidation:rule="!(has(self.spectrumXOptimized) && self.spectrumXOptimized.enabled) || !(has(self.roceOptimized) && self.roceOptimized.enabled)",message="spectrumXOptimized includes RoCE optimizations, so separate roceOptimized section must not be enabled"
// +kubebuilder:validation:XValidation:rule="!(has(self.spectrumXOptimized) && self.spectrumXOptimized.enabled) || !has(self.rawNvConfig) || size(self.rawNvConfig) == 0",message="when spectrumXOptimized is enabled, rawNvConfig must be empty"
type ConfigurationTemplateSpec struct {
	// Number of VFs to be configured
	// +required
	NumVfs int `json:"numVfs"`
	// LinkType to be configured, Ethernet|Infiniband
	// +kubebuilder:validation:Enum=Ethernet;Infiniband
	// +required
	LinkType LinkTypeEnum `json:"linkType"`
	// PCI performance optimization settings
	PciPerformanceOptimized *PciPerformanceOptimizedSpec `json:"pciPerformanceOptimized,omitempty"`
	// RoCE optimization settings
	RoceOptimized *RoceOptimizedSpec `json:"roceOptimized,omitempty"`
	// GPU Direct optimization settings
	GpuDirectOptimized *GpuDirectOptimizedSpec `json:"gpuDirectOptimized,omitempty"`
	// Spectrum-X optimization settings. Works only with linkType==Ethernet && numVfs==0. Other optimizations must be skipped or disabled. RawNvConfig must be empty.
	SpectrumXOptimized *SpectrumXOptimizedSpec `json:"spectrumXOptimized,omitempty"`
	// List of arbitrary nv config parameters
	RawNvConfig []NvConfigParam `json:"rawNvConfig,omitempty"`
}

// NicConfigurationTemplateSpec defines the desired state of NicConfigurationTemplate
// +kubebuilder:validation:XValidation:rule="!(has(self.template.spectrumXOptimized) && self.template.spectrumXOptimized.enabled) || (self.nicSelector.nicType == '1023' || self.nicSelector.nicType == 'a2dc')",message="spectrumXOptimized can be enabled only for ConnectX-8 or BlueField-3 SuperNICs"
// +kubebuilder:validation:XValidation:rule="!has(self.template.spectrumXOptimized) || !has(self.template.spectrumXOptimized.multiplaneMode) || self.template.spectrumXOptimized.multiplaneMode != 'hwplb' || self.nicSelector.nicType == '1023'",message="hwplb MultiplaneMode can only be enabled for ConnectX-8 (NicType 1023)"
type NicConfigurationTemplateSpec struct {
	// NodeSelector contains labels required on the node. When empty, the template will be applied to matching devices on all nodes.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// NIC selector configuration
	// +required
	NicSelector *NicSelectorSpec `json:"nicSelector"`
	// ResetToDefault specifies whether node agent needs to perform a reset flow
	// The following operations will be performed:
	// * Nvconfig reset of all non-volatile configurations
	//   - Mstconfig -d <device> reset for each PF
	//   - Mstconfig -d <device> set ADVANCED_PCI_SETTINGS=1
	// * Node reboot
	//   - Applies new NIC NV config
	//   - Will undo any runtime configuration previously performed for the device/driver
	// +optional
	// +kubebuilder:default:=false
	ResetToDefault bool `json:"resetToDefault,omitempty"`
	// Configuration template to be applied to matching devices
	Template *ConfigurationTemplateSpec `json:"template"`
}

// NicTemplateStatus defines the observed state of NicConfigurationTemplate and NicFirmwareTemplate
type NicTemplateStatus struct {
	// NicDevice CRs matching this configuration / firmware template
	NicDevices []string `json:"nicDevices,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NicConfigurationTemplate is the Schema for the nicconfigurationtemplates API
type NicConfigurationTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Defines the desired state of NICs
	Spec NicConfigurationTemplateSpec `json:"spec,omitempty"`
	// Defines the observed state of NicConfigurationTemplate
	Status NicTemplateStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NicConfigurationTemplateList contains a list of NicConfigurationTemplate
type NicConfigurationTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicConfigurationTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NicConfigurationTemplate{}, &NicConfigurationTemplateList{})
}

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

// NicDeviceConfigurationSpec contains desired configuration of the NIC
type NicDeviceConfigurationSpec struct {
	// ResetToDefault specifies whether node agent needs to perform a reset flow.
	// The following operations will be performed:
	// * Nvconfig reset of all non-volatile configurations
	//   - Mstconfig -d <device> reset for each PF
	//   - Mstconfig -d <device> set ADVANCED_PCI_SETTINGS=1
	// * Node reboot
	//   - Applies new NIC NV config
	//   - Will undo any runtime configuration previously performed for the device/driver
	ResetToDefault bool `json:"resetToDefault,omitempty"`
	// Configuration template applied from the NicConfigurationTemplate CR
	Template *ConfigurationTemplateSpec `json:"template,omitempty"`
}

type NicDeviceInterfaceNameSpec struct {
	// NicIndex is the index of the NIC in the flattened list of NICs based on the Template
	NicIndex int `json:"nicIndex"`
	// RailIndex is the index of the rail where the given NIC belongs to based on the Template
	RailIndex int `json:"railIndex"`
	// PlaneIndices is the indices of the planes for the given NIC based on the Template
	PlaneIndices []int `json:"planeIndices"`
	// --- Parameters from the NicInterfaceNameTemplate CR ---
	// RdmaDevicePrefix specifies the prefix for the rdma device name
	RdmaDevicePrefix string `json:"rdmaDevicePrefix"`
	// NetDevicePrefix specifies the prefix for the net device name
	NetDevicePrefix string `json:"netDevicePrefix"`
}

// NicDeviceSpec defines the desired state of NicDevice
type NicDeviceSpec struct {
	// Configuration specifies the configuration requested by NicConfigurationTemplate
	Configuration *NicDeviceConfigurationSpec `json:"configuration,omitempty"`
	// Firmware specifies the fw upgrade policy requested by NicFirmwareTemplate
	Firmware *FirmwareTemplateSpec `json:"firmware,omitempty"`
	// InterfaceNameTemplate specifies the interface name template to be applied to the NIC
	InterfaceNameTemplate *NicDeviceInterfaceNameSpec `json:"interfaceNameTemplate,omitempty"`
}

// NicDevicePortSpec describes the ports of the NIC
type NicDevicePortSpec struct {
	// PCI is a PCI address of the port, e.g. 0000:3b:00.0
	PCI string `json:"pci"`
	// NetworkInterface is the name of the network interface for this port, e.g. eth1
	NetworkInterface string `json:"networkInterface,omitempty"`
	// RdmaInterface is the name of the rdma interface for this port, e.g. mlx5_1
	RdmaInterface string `json:"rdmaInterface,omitempty"`
}

// NicDeviceStatus defines the observed state of NicDevice
type NicDeviceStatus struct {
	// Node where the device is located
	Node string `json:"node"`
	// Type of device, e.g. ConnectX7
	Type string `json:"type"`
	// Serial number of the device, e.g. MT2116X09299
	SerialNumber string `json:"serialNumber"`
	// Part number of the device, e.g. MCX713106AEHEA_QP1
	PartNumber string `json:"partNumber"`
	// Product Serial ID of the device, e.g. MT_0000000221
	PSID string `json:"psid"`
	// Firmware version currently installed on the device, e.g. 22.31.1014
	FirmwareVersion string `json:"firmwareVersion"`
	// DPU indicates if the device is a BlueField in DPU mode
	DPU bool `json:"dpu"`
	// ModelName is the model name of the device, e.g. ConnectX-6 or BlueField-3
	ModelName string `json:"modelName"`
	// SuperNIC indicates if the device is a SuperNIC
	SuperNIC bool `json:"superNIC"`
	// List of ports for the device
	Ports []NicDevicePortSpec `json:"ports"`
	// List of conditions observed for the device
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NicDevice is the Schema for the nicdevices API
type NicDevice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NicDeviceSpec   `json:"spec,omitempty"`
	Status NicDeviceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NicDeviceList contains a list of NicDevice
type NicDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NicDevice{}, &NicDeviceList{})
}

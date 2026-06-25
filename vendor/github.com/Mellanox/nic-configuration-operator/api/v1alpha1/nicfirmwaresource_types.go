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

// NicFirmwareSourceSpec represents a list of url sources for FW
// +kubebuilder:validation:XValidation:rule="size(self.binUrlSources) > 0 || size(self.bfbUrlSource) > 0 || size(self.docaSpcXCCUrlSource) > 0",message="At least one of binUrlSources, bfbUrlSource, or docaSpcXCCUrlSource must be specified"
type NicFirmwareSourceSpec struct {
	// BinUrlSources represents a list of url sources for ConnectX Firmware
	// +kubebuilder:validation:MinItems=1
	// +optional
	BinUrlSources []string `json:"binUrlSources,omitempty"`
	// BFBUrlSource represents a url source for BlueField Bundle
	// +optional
	BFBUrlSource string `json:"bfbUrlSource,omitempty"`
	// DocaSpcXCCUrlSource represents a url source for DOCA SPC-X CC .deb package for ubuntu 22.04
	// Will be removed in the future, once Doca SPC-X CC algorithm will be publicly available
	// +optional
	DocaSpcXCCUrlSource string `json:"docaSpcXCCUrlSource,omitempty"`
}

// NicFirmwareSourceStatus represents the status of the FW from given sources, e.g. version available for PSIDs
type NicFirmwareSourceStatus struct {
	// State represents the firmware processing state
	// +kubebuilder:validation:Enum=Downloading;Processing;Success;ProcessingFailed;DownloadFailed;CacheVerificationFailed
	// +required
	State string `json:"state"`
	// Reason shows an error message if occurred
	Reason string `json:"reason,omitempty"`
	// Versions is a map of available FW binaries versions to PSIDs
	// a PSID should have only a single FW version available for it
	BinaryVersions map[string][]string `json:"binaryVersions,omitempty"`
	// BFBVersions represents the FW versions available in the provided BFB bundle
	BFBVersions map[string]string `json:"bfbVersions,omitempty"`
	// DocaSpcXCCVersion represents the FW versions available in the provided DOCA SPC-X CC .deb package for ubuntu 22.04
	DocaSpcXCCVersion string `json:"docaSpcXCCVersion,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// NicFirmwareSource is the Schema for the nicfirmwaresources API
type NicFirmwareSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NicFirmwareSourceSpec   `json:"spec,omitempty"`
	Status NicFirmwareSourceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NicFirmwareSourceList contains a list of NicFirmwareSource
type NicFirmwareSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicFirmwareSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NicFirmwareSource{}, &NicFirmwareSourceList{})
}

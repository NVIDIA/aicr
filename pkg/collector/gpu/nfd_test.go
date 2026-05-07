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

package gpu

import (
	"testing"

	nfdv1alpha1 "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
)

func TestExtractHardwareInfo(t *testing.T) {
	tests := []struct {
		name           string
		pciFeatures    *nfdv1alpha1.Features
		kernelFeatures *nfdv1alpha1.Features
		wantPresent    bool
		wantCount      int
		wantDriver     bool
	}{
		{
			name: "NVIDIA GPUs present (2 NVIDIA + 1 Intel)",
			pciFeatures: &nfdv1alpha1.Features{
				Instances: map[string]nfdv1alpha1.InstanceFeatureSet{
					nfdPCIDeviceFeature: {
						Elements: []nfdv1alpha1.InstanceFeature{
							{Attributes: map[string]string{"vendor": nvidiaVendorID, "class": pciClassVGA}},
							{Attributes: map[string]string{"vendor": nvidiaVendorID, "class": pciClass3D}},
							{Attributes: map[string]string{"vendor": "8086", "class": pciClassVGA}},
						},
					},
				},
			},
			wantPresent: true,
			wantCount:   2,
		},
		{
			name: "no NVIDIA GPUs (Intel only)",
			pciFeatures: &nfdv1alpha1.Features{
				Instances: map[string]nfdv1alpha1.InstanceFeatureSet{
					nfdPCIDeviceFeature: {
						Elements: []nfdv1alpha1.InstanceFeature{
							{Attributes: map[string]string{"vendor": "8086", "class": pciClassVGA}},
						},
					},
				},
			},
			wantPresent: false,
			wantCount:   0,
		},
		{
			name: "3D controller class detected",
			pciFeatures: &nfdv1alpha1.Features{
				Instances: map[string]nfdv1alpha1.InstanceFeatureSet{
					nfdPCIDeviceFeature: {
						Elements: []nfdv1alpha1.InstanceFeature{
							{Attributes: map[string]string{"vendor": nvidiaVendorID, "class": pciClass3D}},
						},
					},
				},
			},
			wantPresent: true,
			wantCount:   1,
		},
		{
			name: "driver loaded",
			pciFeatures: &nfdv1alpha1.Features{
				Instances: map[string]nfdv1alpha1.InstanceFeatureSet{
					nfdPCIDeviceFeature: {
						Elements: []nfdv1alpha1.InstanceFeature{
							{Attributes: map[string]string{"vendor": nvidiaVendorID, "class": pciClassVGA}},
						},
					},
				},
			},
			kernelFeatures: &nfdv1alpha1.Features{
				Flags: map[string]nfdv1alpha1.FlagFeatureSet{
					nfdKernelLoadedModuleKey: {
						Elements: map[string]nfdv1alpha1.Nil{
							nvidiaKernelModule: {},
						},
					},
				},
			},
			wantPresent: true,
			wantCount:   1,
			wantDriver:  true,
		},
		{
			name: "driver not loaded",
			pciFeatures: &nfdv1alpha1.Features{
				Instances: map[string]nfdv1alpha1.InstanceFeatureSet{
					nfdPCIDeviceFeature: {
						Elements: []nfdv1alpha1.InstanceFeature{
							{Attributes: map[string]string{"vendor": nvidiaVendorID, "class": pciClassVGA}},
						},
					},
				},
			},
			kernelFeatures: &nfdv1alpha1.Features{
				Flags: map[string]nfdv1alpha1.FlagFeatureSet{
					nfdKernelLoadedModuleKey: {
						Elements: map[string]nfdv1alpha1.Nil{
							"i915": {},
						},
					},
				},
			},
			wantPresent: true,
			wantCount:   1,
			wantDriver:  false,
		},
		{
			name:        "empty features",
			pciFeatures: &nfdv1alpha1.Features{},
			wantPresent: false,
			wantCount:   0,
		},
		{
			name:        "nil features",
			pciFeatures: nil,
			wantPresent: false,
			wantCount:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := extractHardwareInfo(tt.pciFeatures, tt.kernelFeatures)
			if info.GPUPresent != tt.wantPresent {
				t.Errorf("GPUPresent = %v, want %v", info.GPUPresent, tt.wantPresent)
			}
			if info.GPUCount != tt.wantCount {
				t.Errorf("GPUCount = %v, want %v", info.GPUCount, tt.wantCount)
			}
			if info.DriverLoaded != tt.wantDriver {
				t.Errorf("DriverLoaded = %v, want %v", info.DriverLoaded, tt.wantDriver)
			}
		})
	}
}

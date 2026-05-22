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

package mirror

func testMirrorList() *MirrorList {
	return &MirrorList{
		Images: []string{
			"nvcr.io/nvidia/cloud-native/gpu-operator-validator:v25.3.0",
			"nvcr.io/nvidia/gpu-operator:v25.3.0",
			"registry.k8s.io/sig-storage/csi-provisioner:v5.2.0",
		},
		Charts: []ChartRef{
			{
				Name:       "gpu-operator",
				Repository: "oci://ghcr.io/nvidia",
				Chart:      "gpu-operator",
				Version:    "v25.3.0",
				Namespace:  "gpu-operator",
			},
			{
				Name:       "aws-ebs-csi-driver",
				Repository: "https://kubernetes-sigs.github.io/aws-ebs-csi-driver",
				Chart:      "aws-ebs-csi-driver",
				Version:    "2.40.0",
				Namespace:  "kube-system",
			},
		},
		Components: []ComponentImages{
			{
				Component: "gpu-operator",
				Type:      "helm",
				Images: []string{
					"nvcr.io/nvidia/cloud-native/gpu-operator-validator:v25.3.0",
					"nvcr.io/nvidia/gpu-operator:v25.3.0",
				},
			},
			{
				Component: "aws-ebs-csi-driver",
				Type:      "helm",
				Images: []string{
					"registry.k8s.io/sig-storage/csi-provisioner:v5.2.0",
				},
			},
		},
		Metadata: MirrorListMetadata{
			RecipeVersion: "v1.0.0",
			Criteria:      "service=eks accelerator=h100 intent=training os=ubuntu",
		},
	}
}

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

package main

// Kubernetes API groups referenced across conformance checks.
const (
	apiGroupAPIExtensions = "apiextensions.k8s.io"
	apiGroupResourceK8sIO = "resource.k8s.io"
	apiGroupGateway       = "gateway.networking.k8s.io"
	apiGroupNVIDIA        = "nvidia.com"

	// resourceNVIDIAGPU is the canonical Kubernetes extended-resource name for NVIDIA GPUs.
	resourceNVIDIAGPU = "nvidia.com/gpu"
	// resourceCRDs is the Kubernetes API resource name for CustomResourceDefinitions.
	resourceCRDs = "customresourcedefinitions"
	// versionV1alpha1 is the API version used by NVIDIA dynamo CRDs and trainjob CRDs.
	versionV1alpha1 = "v1alpha1"
	// labelNVIDIAGPUPresent is the "key=value" selector for GPU-bearing nodes
	// when scaled-up via the cluster autoscaler.
	labelNVIDIAGPUPresent = "nvidia.com/gpu.present=true"
)

// Keys used when constructing unstructured Kubernetes manifests as map[string]any.
const (
	keyAPIVersion = "apiVersion"
	keyKind       = "kind"
	keyMetadata   = "metadata"
	keyName       = "name"
	keyNamespace  = "namespace"
	keySpec       = "spec"
)

// Common workload-related string constants used in multiple checks.
const (
	labelApp                    = "app"
	containerNameSleep          = "sleep"
	gpuClaimName                = "gpu"
	statusUnknown               = "unknown"
	deploymentClusterAutoscaler = "cluster-autoscaler"
	namespaceDynamoSystem       = "dynamo-system"
)

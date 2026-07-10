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

import "k8s.io/apimachinery/pkg/runtime/schema"

// resourceClaimTemplateGVR identifies the standard K8s DRA
// ResourceClaimTemplate kind at the GA (v1) group-version. Consumed by the
// NVLS/NCCL validator (nccl_all_reduce_bw_constraint.go: ComputeDomain→RCT
// reconciliation and cleanup). inference-perf no longer uses it — its worker
// claim template is created at the allocmode probe's SERVED version via
// allocmode.GVRAt (see applyWorkerClaimTemplate).
var resourceClaimTemplateGVR = schema.GroupVersionResource{
	Group:    "resource.k8s.io",
	Version:  "v1",
	Resource: "resourceclaimtemplates",
}

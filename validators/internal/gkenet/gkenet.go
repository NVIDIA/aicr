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

// Package gkenet discovers the GKE multi-NIC networking objects that
// GPUDirect TCPXO depends on. It is shared by the deployment-phase
// prerequisite check and the performance-phase NCCL benchmark so both
// gate on the same definition of "the cluster has GPU NIC networks".
package gkenet

import (
	"context"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// RequiredGPUNICNetworks is the number of GPU NIC networks GPUDirect TCPXO
// needs on an a3-megagpu-8g node — one per GPU, bound to eth1..eth8. Both
// the deployment-phase prerequisite check and the performance-phase NCCL
// benchmark gate on this value so a cluster that clears the deployment gate
// cannot fail the benchmark's own discovery for the same reason.
//
// The floor is fixed to the a3-megagpu-8g shape rather than derived from the
// node's GPU count: only the H100 GKE training recipe declares gke-nccl-tcpxo,
// and that recipe targets a3-megagpu-8g. Making it SKU-aware is tracked
// separately.
const RequiredGPUNICNetworks = 8

// GPUNICNameSubstring identifies a GPU NIC network by name.
//
// Network names are chosen by whoever provisions the cluster, not assigned by
// GKE — Google's own sample manifests name them "vpc1".."vpc8". Matching a
// substring rather than exact names is what lets a cluster carry a local prefix
// (e.g. "aicr-demo2-gpu-nic-0") while still being discoverable. Because the
// names are operator-chosen, containing this substring is a documented
// provisioning REQUIREMENT, not an observation about GKE's behavior; see
// docs/integrator/gke-tcpxo-networking.md.
const GPUNICNameSubstring = "gpu-nic"

// NetworkGVR is the cluster-scoped GKE Network CR that multi-networking
// binds into the cluster. Its absence is what this package detects.
var NetworkGVR = schema.GroupVersionResource{
	Group: "networking.gke.io", Version: "v1", Resource: "networks",
}

// DiscoverGPUNICNetworks lists networks.networking.gke.io and returns the
// GPU NIC network names, sorted alphabetically.
//
// The returned error is the RAW Kubernetes API error, deliberately not wrapped
// in a pkg/errors code, because callers classify it by shape and would lose
// that ability behind a wrap. The deployment check relies on exactly this:
// it routes an apierrors.IsNotFound error (the cluster does not serve this GVR
// at all, i.e. it was created without --enable-multi-networking) through
// validators.Capability.Require, which is declaration-gated, and every other
// error through RequireList, which always blocks. Do not wrap or normalize
// these errors without updating that caller.
//
// A cluster that serves the API but has no matching networks returns an empty
// slice and a nil error — "none found" is a verdict for the caller, distinct
// from both a failure to read and an absent API.
func DiscoverGPUNICNetworks(ctx context.Context, dynamicClient dynamic.Interface) ([]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	networks, err := dynamicClient.Resource(NetworkGVR).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var gpuNICs []string
	for _, n := range networks.Items {
		if name := n.GetName(); strings.Contains(name, GPUNICNameSubstring) {
			gpuNICs = append(gpuNICs, name)
		}
	}

	sort.Strings(gpuNICs)
	return gpuNICs, nil
}

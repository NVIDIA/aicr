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

package gkenet

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Network condition types the GKE multi-networking controller reports on a
// Network's status.conditions. NetworkReady/Ready means the Network is fully
// configured; ParamsReady means its parametersRef binding resolved to a valid,
// ready GKENetworkParamSet (GNPDeleted / ParamsNotReady otherwise).
const (
	conditionReady       = "Ready"
	conditionParamsReady = "ParamsReady"
	conditionTrue        = "True"
)

// GPUNICNetworkStatus is the capability view of one GPU NIC Network — what the
// deployment check gates on beyond mere existence (#2265): is it ready, and is
// its GKENetworkParamSet binding intact.
type GPUNICNetworkStatus struct {
	// Name is the Network's metadata.name.
	Name string
	// Ready is the Network's status.conditions[Ready] — fully configured.
	Ready bool
	// ParamsReady is status.conditions[ParamsReady] — the parametersRef binding
	// resolved to a valid GKENetworkParamSet.
	ParamsReady bool
	// ParamSetName is the spec.parametersRef.name the Network binds to ("" if unset).
	ParamSetName string
	// ReadyDetail is the reason/message of a failing Ready condition ("" when healthy).
	ReadyDetail string
	// ParamsReadyDetail is the reason/message of a failing ParamsReady condition
	// ("" when healthy).
	ParamsReadyDetail string
	// retained for the message that leads with it.
}

// Bound reports whether the Network is Ready, its ParamsReady binding resolved, AND
// it references a real GKENetworkParamSet — the single definition of "usable by a
// workload" shared by the readiness arm and the runtime-wiring arm (#2265).
func (s GPUNICNetworkStatus) Bound() bool {
	return s.Ready && s.ParamsReady && s.ParamSetName != ""
}

// DiscoverGPUNICNetworkStatus lists networks.networking.gke.io and returns the
// capability status of every GPU NIC Network (the same substring filter as
// DiscoverGPUNICNetworks), for the deployment check's case-2 arm: an existing
// but unready or mis-bound Network must fail, not count toward the census.
//
// It preserves the package's raw-error contract: the list error is returned
// unwrapped so callers keep classifying it by shape (apierrors.IsNotFound vs
// everything else). A served-but-empty result is an empty slice and nil error.
func DiscoverGPUNICNetworkStatus(ctx context.Context, dynamicClient dynamic.Interface) ([]GPUNICNetworkStatus, error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	networks, err := dynamicClient.Resource(NetworkGVR).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var out []GPUNICNetworkStatus
	for i := range networks.Items {
		n := &networks.Items[i]
		name := n.GetName()
		if !strings.Contains(name, recipe.GPUNICNameSubstring) {
			continue
		}
		out = append(out, networkStatus(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// networkStatus reads one Network's readiness + binding conditions from its
// unstructured form. Absent conditions read as not-ready — a Network that never
// got its status populated has not proven it can bind a NIC.
func networkStatus(n *unstructured.Unstructured) GPUNICNetworkStatus {
	st := GPUNICNetworkStatus{
		Name:         n.GetName(),
		ParamSetName: paramSetRefName(n),
		// Absent conditions read as not-ready with a clear cause, never an empty ().
		ReadyDetail:       "no Ready condition reported",
		ParamsReadyDetail: "no ParamsReady condition reported",
	}
	conds, found, _ := unstructured.NestedSlice(n.Object, "status", "conditions")
	if !found {
		st.ReadyDetail = "no status.conditions reported"
		st.ParamsReadyDetail = "no status.conditions reported"
		return st
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(m, "type")
		condStatus, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		message, _, _ := unstructured.NestedString(m, "message")
		switch condType {
		case conditionReady:
			st.Ready = condStatus == conditionTrue
			if !st.Ready {
				st.ReadyDetail = conditionDetail("Ready", reason, message)
			}
		case conditionParamsReady:
			st.ParamsReady = condStatus == conditionTrue
			if !st.ParamsReady {
				st.ParamsReadyDetail = conditionDetail("ParamsReady", reason, message)
			}
		}
	}
	return st
}

// paramSetRefName reads spec.parametersRef.name ("" when absent or not a
// GKENetworkParamSet reference).
func paramSetRefName(n *unstructured.Unstructured) string {
	kind, _, _ := unstructured.NestedString(n.Object, "spec", "parametersRef", "kind")
	if kind != "GKENetworkParamSet" {
		return ""
	}
	name, _, _ := unstructured.NestedString(n.Object, "spec", "parametersRef", "name")
	return name
}

// conditionDetail formats a failing condition for the operator-facing message.
func conditionDetail(condType, reason, message string) string {
	switch {
	case reason != "" && message != "":
		return condType + "=" + reason + ": " + message
	case reason != "":
		return condType + "=" + reason
	default:
		return condType + " is not True"
	}
}

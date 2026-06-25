// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Mellanox v1alpha1 status-state strings emitted by the Network Operator
// for NicClusterPolicy, NicNodePolicy, HostDeviceNetwork, IPoIBNetwork,
// MacvlanNetwork. Mirrors github.com/Mellanox/network-operator
// (api/v1alpha1/nicclusterpolicy_types.go constants); duplicated here so
// the crstate package stays free of typed-API imports.
const (
	netopStateReady    = "ready"
	netopStateNotReady = "notReady"
	netopStateIgnore   = "ignore"
	netopStateError    = "error"
)

// statusStringValidator is the validator used for every Network-Operator
// v1alpha1 Kind whose readiness signal is the freeform .status.state
// string. ready/ignore→success, error→error, notReady→in-progress,
// anything else (or empty) is treated as in-progress because the
// reconciler hasn't written a verdict yet.
//
// For NicClusterPolicy and NicNodePolicy the freeform .status.state is
// also folded together with .status.appliedStates[], which carries a
// per-component breakdown ({name, state, message}). Surfacing those in
// Result.Details (and a one-line summary in Result.Reason) is what lets
// the deploy state machine's progress updates announce *which*
// component is still reconciling — operators staring at "Waiting for
// NicClusterPolicy" don't have to kubectl get nicclusterpolicy -o yaml
// just to find out the rdma-shared device plugin is the laggard.
func statusStringValidator(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	src := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())

	if err := c.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{State: StateNotDeployed, Reason: "not found in cluster", Source: src}, nil
		}
		return Result{State: StateError, Reason: fmt.Sprintf("get error: %v", err), Source: src}, err
	}

	state, _, _ := unstructured.NestedString(live.Object, "status", "state")
	reason, _, _ := unstructured.NestedString(live.Object, "status", "reason")
	componentSummary, componentDetails := summarizeAppliedStates(live)

	switch state {
	case netopStateReady, netopStateIgnore:
		msg := state
		if componentSummary != "" {
			msg = fmt.Sprintf("%s — %s", state, componentSummary)
		}
		return Result{State: StateSuccess, Reason: msg, Details: componentDetails, Source: src}, nil
	case netopStateError:
		msg := reason
		if msg == "" {
			msg = "controller reported error state"
		}
		if componentSummary != "" {
			msg = fmt.Sprintf("%s — %s", msg, componentSummary)
		}
		return Result{State: StateError, Reason: msg, Details: componentDetails, Source: src}, nil
	case netopStateNotReady, "":
		msg := componentSummary
		switch {
		case msg != "" && reason != "":
			msg = fmt.Sprintf("%s (%s)", msg, reason)
		case msg == "" && reason != "":
			msg = reason
		case msg == "":
			msg = "controller reports notReady"
		}
		return Result{State: StateInProgress, Reason: msg, Details: componentDetails, Source: src}, nil
	default:
		msg := fmt.Sprintf("unknown state %q", state)
		if componentSummary != "" {
			msg = fmt.Sprintf("%s — %s", msg, componentSummary)
		}
		return Result{State: StateInProgress, Reason: msg, Details: componentDetails, Source: src}, nil
	}
}

// summarizeAppliedStates folds .status.appliedStates[] into:
//   - a one-line "ready: N/M (a, b); pending: c; error: d (msg)" summary
//     suitable for a progress indicator update; and
//   - a per-component Details map of "<component>" → "<state>: <message>"
//     so JSON consumers (and Phase 3's report) can render the full
//     breakdown without re-fetching.
//
// Returns ("", nil) when appliedStates is absent (e.g. HostDeviceNetwork
// status has no per-component sub-states).
func summarizeAppliedStates(live *unstructured.Unstructured) (string, map[string]string) {
	raw, found, _ := unstructured.NestedSlice(live.Object, "status", "appliedStates")
	if !found || len(raw) == 0 {
		return "", nil
	}

	details := make(map[string]string, len(raw))
	var ready, pending, errored []string

	for _, item := range raw {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(entry, "name")
		state, _, _ := unstructured.NestedString(entry, "state")
		msg, _, _ := unstructured.NestedString(entry, "message")
		if name == "" {
			continue
		}
		if msg == "" {
			details[name] = state
		} else {
			details[name] = fmt.Sprintf("%s: %s", state, msg)
		}
		switch state {
		case netopStateReady, netopStateIgnore:
			ready = append(ready, name)
		case netopStateError:
			if msg != "" {
				errored = append(errored, fmt.Sprintf("%s (%s)", name, msg))
			} else {
				errored = append(errored, name)
			}
		case netopStateNotReady, "":
			pending = append(pending, name)
		default:
			// Unknown state — bucket alongside pending so operators
			// still see the component called out.
			pending = append(pending, fmt.Sprintf("%s (state=%s)", name, state))
		}
	}

	sort.Strings(ready)
	sort.Strings(pending)
	sort.Strings(errored)

	total := len(ready) + len(pending) + len(errored)
	var parts []string
	parts = append(parts, fmt.Sprintf("ready: %d/%d", len(ready), total))
	if len(pending) > 0 {
		parts = append(parts, fmt.Sprintf("pending: %s", strings.Join(pending, ", ")))
	}
	if len(errored) > 0 {
		parts = append(parts, fmt.Sprintf("error: %s", strings.Join(errored, ", ")))
	}
	if len(ready) > 0 && len(pending) == 0 && len(errored) == 0 {
		// All ready — list a few names for confirmation rather than
		// just "ready: N/N".
		parts = append(parts, "components: "+strings.Join(ready, ", "))
	}
	return strings.Join(parts, "; "), details
}

// registerStatusStringValidators registers every Network-Operator v1alpha1
// Kind whose readiness signal is .status.state.
func registerStatusStringValidators(r *Registry) {
	gv := schema.GroupVersion{Group: "mellanox.com", Version: "v1alpha1"}
	for _, kind := range []string{
		"NicClusterPolicy",
		"NicNodePolicy",
		"HostDeviceNetwork",
		"IPoIBNetwork",
		"MacvlanNetwork",
	} {
		r.Register(gv.WithKind(kind), statusStringValidator)
	}
}

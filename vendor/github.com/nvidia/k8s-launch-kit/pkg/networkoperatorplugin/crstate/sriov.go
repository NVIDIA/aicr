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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SR-IOV operator API constants (sriovnetwork.openshift.io/v1). Mirrored
// here so the crstate package does not pull the SR-IOV operator types
// into go.mod — we read the live CRs via unstructured.
const (
	sriovGroup                      = "sriovnetwork.openshift.io"
	sriovVersion                    = "v1"
	sriovKindNetworkNodePolicy      = "SriovNetworkNodePolicy"
	sriovKindNetworkNodeState       = "SriovNetworkNodeState"
	sriovSyncStatusSucceeded        = "Succeeded"
	sriovSyncStatusFailed           = "Failed"
	sriovSyncStatusInProgress       = "InProgress"
)

// sriovNetworkNodePolicyValidator classifies an SriovNetworkNodePolicy by
// aggregating the SriovNetworkNodeState of every node the policy targets,
// and cross-checks that each expected PF is present with the expected
// numVfs. This catches the silent-failure case where the SR-IOV operator
// reports Succeeded because the policy's pfNames selector matched zero
// interfaces (typically because NicInterfaceNameTemplate udev rules
// failed to apply and the rail-named PFs never appeared).
func sriovNetworkNodePolicyValidator(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
	src := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())

	// 1. Look up the live policy.
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := c.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{State: StateNotDeployed, Reason: "not found in cluster", Source: src}, nil
		}
		return Result{State: StateError, Reason: fmt.Sprintf("get error: %v", err), Source: src}, err
	}

	// 2. Determine the target nodes from spec.nodeSelector. An empty
	//    selector targets every node.
	nodeSelector, _, _ := unstructured.NestedStringMap(live.Object, "spec", "nodeSelector")
	targetNodes, err := listNodesMatchingSelector(ctx, c, nodeSelector)
	if err != nil {
		return Result{State: StateError, Reason: fmt.Sprintf("list nodes: %v", err), Source: src}, err
	}
	if len(targetNodes) == 0 {
		return Result{
			State:  StateError,
			Reason: "spec.nodeSelector matched no nodes — check labels",
			Source: src,
		}, nil
	}

	// 3. Compute expected PF set + numVfs from spec.
	expectedPFNames, _, _ := unstructured.NestedStringSlice(live.Object, "spec", "nicSelector", "pfNames")
	expectedRoot, _, _ := unstructured.NestedStringSlice(live.Object, "spec", "nicSelector", "rootDevices")
	expectedVendor, _, _ := unstructured.NestedString(live.Object, "spec", "nicSelector", "vendor")
	expectedDeviceID, _, _ := unstructured.NestedString(live.Object, "spec", "nicSelector", "deviceID")
	expectedNumVfs, _, _ := unstructured.NestedInt64(live.Object, "spec", "numVfs")

	// 4. Aggregate per-node verdicts.
	policyNS := obj.GetNamespace()
	var (
		anyError      bool
		anyInProgress bool
		details       = make(map[string]string)
	)
	for _, node := range targetNodes {
		state := &unstructured.Unstructured{}
		state.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   sriovGroup,
			Version: sriovVersion,
			Kind:    sriovKindNetworkNodeState,
		})
		stateKey := types.NamespacedName{Namespace: policyNS, Name: node}
		if err := c.Get(ctx, stateKey, state); err != nil {
			if apierrors.IsNotFound(err) {
				anyInProgress = true
				details[node] = "SriovNetworkNodeState not yet created"
				continue
			}
			return Result{State: StateError, Reason: fmt.Sprintf("get SriovNetworkNodeState/%s: %v", node, err), Source: src}, err
		}

		syncStatus, _, _ := unstructured.NestedString(state.Object, "status", "syncStatus")
		lastSyncErr, _, _ := unstructured.NestedString(state.Object, "status", "lastSyncError")

		switch syncStatus {
		case sriovSyncStatusFailed:
			anyError = true
			msg := lastSyncErr
			if msg == "" {
				msg = "syncStatus=Failed"
			}
			details[node] = msg
			continue
		case "", sriovSyncStatusInProgress:
			anyInProgress = true
			details[node] = "syncStatus=" + valueOr(syncStatus, "Unknown")
			continue
		case sriovSyncStatusSucceeded:
			// fallthrough to cross-check below
		default:
			anyInProgress = true
			details[node] = "unknown syncStatus " + syncStatus
			continue
		}

		// Succeeded — cross-check expected vs actual. crossCheckPFs
		// distinguishes hard errors (PF missing from interfaces[],
		// which won't fix itself — udev rules didn't apply, or the
		// pfNames selector is wrong) from soft "still working"
		// signals (PF present but numVfs not at target yet —
		// SriovNetworkNodeState.status.syncStatus flips to
		// "Succeeded" before the operator has finished writing
		// VF count, so this is a normal mid-reconciliation
		// observation).
		hardErr, softProgress := crossCheckPFs(state, expectedPFNames, expectedRoot, expectedVendor, expectedDeviceID, expectedNumVfs)
		switch {
		case hardErr != "":
			anyError = true
			details[node] = hardErr
		case softProgress != "":
			anyInProgress = true
			details[node] = softProgress
		default:
			details[node] = "syncStatus=Succeeded; PFs match"
		}
	}

	switch {
	case anyError:
		return Result{
			State:   StateError,
			Reason:  summarizeNodeStates(details),
			Details: details,
			Source:  src,
		}, nil
	case anyInProgress:
		return Result{
			State:   StateInProgress,
			Reason:  summarizeNodeStates(details),
			Details: details,
			Source:  src,
		}, nil
	default:
		return Result{
			State:   StateSuccess,
			Reason:  fmt.Sprintf("%d/%d nodes ready", len(details), len(targetNodes)),
			Details: details,
			Source:  src,
		}, nil
	}
}

// crossCheckPFs verifies that every expected PF the policy selects is
// present in nodeState.status.interfaces[] and has the expected
// numVfs. The check produces two buckets, returned as separate
// strings:
//
//   - hardErr     — the PF is missing from interfaces[] entirely, OR
//                   no interfaces are reported at all. The SR-IOV
//                   operator has nothing to converge on; this won't
//                   fix itself. Typical causes: NicInterfaceNameTemplate
//                   udev rules didn't apply (so the policy's pfNames
//                   never appeared), or the policy's nicSelector
//                   doesn't match anything on the node.
//
//   - softProgress — the PF is present but numVfs doesn't match yet
//                   (typically 0 vs expected=8). The operator's
//                   SriovNetworkNodeState.status.syncStatus flips to
//                   "Succeeded" before it has finished writing the VF
//                   count, so this is a normal mid-reconciliation
//                   observation, NOT a failure. The deploy loop
//                   keeps polling; the value will eventually match.
//
// Both empty means everything checks out.
func crossCheckPFs(nodeState *unstructured.Unstructured, pfNames, rootDevices []string, vendor, deviceID string, expectedNumVfs int64) (hardErr, softProgress string) {
	interfaces, found, err := unstructured.NestedSlice(nodeState.Object, "status", "interfaces")
	if err != nil || !found || len(interfaces) == 0 {
		// No interfaces yet → soft progress; operator may still be
		// enumerating. Distinct from "interfaces present but the
		// expected PF is missing" — that's a hard error.
		return "", "no interfaces reported in SriovNetworkNodeState.status yet — operator hasn't enumerated"
	}

	// Index by PCI address and pfName for lookup.
	byPCI := make(map[string]map[string]interface{}, len(interfaces))
	byName := make(map[string]map[string]interface{}, len(interfaces))
	for _, raw := range interfaces {
		iface, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if pci, _, _ := unstructured.NestedString(iface, "pciAddress"); pci != "" {
			byPCI[pci] = iface
		}
		if name, _, _ := unstructured.NestedString(iface, "name"); name != "" {
			byName[name] = iface
		}
	}

	// Resolve the expected PF set: prefer pfNames; otherwise rootDevices.
	// vendor/deviceID alone are too coarse to enumerate the expected set
	// from spec (the operator picks dynamically), so we skip the
	// existence cross-check in that case and only confirm numVfs on
	// every matched interface.
	switch {
	case len(pfNames) > 0:
		var missing []string
		var vfMismatch []string
		for _, name := range pfNames {
			iface, ok := byName[name]
			if !ok {
				missing = append(missing, name)
				continue
			}
			if msg := checkNumVfs(iface, name, expectedNumVfs); msg != "" {
				vfMismatch = append(vfMismatch, msg)
			}
		}
		if len(missing) > 0 {
			return fmt.Sprintf("policy selected pfNames=%v but missing %v on this node — check NicInterfaceNameTemplate udev rules", pfNames, missing), ""
		}
		if len(vfMismatch) > 0 {
			return "", strings.Join(vfMismatch, "; ")
		}
		return "", ""
	case len(rootDevices) > 0:
		var missing []string
		var vfMismatch []string
		for _, pci := range rootDevices {
			iface, ok := byPCI[pci]
			if !ok {
				missing = append(missing, pci)
				continue
			}
			if msg := checkNumVfs(iface, pci, expectedNumVfs); msg != "" {
				vfMismatch = append(vfMismatch, msg)
			}
		}
		if len(missing) > 0 {
			return fmt.Sprintf("policy selected rootDevices=%v but missing %v on this node", rootDevices, missing), ""
		}
		if len(vfMismatch) > 0 {
			return "", strings.Join(vfMismatch, "; ")
		}
		return "", ""
	default:
		// vendor/deviceID-only selector — operator does the dynamic
		// matching, so we can only check that some interfaces match.
		var matched int
		var vfMismatch []string
		for _, iface := range byPCI {
			if vendor != "" {
				if v, _, _ := unstructured.NestedString(iface, "vendor"); !strings.EqualFold(v, vendor) {
					continue
				}
			}
			if deviceID != "" {
				if d, _, _ := unstructured.NestedString(iface, "deviceID"); !strings.EqualFold(d, deviceID) {
					continue
				}
			}
			matched++
			name, _, _ := unstructured.NestedString(iface, "name")
			if msg := checkNumVfs(iface, valueOr(name, "<unnamed>"), expectedNumVfs); msg != "" {
				vfMismatch = append(vfMismatch, msg)
			}
		}
		if matched == 0 {
			return fmt.Sprintf("vendor=%q deviceID=%q selector matched no interfaces on this node", vendor, deviceID), ""
		}
		if len(vfMismatch) > 0 {
			return "", strings.Join(vfMismatch, "; ")
		}
		return "", ""
	}
}

// checkNumVfs returns a short message when the interface's VF count
// doesn't match the policy's spec.numVfs. The caller treats this as
// "still reconciling", not "permanently broken" — the SR-IOV operator
// publishes syncStatus=Succeeded before it has finished writing
// numVfs on every PF, so a non-matching value here is normal during
// the apply window.
func checkNumVfs(iface map[string]interface{}, label string, expected int64) string {
	if expected <= 0 {
		return ""
	}
	actual, _, _ := unstructured.NestedInt64(iface, "numVfs")
	if actual == expected {
		return ""
	}
	return fmt.Sprintf("expected numVfs=%d on %s but found %d", expected, label, actual)
}

// listNodesMatchingSelector returns the list of node names matching the
// label selector. An empty/nil selector returns every node.
func listNodesMatchingSelector(ctx context.Context, c client.Client, selector map[string]string) ([]string, error) {
	var nodes corev1.NodeList
	opts := []client.ListOption{}
	if len(selector) > 0 {
		opts = append(opts, client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set(selector))})
	}
	if err := c.List(ctx, &nodes, opts...); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names, nil
}

// summarizeNodeStates produces a short one-liner that mentions a few
// representative node states for log/error messages without printing
// every single node.
func summarizeNodeStates(details map[string]string) string {
	if len(details) == 0 {
		return "no node states observed"
	}
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	const max = 3
	var parts []string
	for i, k := range keys {
		if i >= max {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(keys)-max))
			break
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, details[k]))
	}
	return strings.Join(parts, "; ")
}

func valueOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func registerSriovValidators(r *Registry) {
	r.Register(schema.GroupVersionKind{
		Group:   sriovGroup,
		Version: sriovVersion,
		Kind:    sriovKindNetworkNodePolicy,
	}, sriovNetworkNodePolicyValidator)
}

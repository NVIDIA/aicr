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

	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nicopGroup                      = "configuration.net.nvidia.com"
	nicopVersion                    = "v1alpha1"
	nicopKindInterfaceNameTemplate  = "NicInterfaceNameTemplate"
	nicopKindConfigurationTemplate  = "NicConfigurationTemplate"
	nicopKindDevice                 = "NicDevice"
)

// templateKind identifies which NicDevice spec field a template populates.
type templateKind int

const (
	templateKindInterfaceName templateKind = iota
	templateKindConfiguration
)

// nicTemplateValidator returns a Validator closure for the given template
// kind. The validator:
//   1. Verifies the template CR exists.
//   2. Lists NicDevice CRs on the template's target nodes.
//   3. Filters to devices whose spec was populated by the operator
//      (the template-relevant spec field is non-nil).
//   4. Classifies each contributing device via its conditions[] and
//      aggregates the per-device verdicts (any error→error, any
//      in-progress→in-progress, all success→success).
func nicTemplateValidator(kind templateKind) Validator {
	return func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
		src := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())

		// 1. Look up the live template.
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
		if err := c.Get(ctx, key, live); err != nil {
			if apierrors.IsNotFound(err) {
				return Result{State: StateNotDeployed, Reason: "not found in cluster", Source: src}, nil
			}
			return Result{State: StateError, Reason: fmt.Sprintf("get error: %v", err), Source: src}, err
		}

		nodeSelector, _, _ := unstructured.NestedStringMap(live.Object, "spec", "nodeSelector")
		targetNodes, err := listNodesMatchingSelector(ctx, c, nodeSelector)
		if err != nil {
			return Result{State: StateError, Reason: fmt.Sprintf("list nodes: %v", err), Source: src}, err
		}
		if len(targetNodes) == 0 {
			return Result{State: StateError, Reason: "spec.nodeSelector matched no nodes — check labels", Source: src}, nil
		}
		targetSet := make(map[string]struct{}, len(targetNodes))
		for _, n := range targetNodes {
			targetSet[n] = struct{}{}
		}

		// 2. List all NicDevice CRs cluster-wide. NicDevice is namespaced;
		//    the operator creates them in the operator namespace
		//    alongside the template. List with no namespace constraint —
		//    we filter by node next.
		devices := &unstructured.UnstructuredList{}
		devices.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   nicopGroup,
			Version: nicopVersion,
			Kind:    nicopKindDevice + "List",
		})
		if err := c.List(ctx, devices, client.InNamespace(obj.GetNamespace())); err != nil {
			return Result{State: StateError, Reason: fmt.Sprintf("list NicDevice: %v", err), Source: src}, err
		}

		// 3. Filter to devices on target nodes whose template-relevant
		//    spec is populated. Also count devices-on-target-nodes
		//    (regardless of spec) so we can distinguish "no NicDevice
		//    CRs yet" from "CRs exist but template hasn't been
		//    applied".
		var (
			contributing    []unstructured.Unstructured
			devicesOnTarget int
		)
		for _, d := range devices.Items {
			node, _, _ := unstructured.NestedString(d.Object, "status", "node")
			if node == "" {
				continue
			}
			if _, ok := targetSet[node]; !ok {
				continue
			}
			devicesOnTarget++
			if !deviceTargetedByTemplate(&d, kind) {
				continue
			}
			contributing = append(contributing, d)
		}
		if len(contributing) == 0 {
			// nic-configuration-operator populates each NicDevice's
			// spec.{interfaceNameTemplate|configuration} when it
			// reconciles a matching template. Until that happens (or
			// when no NicDevice CRs exist on the target nodes at all)
			// we report in-progress rather than error — at deploy
			// time the operator may still be catching up. Persistent
			// misconfiguration surfaces as the deploy ultimately
			// timing out, at which point the Reason explains where
			// to look.
			specField := "spec.interfaceNameTemplate"
			if kind == templateKindConfiguration {
				specField = "spec.configuration"
			}
			var reason string
			if devicesOnTarget == 0 {
				reason = fmt.Sprintf("no NicDevice CRs on target nodes yet — waiting for nic-configuration-operator daemon to discover devices (selector: %v)", nodeSelector)
			} else {
				reason = fmt.Sprintf("found %d NicDevice(s) on target nodes; waiting for nic-configuration-operator to populate %s (check spec.nicSelector: nicType, pciAddresses, serialNumbers)", devicesOnTarget, specField)
			}
			return Result{
				State:  StateInProgress,
				Reason: reason,
				Source: src,
			}, nil
		}

		// 4. Classify each contributing device.
		var (
			anyError      bool
			anyInProgress bool
			details       = make(map[string]string)
		)
		for i := range contributing {
			d := &contributing[i]
			label := deviceLabel(d)
			state, reason := classifyDevice(d, kind)
			details[label] = reason
			switch state {
			case StateError:
				anyError = true
			case StateInProgress:
				anyInProgress = true
			case StateSuccess:
				// no-op
			case StateNotDeployed:
				// treat unset as in-progress: the device hasn't been reconciled yet
				anyInProgress = true
			}
		}

		switch {
		case anyError:
			return Result{State: StateError, Reason: summarizeNodeStates(details), Details: details, Source: src}, nil
		case anyInProgress:
			return Result{State: StateInProgress, Reason: summarizeNodeStates(details), Details: details, Source: src}, nil
		default:
			return Result{State: StateSuccess, Reason: fmt.Sprintf("%d device(s) reconciled", len(details)), Details: details, Source: src}, nil
		}
	}
}

// deviceTargetedByTemplate reports whether the given device's spec field
// (for the template kind) was populated by the operator — i.e. the
// template's nicSelector picked this device.
func deviceTargetedByTemplate(d *unstructured.Unstructured, kind templateKind) bool {
	switch kind {
	case templateKindInterfaceName:
		_, found, _ := unstructured.NestedMap(d.Object, "spec", "interfaceNameTemplate")
		return found
	case templateKindConfiguration:
		_, found, _ := unstructured.NestedMap(d.Object, "spec", "configuration")
		return found
	default:
		return false
	}
}

// deviceLabel returns "<node>/<first-pci-address>" for a NicDevice, used
// as the key in Result.Details.
func deviceLabel(d *unstructured.Unstructured) string {
	node, _, _ := unstructured.NestedString(d.Object, "status", "node")
	ports, _, _ := unstructured.NestedSlice(d.Object, "status", "ports")
	pci := ""
	if len(ports) > 0 {
		if port, ok := ports[0].(map[string]interface{}); ok {
			pci, _, _ = unstructured.NestedString(port, "pci")
		}
	}
	switch {
	case node != "" && pci != "":
		return node + "/" + pci
	case node != "":
		return node + "/" + d.GetName()
	default:
		return d.GetName()
	}
}

// classifyDevice maps the relevant condition type+status+reason to one of
// the four states. The mapping mirrors nic-configuration-operator's
// controller (internal/controller/nicdevice_controller.go).
func classifyDevice(d *unstructured.Unstructured, kind templateKind) (CRState, string) {
	conds, _, _ := unstructured.NestedSlice(d.Object, "status", "conditions")
	byType := map[string]map[string]interface{}{}
	for _, raw := range conds {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(cond, "type")
		if t != "" {
			byType[t] = cond
		}
	}

	switch kind {
	case templateKindInterfaceName:
		return classifyInterfaceName(byType)
	case templateKindConfiguration:
		return classifyConfiguration(byType)
	default:
		return StateInProgress, "unknown template kind"
	}
}

// classifyInterfaceName inspects InterfaceNameApplied. Mismatch is the
// silent-failure case we want to catch: udev rules didn't apply, so
// downstream SR-IOV selectors that key on the new name will match
// nothing.
func classifyInterfaceName(byType map[string]map[string]interface{}) (CRState, string) {
	cond, ok := byType[consts.InterfaceNameCondition]
	if !ok {
		return StateInProgress, "InterfaceNameApplied condition not yet set"
	}
	reason, _, _ := unstructured.NestedString(cond, "reason")
	status, _, _ := unstructured.NestedString(cond, "status")
	message, _, _ := unstructured.NestedString(cond, "message")

	switch {
	case reason == consts.InterfaceNameAppliedReason && status == "True":
		return StateSuccess, "InterfaceNameApplied"
	case reason == consts.InterfaceNameMismatchReason:
		return StateError, fallbackMessage(message, "interface name mismatch — udev rules did not apply")
	default:
		return StateInProgress, fallbackMessage(fmt.Sprintf("InterfaceNameApplied=%s reason=%s", status, reason), "InterfaceNameApplied unknown")
	}
}

// classifyConfiguration inspects ConfigUpdateInProgress (primary) and
// FirmwareUpdateInProgress (when present). Reason is the authoritative
// classifier — Status=False with Reason=UpdateSuccessful is the *success*
// terminal state, not failure.
func classifyConfiguration(byType map[string]map[string]interface{}) (CRState, string) {
	if cond, ok := byType[consts.ConfigUpdateInProgressCondition]; ok {
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		switch reason {
		case consts.UpdateSuccessfulReason:
			// proceed to firmware check below if present
		case consts.UpdateStartedReason, consts.PendingRebootReason:
			return StateInProgress, fallbackMessage(message, "config update in progress: "+reason)
		case consts.PendingFirmwareUpdateReason:
			return StateInProgress, fallbackMessage(message, "config update pending firmware: "+reason)
		case consts.NonVolatileConfigUpdateFailedReason,
			consts.RuntimeConfigUpdateFailedReason,
			consts.SpecValidationFailed,
			consts.IncorrectSpecReason,
			consts.FirmwareError:
			return StateError, fallbackMessage(message, "config update failed: "+reason)
		case consts.DeviceConfigSpecEmptyReason:
			// Device not targeted by any NicConfigurationTemplate —
			// callers should filter these out before classify(); if we
			// reach here, treat as success-but-empty.
			return StateSuccess, "no config requested"
		case "":
			return StateInProgress, "ConfigUpdateInProgress reason not yet set"
		default:
			return StateInProgress, fallbackMessage(message, "ConfigUpdateInProgress reason="+reason)
		}
	} else {
		return StateInProgress, "ConfigUpdateInProgress condition not yet set"
	}

	// At this point ConfigUpdateInProgress.Reason=UpdateSuccessful.
	// If firmware section is also present, gate on its terminal state.
	if cond, ok := byType[consts.FirmwareUpdateInProgressCondition]; ok {
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		switch reason {
		case consts.DeviceFwMatchReason:
			return StateSuccess, "device firmware matches and config reconciled"
		case consts.FirmwareUpdateStartedReason, consts.PendingNodeMaintenanceReason:
			return StateInProgress, fallbackMessage(message, "firmware update in progress: "+reason)
		case consts.FirmwareUpdateFailedReason,
			consts.FirmwareSourceNotReadyReason,
			consts.FirmwareSourceFailedReason,
			consts.DeviceFwMismatchReason:
			return StateError, fallbackMessage(message, "firmware update failed: "+reason)
		case consts.DeviceFirmwareSpecEmptyReason:
			// No firmware section configured — config success alone is
			// sufficient.
			return StateSuccess, "config reconciled (no firmware spec)"
		default:
			return StateInProgress, fallbackMessage(message, "FirmwareUpdateInProgress reason="+reason)
		}
	}

	return StateSuccess, "config reconciled"
}

func fallbackMessage(msg, def string) string {
	if msg == "" {
		return def
	}
	return msg
}

func registerNicConfigValidators(r *Registry) {
	gv := schema.GroupVersion{Group: nicopGroup, Version: nicopVersion}
	r.Register(gv.WithKind(nicopKindInterfaceNameTemplate), nicTemplateValidator(templateKindInterfaceName))
	r.Register(gv.WithKind(nicopKindConfigurationTemplate), nicTemplateValidator(templateKindConfiguration))
}

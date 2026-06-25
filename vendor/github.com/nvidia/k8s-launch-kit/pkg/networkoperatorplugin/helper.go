// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package networkoperatorplugin

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// waitHelperDefaultTimeout caps `WaitNicClusterPolicyReady` /
// `WaitNicNodePolicyReady` when the caller doesn't supply a deadline.
// Only applies to the helper / discovery path — the deploy state machine
// in ApplyManifestsFromDir is bounded solely by ctx.Deadline().
const waitHelperDefaultTimeout = 15 * time.Minute

// WaitNicClusterPolicyReady polls NicClusterPolicy via the crstate
// registry until it reaches StateSuccess or StateError, with a timeout.
func WaitNicClusterPolicyReady(parentCtx context.Context, c client.Client, name string) error {
	return waitViaRegistry(parentCtx, c, schema.GroupVersionKind{
		Group:   "mellanox.com",
		Version: "v1alpha1",
		Kind:    "NicClusterPolicy",
	}, "", name, "NIC Cluster Policy")
}

// WaitNicNodePolicyReady polls NicNodePolicy via the crstate registry
// until it reaches StateSuccess or StateError, with a timeout.
func WaitNicNodePolicyReady(parentCtx context.Context, c client.Client, name string) error {
	return waitViaRegistry(parentCtx, c, schema.GroupVersionKind{
		Group:   "mellanox.com",
		Version: "v1alpha1",
		Kind:    "NicNodePolicy",
	}, "", name, fmt.Sprintf("NIC Node Policy %q", name))
}

// waitViaRegistry polls the registry's per-Kind validator until it reports
// a terminal state. The function never re-applies; absent objects are
// treated as in-progress until the timeout. Use ApplyManifestsFromDir's
// state machine when you also need apply-on-not-deployed semantics.
//
// When parentCtx has no deadline, a conservative 15-minute default is
// applied — this helper is also called from the non-deploy `discover`
// path where a bounded wait keeps us from hanging forever on a broken
// cluster. Callers that want an unbounded (or maintenance-window-sized)
// budget should wrap their own ctx via context.WithTimeout before
// invoking and the helper will honor it.
func waitViaRegistry(parentCtx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name, label string) error {
	uiOutput := ui.FromContext(parentCtx)
	progress := uiOutput.StartProgress(fmt.Sprintf("Waiting for %s to become ready", label))

	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, waitHelperDefaultTimeout)
		defer cancel()
	}

	registry := crstate.NewDefault()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	ticker := time.NewTicker(deployPollInterval)
	defer ticker.Stop()

	for {
		res, err := registry.Validate(ctx, c, obj)
		if err != nil {
			log.Log.V(1).Info("validate transient failure", "kind", gvk.Kind, "name", name, "error", err.Error())
		} else {
			switch res.State {
			case crstate.StateSuccess:
				progress.Success(fmt.Sprintf("%s is ready", label))
				log.Log.Info("manifest is ready", "kind", gvk.Kind, "name", name)
				return nil
			case crstate.StateError:
				progress.Fail(fmt.Sprintf("%s error: %s", label, res.Reason))
				return fmt.Errorf("%s %q in error state: %s", gvk.Kind, name, res.Reason)
			case crstate.StateInProgress, crstate.StateNotDeployed:
				if res.Reason != "" {
					progress.Update(fmt.Sprintf("%s: %s", label, res.Reason))
				}
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail(fmt.Sprintf("Timeout waiting for %s", label))
			return fmt.Errorf("timeout waiting for %s %q to become ready", gvk.Kind, name)
		case <-ticker.C:
		}
	}
}

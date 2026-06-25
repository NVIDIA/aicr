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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	spcxGroup                = "spectrumx.nvidia.com"
	spcxVersionAlpha2        = "v1alpha2"
	spcxKindRailPoolConfig   = "SpectrumXRailPoolConfig"
	spcxSyncStatusSucceeded  = "Succeeded"
	spcxSyncStatusFailed     = "Failed"
	spcxSyncStatusInProgress = "InProgress"
	spcxSyncStatusUnknown    = "Unknown"
)

// spectrumXRailPoolConfigValidator maps .status.syncStatus to a CRState.
// v1alpha2 is the RA2.2 control surface; v1alpha1 is the glue resource
// used by the RA2.1 chain and has no useful readiness signal — it falls
// through to the default existence-only validator.
func spectrumXRailPoolConfigValidator(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
	src := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := c.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{State: StateNotDeployed, Reason: "not found in cluster", Source: src}, nil
		}
		return Result{State: StateError, Reason: fmt.Sprintf("get error: %v", err), Source: src}, err
	}

	status, _, _ := unstructured.NestedString(live.Object, "status", "syncStatus")
	reason, _, _ := unstructured.NestedString(live.Object, "status", "reason")

	switch status {
	case spcxSyncStatusSucceeded:
		return Result{State: StateSuccess, Reason: status, Source: src}, nil
	case spcxSyncStatusFailed:
		msg := reason
		if msg == "" {
			msg = "syncStatus=Failed"
		}
		return Result{State: StateError, Reason: msg, Source: src}, nil
	case "", spcxSyncStatusInProgress, spcxSyncStatusUnknown:
		msg := reason
		if msg == "" {
			msg = "syncStatus=" + valueOr(status, "Unknown")
		}
		return Result{State: StateInProgress, Reason: msg, Source: src}, nil
	default:
		return Result{State: StateInProgress, Reason: fmt.Sprintf("unknown syncStatus %q", status), Source: src}, nil
	}
}

func registerSpectrumXValidators(r *Registry) {
	r.Register(schema.GroupVersionKind{
		Group:   spcxGroup,
		Version: spcxVersionAlpha2,
		Kind:    spcxKindRailPoolConfig,
	}, spectrumXRailPoolConfigValidator)
}

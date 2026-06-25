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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultExistenceValidator returns StateSuccess when the object exists in
// the cluster, StateNotDeployed when the API server returns NotFound, and
// (non-nil error) when the Get call fails for any other reason.
//
// Used as the fallback for GVKs without a registered validator. Suitable
// for CRDs whose readiness has no observable signal beyond existence
// (e.g. NV-IPAM IPPool / CIDRPool, SriovNetwork, OVSNetwork).
func DefaultExistenceValidator(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	err := c.Get(ctx, key, live)
	src := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	switch {
	case err == nil:
		return Result{State: StateSuccess, Source: src}, nil
	case apierrors.IsNotFound(err):
		return Result{State: StateNotDeployed, Reason: "not found in cluster", Source: src}, nil
	default:
		return Result{State: StateError, Reason: fmt.Sprintf("get error: %v", err), Source: src}, err
	}
}

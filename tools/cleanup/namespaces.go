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

import (
	"context"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// notOwnedNamespaces are namespaces recipes deploy into that cleanup must
// never delete, each with the reason.
var notOwnedNamespaces = map[string]string{
	defaults.KubeSystemNamespace: "shared control-plane namespace, and deleting it breaks the cluster",
	metav1.NamespaceDefault:      "shared namespace for workloads that name none",
	metav1.NamespacePublic:       "shared namespace holding cluster bootstrap data",
	corev1.NamespaceNodeLease:    "shared namespace holding the kubelet heartbeat Leases",
}

// runtimeNamespaces are namespaces cleanup owns that no recipe names.
var runtimeNamespaces = []string{
	// Nodewright's former name. Older installs keep their release here
	// because Helm cannot move a release.
	"skyhook",
	// Created by kai-scheduler.
	"kai-resource-reservation",
	// Created by the CNCF evidence collector, without a per-run suffix.
	"gang-scheduling-test",
}

// ownedNamespaces returns the sorted namespaces any recipe in the embedded
// catalog can deploy a component into, plus runtimeNamespaces, minus
// notOwnedNamespaces.
func ownedNamespaces(ctx context.Context) ([]string, error) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "load component registry", err)
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()
	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "load recipe metadata", err)
	}

	set := map[string]bool{}
	for _, ns := range runtimeNamespaces {
		set[ns] = true
	}
	for _, c := range registry.Components {
		set[c.Helm.DefaultNamespace] = true
	}
	var refs []recipe.ComponentRef
	if store.Base != nil {
		refs = append(refs, store.Base.Spec.ComponentRefs...)
	}
	for _, overlay := range store.Overlays {
		refs = append(refs, overlay.Spec.ComponentRefs...)
	}
	for _, mixin := range store.Mixins {
		refs = append(refs, mixin.Spec.ComponentRefs...)
	}
	for _, ref := range refs {
		set[ref.Namespace] = true
	}

	delete(set, "")
	for ns := range notOwnedNamespaces {
		delete(set, ns)
	}
	return slices.Sorted(maps.Keys(set)), nil
}

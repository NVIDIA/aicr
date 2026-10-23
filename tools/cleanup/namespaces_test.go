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
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// catalogNamespaces maps every namespace the embedded catalog deploys into,
// from a registry defaultNamespace or a recipe componentRef, to where it is
// named.
func catalogNamespaces(t *testing.T) map[string]string {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor() error = %v", err)
	}

	named := map[string]string{}
	add := func(ns, source string) {
		if ns != "" && named[ns] == "" {
			named[ns] = source
		}
	}
	for _, c := range registry.Components {
		add(c.Helm.DefaultNamespace, "the defaultNamespace of "+c.Name)
	}
	addRefs := func(refs []recipe.ComponentRef, recipeName string) {
		for _, ref := range refs {
			add(ref.Namespace, "the "+ref.Name+" componentRef of "+recipeName)
		}
	}
	if store.Base != nil {
		addRefs(store.Base.Spec.ComponentRefs, "the base recipe")
	}
	for _, name := range slices.Sorted(maps.Keys(store.Overlays)) {
		addRefs(store.Overlays[name].Spec.ComponentRefs, "overlay "+name)
	}
	for _, name := range slices.Sorted(maps.Keys(store.Mixins)) {
		addRefs(store.Mixins[name].Spec.ComponentRefs, "mixin "+name)
	}
	return named
}

func TestOwnedNamespaces(t *testing.T) {
	got, err := ownedNamespaces(context.Background())
	if err != nil {
		t.Fatalf("ownedNamespaces() error = %v", err)
	}
	if !slices.IsSorted(got) {
		t.Errorf("ownedNamespaces() is not sorted: %q", got)
	}
	if slices.Contains(got, "") {
		t.Error("ownedNamespaces() contains an empty namespace")
	}
	owned := map[string]bool{}
	for _, ns := range got {
		owned[ns] = true
	}
	catalog := catalogNamespaces(t)

	t.Run("catalog namespaces are owned or excepted", func(t *testing.T) {
		for ns, source := range catalog {
			if !owned[ns] && notOwnedNamespaces[ns] == "" {
				t.Errorf("%q, %s, is not cleaned up and notOwnedNamespaces gives no reason", ns, source)
			}
		}
	})

	t.Run("owned namespaces come from the catalog or runtimeNamespaces", func(t *testing.T) {
		for _, ns := range got {
			if catalog[ns] == "" && runtimeNamespaces[ns] == "" {
				t.Errorf("%q is cleaned up, but no recipe names it and runtimeNamespaces gives no reason", ns)
			}
		}
	})

	t.Run("exceptions are reasoned and current", func(t *testing.T) {
		for ns, reason := range notOwnedNamespaces {
			if reason == "" {
				t.Errorf("notOwnedNamespaces[%q] has no reason", ns)
			}
			if owned[ns] {
				t.Errorf("not-owned namespace %q is cleaned up", ns)
			}
		}
		for ns, reason := range runtimeNamespaces {
			if reason == "" {
				t.Errorf("runtimeNamespaces[%q] has no reason", ns)
			}
			if source := catalog[ns]; source != "" {
				t.Errorf("runtimeNamespaces lists %q, which is already %s", ns, source)
			}
			if notOwnedNamespaces[ns] != "" {
				t.Errorf("%q is in both runtimeNamespaces and notOwnedNamespaces", ns)
			}
			if !owned[ns] {
				t.Errorf("runtime namespace %q is not cleaned up", ns)
			}
		}
		for ns, reason := range sharedNamespaces {
			if reason == "" {
				t.Errorf("sharedNamespaces[%q] has no reason", ns)
			}
			if catalog[ns] == "" {
				t.Errorf("sharedNamespaces lists %q, which no recipe names", ns)
			}
			if !owned[ns] {
				t.Errorf("shared namespace %q is not cleaned up", ns)
			}
		}
	})
}

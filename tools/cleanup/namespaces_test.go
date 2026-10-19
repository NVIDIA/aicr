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
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

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
	for ns := range notOwnedNamespaces {
		if slices.Contains(got, ns) {
			t.Errorf("ownedNamespaces() contains not-owned namespace %q", ns)
		}
	}
	for _, ns := range runtimeNamespaces {
		if !slices.Contains(got, ns) {
			t.Errorf("ownedNamespaces() is missing runtime namespace %q", ns)
		}
	}

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	for _, c := range registry.Components {
		ns := c.Helm.DefaultNamespace
		if ns == "" || notOwnedNamespaces[ns] != "" {
			continue
		}
		if !slices.Contains(got, ns) {
			t.Errorf("ownedNamespaces() is missing %q, the defaultNamespace of %s", ns, c.Name)
		}
	}
	for _, ns := range []string{"privileged-gpu-operator", "privileged-nodewright-operator"} {
		if !slices.Contains(got, ns) {
			t.Errorf("ownedNamespaces() is missing mixin namespace %q", ns)
		}
	}
}

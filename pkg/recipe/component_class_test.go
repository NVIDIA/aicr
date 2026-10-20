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

package recipe

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestComponentClass_Rank(t *testing.T) {
	want := []ComponentClass{ComponentClassCore, ComponentClassOps, ComponentClassWorkload}
	if !slices.Equal(ComponentClasses, want) {
		t.Fatalf("ComponentClasses = %v, want %v", ComponentClasses, want)
	}
	for i, class := range ComponentClasses {
		rank, ok := class.Rank()
		if !ok || rank != i {
			t.Errorf("%s.Rank() = (%d, %v), want (%d, true)", class, rank, ok, i)
		}
	}
	for _, class := range []ComponentClass{"", "apps", "Core"} {
		if _, ok := class.Rank(); ok {
			t.Errorf("%q.Rank() ok = true, want false", class)
		}
	}
}

func TestComponentConfig_GetClass(t *testing.T) {
	tests := []struct {
		name  string
		class ComponentClass
		want  ComponentClass
	}{
		{"unset defaults to core", "", ComponentClassCore},
		{"core", ComponentClassCore, ComponentClassCore},
		{"ops", ComponentClassOps, ComponentClassOps},
		{"workload", ComponentClassWorkload, ComponentClassWorkload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp := &ComponentConfig{Name: "x", Class: tt.class}
			if got := comp.GetClass(); got != tt.want {
				t.Errorf("GetClass() = %q, want %q", got, tt.want)
			}
		})
	}
	var nilComp *ComponentConfig
	if got := nilComp.GetClass(); got != ComponentClassCore {
		t.Errorf("nil GetClass() = %q, want core", got)
	}
}

func TestLoadComponentRegistry_Class(t *testing.T) {
	tests := []struct {
		name    string
		class   string
		want    ComponentClass
		wantErr bool
	}{
		{name: "absent", want: ComponentClassCore},
		{name: "core", class: "core", want: ComponentClassCore},
		{name: "ops", class: "ops", want: ComponentClassOps},
		{name: "workload", class: "workload", want: ComponentClassWorkload},
		{name: "unknown", class: "apps", wantErr: true},
		{name: "wrong case", class: "Ops", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := "  - name: example\n    displayName: Example\n"
			if tt.class != "" {
				entry += "    class: " + tt.class + "\n"
			}
			entry += "    healthCheck:\n      assertFile: checks/example/health-check.yaml\n" +
				"    helm:\n      defaultChart: example/chart\n      defaultVersion: 1.0.0\n"
			dp := newInMemoryProvider("registry-class-"+tt.name, map[string][]byte{
				"registry.yaml": fmt.Appendf(nil, "apiVersion: %s\nkind: %s\ncomponents:\n%s",
					ComponentRegistryAPIVersion, ComponentRegistryKind, entry),
			})
			t.Cleanup(func() { EvictCachedRegistry(dp) })

			registry, err := GetComponentRegistryFor(dp)
			if tt.wantErr {
				if err == nil {
					t.Fatal("GetComponentRegistryFor() error = nil, want class rejection")
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("GetComponentRegistryFor() error = %v", err)
			}
			if got := registry.Get("example").GetClass(); got != tt.want {
				t.Errorf("GetClass() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEmbeddedRegistryDeclaresClass pins the explicit-class rule of ADR-018:
// with a narrow core, "unset means core" would quietly misplace a component,
// so every embedded entry says which class it is in. External registries keep
// the default.
func TestEmbeddedRegistryDeclaresClass(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	for _, comp := range registry.Components {
		if comp.Class == "" {
			t.Errorf("registry component %q declares no class; every embedded entry must set class to one of %v", comp.Name, ComponentClasses)
		}
	}
}

// TestClassDependencyDirectionGuard resolves every catalog leaf and fails on
// any dependency edge from an enabled component to a later class. Because the
// classes are totally ordered, this one check also rules out class-level
// cycles. It reads the resolved graph, not the overlay YAML, so edges that
// base and mixins contribute are checked too. The direction is a catalog
// contract only: ordinary bundling never evaluates it, and --split (ADR-018)
// enforces it at bundle time.
func TestClassDependencyDirectionGuard(t *testing.T) {
	ctx := context.Background()
	store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("buildMetadataStore: %v", err)
	}
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	leaves := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		leaves++
		criteria := overlay.Spec.Criteria
		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, criteria)
			if err != nil {
				t.Fatalf("resolve %s: %v", name, err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, name) {
				t.Fatalf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
			}
			for _, ref := range result.ComponentRefs {
				if !ref.IsEnabled() {
					continue
				}
				from := registry.Get(ref.Name)
				if from == nil {
					t.Fatalf("%s: component %q has no registry entry", name, ref.Name)
				}
				fromRank, _ := from.GetClass().Rank()
				for _, depName := range ref.DependencyRefs {
					dep := result.GetComponentRef(depName)
					if dep == nil || !dep.IsEnabled() {
						// A declared-but-disabled dependency is satisfied
						// outside the recipe; a missing one fails resolution.
						continue
					}
					to := registry.Get(depName)
					if to == nil {
						t.Fatalf("%s: dependency %q has no registry entry", name, depName)
					}
					toRank, _ := to.GetClass().Rank()
					if toRank > fromRank {
						t.Errorf("%s: %s (%s) depends on %s (%s); a component may depend only on its own class or an earlier one (ADR-018)",
							name, ref.Name, from.GetClass(), depName, to.GetClass())
					}
				}
			}
		})
	}
	if leaves == 0 {
		t.Fatal("no leaf overlays found; the guard checked nothing")
	}
}

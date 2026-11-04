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
	"strings"
	"testing"
)

func TestComponentRegistry_ValidateValuePathLists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		required []string
		secret   []string
		wantErrs []string
	}{
		{"both absent", nil, nil, nil},
		{"well-formed lists", []string{"controlPlane.url", "controlPlane.token"}, []string{"controlPlane.token"}, nil},
		{"empty required entry", []string{"controlPlane.url", " "}, nil, []string{"requiredValues[1] is empty"}},
		{"repeated required entry", []string{"a.b", "a.b"}, nil, []string{"requiredValues repeats \"a.b\""}},
		{"empty secret entry", nil, []string{""}, []string{"installTimeSecretPaths[0] is empty"}},
		{"repeated secret entry", nil, []string{"x", "x"}, []string{"installTimeSecretPaths repeats \"x\""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry := &ComponentRegistry{Components: []ComponentConfig{{
				Name:                   "fixture",
				DisplayName:            "Fixture",
				RequiredValues:         tt.required,
				InstallTimeSecretPaths: tt.secret,
			}}}
			errs := registry.Validate()
			if len(errs) != len(tt.wantErrs) {
				t.Fatalf("Validate() returned %d errors, want %d: %v", len(errs), len(tt.wantErrs), errs)
			}
			for i, want := range tt.wantErrs {
				if !strings.Contains(errs[i].Error(), want) {
					t.Errorf("errs[%d] = %q, want it to contain %q", i, errs[i].Error(), want)
				}
			}
		})
	}
}

// TestRunaiClusterDeclaresRegistrationInputs pins the registry contract the
// Run:ai docs rely on: the three registration inputs are required and the
// bootstrap token is the one install-time secret among them.
func TestRunaiClusterDeclaresRegistrationInputs(t *testing.T) {
	t.Parallel()
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("runai-cluster")
	if comp == nil {
		t.Fatal("runai-cluster is not in the embedded registry")
	}
	wantRequired := []string{"controlPlane.url", "cluster.uid", "cluster.url", "controlPlane.bootstrapToken"}
	if strings.Join(comp.RequiredValues, ",") != strings.Join(wantRequired, ",") {
		t.Errorf("requiredValues = %v, want %v", comp.RequiredValues, wantRequired)
	}
	if len(comp.InstallTimeSecretPaths) != 1 || comp.InstallTimeSecretPaths[0] != "controlPlane.bootstrapToken" {
		t.Errorf("installTimeSecretPaths = %v, want [controlPlane.bootstrapToken]", comp.InstallTimeSecretPaths)
	}
	if len(comp.ValueOverrideKeys) == 0 || comp.ValueOverrideKeys[0] != "runai" {
		t.Errorf("valueOverrideKeys = %v; the first alias is what bundle-time hints print, and the docs use runai", comp.ValueOverrideKeys)
	}
}

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

package validations

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckRDMANamespaceModeCoherent(t *testing.T) {
	t.Parallel()

	const component = "network-operator"
	result := func(mode string, overrides map[string]any) *recipe.RecipeResult {
		r := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: component, Overrides: overrides}},
		}
		r.Metadata.RDMANamespaceMode = mode
		return r
	}
	rdmaCNI := func(enabled any) map[string]any {
		return map[string]any{"rdmaCni": map[string]any{"enabled": enabled}}
	}
	setEnabled := config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
		"networkoperator": {"rdmaCni.enabled": "true"},
	}))
	componentOff := config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
		"networkoperator": {"enabled": "false"},
	}))

	tests := []struct {
		name         string
		recipeResult *recipe.RecipeResult
		cfg          *config.Config
		wantWarnings int
		wantContains string
	}{
		{name: "nil recipe skipped"},
		{name: "recipe without rdmaCni skipped", recipeResult: result(recipe.RDMANamespaceModeShared, nil)},
		{name: "component disabled skipped", recipeResult: result(recipe.RDMANamespaceModeShared, rdmaCNI(true)), cfg: componentOff},
		{name: "shared with host-device alone is silent", recipeResult: result(recipe.RDMANamespaceModeShared, rdmaCNI(false))},
		{name: "exclusive with rdma CNI enabled via --set is silent", recipeResult: result(recipe.RDMANamespaceModeExclusive, rdmaCNI(false)), cfg: setEnabled},
		{
			name:         "shared with rdma CNI warns with the opt-out",
			recipeResult: result(recipe.RDMANamespaceModeShared, rdmaCNI(true)),
			wantWarnings: 1,
			wantContains: "--set network-operator:rdmaCni.enabled=false",
		},
		{
			name:         "exclusive with host-device alone warns with the opt-in",
			recipeResult: result(recipe.RDMANamespaceModeExclusive, rdmaCNI(false)),
			wantWarnings: 1,
			wantContains: "--set network-operator:rdmaCni.enabled=true",
		},
		{name: "no evidence is left to the evidence check", recipeResult: result("", rdmaCNI(false))},
		{name: "unknown evidence is left to the evidence check", recipeResult: result(recipe.RDMANamespaceModeUnknown, rdmaCNI(true))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warnings, errs := CheckRDMANamespaceModeCoherent(context.Background(), component, tt.recipeResult, tt.cfg, nil)
			if len(errs) != 0 {
				t.Fatalf("errors = %v, want none", errs)
			}
			if len(warnings) != tt.wantWarnings {
				t.Fatalf("warnings = %v, want %d", warnings, tt.wantWarnings)
			}
			if tt.wantContains != "" && !strings.Contains(warnings[0], tt.wantContains) {
				t.Errorf("warning %q does not contain %q", warnings[0], tt.wantContains)
			}
		})
	}
}

func TestCheckRDMANamespaceModeEvidence(t *testing.T) {
	t.Parallel()

	const component = "network-operator"
	result := func(mode string, overrides map[string]any) *recipe.RecipeResult {
		r := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: component, Overrides: overrides}},
		}
		r.Metadata.RDMANamespaceMode = mode
		return r
	}
	rdmaCNI := func(enabled any) map[string]any {
		return map[string]any{"rdmaCni": map[string]any{"enabled": enabled}}
	}

	tests := []struct {
		name         string
		recipeResult *recipe.RecipeResult
		wantNotes    int
		wantContains string
	}{
		{name: "nil recipe skipped"},
		{name: "recipe without rdmaCni skipped", recipeResult: result("", nil)},
		{name: "shared evidence is silent", recipeResult: result(recipe.RDMANamespaceModeShared, rdmaCNI(true))},
		{name: "exclusive evidence is silent", recipeResult: result(recipe.RDMANamespaceModeExclusive, rdmaCNI(false))},
		{
			name:         "no evidence names the mode the default attachment needs",
			recipeResult: result("", rdmaCNI(false)),
			wantNotes:    1,
			wantContains: "needs shared",
		},
		{
			name:         "no evidence names the mode the rdma CNI needs",
			recipeResult: result("", rdmaCNI(true)),
			wantNotes:    1,
			wantContains: "needs exclusive",
		},
		{
			name:         "unrecognized evidence notes it",
			recipeResult: result(recipe.RDMANamespaceModeUnknown, rdmaCNI(true)),
			wantNotes:    1,
			wantContains: "not exclusive or shared",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			notes, errs := CheckRDMANamespaceModeEvidence(context.Background(), component, tt.recipeResult, nil, nil)
			if len(errs) != 0 {
				t.Fatalf("errors = %v, want none", errs)
			}
			if len(notes) != tt.wantNotes {
				t.Fatalf("notes = %v, want %d", notes, tt.wantNotes)
			}
			if tt.wantContains != "" && !strings.Contains(notes[0], tt.wantContains) {
				t.Errorf("note %q does not contain %q", notes[0], tt.wantContains)
			}
		})
	}
}

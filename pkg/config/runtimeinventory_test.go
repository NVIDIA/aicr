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

package config

import (
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestRecipeSpecResolveRuntimeInventoryMode mirrors
// TestRecipeSpecResolveAccountingMode. The presence bool matters as much as the
// value: absence must leave the recipe's own declaration alone rather than
// materializing a default, since unlike Slurm accounting there is no meaningful
// "off by default" for a component the recipe may not declare at all.
func TestRecipeSpecResolveRuntimeInventoryMode(t *testing.T) {
	t.Parallel()

	specWith := func(mode string) *RecipeSpec {
		return &RecipeSpec{
			Configuration: &RecipeConfigurationSpec{
				RuntimeInventory: &RuntimeInventorySpec{Mode: mode},
			},
		}
	}

	tests := []struct {
		name        string
		spec        *RecipeSpec
		want        recipe.RuntimeInventoryMode
		wantPresent bool
		wantErr     bool
	}{
		{name: "nil spec"},
		{name: "absent configuration", spec: &RecipeSpec{}},
		{
			name: "configuration without the block",
			spec: &RecipeSpec{Configuration: &RecipeConfigurationSpec{}},
		},
		{
			name: "enabled", spec: specWith("enabled"),
			want: recipe.RuntimeInventoryEnabled, wantPresent: true,
		},
		{
			name: "disabled", spec: specWith("disabled"),
			want: recipe.RuntimeInventoryDisabled, wantPresent: true,
		},
		{name: "explicit empty is invalid", spec: specWith(""), wantErr: true},
		{name: "unknown value", spec: specWith("off"), wantErr: true},
		{name: "wrong case", spec: specWith("Disabled"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, present, err := tt.spec.ResolveRuntimeInventoryMode()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveRuntimeInventoryMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				// The wrapped code is what callers branch on, so assert it
				// rather than merely that some error occurred.
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
				}
				if present {
					t.Error("present = true on error; callers must not act on a rejected value")
				}
				return
			}
			if got != tt.want {
				t.Errorf("mode = %q, want %q", got, tt.want)
			}
			if present != tt.wantPresent {
				t.Errorf("present = %v, want %v", present, tt.wantPresent)
			}
		})
	}
}

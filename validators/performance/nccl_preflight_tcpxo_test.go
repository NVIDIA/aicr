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
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestGKETCPXOPreflightApplies(t *testing.T) {
	tests := []struct {
		name        string
		variant     ncclVariant
		accelerator recipe.CriteriaAcceleratorType
		service     recipe.CriteriaServiceType
		want        bool
	}{
		{
			"default + H100 + GKE → check required",
			variantDefault, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, true,
		},
		{
			"default + H100 + EKS → not required (EFA, not TCPXO)",
			variantDefault, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceEKS, false,
		},
		{
			"NET + H100 + GKE → not required (GKE has no NET variant template)",
			variantNET, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, false,
		},
		{
			"default + GB200 + GKE → not required (no GKE GB200 template)",
			variantDefault, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceGKE, false,
		},
		{
			"default + H100 + OKE → not required",
			variantDefault, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceOKE, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gkeTCPXOPreflightApplies(tt.variant, tt.accelerator, tt.service); got != tt.want {
				t.Errorf("gkeTCPXOPreflightApplies() = %v, want %v", got, tt.want)
			}
		})
	}
}

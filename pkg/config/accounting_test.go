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
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestRecipeSpecResolveAccountingMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		spec        *RecipeSpec
		want        recipe.AccountingMode
		wantPresent bool
		wantErr     bool
	}{
		{name: "nil"},
		{name: "absent", spec: &RecipeSpec{}},
		{
			name: "explicit empty",
			spec: &RecipeSpec{
				Configuration: &RecipeConfigurationSpec{
					Slurm: &SlurmConfigurationSpec{
						Accounting: &SlurmAccountingSpec{Mode: ""},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "disabled",
			spec: &RecipeSpec{
				Configuration: &RecipeConfigurationSpec{
					Slurm: &SlurmConfigurationSpec{
						Accounting: &SlurmAccountingSpec{Mode: "disabled"},
					},
				},
			},
			want:        recipe.AccountingModeDisabled,
			wantPresent: true,
		},
		{
			name: "customer managed",
			spec: &RecipeSpec{
				Configuration: &RecipeConfigurationSpec{
					Slurm: &SlurmConfigurationSpec{
						Accounting: &SlurmAccountingSpec{Mode: "customer-managed"},
					},
				},
			},
			want:        recipe.AccountingModeCustomerManaged,
			wantPresent: true,
		},
		{
			name: "AICR provided",
			spec: &RecipeSpec{
				Configuration: &RecipeConfigurationSpec{
					Slurm: &SlurmConfigurationSpec{
						Accounting: &SlurmAccountingSpec{Mode: "aicr-provided"},
					},
				},
			},
			want:        recipe.AccountingModeAICRProvided,
			wantPresent: true,
		},
		{
			name: "invalid",
			spec: &RecipeSpec{
				Configuration: &RecipeConfigurationSpec{
					Slurm: &SlurmConfigurationSpec{
						Accounting: &SlurmAccountingSpec{Mode: "managed"},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, present, err := tt.spec.ResolveAccountingMode()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveAccountingMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want || present != tt.wantPresent {
				t.Errorf("ResolveAccountingMode() = %q, %v; want %q, %v",
					got, present, tt.want, tt.wantPresent)
			}
		})
	}
}

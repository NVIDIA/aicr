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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
)

// testRecipeDynamoVersion is a synthetic dynamo-platform version that differs
// from defaults.DynamoRuntimeImageTag.
const testRecipeDynamoVersion = "0.0.0-test"

func TestResolveDynamoRuntimeTag(t *testing.T) {
	refsCtx := func(refs ...recipe.ComponentRef) *validators.Context {
		return &validators.Context{
			ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{ComponentRefs: refs}),
		}
	}
	dynamo := func(version string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: dynamoPlatformComponent, Version: version}
	}

	tests := []struct {
		name    string
		ctx     *validators.Context
		want    string
		wantErr bool
	}{
		{name: "nil context", ctx: nil, want: defaults.DynamoRuntimeImageTag},
		{name: "nil validation input", ctx: &validators.Context{}, want: defaults.DynamoRuntimeImageTag},
		{name: "no dynamo-platform ref", ctx: refsCtx(recipe.ComponentRef{Name: "gpu-operator", Version: "v26.3.0"}),
			want: defaults.DynamoRuntimeImageTag},
		{name: "ref without version", ctx: refsCtx(dynamo("  ")), want: defaults.DynamoRuntimeImageTag},
		{name: "recipe version", ctx: refsCtx(dynamo(testRecipeDynamoVersion)), want: testRecipeDynamoVersion},
		{name: "leading v", ctx: refsCtx(dynamo("v" + testRecipeDynamoVersion)), want: testRecipeDynamoVersion},
		{name: "not semver", ctx: refsCtx(dynamo("latest")), wantErr: true},
		{name: "build metadata", ctx: refsCtx(dynamo(testRecipeDynamoVersion + "+build.7")), wantErr: true},
		{name: "path injection", ctx: refsCtx(dynamo(testRecipeDynamoVersion + "/../evil")), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDynamoRuntimeTag(tt.ctx)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveDynamoRuntimeTag() = %q, want error", got)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("resolveDynamoRuntimeTag() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveDynamoRuntimeTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

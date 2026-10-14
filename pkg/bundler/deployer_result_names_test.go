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

package bundler

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestDeployerResultNames pins the display names the CLI prints: a terraform
// bundle built with --terraform-child-module is reported as a child module,
// not as the root module whose README it then contradicts.
func TestDeployerResultNames(t *testing.T) {
	tests := []struct {
		name        string
		deployer    config.DeployerType
		childModule bool
		wantResult  string
		wantDeploy  string
	}{
		{"terraform root module", config.DeployerTerraform, false, "terraform-bundle", "Terraform root module"},
		{"terraform child module", config.DeployerTerraform, true, "terraform-bundle", "Terraform child module"},
		{"child-module flag is ignored elsewhere", config.DeployerHelm, true, "helm-bundle", "Helm per-component bundle"},
		{"helmfile", config.DeployerHelmfile, false, "helmfile-bundle", "Helmfile release graph"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotResult, gotDeploy := deployerResultNames(tt.deployer, tt.childModule)
			if string(gotResult) != tt.wantResult || gotDeploy != tt.wantDeploy {
				t.Errorf("deployerResultNames(%q, %v) = (%q, %q), want (%q, %q)",
					tt.deployer, tt.childModule, gotResult, gotDeploy, tt.wantResult, tt.wantDeploy)
			}
		})
	}
}

// TestMake_TerraformReportsTheForm runs the bundler end to end for both
// terraform forms, so the child-module option is proven to reach the
// reported type rather than only the pure function above.
func TestMake_TerraformReportsTheForm(t *testing.T) {
	for _, tt := range []struct {
		name  string
		child bool
		want  string
	}{
		{"root module", false, "Terraform root module"},
		{"child module", true, "Terraform child module"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(
				config.WithDeployer(config.DeployerTerraform),
				config.WithTerraformChildModule(tt.child),
				config.WithVersion("v1.0.0"),
			)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			recipeResult := &recipe.RecipeResult{
				APIVersion: "aicr.run/v1",
				Kind:       "Recipe",
				Criteria:   &recipe.Criteria{Service: "eks", Accelerator: "h100", Intent: "training"},
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
				},
				DeploymentOrder: []string{"gpu-operator"},
			}
			output, err := b.Make(context.Background(), recipeResult, t.TempDir())
			if err != nil {
				t.Fatalf("Make() error = %v", err)
			}
			if output.Deployment == nil || output.Deployment.Type != tt.want {
				t.Fatalf("deployment type = %+v, want %q", output.Deployment, tt.want)
			}
		})
	}
}

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
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
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

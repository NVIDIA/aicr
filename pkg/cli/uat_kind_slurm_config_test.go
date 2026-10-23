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

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKindSlurmUATBundles generates the Kind Slurm UAT lane's bundle the two
// ways users are told to: from the lane's own AICRConfig, as
// demos/cuj1-slinky-slurm.md does, and from the equivalent flags. That lane is
// dispatch-only, so no PR job generates its bundle; this is what catches a
// recipe change that makes its bundle-time overrides illegal, such as a
// profile lock on a component it disables.
//
// Offline: recipe resolution and bundle generation read the embedded catalog
// and render values; no chart is fetched and no cluster is contacted.
func TestKindSlurmUATBundles(t *testing.T) {
	// Resolved from the package directory, so this reads the lane config in
	// this tree, never an installed copy.
	cfg, err := filepath.Abs(filepath.Join("..", "..", "tests", "uat", "kind", "tests", "h100-training-slurm-config.yaml"))
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}
	if _, statErr := os.Stat(cfg); statErr != nil {
		t.Fatalf("lane config missing: %v", statErr)
	}

	tests := []struct {
		name   string
		recipe []string
		bundle []string
	}{
		{
			name:   "lane config",
			recipe: []string{"recipe", "--config", cfg},
			bundle: []string{"bundle", "--config", cfg},
		},
		{
			name: "equivalent flags",
			recipe: []string{"recipe", "--service", "kind", "--accelerator", "h100",
				"--intent", "training", "--platform", "slurm", "--output", "recipe.yaml"},
			bundle: []string{"bundle", "--recipe", "recipe.yaml", "--deployer", "helmfile",
				"--set", "gpuoperator:enabled=false", "--set", "dradriver:enabled=false",
				"--output", "bundle"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The config's output paths are relative and resolve against the
			// working directory, so run where they cannot land in the tree.
			t.Chdir(t.TempDir())

			if err := newRootCmd().Run(t.Context(), append([]string{name}, tt.recipe...)); err != nil {
				t.Fatalf("aicr %v: %v", tt.recipe, err)
			}
			if err := newRootCmd().Run(t.Context(), append([]string{name}, tt.bundle...)); err != nil {
				t.Fatalf("aicr %v: %v", tt.bundle, err)
			}
			if _, err := os.Stat(filepath.Join("bundle", "helmfile.yaml")); err != nil {
				t.Fatalf("bundle did not produce the helmfile the lane deploys: %v", err)
			}
			for _, disabled := range []string{"gpu-operator", "nvidia-dra-driver-gpu"} {
				matches, globErr := filepath.Glob(filepath.Join("bundle", "*-"+disabled))
				if globErr != nil {
					t.Fatalf("glob %s: %v", disabled, globErr)
				}
				if len(matches) != 0 {
					t.Errorf("bundle carries %v; the lane disables %s", matches, disabled)
				}
			}
		})
	}
}

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
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestGPUOperatorTolerationsWithoutAcceleratedInput renders a stock leaf with
// nil accelerated tolerations and requires the gpu-operator operands to still
// tolerate nvidia.com/gpu alongside the NVSentinel release taint (#3357).
func TestGPUOperatorTolerationsWithoutAcceleratedInput(t *testing.T) {
	ctx := context.Background()
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{
		Version: "gpu-operator-toleration-test",
		Filter: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSUbuntu,
			Intent:      recipe.CriteriaIntentTraining,
		},
	})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	var rr *recipe.RecipeResult
	for _, leaf := range leaves {
		if leaf.Err == nil && leaf.Result != nil {
			rr = leaf.Result
			break
		}
	}
	if rr == nil {
		t.Fatalf("no h100 EKS ubuntu training leaf resolved among %d", len(leaves))
	}

	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerHelm),
		config.WithDeterministic(true),
		config.WithEstimatedNodeCount(3),
	)
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outputDir := t.TempDir()
	if _, err = b.Make(ctx, rr, outputDir); err != nil {
		t.Fatalf("Make: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(outputDir, "*-gpu-operator", "values.yaml"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("gpu-operator values.yaml: matches=%v err=%v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read values: %v", err)
	}
	var values struct {
		Daemonsets struct {
			Tolerations []corev1.Toleration `json:"tolerations"`
		} `json:"daemonsets"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	var gpu, release bool
	for _, tol := range values.Daemonsets.Tolerations {
		switch {
		case tol.Operator != corev1.TolerationOpExists || tol.Effect != corev1.TaintEffectNoSchedule:
		case tol.Key == "nvidia.com/gpu":
			gpu = true
		case tol.Key == "nvsentinel.dgxc.nvidia.com/external-remediation":
			release = true
		}
	}
	if !gpu || !release {
		t.Fatalf("daemonsets.tolerations = %+v; want nvidia.com/gpu and the NVSentinel release key, both Exists/NoSchedule", values.Daemonsets.Tolerations)
	}
}

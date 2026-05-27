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
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestMake_DRAChartVersionAnnotation_GeneratedArtifactParity is the
// generated-artifact acceptance test the issue calls out. It bundles
// the same recipe through two deployer code paths — the default Helm
// deployer AND the helmfile (GitOps-style) deployer — and asserts the
// bundler-derived aicr.nvidia.com/gpu-operator-chart-version
// annotation lands in the rendered nvidia-dra-driver-gpu values.yaml
// at the structurally correct `controller.podAnnotations` and
// `kubeletPlugin.podAnnotations` paths for BOTH outputs.
//
// This catches the cross-deployer parity risk for the deployers whose
// values land in a known `<order>-<name>/values.yaml` layout. Coverage
// for Flux / Argo CD / argocd-helm (which emit different file shapes,
// e.g. configmap-values.yaml or Application sources) lives in
// TestMake_DRAChartVersionAnnotation_AllDeployersCarryAnnotation
// below — that test asserts the annotation reaches *some* file in the
// rendered bundle for every supported deployer, without coupling to
// per-deployer layout conventions.
func TestMake_DRAChartVersionAnnotation_GeneratedArtifactParity(t *testing.T) {
	const (
		gpuOpVersion       = "v26.4.0"
		expectedAnnotation = "aicr.nvidia.com/gpu-operator-chart-version"
	)

	deployers := []struct {
		name     string
		deployer config.DeployerType
		// where to look for the rendered DRA values inside the bundle dir
		valuesPath string
	}{
		{
			name:       "helm-deployer",
			deployer:   config.DeployerHelm,
			valuesPath: "002-nvidia-dra-driver-gpu/values.yaml",
		},
		{
			name:       "helmfile-deployer",
			deployer:   config.DeployerHelmfile,
			valuesPath: "002-nvidia-dra-driver-gpu/values.yaml",
		},
	}

	for _, tc := range deployers {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewConfig(
				config.WithDeployer(tc.deployer),
				config.WithVersion("v1.0.0"),
			)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			rr := &recipe.RecipeResult{
				APIVersion: "aicr.nvidia.com/v1alpha1",
				Kind:       "Recipe",
				Criteria: &recipe.Criteria{
					Service:     "eks",
					Accelerator: "h100",
					Intent:      "training",
				},
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    gpuOperatorComponentName,
						Version: gpuOpVersion,
						Type:    "helm",
						Source:  "https://helm.ngc.nvidia.com/nvidia",
					},
					{
						Name:    draComponentName,
						Version: "25.12.0",
						Type:    "helm",
						Source:  "https://helm.ngc.nvidia.com/nvidia",
					},
				},
				DeploymentOrder: []string{gpuOperatorComponentName, draComponentName},
			}

			tmpDir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := b.Make(ctx, rr, tmpDir); err != nil {
				t.Fatalf("Make() error = %v", err)
			}

			valuesBytes := readBundleValues(t, tmpDir, tc.valuesPath)

			var values map[string]any
			if err := yaml.Unmarshal(valuesBytes, &values); err != nil {
				t.Fatalf("rendered values not valid YAML: %v\n%s", err, string(valuesBytes))
			}

			for _, podPath := range []string{"controller", "kubeletPlugin"} {
				got := dig(values, podPath, "podAnnotations", expectedAnnotation)
				if got != gpuOpVersion {
					t.Errorf("%s: %s.podAnnotations[%s] = %v, want %s\nfull values:\n%s",
						tc.name, podPath, expectedAnnotation, got, gpuOpVersion,
						string(valuesBytes))
				}
			}
		})
	}
}

// TestMake_DRAChartVersionAnnotation_AllDeployersCarryAnnotation
// extends parity coverage to all five supported deployers — Helm,
// helmfile, Flux, Argo CD, and argocd-helm. Each deployer renders the
// shared componentValues map differently (values.yaml under a numbered
// subdir for Helm/helmfile; configmap-values.yaml for Flux; embedded
// in Application source for the Argo CD variants), so a per-path
// YAML-structure assertion isn't feasible without coupling the test
// to each deployer's bundle layout.
//
// The weaker invariant this test pins instead is: the literal
// annotation key AND the resolved gpu-operator version must both
// appear in at least one rendered file inside the deployer's bundle.
// A future refactor that quietly drops the annotation from one
// deployer's output shape (e.g. by re-reading the recipe values file
// from disk instead of consuming the injected componentValues map)
// fails this assertion immediately. Catches the bundle-layout-drift
// case Mark called out on PR #1033.
func TestMake_DRAChartVersionAnnotation_AllDeployersCarryAnnotation(t *testing.T) {
	const (
		gpuOpVersion       = "v26.4.0"
		expectedAnnotation = "aicr.nvidia.com/gpu-operator-chart-version"
	)

	deployers := []struct {
		name     string
		deployer config.DeployerType
	}{
		{name: "helm", deployer: config.DeployerHelm},
		{name: "helmfile", deployer: config.DeployerHelmfile},
		{name: "flux", deployer: config.DeployerFlux},
		{name: "argocd", deployer: config.DeployerArgoCD},
		{name: "argocd-helm", deployer: config.DeployerArgoCDHelm},
	}

	for _, tc := range deployers {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewConfig(
				config.WithDeployer(tc.deployer),
				config.WithVersion("v1.0.0"),
			)
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			rr := &recipe.RecipeResult{
				APIVersion: "aicr.nvidia.com/v1alpha1",
				Kind:       "Recipe",
				Criteria: &recipe.Criteria{
					Service:     "eks",
					Accelerator: "h100",
					Intent:      "training",
				},
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    gpuOperatorComponentName,
						Version: gpuOpVersion,
						Type:    recipe.ComponentTypeHelm,
						Source:  "https://helm.ngc.nvidia.com/nvidia",
					},
					{
						Name:    draComponentName,
						Version: "25.12.0",
						Type:    recipe.ComponentTypeHelm,
						Source:  "https://helm.ngc.nvidia.com/nvidia",
					},
				},
				DeploymentOrder: []string{gpuOperatorComponentName, draComponentName},
			}

			tmpDir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := b.Make(ctx, rr, tmpDir); err != nil {
				t.Fatalf("Make() error = %v", err)
			}

			// Walk the bundle and find any file that contains BOTH the
			// annotation key AND the expected version. Requiring both
			// prevents false positives where the key appears in a
			// comment or doc but the value drifted.
			found := bundleContainsBoth(t, tmpDir, expectedAnnotation, gpuOpVersion)
			if !found {
				t.Errorf("%s: no rendered bundle file contains both %q and %q",
					tc.name, expectedAnnotation, gpuOpVersion)
			}
		})
	}
}

// bundleContainsBoth walks bundleDir and returns true if any regular
// file beneath it contains BOTH needle substrings. Used by the
// all-deployers parity check to keep per-deployer layout assumptions
// out of the test body.
func bundleContainsBoth(t *testing.T, bundleDir, needle1, needle2 string) bool {
	t.Helper()
	var found bool
	err := filepath.Walk(bundleDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || found {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			// Skip unreadable entries (symlinks, sockets); these aren't
			// rendered bundle artifacts.
			return nil
		}
		s := string(data)
		if strings.Contains(s, needle1) && strings.Contains(s, needle2) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", bundleDir, err)
	}
	return found
}

// TestMake_DRAChartVersionAnnotation_DisabledRecipeUnaffected pins
// the negative case in the rendered output: when nvidia-dra-driver-gpu
// is filtered out by the caller (via --set or recipe-level disable),
// the rendered bundle has no DRA values file at all — and the
// gpu-operator values are emitted without any DRA-related annotation
// leaking back into them. Bundles in a single deployer here; the
// gating logic is shared across all deployers so per-deployer
// coverage is unnecessary.
func TestMake_DRAChartVersionAnnotation_DisabledRecipeUnaffected(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rr := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    gpuOperatorComponentName,
				Version: "v26.4.0",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			// nvidia-dra-driver-gpu intentionally omitted.
		},
		DeploymentOrder: []string{gpuOperatorComponentName},
	}

	tmpDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := b.Make(ctx, rr, tmpDir); err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	// DRA directory should not exist when the component is filtered out.
	if _, statErr := os.Stat(filepath.Join(tmpDir, "002-nvidia-dra-driver-gpu")); !os.IsNotExist(statErr) {
		t.Errorf("expected no nvidia-dra-driver-gpu directory when DRA is disabled, got stat=%v", statErr)
	}

	// gpu-operator's own values.yaml must NOT carry the DRA annotation
	// — guards against an injection bug that wrote into the wrong key.
	gpuValues := readBundleValues(t, tmpDir, "001-gpu-operator/values.yaml")
	if strings.Contains(string(gpuValues), "aicr.nvidia.com/gpu-operator-chart-version") {
		t.Errorf("gpu-operator values unexpectedly contain the DRA chart-version annotation:\n%s",
			string(gpuValues))
	}
}

// readBundleValues loads a per-component values.yaml from a generated
// bundle directory and fails the test with a clear message if it
// isn't there. Centralizes the file path / read-error handling so the
// parity assertions read like declarative checks.
func readBundleValues(t *testing.T, bundleDir, relPath string) []byte {
	t.Helper()
	abs := filepath.Join(bundleDir, relPath)
	data, err := os.ReadFile(abs)
	if err != nil {
		// List the bundle directory so the failure message tells the
		// reader where the file actually landed (deployers occasionally
		// shift component subdir numbering).
		entries, _ := os.ReadDir(bundleDir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("read %s: %v\nbundle contents: %v", abs, err, names)
	}
	return data
}

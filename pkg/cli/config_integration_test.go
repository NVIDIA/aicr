// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// writeYAML writes content to a temp file in the test's temp dir and returns its path.
func writeYAML(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// TestApplyCriteriaFromConfig_OverridesSnapshot locks in the documented
// precedence: when both a snapshot and a config supply the same criteria
// field, the config wins. CLI flags subsequently override either.
func TestApplyCriteriaFromConfig_OverridesSnapshot(t *testing.T) {
	criteria := recipe.NewCriteria()
	criteria.Service = recipe.CriteriaServiceType("eks")
	criteria.Accelerator = recipe.CriteriaAcceleratorType("h100")
	criteria.Intent = recipe.CriteriaIntentType("training")
	criteria.OS = recipe.CriteriaOSType("ubuntu")

	cfg := &appcfg.AICRConfig{
		Spec: appcfg.Spec{
			Recipe: &appcfg.RecipeSpec{
				Criteria: &appcfg.CriteriaSpec{
					Service: "gke",       // overrides snapshot eks
					Intent:  "inference", // overrides snapshot training
					// accelerator + os intentionally unset; snapshot values must persist
				},
			},
		},
	}

	if err := applyCriteriaFromConfig(criteria, cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if string(criteria.Service) != "gke" {
		t.Errorf("service = %q, want gke (config overrides snapshot)", criteria.Service)
	}
	if string(criteria.Intent) != "inference" {
		t.Errorf("intent = %q, want inference (config overrides snapshot)", criteria.Intent)
	}
	if string(criteria.Accelerator) != "h100" {
		t.Errorf("accelerator = %q, want h100 (snapshot preserved when config silent)", criteria.Accelerator)
	}
	if string(criteria.OS) != "ubuntu" {
		t.Errorf("os = %q, want ubuntu (snapshot preserved when config silent)", criteria.OS)
	}
}

// TestApplyCriteriaFromConfig_FillsEmptyCriteria covers the no-snapshot path:
// the criteria starts as NewCriteria (all "any") and config populates it.
func TestApplyCriteriaFromConfig_FillsEmptyCriteria(t *testing.T) {
	criteria := recipe.NewCriteria()
	cfg := &appcfg.AICRConfig{
		Spec: appcfg.Spec{
			Recipe: &appcfg.RecipeSpec{
				Criteria: &appcfg.CriteriaSpec{
					Service: "eks",
				},
			},
		},
	}
	if err := applyCriteriaFromConfig(criteria, cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(criteria.Service) != "eks" {
		t.Errorf("service = %q, want eks", criteria.Service)
	}
}

const testRecipeConfig = `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: cfg-test
spec:
  recipe:
    criteria:
      service: eks
      accelerator: h100
      intent: training
      os: ubuntu
`

func TestRecipeCmd_ConfigFlag_AppliesCriteria(t *testing.T) {
	cfgPath := writeYAML(t, "config.yaml", testRecipeConfig)
	root := newRootCmd()

	// Use --output - to write to stdout-substitute (cmd.Writer is captured).
	err := root.Run(context.Background(), []string{
		name, "recipe", "--config", cfgPath, "-o", "-",
	})
	if err != nil {
		t.Fatalf("recipe with --config failed: %v", err)
	}
}

func TestRecipeCmd_ConfigFlag_FlagOverride(t *testing.T) {
	cfgPath := writeYAML(t, "config.yaml", testRecipeConfig)
	root := newRootCmd()

	err := root.Run(context.Background(), []string{
		name, "recipe", "--config", cfgPath, "--service", "gke", "-o", "-",
	})
	if err != nil {
		t.Fatalf("recipe with --config + flag override failed: %v", err)
	}
}

func TestRecipeCmd_ConfigFlag_InvalidConfig(t *testing.T) {
	cfgPath := writeYAML(t, "config.yaml", "kind: NotARealKind\napiVersion: v1\nspec: {recipe: {}}\n")
	root := newRootCmd()

	err := root.Run(context.Background(), []string{
		name, "recipe", "--config", cfgPath,
	})
	if err == nil {
		t.Fatal("expected error for invalid kind, got nil")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Errorf("error %q should mention 'invalid kind'", err.Error())
	}
}

func TestRecipeCmd_ConfigFlag_MissingFile(t *testing.T) {
	root := newRootCmd()
	err := root.Run(context.Background(), []string{
		name, "recipe", "--config", "/does/not/exist.yaml",
	})
	if err == nil {
		t.Fatal("expected error for missing config file, got nil")
	}
}

const testBundleConfig = `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  bundle:
    input:
      recipe: %q
    deployment:
      deployer: argocd
      repo: https://example.git/charts
      set:
        - gpuoperator:driver.version=570.86.16
    scheduling:
      systemNodeSelector:
        role: system
      acceleratedNodeTolerations:
        - "nvidia.com/gpu=present:NoSchedule"
      nodes: 4
      storageClass: gp3
    attestation:
      enabled: false
    registry:
      insecureTLS: false
      plainHTTP: false
`

// runBundleParse executes the bundle command's flag parser via a minimal
// shim Action, returning the captured *bundleCmdOptions for assertion.
func runBundleParse(t *testing.T, args []string) *bundleCmdOptions {
	t.Helper()
	var captured *bundleCmdOptions
	cmd := bundleCmd()
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		cfg, err := loadCmdConfig(ctx, c)
		if err != nil {
			return err
		}
		opts, err := parseBundleCmdOptions(c, cfg)
		if err != nil {
			return err
		}
		captured = opts
		return nil
	}
	if err := cmd.Run(context.Background(), append([]string{"bundle"}, args...)); err != nil {
		t.Fatalf("bundle.Run: %v", err)
	}
	return captured
}

func TestBundleCmd_ConfigFlag_PopulatesAllSections(t *testing.T) {
	// Write a fake recipe file so the existence check passes.
	recipePath := writeYAML(t, "recipe.yaml", "kind: Recipe\n")
	cfgPath := writeYAML(t, "config.yaml",
		strings.ReplaceAll(testBundleConfig, "%q", recipePath))

	opts := runBundleParse(t, []string{"--config", cfgPath, "-o", t.TempDir()})

	if opts.recipeFilePath != recipePath {
		t.Errorf("recipeFilePath = %q, want %q", opts.recipeFilePath, recipePath)
	}
	if got := opts.deployer.String(); got != "argocd" {
		t.Errorf("deployer = %q, want argocd", got)
	}
	if opts.repoURL != "https://example.git/charts" {
		t.Errorf("repoURL = %q", opts.repoURL)
	}
	if len(opts.valueOverrides) == 0 {
		t.Errorf("expected valueOverrides to be populated, got empty")
	}
	if v := opts.systemNodeSelector["role"]; v != "system" {
		t.Errorf("systemNodeSelector[role] = %q, want system", v)
	}
	if len(opts.acceleratedNodeTolerations) == 0 {
		t.Errorf("expected acceleratedNodeTolerations to be populated")
	}
	if opts.estimatedNodeCount != 4 {
		t.Errorf("estimatedNodeCount = %d, want 4", opts.estimatedNodeCount)
	}
	if opts.storageClass != "gp3" {
		t.Errorf("storageClass = %q, want gp3", opts.storageClass)
	}
	if opts.attest {
		t.Errorf("attest = true, want false")
	}
}

func TestBundleCmd_ConfigFlag_FlagOverridesScalar(t *testing.T) {
	recipePath := writeYAML(t, "recipe.yaml", "kind: Recipe\n")
	cfgPath := writeYAML(t, "config.yaml",
		strings.ReplaceAll(testBundleConfig, "%q", recipePath))

	opts := runBundleParse(t, []string{
		"--config", cfgPath,
		"--deployer", "helm",
		"--storage-class", "premium",
		"-o", t.TempDir(),
	})
	if got := opts.deployer.String(); got != "helm" {
		t.Errorf("deployer = %q, want helm (CLI override)", got)
	}
	if opts.storageClass != "premium" {
		t.Errorf("storageClass = %q, want premium (CLI override)", opts.storageClass)
	}
}

func TestBundleCmd_ConfigFlag_FlagReplacesSlice(t *testing.T) {
	recipePath := writeYAML(t, "recipe.yaml", "kind: Recipe\n")
	cfgPath := writeYAML(t, "config.yaml",
		strings.ReplaceAll(testBundleConfig, "%q", recipePath))

	opts := runBundleParse(t, []string{
		"--config", cfgPath,
		"--set", "alloy:clusterName=mine",
		"-o", t.TempDir(),
	})
	// CLI replaces config: only the CLI-provided override is present.
	if len(opts.valueOverrides) != 1 {
		t.Errorf("valueOverrides count = %d, want 1 (CLI replaces config slice)", len(opts.valueOverrides))
	}
}

func TestBundleCmd_ConfigFlag_RecipeFromConfig(t *testing.T) {
	recipePath := writeYAML(t, "recipe.yaml", "kind: Recipe\n")
	cfgPath := writeYAML(t, "config.yaml",
		strings.ReplaceAll(testBundleConfig, "%q", recipePath))

	// No --recipe on CLI; resolved from config.
	opts := runBundleParse(t, []string{"--config", cfgPath, "-o", t.TempDir()})
	if opts.recipeFilePath != recipePath {
		t.Errorf("recipeFilePath = %q, want %q (from config)", opts.recipeFilePath, recipePath)
	}
}

func TestBundleCmd_ConfigFlag_RecipeMissingFromConfigAndCLI(t *testing.T) {
	cfgPath := writeYAML(t, "config.yaml", `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  bundle:
    deployment:
      deployer: helm
`)
	cmd := bundleCmd()
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		cfg, err := loadCmdConfig(ctx, c)
		if err != nil {
			return err
		}
		_, err = parseBundleCmdOptions(c, cfg)
		return err
	}
	err := cmd.Run(context.Background(), []string{"bundle", "--config", cfgPath, "-o", t.TempDir()})
	if err == nil {
		t.Fatal("expected error when --recipe is missing from both CLI and config")
	}
	if !strings.Contains(err.Error(), "--recipe is required") {
		t.Errorf("error %q should mention --recipe required", err.Error())
	}
}

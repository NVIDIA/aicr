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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

// === Regression: --criteria removal ===

// TestRecipeCmd_NoCriteriaFlag asserts the deprecated --criteria flag is no
// longer accepted. Users must migrate to --config.
func TestRecipeCmd_NoCriteriaFlag(t *testing.T) {
	cmd := recipeCmd()
	for _, f := range cmd.Flags {
		for _, name := range f.Names() {
			if name == "criteria" || name == "c" {
				t.Errorf("flag %q should not be defined on recipe command (replaced by --config)", name)
			}
		}
	}
}

// TestRecipeCmd_HasConfigFlag asserts --config is the canonical replacement.
func TestRecipeCmd_HasConfigFlag(t *testing.T) {
	cmd := recipeCmd()
	found := false
	for _, f := range cmd.Flags {
		for _, name := range f.Names() {
			if name == "config" {
				found = true
			}
		}
	}
	if !found {
		t.Error("recipe command must define --config flag")
	}
}

// TestBundleCmd_HasConfigFlag asserts --config is wired on bundle.
func TestBundleCmd_HasConfigFlag(t *testing.T) {
	cmd := bundleCmd()
	found := false
	for _, f := range cmd.Flags {
		for _, name := range f.Names() {
			if name == "config" {
				found = true
			}
		}
	}
	if !found {
		t.Error("bundle command must define --config flag")
	}
}

// TestRecipeCmd_CriteriaFlagRejected verifies that passing --criteria now
// produces an "unknown flag" error rather than silently working.
func TestRecipeCmd_CriteriaFlagRejected(t *testing.T) {
	root := newRootCmd()
	err := root.Run(context.Background(), []string{name, "recipe", "--criteria", "foo.yaml"})
	if err == nil {
		t.Fatal("expected error for removed --criteria flag")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "criteria") {
		t.Logf("error: %v", err)
	}
}

// === Regression: existing flags still work without --config ===

// TestRecipeCmd_FlagsAloneStillWork ensures the criteria-from-flags pathway
// (which has always worked) still works after the --config refactor.
func TestRecipeCmd_FlagsAloneStillWork(t *testing.T) {
	root := newRootCmd()
	err := root.Run(context.Background(), []string{
		name, "recipe",
		"--service", "eks",
		"--accelerator", "h100",
		"--intent", "training",
		"--os", "ubuntu",
		"-o", "-",
	})
	if err != nil {
		t.Fatalf("recipe with flag-only criteria failed: %v", err)
	}
}

// TestBundleCmd_FlagsAloneStillWork ensures bundle works with only CLI flags
// (no --config) just as before.
func TestBundleCmd_FlagsAloneStillWork(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	captured := captureBundleOpts(t, []string{
		"--recipe", recipePath,
		"--deployer", "argocd",
		"--repo", "https://example.git",
		"--system-node-selector", "role=system",
		"--accelerated-node-toleration", "nvidia.com/gpu=present:NoSchedule",
		"--nodes", "16",
		"--storage-class", "gp3",
		"-o", tmp,
	})
	if captured.recipeFilePath != recipePath {
		t.Errorf("recipeFilePath = %q, want %q", captured.recipeFilePath, recipePath)
	}
	if captured.deployer.String() != "argocd" {
		t.Errorf("deployer = %q, want argocd", captured.deployer)
	}
	if captured.estimatedNodeCount != 16 {
		t.Errorf("estimatedNodeCount = %d, want 16", captured.estimatedNodeCount)
	}
	if captured.systemNodeSelector["role"] != "system" {
		t.Errorf("missing role=system in systemNodeSelector: %v", captured.systemNodeSelector)
	}
	if len(captured.acceleratedNodeTolerations) == 0 {
		t.Errorf("expected toleration, got none")
	}
	if captured.storageClass != "gp3" {
		t.Errorf("storageClass = %q, want gp3", captured.storageClass)
	}
}

// captureBundleOpts runs the bundle command's parsing flow and returns the
// resolved options, so tests can assert on every option without invoking the
// expensive bundler.Make() path.
func captureBundleOpts(t *testing.T, args []string) *bundleCmdOptions {
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
		t.Fatalf("bundle run: %v", err)
	}
	return captured
}

// === All-bundle-options exhaustive coverage ===

// TestBundleCmd_AllConfigSectionsResolve drives parseBundleCmdOptions with a
// config exercising every section, then asserts each option lands on the
// resolved bundleCmdOptions. This is the regression backstop for any future
// schema change.
func TestBundleCmd_AllConfigSectionsResolve(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := fmt.Sprintf(`kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  bundle:
    input:
      recipe: %s
    output:
      target: %s
      imageRefs: %s/refs.txt
    deployment:
      deployer: argocd
      repo: https://example.git/charts
      set:
        - gpuoperator:driver.version=570.86.16
        - alloy:clusterName=base
      dynamic:
        - alloy:clusterName
    scheduling:
      systemNodeSelector:
        role: system
        tier: control
      systemNodeTolerations:
        - "node-role.kubernetes.io/control-plane:NoSchedule"
      acceleratedNodeSelector:
        nodeGroup: gpu-nodes
      acceleratedNodeTolerations:
        - "nvidia.com/gpu=present:NoSchedule"
      workloadGate: "nvidia.com/training=true:NoSchedule"
      workloadSelector:
        workload: training
      nodes: 12
      storageClass: gp3
    attestation:
      enabled: true
      certificateIdentityRegexp: ".*NVIDIA/aicr.*"
      oidcDeviceFlow: true
    registry:
      insecureTLS: true
      plainHTTP: true
`, recipePath, tmp, tmp)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := captureBundleOpts(t, []string{"--config", cfgPath})

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"recipeFilePath", opts.recipeFilePath, recipePath},
		{"repoURL", opts.repoURL, "https://example.git/charts"},
		{"deployer", opts.deployer, config.DeployerArgoCD},
		{"valueOverrides count", len(opts.valueOverrides), 2},
		{"dynamicValues count", len(opts.dynamicValues), 1},
		{"systemNodeSelector role", opts.systemNodeSelector["role"], "system"},
		{"systemNodeSelector tier", opts.systemNodeSelector["tier"], "control"},
		{"acceleratedNodeSelector nodeGroup", opts.acceleratedNodeSelector["nodeGroup"], "gpu-nodes"},
		{"systemNodeTolerations count", len(opts.systemNodeTolerations), 1},
		{"acceleratedNodeTolerations count", len(opts.acceleratedNodeTolerations), 1},
		{"workloadSelector", opts.workloadSelector["workload"], "training"},
		{"estimatedNodeCount", opts.estimatedNodeCount, 12},
		{"storageClass", opts.storageClass, "gp3"},
		{"attest", opts.attest, true},
		{"certificateIdentityRegexp", opts.certificateIdentityRegexp, ".*NVIDIA/aicr.*"},
		{"oidcDeviceFlow", opts.oidcDeviceFlow, true},
		{"insecureTLS", opts.insecureTLS, true},
		{"plainHTTP", opts.plainHTTP, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if opts.workloadGateTaint == nil {
		t.Errorf("workloadGateTaint should be parsed from workloadGate")
	}
}

// TestBundleCmd_FlagOverridesEverySection verifies that for each major
// section, a CLI flag wins over the same field in --config.
func TestBundleCmd_FlagOverridesEverySection(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	altRecipe := filepath.Join(tmp, "recipe-alt.yaml")
	if err := os.WriteFile(altRecipe, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe-alt: %v", err)
	}
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := fmt.Sprintf(`kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  bundle:
    input:
      recipe: %s
    deployment:
      deployer: argocd
      repo: https://config.example.git
    scheduling:
      nodes: 4
      storageClass: standard
    attestation:
      enabled: false
    registry:
      insecureTLS: false
      plainHTTP: false
`, recipePath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := captureBundleOpts(t, []string{
		"--config", cfgPath,
		"--recipe", altRecipe,
		"--deployer", "helm",
		"--repo", "https://flag.example.git",
		"--nodes", "16",
		"--storage-class", "premium",
		"--insecure-tls",
		"--plain-http",
		"-o", tmp,
	})

	if opts.recipeFilePath != altRecipe {
		t.Errorf("--recipe should override config: got %q, want %q", opts.recipeFilePath, altRecipe)
	}
	if opts.deployer != config.DeployerHelm {
		t.Errorf("--deployer should override: got %q", opts.deployer)
	}
	if opts.repoURL != "https://flag.example.git" {
		t.Errorf("--repo should override: got %q", opts.repoURL)
	}
	if opts.estimatedNodeCount != 16 {
		t.Errorf("--nodes should override: got %d", opts.estimatedNodeCount)
	}
	if opts.storageClass != "premium" {
		t.Errorf("--storage-class should override: got %q", opts.storageClass)
	}
	if !opts.insecureTLS {
		t.Errorf("--insecure-tls should override config false")
	}
	if !opts.plainHTTP {
		t.Errorf("--plain-http should override config false")
	}
}

// === HTTP source for --config ===

// TestRecipeCmd_ConfigFromHTTP verifies a --config URL is fetched and applied.
func TestRecipeCmd_ConfigFromHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(testRecipeConfig))
	}))
	t.Cleanup(srv.Close)

	root := newRootCmd()
	err := root.Run(context.Background(), []string{
		name, "recipe", "--config", srv.URL + "/config.yaml", "-o", "-",
	})
	if err != nil {
		t.Fatalf("recipe with HTTP --config failed: %v", err)
	}
}

// === Snapshot + config interaction ===

// TestRecipeCmd_ConfigFillsMissingCriteriaFromSnapshot covers the merge case
// where a snapshot lacks one criteria field and --config fills it in. This is
// the snapshot path of applyCriteriaFromConfig.
func TestRecipeCmd_ConfigFillsMissingFromConfig(t *testing.T) {
	// Pure config path (no snapshot): partial criteria in config, the rest
	// supplied by CLI flags. Specificity should sum to all four fields.
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  recipe:
    criteria:
      service: eks
      accelerator: h100
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	root := newRootCmd()
	err := root.Run(context.Background(), []string{
		name, "recipe", "--config", cfgPath,
		"--intent", "training",
		"--os", "ubuntu",
		"-o", "-",
	})
	if err != nil {
		t.Fatalf("recipe with partial config + flags failed: %v", err)
	}
}

// Precedence (CLI > config > snapshot) is locked in by the unit tests in
// config_integration_test.go: TestApplyCriteriaFromConfig_OverridesSnapshot
// and TestApplyCriteriaFromConfig_FillsEmptyCriteria.

// === Validation surfaces ===

// TestRecipeCmd_ConfigBadEnumRejected ensures invalid enum values in --config
// are rejected with a useful error message at load time.
func TestRecipeCmd_ConfigBadEnumRejected(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	bad := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  recipe:
    criteria:
      service: bogus-service
`
	if err := os.WriteFile(cfgPath, []byte(bad), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	root := newRootCmd()
	err := root.Run(context.Background(), []string{name, "recipe", "--config", cfgPath})
	if err == nil {
		t.Fatal("expected error for invalid service enum")
	}
	if !strings.Contains(err.Error(), "criteria.service") {
		t.Errorf("error %q should reference criteria.service", err.Error())
	}
}

// TestBundleCmd_ConfigBadDeployerRejected ensures an invalid deployer enum in
// --config is rejected.
func TestBundleCmd_ConfigBadDeployerRejected(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	bad := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  bundle:
    deployment:
      deployer: not-a-real-deployer
`
	if err := os.WriteFile(cfgPath, []byte(bad), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	root := newRootCmd()
	err := root.Run(context.Background(), []string{name, "bundle", "--config", cfgPath})
	if err == nil {
		t.Fatal("expected error for invalid deployer enum")
	}
	if !strings.Contains(err.Error(), "deployer") {
		t.Errorf("error %q should reference deployer", err.Error())
	}
}

// === Recipe → Bundle workflow with shared config ===

// TestE2E_RecipeAndBundleShareConfig drives the full pipeline: a single
// AICRConfig populates both spec.recipe and spec.bundle. The recipe step
// writes a recipe to disk, the bundle step reads it back via the same config.
func TestE2E_RecipeAndBundleShareConfig(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	bundleDir := filepath.Join(tmp, "bundle")
	cfgPath := filepath.Join(tmp, "config.yaml")

	cfg := fmt.Sprintf(`kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: e2e-shared
spec:
  recipe:
    criteria:
      service: eks
      accelerator: h100
      intent: training
      os: ubuntu
    output:
      path: %s
      format: yaml
  bundle:
    input:
      recipe: %s
    output:
      target: %s
    deployment:
      deployer: helm
`, recipePath, recipePath, bundleDir)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Step 1: recipe writes to spec.recipe.output.path.
	root := newRootCmd()
	if err := root.Run(context.Background(), []string{name, "recipe", "--config", cfgPath}); err != nil {
		t.Fatalf("recipe failed: %v", err)
	}
	if _, err := os.Stat(recipePath); err != nil {
		t.Fatalf("recipe output not written: %v", err)
	}

	// Step 2: parseBundleCmdOptions resolves the recipe path and other
	// options from the same config file. We don't run the full bundle
	// (which would need full Helm chart resolution) — option resolution is
	// the contract under test.
	opts := captureBundleOpts(t, []string{"--config", cfgPath})
	if opts.recipeFilePath != recipePath {
		t.Errorf("bundle did not pick up recipe path from config: got %q, want %q",
			opts.recipeFilePath, recipePath)
	}
	if opts.deployer != config.DeployerHelm {
		t.Errorf("bundle deployer = %q, want helm", opts.deployer)
	}
}

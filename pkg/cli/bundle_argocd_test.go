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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

func derefOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// writeArgoCDConfig writes an AICRConfig whose spec.bundle.deployment holds
// deployment (indented under "deployment:") and returns its path.
func writeArgoCDConfig(t *testing.T, deployment string) string {
	t.Helper()
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	cfgPath := filepath.Join(tmp, "config.yaml")
	body := "kind: AICRConfig\napiVersion: aicr.run/v1beta1\nspec:\n  bundle:\n" +
		"    input:\n      recipe: " + recipePath + "\n" +
		"    output:\n      target: " + filepath.Join(tmp, "out") + "\n" +
		"    deployment:\n      deployer: argocd\n" + deployment
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func legacyKeys(paths []config.ComponentPath) []string {
	var keys []string
	for _, p := range paths {
		if p.Component == config.DeployerOverrideKey {
			keys = append(keys, p.Path)
		}
	}
	return keys
}

func TestParseBundleCmdOptions_ArgoCDFlags(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	out := filepath.Join(tmp, "out")
	base := []string{"--recipe", recipePath, "--output", out}

	t.Run("flags land as set fields", func(t *testing.T) {
		opts := captureBundleOpts(t, append(base, "--deployer", "argocd-helm",
			"--argocd-name-prefix", "tenant-a-",
			"--argocd-destination-server", "https://10.0.0.1:6443",
			"--argocd-project", "gpu-infra",
			"--argocd-cascade-delete=false",
			"--argocd-sync-retry-limit", "20",
			"--argocd-sync-retry-backoff-duration", "30s",
			"--argocd-sync-retry-backoff-factor", "2",
			"--argocd-sync-retry-backoff-max-duration", "5m"))
		s := opts.argoCD
		if derefOr(s.NamePrefix, "") != "tenant-a-" ||
			derefOr(s.DestinationServer, "") != "https://10.0.0.1:6443" ||
			derefOr(s.Project, "") != "gpu-infra" ||
			derefOr(s.SyncRetryLimit, 0) != 20 ||
			derefOr(s.SyncRetryBackoffDuration, "") != "30s" ||
			derefOr(s.SyncRetryBackoffFactor, 0) != 2 ||
			derefOr(s.SyncRetryBackoffMaxDuration, "") != "5m" {

			t.Errorf("argoCD = %+v, missing a flag value", s)
		}
		// An explicit false is a setting, not the absence of one.
		if s.CascadeDelete == nil || *s.CascadeDelete {
			t.Errorf("CascadeDelete = %v, want explicit false", s.CascadeDelete)
		}
	})

	t.Run("unset flags stay nil", func(t *testing.T) {
		opts := captureBundleOpts(t, append(base, "--deployer", "argocd"))
		if !opts.argoCD.IsZero() {
			t.Errorf("argoCD = %+v, want zero", opts.argoCD)
		}
	})

	for _, deployer := range []string{"helm", "flux", "fleet"} {
		t.Run("rejected on "+deployer, func(t *testing.T) {
			_, err := tryCaptureBundleOpts(t, append(base, "--deployer", deployer, "--argocd-sync-retry-limit", "3"))
			if err == nil || !strings.Contains(err.Error(), "--argocd-sync-retry-limit is only valid with --deployer argocd") {
				t.Errorf("error = %v, want the deployer restriction", err)
			}
		})
	}
}

// TestParseBundleCmdOptions_ArgoCDPrecedence pins CLI-over-config precedence
// across the two spellings. A setting given both ways within one source is
// left for the bundler to reject; see TestCreateDeployer_ArgoCDSettings in
// pkg/bundler.
func TestParseBundleCmdOptions_ArgoCDPrecedence(t *testing.T) {
	t.Run("flag completes a config backoff with its limit", func(t *testing.T) {
		cfgPath := writeArgoCDConfig(t, "      argocd:\n        syncRetry:\n          backoff:\n            duration: 30s\n")
		opts := captureBundleOpts(t, []string{"--config", cfgPath, "--argocd-sync-retry-limit", "10"})
		if derefOr(opts.argoCD.SyncRetryLimit, -1) != 10 || derefOr(opts.argoCD.SyncRetryBackoffDuration, "") != "30s" {
			t.Errorf("argoCD = %+v, want limit 10 from the flag and duration 30s from the config", opts.argoCD)
		}
		if err := opts.argoCD.Validate(); err != nil {
			t.Errorf("layered settings rejected: %v", err)
		}
	})

	t.Run("flag overrides config field", func(t *testing.T) {
		cfgPath := writeArgoCDConfig(t, "      argocd:\n        project: from-config\n        namePrefix: cfg-\n")
		opts := captureBundleOpts(t, []string{"--config", cfgPath, "--argocd-project", "from-cli"})
		if got := derefOr(opts.argoCD.Project, ""); got != "from-cli" {
			t.Errorf("Project = %q, want from-cli", got)
		}
		if got := derefOr(opts.argoCD.NamePrefix, ""); got != "cfg-" {
			t.Errorf("NamePrefix = %q, want the config value to survive", got)
		}
	})

	t.Run("flag overrides deprecated config entry", func(t *testing.T) {
		cfgPath := writeArgoCDConfig(t, "      set:\n        - deployer:project=from-config\n        - deployer:namePrefix=cfg-\n")
		opts := captureBundleOpts(t, []string{"--config", cfgPath, "--argocd-project", "from-cli"})
		if got := derefOr(opts.argoCD.Project, ""); got != "from-cli" {
			t.Errorf("Project = %q, want from-cli", got)
		}
		if got := legacyKeys(opts.valueOverrides); len(got) != 1 || got[0] != "namePrefix" {
			t.Errorf("deprecated entries = %v, want only namePrefix left", got)
		}
	})

	t.Run("deprecated --set overrides config field", func(t *testing.T) {
		cfgPath := writeArgoCDConfig(t, "      argocd:\n        project: from-config\n        namePrefix: cfg-\n")
		opts := captureBundleOpts(t, []string{"--config", cfgPath, "--set", "deployer:project=from-cli"})
		if opts.argoCD.Project != nil {
			t.Errorf("Project = %q, want the config field cleared", *opts.argoCD.Project)
		}
		if got := derefOr(opts.argoCD.NamePrefix, ""); got != "cfg-" {
			t.Errorf("NamePrefix = %q, want the config value to survive", got)
		}
		if got := legacyKeys(opts.valueOverrides); len(got) != 1 || got[0] != "project" {
			t.Errorf("deprecated entries = %v, want project", got)
		}
	})

	t.Run("both spellings on the CLI reach the bundler", func(t *testing.T) {
		cfgPath := writeArgoCDConfig(t, "")
		opts := captureBundleOpts(t, []string{"--config", cfgPath,
			"--set", "deployer:project=a", "--argocd-project", "b"})
		if derefOr(opts.argoCD.Project, "") != "b" || len(legacyKeys(opts.valueOverrides)) != 1 {
			t.Errorf("argoCD = %+v, overrides = %v; want both kept for the bundler to reject",
				opts.argoCD, opts.valueOverrides)
		}
	})
}

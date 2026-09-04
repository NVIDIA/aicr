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
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// TestResolveCNCFAllocationPolicy exercises the #1629 policy threading for
// --cncf-submission runs: no recipe context resolves to an empty policy
// (standalone runs keep the evidence script's capability detection), a
// recipe-backed run resolves the policy from the hydrated recipe, and a
// broken recipe path fails closed instead of silently collecting without a
// policy. The validate Action is overridden so only the resolution flow runs
// — never the collector (which would contact a live cluster).
func TestResolveCNCFAllocationPolicy(t *testing.T) {
	tests := []struct {
		name       string
		recipeYAML string // written to a temp recipe file when non-empty
		recipePath string // used verbatim when non-empty (overrides recipeYAML)
		wantPolicy string
		wantErr    bool
	}{
		{
			name:       "no recipe context resolves empty policy",
			wantPolicy: "",
		},
		{
			name: "recipe context resolves the hydrated policy",
			// Auto-hydrates from the embedded catalog; stock recipes default
			// to device-plugin allocation since the #1327/#1671 flip.
			recipeYAML: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n",
			wantPolicy: v1.GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			name:       "unreadable recipe fails closed",
			recipePath: filepath.Join(t.TempDir(), "does-not-exist.yaml"),
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"validate", "--no-cluster"}
			recipePath := tt.recipePath
			if tt.recipeYAML != "" {
				recipePath = filepath.Join(t.TempDir(), "recipe.yaml")
				if err := os.WriteFile(recipePath, []byte(tt.recipeYAML), 0o600); err != nil {
					t.Fatalf("failed to write test recipe file: %v", err)
				}
			}
			if recipePath != "" {
				args = append(args, "--recipe", recipePath)
			}

			var gotPolicy string
			cmd := validateCmd()
			cmd.Action = func(ctx context.Context, c *cli.Command) error {
				cfg, err := loadFacadeConfig(ctx, c)
				if err != nil {
					return err
				}
				resolved, err := cfg.Unwrap().Validation().Resolve()
				if err != nil {
					return err
				}
				recipeFilePath := stringFlagOrConfig(c, "recipe", resolved.RecipePath)
				gotPolicy, err = resolveCNCFAllocationPolicy(ctx, c, cfg, recipeFilePath)
				return err
			}
			err := cmd.Run(t.Context(), args)

			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && gotPolicy != tt.wantPolicy {
				t.Errorf("policy = %q, want %q", gotPolicy, tt.wantPolicy)
			}
		})
	}
}

func TestValidateCmd_CommandStructure(t *testing.T) {
	cmd := validateCmd()

	if cmd.Name != "validate" {
		t.Errorf("command name = %q, want %q", cmd.Name, "validate")
	}

	requiredFlags := []string{"recipe", "phase", "namespace", "node-selector", "toleration", "timeout"}
	for _, flagName := range requiredFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing required flag: %s", flagName)
		}
	}
}

func TestValidateCmd_AgentFlags(t *testing.T) {
	cmd := validateCmd()

	agentFlags := []string{
		"namespace",
		"image",
		"image-pull-secret",
		"job-name",
		"service-account-name",
		"node-selector",
		"toleration",
		"timeout",
		"no-cleanup",
	}

	for _, flagName := range agentFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing agent flag: %s", flagName)
		}
	}
}

func hasFlag(flag interface{ Names() []string }, name string) bool {
	return slices.Contains(flag.Names(), name)
}

func TestValidateCmd_CNCFSubmissionFlags(t *testing.T) {
	cmd := validateCmd()

	// Verify --cncf-submission and --feature flags exist
	evidenceFlags := []string{"cncf-submission", "feature", "evidence-dir"}
	for _, flagName := range evidenceFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing evidence flag: %s", flagName)
		}
	}

	// Verify --feature has -f alias
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "feature") && !hasFlag(flag, "f") {
			t.Error("--feature flag missing -f alias")
		}
	}
}

func TestValidateCmd_CNCFSubmissionFlagValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		errContain string
	}{
		{
			name:       "cncf-submission without evidence-dir",
			args:       []string{"aicr", "validate", "--cncf-submission"},
			wantErr:    true,
			errContain: "--cncf-submission requires --evidence-dir",
		},
		{
			name:       "feature without cncf-submission",
			args:       []string{"aicr", "validate", "--feature", "dra", "--evidence-dir", "/tmp/test"},
			wantErr:    true,
			errContain: "--feature requires --cncf-submission",
		},
		{
			name:       "cncf-submission with invalid feature",
			args:       []string{"aicr", "validate", "--cncf-submission", "--evidence-dir", "/tmp/test", "--feature", "nonexistent"},
			wantErr:    true,
			errContain: "unknown feature",
		},
		{
			name:       "cncf-submission with no-cluster",
			args:       []string{"aicr", "validate", "--cncf-submission", "--evidence-dir", "/tmp/test", "--no-cluster"},
			wantErr:    true,
			errContain: "--cncf-submission cannot be combined with --no-cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validateCmd()
			// Wrap in a parent app so flag parsing works correctly.
			app := &cli.Command{
				Name:     "aicr",
				Commands: []*cli.Command{cmd},
			}
			err := app.Run(t.Context(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				// Every flag-combination rejection in this command is a
				// well-formed-but-invalid request, so assert the structured
				// code (not just the message text) — a wrong code would map to
				// the wrong HTTP status / exit behavior for library callers.
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
				}
				if tt.errContain != "" && (err == nil || !strings.Contains(err.Error(), tt.errContain)) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
		})
	}
}

// TestValidateCmd_NoClusterEvidenceFlags asserts that explicitly requesting an
// attestation (--emit-attestation / --push) alongside the offline --no-cluster
// dry-run is rejected with ErrCodeInvalidRequest, rather than warn-and-ignored.
// (Config-driven suppression is a separate path — see evidenceConfigForRunMode.)
func TestValidateCmd_NoClusterEvidenceFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "emit-attestation with no-cluster",
			args: []string{"aicr", "validate", "--no-cluster", "--emit-attestation", "/tmp/out"},
		},
		{
			name: "push with no-cluster",
			args: []string{"aicr", "validate", "--no-cluster", "--emit-attestation", "/tmp/out", "--push", "ghcr.io/x/y"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validateCmd()
			app := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd}}
			err := app.Run(t.Context(), tt.args)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), "cannot be combined with --no-cluster") {
				t.Errorf("error = %v, want a combined-with-no-cluster message", err)
			}
		})
	}
}

func TestValidateCmdFlags_FailFastDefault(t *testing.T) {
	cmd := validateCmd()
	for _, f := range cmd.Flags {
		if !hasFlag(f, "fail-fast") {
			continue
		}
		bf, ok := f.(*cli.BoolFlag)
		if !ok {
			t.Fatal("--fail-fast should be a *cli.BoolFlag")
		}
		if bf.Value {
			t.Error("--fail-fast default should be false")
		}
		return
	}
	t.Error("--fail-fast flag not found in validateCmd flags")
}

func TestValidateCmd_RecipeKindHandling(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		wantErr     bool
		errContain  string
		errAbsent   string
	}{
		{
			name:        "RecipeMetadata without criteria returns clear error",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  "has no criteria",
		},
		{
			name:        "RecipeMetadata with criteria auto-hydrates",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "has no criteria",
		},
		{
			name:        "RecipeMixin kind is rejected",
			yamlContent: "kind: RecipeMixin\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  `kind "RecipeMixin"`,
		},
		{
			name:        "unknown kind is rejected",
			yamlContent: "kind: SomethingElse\napiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  `kind "SomethingElse"`,
		},
		{
			name:        "RecipeResult kind passes kind check",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "is required",
		},
		{
			name:        "empty kind passes kind check",
			yamlContent: "apiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "is required",
		},
		{
			name:        "legacy apiVersion is rejected",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\n",
			wantErr:     true,
			errContain:  "apiVersion",
			errAbsent:   "--no-cluster requires --snapshot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			recipeFile := filepath.Join(dir, "recipe.yaml")
			if err := os.WriteFile(recipeFile, []byte(tt.yamlContent), 0o600); err != nil {
				t.Fatalf("failed to write test recipe file: %v", err)
			}

			cmd := validateCmd()
			app := &cli.Command{
				Name:     "aicr",
				Commands: []*cli.Command{cmd},
			}
			err := app.Run(t.Context(), []string{"aicr", "validate", "--recipe", recipeFile, "--no-cluster"})

			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
				return
			}
			if tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
			if tt.errAbsent != "" && err != nil {
				if strings.Contains(err.Error(), tt.errAbsent) {
					t.Errorf("error = %v, should NOT contain %q", err, tt.errAbsent)
				}
			}
		})
	}
}

// TestValidateCmd_KubeconfigSelectsValidationCluster is the #1787 regression
// test: an explicit --kubeconfig must select the cluster for the validator
// engine itself, not only for cm:// artifact I/O. With local recipe/snapshot
// files and a nonexistent --kubeconfig path, the run must fail fast on the
// explicit path — before the fix, the engine silently fell back to default
// discovery (KUBECONFIG env → ~/.kube/config → in-cluster), so the error
// (if any) never mentioned the flag path. KUBECONFIG is pinned to a
// nonexistent temp path so neither branch can ever reach a real cluster.
//
// A hydrated RecipeMetadata would fail readiness checks against this minimal
// snapshot before client construction. This criteria-less RecipeResult keeps
// readiness a no-op while its inline gpu-operator override supplies the
// whole-GPU advertiser required by validation-input construction.
func TestValidateCmd_KubeconfigSelectsValidationCluster(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(tmp, "env-default.kubeconfig"))

	recipePath := filepath.Join(tmp, "recipe.yaml")
	recipeYAML := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\nmetadata:\n  version: test\ncomponentRefs:\n  - name: gpu-operator\n    type: Helm\n    source: https://helm.ngc.nvidia.com/nvidia\n    version: v25.10.0\n    overrides:\n      devicePlugin:\n        enabled: true\n"
	if err := os.WriteFile(recipePath, []byte(recipeYAML), 0o600); err != nil {
		t.Fatalf("failed to write test recipe file: %v", err)
	}
	snapshotPath := filepath.Join(tmp, "snapshot.yaml")
	if err := os.WriteFile(snapshotPath, []byte("kind: Snapshot\nmetadata:\n  version: test\nmeasurements:\n  - type: K8s\n"), 0o600); err != nil {
		t.Fatalf("failed to write test snapshot file: %v", err)
	}

	flagPath := filepath.Join(tmp, "explicit-flag.kubeconfig") // intentionally not created

	cmd := validateCmd()
	err := cmd.Run(t.Context(), []string{"validate",
		"--recipe", recipePath,
		"--snapshot", snapshotPath,
		"--kubeconfig", flagPath,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent --kubeconfig, got nil")
	}
	if !strings.Contains(err.Error(), flagPath) {
		t.Errorf("error must fail on the explicit --kubeconfig path %q, got:\n%v", flagPath, err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
	}
}

// TestDeployAgentForValidation_ExplicitKubeconfigFailsFast covers the #1787
// live-snapshot path: the preliminary namespace creation must use the same
// explicit kubeconfig the snapshot agent deploys with — not the process
// default. A nonexistent explicit path must fail before any cluster contact.
// KUBECONFIG is pinned to a nonexistent temp path so the pre-fix fallback to
// default discovery can never reach a real cluster either.
func TestDeployAgentForValidation_ExplicitKubeconfigFailsFast(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(tmp, "env-default.kubeconfig"))

	flagPath := filepath.Join(tmp, "explicit-flag.kubeconfig") // intentionally not created
	cfg := &validateAgentConfig{
		kubeconfig: flagPath,
		namespace:  "aicr-validation-test",
		jobName:    "aicr-validate-test",
	}

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = deployAgentForValidation(t.Context(), client, cfg)
	if err == nil {
		t.Fatal("expected error for nonexistent explicit kubeconfig, got nil")
	}
	if !strings.Contains(err.Error(), flagPath) {
		t.Errorf("error must fail on the explicit kubeconfig path %q, got:\n%v", flagPath, err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
	}
}

// TestValidateAgentConfig_ToAgentConfig_ForwardsRunID covers ONLY the
// toAgentConfig projection boundary: given a validateAgentConfig whose
// runID field is already set, toAgentConfig must copy it onto the facade
// AgentConfig.RunID unchanged (and set NameBase to validateNameBase rather
// than leaving JobName/ServiceAccountName to carry the naming prefix).
//
// What this does NOT cover: it never runs the validateCmd Action, so it
// says nothing about whether `aicr validate` generates exactly one RunID
// per invocation and hands that SAME value to both the live-capture
// snapshot agent and the validator Jobs — that single-generation invariant
// (ADR-020 Ruling 7) is not exercised by an automated test at all; see the
// WARNING comment on the Action's `runID := v1.GenerateRunID()` call in
// validate.go for why, and TestParseValidateAgentConfig_ForwardsCallerRunID
// below for the adjacent (also boundary-only) coverage on the parsing side.
func TestValidateAgentConfig_ToAgentConfig_ForwardsRunID(t *testing.T) {
	cfg := &validateAgentConfig{
		namespace: "aicr-validation-test",
		runID:     "20260821-142233-9f3a1c0b7e2d4a55",
	}

	ac := cfg.toAgentConfig()

	if ac.RunID != cfg.runID {
		t.Errorf("AgentConfig.RunID = %q, want %q (validateAgentConfig.runID)", ac.RunID, cfg.runID)
	}
	if ac.NameBase != validateNameBase {
		t.Errorf("AgentConfig.NameBase = %q, want %q", ac.NameBase, validateNameBase)
	}
}

// TestParseValidateAgentConfig_ForwardsCallerRunID covers ONLY
// parseValidateAgentConfig's own mapping: the runID parameter it is given
// lands unchanged on the returned validateAgentConfig.runID field.
//
// This test replaces cmd.Action outright (to isolate that mapping from
// recipe/snapshot I/O without touching a cluster), so NONE of the
// production Action code at validate.go's `runID := v1.GenerateRunID()`
// call through the parseValidateAgentConfig call site actually runs here.
// It cannot detect a regression in how the real Action generates or
// threads runID — in particular it says nothing about ADR-020 Ruling 7's
// single-generation invariant (one v1.GenerateRunID() call feeding BOTH
// the live-capture agent and the validator Jobs). See the WARNING comment
// on that call site in validate.go: no automated test in this package
// enforces single-generation. The two consumer sites are NOT mutually
// exclusive — with neither --snapshot nor --no-cluster, the live-capture
// branch and runValidation both consume the id in one invocation. What
// blocks the test is that exercising both requires live-cluster I/O (both
// branches deploy Jobs) with no injectable seam in the Action to fake it.
func TestParseValidateAgentConfig_ForwardsCallerRunID(t *testing.T) {
	const wantRunID = "20260821-142233-9f3a1c0b7e2d4a55"

	var captured *validateAgentConfig
	cmd := validateCmd()
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		cfg, err := loadFacadeConfig(ctx, c)
		if err != nil {
			return err
		}
		opts, _, err := cfg.ValidateSettings()
		if err != nil {
			return err
		}
		shared := validateSharedResolved{namespace: "aicr-validation-test"}
		captured = parseValidateAgentConfig(c, opts, shared, wantRunID)
		return nil
	}
	if err := cmd.Run(t.Context(), []string{"validate", "--no-cluster"}); err != nil {
		t.Fatalf("validate run: %v", err)
	}

	if captured.runID != wantRunID {
		t.Errorf("validateAgentConfig.runID = %q, want %q", captured.runID, wantRunID)
	}
}

// TestValidateCmd_NoConfigDefaultsToCleanup is the regression test for the
// fix-round-2 security defect: a plain `aicr validate` — no --config, no
// --no-cleanup, which is the common invocation — must clean up by default.
//
// Before the fix, ValidateSettings() returned only (ValidateSettings, error),
// and its zero value (Cleanup: false, returned for both a nil Config and an
// absent spec.validate) was trusted directly as the fallback for
// --no-cleanup. That silently flipped the CLI's own default: a plain
// invocation left the cluster-admin ClusterRoleBinding and validator Jobs
// active. The Action is overridden here to isolate exactly the computation
// that broke — loadFacadeConfig, ValidateSettings' presence bool,
// validateCleanupFallback, then the same boolFlagOrConfig call the
// production Action makes — without needing a live cluster to reach it.
func TestValidateCmd_NoConfigDefaultsToCleanup(t *testing.T) {
	var gotNoCleanup bool
	cmd := validateCmd()
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		cfg, err := loadFacadeConfig(ctx, c)
		if err != nil {
			return err
		}
		opts, present, err := cfg.ValidateSettings()
		if err != nil {
			return err
		}
		cleanupFallback := validateCleanupFallback(opts, present)
		gotNoCleanup = boolFlagOrConfig(c, "no-cleanup", !cleanupFallback)
		return nil
	}
	if err := cmd.Run(t.Context(), []string{"validate"}); err != nil {
		t.Fatalf("validate run: %v", err)
	}

	if gotNoCleanup {
		t.Error("noCleanup = true, want false: a plain `aicr validate` " +
			"(no --config, no --no-cleanup) must clean up by default")
	}
}

// TestValidateCleanupFallback drives validateCleanupFallback directly for
// both branches. present=false is also exercised indirectly by
// TestValidateCmd_NoConfigDefaultsToCleanup through a full command run; the
// two present=true rows here are not exercised by any other test — present's
// whole point is to gate ONLY when spec.validate was actually evaluated, so
// a present=true document that decided Cleanup=false must flow through
// unmodified rather than being silently overridden by the CLI's own
// clean-up-by-default.
func TestValidateCleanupFallback(t *testing.T) {
	tests := []struct {
		name    string
		opts    aicr.ValidateSettings
		present bool
		want    bool
	}{
		{"absent spec.validate defaults to cleanup", aicr.ValidateSettings{Cleanup: false}, false, true},
		{"present spec.validate with cleanup true", aicr.ValidateSettings{Cleanup: true}, true, true},
		{"present spec.validate with cleanup false", aicr.ValidateSettings{Cleanup: false}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validateCleanupFallback(tt.opts, tt.present); got != tt.want {
				t.Errorf("validateCleanupFallback(%+v, present=%t) = %t, want %t",
					tt.opts, tt.present, got, tt.want)
			}
		})
	}
}

// TestValidateCmd_DeclaredNameDefaultsNeverReachTheAgent is the validate-side
// half of TestSnapshotCmd_DeclaredNameDefaultsNeverReachTheDeployer; see that
// test for why an unset --service-account-name must not resolve to a name aicr
// chose. The released v1 defaults here are "aicr-validate" for --job-name and
// "aicr" for --service-account-name, and both stay declared for `--help` and
// testdata/cli-surface.golden while neither is delivered as a value.
//
// The prefix the agent's objects actually get comes from
// AgentConfig.NameBase (validateNameBase) instead, which is what keeps this
// command's agent resources distinguishable from `aicr snapshot`'s.
func TestValidateCmd_DeclaredNameDefaultsNeverReachTheAgent(t *testing.T) {
	cmd := validateCmd()
	if got := stringFlagValue(t, cmd, "job-name"); got != validateNameBase {
		t.Errorf("--job-name declared default = %q, want the released %q",
			got, validateNameBase)
	}
	if got := stringFlagValue(t, cmd, "service-account-name"); got != name {
		t.Errorf("--service-account-name declared default = %q, want the released %q",
			got, name)
	}

	capture := func(t *testing.T, args ...string) *validateAgentConfig {
		t.Helper()
		var captured *validateAgentConfig
		c := validateCmd()
		c.Action = func(ctx context.Context, cc *cli.Command) error {
			cfg, err := loadFacadeConfig(ctx, cc)
			if err != nil {
				return err
			}
			opts, _, err := cfg.ValidateSettings()
			if err != nil {
				return err
			}
			shared := validateSharedResolved{namespace: "aicr-validation-test"}
			captured = parseValidateAgentConfig(cc, opts, shared, "20260821-142233-9f3a1c0b7e2d4a55")
			return nil
		}
		if err := c.Run(t.Context(), append([]string{"validate", "--no-cluster"}, args...)); err != nil {
			t.Fatalf("validate run: %v", err)
		}
		return captured
	}

	unset := capture(t)
	if unset.serviceAccountName != "" {
		t.Errorf("serviceAccountName = %q for an unset flag, want empty; a "+
			"non-empty value is probed for existence and a hit disables RBAC "+
			"management for the whole run", unset.serviceAccountName)
	}
	if unset.jobName != "" {
		t.Errorf("jobName = %q for an unset flag, want empty", unset.jobName)
	}
	if base := unset.toAgentConfig().NameBase; base != validateNameBase {
		t.Errorf("AgentConfig.NameBase = %q, want %q — it is what supplies the "+
			"prefix once the declared defaults stop being delivered",
			base, validateNameBase)
	}

	// Guard against the vacuous pass: an operator-supplied name must survive,
	// since that is the only route into exact-ServiceAccount mode.
	explicit := capture(t, "--job-name", "validate-gpu-nodes",
		"--service-account-name", "irsa-snapshotter")
	if explicit.serviceAccountName != "irsa-snapshotter" {
		t.Errorf("serviceAccountName = %q, want the operator's %q",
			explicit.serviceAccountName, "irsa-snapshotter")
	}
	if explicit.jobName != "validate-gpu-nodes" {
		t.Errorf("jobName = %q, want the operator's %q",
			explicit.jobName, "validate-gpu-nodes")
	}
}

// TestClassifyIgnoredAKSGPUPools pins the provenance matrix of the
// ignored-projection note: explicit CLI presence (either flag form) always
// warns; a purely ambient env source is demoted to debug; nothing logs
// unless both a pre-captured snapshot and a pools path are in play.
func TestClassifyIgnoredAKSGPUPools(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		envValue      string
		poolsPath     string
		snapshotPath  string
		flagName      string // empty means aks-gpu-pools
		wantShouldLog bool
		wantAtWarn    bool
	}{
		{
			name:         "no snapshot: silent",
			args:         []string{"--aks-gpu-pools", "p.json"},
			poolsPath:    "p.json",
			snapshotPath: "",
		},
		{
			name:         "no pools path: silent",
			snapshotPath: "snap.yaml",
		},
		{
			name:          "explicit flag (space form): warn",
			args:          []string{"validate", "--aks-gpu-pools", "p.json", "--snapshot", "snap.yaml"},
			poolsPath:     "p.json",
			snapshotPath:  "snap.yaml",
			wantShouldLog: true,
			wantAtWarn:    true,
		},
		{
			name:          "explicit flag (= form): warn",
			args:          []string{"validate", "--aks-gpu-pools=p.json", "-s", "snap.yaml"},
			poolsPath:     "p.json",
			snapshotPath:  "snap.yaml",
			wantShouldLog: true,
			wantAtWarn:    true,
		},
		{
			name:          "ambient env only: debug",
			args:          []string{"validate", "-s", "snap.yaml"},
			envValue:      "p.json",
			poolsPath:     "p.json",
			snapshotPath:  "snap.yaml",
			wantShouldLog: true,
			wantAtWarn:    false,
		},
		{
			name:          "explicit flag beats identical env: warn",
			args:          []string{"validate", "--aks-gpu-pools", "p.json", "-s", "snap.yaml"},
			envValue:      "p.json",
			poolsPath:     "p.json",
			snapshotPath:  "snap.yaml",
			wantShouldLog: true,
			wantAtWarn:    true,
		},
		{
			name:          "oke-addons explicit flag: warn",
			args:          []string{"validate", "--oke-addons", "a.json", "-s", "snap.yaml"},
			poolsPath:     "a.json",
			snapshotPath:  "snap.yaml",
			flagName:      "oke-addons",
			wantShouldLog: true,
			wantAtWarn:    true,
		},
		{
			name:          "oke-addons ambient env only (AICR_OKE_ADDONS_PATH): debug",
			args:          []string{"validate", "-s", "snap.yaml"},
			envValue:      "a.json",
			poolsPath:     "a.json",
			snapshotPath:  "snap.yaml",
			flagName:      "oke-addons",
			wantShouldLog: true,
			wantAtWarn:    false,
		},
		{
			name: "similarly-prefixed flag is not a false positive",
			args: []string{"validate", "--aks-gpu-pools-extra", "x", "-s", "snap.yaml"},
			// pools path resolvable only via env in this shape.
			envValue:      "p.json",
			poolsPath:     "p.json",
			snapshotPath:  "snap.yaml",
			wantShouldLog: true,
			wantAtWarn:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flagName := tt.flagName
			if flagName == "" {
				flagName = "aks-gpu-pools"
			}
			shouldLog, atWarn := classifyIgnoredProjection(tt.args, tt.envValue, tt.poolsPath, tt.snapshotPath, flagName)
			if shouldLog != tt.wantShouldLog || atWarn != tt.wantAtWarn {
				t.Fatalf("classify = (log=%v, warn=%v), want (log=%v, warn=%v)",
					shouldLog, atWarn, tt.wantShouldLog, tt.wantAtWarn)
			}
		})
	}
}

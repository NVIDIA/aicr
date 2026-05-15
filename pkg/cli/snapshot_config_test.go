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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// === Regression: --config wiring on snapshot command ===

// TestSnapshotCmd_HasConfigFlag asserts --config is wired on the snapshot
// command. Regression guard for issue NVIDIA/aicr#913.
func TestSnapshotCmd_HasConfigFlag(t *testing.T) {
	cmd := snapshotCmd()
	found := false
	for _, f := range cmd.Flags {
		for _, name := range f.Names() {
			if name == "config" {
				found = true
			}
		}
	}
	if !found {
		t.Error("snapshot command must define --config flag")
	}
}

// captureSnapshotOpts runs parseSnapshotCmdOptions through the snapshot CLI
// command and returns the resolved options, so tests can assert on every
// merged value without invoking the snapshotter deploy path.
func captureSnapshotOpts(t *testing.T, args []string) *snapshotCmdOptions {
	t.Helper()
	var captured *snapshotCmdOptions
	cmd := snapshotCmd()
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		cfg, err := loadCmdConfig(ctx, c)
		if err != nil {
			return err
		}
		opts, err := parseSnapshotCmdOptions(c, cfg)
		if err != nil {
			return err
		}
		captured = opts
		return nil
	}
	if err := cmd.Run(context.Background(), append([]string{"snapshot"}, args...)); err != nil {
		t.Fatalf("snapshot run: %v", err)
	}
	return captured
}

// runSnapshotCmdExpectErr runs the snapshot command and returns the error
// produced by parseSnapshotCmdOptions. Used for negative-path tests.
func runSnapshotCmdExpectErr(t *testing.T, args []string) error {
	t.Helper()
	cmd := snapshotCmd()
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		cfg, err := loadCmdConfig(ctx, c)
		if err != nil {
			return err
		}
		_, err = parseSnapshotCmdOptions(c, cfg)
		return err
	}
	return cmd.Run(context.Background(), append([]string{"snapshot"}, args...))
}

// === Behavior preservation: flags-only path still works ===

// TestSnapshotCmd_FlagsAloneStillWork ensures the pre-config flag pathway
// continues to work after the --config refactor.
func TestSnapshotCmd_FlagsAloneStillWork(t *testing.T) {
	opts := captureSnapshotOpts(t, []string{
		"--namespace", "aicr-validation",
		"--node-selector", "nodeGroup=gpu-worker",
		"--toleration", "dedicated=gpu-workload:NoSchedule",
		"--timeout", "5m",
		"--no-cleanup",
		"-o", "snapshot.yaml",
	})

	if opts.namespace != "aicr-validation" {
		t.Errorf("namespace = %q, want aicr-validation", opts.namespace)
	}
	if opts.nodeSelector["nodeGroup"] != "gpu-worker" {
		t.Errorf("nodeSelector = %v, want nodeGroup=gpu-worker", opts.nodeSelector)
	}
	if len(opts.tolerations) != 1 || opts.tolerations[0].Key != "dedicated" {
		t.Errorf("tolerations = %+v, want one entry keyed by dedicated", opts.tolerations)
	}
	if opts.timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", opts.timeout)
	}
	if opts.cleanup {
		t.Errorf("cleanup = true, want false (--no-cleanup)")
	}
	if opts.tmplOpts.outputPath != "snapshot.yaml" {
		t.Errorf("outputPath = %q, want snapshot.yaml", opts.tmplOpts.outputPath)
	}
}

// TestSnapshotCmd_FlagsAloneDefaultsTolerateAll preserves the legacy
// behavior of `aicr snapshot` (no --toleration, no --config) tolerating
// every taint. A regression here would silently break snapshot capture
// on every tainted-node cluster.
func TestSnapshotCmd_FlagsAloneDefaultsTolerateAll(t *testing.T) {
	opts := captureSnapshotOpts(t, []string{"-o", "-"})
	if len(opts.tolerations) == 0 {
		t.Fatal("expected default tolerate-all tolerations when neither CLI nor config set them")
	}
	// DefaultTolerations() is a single bare Exists entry. Don't pin to that
	// exact shape — just verify it tolerates all taints.
	if opts.tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("default toleration operator = %v, want Exists", opts.tolerations[0].Operator)
	}
}

// === Config-driven: every section resolves into options ===

// TestSnapshotCmd_AllConfigSectionsResolve drives parseSnapshotCmdOptions
// with a config exercising every section, then asserts each option lands
// on the resolved snapshotCmdOptions. Regression backstop for schema drift.
func TestSnapshotCmd_AllConfigSectionsResolve(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
      format: yaml
    agent:
      namespace: aicr-validation
      image: ghcr.io/example/aicr:test
      imagePullSecrets:
        - secret-a
        - secret-b
      jobName: snap-job
      serviceAccountName: snap-sa
      nodeSelector:
        nodeGroup: gpu-worker
      tolerations:
        - dedicated=gpu-workload:NoSchedule
        - nvidia.com/gpu=present:NoSchedule
      requireGpu: true
      os: ubuntu
      requests: "cpu=500m,memory=1Gi"
      limits: "cpu=1,memory=2Gi"
    execution:
      timeout: 7m
      noCleanup: true
      maxNodesPerEntry: 4
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := captureSnapshotOpts(t, []string{"--config", cfgPath})

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"namespace", opts.namespace, "aicr-validation"},
		{"image", opts.image, "ghcr.io/example/aicr:test"},
		{"jobName", opts.jobName, "snap-job"},
		{"serviceAccountName", opts.serviceAccountName, "snap-sa"},
		{"requireGPU", opts.requireGPU, true},
		{"os", opts.os, "ubuntu"},
		{"timeout", opts.timeout, 7 * time.Minute},
		{"cleanup", opts.cleanup, false},
		{"maxNodesPerEntry", opts.maxNodesPerEntry, 4},
		{"outputPath", opts.tmplOpts.outputPath, "snapshot.yaml"},
		{"nodeSelector size", len(opts.nodeSelector), 1},
		{"nodeSelector value", opts.nodeSelector["nodeGroup"], "gpu-worker"},
		{"tolerations size", len(opts.tolerations), 2},
		{"imagePullSecrets size", len(opts.imagePullSecrets), 2},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if opts.requests == nil || opts.requests.Cpu().String() != "500m" {
		t.Errorf("requests cpu = %v, want 500m", opts.requests)
	}
	if opts.limits == nil || opts.limits.Memory().String() != "2Gi" {
		t.Errorf("limits memory = %v, want 2Gi", opts.limits)
	}
}

// TestSnapshotCmd_ConfigOnly_NoCLIFlags mirrors the demo workflow:
// `aicr snapshot --config aicr-config.yaml` with no other flags. Success
// criterion from issue #913.
func TestSnapshotCmd_ConfigOnly_NoCLIFlags(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    agent:
      namespace: aicr-validation
      nodeSelector:
        nodeGroup: gpu-worker
      tolerations:
        - dedicated=gpu-workload:NoSchedule
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := captureSnapshotOpts(t, []string{"--config", cfgPath})

	if opts.namespace != "aicr-validation" {
		t.Errorf("namespace = %q, want aicr-validation", opts.namespace)
	}
	if opts.nodeSelector["nodeGroup"] != "gpu-worker" {
		t.Errorf("nodeSelector nodeGroup = %q, want gpu-worker", opts.nodeSelector["nodeGroup"])
	}
	if len(opts.tolerations) != 1 || opts.tolerations[0].Key != "dedicated" {
		t.Errorf("tolerations = %+v, want one entry for dedicated", opts.tolerations)
	}
	if opts.tmplOpts.outputPath != "snapshot.yaml" {
		t.Errorf("outputPath = %q, want snapshot.yaml", opts.tmplOpts.outputPath)
	}
}

// === CLI overrides config in every dimension ===

// TestSnapshotCmd_FlagOverridesEverySection verifies CLI flags win over
// the corresponding config fields. This is the documented precedence
// rule for AICRConfig consumers.
func TestSnapshotCmd_FlagOverridesEverySection(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: config-snapshot.yaml
    agent:
      namespace: config-ns
      image: config:image
      jobName: config-job
      serviceAccountName: config-sa
      nodeSelector:
        nodeGroup: config-group
      tolerations:
        - cfg=val:NoSchedule
      requireGpu: false
      os: ubuntu
    execution:
      timeout: 1m
      noCleanup: false
      maxNodesPerEntry: 2
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := captureSnapshotOpts(t, []string{
		"--config", cfgPath,
		"--namespace", "flag-ns",
		"--image", "flag:image",
		"--job-name", "flag-job",
		"--service-account-name", "flag-sa",
		"--node-selector", "nodeGroup=flag-group",
		"--toleration", "flag=val:NoSchedule",
		"--timeout", "9m",
		"--no-cleanup",
		"--max-nodes-per-entry", "10",
		"--os", "rhel",
		"--require-gpu",
		"-o", "flag-snapshot.yaml",
	})

	wants := []struct {
		name string
		got  any
		want any
	}{
		{"namespace", opts.namespace, "flag-ns"},
		{"image", opts.image, "flag:image"},
		{"jobName", opts.jobName, "flag-job"},
		{"serviceAccountName", opts.serviceAccountName, "flag-sa"},
		{"nodeSelector", opts.nodeSelector["nodeGroup"], "flag-group"},
		{"tolerations[0].Key", opts.tolerations[0].Key, "flag"},
		{"timeout", opts.timeout, 9 * time.Minute},
		{"cleanup", opts.cleanup, false},
		{"maxNodesPerEntry", opts.maxNodesPerEntry, 10},
		{"os", opts.os, "rhel"},
		{"requireGPU", opts.requireGPU, true},
		{"outputPath", opts.tmplOpts.outputPath, "flag-snapshot.yaml"},
	}
	for _, w := range wants {
		if !reflect.DeepEqual(w.got, w.want) {
			t.Errorf("%s: got %v, want %v", w.name, w.got, w.want)
		}
	}
}

// === Config: tolerations nil-vs-empty semantics ===

// TestSnapshotCmd_ConfigEmptyTolerationsOptOut verifies that
// `tolerations: []` in config drops the implicit tolerate-all default —
// the explicit opt-out path documented on ValidateAgentSpec, which
// SnapshotAgentSpec mirrors.
func TestSnapshotCmd_ConfigEmptyTolerationsOptOut(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    agent:
      namespace: aicr-validation
      tolerations: []
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := captureSnapshotOpts(t, []string{"--config", cfgPath})
	if opts.tolerations == nil {
		t.Fatal("expected non-nil empty tolerations from explicit []")
	}
	if len(opts.tolerations) != 0 {
		t.Errorf("expected zero tolerations, got %+v", opts.tolerations)
	}
}

// === Negative cases ===

// TestSnapshotCmd_InvalidConfig_BadTimeout verifies that a malformed
// timeout in config surfaces as ErrCodeInvalidRequest via Resolve, not
// silently fall through.
func TestSnapshotCmd_InvalidConfig_BadTimeout(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    execution:
      timeout: not-a-duration
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := runSnapshotCmdExpectErr(t, []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("expected error for malformed timeout, got nil")
	}
	if !strings.Contains(err.Error(), "spec.snapshot.execution.timeout") {
		t.Errorf("error %q must reference spec.snapshot.execution.timeout", err.Error())
	}
}

// TestSnapshotCmd_InvalidConfig_BadFormat verifies the format enum is
// validated at config-load time.
func TestSnapshotCmd_InvalidConfig_BadFormat(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
      format: xml
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := runSnapshotCmdExpectErr(t, []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
	if !strings.Contains(err.Error(), "spec.snapshot.output.format") {
		t.Errorf("error %q must reference spec.snapshot.output.format", err.Error())
	}
}

// TestSnapshotCmd_InvalidConfig_UnknownField confirms the strict-decode
// path catches typos in spec.snapshot. This guards the "additive only"
// promise from the issue.
func TestSnapshotCmd_InvalidConfig_UnknownField(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    bogusKey: oops
    output:
      path: snapshot.yaml
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := runSnapshotCmdExpectErr(t, []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "bogusKey") {
		t.Errorf("error %q must reference the unknown field", err.Error())
	}
}

// TestSnapshotCmd_RequireGPURuntimeClass_StillMutuallyExclusive verifies
// the existing mutual-exclusion check survives the config-merge layer.
// The conflict should be detected regardless of whether the values come
// from CLI flags or config.
func TestSnapshotCmd_RequireGPURuntimeClass_StillMutuallyExclusive(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    agent:
      requireGpu: true
      runtimeClassName: nvidia
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := runSnapshotCmdExpectErr(t, []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("expected mutual-exclusion error from config-only conflict, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q must reference mutual exclusion", err.Error())
	}
}

// TestSnapshotCmd_ConfigBadResourcesRejected verifies bad `requests` /
// `limits` content from config is rejected with the same error code as
// CLI-supplied bad input. The config field uses the kubectl
// name=quantity comma-separated form.
func TestSnapshotCmd_ConfigBadResourcesRejected(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    agent:
      requests: "cpu="
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := runSnapshotCmdExpectErr(t, []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("expected error for malformed requests, got nil")
	}
	// PropagateOrWrap preserves the inner error's ErrCodeInvalidRequest
	// without re-prefixing, so assert on the parser's diagnostic message
	// (the offending entry) rather than the wrap label.
	if !strings.Contains(err.Error(), "cpu=") {
		t.Errorf("error %q must reference the offending entry cpu=", err.Error())
	}
}

// === toAgentConfig conversion sanity check ===

// TestSnapshotCmdOptions_ToAgentConfig pins the field-by-field
// translation from snapshotCmdOptions to snapshotter.AgentConfig.
// Regression guard: silent drops would manifest as default empty values
// reaching the deployer.
func TestSnapshotCmdOptions_ToAgentConfig(t *testing.T) {
	opts := &snapshotCmdOptions{
		kubeconfig:         "/kube/config",
		namespace:          "ns",
		image:              "img",
		imagePullSecrets:   []string{"secret"},
		jobName:            "job",
		serviceAccountName: "sa",
		nodeSelector:       map[string]string{"k": "v"},
		tolerations:        []corev1.Toleration{{Key: "t"}},
		timeout:            3 * time.Minute,
		cleanup:            true,
		debug:              true,
		privileged:         false,
		requireGPU:         true,
		runtimeClass:       "nvidia",
		os:                 "ubuntu",
		maxNodesPerEntry:   5,
		requests:           corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		limits:             corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		tmplOpts: &snapshotTemplateOptions{
			outputPath:   "snapshot.yaml",
			templatePath: "tpl.tmpl",
		},
	}
	ac := opts.toAgentConfig()
	if ac == nil {
		t.Fatal("nil AgentConfig")
	}
	wants := []struct {
		name string
		got  any
		want any
	}{
		{"Kubeconfig", ac.Kubeconfig, "/kube/config"},
		{"Namespace", ac.Namespace, "ns"},
		{"Image", ac.Image, "img"},
		{"JobName", ac.JobName, "job"},
		{"ServiceAccountName", ac.ServiceAccountName, "sa"},
		{"Timeout", ac.Timeout, 3 * time.Minute},
		{"Cleanup", ac.Cleanup, true},
		{"Debug", ac.Debug, true},
		{"Privileged", ac.Privileged, false},
		{"RequireGPU", ac.RequireGPU, true},
		{"RuntimeClassName", ac.RuntimeClassName, "nvidia"},
		{"OS", ac.OS, "ubuntu"},
		{"MaxNodesPerEntry", ac.MaxNodesPerEntry, 5},
		{"Output", ac.Output, "snapshot.yaml"},
		{"TemplatePath", ac.TemplatePath, "tpl.tmpl"},
		{"NodeSelector[k]", ac.NodeSelector["k"], "v"},
	}
	for _, w := range wants {
		if !reflect.DeepEqual(w.got, w.want) {
			t.Errorf("%s: got %v, want %v", w.name, w.got, w.want)
		}
	}
	if len(ac.Tolerations) != 1 || ac.Tolerations[0].Key != "t" {
		t.Errorf("Tolerations = %+v", ac.Tolerations)
	}
	if ac.Requests.Cpu().String() != "1" {
		t.Errorf("Requests CPU = %v, want 1", ac.Requests.Cpu())
	}
	if ac.Limits.Memory().String() != "2Gi" {
		t.Errorf("Limits Memory = %v, want 2Gi", ac.Limits.Memory())
	}
}

// === Privileged pointer semantics ===

// TestSnapshotCmd_ConfigPrivilegedFalseHonored verifies an explicit
// privileged=false in config flows through to the agent unless the CLI
// flag overrides it. The CLI default for --privileged is true; without
// the pointer-vs-bool distinction in SnapshotExecutionSpec, an opt-out
// from PSS-restricted namespaces would silently fail.
func TestSnapshotCmd_ConfigPrivilegedFalseHonored(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    execution:
      privileged: false
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	opts := captureSnapshotOpts(t, []string{"--config", cfgPath})
	if opts.privileged {
		t.Error("expected privileged=false from config to override CLI default=true")
	}
}

// TestSnapshotCmd_NoConfigPrivilegedDefaultsTrue preserves the legacy
// default. Without --privileged and without --config, the agent runs
// privileged so the system collectors keep working.
func TestSnapshotCmd_NoConfigPrivilegedDefaultsTrue(t *testing.T) {
	opts := captureSnapshotOpts(t, []string{"-o", "-"})
	if !opts.privileged {
		t.Error("expected privileged=true by default (CLI flag default)")
	}
}

// === HTTP source: snapshot section round-trips through config.Load ===

// testSnapshotConfig is a canned config used by the HTTP source test;
// declared at package scope so other tests can reuse it if needed.
var testSnapshotConfig = `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  snapshot:
    output:
      path: snapshot.yaml
    agent:
      namespace: aicr-validation
      nodeSelector:
        nodeGroup: gpu-worker
`

func TestSnapshotCmd_ConfigFromInlineFile(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(testSnapshotConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	opts := captureSnapshotOpts(t, []string{"--config", cfgPath})
	if opts.namespace != "aicr-validation" {
		t.Errorf("namespace = %q, want aicr-validation", opts.namespace)
	}
	if opts.nodeSelector["nodeGroup"] != "gpu-worker" {
		t.Errorf("nodeSelector nodeGroup = %q, want gpu-worker", opts.nodeSelector["nodeGroup"])
	}
}

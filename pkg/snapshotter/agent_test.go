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

package snapshotter

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
	"github.com/NVIDIA/aicr/pkg/serializer"
	corev1 "k8s.io/api/core/v1"
)

func TestLogWriter(t *testing.T) {
	writer := logWriter()
	if writer == nil {
		t.Fatal("logWriter() returned nil")
	}
	if writer != os.Stderr {
		t.Errorf("logWriter() = %v, want os.Stderr", writer)
	}
}

func TestDefaultTolerations(t *testing.T) {
	tolerations := DefaultTolerations()

	if len(tolerations) != 1 {
		t.Fatalf("DefaultTolerations() returned %d tolerations, want 1", len(tolerations))
	}

	tol := tolerations[0]
	if tol.Operator != corev1.TolerationOpExists {
		t.Errorf("DefaultTolerations()[0].Operator = %v, want %v", tol.Operator, corev1.TolerationOpExists)
	}
	if tol.Key != "" {
		t.Errorf("DefaultTolerations()[0].Key = %q, want empty string", tol.Key)
	}
}

func TestBuildAgentConfigTolerations(t *testing.T) {
	explicit := []corev1.Toleration{
		{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "gpu-workload",
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}
	tests := []struct {
		name  string
		input []corev1.Toleration
		want  []corev1.Toleration
	}{
		{
			name: "omitted uses tolerate-all default",
			want: DefaultTolerations(),
		},
		{
			name:  "explicit empty disables default",
			input: []corev1.Toleration{},
			want:  []corev1.Toleration{},
		},
		{
			name:  "explicit tolerations are preserved",
			input: explicit,
			want:  explicit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildAgentConfig(&AgentConfig{Tolerations: tt.input}, "20260821-142233-9f3a1c0b7e2d4a55", "snapshot.yaml", false).Tolerations
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildAgentConfig().Tolerations = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestBuildAgentConfigPropagatesRunIDAndOwnership confirms buildAgentConfig
// forwards its runID argument, AgentConfig.NameBase, and its ownsOutput
// parameter onto agent.Config.RunID / agent.Config.NameBase /
// agent.Config.OwnsOutputConfigMap — the projection deployAndWaitForResult
// relies on so the deployer scopes every resource name to this run, applies
// the caller's naming prefix, and Cleanup knows whether it may delete the
// staging ConfigMap.
//
// runID is a parameter, not a read of AgentConfig.RunID: DeployAndCollect
// resolves it into a local and never writes a generated ID back into the
// caller's config. The AgentConfig below deliberately leaves RunID empty so a
// projection that regressed to reading config.RunID would produce "" and fail
// here.
func TestBuildAgentConfigPropagatesRunIDAndOwnership(t *testing.T) {
	tests := []struct {
		name       string
		ownsOutput bool
	}{
		{name: "owned staging ConfigMap", ownsOutput: true},
		{name: "user-supplied ConfigMap", ownsOutput: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildAgentConfig(&AgentConfig{
				NameBase: "aicr-validate",
			}, "20260821-142233-9f3a1c0b7e2d4a55", "cm://ns/name", tt.ownsOutput)
			if got.RunID != "20260821-142233-9f3a1c0b7e2d4a55" {
				t.Errorf("agent.Config.RunID = %q, want the runID argument", got.RunID)
			}
			if got.NameBase != "aicr-validate" {
				t.Errorf("agent.Config.NameBase = %q, want the AgentConfig.NameBase value", got.NameBase)
			}
			if got.OwnsOutputConfigMap != tt.ownsOutput {
				t.Errorf("agent.Config.OwnsOutputConfigMap = %v, want %v", got.OwnsOutputConfigMap, tt.ownsOutput)
			}
		})
	}
}

func TestAgentConfig_Defaults(t *testing.T) {
	// Test that AgentConfig can be instantiated with zero values
	cfg := AgentConfig{}

	if cfg.Cleanup {
		t.Error("AgentConfig.Cleanup should default to false")
	}
	if cfg.Debug {
		t.Error("AgentConfig.Debug should default to false")
	}
	if cfg.Privileged {
		t.Error("AgentConfig.Privileged should default to false")
	}
	if cfg.Timeout != 0 {
		t.Errorf("AgentConfig.Timeout should default to 0, got %v", cfg.Timeout)
	}
}

func TestGetKubeClientPreservesKubeconfigErrorCode(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("invalid yaml content"), 0o600); err != nil {
		t.Fatalf("failed to write invalid kubeconfig: %v", err)
	}

	_, err := getKubeClient(kubeconfig)
	if err == nil {
		t.Fatal("getKubeClient() error = nil, want ErrCodeInvalidRequest")
	}

	var structuredErr *errors.StructuredError
	if !stderrors.As(err, &structuredErr) {
		t.Fatalf("getKubeClient() error = %v, want *errors.StructuredError", err)
	}
	if structuredErr.Code != errors.ErrCodeInvalidRequest {
		t.Errorf("getKubeClient() error code = %s, want %s", structuredErr.Code, errors.ErrCodeInvalidRequest)
	}
}

func TestParseNodeSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selectors []string
		want      map[string]string
		wantErr   bool
	}{
		{
			name:      "empty selectors",
			selectors: []string{},
			want:      map[string]string{},
			wantErr:   false,
		},
		{
			name:      "single selector",
			selectors: []string{"nodeGroup=system-pool"},
			want:      map[string]string{"nodeGroup": "system-pool"},
			wantErr:   false,
		},
		{
			name:      "multiple selectors",
			selectors: []string{"nodeGroup=system-pool", "accelerator=nvidia-gpu"},
			want:      map[string]string{"nodeGroup": "system-pool", "accelerator": "nvidia-gpu"},
			wantErr:   false,
		},
		{
			name:      "selector with equals in value",
			selectors: []string{"label=key=value"},
			want:      map[string]string{"label": "key=value"},
			wantErr:   false,
		},
		{
			name:      "invalid selector no equals",
			selectors: []string{"invalid"},
			want:      nil,
			wantErr:   true,
		},
		{
			name:      "invalid selector only key",
			selectors: []string{"key="},
			want:      map[string]string{"key": ""},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseNodeSelectors(tt.selectors)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseNodeSelectors() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("ParseNodeSelectors() got %d selectors, want %d", len(got), len(tt.want))
					return
				}
				for k, v := range tt.want {
					if got[k] != v {
						t.Errorf("ParseNodeSelectors() got[%s] = %s, want %s", k, got[k], v)
					}
				}
			}
		})
	}
}

func TestParseTaint(t *testing.T) {
	tests := []struct {
		name     string
		taintStr string
		want     *corev1.Taint
		wantErr  bool
	}{
		{
			name:     "taint with key, value, and effect",
			taintStr: "skyhook.nvidia.com/runtime-required=true:NoSchedule",
			want: &corev1.Taint{
				Key:    "skyhook.nvidia.com/runtime-required",
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			},
			wantErr: false,
		},
		{
			name:     "taint with key and effect (no value)",
			taintStr: "dedicated:NoSchedule",
			want: &corev1.Taint{
				Key:    "dedicated",
				Value:  "",
				Effect: corev1.TaintEffectNoSchedule,
			},
			wantErr: false,
		},
		{
			name:     "taint with PreferNoSchedule effect",
			taintStr: "workload-type=training:PreferNoSchedule",
			want: &corev1.Taint{
				Key:    "workload-type",
				Value:  "training",
				Effect: corev1.TaintEffectPreferNoSchedule,
			},
			wantErr: false,
		},
		{
			name:     "taint with NoExecute effect",
			taintStr: "node.kubernetes.io/not-ready:NoExecute",
			want: &corev1.Taint{
				Key:    "node.kubernetes.io/not-ready",
				Value:  "",
				Effect: corev1.TaintEffectNoExecute,
			},
			wantErr: false,
		},
		{
			name:     "taint with value containing equals",
			taintStr: "key=value=with=equals:NoSchedule",
			want: &corev1.Taint{
				Key:    "key",
				Value:  "value=with=equals",
				Effect: corev1.TaintEffectNoSchedule,
			},
			wantErr: false,
		},
		{
			name:     "empty taint string",
			taintStr: "",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid format - no colon",
			taintStr: "key=value",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid format - multiple colons",
			taintStr: "key=value:effect:extra",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid format - only colon",
			taintStr: ":NoSchedule",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid taint effect - InvalidEffect",
			taintStr: "key=value:InvalidEffect",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid taint effect - empty effect",
			taintStr: "key=value:",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid taint effect - random string",
			taintStr: "key=value:BadEffect",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid taint effect - lowercase",
			taintStr: "key=value:noschedule",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "invalid taint effect - mixed case",
			taintStr: "key=value:NoScheduleButWrong",
			want:     nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTaint(tt.taintStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTaint() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got == nil {
					t.Fatal("ParseTaint() returned nil, want non-nil")
				}
				if got.Key != tt.want.Key {
					t.Errorf("ParseTaint() Key = %s, want %s", got.Key, tt.want.Key)
				}
				if got.Value != tt.want.Value {
					t.Errorf("ParseTaint() Value = %s, want %s", got.Value, tt.want.Value)
				}
				if got.Effect != tt.want.Effect {
					t.Errorf("ParseTaint() Effect = %s, want %s", got.Effect, tt.want.Effect)
				}
			}
		})
	}
}

func TestParseTolerations(t *testing.T) {
	tests := []struct {
		name        string
		tolerations []string
		wantLen     int
		wantErr     bool
	}{
		{
			name:        "empty tolerations returns defaults",
			tolerations: []string{},
			wantLen:     1, // Default toleration
			wantErr:     false,
		},
		{
			name:        "single toleration with value",
			tolerations: []string{"dedicated=system-workload:NoSchedule"},
			wantLen:     1,
			wantErr:     false,
		},
		{
			name:        "single toleration without value",
			tolerations: []string{"nvidia.com/gpu:NoSchedule"},
			wantLen:     1,
			wantErr:     false,
		},
		{
			name:        "multiple tolerations",
			tolerations: []string{"dedicated=user:NoSchedule", "nvidia.com/gpu:NoSchedule"},
			wantLen:     2,
			wantErr:     false,
		},
		{
			name:        "invalid toleration no effect",
			tolerations: []string{"key=value"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid toleration too many colons",
			tolerations: []string{"key:value:extra"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid taint effect - InvalidEffect",
			tolerations: []string{"key=value:InvalidEffect"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid taint effect - empty effect",
			tolerations: []string{"key=value:"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid taint effect - random string",
			tolerations: []string{"key=value:BadEffect"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid taint effect - lowercase",
			tolerations: []string{"key=value:noschedule"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid taint effect - mixed case",
			tolerations: []string{"key=value:NoScheduleButWrong"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "invalid taint effect in second toleration",
			tolerations: []string{"key1=value1:NoSchedule", "key2=value2:InvalidEffect"},
			wantLen:     0,
			wantErr:     true,
		},
		{
			name:        "wildcard toleration",
			tolerations: []string{"*"},
			wantLen:     1,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTolerations(tt.tolerations)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTolerations() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got) != tt.wantLen {
				t.Errorf("ParseTolerations() got %d tolerations, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestParseTolerationsOperator(t *testing.T) {
	// Test that tolerations have correct operators set
	tests := []struct {
		name         string
		toleration   string
		wantOperator corev1.TolerationOperator
		wantKey      string
		wantValue    string
		wantEffect   corev1.TaintEffect
	}{
		{
			name:         "toleration with value uses Equal operator",
			toleration:   "dedicated=user-workload:NoSchedule",
			wantOperator: corev1.TolerationOpEqual,
			wantKey:      "dedicated",
			wantValue:    "user-workload",
			wantEffect:   corev1.TaintEffectNoSchedule,
		},
		{
			name:         "toleration without value uses Exists operator",
			toleration:   "nvidia.com/gpu:NoExecute",
			wantOperator: corev1.TolerationOpExists,
			wantKey:      "nvidia.com/gpu",
			wantValue:    "",
			wantEffect:   corev1.TaintEffectNoExecute,
		},
		{
			name:         "wildcard toleration produces Exists with empty key",
			toleration:   "*",
			wantOperator: corev1.TolerationOpExists,
			wantKey:      "",
			wantValue:    "",
			wantEffect:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTolerations([]string{tt.toleration})
			if err != nil {
				t.Fatalf("ParseTolerations() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ParseTolerations() got %d tolerations, want 1", len(got))
			}

			tol := got[0]
			if tol.Operator != tt.wantOperator {
				t.Errorf("Operator = %v, want %v", tol.Operator, tt.wantOperator)
			}
			if tol.Key != tt.wantKey {
				t.Errorf("Key = %v, want %v", tol.Key, tt.wantKey)
			}
			if tol.Value != tt.wantValue {
				t.Errorf("Value = %v, want %v", tol.Value, tt.wantValue)
			}
			if tol.Effect != tt.wantEffect {
				t.Errorf("Effect = %v, want %v", tol.Effect, tt.wantEffect)
			}
		})
	}
}

// TestAgentOutputURILogic exercises agentConfigMapTarget — the rule that
// decides where the agent Job stages its result:
//  1. A file path leaves the Job on the run-scoped internal ConfigMap in its
//     namespace, which this run owns.
//  2. A cm:// URI makes that ConfigMap the Job's target directly; this run
//     does NOT own it (the caller supplied it), so a rewrite failure must be
//     fatal rather than a warning.
//  3. Stdout (empty or "-") behaves like a file path.
func TestAgentOutputURILogic(t *testing.T) {
	const testRunID = "20260821-142233-9f3a1c0b7e2d4a55"

	tests := []struct {
		name               string
		agentNamespace     string
		userOutput         string
		wantAgentOutputHas string // substring that should be in agentOutput
		wantUsesUserOutput bool   // whether agentOutput should equal userOutput
	}{
		{
			name:               "file output uses run-scoped internal ConfigMap",
			agentNamespace:     "default",
			userOutput:         "snapshot.yaml",
			wantAgentOutputHas: "cm://default/aicr-agent-snapshot-" + testRunID,
			wantUsesUserOutput: false,
		},
		{
			name:               "stdout uses run-scoped internal ConfigMap",
			agentNamespace:     "default",
			userOutput:         "",
			wantAgentOutputHas: "cm://default/aicr-agent-snapshot-" + testRunID,
			wantUsesUserOutput: false,
		},
		{
			name:               "dash stdout uses run-scoped internal ConfigMap",
			agentNamespace:     "default",
			userOutput:         "-",
			wantAgentOutputHas: "cm://default/aicr-agent-snapshot-" + testRunID,
			wantUsesUserOutput: false,
		},
		{
			name:               "ConfigMap URI uses user's URI",
			agentNamespace:     "default",
			userOutput:         "cm://custom-ns/my-snapshot",
			wantAgentOutputHas: "cm://custom-ns/my-snapshot",
			wantUsesUserOutput: true,
		},
		{
			name:               "custom namespace uses that namespace for the run-scoped ConfigMap",
			agentNamespace:     "custom-namespace",
			userOutput:         "output.yaml",
			wantAgentOutputHas: "cm://custom-namespace/aicr-agent-snapshot-" + testRunID,
			wantUsesUserOutput: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentOutput, ownsOutput, err := agentConfigMapTarget(&AgentConfig{
				Namespace: tt.agentNamespace,
				Output:    tt.userOutput,
			}, testRunID)
			if err != nil {
				t.Fatalf("agentConfigMapTarget: %v", err)
			}

			if tt.wantUsesUserOutput {
				if agentOutput != tt.userOutput {
					t.Errorf("agentOutput = %q, want %q (user's URI)", agentOutput, tt.userOutput)
				}
			} else {
				if agentOutput != tt.wantAgentOutputHas {
					t.Errorf("agentOutput = %q, want %q", agentOutput, tt.wantAgentOutputHas)
				}
			}
			// ownsOutput is the logical inverse of "the user supplied this
			// output": the run owns (and Cleanup may delete) the staging
			// ConfigMap only when it did NOT come from the user.
			wantOwnsOutput := !tt.wantUsesUserOutput
			if ownsOutput != wantOwnsOutput {
				t.Errorf("ownsOutput = %v, want %v — it must track whether this run owns the "+
					"ConfigMap, since it decides whether Cleanup may delete it and whether an "+
					"AKS-pool-merge rewrite failure is fatal", ownsOutput, wantOwnsOutput)
			}
		})
	}
}

// TestAgentConfigMapTargetIsRunScoped and TestAgentConfigMapTargetLeavesUserURIAlone
// pin the ownsOutput inversion: the run owns (and Cleanup may delete) the
// staging ConfigMap only when the user did NOT name a cm:// destination.
// Getting this backwards deletes the user's own output ConfigMap.
func TestAgentConfigMapTargetIsRunScoped(t *testing.T) {
	const runID = "20260821-142233-9f3a1c0b7e2d4a55"
	cfg := &AgentConfig{Namespace: "gpu-operator"}
	uri, ownsOutput, err := agentConfigMapTarget(cfg, runID)
	if err != nil {
		t.Fatalf("agentConfigMapTarget() error = %v", err)
	}
	// The staging ConfigMap is deliberately prefixed "aicr-agent-snapshot",
	// NOT "aicr-snapshot": pkg/validator names its own snapshot data
	// ConfigMap "aicr-snapshot-<runID>", and `aicr validate` gives both
	// subsystems the same run ID in the same namespace. See
	// TestStagingConfigMapNameDoesNotCollideWithValidator in pkg/k8s/agent.
	want := "cm://gpu-operator/aicr-agent-snapshot-20260821-142233-9f3a1c0b7e2d4a55"
	if uri != want {
		t.Errorf("uri = %q, want %q", uri, want)
	}
	// The URI must be built from the one exported helper the agent package
	// also deletes by, not from a second copy of the format string.
	if wantHelper := "cm://gpu-operator/" + agent.StagingConfigMapName(runID); uri != wantHelper {
		t.Errorf("uri = %q, want %q (agent.StagingConfigMapName)", uri, wantHelper)
	}
	if !ownsOutput {
		t.Error("ownsOutput = false, want true for the internal staging ConfigMap")
	}
}

func TestAgentConfigMapTargetLeavesUserURIAlone(t *testing.T) {
	cfg := &AgentConfig{Namespace: "gpu-operator", Output: "cm://gpu-operator/aicr-snapshot"}
	uri, ownsOutput, err := agentConfigMapTarget(cfg, "20260821-142233-9f3a1c0b7e2d4a55")
	if err != nil {
		t.Fatalf("agentConfigMapTarget() error = %v", err)
	}
	if uri != cfg.Output {
		t.Errorf("uri = %q, want the user's URI %q unchanged", uri, cfg.Output)
	}
	if ownsOutput {
		t.Error("ownsOutput = true; a user-supplied cm:// output is delivered, not run-owned")
	}
}

func TestAgentConfigWithTemplatePath(t *testing.T) {
	// Test that AgentConfig can hold TemplatePath
	cfg := AgentConfig{
		Namespace:    "default",
		TemplatePath: "/path/to/template.tmpl",
		Output:       "output.yaml",
	}

	if cfg.TemplatePath != "/path/to/template.tmpl" {
		t.Errorf("AgentConfig.TemplatePath = %q, want %q", cfg.TemplatePath, "/path/to/template.tmpl")
	}
}

// snapshotFixture is a minimal but realistic agent document. The
// "unmodeledField" key stands in for a field a NEWER agent image emits that
// this binary's Snapshot type does not know about — the reason delivery must
// write raw bytes instead of re-serializing the parsed struct.
const snapshotFixture = `apiVersion: aicr.run/v1alpha2
kind: Snapshot
metadata:
  version: v9.9.9
  source: node-1
unmodeledField:
  fromANewerAgent: true
measurements: []
`

// TestDeliverSnapshot_FileIsByteIdentical is the byte-identity guarantee that
// routing `aicr snapshot` through the facade had to preserve: what lands on
// disk is exactly what the agent emitted, including fields this binary's
// Snapshot type does not model. A re-serialization of the parsed struct would
// drop unmodeledField and reorder keys.
func TestDeliverSnapshot_FileIsByteIdentical(t *testing.T) {
	out := filepath.Join(t.TempDir(), "snapshot.yaml")

	if err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{Output: out}); err != nil {
		t.Fatalf("DeliverSnapshot: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read delivered snapshot: %v", err)
	}
	if string(got) != snapshotFixture {
		t.Errorf("delivered bytes differ from the agent's output\n got:\n%s\nwant:\n%s", got, snapshotFixture)
	}
}

// TestDeliverSnapshot_HonorsFormat is the regression test for issue #2398:
// `aicr snapshot --format json` used to write the agent's YAML into whatever
// destination the user named, so a .json file held a YAML document and every
// downstream consumer (aicr diff, which picks its decoder from the file
// extension, and jq) failed on it. The agent always stages YAML, so delivery
// is the only place the requested format can be applied.
func TestDeliverSnapshot_HonorsFormat(t *testing.T) {
	tests := []struct {
		name       string
		format     serializer.Format
		wantPrefix string
		wantHas    []string
	}{
		{
			name:       "json",
			format:     serializer.FormatJSON,
			wantPrefix: "{",
			wantHas:    []string{`"apiVersion": "aicr.run/v1alpha2"`, `"fromANewerAgent": true`},
		},
		{
			name:       "table",
			format:     serializer.FormatTable,
			wantPrefix: "FIELD",
			wantHas:    []string{"APIVersion", "aicr.run/v1alpha2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "snapshot.out")
			if err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
				Output: out,
				Format: tt.format,
			}); err != nil {
				t.Fatalf("DeliverSnapshot(%s): %v", tt.format, err)
			}

			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read delivered snapshot: %v", err)
			}
			if !strings.HasPrefix(string(got), tt.wantPrefix) {
				t.Errorf("delivered %s output does not start with %q — the agent's YAML was "+
					"written unconverted:\n%s", tt.format, tt.wantPrefix, got)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(string(got), want) {
					t.Errorf("delivered %s output missing %q:\n%s", tt.format, want, got)
				}
			}
		})
	}
}

// TestDeliverSnapshot_JSONDecodesAsSnapshot closes the loop the issue
// reported: the JSON rendering must be readable by the same decoders that
// consume a .json snapshot, and it must carry fields this binary's Snapshot
// type does not model (the generic-map path, not a typed round trip).
func TestDeliverSnapshot_JSONDecodesAsSnapshot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "snapshot.json")
	if err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
		Output: out,
		Format: serializer.FormatJSON,
	}); err != nil {
		t.Fatalf("DeliverSnapshot(json): %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read delivered snapshot: %v", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("delivered .json does not decode as JSON: %v\n%s", err, data)
	}
	if snap.APIVersion != "aicr.run/v1alpha2" || snap.Metadata["version"] != "v9.9.9" {
		t.Errorf("decoded snapshot = %+v, want the fixture's apiVersion and metadata", snap.Header)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal delivered JSON: %v", err)
	}
	if _, ok := doc["unmodeledField"]; !ok {
		t.Error("JSON rendering dropped unmodeledField; it must go through a generic map so " +
			"a newer agent image's fields survive delivery")
	}
}

// TestDeliverSnapshot_UnsetFormatStaysByteIdentical protects SDK callers that
// predate SnapshotDelivery.Format: the zero value must keep meaning "the
// agent's YAML, unchanged".
func TestDeliverSnapshot_UnsetFormatStaysByteIdentical(t *testing.T) {
	for _, format := range []serializer.Format{"", serializer.FormatYAML} {
		out := filepath.Join(t.TempDir(), "snapshot.yaml")
		if err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
			Output: out,
			Format: format,
		}); err != nil {
			t.Fatalf("DeliverSnapshot(%q): %v", format, err)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read delivered snapshot: %v", err)
		}
		if string(got) != snapshotFixture {
			t.Errorf("format %q altered the agent's bytes\n got:\n%s\nwant:\n%s", format, got, snapshotFixture)
		}
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written. The pipe is drained concurrently: a snapshot is larger than the
// OS pipe buffer, so reading only after fn returns would deadlock on write.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(): %v", err)
	}
	saved := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = saved })

	drained := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, copyErr := io.Copy(&buf, reader)
		if copyErr != nil {
			t.Errorf("drain stdout: %v", copyErr)
		}
		drained <- buf.Bytes()
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	out := <-drained
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return out
}

// TestDeliverSnapshot_StdoutMatchesFile pins stdout as a delivery destination
// like any other: `aicr snapshot > snapshot.yaml` has to produce the same
// artifact as `-o snapshot.yaml`, for every format. Stdout used to append a
// newline the file path did not, so the two differed by a byte and a piped
// hash could not reproduce the file.
func TestDeliverSnapshot_StdoutMatchesFile(t *testing.T) {
	for _, format := range []serializer.Format{"", serializer.FormatYAML, serializer.FormatJSON, serializer.FormatTable} {
		t.Run(string(format), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "snapshot.out")
			if err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
				Output: file,
				Format: format,
			}); err != nil {
				t.Fatalf("DeliverSnapshot(file, %q): %v", format, err)
			}
			wantBytes, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read delivered snapshot: %v", err)
			}

			var deliverErr error
			got := captureStdout(t, func() {
				deliverErr = DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
					Output: "-",
					Format: format,
				})
			})
			if deliverErr != nil {
				t.Fatalf("DeliverSnapshot(stdout, %q): %v", format, deliverErr)
			}

			if !bytes.Equal(got, wantBytes) {
				t.Errorf("stdout and file delivery differ for format %q\n stdout (%d bytes):\n%s\n file (%d bytes):\n%s",
					format, len(got), got, len(wantBytes), wantBytes)
			}
			// Every rendering terminates itself, which is what lets
			// stdout add nothing without gluing the next shell prompt
			// to the output.
			if len(wantBytes) > 0 && wantBytes[len(wantBytes)-1] != '\n' {
				t.Errorf("format %q rendering is not newline-terminated; stdout would leave a "+
					"dangling prompt:\n%s", format, wantBytes)
			}
		})
	}
}

// TestDeliverSnapshot_StdoutPreservesTrailingNewline is the byte-identity
// guarantee on the stdout path. The agent's document always ends in a newline
// (MarshalYAMLDeterministic), and appending another gave it a trailing blank
// line; an SDK caller can also hand over bytes with no terminator, and stdout
// must not invent one. Neither input may be rewritten.
func TestDeliverSnapshot_StdoutPreservesTrailingNewline(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"trailing newline", snapshotFixture},
		{"no trailing newline", strings.TrimSuffix(snapshotFixture, "\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deliverErr error
			got := captureStdout(t, func() {
				deliverErr = DeliverSnapshot(t.Context(), []byte(tt.data), SnapshotDelivery{Output: "-"})
			})
			if deliverErr != nil {
				t.Fatalf("DeliverSnapshot(stdout): %v", deliverErr)
			}
			if string(got) != tt.data {
				t.Errorf("stdout altered the agent's bytes\n got  (%d bytes): %q\n want (%d bytes): %q",
					len(got), got, len(tt.data), tt.data)
			}
		})
	}
}

// TestDeliverSnapshot_RejectsUnknownFormat keeps an unrecognized format from
// silently degrading to another encoding — the exact failure mode of issue
// #2398. The CLI validates --format up front, so this is the SDK-caller guard.
//
// Every destination must reject the same set. The cm:// case is the one that
// needs the explicit check: serializer's ConfigMap writer coerces an unknown
// format to JSON, so without it that destination would succeed where stdout
// and file delivery fail.
func TestDeliverSnapshot_RejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	file := filepath.Join(dir, "snapshot.out")

	tests := []struct {
		name   string
		output string
	}{
		{"file", file},
		{"stdout", "-"},
		{"configmap", "cm://default/aicr-snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
				Output: tt.output,
				Format: serializer.Format("toml"),
			})
			if err == nil {
				t.Fatalf("DeliverSnapshot(%s, format=toml) = nil error, want a rejection", tt.output)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
			}
		})
	}

	// The rejection must land before any destination is touched — no
	// truncated file, and for cm:// no cluster contact (this test has no
	// cluster, so a format check that ran late would surface as a
	// connection error instead of ErrCodeInvalidRequest above).
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read working dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("rejected format still wrote to the destination: %v", entries)
	}
}

// TestRawSnapshotDocMarshalJSON pins the wrapper's JSON contract: it has no
// exported fields, so without MarshalJSON a cm:// destination in JSON format
// would receive "{}" — a successful write of an empty snapshot.
func TestRawSnapshotDocMarshalJSON(t *testing.T) {
	got, err := json.Marshal(rawSnapshotDoc{doc: map[string]any{
		"kind":           "Snapshot",
		"futureTopLevel": "keep-me",
	}})
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := `{"futureTopLevel":"keep-me","kind":"Snapshot"}`
	if string(got) != want {
		t.Errorf("MarshalJSON = %s, want %s", got, want)
	}
}

// TestDeliverSnapshot_ConfigMapRejectsMalformedURI proves a cm:// destination
// is parsed rather than prefix-matched, and — critically — that delivery to a
// ConfigMap is not a silent no-op. It used to log success and return nil, so
// an SDK caller that collected to the default internal ConfigMap and then
// delivered to cm://ns/name got no artifact AND no error.
//
// A malformed URI is the assertion that needs no cluster: it must fail with
// ErrCodeInvalidRequest, which is only reachable if delivery actually tries to
// resolve and write the destination.
func TestDeliverSnapshot_ConfigMapRejectsMalformedURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{"scheme only", "cm://"},
		{"namespace without name", "cm://aicr-snapshot"},
		{"empty namespace", "cm:///aicr-snapshot"},
		{"empty name", "cm://default/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{Output: tt.uri})
			if err == nil {
				t.Fatalf("DeliverSnapshot(%q) = nil error, want a rejection; a ConfigMap "+
					"destination must never report success without writing one", tt.uri)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestDeliverSnapshot_ConfigMapWritesNoLocalFile guards the destination
// dispatch: a cm:// Output must not fall through to the file branch and drop a
// literal "cm:..." file in the working directory. The delivery below cannot
// reach a cluster, so it is expected to fail — what matters is that it failed
// on the ConfigMap path and left the filesystem alone.
func TestDeliverSnapshot_ConfigMapWritesNoLocalFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Point at an unreachable apiserver so the write fails fast and
	// deterministically instead of picking up an ambient kubeconfig.
	unreachable := filepath.Join(dir, "does-not-exist.kubeconfig")
	err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
		Output:     "cm://default/aicr-snapshot",
		Kubeconfig: unreachable,
	})
	if err == nil {
		t.Fatal("DeliverSnapshot(cm://) with an unusable kubeconfig = nil error, want a write failure")
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read working dir: %v", readErr)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(unreachable) {
			t.Errorf("cm:// delivery created local file %q; the ConfigMap branch must not "+
				"fall through to file output", e.Name())
		}
	}
}

// TestDeliverSnapshot_Template renders through a Go template rather than
// copying bytes — the one delivery mode that must parse the document, since
// the template addresses Snapshot fields.
func TestDeliverSnapshot_Template(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "snapshot.tmpl")
	if err := os.WriteFile(tmpl, []byte("version={{ .Metadata.version }}\n"), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	out := filepath.Join(dir, "report.md")

	if err := DeliverSnapshot(t.Context(), []byte(snapshotFixture), SnapshotDelivery{
		Output:       out,
		TemplatePath: tmpl,
	}); err != nil {
		t.Fatalf("DeliverSnapshot(template): %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read rendered report: %v", err)
	}
	if string(got) != "version=v9.9.9\n" {
		t.Errorf("rendered report = %q, want %q", got, "version=v9.9.9\n")
	}
}

// TestDeliverSnapshot_TemplateRejectsUnparseableDocument confirms the template
// mode fails loudly rather than emitting a half-rendered report: it is the
// only mode that depends on the document parsing.
func TestDeliverSnapshot_TemplateRejectsUnparseableDocument(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "snapshot.tmpl")
	if err := os.WriteFile(tmpl, []byte("{{ .Metadata.version }}\n"), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}

	err := DeliverSnapshot(t.Context(), []byte("\tnot: [valid yaml"), SnapshotDelivery{
		Output:       filepath.Join(dir, "report.md"),
		TemplatePath: tmpl,
	})
	if err == nil {
		t.Fatal("DeliverSnapshot(unparseable) = nil error, want a parse failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error = %v, want code ErrCodeInternal", err)
	}
}

// TestAgentConfigMapTargetRejectsMalformedURI is the fail-before-mutate guard.
// The Job's ConfigMap target used to be accepted on the "cm://" prefix alone,
// so a typo like "cm://aicr-snapshot" (name, no namespace) was only caught by
// the in-pod writer — after RBAC and the Job had been created. With Cleanup
// false (the zero value, and what SDK callers get unless they opt in) those
// resources, including a cluster-admin binding, stay behind.
func TestAgentConfigMapTargetRejectsMalformedURI(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"scheme only", "cm://"},
		{"namespace without name", "cm://aicr-snapshot"},
		{"empty namespace", "cm:///aicr-snapshot"},
		{"empty name", "cm://default/"},
		{"whitespace name", "cm://default/   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := agentConfigMapTarget(&AgentConfig{Namespace: "default", Output: tt.output}, "20260821-142233-9f3a1c0b7e2d4a55")
			if err == nil {
				t.Fatalf("agentConfigMapTarget(%q) = nil error, want rejection before any cluster access", tt.output)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestDeployAndCollectRejectsBeforeClusterAccess proves the rejections above
// happen before the Kubernetes client is built: with a kubeconfig that cannot
// be loaded, a valid-but-rejected config must still surface its own
// ErrCodeInvalidRequest rather than a kubeconfig failure. If the order were
// reversed, the documented contract would be masked whenever cluster access
// was also unavailable — and, worse, a reachable cluster would get RBAC and a
// Job before the input was ever checked.
func TestDeployAndCollectRejectsBeforeClusterAccess(t *testing.T) {
	badKubeconfig := filepath.Join(t.TempDir(), "does-not-exist.kubeconfig")

	tests := []struct {
		name    string
		config  *AgentConfig
		wantMsg string
	}{
		{
			name: "malformed ConfigMap output",
			config: &AgentConfig{
				Namespace:  "default",
				Kubeconfig: badKubeconfig,
				Output:     "cm://aicr-snapshot",
			},
			wantMsg: "invalid ConfigMap output URI",
		},
		{
			name: "cluster-config path in Job mode",
			config: &AgentConfig{
				Namespace:         "default",
				Kubeconfig:        badKubeconfig,
				ClusterConfigPath: "/host/cluster-config.yaml",
			},
			wantMsg: "--cluster-config is not supported in agent Job mode",
		},
		{
			// Empty Namespace would build the invalid internal URI
			// "cm:///aicr-snapshot", which only fails when it is finally
			// parsed — after RBAC and the Job exist. The CLI always supplies
			// a default, so this is the SDK-caller path.
			name: "empty namespace",
			config: &AgentConfig{
				Kubeconfig: badKubeconfig,
			},
			wantMsg: "Namespace is required",
		},
		{
			name: "whitespace-only namespace",
			config: &AgentConfig{
				Namespace:  "   ",
				Kubeconfig: badKubeconfig,
			},
			wantMsg: "Namespace is required",
		},
		{
			// Ruling 5 mitigation: nameWithRunID (pkg/k8s/agent) silently
			// falls back to unscoped, collision-prone names when RunID is
			// empty. A caller-supplied all-whitespace RunID bypasses the
			// simple "== \"\"" default check, so it must fail closed here
			// rather than reach that fallback deep in agent.Deploy.
			name: "whitespace-only RunID",
			config: &AgentConfig{
				Namespace:  "default",
				Kubeconfig: badKubeconfig,
				RunID:      "   ",
			},
			wantMsg: "RunID must not be all-whitespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := DeployAndCollect(t.Context(), tt.config)
			if err == nil {
				t.Fatal("DeployAndCollect() = nil error, want rejection")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want it to mention %q (a kubeconfig failure here means the "+
					"input check ran too late)", err, tt.wantMsg)
			}
		})
	}
}

// TestDeployAndCollectGeneratesRunIDWithoutMutatingConfig confirms
// DeployAndCollect resolves an empty RunID to a freshly generated one before
// it is folded into the internal staging ConfigMap's name, AND that it leaves
// the caller's *AgentConfig untouched while doing so.
//
// The non-mutation half is the load-bearing one. Writing the generated ID
// back into the caller's config would turn a caller who reuses one config
// pointer for a second run into a caller pinning a duplicate RunID — the one
// state ADR-020 declares unsupported — and run 2 would hard-fail
// ErrCodeInternal on the first still-existing run-scoped object. In-tree
// callers reach this through the pkg/client/v1 facade, which builds a fresh
// internal config per call, but pkg/snapshotter is public.
//
// The generated value is observed through the "snapshot agent run" log line,
// which is emitted just before the malformed-Output rejection below fires;
// there is no other seam that does not require cluster access.
func TestDeployAndCollectGeneratesRunIDWithoutMutatingConfig(t *testing.T) {
	runIDPattern := regexp.MustCompile(`runID=(\d{8}-\d{6}-[0-9a-f]{16})`)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	cfg := &AgentConfig{
		Namespace:  "default",
		Kubeconfig: filepath.Join(t.TempDir(), "does-not-exist.kubeconfig"),
		Output:     "cm://aicr-snapshot", // malformed: no namespace — rejected after RunID resolution
	}
	if cfg.RunID != "" {
		t.Fatalf("test precondition: cfg.RunID = %q, want empty", cfg.RunID)
	}

	_, _, err := DeployAndCollect(t.Context(), cfg)
	if err == nil {
		t.Fatal("DeployAndCollect() = nil error, want rejection from the malformed Output")
	}

	if !runIDPattern.MatchString(logs.String()) {
		t.Errorf("no generated run ID matching %s in log output; DeployAndCollect must "+
			"resolve an empty RunID with runid.Generate(). Logs:\n%s", runIDPattern, logs.String())
	}

	if cfg.RunID != "" {
		t.Errorf("cfg.RunID = %q after DeployAndCollect, want it left empty — the generated ID "+
			"must stay in a local so a caller reusing this config for a second run does not "+
			"silently pin a duplicate RunID", cfg.RunID)
	}
}

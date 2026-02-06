// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"testing"

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

func TestAgentConfig_Defaults(t *testing.T) {
	// Test that AgentConfig can be instantiated with zero values
	cfg := AgentConfig{}

	if cfg.Enabled {
		t.Error("AgentConfig.Enabled should default to false")
	}
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

func TestAgentOutputURILogic(t *testing.T) {
	// Test the logic for determining agentOutput based on user's finalOutput
	// This tests the rules:
	// 1. If user specifies a file path, agent uses default ConfigMap in agent's namespace
	// 2. If user specifies a ConfigMap URI, agent uses that URI
	// 3. If user specifies stdout, agent uses default ConfigMap in agent's namespace

	tests := []struct {
		name               string
		agentNamespace     string
		userOutput         string
		wantAgentOutputHas string // substring that should be in agentOutput
		wantUsesUserOutput bool   // whether agentOutput should equal userOutput
	}{
		{
			name:               "file output uses default ConfigMap with agent namespace",
			agentNamespace:     "gpu-operator",
			userOutput:         "snapshot.yaml",
			wantAgentOutputHas: "cm://gpu-operator/eidos-snapshot",
			wantUsesUserOutput: false,
		},
		{
			name:               "stdout uses default ConfigMap with agent namespace",
			agentNamespace:     "gpu-operator",
			userOutput:         "",
			wantAgentOutputHas: "cm://gpu-operator/eidos-snapshot",
			wantUsesUserOutput: false,
		},
		{
			name:               "dash stdout uses default ConfigMap with agent namespace",
			agentNamespace:     "gpu-operator",
			userOutput:         "-",
			wantAgentOutputHas: "cm://gpu-operator/eidos-snapshot",
			wantUsesUserOutput: false,
		},
		{
			name:               "ConfigMap URI uses user's URI",
			agentNamespace:     "gpu-operator",
			userOutput:         "cm://custom-ns/my-snapshot",
			wantAgentOutputHas: "cm://custom-ns/my-snapshot",
			wantUsesUserOutput: true,
		},
		{
			name:               "custom namespace uses that namespace for default ConfigMap",
			agentNamespace:     "custom-namespace",
			userOutput:         "output.yaml",
			wantAgentOutputHas: "cm://custom-namespace/eidos-snapshot",
			wantUsesUserOutput: false,
		},
	}

	const configMapURIScheme = "cm://"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the logic from measureWithAgent
			finalOutput := tt.userOutput
			agentOutput := configMapURIScheme + tt.agentNamespace + "/eidos-snapshot"

			hasConfigMapPrefix := len(finalOutput) >= len(configMapURIScheme) &&
				finalOutput[:len(configMapURIScheme)] == configMapURIScheme
			if hasConfigMapPrefix {
				agentOutput = finalOutput
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
		})
	}
}

func TestAgentConfigWithTemplatePath(t *testing.T) {
	// Test that AgentConfig can hold TemplatePath
	cfg := AgentConfig{
		Enabled:      true,
		Namespace:    "gpu-operator",
		TemplatePath: "/path/to/template.tmpl",
		Output:       "output.yaml",
	}

	if cfg.TemplatePath != "/path/to/template.tmpl" {
		t.Errorf("AgentConfig.TemplatePath = %q, want %q", cfg.TemplatePath, "/path/to/template.tmpl")
	}
}

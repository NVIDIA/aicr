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

package validators

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestResolveNamespace(t *testing.T) {
	tests := []struct {
		name     string
		envKey   string
		envVal   string
		expected string
	}{
		{
			name:     "no env var returns default",
			expected: "default",
		},
		{
			name:     "AICR_NAMESPACE set returns its value",
			envKey:   "AICR_NAMESPACE",
			envVal:   "gpu-validation",
			expected: "gpu-validation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear AICR_NAMESPACE for every subtest to ensure isolation.
			t.Setenv("AICR_NAMESPACE", "")

			if tt.envKey != "" {
				t.Setenv(tt.envKey, tt.envVal)
			}

			got := resolveNamespace()
			if got != tt.expected {
				t.Errorf("resolveNamespace() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestParseNodeSelectorEnv(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		expected map[string]string
		wantErr  bool
	}{
		{
			name:     "env var unset returns nil",
			envVal:   "",
			expected: nil,
		},
		{
			name:     "single key=value pair",
			envVal:   "my-org/gpu-pool=true",
			expected: map[string]string{"my-org/gpu-pool": "true"},
		},
		{
			name:     "multiple key=value pairs",
			envVal:   "accelerator=h100,pool=gpu",
			expected: map[string]string{"accelerator": "h100", "pool": "gpu"},
		},
		{
			name:    "invalid format missing equals",
			envVal:  "invalidkey",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_NODE_SELECTOR", tt.envVal)
			got, err := parseNodeSelectorEnv()
			if (err != nil) != tt.wantErr {
				t.Errorf("parseNodeSelectorEnv() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.expected) {
					t.Errorf("parseNodeSelectorEnv() = %v, want %v", got, tt.expected)
					return
				}
				for k, want := range tt.expected {
					if got[k] != want {
						t.Errorf("parseNodeSelectorEnv()[%q] = %q, want %q", k, got[k], want)
					}
				}
			}
		})
	}
}

func TestParseTolerationEnv(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		expected []corev1.Toleration
		wantErr  bool
	}{
		{
			name:     "env var unset returns nil",
			envVal:   "",
			expected: nil,
		},
		{
			name:   "key=value:effect toleration",
			envVal: "gpu-type=h100:NoSchedule",
			expected: []corev1.Toleration{
				{Key: "gpu-type", Value: "h100", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual},
			},
		},
		{
			name:   "key:effect toleration (exists operator)",
			envVal: "dedicated:NoExecute",
			expected: []corev1.Toleration{
				{Key: "dedicated", Effect: corev1.TaintEffectNoExecute, Operator: corev1.TolerationOpExists},
			},
		},
		{
			name:    "invalid format",
			envVal:  "badformat",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_TOLERATIONS", tt.envVal)
			got, err := parseTolerationEnv()
			if (err != nil) != tt.wantErr {
				t.Errorf("parseTolerationEnv() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.expected) {
					t.Errorf("parseTolerationEnv() = %v, want %v", got, tt.expected)
					return
				}
				for i, want := range tt.expected {
					if got[i].Key != want.Key || got[i].Value != want.Value ||
						got[i].Effect != want.Effect || got[i].Operator != want.Operator {
						t.Errorf("parseTolerationEnv()[%d] = %+v, want %+v", i, got[i], want)
					}
				}
			}
		})
	}
}

func TestEnvOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		setVal   string
		setEnv   bool
		fallback string
		expected string
	}{
		{
			name:     "env var set returns its value",
			key:      "AICR_TEST_ENV_OR_DEFAULT",
			setVal:   "custom-value",
			setEnv:   true,
			fallback: "fallback-value",
			expected: "custom-value",
		},
		{
			name:     "env var not set returns fallback",
			key:      "AICR_TEST_ENV_OR_DEFAULT_UNSET",
			setEnv:   false,
			fallback: "fallback-value",
			expected: "fallback-value",
		},
		{
			name:     "env var set to empty returns fallback",
			key:      "AICR_TEST_ENV_OR_DEFAULT_EMPTY",
			setVal:   "",
			setEnv:   true,
			fallback: "fallback-value",
			expected: "fallback-value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(tt.key, tt.setVal)
			}

			got := envOrDefault(tt.key, tt.fallback)
			if got != tt.expected {
				t.Errorf("envOrDefault(%q, %q) = %q, want %q", tt.key, tt.fallback, got, tt.expected)
			}
		})
	}
}

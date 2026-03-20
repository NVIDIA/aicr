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

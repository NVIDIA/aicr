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

package evidence

import (
	"testing"
)

func TestResolveFeature(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"canonical passthrough", "dra-support", "dra-support"},
		{"canonical passthrough gang", "gang-scheduling", "gang-scheduling"},
		{"canonical passthrough cluster", "cluster-autoscaling", "cluster-autoscaling"},
		{"alias dra", "dra", "dra-support"},
		{"alias gang", "gang", "gang-scheduling"},
		{"alias secure", "secure", "secure-access"},
		{"alias metrics", "metrics", "accelerator-metrics"},
		{"alias gateway", "gateway", "inference-gateway"},
		{"alias operator", "operator", "robust-operator"},
		{"alias hpa", "hpa", "pod-autoscaling"},
		{"unknown passthrough", "unknown-feature", "unknown-feature"},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ResolveFeature(tt.input)
			if result != tt.expected {
				t.Errorf("ResolveFeature(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestScriptSection(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"dra-support", "dra-support", "dra"},
		{"gang-scheduling", "gang-scheduling", "gang"},
		{"secure-access", "secure-access", "secure"},
		{"accelerator-metrics", "accelerator-metrics", "metrics"},
		{"inference-gateway", "inference-gateway", "gateway"},
		{"robust-operator", "robust-operator", "operator"},
		{"pod-autoscaling", "pod-autoscaling", "hpa"},
		{"cluster-autoscaling", "cluster-autoscaling", "cluster-autoscaling"},
		{"all passthrough", "all", "all"},
		{"unknown passthrough", "unknown", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ScriptSection(tt.input)
			if result != tt.expected {
				t.Errorf("ScriptSection(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsValidFeature(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid canonical", "dra-support", true},
		{"valid canonical gang", "gang-scheduling", true},
		{"valid canonical cluster", "cluster-autoscaling", true},
		{"valid alias dra", "dra", true},
		{"valid alias hpa", "hpa", true},
		{"valid all", "all", true},
		{"invalid", "typo", false},
		{"invalid empty", "", false},
		{"invalid partial", "gang-sched", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidFeature(tt.input)
			if result != tt.expected {
				t.Errorf("IsValidFeature(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNewCollector(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c := NewCollector("/tmp/out")
		if c.outputDir != "/tmp/out" {
			t.Errorf("outputDir = %q, want %q", c.outputDir, "/tmp/out")
		}
		if len(c.features) != 0 {
			t.Errorf("features = %v, want empty", c.features)
		}
		if c.noCleanup {
			t.Error("noCleanup = true, want false")
		}
		if c.kubeconfig != "" {
			t.Errorf("kubeconfig = %q, want empty", c.kubeconfig)
		}
	})

	t.Run("with options", func(t *testing.T) {
		c := NewCollector("/tmp/out",
			WithFeatures([]string{"dra", "gang"}),
			WithNoCleanup(true),
			WithKubeconfig("/path/to/kubeconfig"),
		)
		if len(c.features) != 2 {
			t.Errorf("features length = %d, want 2", len(c.features))
		}
		if !c.noCleanup {
			t.Error("noCleanup = false, want true")
		}
		if c.kubeconfig != "/path/to/kubeconfig" {
			t.Errorf("kubeconfig = %q, want %q", c.kubeconfig, "/path/to/kubeconfig")
		}
	})

	t.Run("empty kubeconfig not set", func(t *testing.T) {
		c := NewCollector("/tmp/out", WithKubeconfig(""))
		if c.kubeconfig != "" {
			t.Errorf("kubeconfig = %q, want empty", c.kubeconfig)
		}
	})
}

func TestFeatureDescriptionsComplete(t *testing.T) {
	for _, f := range ValidFeatures {
		if _, ok := FeatureDescriptions[f]; !ok {
			t.Errorf("ValidFeature %q missing from FeatureDescriptions", f)
		}
		if _, ok := featureToScript[f]; !ok {
			t.Errorf("ValidFeature %q missing from featureToScript", f)
		}
	}
}

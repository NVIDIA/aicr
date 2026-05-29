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

package bundler

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

func TestIndentBlock(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		prefix string
		want   string
	}{
		{"single line", "a", "  ", "  a"},
		{"multi line", "a\nb", "  ", "  a\n  b"},
		{"blank line preserved without trailing space", "a\n\nb", "  ", "  a\n\n  b"},
		{"trailing newline trimmed", "a\n", "  ", "  a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := indentBlock(tt.in, tt.prefix); got != tt.want {
				t.Errorf("indentBlock(%q, %q) = %q, want %q", tt.in, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestRenderReadinessGateManifest(t *testing.T) {
	testYAML := []byte(`apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`)
	manifest, err := renderReadinessGateManifest("gpu-operator", "ghcr.io/nvidia/aicr-gate:v1.2.3", testYAML)
	if err != nil {
		t.Fatalf("renderReadinessGateManifest: %v", err)
	}
	got := string(manifest)

	// Each required resource kind is present.
	for _, want := range []string{
		"kind: ServiceAccount",
		"kind: ClusterRole",
		"kind: ClusterRoleBinding",
		"kind: ConfigMap",
		"kind: Job",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}

	// Per-release resource names derive from the component name.
	if !strings.Contains(got, "name: gpu-operator-readiness-gate") {
		t.Errorf("manifest missing gate resource name:\n%s", got)
	}
	if !strings.Contains(got, "name: gpu-operator-readiness-bundle") {
		t.Errorf("manifest missing bundle ConfigMap name:\n%s", got)
	}

	// Namespace is left as a template token for the writer's render pass.
	if !strings.Contains(got, "namespace: {{ .Release.Namespace }}") {
		t.Errorf("manifest missing namespace template token:\n%s", got)
	}
	if !strings.Contains(got, "--namespace={{ .Release.Namespace }}") {
		t.Errorf("manifest missing gate --namespace arg token:\n%s", got)
	}

	// Image is baked in literally.
	if !strings.Contains(got, "image: ghcr.io/nvidia/aicr-gate:v1.2.3") {
		t.Errorf("manifest missing gate image:\n%s", got)
	}

	// ClusterPolicy read permission is granted.
	if !strings.Contains(got, "clusterpolicies") {
		t.Errorf("manifest missing clusterpolicies RBAC:\n%s", got)
	}

	// The chainsaw Test is embedded, indented under the ConfigMap data block.
	if !strings.Contains(got, "    kind: Test") {
		t.Errorf("manifest missing indented embedded chainsaw Test:\n%s", got)
	}
}

func TestRenderReadinessGateManifest_EmptyComponentName(t *testing.T) {
	if _, err := renderReadinessGateManifest("", "img:tag", []byte("x")); err == nil {
		t.Fatal("expected error for empty component name, got nil")
	}
}

func TestGateImage(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"explicit version", "v1.2.3", "ghcr.io/nvidia/aicr-gate:v1.2.3"},
		{"empty falls back to dev", "", "ghcr.io/nvidia/aicr-gate:dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &DefaultBundler{Config: config.NewConfig(config.WithVersion(tt.version))}
			if got := b.gateImage(); got != tt.want {
				t.Errorf("gateImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

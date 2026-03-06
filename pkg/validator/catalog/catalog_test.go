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

package catalog

import (
	"strings"
	"testing"
	"time"
)

func TestLoadEmbeddedCatalog(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if catalog.APIVersion != expectedAPIVersion {
		t.Errorf("APIVersion = %q, want %q", catalog.APIVersion, expectedAPIVersion)
	}
	if catalog.Kind != expectedKind {
		t.Errorf("Kind = %q, want %q", catalog.Kind, expectedKind)
	}
	if catalog.Metadata.Version != "1.0.0" {
		t.Errorf("Metadata.Version = %q, want %q", catalog.Metadata.Version, "1.0.0")
	}
	if len(catalog.Validators) == 0 {
		t.Fatal("expected at least one validator in embedded catalog")
	}
	if catalog.Validators[0].Name != "operator-health" {
		t.Errorf("first validator name = %q, want %q", catalog.Validators[0].Name, "operator-health")
	}
}

func TestParseValidCatalog(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: test-catalog
  version: "1.0.0"
validators:
  - name: gpu-operator-health
    phase: deployment
    description: "Check GPU operator"
    image: ghcr.io/nvidia/aicr-validators/gpu-operator:v1.0.0
    timeout: 2m
    args: []
    env: []
  - name: nccl-bandwidth
    phase: performance
    description: "NCCL bandwidth test"
    image: ghcr.io/nvidia/aicr-validators/nccl:v1.0.0
    timeout: 10m
    args:
      - "--min-bw=100"
    env:
      - name: NCCL_DEBUG
        value: WARN
  - name: dra-support
    phase: conformance
    description: "DRA support check"
    image: ghcr.io/nvidia/aicr-validators/dra:v1.0.0
    timeout: 5m
    args: []
    env: []
`)

	catalog, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(catalog.Validators) != 3 {
		t.Fatalf("Validators length = %d, want 3", len(catalog.Validators))
	}

	v := catalog.Validators[0]
	if v.Name != "gpu-operator-health" {
		t.Errorf("Validators[0].Name = %q, want %q", v.Name, "gpu-operator-health")
	}
	if v.Phase != "deployment" {
		t.Errorf("Validators[0].Phase = %q, want %q", v.Phase, "deployment")
	}
	if v.Timeout != 2*time.Minute {
		t.Errorf("Validators[0].Timeout = %v, want %v", v.Timeout, 2*time.Minute)
	}

	v1 := catalog.Validators[1]
	if len(v1.Args) != 1 || v1.Args[0] != "--min-bw=100" {
		t.Errorf("Validators[1].Args = %v, want [--min-bw=100]", v1.Args)
	}
	if len(v1.Env) != 1 || v1.Env[0].Name != "NCCL_DEBUG" || v1.Env[0].Value != "WARN" {
		t.Errorf("Validators[1].Env = %v, want [{NCCL_DEBUG WARN}]", v1.Env)
	}
}

func TestForPhase(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: v1
    phase: deployment
    image: img1:latest
  - name: v2
    phase: deployment
    image: img2:latest
  - name: v3
    phase: performance
    image: img3:latest
  - name: v4
    phase: conformance
    image: img4:latest
`)

	catalog, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	tests := []struct {
		phase    string
		expected int
	}{
		{"deployment", 2},
		{"performance", 1},
		{"conformance", 1},
		{"nonexistent", 0},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			got := catalog.ForPhase(tt.phase)
			if len(got) != tt.expected {
				t.Errorf("ForPhase(%q) returned %d entries, want %d", tt.phase, len(got), tt.expected)
			}
		})
	}
}

func TestForPhaseNoMatch(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	got := catalog.ForPhase("nonexistent")
	if len(got) != 0 {
		t.Errorf("ForPhase(nonexistent) returned %d entries, want 0", len(got))
	}
}

func TestParseInvalidAPIVersion(t *testing.T) {
	data := []byte(`
apiVersion: wrong/v1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators: []
`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for invalid apiVersion")
	}
}

func TestParseInvalidKind(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: WrongKind
metadata:
  name: test
  version: "1.0.0"
validators: []
`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for invalid kind")
	}
}

func TestParseDuplicateNames(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: same-name
    phase: deployment
    image: img1:latest
  - name: same-name
    phase: conformance
    image: img2:latest
`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for duplicate names")
	}
}

func TestParseEmptyName(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: ""
    phase: deployment
    image: img:latest
`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestParseInvalidPhase(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: v1
    phase: readiness
    image: img:latest
`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for invalid phase 'readiness'")
	}
}

func TestParseEmptyImage(t *testing.T) {
	data := []byte(`
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: v1
    phase: deployment
    image: ""
`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for empty image")
	}
}

func TestParseInvalidYAML(t *testing.T) {
	data := []byte(`not: valid: yaml: [`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestReplaceRegistry(t *testing.T) {
	tests := []struct {
		name        string
		image       string
		newRegistry string
		expected    string
	}{
		{
			name:        "3-part image replaces registry and org",
			image:       "ghcr.io/nvidia/aicr-validators/deployment:latest",
			newRegistry: "localhost:5001",
			expected:    "localhost:5001/aicr-validators/deployment:latest",
		},
		{
			name:        "2-part image replaces registry",
			image:       "registry.io/image:tag",
			newRegistry: "newregistry",
			expected:    "newregistry/image:tag",
		},
		{
			name:        "1-part image returns unchanged",
			image:       "image",
			newRegistry: "localhost:5001",
			expected:    "image",
		},
		{
			name:        "empty override still applied on 3-part",
			image:       "ghcr.io/nvidia/aicr-validators/deployment:latest",
			newRegistry: "",
			expected:    "/aicr-validators/deployment:latest",
		},
		{
			name:        "3-part image with nested path",
			image:       "ghcr.io/nvidia/aicr-validators/sub/path:v1.0.0",
			newRegistry: "myregistry.io",
			expected:    "myregistry.io/aicr-validators/sub/path:v1.0.0",
		},
		{
			name:        "2-part image with tag and digest",
			image:       "registry.io/myimage@sha256:abc123",
			newRegistry: "other.io",
			expected:    "other.io/myimage@sha256:abc123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceRegistry(tt.image, tt.newRegistry)
			if got != tt.expected {
				t.Errorf("replaceRegistry(%q, %q) = %q, want %q", tt.image, tt.newRegistry, got, tt.expected)
			}
		})
	}
}

func TestLoadWithRegistryOverride(t *testing.T) {
	t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "localhost:5001")

	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for i, v := range cat.Validators {
		if !strings.HasPrefix(v.Image, "localhost:5001/") {
			t.Errorf("Validators[%d].Image = %q, want prefix %q", i, v.Image, "localhost:5001/")
		}
	}
}

func TestLoadWithoutRegistryOverride(t *testing.T) {
	t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")

	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for i, v := range cat.Validators {
		if strings.HasPrefix(v.Image, "localhost:5001/") {
			t.Errorf("Validators[%d].Image should not have localhost prefix: %q", i, v.Image)
		}
	}
}

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
)

func TestLoadEmbeddedCatalog(t *testing.T) {
	catalog, err := Load("", "")
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
	catalog, err := Load("", "")
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

	cat, err := Load("", "")
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

	cat, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for i, v := range cat.Validators {
		if strings.HasPrefix(v.Image, "localhost:5001/") {
			t.Errorf("Validators[%d].Image should not have localhost prefix: %q", i, v.Image)
		}
	}
}

func TestLoadWithExternalCatalog(t *testing.T) {
	// Isolate from caller's shell so the image-equality assertions below
	// compare against the default-resolution path, not an override.
	t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")
	t.Setenv("AICR_VALIDATOR_IMAGE_TAG", "")

	// Create external data directory with registry.yaml (required) and catalog
	tmpDir := t.TempDir()

	registryContent := `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components: []
`
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	validatorsDir := filepath.Join(tmpDir, "validators")
	if err := os.MkdirAll(validatorsDir, 0755); err != nil {
		t.Fatalf("failed to create validators dir: %v", err)
	}

	externalCatalog := `apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: external-validators
  version: "1.0.0"
validators:
  - name: dynamo-cluster-check
    phase: conformance
    description: "Custom Dynamo cluster validation"
    image: example.com/dynamo/validators:v1.0.0
    timeout: 5m
    args: ["dynamo-cluster"]
    env: []
`
	if err := os.WriteFile(filepath.Join(validatorsDir, "catalog.yaml"), []byte(externalCatalog), 0600); err != nil {
		t.Fatalf("failed to write catalog.yaml: %v", err)
	}

	// Set up layered provider
	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Save and restore global provider
	originalProvider := recipe.GetDataProvider()
	recipe.SetDataProvider(layered)
	defer recipe.SetDataProvider(originalProvider)

	// Load catalog — should merge embedded + external
	cat, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Should contain the new external validator
	found := false
	for _, v := range cat.Validators {
		if v.Name == "dynamo-cluster-check" {
			found = true
			if v.Image != "example.com/dynamo/validators:v1.0.0" {
				t.Errorf("dynamo-cluster-check image = %q, want %q", v.Image, "example.com/dynamo/validators:v1.0.0")
			}
			if v.Phase != "conformance" {
				t.Errorf("dynamo-cluster-check phase = %q, want %q", v.Phase, "conformance")
			}
			break
		}
	}
	if !found {
		t.Error("expected dynamo-cluster-check from external catalog")
	}

	// Should still contain embedded validators
	foundEmbedded := false
	for _, v := range cat.Validators {
		if v.Name == "operator-health" {
			foundEmbedded = true
			break
		}
	}
	if !foundEmbedded {
		t.Error("expected operator-health from embedded catalog")
	}
}

// TestResolveImageCIContract verifies that:
//  1. .goreleaser.yaml injects FullCommit (not ShortCommit) so the CLI has a
//     40-char SHA matching on-push.yaml's image tags.
//  2. ResolveImage produces the correct :sha-<commit> tag with a full SHA.
func TestResolveImageCIContract(t *testing.T) {
	t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")
	// Isolate from caller's shell — AICR_VALIDATOR_IMAGE_TAG rewrites the
	// resolved tag and would otherwise turn this contract test's default
	// resolution into an override-driven path during dogfooding.
	t.Setenv("AICR_VALIDATOR_IMAGE_TAG", "")

	// Guard: .goreleaser.yaml must use FullCommit for both aicr and aicrd.
	data, err := os.ReadFile("../../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("failed to read .goreleaser.yaml: %v", err)
	}
	for _, want := range []string{
		"pkg/cli.commit={{.FullCommit}}",
		"pkg/api.commit={{.FullCommit}}",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("goreleaser must inject FullCommit so :sha-<commit> matches on-push.yaml; missing %q", want)
		}
	}

	// Verify ResolveImage produces the expected tag with a full 40-char SHA.
	const img = "ghcr.io/nvidia/aicr-validators/deployment:latest"
	fullCommit := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

	got := ResolveImage(img, "dev", fullCommit)
	want := "ghcr.io/nvidia/aicr-validators/deployment:sha-" + fullCommit
	if got != want {
		t.Fatalf("ResolveImage with full SHA:\n  got  %q\n  want %q", got, want)
	}
}

func TestResolveImage(t *testing.T) {
	const imgLatest = "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest"
	const imgPinned = "ghcr.io/nvidia/aicr-validators/aiperf-bench:v1.2.3"

	tests := []struct {
		name     string
		image    string
		version  string
		commit   string
		registry string // if non-empty, sets AICR_VALIDATOR_IMAGE_REGISTRY for the test
		tag      string // if non-empty, sets AICR_VALIDATOR_IMAGE_TAG for the test
		want     string
	}{
		{
			name:    "dev version — no tag rewrite, no registry override",
			image:   imgLatest,
			version: "dev",
			want:    imgLatest,
		},
		{
			name:    "-next version — no tag rewrite",
			image:   imgLatest,
			version: "v0.11.1-next",
			want:    imgLatest,
		},
		{
			name:    "git-describe snapshot is not a release",
			image:   imgLatest,
			version: "v0.0.0-12-gabc1234",
			want:    imgLatest,
		},
		{
			name:    "pre-release rc suffix is not a release",
			image:   imgLatest,
			version: "v1.0.0-rc1",
			want:    imgLatest,
		},
		{
			name:    "snapshot with valid commit resolves to sha tag",
			image:   imgLatest,
			version: "v0.0.0-12-gabc1234",
			commit:  "abc1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:sha-abc1234",
		},
		{
			name:    "release version rewrites :latest to :vX.Y.Z",
			image:   imgLatest,
			version: "v0.11.1",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v0.11.1",
		},
		{
			name:    "release version with no leading v still produces :vX.Y.Z",
			image:   imgLatest,
			version: "0.11.1",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v0.11.1",
		},
		{
			name:    "explicit version tag is never overwritten",
			image:   imgPinned,
			version: "v0.11.1",
			want:    imgPinned,
		},
		{
			name:     "registry override replaces ghcr.io/nvidia prefix",
			image:    imgLatest,
			version:  "dev",
			registry: "localhost:5001",
			want:     "localhost:5001/aicr-validators/aiperf-bench:latest",
		},
		{
			name:     "version rewrite and registry override compose",
			image:    imgLatest,
			version:  "v0.11.1",
			registry: "localhost:5001",
			want:     "localhost:5001/aicr-validators/aiperf-bench:v0.11.1",
		},
		{
			name:    "dev version with valid commit resolves to sha tag",
			image:   imgLatest,
			version: "dev",
			commit:  "abc1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:sha-abc1234",
		},
		{
			name:    "dev version with unknown commit keeps latest",
			image:   imgLatest,
			version: "dev",
			commit:  "unknown",
			want:    imgLatest,
		},
		{
			name:    "dev version with empty commit keeps latest",
			image:   imgLatest,
			version: "dev",
			commit:  "",
			want:    imgLatest,
		},
		{
			name:    "dev version with too-short commit keeps latest",
			image:   imgLatest,
			version: "dev",
			commit:  "abc12",
			want:    imgLatest,
		},
		{
			name:    "dev version with 40-char full SHA resolves",
			image:   imgLatest,
			version: "dev",
			commit:  "abcdef1234abcdef1234abcdef1234abcdef1234", // exactly 40 hex chars
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:sha-abcdef1234abcdef1234abcdef1234abcdef1234",
		},
		{
			name:    "dev version with 41-char commit keeps latest",
			image:   imgLatest,
			version: "dev",
			commit:  "abcdef1234abcdef1234abcdef1234abcdef12345", // 41 hex chars
			want:    imgLatest,
		},
		{
			name:    "dev version with non-hex commit keeps latest",
			image:   imgLatest,
			version: "dev",
			commit:  "xyz1234",
			want:    imgLatest,
		},
		{
			name:    "-next version with valid commit resolves to sha tag",
			image:   imgLatest,
			version: "v0.11.1-next",
			commit:  "abc1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:sha-abc1234",
		},
		{
			name:    "release version ignores commit (release takes precedence)",
			image:   imgLatest,
			version: "v0.11.1",
			commit:  "abc1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v0.11.1",
		},
		{
			name:    "explicit tag not modified by commit",
			image:   imgPinned,
			version: "dev",
			commit:  "abc1234",
			want:    imgPinned,
		},
		{
			name:     "dev + commit + registry override compose",
			image:    imgLatest,
			version:  "dev",
			commit:   "abc1234",
			registry: "localhost:5001",
			want:     "localhost:5001/aicr-validators/aiperf-bench:sha-abc1234",
		},
		{
			name:    "uppercase commit is normalized to lowercase",
			image:   imgLatest,
			version: "dev",
			commit:  "ABC1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:sha-abc1234",
		},
		// --- AICR_VALIDATOR_IMAGE_TAG escape hatch -----------------------
		// Motivating scenario: a contributor building aicr from an
		// un-merged feature-branch checkout. The commit isn't on main,
		// so on-push.yaml never pushed :sha-<commit> to ghcr, and
		// `aicr validate` pod ImagePullBackOffs. Setting
		// AICR_VALIDATOR_IMAGE_TAG=latest (or any published tag) forces
		// every validator image to a reachable tag without losing the
		// registry / version-based resolution as the default.
		{
			name:    "tag override rewrites :latest on dev build (feature-branch dogfooding)",
			image:   imgLatest,
			version: "dev",
			commit:  "abc1234",
			tag:     "latest",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest",
		},
		{
			name:    "tag override replaces :sha-<commit> when both resolve",
			image:   imgLatest,
			version: "v0.11.1-next",
			commit:  "abc1234",
			tag:     "latest",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest",
		},
		{
			name:    "tag override replaces the release :vX.Y.Z tag",
			image:   imgLatest,
			version: "v0.11.1",
			tag:     "latest",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest",
		},
		{
			name:    "tag override replaces an explicit catalog tag",
			image:   imgPinned, // :v1.2.3 is normally untouched
			version: "v0.11.1",
			tag:     "latest",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest",
		},
		{
			name:    "tag override with no tag on image appends the tag",
			image:   "ghcr.io/nvidia/aicr-validators/aiperf-bench",
			version: "dev",
			tag:     "latest",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest",
		},
		{
			name:    "empty tag env var leaves image untouched (no-op)",
			image:   imgPinned,
			version: "v0.11.1",
			tag:     "", // explicitly empty — should behave like unset
			want:    imgPinned,
		},
		{
			name:    "tag override preserves registry port (localhost:5001 edge case)",
			image:   "localhost:5001/aicr-validators/aiperf-bench:sha-abc1234",
			version: "dev",
			commit:  "abc1234",
			tag:     "latest",
			want:    "localhost:5001/aicr-validators/aiperf-bench:latest",
		},
		{
			name:     "tag override and registry override compose",
			image:    imgLatest,
			version:  "dev",
			commit:   "abc1234",
			tag:      "v0.11.0",
			registry: "localhost:5001",
			want:     "localhost:5001/aicr-validators/aiperf-bench:v0.11.0",
		},
		// --- digest-pinned refs must never be tag-rewritten --------------
		// A tag override is incompatible with a content-addressable digest
		// pin. Naive last-colon splitting would corrupt `@sha256:<hash>`
		// into `@sha256:<newTag>`, emitting an invalid reference.
		{
			name:    "digest-pinned image is not rewritten by tag override",
			image:   "ghcr.io/foo/bar@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			version: "dev",
			commit:  "abc1234",
			tag:     "latest",
			want:    "ghcr.io/foo/bar@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:    "mixed ref (name:tag@digest) is not rewritten by tag override",
			image:   "ghcr.io/foo/bar:v1.0.0@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			version: "dev",
			commit:  "abc1234",
			tag:     "latest",
			want:    "ghcr.io/foo/bar:v1.0.0@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			// Registry override still applies to digest refs (existing
			// replaceRegistry behavior), so compose test verifies the two
			// env vars don't step on each other on a digest-pinned image.
			name:     "digest ref + registry override: digest preserved, prefix replaced",
			image:    "ghcr.io/nvidia/aicr-validators/aiperf-bench@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			version:  "dev",
			commit:   "abc1234",
			tag:      "latest",
			registry: "localhost:5001",
			want:     "localhost:5001/aicr-validators/aiperf-bench@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.registry != "" {
				t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", tt.registry)
			} else {
				t.Setenv("AICR_VALIDATOR_IMAGE_REGISTRY", "")
			}
			t.Setenv("AICR_VALIDATOR_IMAGE_TAG", tt.tag)
			got := ResolveImage(tt.image, tt.version, tt.commit)
			if got != tt.want {
				t.Errorf("ResolveImage(%q, %q, %q) = %q, want %q", tt.image, tt.version, tt.commit, got, tt.want)
			}
		})
	}
}

// TestImagePullPolicy covers the shared helper that both the outer
// validator Deployer and the inner aiperf-bench Job call, so they stay in
// lockstep. The digest-pin case is the specific Codex P3 concern: forcing
// PullAlways on a digest-pinned ref (e.g. an external catalog entry that
// stayed `name@sha256:…`) would break disconnected/private clusters by
// making kubelet re-contact the registry every run, for no correctness
// benefit (the digest is cryptographically immutable).
func TestImagePullPolicy(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		envTag string // AICR_VALIDATOR_IMAGE_TAG — empty means unset
		want   corev1.PullPolicy
	}{
		// ----- side-loaded refs win unconditionally -----
		{name: "ko.local → Never", image: "ko.local/aicr-validators/x:latest", want: corev1.PullNever},
		{name: "kind.local → Never", image: "kind.local/aicr-validators/x:latest", want: corev1.PullNever},
		{name: "ko.local + override still Never", image: "ko.local/aicr-validators/x:edge", envTag: "edge", want: corev1.PullNever},
		// The side-load check must anchor on the full registry segment
		// (trailing slash) so a real registry like `ko.localhost:5000/...`
		// is not misread as `ko.local/...` and forced to PullNever —
		// kubelet would then be unable to pull from the real registry.
		{name: "ko.localhost:5000 registry → not treated as side-load", image: "ko.localhost:5000/aicr-validators/x:v1", want: corev1.PullIfNotPresent},
		{name: "kind.localhost:5000 registry → not treated as side-load", image: "kind.localhost:5000/aicr-validators/x:v1", want: corev1.PullIfNotPresent},

		// ----- digest pins are immutable → IfNotPresent -----
		{
			name:  "digest-only ref → IfNotPresent (immutable by construction)",
			image: "ghcr.io/foo/bar@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want:  corev1.PullIfNotPresent,
		},
		{
			// Codex P3: the tag override must NOT upgrade a digest ref to
			// PullAlways. Doing so would make disconnected/air-gapped
			// clusters re-contact the registry every run for no gain.
			name:   "digest-only ref + override → IfNotPresent (override does not apply)",
			image:  "ghcr.io/foo/bar@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			envTag: "latest",
			want:   corev1.PullIfNotPresent,
		},
		{
			name:  "mixed ref name:tag@digest → IfNotPresent (digest wins)",
			image: "ghcr.io/foo/bar:v1.0.0@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want:  corev1.PullIfNotPresent,
		},

		// ----- override forces Always on non-digest refs -----
		{
			name:   "override with :edge → Always (avoid stale cache on mutable tag)",
			image:  "ghcr.io/nvidia/aicr-validators/performance:edge",
			envTag: "edge",
			want:   corev1.PullAlways,
		},
		{
			name:   "override with release :v0.11.0 → Always (safe over-pull, not a regression)",
			image:  "ghcr.io/nvidia/aicr-validators/performance:v0.11.0",
			envTag: "v0.11.0",
			want:   corev1.PullAlways,
		},

		// ----- default policy (no override) -----
		{name: ":latest → Always", image: "ghcr.io/nvidia/aicr-validators/performance:latest", want: corev1.PullAlways},
		{name: ":vX.Y.Z → IfNotPresent", image: "ghcr.io/nvidia/aicr-validators/performance:v1.0.0", want: corev1.PullIfNotPresent},
		{name: ":sha-<commit> → IfNotPresent (main-branch dev default)", image: "ghcr.io/nvidia/aicr-validators/performance:sha-abc1234", want: corev1.PullIfNotPresent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICR_VALIDATOR_IMAGE_TAG", tt.envTag)
			if got := ImagePullPolicy(tt.image); got != tt.want {
				t.Errorf("ImagePullPolicy(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

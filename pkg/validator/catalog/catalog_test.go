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

package catalog

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"gopkg.in/yaml.v3"
)

// TestCatalogTimeoutsFitFacadeBudget asserts every catalog entry's per-check
// timeout stays below the facade-level ValidationOperationTimeout. Otherwise a
// controller/library caller (which gets that facade deadline when no explicit
// timeout is set) could hit the top-level "context deadline exceeded" before
// the per-check Job reaches its own deadline and emits a structured result.
func TestCatalogTimeoutsFitFacadeBudget(t *testing.T) {
	catalog, err := LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	for _, v := range catalog.Validators {
		if v.Timeout >= defaults.ValidationOperationTimeout {
			t.Errorf("validator %q timeout %v must be < facade ValidationOperationTimeout %v "+
				"(else the orchestration cap preempts the per-check structured result)",
				v.Name, v.Timeout, defaults.ValidationOperationTimeout)
		}
	}
}

func TestLoadEmbeddedCatalog(t *testing.T) {
	catalog, err := LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if catalog.APIVersion != v1.CatalogAPIVersion {
		t.Errorf("APIVersion = %q, want %q", catalog.APIVersion, v1.CatalogAPIVersion)
	}
	if catalog.Kind != v1.CatalogKind {
		t.Errorf("Kind = %q, want %q", catalog.Kind, v1.CatalogKind)
	}
	if catalog.Metadata == nil {
		t.Fatal("Metadata is nil")
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
apiVersion: validator.nvidia.com/v1alpha1
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
apiVersion: validator.nvidia.com/v1alpha1
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
		name     string
		phase    v1.Phase
		expected int
	}{
		{"deployment", v1.PhaseDeployment, 2},
		{"performance", v1.PhasePerformance, 1},
		{"conformance", v1.PhaseConformance, 1},
		{"nonexistent", v1.Phase("nonexistent"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := catalog.ForPhase(tt.phase)
			if len(got) != tt.expected {
				t.Errorf("ForPhase(%q) returned %d entries, want %d", tt.phase, len(got), tt.expected)
			}
		})
	}
}

func TestForPhaseNoMatch(t *testing.T) {
	catalog, err := LoadWithDataProvider(context.Background(), nil, "", "")
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
apiVersion: validator.nvidia.com/v1alpha1
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
apiVersion: validator.nvidia.com/v1alpha1
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
apiVersion: validator.nvidia.com/v1alpha1
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
apiVersion: validator.nvidia.com/v1alpha1
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
apiVersion: validator.nvidia.com/v1alpha1
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

	cat, err := LoadWithDataProvider(context.Background(), nil, "", "")
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

	cat, err := LoadWithDataProvider(context.Background(), nil, "", "")
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

	registryContent := `apiVersion: validator.nvidia.com/v1alpha1alpha1
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

	externalCatalog := `apiVersion: validator.nvidia.com/v1alpha1
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

	// Load catalog — should merge embedded + external
	cat, err := LoadWithDataProvider(context.Background(), layered, "", "")
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
		"pkg/server.commit={{.FullCommit}}",
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
			// Goreleaser snapshot template `{{ .Tag }}-next` is not a
			// published image tag — must fall through to :sha-<commit>
			// (the path on-push.yaml does publish). Regression guard for
			// issue #916: silently dropping the -next exclusion would
			// route every main-development build to an unpublished
			// :v0.X.Y-next tag.
			name:    "-next snapshot version — no tag rewrite",
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
			// on-tag.yaml publishes `:v1.0.0-rc1` for pre-release tags;
			// the binary's version is the same `.Tag` string goreleaser
			// stamps in, so they must resolve to the same image tag.
			// Regression guard for issue #916.
			name:    "pre-release rc tag rewrites :latest to :vX.Y.Z-rc1",
			image:   imgLatest,
			version: "v1.0.0-rc1",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v1.0.0-rc1",
		},
		{
			name:    "pre-release beta tag rewrites :latest to :vX.Y.Z-beta",
			image:   imgLatest,
			version: "v1.0.0-beta",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v1.0.0-beta",
		},
		{
			name:    "pre-release alpha.1 (dotted) tag rewrites :latest",
			image:   imgLatest,
			version: "v1.0.0-alpha.1",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v1.0.0-alpha.1",
		},
		{
			// Snapshot of a pre-release checkout: goreleaser snapshot
			// produces `v1.0.0-rc1-next`. The internal dash takes the
			// suffix out of the single-segment shape AND the trailing
			// -next is excluded — either guard alone is sufficient,
			// both are present for defense-in-depth.
			name:    "snapshot of a pre-release tag falls through",
			image:   imgLatest,
			version: "v1.0.0-rc1-next",
			want:    imgLatest,
		},
		{
			// Snapshot of stable release falls through to :sha-<commit>.
			name:    "snapshot of stable release with commit → sha tag",
			image:   imgLatest,
			version: "v1.0.0-next",
			commit:  "abc1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:sha-abc1234",
		},
		{
			// Pre-release version takes precedence over commit (same
			// as stable releases — the release workflow published the
			// tagged image, no need to chase :sha-<commit>).
			name:    "pre-release tag ignores commit (release wins)",
			image:   imgLatest,
			version: "v1.0.0-rc1",
			commit:  "abc1234",
			want:    "ghcr.io/nvidia/aicr-validators/aiperf-bench:v1.0.0-rc1",
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

func TestCatalogOmitEmpty(t *testing.T) {
	// Catalog without optional fields - should serialize cleanly
	catalog := &ValidatorCatalog{
		Validators: []ValidatorEntry{
			{
				Name:        "test-validator",
				Phase:       "deployment",
				Description: "Test validator",
				Image:       "test:latest",
				Timeout:     2 * time.Minute,
			},
		},
	}

	// Test YAML marshaling
	yamlData, err := yaml.Marshal(catalog)
	if err != nil {
		t.Fatalf("yaml.Marshal() failed: %v", err)
	}

	yamlStr := string(yamlData)
	// Verify optional fields are omitted
	if strings.Contains(yamlStr, "apiVersion:") {
		t.Error("YAML should not contain apiVersion when empty")
	}
	if strings.Contains(yamlStr, "kind:") {
		t.Error("YAML should not contain kind when empty")
	}
	if strings.Contains(yamlStr, "metadata:") {
		t.Error("YAML should not contain metadata when nil")
	}

	// But required field should be present
	if !strings.Contains(yamlStr, "validators:") {
		t.Error("YAML should contain validators")
	}

	// Test JSON marshaling
	jsonData, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	jsonStr := string(jsonData)
	// Verify optional fields are omitted in JSON
	if strings.Contains(jsonStr, "apiVersion") {
		t.Error("JSON should not contain apiVersion when empty")
	}
	if strings.Contains(jsonStr, "kind") {
		t.Error("JSON should not contain kind when empty")
	}
	if strings.Contains(jsonStr, "metadata") {
		t.Error("JSON should not contain metadata when nil")
	}

	// But required field should be present
	if !strings.Contains(jsonStr, "validators") {
		t.Error("JSON should contain validators")
	}
}

func TestParseDependencyAffinity(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantErr       bool
		wantErrSubstr string // when set, the error message must contain this
	}{
		{
			name: "valid dependencyAffinity required",
			yaml: `
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: ai-service-metrics
    phase: conformance
    description: Verify AI service metrics via Prometheus
    image: ghcr.io/nvidia/aicr-validators/conformance:latest
    timeout: 5m
    dependencyAffinity:
      - componentRef: kube-prometheus-stack
        podLabelSelector:
          app.kubernetes.io/name: prometheus
        requirement: required
`,
			wantErr: false,
		},
		{
			name: "valid dependencyAffinity preferred default requirement",
			yaml: `
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: ai-service-metrics
    phase: conformance
    description: x
    image: ghcr.io/x:latest
    timeout: 5m
    dependencyAffinity:
      - componentRef: kube-prometheus-stack
        podLabelSelector:
          app.kubernetes.io/name: prometheus
`,
			wantErr: false,
		},
		{
			name: "invalid empty componentRef",
			yaml: `
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: ai-service-metrics
    phase: conformance
    description: x
    image: ghcr.io/x:latest
    timeout: 5m
    dependencyAffinity:
      - podLabelSelector:
          app.kubernetes.io/name: prometheus
        requirement: required
`,
			wantErr:       true,
			wantErrSubstr: `componentRef is required`,
		},
		{
			name: "invalid empty selector",
			yaml: `
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: ai-service-metrics
    phase: conformance
    description: x
    image: ghcr.io/x:latest
    timeout: 5m
    dependencyAffinity:
      - componentRef: kube-prometheus-stack
        requirement: required
`,
			wantErr:       true,
			wantErrSubstr: `podLabelSelector is required`,
		},
		{
			name: "invalid requirement value",
			yaml: `
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: test
  version: "1.0.0"
validators:
  - name: ai-service-metrics
    phase: conformance
    description: x
    image: ghcr.io/x:latest
    timeout: 5m
    dependencyAffinity:
      - componentRef: kube-prometheus-stack
        podLabelSelector:
          app.kubernetes.io/name: prometheus
        requirement: soft
`,
			wantErr:       true,
			wantErrSubstr: `invalid requirement "soft"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Errorf("error message = %v, want substring %q", err, tt.wantErrSubstr)
				}
			}
		})
	}
}

func TestEmbeddedCatalog_AIServiceMetricsHasDependencyAffinity(t *testing.T) {
	// "-next" suffix bypasses the release-version image-tag rewrite path,
	// matching how goreleaser snapshots stamp dev binaries.
	cat, err := LoadWithDataProvider(context.Background(), nil, "v0.0.0-next", "")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	var entry *v1.ValidatorEntry
	for i, v := range cat.Validators {
		if v.Name == "ai-service-metrics" {
			entry = &cat.Validators[i]
			break
		}
	}
	if entry == nil {
		t.Fatal("ai-service-metrics not found in embedded catalog")
	}
	if len(entry.DependencyAffinity) != 1 {
		t.Fatalf("expected 1 dependencyAffinity, got %d", len(entry.DependencyAffinity))
	}
	dep := entry.DependencyAffinity[0]
	if dep.ComponentRef != "kube-prometheus-stack" {
		t.Errorf("componentRef = %q, want kube-prometheus-stack", dep.ComponentRef)
	}
	if dep.PodLabelSelector["app.kubernetes.io/name"] != "prometheus" {
		t.Errorf("podLabelSelector = %v, expected app.kubernetes.io/name=prometheus", dep.PodLabelSelector)
	}
	if dep.RequirementOrDefault() != v1.DependencyRequirementPreferred {
		t.Errorf("requirement = %q, want preferred", dep.RequirementOrDefault())
	}
}

// TestEmbeddedCatalog_InferencePerfEntryExists locks the embedded catalog entry
// name to the v1.InferencePerfCheckName constant that scopes HF_TOKEN forwarding
// (see buildValidatorEnv in pkg/validator/v1). Without this, renaming the
// "inference-perf" catalog entry would silently disable HF_TOKEN forwarding —
// the forwarding predicate would just never match — with no other test failing.
func TestEmbeddedCatalog_InferencePerfEntryExists(t *testing.T) {
	cat, err := LoadWithDataProvider(context.Background(), nil, "v0.0.0-next", "")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	for _, v := range cat.Validators {
		if v.Name == v1.InferencePerfCheckName {
			if v.Phase != "performance" {
				t.Errorf("%q phase = %q, want performance", v1.InferencePerfCheckName, v.Phase)
			}
			return
		}
	}
	t.Fatalf("no embedded catalog entry named %q (HF_TOKEN forwarding would silently no-op)", v1.InferencePerfCheckName)
}

func TestCatalogEmbedding(t *testing.T) {
	// Simulate embedding in a CR spec
	type ValidatorCatalogSpec struct {
		Catalog ValidatorCatalog `json:"catalog" yaml:"catalog"`
		Enabled bool             `json:"enabled" yaml:"enabled"`
	}

	spec := ValidatorCatalogSpec{
		Catalog: ValidatorCatalog{
			// No APIVersion/Kind/Metadata - clean embedding
			Validators: []ValidatorEntry{
				{
					Name:        "gpu-operator-health",
					Phase:       "deployment",
					Description: "Check GPU operator",
					Image:       "test:latest",
					Timeout:     2 * time.Minute,
				},
			},
		},
		Enabled: true,
	}

	// Test YAML marshaling
	yamlData, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("yaml.Marshal() failed: %v", err)
	}

	yamlStr := string(yamlData)
	// Verify clean embedding without resource metadata
	if strings.Contains(yamlStr, "apiVersion:") {
		t.Error("Embedded catalog should not contain apiVersion")
	}
	if strings.Contains(yamlStr, "kind:") {
		t.Error("Embedded catalog should not contain kind")
	}
	if strings.Contains(yamlStr, "metadata:") {
		t.Error("Embedded catalog should not contain metadata")
	}

	// Test JSON marshaling
	jsonData, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}

	jsonStr := string(jsonData)
	// Verify clean embedding without resource metadata in JSON
	if strings.Contains(jsonStr, "apiVersion") {
		t.Error("Embedded catalog should not contain apiVersion in JSON")
	}
	if strings.Contains(jsonStr, "kind") {
		t.Error("Embedded catalog should not contain kind in JSON")
	}
	if strings.Contains(jsonStr, "metadata") {
		t.Error("Embedded catalog should not contain metadata in JSON")
	}
}

// fakeCatalogProvider is a recipe.DataProvider that serves a single,
// distinct validators/catalog.yaml. It lets a test prove catalog.Load read
// from the provider it was handed rather than the package global: the
// embedded catalog has no validator by this name, so seeing it back is
// proof the explicit provider's bytes were used.
type fakeCatalogProvider struct {
	catalogYAML []byte
	reads       int
}

func (p *fakeCatalogProvider) ReadFile(_ context.Context, path string) ([]byte, error) {
	if path == "validators/catalog.yaml" {
		p.reads++
		return p.catalogYAML, nil
	}
	return nil, os.ErrNotExist
}

func (p *fakeCatalogProvider) WalkDir(context.Context, string, fs.WalkDirFunc) error { return nil }

func (p *fakeCatalogProvider) Source(path string) string { return "fake://" + path }

// TestLoadHonorsExplicitProvider proves LoadWithDataProvider reads the catalog
// from the DataProvider it is handed (not the package global) by serving a
// distinct catalog whose marker validator does not exist in the embedded data.
func TestLoadHonorsExplicitProvider(t *testing.T) {
	const marker = "explicit-provider-marker"
	dp := &fakeCatalogProvider{catalogYAML: []byte(`
apiVersion: validator.nvidia.com/v1alpha1
kind: ValidatorCatalog
metadata:
  name: fake-catalog
  version: "9.9.9"
validators:
  - name: ` + marker + `
    phase: deployment
    description: "marker validator unique to the fake provider"
    image: ghcr.io/nvidia/aicr-validators/marker:v9.9.9
    timeout: 1m
`)}

	cat, err := LoadWithDataProvider(context.Background(), dp, "", "")
	if err != nil {
		t.Fatalf("LoadWithDataProvider() error = %v, want nil", err)
	}
	if dp.reads == 0 {
		t.Error("LoadWithDataProvider() did not call ReadFile on the explicit provider")
	}
	if len(cat.Validators) != 1 || cat.Validators[0].Name != marker {
		t.Errorf("LoadWithDataProvider() returned %d validators (first = %q); want exactly the %q marker — "+
			"catalog was not read from the explicit provider",
			len(cat.Validators), firstValidatorName(cat), marker)
	}
}

func firstValidatorName(cat *ValidatorCatalog) string {
	if cat == nil || len(cat.Validators) == 0 {
		return ""
	}
	return cat.Validators[0].Name
}

// TestLoadDataProvider verifies LoadWithDataProvider honors an explicit
// DataProvider and falls back to the package-global provider when nil is passed.
func TestLoadDataProvider(t *testing.T) {
	tests := []struct {
		name string
		dp   recipe.DataProvider
	}{
		{
			name: "explicit embedded provider",
			dp:   recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "."),
		},
		{
			name: "nil falls back to global",
			dp:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := LoadWithDataProvider(context.Background(), tt.dp, "", "")
			if err != nil {
				t.Fatalf("LoadWithDataProvider() error = %v, want nil", err)
			}
			if cat == nil {
				t.Fatal("LoadWithDataProvider() returned nil catalog")
			}
			if len(cat.Validators) == 0 {
				t.Error("LoadWithDataProvider() returned catalog with no validators")
			}
		})
	}
}

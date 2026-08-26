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

package flux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testVersion = "v1.0.0"

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

func TestGenerate_Success(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		Version: "v0.9.0",
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if output == nil {
		t.Fatal("Generate() returned nil output")
	}

	if len(output.Files) == 0 {
		t.Error("Generate() returned no files")
	}

	if output.TotalSize == 0 {
		t.Error("Generate() returned zero total size")
	}

	if output.Duration == 0 {
		t.Error("Generate() returned zero duration")
	}

	// Verify expected files exist.
	expectedFiles := []string{
		"sources/helmrepo-charts-jetstack-io.yaml",
		"sources/helmrepo-helm-ngc-nvidia-com-nvidia.yaml",
		"cert-manager/helmrelease.yaml",
		"gpu-operator/helmrelease.yaml",
		"kustomization.yaml",
		"README.md",
	}

	for _, relPath := range expectedFiles {
		fullPath := filepath.Join(outputDir, relPath)
		if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
			t.Errorf("expected file %s does not exist", relPath)
		}
	}

	// We also expect a gitrepo source for the default repo.
	gitRepoFiles := listFilesWithPrefix(t, filepath.Join(outputDir, "sources"), "gitrepo-")
	if len(gitRepoFiles) == 0 {
		t.Error("expected at least one gitrepo source file")
	}

	// Verify generated HelmRelease files are valid YAML.
	assertValidYAML(t, filepath.Join(outputDir, "cert-manager", "helmrelease.yaml"))
	assertValidYAML(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))

	// Verify kustomization.yaml is valid YAML.
	assertValidYAML(t, filepath.Join(outputDir, "kustomization.yaml"))

	// Verify README contains component information.
	content := readFile(t, filepath.Join(outputDir, "README.md"))
	if !strings.Contains(content, "cert-manager") {
		t.Error("README should contain cert-manager")
	}
	if !strings.Contains(content, "gpu-operator") {
		t.Error("README should contain gpu-operator")
	}

	// Verify HTTPS sources strip v-prefix from version (SemVer matching in index.yaml).
	hrContent := readFile(t, filepath.Join(outputDir, "cert-manager", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "version: 1.17.2") {
		t.Errorf("HTTPS HelmRelease should strip v-prefix from version, got:\n%s", hrContent)
	}

	// Verify deployment steps.
	if len(output.DeploymentSteps) == 0 {
		t.Error("Generate() returned no deployment steps")
	}
}

func TestGenerate_NilRecipeResult(t *testing.T) {
	g := &Generator{
		Version: "v0.9.0",
	}
	ctx := context.Background()
	outputDir := t.TempDir()

	_, err := g.Generate(ctx, outputDir)
	if err == nil {
		t.Fatal("Generate() should return error for nil recipe result")
	}
}

func TestGenerate_EmptyComponents(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Should still generate root kustomization.yaml and README.
	expectedFiles := []string{
		"kustomization.yaml",
		"README.md",
	}

	for _, relPath := range expectedFiles {
		fullPath := filepath.Join(outputDir, relPath)
		if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
			t.Errorf("expected file %s does not exist", relPath)
		}
	}

	// Verify output has at least the root files + default gitrepo source.
	if len(output.Files) < 2 {
		t.Errorf("expected at least 2 files, got %d", len(output.Files))
	}
}

func TestGenerate_WithRepoURL(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	customRepoURL := "https://github.com/my-org/my-gitops-repo.git"

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
		RepoURL:      customRepoURL,
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify GitRepository source contains custom repo URL.
	gitRepoFiles := listFilesWithPrefix(t, filepath.Join(outputDir, "sources"), "gitrepo-")
	if len(gitRepoFiles) == 0 {
		t.Fatal("expected at least one gitrepo source file")
	}

	content := readFile(t, gitRepoFiles[0])
	if !strings.Contains(content, customRepoURL) {
		t.Errorf("GitRepository source should contain custom repo URL %s, got:\n%s", customRepoURL, content)
	}
}

func TestGenerate_WithOCIHelmRepo(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "oci://nvcr.io/nvidia",
		},
	}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify HelmRepository source has type: oci.
	helmRepoFiles := listFilesWithPrefix(t, filepath.Join(outputDir, "sources"), "helmrepo-")
	if len(helmRepoFiles) == 0 {
		t.Fatal("expected at least one helmrepo source file")
	}

	content := readFile(t, helmRepoFiles[0])
	if !strings.Contains(content, "type: oci") {
		t.Errorf("HelmRepository source should contain 'type: oci', got:\n%s", content)
	}
	if !strings.Contains(content, "oci://nvcr.io/nvidia") {
		t.Errorf("HelmRepository source should contain OCI URL, got:\n%s", content)
	}

	// Verify HelmRelease preserves v-prefix for OCI chart versions.
	// OCI tags are literal — stripping the v prefix produces a tag that
	// does not exist in the registry.
	hrContent := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "version: v25.3.3") {
		t.Errorf("OCI HelmRelease should preserve v-prefix in version, got:\n%s", hrContent)
	}
}

func TestGenerate_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	_, err := g.Generate(ctx, t.TempDir())
	if err == nil {
		t.Fatal("Generate() should return error for cancelled context")
	}
}

func TestGenerate_WithChecksums(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
	}

	g := &Generator{
		RecipeResult:     recipeResult,
		ComponentValues:  map[string]map[string]any{"cert-manager": {"crds": map[string]any{"enabled": true}}},
		Version:          "v0.9.0",
		IncludeChecksums: true,
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	checksumPath := filepath.Join(outputDir, "checksums.txt")
	if _, statErr := os.Stat(checksumPath); os.IsNotExist(statErr) {
		t.Error("checksums.txt should exist when IncludeChecksums is true")
	}
}

func TestGenerate_SourceDeduplication(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	// Two components sharing the same Helm repository.
	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
		{
			Name:      "network-operator",
			Namespace: "network-operator",
			Chart:     "network-operator",
			Version:   "v25.3.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator", "network-operator"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}, "network-operator": {}},
		Version:         "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Should have exactly one helmrepo source for the shared URL.
	helmRepoFiles := listFilesWithPrefix(t, filepath.Join(outputDir, "sources"), "helmrepo-")
	if len(helmRepoFiles) != 1 {
		t.Errorf("expected 1 helmrepo source file for shared repo, got %d", len(helmRepoFiles))
	}

	// Both HelmReleases should reference the same source.
	gpuHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	netHR := readFile(t, filepath.Join(outputDir, "network-operator", "helmrelease.yaml"))

	gpuSourceName := extractSourceName(t, gpuHR)
	netSourceName := extractSourceName(t, netHR)

	if gpuSourceName != netSourceName {
		t.Errorf("both HelmReleases should reference same source, got %q and %q", gpuSourceName, netSourceName)
	}
}

func TestGenerate_DependsOnOrdering(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:           "gpu-operator",
			Namespace:      "gpu-operator",
			Chart:          "gpu-operator",
			Version:        "v25.3.3",
			Type:           recipe.ComponentTypeHelm,
			Source:         "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager"},
		},
		{
			Name:           "network-operator",
			Namespace:      "network-operator",
			Chart:          "network-operator",
			Version:        "v25.3.0",
			Type:           recipe.ComponentTypeHelm,
			Source:         "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"gpu-operator"},
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator", "network-operator"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"cert-manager": {}, "gpu-operator": {}, "network-operator": {}},
		Version:         "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// cert-manager (first) should NOT have dependsOn.
	certHR := readFile(t, filepath.Join(outputDir, "cert-manager", "helmrelease.yaml"))
	if strings.Contains(certHR, "dependsOn") {
		t.Error("cert-manager (first component) should NOT have dependsOn")
	}

	// gpu-operator should depend on cert-manager.
	gpuHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(gpuHR, "dependsOn") {
		t.Error("gpu-operator should have dependsOn")
	}
	if !strings.Contains(gpuHR, "name: cert-manager") {
		t.Error("gpu-operator should depend on cert-manager")
	}

	// network-operator should depend on gpu-operator.
	netHR := readFile(t, filepath.Join(outputDir, "network-operator", "helmrelease.yaml"))
	if !strings.Contains(netHR, "dependsOn") {
		t.Error("network-operator should have dependsOn")
	}
	if !strings.Contains(netHR, "name: gpu-operator") {
		t.Error("network-operator should depend on gpu-operator")
	}
}

func TestGenerate_ManifestOnlyComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml":  []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
			"deployment.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify templates/ directory exists (manifest files packaged as local Helm chart).
	templatesDir := filepath.Join(outputDir, "custom-manifests", "templates")
	if _, statErr := os.Stat(templatesDir); os.IsNotExist(statErr) {
		t.Error("expected templates/ directory to exist")
	}

	// Verify manifest files exist in templates/.
	for _, name := range []string{"configmap.yaml", "deployment.yaml"} {
		path := filepath.Join(templatesDir, name)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected template file %s to exist", name)
		}
	}

	// Verify Chart.yaml exists.
	chartPath := filepath.Join(outputDir, "custom-manifests", "Chart.yaml")
	if _, statErr := os.Stat(chartPath); os.IsNotExist(statErr) {
		t.Error("expected Chart.yaml to exist")
	}

	// Verify HelmRelease CR exists with GitRepository source.
	hrPath := filepath.Join(outputDir, "custom-manifests", "helmrelease.yaml")
	if _, statErr := os.Stat(hrPath); os.IsNotExist(statErr) {
		t.Error("expected helmrelease.yaml to exist")
	}
	content := readFile(t, hrPath)
	if !strings.Contains(content, "kind: GitRepository") {
		t.Error("manifest-only HelmRelease should reference GitRepository source")
	}
}

func TestGenerate_MixedComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	manifests := map[string]map[string][]byte{
		"gpu-operator": {
			"dcgm-exporter.yaml": []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: dcgm-exporter"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentValues:    map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}}},
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify primary HelmRelease exists.
	hrPath := filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml")
	if _, statErr := os.Stat(hrPath); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator/helmrelease.yaml to exist")
	}

	// Verify post directory exists with local Helm chart.
	postDir := filepath.Join(outputDir, "gpu-operator-post")
	if _, statErr := os.Stat(postDir); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/ directory to exist")
	}

	// Verify post Chart.yaml and templates/ exist.
	postChart := filepath.Join(postDir, "Chart.yaml")
	if _, statErr := os.Stat(postChart); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/Chart.yaml to exist")
	}
	postTemplates := filepath.Join(postDir, "templates", "dcgm-exporter.yaml")
	if _, statErr := os.Stat(postTemplates); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/templates/dcgm-exporter.yaml to exist")
	}

	// Verify post HelmRelease depends on the primary HelmRelease.
	postHR := filepath.Join(postDir, "helmrelease.yaml")
	if _, statErr := os.Stat(postHR); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/helmrelease.yaml to exist")
	}
	content := readFile(t, postHR)
	if !strings.Contains(content, "name: gpu-operator") {
		t.Error("post HelmRelease should depend on gpu-operator")
	}
	if !strings.Contains(content, "kind: GitRepository") {
		t.Error("post HelmRelease should reference GitRepository source")
	}
}

func TestGenerate_Reproducible(t *testing.T) {
	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	values := map[string]map[string]any{
		"cert-manager": {"crds": map[string]any{"enabled": true}},
		"gpu-operator": {"driver": map[string]any{"enabled": true}},
	}

	// Generate twice.
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	g1 := &Generator{RecipeResult: recipeResult, ComponentValues: values, Version: "v0.9.0"}
	g2 := &Generator{RecipeResult: recipeResult, ComponentValues: values, Version: "v0.9.0"}

	ctx := context.Background()
	out1, err := g1.Generate(ctx, dir1)
	if err != nil {
		t.Fatalf("Generate() run 1 error = %v", err)
	}
	out2, err := g2.Generate(ctx, dir2)
	if err != nil {
		t.Fatalf("Generate() run 2 error = %v", err)
	}

	if len(out1.Files) != len(out2.Files) {
		t.Fatalf("file counts differ: %d vs %d", len(out1.Files), len(out2.Files))
	}

	// Compare file contents by relative path.
	for i, f1 := range out1.Files {
		f2 := out2.Files[i]
		rel1, _ := filepath.Rel(dir1, f1)
		rel2, _ := filepath.Rel(dir2, f2)
		if rel1 != rel2 {
			t.Errorf("file paths differ at index %d: %s vs %s", i, rel1, rel2)
			continue
		}
		content1 := readFile(t, f1)
		content2 := readFile(t, f2)
		if content1 != content2 {
			t.Errorf("file contents differ for %s", rel1)
		}
	}
}

func TestGenerate_DisabledComponentsFiltered(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
		{
			Name:      "disabled-component",
			Namespace: "default",
			Chart:     "disabled",
			Version:   "v1.0.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.example.com",
			Overrides: map[string]any{"enabled": false},
		},
	}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Enabled component should exist.
	if _, statErr := os.Stat(filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml")); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator/helmrelease.yaml to exist")
	}

	// Disabled component should NOT exist.
	if _, statErr := os.Stat(filepath.Join(outputDir, "disabled-component")); !os.IsNotExist(statErr) {
		t.Error("disabled-component directory should NOT be created")
	}
}

func TestGenerate_WithDynamicValues(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {
				"driver": map[string]any{
					"enabled": true,
					"version": "570.86.16",
				},
				"toolkit": map[string]any{"enabled": true},
			},
		},
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version"},
		},
		Version: "v0.9.0",
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if output == nil {
		t.Fatal("Generate() returned nil output")
	}

	// Verify ConfigMap file exists for gpu-operator.
	cmPath := filepath.Join(outputDir, "gpu-operator", "configmap-values.yaml")
	if _, statErr := os.Stat(cmPath); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator/configmap-values.yaml to exist")
	}

	// Verify ConfigMap contains dynamic value.
	cmContent := readFile(t, cmPath)
	if !strings.Contains(cmContent, "kind: ConfigMap") {
		t.Error("configmap-values.yaml should contain 'kind: ConfigMap'")
	}
	if !strings.Contains(cmContent, "gpu-operator-values") {
		t.Error("ConfigMap should be named gpu-operator-values")
	}
	if !strings.Contains(cmContent, "driver") {
		t.Error("ConfigMap should contain driver key")
	}
	if !strings.Contains(cmContent, "version") {
		t.Error("ConfigMap should contain version key")
	}

	// Verify HelmRelease has valuesFrom.
	hrContent := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "valuesFrom") {
		t.Error("gpu-operator HelmRelease should contain valuesFrom")
	}
	if !strings.Contains(hrContent, "gpu-operator-values") {
		t.Error("gpu-operator HelmRelease should reference gpu-operator-values ConfigMap")
	}

	// Verify inline values do NOT contain driver.version (it was split out).
	// The inline values should still contain driver.enabled and toolkit.
	if strings.Contains(hrContent, "570.86.16") {
		t.Error("inline values should NOT contain the dynamic driver.version value")
	}
	if !strings.Contains(hrContent, "toolkit") {
		t.Error("inline values should still contain non-dynamic toolkit values")
	}

	// Verify cert-manager has NO ConfigMap (no dynamic values for it).
	certCMPath := filepath.Join(outputDir, "cert-manager", "configmap-values.yaml")
	if _, statErr := os.Stat(certCMPath); !os.IsNotExist(statErr) {
		t.Error("cert-manager should NOT have configmap-values.yaml")
	}
	certHR := readFile(t, filepath.Join(outputDir, "cert-manager", "helmrelease.yaml"))
	if strings.Contains(certHR, "valuesFrom") {
		t.Error("cert-manager HelmRelease should NOT contain valuesFrom")
	}

	// Verify kustomization.yaml includes the ConfigMap resource.
	kustomization := readFile(t, filepath.Join(outputDir, "kustomization.yaml"))
	if !strings.Contains(kustomization, "gpu-operator/configmap-values.yaml") {
		t.Error("kustomization.yaml should include gpu-operator/configmap-values.yaml")
	}

	// Verify deployment notes mention ConfigMaps.
	foundNote := false
	for _, note := range output.DeploymentNotes {
		if strings.Contains(note, "ConfigMap") {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Error("deployment notes should mention ConfigMaps when dynamic values are present")
	}
}

func TestGenerate_WithDynamicValues_ManifestComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ index .Values \"custom-manifests\" \"mykey\" }}"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		ComponentValues: map[string]map[string]any{
			"custom-manifests": {"mykey": "default-value", "otherkey": "keep-me"},
		},
		DynamicValues: map[string][]string{
			"custom-manifests": {"mykey"},
		},
		Version: "v0.9.0",
		RepoURL: "https://github.com/my-org/gitops.git",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify ConfigMap file exists.
	cmPath := filepath.Join(outputDir, "custom-manifests", "configmap-values.yaml")
	if _, statErr := os.Stat(cmPath); os.IsNotExist(statErr) {
		t.Error("expected custom-manifests/configmap-values.yaml to exist")
	}

	// Verify ConfigMap wraps values under the component name key.
	cmContent := readFile(t, cmPath)
	if !strings.Contains(cmContent, "custom-manifests") {
		t.Error("ConfigMap values should be wrapped under component name key")
	}
	if !strings.Contains(cmContent, "mykey") {
		t.Error("ConfigMap should contain the dynamic key")
	}

	// Verify HelmRelease has valuesFrom.
	hrContent := readFile(t, filepath.Join(outputDir, "custom-manifests", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "valuesFrom") {
		t.Error("manifest HelmRelease should contain valuesFrom")
	}

	// Verify inline values still contain the non-dynamic key.
	if !strings.Contains(hrContent, "otherkey") {
		t.Error("inline values should contain non-dynamic otherkey")
	}
}

func TestSplitDynamicPaths(t *testing.T) {
	tests := []struct {
		name         string
		values       map[string]any
		dynamicPaths []string
		wantStatic   map[string]any
		wantDynamic  map[string]any
	}{
		{
			name: "split existing path",
			values: map[string]any{
				"driver": map[string]any{
					"enabled": true,
					"version": "570.86.16",
				},
				"toolkit": map[string]any{"enabled": true},
			},
			dynamicPaths: []string{"driver.version"},
			wantStatic: map[string]any{
				"driver":  map[string]any{"enabled": true},
				"toolkit": map[string]any{"enabled": true},
			},
			wantDynamic: map[string]any{
				"driver": map[string]any{"version": "570.86.16"},
			},
		},
		{
			name:         "missing path gets empty string",
			values:       map[string]any{"foo": "bar"},
			dynamicPaths: []string{"nonexistent.path"},
			wantStatic:   map[string]any{"foo": "bar"},
			wantDynamic: map[string]any{
				"nonexistent": map[string]any{"path": ""},
			},
		},
		{
			name:         "no dynamic paths returns original",
			values:       map[string]any{"foo": "bar"},
			dynamicPaths: nil,
			wantStatic:   map[string]any{"foo": "bar"},
			wantDynamic:  map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitDynamicPaths(tt.values, tt.dynamicPaths)

			// Verify static values.
			staticYAML, _ := yaml.Marshal(got.static)
			wantStaticYAML, _ := yaml.Marshal(tt.wantStatic)
			if string(staticYAML) != string(wantStaticYAML) {
				t.Errorf("static values mismatch:\ngot:  %s\nwant: %s", staticYAML, wantStaticYAML)
			}

			// Verify dynamic values.
			dynamicYAML, _ := yaml.Marshal(got.dynamic)
			wantDynamicYAML, _ := yaml.Marshal(tt.wantDynamic)
			if string(dynamicYAML) != string(wantDynamicYAML) {
				t.Errorf("dynamic values mismatch:\ngot:  %s\nwant: %s", dynamicYAML, wantDynamicYAML)
			}
		})
	}
}

func TestSanitizeSourceName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"https helm repo", "https://charts.jetstack.io", "charts-jetstack-io"},
		{"https with path", "https://helm.ngc.nvidia.com/nvidia", "helm-ngc-nvidia-com-nvidia"},
		{"oci prefix", "oci://nvcr.io/nvidia", "nvcr-io-nvidia"},
		{"git URL with .git", "https://github.com/my-org/my-repo.git", "github-com-my-org-my-repo"},
		{"trailing slash", "https://charts.jetstack.io/", "charts-jetstack-io"},
		{"empty string", "", "default-source"},
		{"http prefix", "http://charts.example.com/repo", "charts-example-com-repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeSourceName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeSourceName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDeclaredDependsOn(t *testing.T) {
	// Flux projects the recipe's declared dependencyRefs straight onto its
	// native dependsOn DAG, one edge per declared dependency. cert-manager is
	// mixed (chart + post-manifests) so its terminal release is
	// cert-manager-post; nfd is chart-only (terminal nfd). gpu-operator declares
	// both, so it gates on both terminals. network-operator declares ONLY nfd, so
	// it gates on nfd alone — no phantom edge to cert-manager, unlike the
	// tier-based argocd/helmfile deployers which would over-constrain it.
	refs := []recipe.ComponentRef{
		{Name: "cert-manager", Namespace: "cert-manager", Type: recipe.ComponentTypeHelm,
			Chart: "cert-manager", Source: "https://charts.jetstack.io"},
		{Name: "nfd", Namespace: "node-feature-discovery", Type: recipe.ComponentTypeHelm,
			Chart: "node-feature-discovery", Source: "https://kubernetes-sigs.github.io/node-feature-discovery/charts"},
		{Name: "gpu-operator", Namespace: "gpu-operator", Type: recipe.ComponentTypeHelm,
			Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager", "nfd"}},
		{Name: "network-operator", Namespace: "network-operator", Type: recipe.ComponentTypeHelm,
			Chart: "network-operator", Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"nfd"}},
		// Declares a dependency on a component that is not in the ref set
		// (disabled / externally provided): the edge is dropped, not rendered.
		{Name: "kai-scheduler", Namespace: "kai-scheduler", Type: recipe.ComponentTypeHelm,
			Chart: "kai-scheduler", Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"gpu-operator", "absent-external-dep"}},
	}
	postManifests := map[string]map[string][]byte{
		// cert-manager mixed (chart + post-manifests) → terminal cert-manager-post.
		"cert-manager": {"crds.yaml": []byte("---")},
	}

	deps := declaredDependsOn(refs, postManifests)

	tests := []struct {
		name string
		comp string
		want []string
	}{
		{"root cert-manager has no deps", "cert-manager", nil},
		{"root nfd has no deps", "nfd", nil},
		{"gpu-operator gates on both declared deps (mixed dep uses -post terminal)", "gpu-operator", []string{"cert-manager-post", "nfd"}},
		{"network-operator gates ONLY on its declared dep, no phantom edge", "network-operator", []string{"nfd"}},
		{"absent/disabled dependency is dropped, not rendered", "kai-scheduler", []string{"gpu-operator"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deps[tt.comp]; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("declaredDependsOn[%q] = %v, want %v", tt.comp, got, tt.want)
			}
			// buildPrimaryDependsOn converts the target list to DependsOnRefs.
			refsOut := buildPrimaryDependsOn(deps, tt.comp)
			if len(refsOut) != len(tt.want) {
				t.Fatalf("buildPrimaryDependsOn(%q) returned %d deps, want %d", tt.comp, len(refsOut), len(tt.want))
			}
			for i, r := range refsOut {
				if r.Name != tt.want[i] {
					t.Errorf("buildPrimaryDependsOn(%q)[%d] = %q, want %q", tt.comp, i, r.Name, tt.want[i])
				}
			}
		})
	}
}

func TestSerialDependsOn(t *testing.T) {
	// --serial ignores the declared dependency graph and chains each component
	// to the PREVIOUS one in deployment order, so releases reconcile strictly
	// one at a time. nfd and cert-manager are independent, but serial mode still
	// makes nfd wait on cert-manager (the linear chain). cert-manager is mixed
	// (post-manifests) so its terminal is cert-manager-post.
	refs := []recipe.ComponentRef{
		{Name: "cert-manager", Namespace: "cert-manager", Type: recipe.ComponentTypeHelm,
			Chart: "cert-manager", Source: "https://charts.jetstack.io"},
		{Name: "nfd", Namespace: "node-feature-discovery", Type: recipe.ComponentTypeHelm,
			Chart: "node-feature-discovery", Source: "https://kubernetes-sigs.github.io/node-feature-discovery/charts"},
		{Name: "gpu-operator", Namespace: "gpu-operator", Type: recipe.ComponentTypeHelm,
			Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager"}},
	}
	postManifests := map[string]map[string][]byte{
		"cert-manager": {"crds.yaml": []byte("---")},
	}

	deps := serialDependsOn(refs, postManifests)

	tests := []struct {
		name string
		comp string
		want []string
	}{
		{"head of chain has no predecessor", "cert-manager", nil},
		{"independent nfd still chained to previous (cert-manager-post)", "nfd", []string{"cert-manager-post"}},
		{"gpu-operator chained to previous (nfd), not its declared dep", "gpu-operator", []string{"nfd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deps[tt.comp]; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("serialDependsOn[%q] = %v, want %v", tt.comp, got, tt.want)
			}
		})
	}
}

// TestGenerate_RejectsCyclicGraph guards the fail-closed invariant: Generate is
// exported, so a hand-built cyclic RecipeResult must be rejected before it is
// projected into a cyclic Flux dependsOn DAG (which would stall reconciliation)
// — in both default and --serial mode.
func TestGenerate_RejectsCyclicGraph(t *testing.T) {
	for _, serial := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "serial"}[serial], func(t *testing.T) {
			recipeResult := &recipe.RecipeResult{}
			recipeResult.Metadata.Version = testVersion
			recipeResult.ComponentRefs = []recipe.ComponentRef{
				{Name: "a", Namespace: "a", Chart: "a", Version: "v1", Type: recipe.ComponentTypeHelm,
					Source: "https://example.com", DependencyRefs: []string{"b"}},
				{Name: "b", Namespace: "b", Chart: "b", Version: "v1", Type: recipe.ComponentTypeHelm,
					Source: "https://example.com", DependencyRefs: []string{"a"}}, // a <-> b cycle
			}
			recipeResult.DeploymentOrder = []string{"a", "b"}

			g := &Generator{
				RecipeResult:    recipeResult,
				ComponentValues: map[string]map[string]any{"a": {}, "b": {}},
				Version:         testVersion,
				Serial:          serial,
			}
			if _, err := g.Generate(context.Background(), t.TempDir()); err == nil {
				t.Fatal("expected Generate to reject a cyclic dependency graph, got nil error")
			}
		})
	}
}

// TestGenerate_DisabledDependencyNotACycle guards that graph validation runs on
// the unfiltered ComponentRefs, not the enabled-filtered set: an enabled
// component whose declared dependency is disabled (provided externally) must
// NOT be mistaken for an undeclared dependency and rejected as a false cycle.
func TestGenerate_DisabledDependencyNotACycle(t *testing.T) {
	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator", Version: "v25.3.3",
			Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager"}},
		// cert-manager is provided externally (e.g. CSP-managed): declared but
		// disabled. gpu-operator's edge to it must be treated as satisfied.
		{Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager", Version: "v1.14.0",
			Type: recipe.ComponentTypeHelm, Source: "https://charts.jetstack.io",
			Overrides: map[string]any{"enabled": false}},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         testVersion,
	}
	if _, err := g.Generate(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Generate() with an externally-provided (disabled) dependency should succeed, got: %v", err)
	}
}

// TestHelmReleaseNamespaceArchitecture locks the design assumption that
// makes flux's bare-name dependsOn references safe: every HelmRelease CR
// is emitted into the flux-system namespace, with targetNamespace pointing
// at the component's actual namespace. Bare-name dependsOn resolves
// within the dependent's own namespace, so as long as every HelmRelease
// shares flux-system, cross-component edges resolve correctly without
// needing the namespace field on DependsOnRef.
//
// If a future refactor changes HelmRelease metadata.namespace to follow
// ref.Namespace (so different components land in different namespaces),
// this test will start failing and force a parallel update to
// DependsOnRef + the template so namespace gets emitted on edges that
// cross namespaces. The helmfile deployer hit exactly this bug; see
// pkg/bundler/deployer/helmfile/TestNeedsRef for the helmfile-side guard.
func TestHelmReleaseNamespaceArchitecture(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		// Two components in DIFFERENT namespaces. If HelmRelease CRs ever
		// move into ref.Namespace, this is the configuration that would
		// expose the gap.
		{
			Name: "cert-manager", Namespace: "cert-manager",
			Chart: "cert-manager", Version: "v1.17.2",
			Type: recipe.ComponentTypeHelm, Source: "https://charts.jetstack.io",
		},
		{
			Name: "kube-prometheus-stack", Namespace: "monitoring",
			Chart: "kube-prometheus-stack", Version: "84.4.0",
			Type: recipe.ComponentTypeHelm, Source: "https://prometheus-community.github.io/helm-charts",
			DependencyRefs: []string{"cert-manager"},
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "kube-prometheus-stack"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager":          {},
			"kube-prometheus-stack": {},
		},
		Version: "v0.9.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Both HelmRelease CRs must declare metadata.namespace: flux-system.
	// targetNamespace is what carries the component's actual namespace.
	for _, comp := range []struct{ name, targetNS string }{
		{"cert-manager", "cert-manager"},
		{"kube-prometheus-stack", "monitoring"},
	} {
		hr := readFile(t, filepath.Join(outputDir, comp.name, "helmrelease.yaml"))
		if !strings.Contains(hr, "namespace: flux-system\n") {
			t.Errorf("%s HelmRelease metadata.namespace must be flux-system "+
				"(bare-name dependsOn depends on this); got:\n%s", comp.name, hr)
		}
		if !strings.Contains(hr, "targetNamespace: "+comp.targetNS+"\n") {
			t.Errorf("%s HelmRelease targetNamespace = ?, want %q; got:\n%s",
				comp.name, comp.targetNS, hr)
		}
	}

	// The dependent HelmRelease must NOT carry a namespace under dependsOn.
	// If it does, the template/struct grew a Namespace field — meaning the
	// architecture changed and DependsOnRef needs same-treatment as helmfile's
	// needsRef helper (qualify cross-namespace edges, leave same-namespace bare).
	kpsHR := readFile(t, filepath.Join(outputDir, "kube-prometheus-stack", "helmrelease.yaml"))
	depStart := strings.Index(kpsHR, "dependsOn:")
	if depStart < 0 {
		t.Fatalf("kube-prometheus-stack HelmRelease should declare dependsOn cert-manager; got:\n%s", kpsHR)
	}
	// Extract the dependsOn block and assert no `namespace:` line within it.
	// The block runs from "dependsOn:" until the next top-level spec key.
	depBlock := kpsHR[depStart:]
	if end := strings.Index(depBlock, "\n  "); end > 0 && !strings.HasPrefix(depBlock[end+1:], "  - ") {
		depBlock = depBlock[:end]
	}
	if strings.Contains(depBlock, "namespace:") {
		t.Errorf("dependsOn block contains namespace: — flux architecture has changed; "+
			"update DependsOnRef + helmrelease.yaml.tmpl + this test to handle cross-namespace edges. "+
			"Block was:\n%s", depBlock)
	}
}

func TestCollectHelmSources(t *testing.T) {
	refs := []recipe.ComponentRef{
		{Name: "a", Type: recipe.ComponentTypeHelm, Source: "https://charts.jetstack.io", Chart: "a", Version: "v1.0.0"},
		{Name: "b", Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "b", Version: "v1.0.0"},
		{Name: "c", Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "c", Version: "v1.0.0"}, // duplicate
		{Name: "d", Type: recipe.ComponentTypeKustomize, Source: "https://github.com/example/repo.git"},
	}

	// Without vendoring: all Helm sources collected.
	sources := collectHelmSources(refs, false, "flux-system")
	if len(sources) != 2 {
		t.Errorf("collectHelmSources(vendorCharts=false) returned %d sources, want 2", len(sources))
	}
	for url, src := range sources {
		if src.Namespace != "flux-system" {
			t.Errorf("collectHelmSources(vendorCharts=false) source %q has Namespace=%q, want %q", url, src.Namespace, "flux-system")
		}
	}

	// With vendoring: vendorable Helm components skip HelmRepository sources.
	sources = collectHelmSources(refs, true, "flux-system")
	if len(sources) != 0 {
		t.Errorf("collectHelmSources(vendorCharts=true) returned %d sources, want 0 (all vendorable)", len(sources))
	}
}

func TestCollectGitSources(t *testing.T) {
	sources := collectGitSources("https://github.com/default/repo.git", "main", "flux-system")

	// Should have 1: the default repo.
	if len(sources) != 1 {
		t.Errorf("collectGitSources() returned %d sources, want 1", len(sources))
	}

	src, ok := sources["https://github.com/default/repo.git"]
	if !ok {
		t.Fatal("expected default repo URL in sources")
	}
	if src.Branch != "main" {
		t.Errorf("expected branch 'main', got %q", src.Branch)
	}
	if src.Namespace != "flux-system" {
		t.Errorf("expected Namespace 'flux-system', got %q", src.Namespace)
	}
}

// ---------- golden file testing ----------

func TestBundleGolden_HelmComponents(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:           "gpu-operator",
			Namespace:      "gpu-operator",
			Chart:          "gpu-operator",
			Version:        "v25.3.3",
			Type:           recipe.ComponentTypeHelm,
			Source:         "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager"},
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		Version: "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	goldenDir := "testdata/helm_components"
	for _, rel := range []string{
		"cert-manager/helmrelease.yaml",
		"gpu-operator/helmrelease.yaml",
		"kustomization.yaml",
	} {
		assertGolden(t, outputDir, goldenDir, rel)
	}
}

// ---------- test helpers ----------

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertValidYAML(t *testing.T, path string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		t.Errorf("invalid YAML in %s: %v\n--- content ---\n%s", path, err, string(content))
	}
}

func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err = os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err = os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", relPath, got, want)
	}
}

func listFilesWithPrefix(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read directory %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

// ---------- vendor-charts tests ----------

// stubChartPuller returns a deterministic .tgz payload for any Pull call.
type stubChartPuller struct{}

var _ localformat.ChartPuller = (*stubChartPuller)(nil)

func (s *stubChartPuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	chartName := c.ChartName
	if chartName == "" {
		chartName = c.Name
	}
	tgz := []byte(fmt.Sprintf("fake-tgz-%s-%s", chartName, c.Version))
	sum := sha256.Sum256(tgz)
	tarball := fmt.Sprintf("%s-%s.tgz", chartName, c.Version)
	rec := localformat.VendorRecord{
		Name:          c.Name,
		Chart:         chartName,
		Version:       c.Version,
		Repository:    c.Repository,
		SHA256:        hex.EncodeToString(sum[:]),
		TarballName:   tarball,
		PullerVersion: "stub v0.0.0",
	}
	return tgz, rec, tarball, nil
}

func TestGenerate_VendorCharts_BasicHelm(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		Version:      "v0.9.0",
		RepoURL:      "https://github.com/my-org/gitops.git",
		VendorCharts: true,
		Puller:       &stubChartPuller{},
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify wrapper Chart.yaml exists with dependencies.
	for _, comp := range []string{"cert-manager", "gpu-operator"} {
		chartPath := filepath.Join(outputDir, comp, "Chart.yaml")
		content := readFile(t, chartPath)
		if !strings.Contains(content, "dependencies:") {
			t.Errorf("%s Chart.yaml should contain dependencies section", comp)
		}
	}

	// Verify chart tarballs exist.
	for _, comp := range []string{"cert-manager", "gpu-operator"} {
		chartsDir := filepath.Join(outputDir, comp, "charts")
		entries, err := os.ReadDir(chartsDir)
		if err != nil {
			t.Fatalf("read charts dir for %s: %v", comp, err)
		}
		found := false
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tgz") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s should have a .tgz file in charts/", comp)
		}
	}

	// Verify HelmReleases reference GitRepository, not HelmRepository.
	for _, comp := range []string{"cert-manager", "gpu-operator"} {
		hr := readFile(t, filepath.Join(outputDir, comp, "helmrelease.yaml"))
		if !strings.Contains(hr, "kind: GitRepository") {
			t.Errorf("%s HelmRelease should reference GitRepository", comp)
		}
		if strings.Contains(hr, "kind: HelmRepository") {
			t.Errorf("%s HelmRelease should NOT reference HelmRepository", comp)
		}
		if !strings.Contains(hr, "chart: ./"+comp) {
			t.Errorf("%s HelmRelease should have chart: ./%s", comp, comp)
		}
	}

	// Verify NO HelmRepository source files exist (all vendored).
	helmRepoFiles := listFilesWithPrefix(t, filepath.Join(outputDir, "sources"), "helmrepo-")
	if len(helmRepoFiles) != 0 {
		t.Errorf("expected 0 helmrepo source files when all components are vendored, got %d", len(helmRepoFiles))
	}

	// Verify provenance.yaml exists.
	provPath := filepath.Join(outputDir, "provenance.yaml")
	if _, statErr := os.Stat(provPath); os.IsNotExist(statErr) {
		t.Error("expected provenance.yaml to exist when vendor-charts is on")
	}
	provContent := readFile(t, provPath)
	if !strings.Contains(provContent, "kind: BundleProvenance") {
		t.Error("provenance.yaml should contain kind: BundleProvenance")
	}
	if !strings.Contains(provContent, "cert-manager") {
		t.Error("provenance.yaml should contain cert-manager record")
	}
	if !strings.Contains(provContent, "gpu-operator") {
		t.Error("provenance.yaml should contain gpu-operator record")
	}

	// Verify deployment notes mention vendored charts.
	foundNote := false
	for _, note := range output.DeploymentNotes {
		if strings.Contains(note, "vendored") {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Error("deployment notes should mention vendored charts")
	}

	// Verify values are nested under the subchart name.
	gpuHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(gpuHR, "gpu-operator:") {
		t.Error("vendored HelmRelease values should be nested under subchart name")
	}
}

func TestGenerate_VendorCharts_MixedComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	manifests := map[string]map[string][]byte{
		"gpu-operator": {
			"dcgm-exporter.yaml": []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: dcgm-exporter"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentValues:    map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}}},
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
		VendorCharts:       true,
		Puller:             &stubChartPuller{},
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify wrapper Chart.yaml + charts/ tarball exist for vendored chart.
	chartPath := filepath.Join(outputDir, "gpu-operator", "Chart.yaml")
	if _, statErr := os.Stat(chartPath); os.IsNotExist(statErr) {
		t.Error("expected wrapper Chart.yaml")
	}
	chartContent := readFile(t, chartPath)
	if !strings.Contains(chartContent, "dependencies:") {
		t.Error("wrapper Chart.yaml should contain dependencies section")
	}

	// Verify primary HelmRelease references GitRepository (vendored).
	hr := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(hr, "kind: GitRepository") {
		t.Error("vendored mixed HelmRelease should reference GitRepository")
	}

	// Verify -post directory still exists for manifests (same as non-vendored).
	postDir := filepath.Join(outputDir, "gpu-operator-post")
	if _, statErr := os.Stat(postDir); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/ directory for manifests")
	}

	// Verify post Chart.yaml and templates/ exist.
	postChart := filepath.Join(postDir, "Chart.yaml")
	if _, statErr := os.Stat(postChart); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/Chart.yaml")
	}
	postTemplates := filepath.Join(postDir, "templates", "dcgm-exporter.yaml")
	if _, statErr := os.Stat(postTemplates); os.IsNotExist(statErr) {
		t.Error("expected gpu-operator-post/templates/dcgm-exporter.yaml")
	}

	// Verify post HelmRelease depends on the primary.
	postHR := readFile(t, filepath.Join(postDir, "helmrelease.yaml"))
	if !strings.Contains(postHR, "name: gpu-operator") {
		t.Error("post HelmRelease should depend on gpu-operator")
	}
	if !strings.Contains(postHR, "kind: GitRepository") {
		t.Error("post HelmRelease should reference GitRepository source")
	}

	// Verify kustomization.yaml references both primary and -post.
	kustomization := readFile(t, filepath.Join(outputDir, "kustomization.yaml"))
	if !strings.Contains(kustomization, "gpu-operator/helmrelease.yaml") {
		t.Error("kustomization.yaml should reference gpu-operator/helmrelease.yaml")
	}
	if !strings.Contains(kustomization, "gpu-operator-post/helmrelease.yaml") {
		t.Error("kustomization.yaml should reference gpu-operator-post/helmrelease.yaml")
	}
}

func TestGenerate_VendorCharts_WithDynamic(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {
				"driver": map[string]any{
					"enabled": true,
					"version": "570.86.16",
				},
				"toolkit": map[string]any{"enabled": true},
			},
		},
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version"},
		},
		Version:      "v0.9.0",
		RepoURL:      "https://github.com/my-org/gitops.git",
		VendorCharts: true,
		Puller:       &stubChartPuller{},
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify ConfigMap exists and values are nested under subchart name.
	cmPath := filepath.Join(outputDir, "gpu-operator", "configmap-values.yaml")
	if _, statErr := os.Stat(cmPath); os.IsNotExist(statErr) {
		t.Fatal("expected ConfigMap file for dynamic values")
	}
	cmContent := readFile(t, cmPath)
	if !strings.Contains(cmContent, "gpu-operator") {
		t.Error("ConfigMap values should be nested under subchart name 'gpu-operator'")
	}

	// Verify HelmRelease has valuesFrom.
	hr := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(hr, "valuesFrom") {
		t.Error("vendored HelmRelease with dynamic values should have valuesFrom")
	}

	// Verify inline values do NOT contain the dynamic value.
	if strings.Contains(hr, "570.86.16") {
		t.Error("inline values should NOT contain the dynamic driver.version value")
	}
}

func TestGenerate_VendorCharts_ManifestOnlyUnaffected(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
			// No Chart, no Source — manifest-only
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
		VendorCharts:       true,
		Puller:             &stubChartPuller{},
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Manifest-only component should NOT have a charts/ directory.
	chartsDir := filepath.Join(outputDir, "custom-manifests", "charts")
	if _, statErr := os.Stat(chartsDir); !os.IsNotExist(statErr) {
		t.Error("manifest-only component should NOT have charts/ directory even with VendorCharts=true")
	}

	// Should still use the manifest-only path (templates/ + Chart.yaml).
	templatesDir := filepath.Join(outputDir, "custom-manifests", "templates")
	if _, statErr := os.Stat(templatesDir); os.IsNotExist(statErr) {
		t.Error("manifest-only component should still have templates/")
	}

	// No provenance.yaml (nothing was vendored).
	provPath := filepath.Join(outputDir, "provenance.yaml")
	if _, statErr := os.Stat(provPath); !os.IsNotExist(statErr) {
		t.Error("provenance.yaml should NOT exist when no charts are vendored")
	}
}

func extractSourceName(t *testing.T, yamlContent string) string {
	t.Helper()
	// Look for "name: <source-name>" under sourceRef.
	for line := range strings.SplitSeq(yamlContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "name:") && !strings.Contains(trimmed, "gpu-operator") && !strings.Contains(trimmed, "network-operator") && !strings.Contains(trimmed, "cert-manager") && !strings.Contains(trimmed, "-values") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		}
	}
	return ""
}

// TestGenerate_WithPreManifests pins the contract that components with
// pre-manifests emit a <name>-pre HelmRelease BEFORE the primary, and
// that the primary's dependsOn points at <name>-pre instead of the
// previous component. Regression guard for issue #923 (the GKE
// ResourceQuota fix from PR #921 was silently dropped on flux because
// the deployer didn't consume ComponentPreManifests).
func TestGenerate_WithPreManifests(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.14.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:           "gpu-operator",
			Namespace:      "gpu-operator",
			Chart:          "gpu-operator",
			Version:        "v25.3.3",
			Type:           recipe.ComponentTypeHelm,
			Source:         "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager"},
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	preManifests := map[string]map[string][]byte{
		"gpu-operator": {
			"gke-critical-pods-quota.yaml": []byte("apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: aicr-gke-critical-pods\n  namespace: gpu-operator\nspec:\n  hard:\n    pods: \"32\"\n"),
		},
	}

	g := &Generator{
		RecipeResult:          recipeResult,
		ComponentValues:       map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}}},
		ComponentPreManifests: preManifests,
		Version:               "v0.9.0",
		RepoURL:               "https://github.com/my-org/gitops.git",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Pre folder: Chart.yaml + templates/<file> + helmrelease.yaml.
	preDir := filepath.Join(outputDir, "gpu-operator-pre")
	for _, rel := range []string{"Chart.yaml", "helmrelease.yaml", filepath.Join("templates", "gke-critical-pods-quota.yaml")} {
		path := filepath.Join(preDir, rel)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected gpu-operator-pre/%s to exist", rel)
		}
	}

	// Pre HelmRelease depends on the previous component (cert-manager),
	// preserving the deployment-order chain.
	preHR := readFile(t, filepath.Join(preDir, "helmrelease.yaml"))
	if !strings.Contains(preHR, "name: cert-manager") {
		t.Errorf("expected pre HelmRelease to depend on cert-manager; got:\n%s", preHR)
	}

	// Primary HelmRelease depends on gpu-operator-pre, NOT cert-manager.
	primaryHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(primaryHR, "name: gpu-operator-pre") {
		t.Errorf("expected primary HelmRelease to depend on gpu-operator-pre; got:\n%s", primaryHR)
	}
	if strings.Contains(primaryHR, "name: cert-manager") {
		t.Errorf("primary HelmRelease should no longer depend on cert-manager directly; got:\n%s", primaryHR)
	}

	// Root kustomization references the pre folder.
	rootKustom := readFile(t, filepath.Join(outputDir, "kustomization.yaml"))
	if !strings.Contains(rootKustom, "gpu-operator-pre/helmrelease.yaml") {
		t.Errorf("expected root kustomization.yaml to include gpu-operator-pre/helmrelease.yaml; got:\n%s", rootKustom)
	}
}

// TestGenerate_PreAndPostManifests pins the full chain when both pre
// and post manifests are present on the same component, and asserts
// the *next* component depends on the previous component's TERMINAL
// release (-post), not its primary. Chain shape:
//
//	gpu-operator-pre → gpu-operator → gpu-operator-post → gke-nccl-tcpxo
//
// This is the realistic GKE shape (synthesized quota pre + dcgm-exporter
// post + downstream component) and a regression guard for the issue
// Codex caught: before the fix, the next component depended on
// "gpu-operator" so Flux could reconcile it in parallel with the post
// manifests, defeating the chain's purpose.
func TestGenerate_PreAndPostManifests(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
		{
			Name:           "gke-nccl-tcpxo",
			Namespace:      "kube-system",
			Chart:          "gke-nccl-tcpxo",
			Version:        "v1.0.0",
			Type:           recipe.ComponentTypeHelm,
			Source:         "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"gpu-operator"},
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator", "gke-nccl-tcpxo"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"gpu-operator":   {"driver": map[string]any{"enabled": true}},
			"gke-nccl-tcpxo": {},
		},
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"quota.yaml": []byte("apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: q\n  namespace: gpu-operator\n"),
			},
		},
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"dcgm-exporter.yaml": []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: dcgm-exporter\n"),
			},
		},
		Version: "v0.9.0",
		RepoURL: "https://github.com/my-org/gitops.git",
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Pre is head of the chain (gpu-operator is index 0, no preceding component).
	preHR := readFile(t, filepath.Join(outputDir, "gpu-operator-pre", "helmrelease.yaml"))
	if strings.Contains(preHR, "dependsOn:") {
		t.Errorf("expected pre HelmRelease to have no dependsOn (head of chain); got:\n%s", preHR)
	}

	// Primary depends on -pre.
	primaryHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(primaryHR, "name: gpu-operator-pre") {
		t.Errorf("expected primary to depend on gpu-operator-pre; got:\n%s", primaryHR)
	}

	// Post depends on the primary.
	postHR := readFile(t, filepath.Join(outputDir, "gpu-operator-post", "helmrelease.yaml"))
	if !strings.Contains(postHR, "name: gpu-operator") {
		t.Errorf("expected post HelmRelease to depend on gpu-operator; got:\n%s", postHR)
	}

	// Next component depends on the TERMINAL release of the previous
	// component (gpu-operator-post), not its primary.
	nextHR := readFile(t, filepath.Join(outputDir, "gke-nccl-tcpxo", "helmrelease.yaml"))
	if !strings.Contains(nextHR, "name: gpu-operator-post") {
		t.Errorf("expected next component to depend on gpu-operator-post (terminal of previous chain); got:\n%s", nextHR)
	}

	// README's "Components" table must mirror the actual HelmRelease
	// graph. A reader skimming the bundle README should see rows for
	// gpu-operator-pre and gpu-operator-post alongside the primary, with
	// dependsOn pointing at the correct chain links — not at the
	// previous component (which is what the README used to render).
	readme := readFile(t, filepath.Join(outputDir, "README.md"))
	for _, want := range []string{
		"| gpu-operator-pre |",
		"| gpu-operator-post |",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("expected README.md to contain row %q; got:\n%s", want, readme)
		}
	}
	if !strings.Contains(readme, "| gpu-operator | HelmRelease | v25.3.3 | gpu-operator | gpu-operator-pre |") {
		t.Errorf("expected README.md to show primary gpu-operator depending on gpu-operator-pre; got:\n%s", readme)
	}
	if !strings.Contains(readme, "| gke-nccl-tcpxo | HelmRelease | v1.0.0 | kube-system | gpu-operator-post |") {
		t.Errorf("expected README.md to show gke-nccl-tcpxo depending on gpu-operator-post; got:\n%s", readme)
	}
}

// TestGenerate_VendoredChartWithPreManifests verifies that the
// pre-manifest rewire works on the vendored-chart code path too. The
// vendored branch (g.generateVendoredHelmComponent) receives the same
// rewired primaryDependsOn, but the dedicated pre/post tests only
// exercise the non-vendored helm path — a future refactor that
// re-shadowed primaryDependsOn on the vendored branch would slip
// through without this guard.
func TestGenerate_VendoredChartWithPreManifests(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}}},
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"quota.yaml": []byte("apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: q\n  namespace: gpu-operator\n"),
			},
		},
		Version:      "v0.9.0",
		RepoURL:      "https://github.com/my-org/gitops.git",
		VendorCharts: true,
		Puller:       &stubChartPuller{},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Pre folder is emitted alongside the vendored wrapper chart.
	preHRPath := filepath.Join(outputDir, "gpu-operator-pre", "helmrelease.yaml")
	if _, statErr := os.Stat(preHRPath); os.IsNotExist(statErr) {
		t.Fatal("expected gpu-operator-pre/helmrelease.yaml to exist on vendored path")
	}

	// Primary HelmRelease (the vendored wrapper) depends on -pre, not on
	// the previous component / chain head.
	primaryHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(primaryHR, "name: gpu-operator-pre") {
		t.Errorf("expected vendored primary HelmRelease to depend on gpu-operator-pre; got:\n%s", primaryHR)
	}

	// Vendored wrapper still references its local GitRepository chart, not
	// a HelmRepository — sanity-check that the vendored path is actually
	// the one we exercised.
	if !strings.Contains(primaryHR, "kind: GitRepository") {
		t.Errorf("expected vendored primary HelmRelease to reference GitRepository source; got:\n%s", primaryHR)
	}
}

// TestGenerate_PreManifestsCollision asserts that a recipe declaring
// both component "foo" (with pre-manifests) and a separate component
// "foo-pre" is rejected at bundle time, mirroring the localformat
// writer's guard.
func TestGenerate_PreManifestsCollision(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "foo",
			Namespace: "foo",
			Chart:     "foo",
			Version:   "v1.0.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://example.com/charts",
		},
		{
			Name:      "foo-pre",
			Namespace: "foo",
			Chart:     "foo-pre",
			Version:   "v1.0.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://example.com/charts",
		},
	}
	recipeResult.DeploymentOrder = []string{"foo", "foo-pre"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentPreManifests: map[string]map[string][]byte{
			"foo": {
				"quota.yaml": []byte("apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: q\n"),
			},
		},
		Version: "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got: %v", err)
	}
	if !strings.Contains(err.Error(), `would inject "foo"-pre`) {
		t.Errorf("expected collision error to name the offending pair, got: %v", err)
	}
}

// TestGenerate_PostManifestsCollision asserts that a recipe declaring
// both a mixed component "foo" (chart + post-manifests) and a separate
// component "foo-post" is rejected at bundle time. Mirrors the
// pre-manifest rule and the parity declared in the localformat writer.
func TestGenerate_PostManifestsCollision(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "foo",
			Namespace: "foo",
			Chart:     "foo",
			Version:   "v1.0.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://example.com/charts",
		},
		{
			Name:      "foo-post",
			Namespace: "foo",
			Chart:     "foo-post",
			Version:   "v1.0.0",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://example.com/charts",
		},
	}
	recipeResult.DeploymentOrder = []string{"foo", "foo-post"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentManifests: map[string]map[string][]byte{
			"foo": {
				"cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n"),
			},
		},
		Version: "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got: %v", err)
	}
	if !strings.Contains(err.Error(), `would inject "foo"-post`) {
		t.Errorf("expected collision error to name the offending pair, got: %v", err)
	}
}

// ---------- OCI mode (ArtifactGenerator) tests ----------

// TestGenerate_OCISourceName_ManifestOnly verifies that manifest-only
// components emit an ArtifactGenerator CR and a chartRef HelmRelease
// (instead of a GitRepository-based HelmRelease) when OCISourceName is set.
func TestGenerate_OCISourceName_ManifestOnly(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		OCISourceName:      "aicr-bundle",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify ArtifactGenerator CR exists.
	agPath := filepath.Join(outputDir, "custom-manifests", "artifactgenerator.yaml")
	if _, statErr := os.Stat(agPath); os.IsNotExist(statErr) {
		t.Fatal("expected custom-manifests/artifactgenerator.yaml to exist")
	}
	agContent := readFile(t, agPath)
	assertValidYAML(t, agPath)
	if !strings.Contains(agContent, "kind: ArtifactGenerator") {
		t.Error("expected ArtifactGenerator kind")
	}
	if !strings.Contains(agContent, "name: aicr-bundle") {
		t.Error("expected OCISourceName reference in ArtifactGenerator")
	}
	if !strings.Contains(agContent, "custom-manifests") {
		t.Error("expected chart path reference in ArtifactGenerator")
	}

	// Verify HelmRelease uses chartRef (ExternalArtifact), not sourceRef (GitRepository).
	hrPath := filepath.Join(outputDir, "custom-manifests", "helmrelease.yaml")
	hrContent := readFile(t, hrPath)
	assertValidYAML(t, hrPath)
	if !strings.Contains(hrContent, "chartRef:") {
		t.Error("expected chartRef in HelmRelease")
	}
	if !strings.Contains(hrContent, "kind: ExternalArtifact") {
		t.Error("expected ExternalArtifact kind in chartRef")
	}
	if strings.Contains(hrContent, "kind: GitRepository") {
		t.Error("HelmRelease should NOT reference GitRepository in OCI mode")
	}

	// Verify kustomization.yaml includes the ArtifactGenerator resource.
	kustomization := readFile(t, filepath.Join(outputDir, "kustomization.yaml"))
	if !strings.Contains(kustomization, "custom-manifests/artifactgenerator.yaml") {
		t.Error("kustomization.yaml should include custom-manifests/artifactgenerator.yaml")
	}
}

// TestGenerate_OCISourceName_SkipsGitSource verifies that no GitRepository
// source CR is emitted when OCISourceName is set (the placeholder
// GitRepository is unnecessary because local charts are served via
// ArtifactGenerator/ExternalArtifact).
func TestGenerate_OCISourceName_SkipsGitSource(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		OCISourceName:      "aicr-bundle",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// No gitrepo source files should exist. This component contributes no
	// HelmRepository source either, so sources/ is not created at all —
	// asserting its absence subsumes the gitrepo-file count. See issue #1947.
	sourcesDir := filepath.Join(outputDir, "sources")
	if _, statErr := os.Stat(sourcesDir); !os.IsNotExist(statErr) {
		gitRepoFiles := listFilesWithPrefix(t, sourcesDir, "gitrepo-")
		t.Errorf("expected sources/ to be absent when no source CR is written, got stat err = %v, %d gitrepo files",
			statErr, len(gitRepoFiles))
	}
}

// TestGenerate_OCISourceName_PreManifests verifies that pre-manifest
// components also use the ArtifactGenerator path in OCI mode.
func TestGenerate_OCISourceName_PreManifests(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	preManifests := map[string]map[string][]byte{
		"gpu-operator": {
			"quota.yaml": []byte("apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: q\n  namespace: gpu-operator\n"),
		},
	}

	g := &Generator{
		RecipeResult:          recipeResult,
		ComponentValues:       map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}}},
		ComponentPreManifests: preManifests,
		Version:               "v0.9.0",
		OCISourceName:         "aicr-bundle",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Pre folder should have ArtifactGenerator + chartRef HelmRelease.
	preDir := filepath.Join(outputDir, "gpu-operator-pre")

	agPath := filepath.Join(preDir, "artifactgenerator.yaml")
	if _, statErr := os.Stat(agPath); os.IsNotExist(statErr) {
		t.Fatal("expected gpu-operator-pre/artifactgenerator.yaml to exist")
	}
	agContent := readFile(t, agPath)
	if !strings.Contains(agContent, "kind: ArtifactGenerator") {
		t.Error("expected ArtifactGenerator kind in pre directory")
	}

	preHR := readFile(t, filepath.Join(preDir, "helmrelease.yaml"))
	if !strings.Contains(preHR, "chartRef:") {
		t.Error("expected chartRef in pre HelmRelease")
	}
	if !strings.Contains(preHR, "kind: ExternalArtifact") {
		t.Error("expected ExternalArtifact kind in pre HelmRelease chartRef")
	}

	// Primary HelmRelease (upstream chart) should still use HelmRepository,
	// not ArtifactGenerator — OCISourceName only affects local-chart paths.
	primaryHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(primaryHR, "kind: HelmRepository") {
		t.Error("primary upstream HelmRelease should still reference HelmRepository")
	}
	if strings.Contains(primaryHR, "chartRef:") {
		t.Error("primary upstream HelmRelease should NOT use chartRef")
	}
}

// TestGenerate_OCISourceName_MixedComponent verifies that a mixed component
// (upstream chart + post-manifests) uses HelmRepository for the primary
// and ArtifactGenerator for the -post local chart.
func TestGenerate_OCISourceName_MixedComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	manifests := map[string]map[string][]byte{
		"gpu-operator": {
			"dcgm-exporter.yaml": []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: dcgm-exporter"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentValues:    map[string]map[string]any{"gpu-operator": {"driver": map[string]any{"enabled": true}}},
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		OCISourceName:      "aicr-bundle",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Primary uses HelmRepository (upstream chart).
	primaryHR := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(primaryHR, "kind: HelmRepository") {
		t.Error("primary HelmRelease should reference HelmRepository")
	}

	// Post uses ArtifactGenerator + ExternalArtifact.
	postDir := filepath.Join(outputDir, "gpu-operator-post")
	agPath := filepath.Join(postDir, "artifactgenerator.yaml")
	if _, statErr := os.Stat(agPath); os.IsNotExist(statErr) {
		t.Fatal("expected gpu-operator-post/artifactgenerator.yaml to exist")
	}

	postHR := readFile(t, filepath.Join(postDir, "helmrelease.yaml"))
	if !strings.Contains(postHR, "chartRef:") {
		t.Error("post HelmRelease should use chartRef")
	}
	if !strings.Contains(postHR, "kind: ExternalArtifact") {
		t.Error("post HelmRelease should reference ExternalArtifact")
	}
	if strings.Contains(postHR, "kind: GitRepository") {
		t.Error("post HelmRelease should NOT reference GitRepository in OCI mode")
	}
}

// TestGenerate_OCISourceName_VendoredChart verifies that vendored
// components also emit ArtifactGenerator + chartRef when OCISourceName
// is set.
func TestGenerate_OCISourceName_VendoredChart(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
	}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"cert-manager": {"crds": map[string]any{"enabled": true}}},
		Version:         "v0.9.0",
		VendorCharts:    true,
		Puller:          &stubChartPuller{},
		OCISourceName:   "aicr-bundle",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Vendored chart still has wrapper Chart.yaml + charts/ tarball.
	chartPath := filepath.Join(outputDir, "cert-manager", "Chart.yaml")
	if _, statErr := os.Stat(chartPath); os.IsNotExist(statErr) {
		t.Error("expected wrapper Chart.yaml for vendored chart")
	}

	// Verify ArtifactGenerator CR exists.
	agPath := filepath.Join(outputDir, "cert-manager", "artifactgenerator.yaml")
	if _, statErr := os.Stat(agPath); os.IsNotExist(statErr) {
		t.Fatal("expected cert-manager/artifactgenerator.yaml to exist")
	}
	agContent := readFile(t, agPath)
	if !strings.Contains(agContent, "kind: ArtifactGenerator") {
		t.Error("expected ArtifactGenerator kind")
	}
	if !strings.Contains(agContent, "name: aicr-bundle") {
		t.Error("expected OCISourceName reference in ArtifactGenerator")
	}

	// Verify HelmRelease uses chartRef.
	hrContent := readFile(t, filepath.Join(outputDir, "cert-manager", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "chartRef:") {
		t.Error("vendored HelmRelease should use chartRef in OCI mode")
	}
	if !strings.Contains(hrContent, "kind: ExternalArtifact") {
		t.Error("vendored HelmRelease should reference ExternalArtifact")
	}
	if strings.Contains(hrContent, "kind: GitRepository") {
		t.Error("vendored HelmRelease should NOT reference GitRepository in OCI mode")
	}
}

// TestGenerate_SourcesDirCreatedOnlyWhenPopulated verifies that sources/ is
// emitted only when at least one source CR is written.
//
// A component contributes a HelmRepository source only when it resolves to a
// remote Helm chart (non-empty Source), and --vendor-charts removes even
// those, since collectHelmSources skips every vendorable ref. GitRepository
// sources are suppressed entirely under an OCI output reference, where local
// charts are consumed via ArtifactGenerator + ExternalArtifact. When both
// filters apply, nothing is contributed and an unconditionally created
// sources/ would remain empty. The bundle inventory validator walks the
// finished tree and rejects any directory contributing no files, which failed
// generation outright. See issue #1947.
//
// The cases below model the empty state with source-less components, which is
// the same state --vendor-charts produces without needing a chart puller.
//
// IncludeChecksums is set so the assertion runs through
// checksum.WriteChecksums → validateExactTree, the check that actually
// rejected the empty directory, rather than only the proxy that sources/ is
// absent.
func TestGenerate_SourcesDirCreatedOnlyWhenPopulated(t *testing.T) {
	t.Parallel()
	localManifests := map[string]map[string][]byte{
		"nfd-ocp": {
			"nfd.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nfd-ocp\n"),
		},
	}
	localOnlyRefs := []recipe.ComponentRef{
		{Name: "nfd-ocp", Namespace: "nfd", Type: recipe.ComponentTypeHelm},
	}
	remoteChartRefs := []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
	}

	tests := []struct {
		name string
		refs []recipe.ComponentRef
		// manifests supplies local chart content; nil for remote-chart refs.
		manifests map[string]map[string][]byte
		// ociSourceName empty selects the local-directory (GitRepository) path.
		ociSourceName string
		// preCreateStale plants an empty sources/ before generation, as a
		// pre-fix run would have left behind.
		preCreateStale bool
		wantSourcesDir bool
	}{
		{
			name:           "OCI output, all-local components: no sources dir",
			refs:           localOnlyRefs,
			manifests:      localManifests,
			ociSourceName:  "aicr-bundle",
			wantSourcesDir: false,
		},
		{
			name:           "OCI output, all-local components, stale empty sources dir removed",
			refs:           localOnlyRefs,
			manifests:      localManifests,
			ociSourceName:  "aicr-bundle",
			preCreateStale: true,
			wantSourcesDir: false,
		},
		{
			name:           "control: OCI output with a remote chart keeps sources dir",
			refs:           remoteChartRefs,
			ociSourceName:  "aicr-bundle",
			wantSourcesDir: true,
		},
		{
			name:           "control: local output writes a GitRepository source",
			refs:           localOnlyRefs,
			manifests:      localManifests,
			wantSourcesDir: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outputDir := t.TempDir()
			sourcesDir := filepath.Join(outputDir, "sources")

			if tt.preCreateStale {
				if err := os.MkdirAll(sourcesDir, 0750); err != nil {
					t.Fatalf("failed to plant stale sources dir: %v", err)
				}
			}

			recipeResult := &recipe.RecipeResult{}
			recipeResult.Metadata.Version = testVersion
			recipeResult.ComponentRefs = tt.refs

			g := &Generator{
				RecipeResult:       recipeResult,
				ComponentManifests: tt.manifests,
				Version:            testVersion,
				OCISourceName:      tt.ociSourceName,
				IncludeChecksums:   true,
			}

			if _, err := g.Generate(context.Background(), outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			info, statErr := os.Stat(sourcesDir)
			switch {
			case tt.wantSourcesDir:
				if statErr != nil {
					t.Fatalf("expected sources/ to exist, stat error = %v", statErr)
				}
				if !info.IsDir() {
					t.Fatalf("expected sources/ to be a directory, got mode %v", info.Mode())
				}
				entries, readErr := os.ReadDir(sourcesDir)
				if readErr != nil {
					t.Fatalf("failed to read sources/: %v", readErr)
				}
				if len(entries) == 0 {
					t.Error("expected sources/ to contain at least one source CR")
				}
			default:
				if !os.IsNotExist(statErr) {
					t.Errorf("expected sources/ to be absent, stat error = %v", statErr)
				}
			}
		})
	}
}

// TestGenerate_StaleSourcesPathIsNotDestroyed covers the guard shared by the
// create and remove routes to the sources path. os.Remove unlinks regular
// files as readily as it removes empty directories, so removal must not run
// blind — and both routes must reject the same user error with the same code
// rather than having the outcome decided by recipe shape.
func TestGenerate_StaleSourcesPathIsNotDestroyed(t *testing.T) {
	t.Parallel()
	localManifests := map[string]map[string][]byte{
		"nfd-ocp": {
			"nfd.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nfd-ocp\n"),
		},
	}
	localOnlyRefs := []recipe.ComponentRef{
		{Name: "nfd-ocp", Namespace: "nfd", Type: recipe.ComponentTypeHelm},
	}
	remoteChartRefs := []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
	}

	const fileContents = "not a directory\n"

	tests := []struct {
		name string
		refs []recipe.ComponentRef
		// plant places something at the sources path before generation.
		plant     func(t *testing.T, sourcesDir string)
		manifests map[string]map[string][]byte
		wantErr   bool
		// verify asserts the planted object survived generation intact.
		verify func(t *testing.T, sourcesDir string)
	}{
		{
			name:      "regular file rejected on the remove route",
			refs:      localOnlyRefs,
			manifests: localManifests,
			plant: func(t *testing.T, sourcesDir string) {
				t.Helper()
				if err := os.WriteFile(sourcesDir, []byte(fileContents), 0600); err != nil {
					t.Fatalf("failed to plant file: %v", err)
				}
			},
			wantErr: true,
			verify: func(t *testing.T, sourcesDir string) {
				t.Helper()
				got, err := os.ReadFile(sourcesDir) //nolint:gosec // test-controlled path
				if err != nil {
					t.Fatalf("planted file was destroyed: %v", err)
				}
				if string(got) != fileContents {
					t.Errorf("planted file contents = %q, want %q", got, fileContents)
				}
			},
		},
		{
			name: "regular file rejected on the create route",
			refs: remoteChartRefs,
			plant: func(t *testing.T, sourcesDir string) {
				t.Helper()
				if err := os.WriteFile(sourcesDir, []byte(fileContents), 0600); err != nil {
					t.Fatalf("failed to plant file: %v", err)
				}
			},
			wantErr: true,
			verify: func(t *testing.T, sourcesDir string) {
				t.Helper()
				got, err := os.ReadFile(sourcesDir) //nolint:gosec // test-controlled path
				if err != nil {
					t.Fatalf("planted file was destroyed: %v", err)
				}
				if string(got) != fileContents {
					t.Errorf("planted file contents = %q, want %q", got, fileContents)
				}
			},
		},
		{
			name:      "symlink rejected and left in place",
			refs:      localOnlyRefs,
			manifests: localManifests,
			plant: func(t *testing.T, sourcesDir string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "elsewhere")
				if err := os.MkdirAll(target, 0750); err != nil {
					t.Fatalf("failed to create symlink target: %v", err)
				}
				if err := os.Symlink(target, sourcesDir); err != nil {
					t.Fatalf("failed to plant symlink: %v", err)
				}
			},
			wantErr: true,
			verify: func(t *testing.T, sourcesDir string) {
				t.Helper()
				info, err := os.Lstat(sourcesDir)
				if err != nil {
					t.Fatalf("planted symlink was destroyed: %v", err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("planted symlink replaced, mode = %v", info.Mode())
				}
			},
		},
		{
			name:      "populated stale directory preserved",
			refs:      localOnlyRefs,
			manifests: localManifests,
			plant: func(t *testing.T, sourcesDir string) {
				t.Helper()
				if err := os.MkdirAll(sourcesDir, 0750); err != nil {
					t.Fatalf("failed to plant directory: %v", err)
				}
				stale := filepath.Join(sourcesDir, "gitrepo-stale.yaml")
				if err := os.WriteFile(stale, []byte("stale\n"), 0600); err != nil {
					t.Fatalf("failed to plant stale source CR: %v", err)
				}
			},
			// Generation itself succeeds; the stale file is surfaced by the
			// inventory validator, which runs only under IncludeChecksums.
			wantErr: false,
			verify: func(t *testing.T, sourcesDir string) {
				t.Helper()
				stale := filepath.Join(sourcesDir, "gitrepo-stale.yaml")
				if _, err := os.Stat(stale); err != nil {
					t.Fatalf("populated stale directory was destroyed: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outputDir := t.TempDir()
			sourcesDir := filepath.Join(outputDir, "sources")
			tt.plant(t, sourcesDir)

			recipeResult := &recipe.RecipeResult{}
			recipeResult.Metadata.Version = testVersion
			recipeResult.ComponentRefs = tt.refs

			g := &Generator{
				RecipeResult:       recipeResult,
				ComponentManifests: tt.manifests,
				Version:            testVersion,
				OCISourceName:      "aicr-bundle",
			}

			_, err := g.Generate(context.Background(), outputDir)
			switch {
			case tt.wantErr && err == nil:
				t.Fatal("expected Generate() to fail, got nil error")
			case tt.wantErr:
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("expected ErrCodeInvalidRequest, got: %v", err)
				}
				if !strings.Contains(err.Error(), sourcesDir) {
					t.Errorf("expected error to name %q, got: %v", sourcesDir, err)
				}
			case err != nil:
				t.Fatalf("Generate() error = %v", err)
			}

			tt.verify(t, sourcesDir)
		})
	}
}

// TestGenerate_OCISourceName_Empty_Preserves verifies that an empty
// OCISourceName preserves the existing behavior (GitRepository-based
// HelmRelease, no ArtifactGenerator).
func TestGenerate_OCISourceName_Empty_Preserves(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
		OCISourceName:      "", // empty = existing behavior
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// No ArtifactGenerator should be emitted.
	agPath := filepath.Join(outputDir, "custom-manifests", "artifactgenerator.yaml")
	if _, statErr := os.Stat(agPath); !os.IsNotExist(statErr) {
		t.Error("expected NO artifactgenerator.yaml when OCISourceName is empty")
	}

	// HelmRelease should reference GitRepository (existing behavior).
	hrContent := readFile(t, filepath.Join(outputDir, "custom-manifests", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "kind: GitRepository") {
		t.Error("HelmRelease should reference GitRepository when OCISourceName is empty")
	}
	if strings.Contains(hrContent, "chartRef:") {
		t.Error("HelmRelease should NOT use chartRef when OCISourceName is empty")
	}
}

// TestGenerate_OCISourceName_DynamicValues verifies that ConfigMap
// splitting works correctly with the chartRef path.
func TestGenerate_OCISourceName_DynamicValues(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"configmap.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ index .Values \"custom-manifests\" \"mykey\" }}"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		ComponentValues: map[string]map[string]any{
			"custom-manifests": {"mykey": "default-value", "otherkey": "keep-me"},
		},
		DynamicValues: map[string][]string{
			"custom-manifests": {"mykey"},
		},
		Version:       "v0.9.0",
		OCISourceName: "aicr-bundle",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify ConfigMap file exists.
	cmPath := filepath.Join(outputDir, "custom-manifests", "configmap-values.yaml")
	if _, statErr := os.Stat(cmPath); os.IsNotExist(statErr) {
		t.Error("expected ConfigMap file for dynamic values in OCI mode")
	}

	// Verify HelmRelease uses chartRef AND has valuesFrom.
	hrContent := readFile(t, filepath.Join(outputDir, "custom-manifests", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "chartRef:") {
		t.Error("expected chartRef in HelmRelease")
	}
	if !strings.Contains(hrContent, "valuesFrom") {
		t.Error("expected valuesFrom in HelmRelease")
	}
	if !strings.Contains(hrContent, "custom-manifests-values") {
		t.Error("expected ConfigMap reference in HelmRelease")
	}

	// Verify ArtifactGenerator exists.
	agPath := filepath.Join(outputDir, "custom-manifests", "artifactgenerator.yaml")
	if _, statErr := os.Stat(agPath); os.IsNotExist(statErr) {
		t.Error("expected artifactgenerator.yaml with dynamic values in OCI mode")
	}
}

// TestGenerate_CustomNamespace verifies that setting Generator.Namespace
// propagates through all generated CRs instead of the default "flux-system".
func TestGenerate_CustomNamespace(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
		},
	}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
		},
		Version:   "v0.9.0",
		Namespace: "gitops",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// HelmRelease must use custom namespace.
	hrContent := readFile(t, filepath.Join(outputDir, "cert-manager", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "namespace: gitops\n") {
		t.Error("HelmRelease metadata.namespace should be 'gitops'")
	}
	if strings.Contains(hrContent, "namespace: flux-system") {
		t.Error("HelmRelease should NOT contain 'flux-system' with custom namespace")
	}

	// HelmRepository source must use custom namespace.
	sourcesDir := filepath.Join(outputDir, "sources")
	entries, readErr := os.ReadDir(sourcesDir)
	if readErr != nil {
		t.Fatalf("failed to read sources dir: %v", readErr)
	}
	for _, entry := range entries {
		content := readFile(t, filepath.Join(sourcesDir, entry.Name()))
		if strings.Contains(content, "namespace: flux-system") {
			t.Errorf("source %s should use namespace 'gitops', not 'flux-system'", entry.Name())
		}
		if !strings.Contains(content, "namespace: gitops") {
			t.Errorf("source %s should contain namespace 'gitops'", entry.Name())
		}
	}
}

// TestGenerate_CustomNamespace_OCIMode verifies that a custom namespace
// propagates through ArtifactGenerator and chartRef HelmRelease CRs.
func TestGenerate_CustomNamespace_OCIMode(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		Namespace:          "custom-ns",
		OCISourceName:      "my-oci-repo",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// ArtifactGenerator must use custom namespace and source name.
	agContent := readFile(t, filepath.Join(outputDir, "custom-manifests", "artifactgenerator.yaml"))
	if !strings.Contains(agContent, "namespace: custom-ns") {
		t.Error("ArtifactGenerator should use namespace 'custom-ns'")
	}
	if !strings.Contains(agContent, "name: my-oci-repo") {
		t.Error("ArtifactGenerator should reference OCISourceName 'my-oci-repo'")
	}

	// chartRef HelmRelease must use custom namespace in both metadata and chartRef.
	hrContent := readFile(t, filepath.Join(outputDir, "custom-manifests", "helmrelease.yaml"))
	if !strings.Contains(hrContent, "namespace: custom-ns") {
		t.Error("HelmRelease should use namespace 'custom-ns'")
	}
	if strings.Contains(hrContent, "flux-system") {
		t.Error("no CR should contain 'flux-system' with custom namespace")
	}
}

// TestGenerate_CustomOCISourceName verifies that a custom OCISourceName
// appears in all ArtifactGenerator CRs.
func TestGenerate_CustomOCISourceName(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "custom-manifests",
			Namespace: "default",
			Type:      recipe.ComponentTypeHelm,
		},
	}

	manifests := map[string]map[string][]byte{
		"custom-manifests": {
			"cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test"),
		},
	}

	g := &Generator{
		RecipeResult:       recipeResult,
		ComponentManifests: manifests,
		Version:            "v0.9.0",
		OCISourceName:      "my-custom-oci-repo",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	agContent := readFile(t, filepath.Join(outputDir, "custom-manifests", "artifactgenerator.yaml"))
	if !strings.Contains(agContent, "name: my-custom-oci-repo") {
		t.Error("ArtifactGenerator should reference custom OCISourceName")
	}
	if strings.Contains(agContent, "aicr-bundle") {
		t.Error("ArtifactGenerator should NOT contain default 'aicr-bundle'")
	}
}

// TestGenerate_SourceOnlyRefChartFallsBackToName pins the chart-name
// fallback: a source-only ref (no explicit chart — a long-standing deployable
// shape whose chart name the localformat deployer derives from the component
// name) must emit a HelmRelease whose chart is the component name, not an
// empty string Flux cannot reconcile.
func TestGenerate_SourceOnlyRefChartFallsBackToName(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
		// Post-manifests make this a source-only MIXED component: the
		// generator emits a <name>-post release, and the README summary must
		// list it (its isMixed predicate keys on chart-or-source, matching
		// terminalReleaseNameFor).
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {"extra.yaml": []byte("kind: ConfigMap\nmetadata:\n  name: extra\n")},
		},
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	hr := readFile(t, filepath.Join(outputDir, "gpu-operator", "helmrelease.yaml"))
	if !strings.Contains(hr, "chart: gpu-operator") {
		t.Errorf("HelmRelease should fall back to the component name for the chart, got:\n%s", hr)
	}

	readme := readFile(t, filepath.Join(outputDir, "README.md"))
	if !strings.Contains(readme, "gpu-operator-post") {
		t.Errorf("README should list the gpu-operator-post release for a source-only mixed component, got:\n%s", readme)
	}
}

// TestGenerate_HelmReleaseCRDUpgradePolicyIsOptIn covers #2264.
//
// helm-controller's spec.upgrade.crds default of Skip means a chart bump whose
// CRDs changed runs a new controller against the previous schema. The fix is
// opt-in via the registry ownsCRDs flag, not a blanket default: an audit found
// 15 components ship CRDs under crds/ and 11 share at least one with another
// component. nfd, gpu-operator and kai-scheduler all appear together in
// base.yaml and all ship the NodeFeature CRDs, so replacing unconditionally
// would have several HelmReleases rewrite the same CRD every reconcile.
//
// Refs are built by running ApplyRegistryDefaults over an empty ref, which is
// what a stock recipe actually resolves to. Constructing them by copying
// registry fields directly is what an earlier revision did, and it silently
// diverged: ApplyRegistryDefaults strips a defaultChart to its bare name, so a
// hand-built ref carrying "gatekeeper/gatekeeper" never matched the real
// resolved "gatekeeper" and the test could not see that gatekeeper emitted no
// policy at all. Do not hand-build these refs.
//
// The owner cases are discovered from the registry rather than hardcoded, so a
// component enrolled in ownsCRDs later is covered without touching this test.
//
// NOTE ON COVERAGE: this exercises the sourceRef template only. An upstream
// Helm component renders spec.chart.spec.sourceRef regardless of
// OCISourceName. TestUsesRegistryChartRejectsRefsWithoutCoordinates covers
// why the chartRef shape cannot emit the policy today.
func TestGenerate_HelmReleaseCRDUpgradePolicyIsOptIn(t *testing.T) {
	registry, regErr := recipe.GetComponentRegistry()
	if regErr != nil {
		t.Fatalf("GetComponentRegistry: %v", regErr)
	}

	// resolvedRef mirrors stock resolution: an empty ref, then registry
	// defaults. Anything else risks testing a shape the resolver never emits.
	resolvedRef := func(t *testing.T, name string) recipe.ComponentRef {
		t.Helper()
		cfg := registry.Get(name)
		if cfg == nil {
			t.Fatalf("%s missing from registry", name)
		}
		ref := recipe.ComponentRef{Name: name, Namespace: name, Type: recipe.ComponentTypeHelm}
		ref.ApplyRegistryDefaults(cfg)
		return ref
	}

	// Every enrolled owner, discovered rather than listed.
	var owners []string
	for _, name := range registry.Names() {
		if cfg := registry.Get(name); cfg != nil && cfg.OwnsCRDs {
			owners = append(owners, name)
		}
	}
	if len(owners) == 0 {
		t.Fatal("no ownsCRDs components in the registry; this test would prove nothing")
	}

	t.Run("owners emit the policy", func(t *testing.T) {
		for _, name := range owners {
			t.Run(name, func(t *testing.T) {
				assertCRDPolicy(t, resolvedRef(t, name), name, "CreateReplace")
			})
		}
	})

	// nfd shares the NodeFeature CRDs with gpu-operator and network-operator,
	// all three of which co-exist in base.yaml.
	t.Run("sharer does not", func(t *testing.T) {
		assertCRDPolicy(t, resolvedRef(t, "nfd"), "nfd", "")
	})

	// ownsCRDs records an audit of the registry's pinned chart. A ref pointing
	// anywhere else is unaudited, so the policy must not carry over.
	t.Run("coordinate overrides disable it", func(t *testing.T) {
		owner := owners[0]
		for _, tc := range []struct {
			name   string
			mutate func(*recipe.ComponentRef)
		}{
			{"version", func(r *recipe.ComponentRef) { r.Version = "0.0.1-unaudited" }},
			{"chart", func(r *recipe.ComponentRef) { r.Chart = "some-fork" }},
			{"source", func(r *recipe.ComponentRef) { r.Source = "oci://example.invalid/charts" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ref := resolvedRef(t, owner)
				tc.mutate(&ref)
				assertCRDPolicy(t, ref, owner, "")
			})
		}
	})
}

// assertCRDPolicy renders one component and asserts spec.upgrade.crds.
//
// Decoded as a map, not a typed struct: a string field cannot tell an absent
// crds key from one explicitly rendered as "", so a negative case would pass
// against a template emitting an empty value. Presence is the assertion.
func assertCRDPolicy(t *testing.T, ref recipe.ComponentRef, component, want string) {
	t.Helper()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{ref}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{component: {}},
		Version:         "v0.9.0",
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	raw := readFile(t, filepath.Join(outputDir, component, "helmrelease.yaml"))
	var doc struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse HelmRelease: %v", err)
	}
	upgrade, hasUpgrade := doc.Spec["upgrade"]

	if want == "" {
		if hasUpgrade {
			t.Errorf("spec.upgrade present (%v), want the key absent entirely\n%s", upgrade, raw)
		}
		return
	}
	if !hasUpgrade {
		t.Fatalf("spec.upgrade absent, want crds = %q\n%s", want, raw)
	}
	upgradeMap, ok := upgrade.(map[string]any)
	if !ok {
		t.Fatalf("spec.upgrade is %T, want a mapping\n%s", upgrade, raw)
	}
	crds, hasCRDs := upgradeMap["crds"]
	if !hasCRDs {
		t.Fatalf("spec.upgrade.crds absent, want %q\n%s", want, raw)
	}
	if crds != want {
		t.Errorf("spec.upgrade.crds = %v, want %q\n%s", crds, want, raw)
	}
}

// TestUsesRegistryChartRejectsRefsWithoutCoordinates pins the property that
// makes the chartRef template's UpgradeCRDs block unreachable today.
//
// chartRef is reached by local, vendored and manifest-backed components. Those
// carry no registry chart coordinates, so usesRegistryChart is false for them
// and the policy cannot render. Asserting that directly is preferable to a
// render test: an attempt at the render route produced a sourceRef HelmRelease
// instead, so such a test would have passed without exercising anything, which
// is the ambiguous-condition shape this suite already avoids elsewhere.
//
// If a future chartRef path does carry registry coordinates, this test still
// holds but the block becomes reachable — at which point the chartRef template
// needs its own render coverage.
func TestUsesRegistryChartRejectsRefsWithoutCoordinates(t *testing.T) {
	registry, regErr := recipe.GetComponentRegistry()
	if regErr != nil {
		t.Fatalf("GetComponentRegistry: %v", regErr)
	}
	cfg := registry.Get("k8s-aibom")
	if cfg == nil || !cfg.OwnsCRDs {
		t.Fatal("k8s-aibom must be an ownsCRDs component for this test to mean anything")
	}

	tests := []struct {
		name string
		ref  recipe.ComponentRef
		want bool
	}{
		{
			name: "manifest-only ref has no chart or source",
			ref: recipe.ComponentRef{
				Name: "k8s-aibom", Type: recipe.ComponentTypeHelm,
				ManifestFiles: []string{"components/k8s-aibom/values.yaml"},
			},
			want: false,
		},
		{
			name: "local chart path is not the registry chart",
			ref: recipe.ComponentRef{
				Name: "k8s-aibom", Type: recipe.ComponentTypeHelm,
				Chart: "./k8s-aibom", Version: cfg.Helm.DefaultVersion,
			},
			want: false,
		},
		{
			// The control: without this, every case above could pass because
			// usesRegistryChart always returns false.
			name: "fully resolved registry ref matches",
			ref: func() recipe.ComponentRef {
				r := recipe.ComponentRef{Name: "k8s-aibom", Type: recipe.ComponentTypeHelm}
				r.ApplyRegistryDefaults(cfg)
				return r
			}(),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usesRegistryChart(tt.ref, cfg); got != tt.want {
				t.Errorf("usesRegistryChart() = %v, want %v (chart=%q source=%q version=%q)",
					got, tt.want, tt.ref.EffectiveChart(), tt.ref.Source, tt.ref.Version)
			}
		})
	}
}

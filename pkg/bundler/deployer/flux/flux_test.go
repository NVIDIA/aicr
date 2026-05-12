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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
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

func TestBuildDependsOn(t *testing.T) {
	refs := []recipe.ComponentRef{
		{Name: "cert-manager", Namespace: "cert-manager", Type: recipe.ComponentTypeHelm},
		{Name: "manifest-only", Namespace: "default", Type: recipe.ComponentTypeHelm},
		{Name: "gpu-operator", Namespace: "gpu-operator", Type: recipe.ComponentTypeHelm},
		{Name: "network-operator", Namespace: "network-operator", Type: recipe.ComponentTypeHelm},
	}

	tests := []struct {
		name    string
		index   int
		wantLen int
		wantDep string
	}{
		{"first has no deps", 0, 0, ""},
		{"second depends on first", 1, 1, "cert-manager"},
		{"third depends on second", 2, 1, "manifest-only"},
		{"fourth depends on third", 3, 1, "gpu-operator"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDependsOn(refs, tt.index)
			if len(got) != tt.wantLen {
				t.Errorf("buildDependsOn() returned %d deps, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && got[0].Name != tt.wantDep {
				t.Errorf("buildDependsOn() dep name = %q, want %q", got[0].Name, tt.wantDep)
			}
		})
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
	sources := collectHelmSources(refs, false)
	if len(sources) != 2 {
		t.Errorf("collectHelmSources(vendorCharts=false) returned %d sources, want 2", len(sources))
	}

	// With vendoring: vendorable Helm components skip HelmRepository sources.
	sources = collectHelmSources(refs, true)
	if len(sources) != 0 {
		t.Errorf("collectHelmSources(vendorCharts=true) returned %d sources, want 0 (all vendorable)", len(sources))
	}
}

func TestCollectGitSources(t *testing.T) {
	sources := collectGitSources("https://github.com/default/repo.git", "main")

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
	for _, line := range strings.Split(yamlContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "name:") && !strings.Contains(trimmed, "gpu-operator") && !strings.Contains(trimmed, "network-operator") && !strings.Contains(trimmed, "cert-manager") && !strings.Contains(trimmed, "-values") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		}
	}
	return ""
}

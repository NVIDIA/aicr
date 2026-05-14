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

package helmfile

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testBundlerVersion = "v1.0.0"

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

// TestGenerate_Scenarios is the golden-file suite for Generator.Generate:
// each row supplies a Generator configuration, the testdata/<name>/ dir
// holding the expected output, and the file basenames to byte-compare.
// Scenarios that exercise different code paths (error validation,
// side-effect checks, context cancellation) live in their own focused
// tests below — they don't fit the runScenario shape and would obscure
// these positive-case goldens.
func TestGenerate_Scenarios(t *testing.T) {
	scenarios := []struct {
		// name is the t.Run subtest label and the testdata/<name>/ dir.
		name string
		// gen is the configured Generator to invoke. Each scenario builds
		// its own to keep RecipeResult + values + manifests + flags
		// localized.
		gen *Generator
		// goldens lists basenames under testdata/<name>/ to byte-compare
		// against. helmfile.yaml is always included; README.md is added
		// only where its content shape is part of the assertion.
		goldens []string
	}{
		{
			// Simplest case: two upstream-helm components in deployment
			// order. Verifies repository dedupe, release ordering via
			// needs:, and values-path normalization. README is included
			// to lock its component-table rendering.
			name: "upstream_helm_only",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
					ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}},
					"gpu-operator": {"driver": map[string]any{"enabled": true}},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{"helmfile.yaml", "README.md"},
		},
		{
			// cluster-values.yaml must be referenced in the release's
			// values: list whenever the component has dynamic paths.
			name: "with_dynamic_values",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}, "replicaCount": 3},
				},
				DynamicValues: map[string][]string{
					"cert-manager": {"replicaCount"},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{"helmfile.yaml"},
		},
		{
			// kai-scheduler is hardcoded in deploy.sh.tmpl as async;
			// helmfile bundle must mirror that with wait:false + 20m
			// timeout. Drift between the two is locked by
			// TestComponentOverrides_ParityWithHelmDeployScript.
			name: "kai_scheduler_async",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("kai-scheduler", "kai-scheduler", "kai-scheduler", "v0.14.1",
						"oci://ghcr.io/kai-scheduler/kai-scheduler"),
				),
				ComponentValues: map[string]map[string]any{
					"kai-scheduler": {"enabled": true},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{"helmfile.yaml"},
		},
		{
			// Mixed component (Helm chart + post manifests): primary
			// upstream-helm release plus a local-helm <name>-post
			// release with needs: pointing at the primary.
			name: "mixed_gpu_operator",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3",
						"https://helm.ngc.nvidia.com/nvidia"),
				),
				ComponentValues: map[string]map[string]any{
					"gpu-operator": {"driver": map[string]any{"enabled": true}},
				},
				ComponentPostManifests: map[string]map[string][]byte{
					"gpu-operator": {
						"nvidia-runtime-class.yaml": []byte("apiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata:\n  name: nvidia\nhandler: nvidia\n"),
					},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{"helmfile.yaml"},
		},
		{
			// Manifest-only component (no chart/source, just manifests):
			// localformat wraps as a local chart, so the release is
			// `chart: ./001-<name>` with no version.
			name: "manifest_only",
			gen: &Generator{
				RecipeResult: recipeWith(
					recipe.ComponentRef{
						Name:      "node-prep",
						Namespace: "kube-system",
						Type:      recipe.ComponentTypeHelm,
					},
				),
				ComponentValues: map[string]map[string]any{
					"node-prep": {},
				},
				ComponentPostManifests: map[string]map[string][]byte{
					"node-prep": {
						"daemonset.yaml": []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: node-prep\nspec: {}\n"),
					},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{"helmfile.yaml"},
		},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			runScenario(t, tc.gen, tc.name, tc.goldens)
		})
	}
}

// TestGenerate_EmptyRecipe asserts the deployer still emits a parseable
// helmfile.yaml when a recipe has zero enabled components (helmfile lint
// would otherwise reject the document; we want a stable artifact).
func TestGenerate_EmptyRecipe(t *testing.T) {
	g := &Generator{
		RecipeResult: &recipe.RecipeResult{},
		Version:      testBundlerVersion,
	}
	ctx := context.Background()
	out, err := g.Generate(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if out == nil || len(out.Files) == 0 {
		t.Fatalf("Generate() returned no files: %#v", out)
	}
}

// TestGenerate_NilRecipeResult asserts the deployer validates its required
// input.
func TestGenerate_NilRecipeResult(t *testing.T) {
	g := &Generator{Version: testBundlerVersion}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatalf("Generate() with nil RecipeResult expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("Generate() error code = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "RecipeResult is required") {
		t.Errorf("Generate() error message = %q, want substring %q",
			err.Error(), "RecipeResult is required")
	}
}

// TestGenerate_WithChecksums asserts that IncludeChecksums wires
// checksum.WriteChecksums into the output and produces a checksums.txt
// adjacent to helmfile.yaml.
func TestGenerate_WithChecksums(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2",
				"https://charts.jetstack.io"),
		),
		ComponentValues:  map[string]map[string]any{"cert-manager": {}},
		Version:          testBundlerVersion,
		IncludeChecksums: true,
	}
	outputDir := t.TempDir()
	_, err := g.Generate(context.Background(), outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	checksumsPath := filepath.Join(outputDir, "checksums.txt")
	if _, statErr := os.Stat(checksumsPath); statErr != nil {
		t.Fatalf("checksums.txt missing: %v", statErr)
	}
}

// TestGenerate_WithDataFiles asserts external data files are picked up by
// output.AddDataFiles and contribute to the file list / total size.
func TestGenerate_WithDataFiles(t *testing.T) {
	outputDir := t.TempDir()
	dataFile := filepath.Join(outputDir, "extra.yaml")
	if err := os.WriteFile(dataFile, []byte("key: value\n"), 0o600); err != nil {
		t.Fatalf("seed data file: %v", err)
	}
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2",
				"https://charts.jetstack.io"),
		),
		ComponentValues: map[string]map[string]any{"cert-manager": {}},
		Version:         testBundlerVersion,
		DataFiles:       []string{"extra.yaml"},
	}
	out, err := g.Generate(context.Background(), outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	found := false
	for _, f := range out.Files {
		if filepath.Base(f) == "extra.yaml" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("extra.yaml not present in output.Files: %v", out.Files)
	}
}

// TestGenerate_ContextCanceled asserts the deployer respects a context
// that's already canceled at entry, before touching the filesystem.
func TestGenerate_ContextCanceled(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2",
				"https://charts.jetstack.io"),
		),
		ComponentValues: map[string]map[string]any{"cert-manager": {}},
		Version:         testBundlerVersion,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.Generate(ctx, t.TempDir())
	if err == nil {
		t.Fatalf("Generate() with canceled context expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("Generate() error code = %v, want ErrCodeTimeout", err)
	}
}

// TestGenerate_MissingDataFile asserts the deployer surfaces a structured
// error when --data references a path that does not exist (defense against
// silent checksum gaps).
func TestGenerate_MissingDataFile(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2",
				"https://charts.jetstack.io"),
		),
		ComponentValues: map[string]map[string]any{"cert-manager": {}},
		Version:         testBundlerVersion,
		DataFiles:       []string{"does-not-exist.yaml"},
	}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatalf("Generate() with missing data file expected error, got nil")
	}
	// AddDataFiles wraps the stat failure as ErrCodeInternal — assert the
	// code rather than just non-nil so a regression that surfaces an
	// uncoded error here fails this test.
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("Generate() error code = %v, want ErrCodeInternal", err)
	}
}

// TestRepositoryFor_OCIStripsScheme asserts that an OCI-prefixed source URL
// from the recipe is stored in the helmfile repository entry as bare
// host+path. Helmfile prepends `oci://` itself when `oci: true` is set —
// an `oci://`-prefixed `url` causes the doubled-scheme bug seen during
// real `helmfile build` integration testing of issue #632.
func TestRepositoryFor_OCIStripsScheme(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantURL string
		wantOCI bool
	}{
		{"oci with scheme", "oci://ghcr.io/example/charts", "ghcr.io/example/charts", true},
		{"https unchanged", "https://charts.jetstack.io", "https://charts.jetstack.io", false},
		{"http unchanged", "http://example.com/charts", "http://example.com/charts", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := repositoryFor(&localformat.Upstream{Repo: tt.repo})
			if r.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", r.URL, tt.wantURL)
			}
			if r.OCI != tt.wantOCI {
				t.Errorf("OCI = %v, want %v", r.OCI, tt.wantOCI)
			}
		})
	}
}

// TestBuildHelmfile_NilUpstream asserts buildHelmfile errors out when a
// KindUpstreamHelm folder arrives without the Upstream pointer set — a
// programmer-error case from localformat that should fail loud rather
// than emit `chart: nil/<chart>`.
func TestBuildHelmfile_NilUpstream(t *testing.T) {
	folders := []localformat.Folder{
		{Index: 1, Dir: "001-x", Kind: localformat.KindUpstreamHelm, Name: "x", Parent: "x", Upstream: nil},
	}
	_, err := buildHelmfile(folders, map[string]string{"x": "ns"}, nil)
	if err == nil {
		t.Fatalf("buildHelmfile with nil Upstream expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error code = %v, want ErrCodeInternal", err)
	}
	if !strings.Contains(err.Error(), "KindUpstreamHelm but Upstream is nil") {
		t.Errorf("error message = %q, want substring 'KindUpstreamHelm but Upstream is nil'", err.Error())
	}
}

// TestBuildHelmfile_UnsupportedKind asserts buildHelmfile rejects an
// unknown FolderKind value rather than silently emitting a release with
// an empty chart.
func TestBuildHelmfile_UnsupportedKind(t *testing.T) {
	folders := []localformat.Folder{
		{Index: 1, Dir: "001-x", Kind: localformat.FolderKind(99), Name: "x", Parent: "x"},
	}
	_, err := buildHelmfile(folders, map[string]string{"x": "ns"}, nil)
	if err == nil {
		t.Fatalf("buildHelmfile with unsupported kind expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "unsupported folder kind") {
		t.Errorf("error message = %q, want substring 'unsupported folder kind'", err.Error())
	}
}

// TestNeedsRef covers the cross-namespace needs: format. Helmfile resolves
// a bare "<name>" only within the dependent release's own namespace, so a
// release in cert-manager/ pointing at agentgateway-crds-post (which lives
// in agentgateway-system/) silently fails to resolve. needsRef must emit
// "<namespace>/<name>" whenever the dependent and dependency namespaces
// differ, and bare "<name>" when they match (helmfile community
// convention for in-namespace chains).
func TestNeedsRef(t *testing.T) {
	tests := []struct {
		name        string
		dependentNS string
		depNS       string
		depName     string
		want        string
	}{
		{"same namespace", "cert-manager", "cert-manager", "cert-manager-crds", "cert-manager-crds"},
		{"cross namespace", "cert-manager", "agentgateway-system", "agentgateway-crds-post", "agentgateway-system/agentgateway-crds-post"},
		{"empty dep namespace falls back to bare", "cert-manager", "", "foo", "foo"},
		{"identical empty namespaces", "", "", "foo", "foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsRef(tt.dependentNS, tt.depNS, tt.depName); got != tt.want {
				t.Errorf("needsRef(dep=%q, depNS=%q, name=%q) = %q, want %q",
					tt.dependentNS, tt.depNS, tt.depName, got, tt.want)
			}
		})
	}
}

// TestBuildHelmfile_CrossNamespaceNeeds asserts the integrated wiring:
// when two consecutive releases live in different namespaces, the dependent
// release's needs: entry is namespace-qualified end-to-end (struct → YAML).
// Locks the fix for the silent-miss bug caught by the Kind smoke test.
func TestBuildHelmfile_CrossNamespaceNeeds(t *testing.T) {
	folders := []localformat.Folder{
		{Index: 1, Dir: "001-cert-manager", Kind: localformat.KindUpstreamHelm, Name: "cert-manager", Parent: "cert-manager",
			Upstream: &localformat.Upstream{Chart: "cert-manager", Repo: "https://charts.jetstack.io", Version: "v1.17.2"}},
		{Index: 2, Dir: "002-kube-prometheus-stack", Kind: localformat.KindUpstreamHelm, Name: "kube-prometheus-stack", Parent: "kube-prometheus-stack",
			Upstream: &localformat.Upstream{Chart: "kube-prometheus-stack", Repo: "https://prometheus-community.github.io/helm-charts", Version: "84.4.0"}},
	}
	ns := map[string]string{
		"cert-manager":          "cert-manager",
		"kube-prometheus-stack": "monitoring",
	}
	doc, err := buildHelmfile(folders, ns, nil)
	if err != nil {
		t.Fatalf("buildHelmfile() error = %v", err)
	}
	if len(doc.Releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(doc.Releases))
	}
	if doc.Releases[0].Needs != nil {
		t.Errorf("release[0] (cert-manager) should have no needs, got %v", doc.Releases[0].Needs)
	}
	want := "cert-manager/cert-manager"
	if got := doc.Releases[1].Needs; len(got) != 1 || got[0] != want {
		t.Errorf("release[1] (kube-prometheus-stack) needs = %v, want [%q] — bare name here causes helmfile to look in monitoring/ for a release that lives in cert-manager/", got, want)
	}
}

// TestBuildHelmfile_SameNamespaceNeedsBare asserts the complement: when the
// predecessor shares a namespace with the dependent, needs: is emitted as
// a bare name (matches helmfile community convention; cleaner diffs).
func TestBuildHelmfile_SameNamespaceNeedsBare(t *testing.T) {
	folders := []localformat.Folder{
		{Index: 1, Dir: "001-gpu-operator", Kind: localformat.KindUpstreamHelm, Name: "gpu-operator", Parent: "gpu-operator",
			Upstream: &localformat.Upstream{Chart: "gpu-operator", Repo: "https://helm.ngc.nvidia.com/nvidia", Version: "v25.3.3"}},
		{Index: 2, Dir: "002-gpu-operator-post", Kind: localformat.KindLocalHelm, Name: "gpu-operator-post", Parent: "gpu-operator"},
	}
	ns := map[string]string{"gpu-operator": "gpu-operator"}
	doc, err := buildHelmfile(folders, ns, nil)
	if err != nil {
		t.Fatalf("buildHelmfile() error = %v", err)
	}
	if got := doc.Releases[1].Needs; len(got) != 1 || got[0] != "gpu-operator" {
		t.Errorf("release[1] needs = %v, want [\"gpu-operator\"] — same-namespace chains should use the bare name form",
			got)
	}
}

// TestBuildHelmfile_CreateNamespaceFromFolder asserts that buildHelmfile
// emits a per-release `createNamespace: false` override when a folder's
// CreateNamespace flag is false (the Talos privileged-namespace pattern),
// and emits no override (relying on helmDefaults.createNamespace: true)
// otherwise. The localformat writer already encodes the
// chart-owns-Namespace decision; the helmfile deployer must honor it or
// helm refuses to import the namespace it created out-of-band.
func TestBuildHelmfile_CreateNamespaceFromFolder(t *testing.T) {
	folders := []localformat.Folder{
		// Pre-injection folder that ships its own Namespace (Talos pattern):
		// localformat would set CreateNamespace=false; expect the override.
		{Index: 1, Dir: "001-gpu-operator-pre", Kind: localformat.KindLocalHelm,
			Name: "gpu-operator-pre", Parent: "gpu-operator", CreateNamespace: false},
		// Primary upstream-helm folder: no Namespace conflict possible from
		// AICR's perspective; expect no per-release override.
		{Index: 2, Dir: "002-gpu-operator", Kind: localformat.KindUpstreamHelm,
			Name: "gpu-operator", Parent: "gpu-operator", CreateNamespace: true,
			Upstream: &localformat.Upstream{Chart: "gpu-operator", Repo: "https://helm.ngc.nvidia.com/nvidia", Version: "v25.3.3"}},
	}
	ns := map[string]string{"gpu-operator": "privileged-gpu-operator"}
	doc, err := buildHelmfile(folders, ns, nil)
	if err != nil {
		t.Fatalf("buildHelmfile() error = %v", err)
	}
	if len(doc.Releases) != 2 {
		t.Fatalf("expected 2 releases, got %d", len(doc.Releases))
	}
	if doc.Releases[0].CreateNamespace == nil || *doc.Releases[0].CreateNamespace {
		t.Errorf("release[0] (gpu-operator-pre) CreateNamespace = %v, want pointer to false — "+
			"the chart owns the Namespace and helm must not create it out-of-band",
			doc.Releases[0].CreateNamespace)
	}
	if doc.Releases[1].CreateNamespace != nil {
		t.Errorf("release[1] (gpu-operator) CreateNamespace = %v, want nil — "+
			"helmDefaults.createNamespace: true covers the common case; no per-release override should be emitted",
			doc.Releases[1].CreateNamespace)
	}
}

// TestGenerate_NamespaceOwningPreManifest is the integration counterpart:
// a pre-manifest containing a Namespace resource flows end-to-end through
// localformat.Write → buildHelmfile and surfaces as `createNamespace: false`
// on the pre-release in the rendered helmfile.yaml. This locks the
// os-talos failure mode the helmfile deployer originally hit (release
// creates namespace via --create-namespace, then the chart's Namespace
// template can't claim it).
func TestGenerate_NamespaceOwningPreManifest(t *testing.T) {
	namespaceManifest := []byte("apiVersion: v1\n" +
		"kind: Namespace\n" +
		"metadata:\n" +
		"  name: privileged-gpu-operator\n" +
		"  labels:\n" +
		"    pod-security.kubernetes.io/enforce: privileged\n")
	g := &Generator{
		RecipeResult: recipeWith(
			recipe.ComponentRef{
				Name:      "gpu-operator",
				Namespace: "privileged-gpu-operator",
				Chart:     "gpu-operator",
				Version:   "v25.3.3",
				Source:    "https://helm.ngc.nvidia.com/nvidia",
				Type:      recipe.ComponentTypeHelm,
			},
		),
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {"talos-namespace.yaml": namespaceManifest},
		},
		Version: testBundlerVersion,
	}
	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, fileHelmfile))
	if err != nil {
		t.Fatalf("read helmfile.yaml: %v", err)
	}
	var doc Helmfile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse helmfile.yaml: %v\n--- content ---\n%s", err, data)
	}
	if len(doc.Releases) != 2 {
		t.Fatalf("expected 2 releases (gpu-operator-pre + gpu-operator), got %d", len(doc.Releases))
	}
	// release[0] is the injected -pre with the Namespace template.
	if got := doc.Releases[0].Name; got != "gpu-operator-pre" {
		t.Fatalf("release[0].name = %q, want %q", got, "gpu-operator-pre")
	}
	if doc.Releases[0].CreateNamespace == nil || *doc.Releases[0].CreateNamespace {
		t.Errorf("release[0] (gpu-operator-pre) CreateNamespace = %v, want pointer to false — "+
			"pre-manifest ships a kind: Namespace, so helm must not create it out-of-band",
			doc.Releases[0].CreateNamespace)
	}
	// release[1] is the primary upstream-helm; no override expected.
	if doc.Releases[1].CreateNamespace != nil {
		t.Errorf("release[1] (gpu-operator) CreateNamespace = %v, want nil",
			doc.Releases[1].CreateNamespace)
	}
	// Global default is unchanged — every other release still benefits.
	if !doc.HelmDefaults.CreateNamespace {
		t.Error("helmDefaults.createNamespace should remain true; only the affected release overrides it")
	}
}

// TestComponentOverrides_ParityWithHelmDeployScript guards against silent
// drift between componentOverrides in this package and the hardcoded
// ASYNC_COMPONENTS / COMPONENT_HELM_TIMEOUT case block in
// pkg/bundler/deployer/helm/templates/deploy.sh.tmpl. Either side changing
// without the other must fail this test until the duplication is unified.
//
// Specifically, for every component name in componentOverrides:
//   - Wait==false  ⟺  the name appears in ASYNC_COMPONENTS="…"
//   - Timeout != 0 ⟺  a "<name>) COMPONENT_HELM_TIMEOUT="<n>m" ;;"
//     case exists with matching duration
//
// The reverse direction is also asserted: any component in either
// deploy.sh.tmpl construct must be present in componentOverrides.
func TestComponentOverrides_ParityWithHelmDeployScript(t *testing.T) {
	scriptPath := filepath.Join("..", "helm", "templates", "deploy.sh.tmpl")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	script := string(body)

	// Parse ASYNC_COMPONENTS="<space-separated names>".
	asyncRe := regexp.MustCompile(`(?m)^ASYNC_COMPONENTS="([^"]*)"`)
	asyncMatch := asyncRe.FindStringSubmatch(script)
	if asyncMatch == nil {
		t.Fatalf("could not find ASYNC_COMPONENTS=\"…\" in %s", scriptPath)
	}
	asyncNames := map[string]bool{}
	for _, n := range strings.Fields(asyncMatch[1]) {
		asyncNames[n] = true
	}

	// Parse the "<name>) COMPONENT_HELM_TIMEOUT=\"<N>m\"" case block.
	// Pattern is a stable two-line shape in deploy.sh.tmpl.
	caseRe := regexp.MustCompile(`(?m)^\s*([a-z0-9-]+)\)\s*\n\s*COMPONENT_HELM_TIMEOUT="(\d+)m"`)
	scriptTimeouts := map[string]time.Duration{}
	for _, m := range caseRe.FindAllStringSubmatch(script, -1) {
		mins, parseErr := time.ParseDuration(m[2] + "m")
		if parseErr != nil {
			t.Fatalf("parse %sm: %v", m[2], parseErr)
		}
		scriptTimeouts[m[1]] = mins
	}

	// Forward direction: every Go override must match deploy.sh.tmpl.
	for name, ov := range componentOverrides {
		if !ov.wait && !asyncNames[name] {
			t.Errorf("componentOverrides[%q].wait=false but %q not in ASYNC_COMPONENTS=%q "+
				"(update deploy.sh.tmpl or the Go map together)",
				name, name, asyncMatch[1])
		}
		if ov.timeout != 0 {
			want := time.Duration(ov.timeout) * time.Second
			got, ok := scriptTimeouts[name]
			if !ok {
				t.Errorf("componentOverrides[%q].timeout=%v but no COMPONENT_HELM_TIMEOUT case "+
					"for %q in deploy.sh.tmpl (update deploy.sh.tmpl or the Go map together)",
					name, want, name)
			} else if got != want {
				t.Errorf("componentOverrides[%q].timeout=%v but deploy.sh.tmpl case sets %v "+
					"(update deploy.sh.tmpl or the Go map together)",
					name, want, got)
			}
		}
	}

	// Reverse direction: every name in either deploy.sh.tmpl construct must
	// exist in componentOverrides.
	for name := range asyncNames {
		if _, ok := componentOverrides[name]; !ok {
			t.Errorf("deploy.sh.tmpl ASYNC_COMPONENTS lists %q but componentOverrides has no entry "+
				"(helmfile bundles would --wait on a release the helm deployer treats as async)", name)
		}
	}
	for name := range scriptTimeouts {
		if _, ok := componentOverrides[name]; !ok {
			t.Errorf("deploy.sh.tmpl has COMPONENT_HELM_TIMEOUT case for %q but componentOverrides has no entry "+
				"(helmfile bundles would use the global timeout for a release the helm deployer special-cases)", name)
		}
	}
}

// TestSanitizeRepoAlias_Edges covers the slug edge cases (empty input,
// >63 char truncation, double-hyphen collapsing) so a future URL with
// embedded ports or query strings doesn't silently produce a malformed
// helmfile repository name.
func TestSanitizeRepoAlias_Edges(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty input", "", "default-repo"},
		{"only scheme", "https://", "default-repo"},
		{"non-alphanum collapsed", "https://EXAMPLE.com/path_with_underscores", "example-com-path-with-underscores"},
		{"truncated >63", "https://" + strings.Repeat("a", 100), strings.Repeat("a", 63)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeRepoAlias(tt.in); got != tt.want {
				t.Errorf("sanitizeRepoAlias(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ---------- helpers ----------

func ref(name, ns, chart, version, source string) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name:      name,
		Namespace: ns,
		Chart:     chart,
		Version:   version,
		Source:    source,
		Type:      recipe.ComponentTypeHelm,
	}
}

func recipeWith(refs ...recipe.ComponentRef) *recipe.RecipeResult {
	r := &recipe.RecipeResult{}
	r.Metadata.Version = testBundlerVersion
	r.ComponentRefs = refs
	order := make([]string, 0, len(refs))
	for _, ref := range refs {
		order = append(order, ref.Name)
	}
	r.DeploymentOrder = order
	return r
}

func runScenario(t *testing.T, g *Generator, scenario string, goldenFiles []string) {
	t.Helper()
	outputDir := t.TempDir()
	out, err := g.Generate(context.Background(), outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if out == nil || len(out.Files) == 0 {
		t.Fatalf("Generate() returned no files")
	}

	// Validate helmfile.yaml is parseable as a YAML document. Helmfile's
	// own validator (`helmfile lint`) is not available in unit tests, but
	// a YAML parse plus a few shape assertions catches the common
	// regressions (key drift, indentation bugs from template changes).
	helmfilePath := filepath.Join(outputDir, fileHelmfile)
	assertHelmfileShape(t, helmfilePath)

	for _, rel := range goldenFiles {
		assertGolden(t, outputDir, filepath.Join("testdata", scenario), rel)
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
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s",
			relPath, got, want)
	}
}

// assertHelmfileShape parses helmfile.yaml and verifies the document has
// the three top-level keys we always emit, plus that every release has
// the required name/namespace/chart fields. Catches indentation /
// missing-key regressions without locking the test to literal byte
// content (those are caught separately by the golden compare).
func assertHelmfileShape(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc Helmfile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v\n--- content ---\n%s", path, err, data)
	}
	if doc.HelmDefaults.Timeout == 0 {
		t.Errorf("helmDefaults.timeout should be non-zero, got 0")
	}
	for i, rel := range doc.Releases {
		if rel.Name == "" {
			t.Errorf("releases[%d].name is empty", i)
		}
		if rel.Namespace == "" {
			t.Errorf("releases[%d] (%s).namespace is empty", i, rel.Name)
		}
		if rel.Chart == "" {
			t.Errorf("releases[%d] (%s).chart is empty", i, rel.Name)
		}
		// KindUpstreamHelm releases must carry a version; KindLocalHelm
		// (chart: ./<dir>) must not (helmfile would otherwise reject the
		// version pin against a local chart).
		isLocal := strings.HasPrefix(rel.Chart, "./")
		switch {
		case isLocal && rel.Version != "":
			t.Errorf("releases[%d] (%s) is a local chart but has version=%q",
				i, rel.Name, rel.Version)
		case !isLocal && rel.Version == "":
			t.Errorf("releases[%d] (%s) references an upstream chart %q but has no version",
				i, rel.Name, rel.Chart)
		}
	}
}

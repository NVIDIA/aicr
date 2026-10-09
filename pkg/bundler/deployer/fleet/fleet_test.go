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

package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testBundlerVersion = "v1.0.0"

func ref(name, namespace, chart, version, source string) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name:      name,
		Namespace: namespace,
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

func readFleetYAML(t *testing.T, dir string) FleetYAML {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, fileFleetYAML))
	if err != nil {
		t.Fatalf("read fleet.yaml: %v", err)
	}
	var doc FleetYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse fleet.yaml: %v", err)
	}
	return doc
}

func readGitRepo(t *testing.T, dir string) GitRepo {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, fileGitRepo))
	if err != nil {
		t.Fatalf("read gitrepo.yaml: %v", err)
	}
	var doc GitRepo
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse gitrepo.yaml: %v", err)
	}
	return doc
}

func TestGenerate_UpstreamChain(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
			ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
		),
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"usePrecompiled": true}},
		},
		Version: testBundlerVersion,
		RepoURL: "https://example.com/fleet.git",
	}
	out := t.TempDir()
	res, err := g.Generate(context.Background(), out)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	cm := readFleetYAML(t, filepath.Join(out, "001-cert-manager"))
	if cm.Name != "aicr-cert-manager" || len(cm.DependsOn) != 0 {
		t.Errorf("cert-manager bundle = %+v, want name aicr-cert-manager and no dependsOn", cm)
	}
	gpu := readFleetYAML(t, filepath.Join(out, "002-gpu-operator"))
	if gpu.Name != "aicr-gpu-operator" {
		t.Errorf("gpu-operator bundle name = %q", gpu.Name)
	}
	if len(gpu.DependsOn) != 1 || gpu.DependsOn[0].Name != "aicr-cert-manager" {
		t.Errorf("gpu-operator dependsOn = %+v, want [aicr-cert-manager]", gpu.DependsOn)
	}
	if gpu.DefaultNamespace != "gpu-operator" || gpu.Labels[BundleLabel] != DefaultAppName {
		t.Errorf("gpu-operator namespace/labels = %q / %v", gpu.DefaultNamespace, gpu.Labels)
	}
	h := gpu.Helm
	if h.ReleaseName != "gpu-operator" || h.Repo != "https://helm.ngc.nvidia.com/nvidia" ||
		h.Chart != "gpu-operator" || h.Version != "v25.3.3" {

		t.Errorf("gpu-operator helm = %+v", h)
	}
	if !h.DisablePreProcess {
		t.Errorf("gpu-operator helm disablePreProcess = false, want true")
	}
	// No AICR deployer adopts objects owned by other releases; Fleet already
	// upgrades an existing release of the same name without the flag.
	if raw, readErr := os.ReadFile(filepath.Join(out, "002-gpu-operator", fileFleetYAML)); readErr != nil {
		t.Fatalf("read fleet.yaml: %v", readErr)
	} else if strings.Contains(string(raw), "takeOwnership") {
		t.Errorf("fleet.yaml sets takeOwnership:\n%s", raw)
	}
	for _, vf := range h.ValuesFiles {
		if _, statErr := os.Stat(filepath.Join(out, "002-gpu-operator", vf)); statErr != nil {
			t.Errorf("valuesFiles references missing file %s", vf)
		}
	}
	if len(h.ValuesFiles) == 0 || h.ValuesFiles[0] != fileValues {
		t.Errorf("valuesFiles = %v, want values.yaml first", h.ValuesFiles)
	}

	ignore, err := os.ReadFile(filepath.Join(out, "002-gpu-operator", fileFleetIgnore))
	if err != nil {
		t.Fatalf("read .fleetignore: %v", err)
	}
	for _, name := range []string{"install.sh", "upstream.env"} {
		if !strings.Contains(string(ignore), name) {
			t.Errorf(".fleetignore missing %s", name)
		}
	}

	gr := readGitRepo(t, out)
	if gr.Kind != "GitRepo" || gr.Metadata.Name != DefaultAppName || gr.Metadata.Namespace != DefaultNamespace {
		t.Errorf("gitrepo metadata = %+v %+v", gr.Kind, gr.Metadata)
	}
	if gr.Spec.Repo != "https://example.com/fleet.git" || gr.Spec.Branch != defaultTargetRevision {
		t.Errorf("gitrepo repo/branch = %q/%q", gr.Spec.Repo, gr.Spec.Branch)
	}
	if strings.Join(gr.Spec.Paths, ",") != "001-cert-manager,002-gpu-operator" {
		t.Errorf("gitrepo paths = %v", gr.Spec.Paths)
	}
	if len(gr.Spec.Targets) != 1 || gr.Spec.Targets[0].ClusterSelector == nil ||
		gr.Spec.Targets[0].ClusterSelector.MatchLabels[BundleLabel] != DefaultAppName {

		t.Errorf("gitrepo targets = %+v, want label selector", gr.Spec.Targets)
	}

	if res.Entrypoint != fileGitRepo {
		t.Errorf("Entrypoint = %q, want %q", res.Entrypoint, fileGitRepo)
	}
	if len(res.Releases) != 2 || res.Releases[1].Manifest != "002-gpu-operator/fleet.yaml" {
		t.Errorf("Releases = %+v", res.Releases)
	}
	if res.Source.AppName != DefaultAppName || res.Source.RepoURL != "https://example.com/fleet.git" {
		t.Errorf("Source = %+v", res.Source)
	}
}

func TestGenerate_OCIChartInRepo(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("k8s-aibom", "k8s-aibom-system", "k8s-aibom", "1.3.0", "oci://ghcr.io/googlecloudplatform/charts"),
		),
		Version: testBundlerVersion,
	}
	out := t.TempDir()
	if _, err := g.Generate(context.Background(), out); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	doc := readFleetYAML(t, filepath.Join(out, "001-k8s-aibom"))
	if doc.Helm.Repo != "oci://ghcr.io/googlecloudplatform/charts/k8s-aibom" || doc.Helm.Chart != "" {
		t.Errorf("OCI helm repo/chart = %q/%q, want full reference in repo and empty chart", doc.Helm.Repo, doc.Helm.Chart)
	}
	if gr := readGitRepo(t, out); gr.Spec.Repo != defaultRepoURL {
		t.Errorf("gitrepo repo = %q, want placeholder %q", gr.Spec.Repo, defaultRepoURL)
	}
}

func TestGenerate_PostManifestsLocalChart(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
		),
		ComponentPostManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: gpu-operator\n"),
			},
		},
		Version: testBundlerVersion,
	}
	out := t.TempDir()
	if _, err := g.Generate(context.Background(), out); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	postDir := filepath.Join(out, "002-gpu-operator-post")
	if _, err := os.Stat(filepath.Join(postDir, "Chart.yaml")); err != nil {
		t.Fatalf("expected local chart in -post folder: %v", err)
	}
	post := readFleetYAML(t, postDir)
	if post.Helm.Repo != "" || post.Helm.Chart != "." || post.Helm.Version != "" {
		t.Errorf("local chart helm = %+v, want chart \".\" and no repo/version", post.Helm)
	}
	if post.Helm.ReleaseName != "gpu-operator-post" {
		t.Errorf("local chart releaseName = %q", post.Helm.ReleaseName)
	}
	if len(post.DependsOn) != 1 || post.DependsOn[0].Name != "aicr-gpu-operator" {
		t.Errorf("-post dependsOn = %+v, want [aicr-gpu-operator]", post.DependsOn)
	}
}

func TestGenerate_FleetLocalTargetsLocalCluster(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
		),
		Version:        testBundlerVersion,
		AppName:        "gpu-stack",
		Namespace:      localNamespace,
		TargetRevision: "release",
	}
	out := t.TempDir()
	res, err := g.Generate(context.Background(), out)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	gr := readGitRepo(t, out)
	if gr.Metadata.Name != "gpu-stack" || gr.Metadata.Namespace != localNamespace || gr.Spec.Branch != "release" {
		t.Errorf("gitrepo = %+v %+v", gr.Metadata, gr.Spec)
	}
	if len(gr.Spec.Targets) != 1 || gr.Spec.Targets[0].ClusterName != "local" || gr.Spec.Targets[0].ClusterSelector != nil {
		t.Errorf("targets = %+v, want clusterName local", gr.Spec.Targets)
	}
	if doc := readFleetYAML(t, filepath.Join(out, "001-cert-manager")); doc.Name != "gpu-stack-cert-manager" {
		t.Errorf("bundle name = %q, want app-name prefix", doc.Name)
	}
	for _, step := range res.DeploymentSteps {
		if strings.Contains(step, "kubectl label") {
			t.Errorf("fleet-local steps should not ask to label clusters: %q", step)
		}
	}
}

func TestGenerate_Errors(t *testing.T) {
	longName := strings.Repeat("a", 50)
	tests := []struct {
		name     string
		gen      *Generator
		ctx      func() context.Context
		want     string
		wantCode errors.ErrorCode
	}{
		{
			name: "nil recipe",
			gen:  &Generator{},
			want: "RecipeResult is required",
		},
		{
			name: "invalid app name",
			gen:  &Generator{RecipeResult: recipeWith(), AppName: "Not_Valid"},
			want: "invalid Fleet app name",
		},
		{
			// The CLI validates --fleet-namespace, but an SDK caller reaches
			// Generate directly; gitrepo.yaml would fail at kubectl apply.
			name:     "invalid namespace",
			gen:      &Generator{RecipeResult: recipeWith(), Namespace: "team.a"},
			want:     "invalid Fleet namespace",
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "bundle name too long",
			gen: &Generator{
				RecipeResult: recipeWith(ref("nvidia-dra-driver-gpu", "nvidia-dra-driver", "dra", "v1.0.0", "https://example.com")),
				AppName:      longName,
			},
			want: "exceeds",
		},
		{
			name: "context canceled",
			gen:  &Generator{RecipeResult: recipeWith()},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want:     "context canceled",
			wantCode: errors.ErrCodeCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx()
			}
			_, err := tt.gen.Generate(ctx, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Generate() error = %v, want containing %q", err, tt.want)
			}
			if tt.wantCode != "" && !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("Generate() error = %v, want code %s", err, tt.wantCode)
			}
		})
	}
}

func readHelmOps(t *testing.T, dir string) []HelmOp {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, fileHelmOps))
	if err != nil {
		t.Fatalf("open helmops.yaml: %v", err)
	}
	defer f.Close()
	var out []HelmOp
	dec := yaml.NewDecoder(f)
	for {
		var ho HelmOp
		if err := dec.Decode(&ho); err != nil {
			break
		}
		out = append(out, ho)
	}
	return out
}

func TestGenerate_HelmOpMode(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
			ref("k8s-aibom", "k8s-aibom-system", "k8s-aibom", "1.3.0", "oci://ghcr.io/googlecloudplatform/charts"),
		),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
		},
		Version: testBundlerVersion,
		Mode:    ModeHelmOp,
	}
	out := t.TempDir()
	res, err := g.Generate(context.Background(), out)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if res.Entrypoint != fileHelmOps {
		t.Errorf("Entrypoint = %q, want %q", res.Entrypoint, fileHelmOps)
	}
	for _, absent := range []string{fileGitRepo, "001-cert-manager/" + fileFleetYAML} {
		if _, statErr := os.Stat(filepath.Join(out, absent)); statErr == nil {
			t.Errorf("helmop mode wrote %s", absent)
		}
	}
	if res.Source.RepoURL != "" || res.Source.AppName != DefaultAppName {
		t.Errorf("Source = %+v, want only AppName", res.Source)
	}

	hos := readHelmOps(t, out)
	if len(hos) != 2 {
		t.Fatalf("got %d HelmOps, want 2", len(hos))
	}
	cm, aibom := hos[0], hos[1]
	if cm.Kind != "HelmOp" || cm.Metadata.Name != "aicr-cert-manager" || cm.Metadata.Namespace != DefaultNamespace {
		t.Errorf("cert-manager HelmOp metadata = %+v %+v", cm.Kind, cm.Metadata)
	}
	if cm.Spec.Helm.Repo != "https://charts.jetstack.io" || cm.Spec.Helm.Chart != "cert-manager" {
		t.Errorf("cert-manager helm = %+v", cm.Spec.Helm)
	}
	crds, _ := cm.Spec.Helm.Values["crds"].(map[string]any)
	if crds["enabled"] != true {
		t.Errorf("cert-manager values not inlined: %v", cm.Spec.Helm.Values)
	}
	if aibom.Spec.Helm.Repo != "oci://ghcr.io/googlecloudplatform/charts/k8s-aibom" || aibom.Spec.Helm.Chart != "" {
		t.Errorf("OCI HelmOp helm = %+v", aibom.Spec.Helm)
	}
	if len(aibom.Spec.DependsOn) != 1 || aibom.Spec.DependsOn[0].Name != "aicr-cert-manager" {
		t.Errorf("k8s-aibom dependsOn = %+v", aibom.Spec.DependsOn)
	}
	if len(aibom.Spec.Targets) != 1 || aibom.Spec.Targets[0].ClusterSelector == nil {
		t.Errorf("targets = %+v, want label selector", aibom.Spec.Targets)
	}
	if raw, readErr := os.ReadFile(filepath.Join(out, fileHelmOps)); readErr != nil {
		t.Fatalf("read helmops.yaml: %v", readErr)
	} else if strings.Contains(string(raw), "takeOwnership") {
		t.Errorf("helmops.yaml sets takeOwnership:\n%s", raw)
	}
	if res.Releases[1].Manifest != fileHelmOps {
		t.Errorf("Releases[1].Manifest = %q", res.Releases[1].Manifest)
	}
}

func TestGenerate_HelmOpModeRejects(t *testing.T) {
	gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
	tests := []struct {
		name string
		gen  *Generator
		want string
	}{
		{
			name: "local chart",
			gen: &Generator{
				RecipeResult: recipeWith(gpu),
				ComponentPostManifests: map[string]map[string][]byte{
					"gpu-operator": {"components/gpu-operator/manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: gpu-operator\n")},
				},
				Mode: ModeHelmOp,
			},
			want: "local chart",
		},
		{
			name: "vendored charts",
			gen:  &Generator{RecipeResult: recipeWith(gpu), VendorCharts: true, Mode: ModeHelmOp},
			want: "vendored charts",
		},
		{
			name: "unknown mode",
			gen:  &Generator{RecipeResult: recipeWith(gpu), Mode: "bogus"},
			want: "invalid Fleet mode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.gen.Generate(context.Background(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Generate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestDeepMerge(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"x": 1, "y": 2}, "b": 1}
	src := map[string]any{"a": map[string]any{"y": 3}, "c": 4}
	got := deepMerge(dst, src)
	a := got["a"].(map[string]any)
	if a["x"] != 1 || a["y"] != 3 || got["b"] != 1 || got["c"] != 4 {
		t.Errorf("deepMerge = %v", got)
	}
}

func TestGenerate_HelmOpRejectionWritesNothing(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
		),
		ComponentPostManifests: map[string]map[string][]byte{
			"gpu-operator": {"components/gpu-operator/manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: gpu-operator\n")},
		},
		Mode: ModeHelmOp,
	}
	out := filepath.Join(t.TempDir(), "bundle")
	if _, err := g.Generate(context.Background(), out); err == nil || !strings.Contains(err.Error(), "gpu-operator") {
		t.Fatalf("Generate() error = %v, want rejection naming gpu-operator", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("rejected helmop bundle left output behind: stat err = %v", err)
	}
}

func TestGenerate_CRDOwnersNamed(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("k8s-aibom", "k8s-aibom-system", "k8s-aibom", "1.5.1", "oci://ghcr.io/googlecloudplatform/charts"),
		),
		Version: testBundlerVersion,
	}
	out := t.TempDir()
	res, err := g.Generate(context.Background(), out)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(out, fileReadme))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if !strings.Contains(string(readme), "- k8s-aibom") {
		t.Errorf("README does not list the CRD-owning component:\n%s", readme)
	}
	var noted bool
	for _, n := range res.DeploymentNotes {
		if strings.Contains(n, "k8s-aibom") && strings.Contains(n, "CRDs") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("DeploymentNotes = %v, want a CRD note naming k8s-aibom", res.DeploymentNotes)
	}
}

func TestGenerate_ReadmeMatchesMode(t *testing.T) {
	for _, mode := range []string{ModeGitRepo, ModeHelmOp} {
		t.Run(mode, func(t *testing.T) {
			g := &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
				),
				DynamicValues: map[string][]string{"cert-manager": {"replicaCount"}},
				Version:       testBundlerVersion,
				Mode:          mode,
			}
			out := t.TempDir()
			res, err := g.Generate(context.Background(), out)
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			readme, err := os.ReadFile(filepath.Join(out, fileReadme))
			if err != nil {
				t.Fatalf("read README: %v", err)
			}
			text := string(readme) + strings.Join(res.DeploymentNotes, "\n")
			mentionsFleetYAML := strings.Contains(text, "fleet.yaml")
			if mode == ModeHelmOp {
				if mentionsFleetYAML || strings.Contains(text, "before pushing") {
					t.Errorf("helmop README/notes give GitRepo-only instructions:\n%s", text)
				}
				if !strings.Contains(text, "spec.helm.values") {
					t.Errorf("helmop README/notes do not say where dynamic values live:\n%s", text)
				}
			} else if !mentionsFleetYAML {
				t.Errorf("gitrepo README does not mention fleet.yaml:\n%s", text)
			}
		})
	}
}

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
	return tgz, localformat.VendorRecord{
		Name:          c.Name,
		Chart:         chartName,
		Version:       c.Version,
		Repository:    c.Repository,
		SHA256:        hex.EncodeToString(sum[:]),
		TarballName:   tarball,
		PullerVersion: "stub v0.0.0",
	}, tarball, nil
}

// A vendored chart is a local chart whose templates read .Values. Fleet
// parses helm.valuesFiles only when helm.chart or helm.repo is set, so the
// folder must name itself as the chart or every value is dropped.
func TestGenerate_VendoredChartKeepsValues(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
		),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}, "replicaCount": 3},
		},
		DynamicValues: map[string][]string{"cert-manager": {"replicaCount"}},
		Version:       testBundlerVersion,
		VendorCharts:  true,
		Puller:        &stubChartPuller{},
	}
	out := t.TempDir()
	if _, err := g.Generate(context.Background(), out); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	dir := filepath.Join(out, "001-cert-manager")
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
		t.Fatalf("vendored folder has no Chart.yaml: %v", err)
	}
	doc := readFleetYAML(t, dir)
	if doc.Helm.Chart != "." || doc.Helm.Repo != "" || doc.Helm.Version != "" {
		t.Errorf("vendored helm = %+v, want chart \".\" and no repo/version", doc.Helm)
	}
	if strings.Join(doc.Helm.ValuesFiles, ",") != fileValues+","+fileClusterValues {
		t.Errorf("valuesFiles = %v, want [%s %s]", doc.Helm.ValuesFiles, fileValues, fileClusterValues)
	}
}

// Fleet copies the bundle name into a label value, so an over-long
// <app>-<release> is rejected, and before anything is written: a failure on
// the last folder must not leave the earlier ones behind.
func TestGenerate_LongBundleNameWritesNothing(t *testing.T) {
	gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
	postCM := map[string]map[string][]byte{
		"gpu-operator": {"components/gpu-operator/manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: gpu-operator\n")},
	}
	tests := []struct {
		name string
		gen  *Generator
		want string
	}{
		{
			// The last component pushes past the limit; the first fits.
			name: "last component",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
					ref("k8s-ephemeral-storage-metrics", "monitoring", "k8s-ephemeral-storage-metrics", "1.0.0", "https://example.com"),
				),
				AppName: strings.Repeat("a", 34),
			},
			want: "k8s-ephemeral-storage-metrics",
		},
		{
			// aicr-x...-gpu-operator fits; only the injected -post folder
			// exceeds the limit.
			name: "post folder",
			gen: &Generator{
				RecipeResult:           recipeWith(gpu),
				ComponentPostManifests: postCM,
				AppName:                strings.Repeat("a", maxBundleNameLen-len("-gpu-operator")),
			},
			want: "gpu-operator-post",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "bundle")
			_, err := tt.gen.Generate(context.Background(), out)
			if err == nil || !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Generate() error = %v, want a length error naming %s", err, tt.want)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Errorf("rejected bundle left output behind: stat err = %v", statErr)
			}
		})
	}
}

func TestGenerate_BundleNameAtLimit(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
		),
		AppName: strings.Repeat("a", maxBundleNameLen-len("-gpu-operator")),
	}
	if _, err := g.Generate(context.Background(), t.TempDir()); err != nil {
		t.Errorf("Generate() error = %v, want a %d-character bundle name accepted", err, maxBundleNameLen)
	}
}

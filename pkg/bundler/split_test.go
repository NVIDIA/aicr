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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"sigs.k8s.io/yaml"
)

// splitTestRecipe is a small recipe with two core and two ops components,
// a shared namespace (monitoring) across the classes, and only ops → core
// edges, so it partitions cleanly.
func splitTestRecipe() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		APIVersion: "aicr.run/v1",
		Kind:       "RecipeResult",
		Metadata:   recipe.RecipeResultMetadata{Version: "v0.1.0"},
		Criteria:   &recipe.Criteria{Service: recipe.CriteriaServiceEKS, Accelerator: "h100", Intent: "training"},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name: "cert-manager", Namespace: "cert-manager", Version: "v1.19.1", Type: "helm",
				Source: "https://charts.jetstack.io", Chart: "cert-manager",
			},
			{
				Name: "prometheus-operator-crds", Namespace: "monitoring", Version: "24.0.1", Type: "helm",
				Source: "https://prometheus-community.github.io/helm-charts", Chart: "prometheus-operator-crds",
			},
			{
				Name: "kube-prometheus-stack", Namespace: "monitoring", Version: "81.1.1", Type: "helm",
				Source: "https://prometheus-community.github.io/helm-charts", Chart: "kube-prometheus-stack",
				DependencyRefs: []string{"prometheus-operator-crds"},
			},
			{
				Name: "kai-scheduler", Namespace: "kai-scheduler", Version: "v0.11.3", Type: "helm",
				Source: "oci://ghcr.io/nvidia/kai-scheduler", Chart: "kai-scheduler",
				DependencyRefs: []string{"cert-manager"},
			},
		},
		DeploymentOrder: []string{"cert-manager", "prometheus-operator-crds", "kube-prometheus-stack", "kai-scheduler"},
	}
}

func newSplitBundler(t *testing.T, opts ...config.Option) *DefaultBundler {
	t.Helper()
	b, err := New(WithConfig(config.NewConfig(append([]config.Option{config.WithSplit(true)}, opts...)...)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return b
}

// assertNoStageLeft fails when a staging directory survived under parent.
func assertNoStageLeft(t *testing.T, parent string) {
	t.Helper()
	stale, _ := filepath.Glob(filepath.Join(parent, splitStagePrefix+"*"))
	if len(stale) > 0 {
		t.Errorf("staging directories left behind: %v", stale)
	}
}

func TestMake_SplitRendersVerifiedSet(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dest := filepath.Join(parent, "set")

	out, err := newSplitBundler(t).Make(t.Context(), splitTestRecipe(), dest)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	if out.OutputDir != dest {
		t.Errorf("OutputDir = %q, want %q", out.OutputDir, dest)
	}
	assertNoStageLeft(t, parent)

	// Set root: recipe.yaml, split.yaml, checksums.txt; no deploy.sh or
	// bundle-info.yaml, which belong to a single bundle.
	for _, want := range []string{RecipeFileName, SplitFileName, checksum.ChecksumFileName} {
		if _, statErr := os.Stat(filepath.Join(dest, want)); statErr != nil {
			t.Errorf("set root missing %s: %v", want, statErr)
		}
	}
	for _, absent := range []string{"deploy.sh", "README.md", "bundle-info.yaml"} {
		if _, statErr := os.Stat(filepath.Join(dest, absent)); statErr == nil {
			t.Errorf("set root unexpectedly has %s", absent)
		}
	}

	// One directory per class, each a deployable Helm bundle without its own
	// checksums.
	wantClassFolders := map[string][]string{
		"core": {"001-cert-manager", "002-prometheus-operator-crds"},
		"ops":  {"001-kube-prometheus-stack", "002-kai-scheduler"},
	}
	for class, folders := range wantClassFolders {
		classDir := filepath.Join(dest, class)
		for _, f := range append([]string{"deploy.sh", "README.md"}, folders...) {
			if _, statErr := os.Stat(filepath.Join(classDir, f)); statErr != nil {
				t.Errorf("%s class missing %s: %v", class, f, statErr)
			}
		}
		if _, statErr := os.Stat(filepath.Join(classDir, checksum.ChecksumFileName)); statErr == nil {
			t.Errorf("%s class has its own checksums.txt; the set root is the verification unit", class)
		}
	}

	// The root manifest covers the whole tree.
	if problems := checksum.VerifyChecksums(dest); len(problems) > 0 {
		t.Errorf("set verification failed: %v", problems)
	}

	// split.yaml records the digest, the class order and every component.
	data, readErr := os.ReadFile(filepath.Join(dest, SplitFileName))
	if readErr != nil {
		t.Fatalf("read split.yaml: %v", readErr)
	}
	var split BundleSplit
	if err := yaml.Unmarshal(data, &split); err != nil {
		t.Fatalf("parse split.yaml: %v\n%s", err, data)
	}
	if split.Kind != "BundleSplit" || split.APIVersion == "" {
		t.Errorf("split.yaml header = %s/%s", split.APIVersion, split.Kind)
	}
	if !strings.HasPrefix(split.RecipeDigest, "sha256:") || len(split.RecipeDigest) != len("sha256:")+64 {
		t.Errorf("recipeDigest = %q, want sha256:<64 hex>", split.RecipeDigest)
	}
	if got := strings.Join(split.ClassOrder, ","); got != "core,ops" {
		t.Errorf("classOrder = %q, want core,ops", got)
	}
	wantClasses := map[string]string{
		"cert-manager": "core", "prometheus-operator-crds": "core",
		"kube-prometheus-stack": "ops", "kai-scheduler": "ops",
	}
	for name, class := range wantClasses {
		if split.Components[name] != class {
			t.Errorf("components[%s] = %q, want %q", name, split.Components[name], class)
		}
	}
	if len(split.Components) != len(wantClasses) {
		t.Errorf("components = %v, want exactly %d entries", split.Components, len(wantClasses))
	}

	// The ops class names what it shares with core and keys its CRD scan by
	// the component that ships the monitoring CRDs; the core class does not
	// list anything shared.
	opsReadme, _ := os.ReadFile(filepath.Join(dest, "ops", "README.md"))
	for _, want := range []string{"`ops` class", "`core`, then `ops`", "namespace `monitoring`", "CRD group `monitoring.coreos.com`"} {
		if !strings.Contains(string(opsReadme), want) {
			t.Errorf("ops README missing %q", want)
		}
	}
	coreReadme, _ := os.ReadFile(filepath.Join(dest, "core", "README.md"))
	if strings.Contains(string(coreReadme), "When uninstalling this class, keep") {
		t.Errorf("core README lists shared resources, but nothing precedes core")
	}
	coreDeploy, _ := os.ReadFile(filepath.Join(dest, "core", "deploy.sh"))
	if !strings.Contains(string(coreDeploy), `ORPHANED_CRD_GROUPS="${ORPHANED_CRD_GROUPS} monitoring.coreos.com"`) {
		t.Errorf("core deploy.sh does not scan monitoring.coreos.com, which prometheus-operator-crds installs")
	}
	opsDeploy, _ := os.ReadFile(filepath.Join(dest, "ops", "deploy.sh"))
	if strings.Contains(string(opsDeploy), "monitoring.coreos.com") {
		t.Errorf("ops deploy.sh scans monitoring.coreos.com, which core installs")
	}
	if !strings.Contains(string(opsDeploy), "Class ops of an AICR bundle set") {
		t.Errorf("ops deploy.sh lacks the class header")
	}

	// Deployment steps walk the classes in order under the final path.
	if out.Deployment == nil || len(out.Deployment.Steps) != 6 {
		t.Fatalf("Deployment steps = %+v, want 3 per class", out.Deployment)
	}
	if out.Deployment.Steps[0] != "cd "+filepath.Join(dest, "core") || out.Deployment.Steps[3] != "cd "+filepath.Join(dest, "ops") {
		t.Errorf("Deployment steps = %v", out.Deployment.Steps)
	}
	for _, f := range out.Results[0].Files {
		if !strings.HasPrefix(f, dest+string(filepath.Separator)) {
			t.Errorf("result file %q is not under the published destination", f)
		}
	}
}

func TestMake_SplitRejectsBeforeWriting(t *testing.T) {
	t.Parallel()

	withEdge := func(from, to string) *recipe.RecipeResult {
		rr := splitTestRecipe()
		for i := range rr.ComponentRefs {
			if rr.ComponentRefs[i].Name == from {
				rr.ComponentRefs[i].DependencyRefs = append(rr.ComponentRefs[i].DependencyRefs, to)
			}
		}
		return rr
	}
	tests := []struct {
		name     string
		recipe   func() *recipe.RecipeResult
		opts     []config.Option
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{
			name:     "edge from core to ops",
			recipe:   func() *recipe.RecipeResult { return withEdge("cert-manager", "kube-prometheus-stack") },
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  "cert-manager (core) → kube-prometheus-stack (ops)",
		},
		{
			name: "edge to a disabled later-class component is satisfied externally",
			recipe: func() *recipe.RecipeResult {
				rr := withEdge("cert-manager", "kube-prometheus-stack")
				rr.ComponentRefs[2].Overrides = map[string]any{"enabled": false}
				return rr
			},
			wantCode: errors.ErrCodeInvalidRequest,
			// kai-scheduler keeps ops non-empty, and the edge to the disabled
			// kube-prometheus-stack is satisfied outside the recipe, so the
			// split succeeds.
			wantMsg: "",
		},
		{
			name: "OCP recipe",
			recipe: func() *recipe.RecipeResult {
				rr := splitTestRecipe()
				rr.Criteria.Service = recipe.CriteriaServiceOCP
				return rr
			},
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  "does not support OCP",
		},
		{
			name: "component without a registry entry",
			recipe: func() *recipe.RecipeResult {
				rr := splitTestRecipe()
				rr.ComponentRefs = append(rr.ComponentRefs, recipe.ComponentRef{
					Name: "hand-authored", Namespace: "hand", Version: "1.0.0", Type: "helm",
					Source: "https://charts.example.com", Chart: "hand-authored",
				})
				rr.DeploymentOrder = append(rr.DeploymentOrder, "hand-authored")
				return rr
			},
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  `"hand-authored" has none`,
		},
		{
			name:   "all ops components disabled at bundle time",
			recipe: splitTestRecipe,
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"kube-prometheus-stack": {config.ComponentEnabledKey: "false"},
				"kai-scheduler":         {config.ComponentEnabledKey: "false"},
			})},
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  "empty class",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			dest := filepath.Join(parent, "set")
			_, err := newSplitBundler(t, tt.opts...).Make(t.Context(), tt.recipe(), dest)
			if tt.wantMsg == "" {
				if err != nil {
					t.Fatalf("Make() error = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Make() = nil error, want rejection")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMsg)
			}
			if _, statErr := os.Stat(dest); statErr == nil {
				t.Errorf("destination %s exists after a rejected split", dest)
			}
			assertNoStageLeft(t, parent)
		})
	}
}

func TestMake_SplitPublicationIsCreateOnlyAndAtomic(t *testing.T) {
	t.Parallel()

	t.Run("existing destination is rejected untouched", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dest := filepath.Join(parent, "set")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dest, "keep.txt")
		if err := os.WriteFile(marker, []byte("published"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := newSplitBundler(t).Make(t.Context(), splitTestRecipe(), dest)
		if err == nil {
			t.Fatal("Make() = nil error, want the existing destination rejected")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
		if data, _ := os.ReadFile(marker); string(data) != "published" {
			t.Errorf("existing destination was altered")
		}
		assertNoStageLeft(t, parent)
	})

	t.Run("implicit output path is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := newSplitBundler(t).Make(t.Context(), splitTestRecipe(), "")
		if err == nil {
			t.Fatal("Make() = nil error, want an explicit output path required")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("missing parent is rejected", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "missing", "set")
		_, err := newSplitBundler(t).Make(t.Context(), splitTestRecipe(), dest)
		if err == nil {
			t.Fatal("Make() = nil error, want the missing parent rejected")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("failure after rendering leaves no destination", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dest := filepath.Join(parent, "set")
		// Attestation runs after every class is rendered and the root
		// checksums are written, so a failing attester exercises the
		// pre-commit cleanup path with a fully populated stage.
		b, err := New(
			WithConfig(config.NewConfig(config.WithSplit(true), config.WithAttest(true))),
			WithAttester(failingAttester{}),
			WithVerifiedBinaryAttestation([]byte("stub")),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		_, err = b.Make(t.Context(), splitTestRecipe(), dest)
		if err == nil {
			t.Fatal("Make() = nil error, want the attester failure")
		}
		if _, statErr := os.Stat(dest); statErr == nil {
			t.Errorf("destination %s exists after a pre-commit failure", dest)
		}
		assertNoStageLeft(t, parent)
	})
}

// failingAttester fails every Attest call.
type failingAttester struct{}

func (failingAttester) Attest(context.Context, attestation.AttestSubject) ([]byte, error) {
	return nil, errors.New(errors.ErrCodeInternal, "attester failed on purpose")
}
func (failingAttester) Identity() string    { return "test" }
func (failingAttester) HasRekorEntry() bool { return false }

func TestMake_SplitRejectsUnsupportedConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		opts    []config.Option
		wantMsg string
	}{
		{"non-helm deployer", []config.Option{config.WithDeployer(config.DeployerArgoCD)}, "only the helm deployer"},
		{"checksums disabled", []config.Option{config.WithIncludeChecksums(false)}, "requires checksums"},
		{"component filter", []config.Option{config.WithBundlers([]string{"cert-manager"})}, "component filter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(WithConfig(config.NewConfig(append([]config.Option{config.WithSplit(true)}, tt.opts...)...)))
			if err == nil {
				t.Fatal("New() = nil error, want the split support matrix rejection")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestMake_NonSplitUnchangedBySplitPlumbing(t *testing.T) {
	t.Parallel()
	// The same recipe through the ordinary path still produces one bundle
	// at the destination, with deploy.sh at the root and no split.yaml.
	dest := filepath.Join(t.TempDir(), "bundle")
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := b.Make(t.Context(), splitTestRecipe(), dest); err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "deploy.sh")); statErr != nil {
		t.Errorf("ordinary bundle missing deploy.sh: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dest, SplitFileName)); statErr == nil {
		t.Errorf("ordinary bundle has split.yaml")
	}
	deploy, _ := os.ReadFile(filepath.Join(dest, "deploy.sh"))
	if !strings.Contains(string(deploy), `ORPHANED_CRD_GROUPS="${ORPHANED_CRD_GROUPS} monitoring.coreos.com"`) {
		t.Errorf("ordinary deploy.sh lost the monitoring.coreos.com scan keyed by kube-prometheus-stack")
	}
}

// TestValidateSplitHelmIdentities pins the whole-set identity check: the
// injected -post release of one component colliding with another declared
// component, and the reserved -readiness suffix, are rejected regardless of
// which class each would land in.
func TestValidateSplitHelmIdentities(t *testing.T) {
	t.Parallel()
	b, err := New(WithConfig(config.NewConfig(config.WithSplit(true))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	gpuOperator := recipe.ComponentRef{
		Name: "gpu-operator", Namespace: "gpu-operator", Version: "v26.3.3", Type: "helm",
		Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator",
		ManifestFiles: []string{"components/gpu-operator/manifests/kernel-module-params.yaml"},
	}

	t.Run("clean set passes", func(t *testing.T) {
		t.Parallel()
		rr := splitTestRecipe()
		if err := b.validateSplitHelmIdentities(t.Context(), rr); err != nil {
			t.Fatalf("validateSplitHelmIdentities() error = %v", err)
		}
	})

	t.Run("declared component collides with an injected -post release", func(t *testing.T) {
		t.Parallel()
		// gpu-operator carries a post manifest, so it emits a
		// gpu-operator-post release in its namespace; a declared component of
		// that name in the same namespace is the cross-class collision the
		// per-class writer cannot see.
		rr := splitTestRecipe()
		rr.ComponentRefs = append(rr.ComponentRefs, gpuOperator, recipe.ComponentRef{
			Name: "gpu-operator-post", Namespace: "gpu-operator", Version: "1.0.0", Type: "helm",
			Source: "https://charts.example.com", Chart: "shim",
		})
		err := b.validateSplitHelmIdentities(t.Context(), rr)
		if err == nil {
			t.Fatal("validateSplitHelmIdentities() = nil, want the release collision")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), `release "gpu-operator-post" in namespace "gpu-operator"`) {
			t.Errorf("error = %v, want the colliding release named", err)
		}
	})

	t.Run("reserved -readiness suffix is rejected", func(t *testing.T) {
		t.Parallel()
		rr := splitTestRecipe()
		rr.ComponentRefs = append(rr.ComponentRefs, recipe.ComponentRef{
			Name: "probe-readiness", Namespace: "probe", Version: "1.0.0", Type: "helm",
			Source: "https://charts.example.com", Chart: "probe",
		})
		err := b.validateSplitHelmIdentities(t.Context(), rr)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("validateSplitHelmIdentities() error = %v, want the reserved suffix rejected", err)
		}
	})
}

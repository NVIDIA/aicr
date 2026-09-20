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

package aicr_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// Component names here are synthetic and match no registry entry, so no record
// applies to any of them. That is deliberate: ADR-021's testing strategy
// forbids asserting a verdict for a real component, because pinning one makes
// every registry pin bump churn the suite.

// syntheticRecipe writes a hydrated RecipeResult carrying the given
// component-to-version table, and no install namespace on either side.
func syntheticRecipe(t *testing.T, path string, components map[string]string) string {
	t.Helper()
	doc := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\nmetadata:\n  version: test\ncomponentRefs:\n"
	for _, name := range sortedKeys(components) {
		doc += fmt.Sprintf("  - name: %s\n    type: Helm\n    source: https://charts.invalid/synthetic\n    version: %s\n",
			name, components[name])
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write %s: %v", path, err)
	}
	return path
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func upgradeCheckClient(t *testing.T) *aicr.Client {
	t.Helper()
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestUpgradeCheckArtifactForms(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	from := syntheticRecipe(t, filepath.Join(dir, "from.yaml"), map[string]string{
		"synthetic-alpha": "1.2.0",
		"synthetic-beta":  "0.18.0",
		"synthetic-gone":  "2.0.0",
	})
	to := syntheticRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{
		"synthetic-alpha": "1.2.0", // unchanged, so it produces no row
		"synthetic-beta":  "0.19.0",
		"synthetic-new":   "0.1.0",
	})

	// A bundle is a directory holding the recipe.yaml the bundler wrote, and
	// reaches exactly the same code path as the recipe file above.
	bundleDir := filepath.Join(dir, "bundle")
	if err := os.Mkdir(bundleDir, 0o750); err != nil {
		t.Fatalf("setup: mkdir bundle: %v", err)
	}
	syntheticRecipe(t, filepath.Join(bundleDir, "recipe.yaml"), map[string]string{
		"synthetic-alpha": "1.2.0",
		"synthetic-beta":  "0.18.0",
		"synthetic-gone":  "2.0.0",
	})

	tests := []struct {
		name string
		from string
	}{
		{"recipe file", from},
		{"bundle directory", bundleDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := upgradeCheckClient(t)
			report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: tt.from, To: to})
			if err != nil {
				t.Fatalf("UpgradeCheck: %v", err)
			}

			got := map[string]upgrade.ChangeKind{}
			for _, c := range report.Components {
				got[c.Component] = c.Change
			}
			want := map[string]upgrade.ChangeKind{
				"synthetic-beta": upgrade.ChangeVersion,
				"synthetic-gone": upgrade.ChangeRemoved,
				"synthetic-new":  upgrade.ChangeAdded,
			}
			if len(got) != len(want) {
				t.Fatalf("rows = %v, want %v", got, want)
			}
			for name, change := range want {
				if got[name] != change {
					t.Errorf("%s change = %q, want %q", name, got[name], change)
				}
			}
			if report.Summary.Components != 3 {
				t.Errorf("Summary.Components = %d, want 3", report.Summary.Components)
			}
			// 0.18.0 -> 0.19.0 is a breaking boundary below 1.0 with no
			// record, which is the one row that stops a strict run.
			if report.Summary.Failing != 1 {
				t.Errorf("Summary.Failing = %d, want 1", report.Summary.Failing)
			}
			if !report.FailsRun() {
				t.Error("FailsRun() = false, want true")
			}
		})
	}
}

// TestUpgradeCheckNoDeployerNeeded proves the deployer is demanded only when
// steps would render, so the common all-safe run needs no flag.
func TestUpgradeCheckNoDeployerNeeded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	from := syntheticRecipe(t, filepath.Join(dir, "from.yaml"), map[string]string{"synthetic-alpha": "1.2.0"})
	to := syntheticRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-alpha": "1.2.3"})

	client := upgradeCheckClient(t)
	report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: from, To: to})
	if err != nil {
		t.Fatalf("UpgradeCheck without --deployer: %v", err)
	}
	if report.Deployer != "" {
		t.Errorf("Deployer = %q, want empty", report.Deployer)
	}
	// Needing no deployer is independent of the exit code: an unrecorded bump
	// is unknown, and unknown fails however small the move, but nothing about
	// it carries steps for a deployer to scope.
	if !report.FailsRun() {
		t.Error("an unrecorded patch bump passed the run, want unknown to fail")
	}
	if got := report.Components[0].Reason; got != upgrade.ReasonNoRecord {
		t.Errorf("reason = %q, want %q", got, upgrade.ReasonNoRecord)
	}
}

func TestUpgradeCheckRejects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recipePath := syntheticRecipe(t, filepath.Join(dir, "recipe.yaml"), map[string]string{"synthetic-alpha": "1.0.0"})
	notABundle := filepath.Join(dir, "not-a-bundle")
	if err := os.Mkdir(notABundle, 0o750); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}

	tests := []struct {
		name string
		req  aicr.UpgradeCheckRequest
	}{
		{"no from", aicr.UpgradeCheckRequest{To: recipePath}},
		{"from directory holds no recipe", aicr.UpgradeCheckRequest{From: notABundle, To: recipePath}},
		{"to directory holds no recipe", aicr.UpgradeCheckRequest{From: recipePath, To: notABundle}},
		// A hydrated result carrying no criteria cannot be re-resolved
		// against this binary's registry, so the target has to be named.
		{"to omitted for a criteria-less artifact", aicr.UpgradeCheckRequest{From: recipePath}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := upgradeCheckClient(t)
			_, err := client.UpgradeCheck(t.Context(), tt.req)
			if err == nil {
				t.Fatal("UpgradeCheck accepted the request, want ErrCodeInvalidRequest")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestUpgradeCheckGuards(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recipePath := syntheticRecipe(t, filepath.Join(dir, "recipe.yaml"), map[string]string{"synthetic-alpha": "1.0.0"})
	req := aicr.UpgradeCheckRequest{From: recipePath, To: recipePath}

	t.Run("nil client", func(t *testing.T) {
		t.Parallel()
		var client *aicr.Client
		if _, err := client.UpgradeCheck(t.Context(), req); err == nil {
			t.Error("nil Client returned no error")
		}
	})

	t.Run("nil context", func(t *testing.T) {
		t.Parallel()
		client := upgradeCheckClient(t)
		//nolint:staticcheck // passing a nil context is the case under test
		if _, err := client.UpgradeCheck(context.Context(nil), req); err == nil {
			t.Error("nil context returned no error")
		}
	})

	t.Run("closed client", func(t *testing.T) {
		t.Parallel()
		client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if err := client.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := client.UpgradeCheck(t.Context(), req); err == nil {
			t.Error("closed Client returned no error")
		}
	})
}

// TestUpgradeCheckSynthesizesTargetFromCriteria covers the one-argument form:
// with no --to, the target is this binary's own resolution of the source's
// criteria. Re-resolving an artifact that already came from these pins must
// therefore report nothing changed.
//
// It asserts no verdict, and cannot: a report with no rows has none. What it
// pins is that the criteria round-trip through the artifact and back.
func TestUpgradeCheckSynthesizesTargetFromCriteria(t *testing.T) {
	t.Parallel()

	client := upgradeCheckClient(t)
	resolved, err := client.ResolveRecipe(t.Context(), aicr.RecipeRequest{
		Service:     "eks",
		Accelerator: "h100",
		Intent:      "training",
		OS:          "ubuntu",
	})
	if err != nil {
		t.Fatalf("ResolveRecipe: %v", err)
	}

	doc, err := serializer.MarshalYAMLDeterministic(resolved.Resolved())
	if err != nil {
		t.Fatalf("marshal resolved recipe: %v", err)
	}
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	if writeErr := os.WriteFile(path, doc, 0o600); writeErr != nil {
		t.Fatalf("setup: write recipe: %v", writeErr)
	}

	report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: path})
	if err != nil {
		t.Fatalf("UpgradeCheck with no target: %v", err)
	}
	if len(report.Components) != 0 {
		t.Errorf("re-resolving an artifact against the pins it came from reported %d change(s): %+v",
			len(report.Components), report.Components)
	}
	if report.FailsRun() {
		t.Error("FailsRun() = true for a report with no changes")
	}
}

// TestUpgradeCheckRejectsUnrecognizedDeployer pins the SDK-side gate. pkg/cli
// validates the flag too, but the facade is the shared surface: an unrecognized
// name matches no explicit step group and would silently collect the remainder
// group, which was authored for the deployers nobody named.
func TestUpgradeCheckRejectsUnrecognizedDeployer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	from := syntheticRecipe(t, filepath.Join(dir, "from.yaml"), map[string]string{"synthetic-alpha": "1.2.0"})
	to := syntheticRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-alpha": "1.3.0"})

	for _, name := range []string{"helm3", "argo", "kustomize", "localformat"} {
		t.Run("rejected/"+name, func(t *testing.T) {
			t.Parallel()
			_, err := upgradeCheckClient(t).UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{
				From: from, To: to, Deployer: name,
			})
			if err == nil {
				t.Fatalf("UpgradeCheck(Deployer=%q) error = nil, want rejection", name)
			}
		})
	}

	// ParseDeployerType folds case and trims, so these are the same deployer
	// spelled differently rather than unknown values. Accepting them and
	// canonicalizing is the contract; the report must not echo the raw input,
	// since stepsFor matches canonical names only.
	for _, name := range []string{"Helm", " helm", "HELM"} {
		t.Run("canonicalized/"+name, func(t *testing.T) {
			t.Parallel()
			report, err := upgradeCheckClient(t).UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{
				From: from, To: to, Deployer: name,
			})
			if err != nil {
				t.Fatalf("UpgradeCheck(Deployer=%q) error = %v, want acceptance", name, err)
			}
			if report.Deployer != "helm" {
				t.Errorf("report.Deployer = %q, want %q", report.Deployer, "helm")
			}
		})
	}
}

// syntheticKustomizeRecipe writes a hydrated RecipeResult whose components pin
// a Kustomize tag rather than a Helm chart version.
func syntheticKustomizeRecipe(t *testing.T, path string, components map[string]string) string {
	t.Helper()
	doc := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\nmetadata:\n  version: test\ncomponentRefs:\n"
	for _, name := range sortedKeys(components) {
		doc += fmt.Sprintf(
			"  - name: %s\n    type: Kustomize\n    source: https://github.invalid/synthetic\n"+
				"    path: deploy\n    tag: %s\n",
			name, components[name])
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write %s: %v", path, err)
	}
	return path
}

// A Kustomize component pins its version in `tag`. Reading `version` alone
// compares "" to "" on both sides, which the matcher skips as unchanged, so the
// row disappears instead of being reported.
func TestUpgradeCheckReportsKustomizeTagChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := upgradeCheckClient(t)

	tests := []struct {
		name     string
		from, to map[string]string
		wantRows int
		wantFrom string
		wantTo   string
	}{
		{
			name:     "a moved tag is one row",
			from:     map[string]string{"synthetic-kustomize": "v1.0.0"},
			to:       map[string]string{"synthetic-kustomize": "v2.0.0"},
			wantRows: 1,
			wantFrom: "v1.0.0",
			wantTo:   "v2.0.0",
		},
		{
			name:     "an unchanged tag is still no row",
			from:     map[string]string{"synthetic-kustomize": "v1.0.0"},
			to:       map[string]string{"synthetic-kustomize": "v1.0.0"},
			wantRows: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fromPath := syntheticKustomizeRecipe(t, filepath.Join(dir, tt.name+"-from.yaml"), tt.from)
			toPath := syntheticKustomizeRecipe(t, filepath.Join(dir, tt.name+"-to.yaml"), tt.to)

			report, err := client.UpgradeCheck(context.Background(), aicr.UpgradeCheckRequest{
				From: fromPath, To: toPath,
			})
			if err != nil {
				t.Fatalf("UpgradeCheck: %v", err)
			}
			if len(report.Components) != tt.wantRows {
				t.Fatalf("report has %d rows, want %d: %+v", len(report.Components), tt.wantRows, report.Components)
			}
			if tt.wantRows == 0 {
				return
			}
			row := report.Components[0]
			if row.From != tt.wantFrom || row.To != tt.wantTo {
				t.Errorf("row = %s -> %s, want %s -> %s", row.From, row.To, tt.wantFrom, tt.wantTo)
			}
			if row.Change != upgrade.ChangeVersion {
				t.Errorf("change = %q, want %q", row.Change, upgrade.ChangeVersion)
			}
		})
	}
}

// syntheticComponent is one component of a namespace-bearing recipe fixture.
type syntheticComponent struct {
	version   string
	namespace string
}

// syntheticNamespacedRecipe writes a hydrated RecipeResult whose components pin
// an install namespace alongside their version.
func syntheticNamespacedRecipe(t *testing.T, path string, components map[string]syntheticComponent) string {
	t.Helper()
	doc := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\nmetadata:\n  version: test\ncomponentRefs:\n"
	for _, name := range sortedKeys(components) {
		c := components[name]
		doc += fmt.Sprintf(
			"  - name: %s\n    type: Helm\n    source: https://charts.invalid/synthetic\n"+
				"    version: %s\n    namespace: %s\n",
			name, c.version, c.namespace)
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write %s: %v", path, err)
	}
	return path
}

// A component's install namespace comes from the registry, and AICR requires the
// recipe to be regenerated from scratch on every upgrade, so a moved default
// relocates the install. Helm cannot move a release between namespaces, so
// applying the new recipe installs a second copy beside the running one. A
// version-only projection reports that as no change whatsoever.
func TestUpgradeCheckReportsNamespaceChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := upgradeCheckClient(t)

	tests := []struct {
		name     string
		from, to map[string]syntheticComponent
		wantRows int
	}{
		{
			name:     "a moved namespace at an unchanged version is one row",
			from:     map[string]syntheticComponent{"synthetic-alpha": {"1.2.0", "synthetic-old"}},
			to:       map[string]syntheticComponent{"synthetic-alpha": {"1.2.0", "synthetic-new"}},
			wantRows: 1,
		},
		{
			name:     "an unchanged namespace is still no row",
			from:     map[string]syntheticComponent{"synthetic-alpha": {"1.2.0", "synthetic-old"}},
			to:       map[string]syntheticComponent{"synthetic-alpha": {"1.2.0", "synthetic-old"}},
			wantRows: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fromPath := syntheticNamespacedRecipe(t, filepath.Join(dir, tt.name+"-from.yaml"), tt.from)
			toPath := syntheticNamespacedRecipe(t, filepath.Join(dir, tt.name+"-to.yaml"), tt.to)

			report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: fromPath, To: toPath})
			if err != nil {
				t.Fatalf("UpgradeCheck: %v", err)
			}
			if len(report.Components) != tt.wantRows {
				t.Fatalf("report has %d rows, want %d: %+v", len(report.Components), tt.wantRows, report.Components)
			}
			if tt.wantRows == 0 {
				return
			}
			row := report.Components[0]
			if row.Change != upgrade.ChangeIdentity {
				t.Errorf("change = %q, want %q", row.Change, upgrade.ChangeIdentity)
			}
			if row.Reason != upgrade.ReasonIdentityChanged {
				t.Errorf("reason = %q, want %q", row.Reason, upgrade.ReasonIdentityChanged)
			}
			// A relocation is unassessable, so it must stop a strict run.
			if !report.FailsRun() {
				t.Error("FailsRun() = false for a report carrying a relocation")
			}
		})
	}
}

// syntheticBundle writes the minimum a bundle needs for the object-name axis
// to be readable: the recipe.yaml that makes the directory a bundle, the
// bundle-info.yaml that locates each release, and the rendered values.yaml the
// deployer wrote. Values are supplied as raw YAML so a test can express an
// absent key, which is the state that matters here.
func syntheticBundle(t *testing.T, dir string, components map[string]string, values map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("setup: mkdir %s: %v", dir, err)
	}
	syntheticRecipe(t, filepath.Join(dir, "recipe.yaml"), components)

	releases := make([]bundleinfo.Release, 0, len(components))
	for i, name := range sortedKeys(components) {
		path := fmt.Sprintf("%03d-%s", i+1, name)
		releases = append(releases, bundleinfo.Release{Name: name, Component: name, Path: path})
		if doc, ok := values[name]; ok {
			if err := os.MkdirAll(filepath.Join(dir, path), 0o750); err != nil {
				t.Fatalf("setup: mkdir %s: %v", path, err)
			}
			if err := os.WriteFile(filepath.Join(dir, path, "values.yaml"), []byte(doc), 0o600); err != nil {
				t.Fatalf("setup: write values for %s: %v", name, err)
			}
		}
	}

	info := &bundleinfo.BundleInfo{
		APIVersion: header.StableGroupVersion,
		Kind:       string(header.KindBundleInfo),
		Build: bundleinfo.Build{
			Deployer: "helm",
			Recipe: bundleinfo.Recipe{
				Path:   "recipe.yaml",
				Digest: "sha256:3b1f8c2ad9e7546102bb8f4c7d0e9a1358cc4f6b2e8d70a94f1c5b3e6d820947",
			},
		},
		Layout: bundleinfo.Layout{Entrypoint: "deploy.sh", Releases: releases},
	}
	if _, err := bundleinfo.Write(t.Context(), dir, info); err != nil {
		t.Fatalf("setup: write bundle info: %v", err)
	}
	return dir
}

// A component's object names come from its merged Helm values, which a
// resolved recipe records by REFERENCE: `valuesFile` is a path resolved
// against whichever binary reads it. Only a bundle wrote the merged result
// down, so only a bundle can say what a running install was named. Dropping a
// fullnameOverride renames every object the chart owns, which Helm applies as
// delete-and-recreate.
func TestUpgradeCheckReportsObjectNameChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := upgradeCheckClient(t)

	components := map[string]string{"synthetic-alpha": "1.2.0"}

	t.Run("a dropped fullnameOverride at an unchanged version is one row", func(t *testing.T) {
		t.Parallel()
		from := syntheticBundle(t, filepath.Join(dir, "dropped"), components,
			map[string]string{"synthetic-alpha": "fullnameOverride: legacy-alpha\n"})
		// The target is a recipe file, which is faithful on this side: it asks
		// what deploys now, and the synthetic ref pins no values at all.
		to := syntheticRecipe(t, filepath.Join(dir, "dropped-to.yaml"), components)

		report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: from, To: to})
		if err != nil {
			t.Fatalf("UpgradeCheck: %v", err)
		}
		if !report.ObjectNamesCompared {
			t.Fatalf("ObjectNamesCompared = false, want true: %s", report.ObjectNamesSkipped)
		}
		if len(report.Components) != 1 {
			t.Fatalf("report has %d rows, want 1: %+v", len(report.Components), report.Components)
		}
		row := report.Components[0]
		if row.Change != upgrade.ChangeIdentity {
			t.Errorf("change = %q, want %q", row.Change, upgrade.ChangeIdentity)
		}
		want := []upgrade.ReportIdentityChange{
			{Field: "fullnameOverride", From: "legacy-alpha", To: ""},
		}
		if !reflect.DeepEqual(row.IdentityChanges, want) {
			t.Errorf("identityChanges = %+v, want %+v", row.IdentityChanges, want)
		}
		// Deliberately not asserting a Verdict for this row beyond what
		// FailsRun implies: no synthetic record applies, and ADR-021's testing
		// strategy keeps verdicts out of the unit suite.
		if !report.FailsRun() {
			t.Error("FailsRun() = false for a report carrying a rename")
		}
	})

	t.Run("an unchanged fullnameOverride is still no row", func(t *testing.T) {
		t.Parallel()
		values := map[string]string{"synthetic-alpha": "fullnameOverride: stable-alpha\n"}
		from := syntheticBundle(t, filepath.Join(dir, "held-from"), components, values)
		to := syntheticBundle(t, filepath.Join(dir, "held-to"), components, values)

		report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: from, To: to})
		if err != nil {
			t.Fatalf("UpgradeCheck: %v", err)
		}
		if !report.ObjectNamesCompared {
			t.Fatalf("ObjectNamesCompared = false, want true: %s", report.ObjectNamesSkipped)
		}
		if len(report.Components) != 0 {
			t.Fatalf("report has %d rows, want 0: %+v", len(report.Components), report.Components)
		}
	})

	t.Run("a nested subchart name is compared too", func(t *testing.T) {
		t.Parallel()
		from := syntheticBundle(t, filepath.Join(dir, "nested-from"), components,
			map[string]string{"synthetic-alpha": "grafana:\n  fullnameOverride: old-grafana\n"})
		to := syntheticBundle(t, filepath.Join(dir, "nested-to"), components,
			map[string]string{"synthetic-alpha": "grafana:\n  fullnameOverride: new-grafana\n"})

		report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: from, To: to})
		if err != nil {
			t.Fatalf("UpgradeCheck: %v", err)
		}
		if len(report.Components) != 1 {
			t.Fatalf("report has %d rows, want 1: %+v", len(report.Components), report.Components)
		}
		want := []upgrade.ReportIdentityChange{
			{Field: "grafana.fullnameOverride", From: "old-grafana", To: "new-grafana"},
		}
		if !reflect.DeepEqual(report.Components[0].IdentityChanges, want) {
			t.Errorf("identityChanges = %+v, want %+v", report.Components[0].IdentityChanges, want)
		}
	})
}

// A recipe file on the SOURCE side cannot state the object names it deployed
// with, and reporting "nothing moved" there would be the same false assurance
// this axis exists to remove. It has to say it did not look.
func TestUpgradeCheckSkipsObjectNamesWithoutABundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := upgradeCheckClient(t)

	components := map[string]string{"synthetic-alpha": "1.2.0"}

	tests := []struct {
		name       string
		from       func(t *testing.T) string
		wantReason string
	}{
		{
			name: "a recipe file records values by reference",
			from: func(t *testing.T) string {
				return syntheticRecipe(t, filepath.Join(dir, "plain.yaml"), components)
			},
			wantReason: "by reference",
		},
		{
			name: "a bundle built before build-record stamping has nothing to locate them through",
			from: func(t *testing.T) string {
				bundle := filepath.Join(dir, "unstamped")
				if err := os.MkdirAll(bundle, 0o750); err != nil {
					t.Fatalf("setup: mkdir: %v", err)
				}
				syntheticRecipe(t, filepath.Join(bundle, "recipe.yaml"), components)
				return bundle
			},
			wantReason: bundleinfo.FileName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			to := syntheticRecipe(t, filepath.Join(dir, tt.name+"-to.yaml"),
				map[string]string{"synthetic-alpha": "1.3.0"})

			report, err := client.UpgradeCheck(t.Context(),
				aicr.UpgradeCheckRequest{From: tt.from(t), To: to})
			assertObjectNamesSkipped(t, report, err, tt.wantReason)
		})
	}
}

// An UNREADABLE target bundle must skip the axis rather than fall back to
// resolving that artifact's values against this binary. The fallback is
// correct for a recipe target, which asks what deploys now; for a bundle it
// would answer a question nobody asked, and could invent a rename or hide one.
func TestUpgradeCheckSkipsObjectNamesForAnUnreadableTargetBundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := upgradeCheckClient(t)
	components := map[string]string{"synthetic-alpha": "1.2.0"}

	from := syntheticBundle(t, filepath.Join(dir, "from"), components,
		map[string]string{"synthetic-alpha": "fullnameOverride: legacy-alpha\n"})

	// A bundle in every respect except the build record that locates its
	// values, which is what an AICR predating stamping produced.
	to := filepath.Join(dir, "unstamped-to")
	if err := os.MkdirAll(to, 0o750); err != nil {
		t.Fatalf("setup: mkdir: %v", err)
	}
	syntheticRecipe(t, filepath.Join(to, "recipe.yaml"), map[string]string{"synthetic-alpha": "1.3.0"})

	report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: from, To: to})
	assertObjectNamesSkipped(t, report, err, bundleinfo.FileName)
}

// A target recipe can name a values file this binary does not ship — a
// component's values file renamed between releases is enough. That must
// withdraw the object-name axis, not the whole command: the version
// comparison is what the check was for before this axis existed, and it is
// still answerable.
func TestUpgradeCheckSkipsObjectNamesWhenTargetValuesAreUnreadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	client := upgradeCheckClient(t)
	components := map[string]string{"synthetic-alpha": "1.2.0"}

	from := syntheticBundle(t, filepath.Join(dir, "from"), components,
		map[string]string{"synthetic-alpha": "fullnameOverride: legacy-alpha\n"})

	// A hydrated recipe naming a values file that is not in the embedded data.
	to := filepath.Join(dir, "missing-values.yaml")
	doc := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\nmetadata:\n  version: test\n" +
		"componentRefs:\n  - name: synthetic-alpha\n    type: Helm\n" +
		"    source: https://charts.invalid/synthetic\n    version: 1.3.0\n" +
		"    valuesFile: components/synthetic-alpha/nonexistent-values.yaml\n"
	if err := os.WriteFile(to, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write %s: %v", to, err)
	}

	report, err := client.UpgradeCheck(t.Context(), aicr.UpgradeCheckRequest{From: from, To: to})
	assertObjectNamesSkipped(t, report, err, "could not be resolved")
}

// assertObjectNamesSkipped checks the report withheld the object-name axis for
// the stated reason, still reported the version axis, and attributed no
// identity change to an axis nothing looked at.
func assertObjectNamesSkipped(t *testing.T, report *aicr.UpgradeReport, err error, wantReason string) {
	t.Helper()
	if err != nil {
		t.Fatalf("UpgradeCheck: %v", err)
	}
	if report.ObjectNamesCompared {
		t.Error("ObjectNamesCompared = true, want false")
	}
	if !strings.Contains(report.ObjectNamesSkipped, wantReason) {
		t.Errorf("ObjectNamesSkipped = %q, want it to mention %q", report.ObjectNamesSkipped, wantReason)
	}
	// The version axis still reports: an unread axis withdraws its own claim,
	// not the whole run.
	if len(report.Components) != 1 {
		t.Fatalf("report has %d rows, want the version row: %+v", len(report.Components), report.Components)
	}
	for _, c := range report.Components {
		if len(c.IdentityChanges) != 0 {
			t.Errorf("%s carries identity changes from an axis that was not compared: %+v",
				c.Component, c.IdentityChanges)
		}
	}
}

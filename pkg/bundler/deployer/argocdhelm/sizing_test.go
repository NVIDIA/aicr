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

package argocdhelm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Fixture rows only; real tiers are validated empirically, not in unit tests.
func sizingFixtureGenerator(nodeCount int) *Generator {
	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.20.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
	})
	rr.DeploymentOrder = []string{"cert-manager", "gpu-operator"}
	return &Generator{
		RecipeResult: rr,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"operator": map[string]any{"resources": map[string]any{
				"requests": map[string]any{"memory": "300Mi"},
				"limits":   map[string]any{"memory": "8Gi"},
			}}},
			"cert-manager": {"replicaCount": 1},
		},
		Version: "v0.0.0-golden",
		SizingRows: map[string][]SizingRow{
			"gpu-operator": {
				{MaxNodes: 250, Values: map[string]any{"operator": map[string]any{"resources": map[string]any{
					"requests": map[string]any{"memory": "256Mi"},
					"limits":   map[string]any{"memory": "768Mi"},
				}}}},
				{MaxNodes: 450, Values: map[string]any{}},
				{MaxNodes: 700, Values: map[string]any{"operator": map[string]any{"resources": map[string]any{
					"limits": map[string]any{"memory": "1Gi"},
				}}}},
			},
		},
		SizingNodeCount: nodeCount,
	}
}

// TestBundleGolden_Sizing freezes a sized child Application: the row
// selector, the merge order and both annotations. cert-manager has no rows
// and must render exactly as an unsized child.
//
//	go test ./pkg/bundler/deployer/argocdhelm/... -run TestBundleGolden -args -update
func TestBundleGolden_Sizing(t *testing.T) {
	outputDir := t.TempDir()
	if _, err := sizingFixtureGenerator(0).Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	for _, rel := range []string{
		"values.yaml",
		"values.schema.json",
		"templates/gpu-operator.yaml",
		"templates/cert-manager.yaml",
		"static/gpu-operator.yaml",
		"static/gpu-operator-sizing.yaml",
	} {
		assertGolden(t, outputDir, "testdata/sizing", rel)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "static", "cert-manager-sizing.yaml")); !os.IsNotExist(err) {
		t.Errorf("cert-manager has no rows but has a sizing file (stat err %v)", err)
	}
}

func TestHelmTemplate_Sizing(t *testing.T) {
	helmtest.RequireBinary(t)

	tests := []struct {
		name         string
		bundleNodes  int
		args         []string
		valuesFile   string
		wantReq      string
		wantLim      string
		wantCount    string
		wantTier     string
		wantErrMatch string
	}{
		{name: "no count keeps static values", wantReq: "300Mi", wantLim: "8Gi", wantCount: "0"},
		{name: "count in the lowest row", args: []string{"--set", "sizing.nodeCount=200"},
			wantReq: "256Mi", wantLim: "768Mi", wantCount: "200", wantTier: "250"},
		{name: "count on a bound", args: []string{"--set", "sizing.nodeCount=250"},
			wantReq: "256Mi", wantLim: "768Mi", wantCount: "250", wantTier: "250"},
		{name: "row that changes nothing", args: []string{"--set", "sizing.nodeCount=300"},
			wantReq: "300Mi", wantLim: "8Gi", wantCount: "300"},
		{name: "count in a higher row", args: []string{"--set", "sizing.nodeCount=500"},
			wantReq: "300Mi", wantLim: "1Gi", wantCount: "500", wantTier: "700"},
		{name: "count above every row", args: []string{"--set", "sizing.nodeCount=2500"},
			wantReq: "300Mi", wantLim: "8Gi", wantCount: "2500"},
		{name: "count from a values file", valuesFile: "sizing:\n  nodeCount: 200\n",
			wantReq: "256Mi", wantLim: "768Mi", wantCount: "200", wantTier: "250"},
		{name: "bundle count is the default", bundleNodes: 500,
			wantReq: "300Mi", wantLim: "1Gi", wantCount: "500", wantTier: "700"},
		{name: "install count replaces the bundle count", bundleNodes: 500, args: []string{"--set", "sizing.nodeCount=0"},
			wantReq: "300Mi", wantLim: "8Gi", wantCount: "0"},
		{name: "root-chart value beats the row",
			args:    []string{"--set", "sizing.nodeCount=200", "--set", "gpuoperator.operator.resources.limits.memory=4Gi"},
			wantReq: "256Mi", wantLim: "4Gi", wantCount: "200", wantTier: "250"},
		{name: "negative count fails", args: []string{"--set", "sizing.nodeCount=-1"}, wantErrMatch: "nodeCount"},
		{name: "fractional count fails", valuesFile: "sizing:\n  nodeCount: 250.5\n", wantErrMatch: "nodeCount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputDir := t.TempDir()
			if _, err := sizingFixtureGenerator(tt.bundleNodes).Generate(context.Background(), outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			args := append([]string{"template", "test-release", outputDir, "--set", "repoURL=oci://example.test/org"}, tt.args...)
			if tt.valuesFile != "" {
				path := filepath.Join(t.TempDir(), "values.yaml")
				if err := os.WriteFile(path, []byte(tt.valuesFile), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-f", path)
			}
			cmdCtx, cancel := context.WithTimeout(context.Background(), helmTemplateTimeout)
			defer cancel()
			out, err := exec.CommandContext(cmdCtx, "helm", args...).CombinedOutput() //nolint:gosec // controlled args
			if tt.wantErrMatch != "" {
				if err == nil || !strings.Contains(string(out), tt.wantErrMatch) {
					t.Fatalf("helm template err = %v, want a failure naming %q:\n%s", err, tt.wantErrMatch, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("helm template failed: %v\n%s", err, out)
			}

			app := renderedApplication(t, out, "gpu-operator")
			annotations := mapAt(t, app, "metadata", "annotations")
			if got := annotations[annotationSizingNodeCount]; got != tt.wantCount {
				t.Errorf("%s = %v, want %q", annotationSizingNodeCount, got, tt.wantCount)
			}
			gotTier, hasTier := annotations[annotationSizingTier]
			if (tt.wantTier == "") == hasTier || (hasTier && gotTier != tt.wantTier) {
				t.Errorf("%s = %v (present %v), want %q", annotationSizingTier, gotTier, hasTier, tt.wantTier)
			}
			var values map[string]any
			valuesText, _ := mapAt(t, app, "spec", "source", "helm")["values"].(string)
			if err := yaml.Unmarshal([]byte(valuesText), &values); err != nil {
				t.Fatalf("child values: %v", err)
			}
			gotReq, _ := component.GetValueByPath(values, "operator.resources.requests.memory")
			gotLim, _ := component.GetValueByPath(values, "operator.resources.limits.memory")
			if gotReq != tt.wantReq || gotLim != tt.wantLim {
				t.Errorf("rendered %v/%v, want %s/%s", gotReq, gotLim, tt.wantReq, tt.wantLim)
			}

			unsized := mapAt(t, renderedApplication(t, out, "cert-manager"), "metadata", "annotations")
			if _, ok := unsized[annotationSizingNodeCount]; ok {
				t.Error("unsized cert-manager child carries a sizing annotation")
			}
		})
	}
}

func TestValidateSizing(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Generator)
		wantCode errors.ErrorCode
	}{
		{name: "valid", mutate: func(*Generator) {}},
		{name: "negative default count", mutate: func(g *Generator) { g.SizingNodeCount = -1 },
			wantCode: errors.ErrCodeInvalidRequest},
		{name: "rows for an unknown component", mutate: func(g *Generator) {
			g.SizingRows["missing"] = []SizingRow{{MaxNodes: 1}}
		}, wantCode: errors.ErrCodeInternal},
		{name: "rows for a local chart", mutate: func(g *Generator) {
			g.RecipeResult.ComponentRefs[0].Source = ""
		}, wantCode: errors.ErrCodeInternal},
		{name: "rows with vendored charts", mutate: func(g *Generator) { g.VendorCharts = true },
			wantCode: errors.ErrCodeInternal},
		{name: "rows out of order", mutate: func(g *Generator) {
			rows := g.SizingRows["gpu-operator"]
			rows[1].MaxNodes = 100
		}, wantCode: errors.ErrCodeInternal},
		{name: "sizing file collides with a component", mutate: func(g *Generator) {
			g.RecipeResult.ComponentRefs[1].Name = "gpu-operator-sizing"
		}, wantCode: errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := sizingFixtureGenerator(0)
			tt.mutate(g)
			err := g.validateSizing()
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("validateSizing() error = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

func TestRenderSizingTierAnnotationRequiresOnePlaceholder(t *testing.T) {
	line := annotationSizingTier + ": " + sizingTierAnnotationSentinel + "\n"
	for _, in := range []string{"metadata: {}\n", line + line} {
		if _, err := renderSizingTierAnnotation([]byte(in)); err == nil {
			t.Errorf("renderSizingTierAnnotation(%q) error = nil, want one", in)
		}
	}
}

// renderedApplication returns the rendered child Application for component.
func renderedApplication(t *testing.T, out []byte, componentName string) map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc["kind"] != "Application" {
			continue
		}
		if name, _ := mapAt(t, doc, "metadata")["name"].(string); name == componentName {
			return doc
		}
	}
	t.Fatalf("no rendered Application %q in:\n%s", componentName, out)
	return nil
}

func mapAt(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	for _, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		m = next
	}
	return m
}

func TestReadmeDocumentsSizingOnlyWhenSized(t *testing.T) {
	for _, tt := range []struct {
		name string
		rows bool
	}{{"sized", true}, {"unsized", false}} {
		t.Run(tt.name, func(t *testing.T) {
			g := sizingFixtureGenerator(300)
			if !tt.rows {
				g.SizingRows = nil
			}
			outputDir := t.TempDir()
			if _, err := g.Generate(context.Background(), outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			readme, err := os.ReadFile(filepath.Join(outputDir, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			has := strings.Contains(string(readme), "## Memory Sizing")
			if has != tt.rows {
				t.Errorf("README has a Memory Sizing section = %v, want %v", has, tt.rows)
			}
			if tt.rows && !strings.Contains(string(readme), "here 300") {
				t.Error("README does not name the bundle's default count")
			}
		})
	}
}

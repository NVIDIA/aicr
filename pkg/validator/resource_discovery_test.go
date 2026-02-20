// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package validator

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/recipe"
	"sigs.k8s.io/yaml"
)

// testDataProvider is a minimal DataProvider for testing manifest file loading.
type testDataProvider struct {
	files map[string][]byte
}

func (p *testDataProvider) ReadFile(path string) ([]byte, error) {
	content, ok := p.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return content, nil
}

func (p *testDataProvider) WalkDir(_ string, _ fs.WalkDirFunc) error { return nil }
func (p *testDataProvider) Source(_ string) string                   { return "test" }

func TestExtractWorkloadResources(t *testing.T) {
	tests := []struct {
		name             string
		manifest         string
		defaultNamespace string
		want             []recipe.ExpectedResource
	}{
		{
			name: "single deployment",
			manifest: `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deploy
  namespace: gpu-operator
`,
			defaultNamespace: "default",
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "my-deploy", Namespace: "gpu-operator"},
			},
		},
		{
			name: "multiple workload types",
			manifest: `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
  namespace: ns1
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: agent
  namespace: ns1
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: ns1
`,
			defaultNamespace: "default",
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "controller", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "agent", Namespace: "ns1"},
				{Kind: "StatefulSet", Name: "db", Namespace: "ns1"},
			},
		},
		{
			name: "non-workload resources filtered out",
			manifest: `---
apiVersion: v1
kind: Service
metadata:
  name: my-svc
  namespace: ns1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: ns1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deploy
  namespace: ns1
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: my-role
`,
			defaultNamespace: "default",
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "my-deploy", Namespace: "ns1"},
			},
		},
		{
			name: "namespace fallback to default",
			manifest: `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: no-ns-deploy
`,
			defaultNamespace: "gpu-operator",
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "no-ns-deploy", Namespace: "gpu-operator"},
			},
		},
		{
			name:     "empty manifest",
			manifest: "",
			want:     nil,
		},
		{
			name:     "only separators",
			manifest: "---\n---\n---\n",
			want:     nil,
		},
		{
			name: "unparseable document skipped",
			manifest: `---
this is not: valid: yaml: [
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: good-deploy
  namespace: ns1
`,
			defaultNamespace: "default",
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "good-deploy", Namespace: "ns1"},
			},
		},
		{
			name: "manifest without leading separator",
			manifest: `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: my-ds
  namespace: kube-system
`,
			defaultNamespace: "default",
			want: []recipe.ExpectedResource{
				{Kind: "DaemonSet", Name: "my-ds", Namespace: "kube-system"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWorkloadResources(tt.manifest, tt.defaultNamespace)
			if len(got) != len(tt.want) {
				t.Errorf("extractWorkloadResources() got %d resources, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractWorkloadResources()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMergeExpectedResources(t *testing.T) {
	tests := []struct {
		name       string
		manual     []recipe.ExpectedResource
		discovered []recipe.ExpectedResource
		want       []recipe.ExpectedResource
	}{
		{
			name:   "no manual, only discovered",
			manual: nil,
			discovered: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "b", Namespace: "ns1"},
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "b", Namespace: "ns1"},
			},
		},
		{
			name: "only manual, no discovered",
			manual: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a", Namespace: "ns1"},
			},
			discovered: nil,
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a", Namespace: "ns1"},
			},
		},
		{
			name: "manual takes precedence on conflict",
			manual: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "overlap", Namespace: "ns1"},
			},
			discovered: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "overlap", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "new", Namespace: "ns1"},
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "overlap", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "new", Namespace: "ns1"},
			},
		},
		{
			name: "different namespaces are not conflicts",
			manual: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "app", Namespace: "ns1"},
			},
			discovered: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "app", Namespace: "ns2"},
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "app", Namespace: "ns1"},
				{Kind: "Deployment", Name: "app", Namespace: "ns2"},
			},
		},
		{
			name:       "both empty",
			manual:     nil,
			discovered: nil,
			want:       []recipe.ExpectedResource{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeExpectedResources(tt.manual, tt.discovered)
			if len(got) != len(tt.want) {
				t.Errorf("mergeExpectedResources() got %d, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("mergeExpectedResources()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSplitYAMLDocuments(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     int // number of documents
	}{
		{
			name:     "single document no separator",
			manifest: "kind: Deployment\nmetadata:\n  name: test\n",
			want:     1,
		},
		{
			name:     "two documents with separator",
			manifest: "kind: A\n---\nkind: B\n",
			want:     2,
		},
		{
			name:     "leading separator",
			manifest: "---\nkind: A\n---\nkind: B\n",
			want:     2,
		},
		{
			name:     "empty manifest",
			manifest: "",
			want:     0,
		},
		{
			name:     "only separators",
			manifest: "---\n---\n---\n",
			want:     0,
		},
		{
			name:     "separator with whitespace",
			manifest: "kind: A\n  ---  \nkind: B\n",
			want:     2,
		},
		{
			name:     "three documents",
			manifest: "---\nkind: A\n---\nkind: B\n---\nkind: C\n",
			want:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitYAMLDocuments(tt.manifest)
			if len(got) != tt.want {
				t.Errorf("splitYAMLDocuments() got %d documents, want %d", len(got), tt.want)
			}
		})
	}
}

func TestCountByKind(t *testing.T) {
	tests := []struct {
		name      string
		resources []recipe.ExpectedResource
		want      string
	}{
		{
			name:      "empty",
			resources: nil,
			want:      "none",
		},
		{
			name: "single deployment",
			resources: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a"},
			},
			want: "1 Deployment",
		},
		{
			name: "multiple types",
			resources: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a"},
				{Kind: "Deployment", Name: "b"},
				{Kind: "DaemonSet", Name: "c"},
			},
			want: "2 Deployments, 1 DaemonSet",
		},
		{
			name: "all three types",
			resources: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "a"},
				{Kind: "DaemonSet", Name: "b"},
				{Kind: "StatefulSet", Name: "c"},
				{Kind: "StatefulSet", Name: "d"},
			},
			want: "1 Deployment, 1 DaemonSet, 2 StatefulSets",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countByKind(tt.resources)
			if got != tt.want {
				t.Errorf("countByKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveExpectedResources_NoManifestFiles(t *testing.T) {
	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "no-manifests",
				Type:      recipe.ComponentTypeHelm,
				Namespace: "default",
			},
		},
	}

	err := resolveExpectedResources(t.Context(), recipeResult)
	if err != nil {
		t.Fatalf("resolveExpectedResources() error = %v", err)
	}

	if len(recipeResult.ComponentRefs[0].ExpectedResources) != 0 {
		t.Errorf("expected no resources for component without manifest files, got %d",
			len(recipeResult.ComponentRefs[0].ExpectedResources))
	}
}

func TestResolveExpectedResources_ManualOnly(t *testing.T) {
	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "manual-comp",
				Namespace: "ns1",
				Type:      recipe.ComponentTypeHelm,
				ExpectedResources: []recipe.ExpectedResource{
					{Kind: "Deployment", Name: "manual-deploy", Namespace: "ns1"},
				},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult)

	got := recipeResult.ComponentRefs[0].ExpectedResources
	if len(got) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(got))
	}
	if got[0].Name != "manual-deploy" {
		t.Errorf("expected manual-deploy, got %s", got[0].Name)
	}
}

func TestResolveExpectedResources_MultipleComponents(t *testing.T) {
	orig := recipe.GetDataProvider()
	recipe.SetDataProvider(&testDataProvider{
		files: map[string][]byte{
			"manifests/deploy.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: comp-a-deploy\n  namespace: ns1\n"),
		},
	})
	t.Cleanup(func() { recipe.SetDataProvider(orig) })

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "comp-a",
				Namespace:     "ns1",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{"manifests/deploy.yaml"},
			},
			{
				Name:      "comp-b",
				Namespace: "ns2",
				Type:      recipe.ComponentTypeHelm,
				// No manifestFiles — should be skipped
			},
			{
				Name:      "comp-c",
				Namespace: "ns3",
				Type:      recipe.ComponentTypeHelm,
				ExpectedResources: []recipe.ExpectedResource{
					{Kind: "Deployment", Name: "manual-only", Namespace: "ns3"},
				},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult)

	// comp-a: should have 1 discovered resource from manifestFiles
	if got := len(recipeResult.ComponentRefs[0].ExpectedResources); got != 1 {
		t.Errorf("comp-a: expected 1 resource, got %d", got)
	}

	// comp-b: no resources → should be skipped (empty ExpectedResources)
	if got := len(recipeResult.ComponentRefs[1].ExpectedResources); got != 0 {
		t.Errorf("comp-b: expected 0 resources, got %d", got)
	}

	// comp-c: manual-only resource preserved
	if got := len(recipeResult.ComponentRefs[2].ExpectedResources); got != 1 {
		t.Errorf("comp-c: expected 1 resource, got %d", got)
	} else if recipeResult.ComponentRefs[2].ExpectedResources[0].Name != "manual-only" {
		t.Errorf("comp-c: expected manual-only, got %s",
			recipeResult.ComponentRefs[2].ExpectedResources[0].Name)
	}
}

func TestRenderManifestFiles(t *testing.T) {
	tests := []struct {
		name   string
		ref    recipe.ComponentRef
		values map[string]any
		files  map[string][]byte
		want   []recipe.ExpectedResource
	}{
		{
			name: "deployment extracted from static manifest",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "test-ns",
				ManifestFiles: []string{"manifests/deploy.yaml"},
			},
			values: map[string]any{},
			files: map[string][]byte{
				"manifests/deploy.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: my-deploy\n  namespace: test-ns\n"),
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "my-deploy", Namespace: "test-ns"},
			},
		},
		{
			name: "multiple workloads from single manifest",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "ns1",
				ManifestFiles: []string{"manifests/multi.yaml"},
			},
			values: map[string]any{},
			files: map[string][]byte{
				"manifests/multi.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: controller\n  namespace: ns1\n---\napiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: agent\n  namespace: ns1\n"),
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "controller", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "agent", Namespace: "ns1"},
			},
		},
		{
			name: "template interpolation with values and release namespace",
			ref: recipe.ComponentRef{
				Name:          "mycomp",
				Namespace:     "prod-ns",
				Chart:         "mychart",
				Version:       "1.0.0",
				ManifestFiles: []string{"manifests/templated.yaml"},
			},
			values: map[string]any{"appName": "web-server"},
			files: map[string][]byte{
				"manifests/templated.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ index .Values \"mycomp\" \"appName\" }}\n  namespace: {{ .Release.Namespace }}\n"),
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "web-server", Namespace: "prod-ns"},
			},
		},
		{
			name: "non-workload resources filtered out",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "ns1",
				ManifestFiles: []string{"manifests/service.yaml"},
			},
			values: map[string]any{},
			files: map[string][]byte{
				"manifests/service.yaml": []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: my-svc\n  namespace: ns1\n"),
			},
			want: nil,
		},
		{
			name: "missing manifest file skipped",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "ns1",
				ManifestFiles: []string{"manifests/nonexistent.yaml"},
			},
			values: map[string]any{},
			files:  map[string][]byte{},
			want:   nil,
		},
		{
			name: "invalid template execution skipped",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "ns1",
				ManifestFiles: []string{"manifests/bad.yaml"},
			},
			values: map[string]any{},
			files: map[string][]byte{
				"manifests/bad.yaml": []byte("{{ .Invalid.Nested.Missing }}"),
			},
			want: nil,
		},
		{
			name: "namespace falls back to ref namespace",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "fallback-ns",
				ManifestFiles: []string{"manifests/no-ns.yaml"},
			},
			values: map[string]any{},
			files: map[string][]byte{
				"manifests/no-ns.yaml": []byte("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: my-db\n"),
			},
			want: []recipe.ExpectedResource{
				{Kind: "StatefulSet", Name: "my-db", Namespace: "fallback-ns"},
			},
		},
		{
			name: "multiple manifest files combined",
			ref: recipe.ComponentRef{
				Name:          "test-comp",
				Namespace:     "ns1",
				ManifestFiles: []string{"manifests/a.yaml", "manifests/b.yaml"},
			},
			values: map[string]any{},
			files: map[string][]byte{
				"manifests/a.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: deploy-a\n  namespace: ns1\n"),
				"manifests/b.yaml": []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: ds-b\n  namespace: ns1\n"),
			},
			want: []recipe.ExpectedResource{
				{Kind: "Deployment", Name: "deploy-a", Namespace: "ns1"},
				{Kind: "DaemonSet", Name: "ds-b", Namespace: "ns1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := recipe.GetDataProvider()
			recipe.SetDataProvider(&testDataProvider{files: tt.files})
			t.Cleanup(func() { recipe.SetDataProvider(orig) })

			got := renderManifestFiles(context.Background(), tt.ref, tt.values)
			if len(got) != len(tt.want) {
				t.Fatalf("renderManifestFiles() got %d resources, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("renderManifestFiles()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestResolveExpectedResources_ManifestFileAutoDetect(t *testing.T) {
	orig := recipe.GetDataProvider()
	recipe.SetDataProvider(&testDataProvider{
		files: map[string][]byte{
			"manifests/deploy.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: auto-detected\n  namespace: test-ns\n"),
		},
	})
	t.Cleanup(func() { recipe.SetDataProvider(orig) })

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "manifest-only",
				Namespace:     "test-ns",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{"manifests/deploy.yaml"},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult)

	got := recipeResult.ComponentRefs[0].ExpectedResources
	want := []recipe.ExpectedResource{
		{Kind: "Deployment", Name: "auto-detected", Namespace: "test-ns"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d resources, got %d: %+v", len(want), len(got), got)
	}
	if got[0] != want[0] {
		t.Errorf("got %+v, want %+v", got[0], want[0])
	}
}

func TestResolveExpectedResources_ManualOverridesManifestFile(t *testing.T) {
	orig := recipe.GetDataProvider()
	recipe.SetDataProvider(&testDataProvider{
		files: map[string][]byte{
			"manifests/workloads.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: overlap\n  namespace: ns1\n---\napiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: discovered-only\n  namespace: ns1\n"),
		},
	})
	t.Cleanup(func() { recipe.SetDataProvider(orig) })

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "mixed-comp",
				Namespace:     "ns1",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{"manifests/workloads.yaml"},
				ExpectedResources: []recipe.ExpectedResource{
					{Kind: "Deployment", Name: "overlap", Namespace: "ns1"},
				},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult)

	got := recipeResult.ComponentRefs[0].ExpectedResources
	want := []recipe.ExpectedResource{
		{Kind: "Deployment", Name: "overlap", Namespace: "ns1"},
		{Kind: "DaemonSet", Name: "discovered-only", Namespace: "ns1"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d resources, got %d: %+v", len(want), len(got), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("resource[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWriteValuesToTempFile(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
	}{
		{
			name:   "simple values",
			values: map[string]any{"key": "value"},
		},
		{
			name:   "nested values",
			values: map[string]any{"a": map[string]any{"b": "c"}},
		},
		{
			name:   "empty values",
			values: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := writeValuesToTempFile(tt.values)
			if err != nil {
				t.Fatalf("writeValuesToTempFile() error = %v", err)
			}
			defer os.Remove(path)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read temp file: %v", err)
			}

			var parsed map[string]any
			if err := yaml.Unmarshal(data, &parsed); err != nil {
				t.Errorf("temp file is not valid YAML: %v", err)
			}
		})
	}
}

func TestResolveExpectedResources_SkipsEmptyChartCoordinates(t *testing.T) {
	// Components without chart coordinates (empty Source/Chart) should skip
	// discovery without error — no CLI lookup is needed.
	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name: "no-chart",
				Type: recipe.ComponentTypeHelm,
				// Source and Chart are empty — skips helm template
			},
		},
	}

	err := resolveExpectedResources(t.Context(), recipeResult)
	if err != nil {
		t.Errorf("resolveExpectedResources() error = %v", err)
	}

	if len(recipeResult.ComponentRefs[0].ExpectedResources) != 0 {
		t.Errorf("expected no resources for component without chart coordinates, got %d",
			len(recipeResult.ComponentRefs[0].ExpectedResources))
	}
}

func TestResolveExpectedResources_ErrorOnMissingCLI(t *testing.T) {
	// Components with chart coordinates should cause a hard error
	// when the required CLI tool is not found in PATH.
	t.Setenv("PATH", t.TempDir()) // empty dir — no binaries

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:   "my-chart",
				Type:   recipe.ComponentTypeHelm,
				Source: "https://charts.example.com",
				Chart:  "my-chart",
			},
		},
	}

	err := resolveExpectedResources(t.Context(), recipeResult)
	if err == nil {
		t.Fatal("expected error when helm CLI is missing, got nil")
	}
	if !strings.Contains(err.Error(), "helm CLI not found") {
		t.Errorf("expected error about missing helm CLI, got: %v", err)
	}
}

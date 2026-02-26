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
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
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
				{Kind: kindDeployment, Name: "my-deploy", Namespace: "gpu-operator"},
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
				{Kind: kindDeployment, Name: "controller", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "agent", Namespace: "ns1"},
				{Kind: kindStatefulSet, Name: "db", Namespace: "ns1"},
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
				{Kind: kindDeployment, Name: "my-deploy", Namespace: "ns1"},
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
				{Kind: kindDeployment, Name: "no-ns-deploy", Namespace: "gpu-operator"},
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
				{Kind: kindDeployment, Name: "good-deploy", Namespace: "ns1"},
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
				{Kind: kindDaemonSet, Name: "my-ds", Namespace: "kube-system"},
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
				{Kind: kindDeployment, Name: "a", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "b", Namespace: "ns1"},
			},
			want: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "a", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "b", Namespace: "ns1"},
			},
		},
		{
			name: "only manual, no discovered",
			manual: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "a", Namespace: "ns1"},
			},
			discovered: nil,
			want: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "a", Namespace: "ns1"},
			},
		},
		{
			name: "manual takes precedence on conflict",
			manual: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "overlap", Namespace: "ns1"},
			},
			discovered: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "overlap", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "new", Namespace: "ns1"},
			},
			want: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "overlap", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "new", Namespace: "ns1"},
			},
		},
		{
			name: "different namespaces are not conflicts",
			manual: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "app", Namespace: "ns1"},
			},
			discovered: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "app", Namespace: "ns2"},
			},
			want: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "app", Namespace: "ns1"},
				{Kind: kindDeployment, Name: "app", Namespace: "ns2"},
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
				{Kind: kindDeployment, Name: "a"},
			},
			want: "1 Deployment",
		},
		{
			name: "multiple types",
			resources: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "a"},
				{Kind: kindDeployment, Name: "b"},
				{Kind: kindDaemonSet, Name: "c"},
			},
			want: "2 Deployments, 1 DaemonSet",
		},
		{
			name: "all three types",
			resources: []recipe.ExpectedResource{
				{Kind: kindDeployment, Name: "a"},
				{Kind: kindDaemonSet, Name: "b"},
				{Kind: kindStatefulSet, Name: "c"},
				{Kind: kindStatefulSet, Name: "d"},
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

	err := resolveExpectedResources(t.Context(), recipeResult, "")
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
					{Kind: kindDeployment, Name: "manual-deploy", Namespace: "ns1"},
				},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult, "")

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
					{Kind: kindDeployment, Name: "manual-only", Namespace: "ns3"},
				},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult, "")

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
				{Kind: kindDeployment, Name: "my-deploy", Namespace: "test-ns"},
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
				{Kind: kindDeployment, Name: "controller", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "agent", Namespace: "ns1"},
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
				{Kind: kindDeployment, Name: "web-server", Namespace: "prod-ns"},
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
				{Kind: kindStatefulSet, Name: "my-db", Namespace: "fallback-ns"},
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
				{Kind: kindDeployment, Name: "deploy-a", Namespace: "ns1"},
				{Kind: kindDaemonSet, Name: "ds-b", Namespace: "ns1"},
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

	_ = resolveExpectedResources(t.Context(), recipeResult, "")

	got := recipeResult.ComponentRefs[0].ExpectedResources
	want := []recipe.ExpectedResource{
		{Kind: kindDeployment, Name: "auto-detected", Namespace: "test-ns"},
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
					{Kind: kindDeployment, Name: "overlap", Namespace: "ns1"},
				},
			},
		},
	}

	_ = resolveExpectedResources(t.Context(), recipeResult, "")

	got := recipeResult.ComponentRefs[0].ExpectedResources
	want := []recipe.ExpectedResource{
		{Kind: kindDeployment, Name: "overlap", Namespace: "ns1"},
		{Kind: kindDaemonSet, Name: "discovered-only", Namespace: "ns1"},
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

	err := resolveExpectedResources(t.Context(), recipeResult, "")
	if err != nil {
		t.Errorf("resolveExpectedResources() error = %v", err)
	}

	if len(recipeResult.ComponentRefs[0].ExpectedResources) != 0 {
		t.Errorf("expected no resources for component without chart coordinates, got %d",
			len(recipeResult.ComponentRefs[0].ExpectedResources))
	}
}

func TestRenderHelmTemplate_LocalChart(t *testing.T) {
	// Build a minimal Helm chart in a temp directory to test the SDK rendering
	// path without network access.
	chartDir := t.TempDir()

	// Chart.yaml — minimal valid chart metadata
	chartYAML := `apiVersion: v2
name: test-chart
version: 0.1.0
`
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatalf("failed to write Chart.yaml: %v", err)
	}

	// templates/ directory with a Deployment and a DaemonSet
	templatesDir := filepath.Join(chartDir, "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		t.Fatalf("failed to create templates dir: %v", err)
	}

	deploymentYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-server
  namespace: {{ .Release.Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: server
  template:
    metadata:
      labels:
        app: server
    spec:
      containers:
        - name: server
          image: nginx:latest
`
	if err := os.WriteFile(filepath.Join(templatesDir, "deployment.yaml"), []byte(deploymentYAML), 0o644); err != nil {
		t.Fatalf("failed to write deployment.yaml: %v", err)
	}

	daemonsetYAML := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{ .Release.Name }}-agent
  namespace: {{ .Release.Namespace }}
spec:
  selector:
    matchLabels:
      app: agent
  template:
    metadata:
      labels:
        app: agent
    spec:
      containers:
        - name: agent
          image: busybox:latest
`
	if err := os.WriteFile(filepath.Join(templatesDir, "daemonset.yaml"), []byte(daemonsetYAML), 0o644); err != nil {
		t.Fatalf("failed to write daemonset.yaml: %v", err)
	}

	// Use a file:// source so locateChart resolves locally without network.
	ref := recipe.ComponentRef{
		Name:      "my-release",
		Namespace: "test-ns",
		Type:      recipe.ComponentTypeHelm,
		Source:    "", // not used for local path
		Chart:     chartDir,
		Version:   "0.1.0",
	}

	resources, err := renderHelmTemplate(t.Context(), ref, nil, "")
	if err != nil {
		t.Fatalf("renderHelmTemplate() error = %v", err)
	}

	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d: %v", len(resources), resources)
	}

	// Verify the extracted resources
	foundDeployment := false
	foundDaemonSet := false
	for _, r := range resources {
		switch {
		case r.Kind == kindDeployment && r.Name == "my-release-server" && r.Namespace == "test-ns":
			foundDeployment = true
		case r.Kind == kindDaemonSet && r.Name == "my-release-agent" && r.Namespace == "test-ns":
			foundDaemonSet = true
		}
	}

	if !foundDeployment {
		t.Errorf("expected Deployment my-release-server in test-ns, got %v", resources)
	}
	if !foundDaemonSet {
		t.Errorf("expected DaemonSet my-release-agent in test-ns, got %v", resources)
	}
}

func TestRenderHelmTemplate_WithValues(t *testing.T) {
	// Test that values are passed through to the chart rendering.
	chartDir := t.TempDir()

	chartYAML := `apiVersion: v2
name: values-test
version: 0.1.0
`
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatalf("failed to write Chart.yaml: %v", err)
	}

	templatesDir := filepath.Join(chartDir, "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		t.Fatalf("failed to create templates dir: %v", err)
	}

	// Template that conditionally creates a StatefulSet based on values
	ssYAML := `{{- if .Values.statefulset.enabled }}
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Values.statefulset.name }}
  namespace: {{ .Release.Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: db
  template:
    metadata:
      labels:
        app: db
    spec:
      containers:
        - name: db
          image: postgres:latest
{{- end }}
`
	if err := os.WriteFile(filepath.Join(templatesDir, "statefulset.yaml"), []byte(ssYAML), 0o644); err != nil {
		t.Fatalf("failed to write statefulset.yaml: %v", err)
	}

	ref := recipe.ComponentRef{
		Name:      "db-release",
		Namespace: "db-ns",
		Type:      recipe.ComponentTypeHelm,
		Chart:     chartDir,
		Version:   "0.1.0",
	}

	// With values that enable the StatefulSet
	values := map[string]any{
		"statefulset": map[string]any{
			"enabled": true,
			"name":    "my-database",
		},
	}

	resources, err := renderHelmTemplate(t.Context(), ref, values, "")
	if err != nil {
		t.Fatalf("renderHelmTemplate() error = %v", err)
	}

	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d: %v", len(resources), resources)
	}

	if resources[0].Kind != kindStatefulSet || resources[0].Name != "my-database" || resources[0].Namespace != "db-ns" {
		t.Errorf("expected StatefulSet my-database in db-ns, got %v", resources[0])
	}

	// With values that disable the StatefulSet — need a fresh chart dir
	// because renderHelmTemplate cleans up the downloaded chart path.
	chartDir2 := t.TempDir()
	writeErr := os.WriteFile(filepath.Join(chartDir2, "Chart.yaml"), []byte(chartYAML), 0o644)
	if writeErr != nil {
		t.Fatalf("failed to write Chart.yaml: %v", writeErr)
	}
	templatesDir2 := filepath.Join(chartDir2, "templates")
	mkdirErr := os.MkdirAll(templatesDir2, 0o755)
	if mkdirErr != nil {
		t.Fatalf("failed to create templates dir: %v", mkdirErr)
	}
	writeErr = os.WriteFile(filepath.Join(templatesDir2, "statefulset.yaml"), []byte(ssYAML), 0o644)
	if writeErr != nil {
		t.Fatalf("failed to write statefulset.yaml: %v", writeErr)
	}

	ref2 := recipe.ComponentRef{
		Name:      "db-release",
		Namespace: "db-ns",
		Type:      recipe.ComponentTypeHelm,
		Chart:     chartDir2,
		Version:   "0.1.0",
	}

	disabledValues := map[string]any{
		"statefulset": map[string]any{"enabled": false},
	}

	resources, err = renderHelmTemplate(t.Context(), ref2, disabledValues, "")
	if err != nil {
		t.Fatalf("renderHelmTemplate() with disabled error = %v", err)
	}

	if len(resources) != 0 {
		t.Errorf("expected 0 resources when disabled, got %d: %v", len(resources), resources)
	}
}

func TestResolveHealthCheckAsserts(t *testing.T) {
	assertContent := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: gpu-operator\n  namespace: gpu-operator\nstatus:\n  readyReplicas: 1\n"

	tests := []struct {
		name              string
		registryYAML      string
		files             map[string][]byte
		componentRefs     []recipe.ComponentRef
		wantHealthAsserts map[string]string // component name -> expected HealthCheckAsserts
	}{
		{
			name: "loads assert file for component with healthCheck.assertFile",
			registryYAML: `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    healthCheck:
      assertFile: checks/gpu-operator/assert.yaml
    helm:
      defaultRepository: https://helm.ngc.nvidia.com/nvidia
      defaultChart: nvidia/gpu-operator
`,
			files: map[string][]byte{
				"checks/gpu-operator/assert.yaml": []byte(assertContent),
			},
			componentRefs: []recipe.ComponentRef{
				{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
			},
			wantHealthAsserts: map[string]string{
				"gpu-operator": assertContent,
			},
		},
		{
			name: "skips component not in registry",
			registryYAML: `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultChart: nvidia/gpu-operator
`,
			files: map[string][]byte{},
			componentRefs: []recipe.ComponentRef{
				{Name: "unknown-component", Type: recipe.ComponentTypeHelm},
			},
			wantHealthAsserts: map[string]string{
				"unknown-component": "",
			},
		},
		{
			name: "skips component without healthCheck.assertFile",
			registryYAML: `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultChart: nvidia/gpu-operator
`,
			files: map[string][]byte{},
			componentRefs: []recipe.ComponentRef{
				{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
			},
			wantHealthAsserts: map[string]string{
				"gpu-operator": "",
			},
		},
		{
			name: "warns and skips when assert file not found",
			registryYAML: `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    healthCheck:
      assertFile: checks/missing/assert.yaml
    helm:
      defaultChart: nvidia/gpu-operator
`,
			files: map[string][]byte{}, // assert file missing
			componentRefs: []recipe.ComponentRef{
				{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
			},
			wantHealthAsserts: map[string]string{
				"gpu-operator": "", // should remain empty
			},
		},
		{
			name: "mixed components — only loads for those with assertFile",
			registryYAML: `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    healthCheck:
      assertFile: checks/gpu-operator/assert.yaml
    helm:
      defaultChart: nvidia/gpu-operator
  - name: network-operator
    displayName: Network Operator
    helm:
      defaultChart: nvidia/network-operator
`,
			files: map[string][]byte{
				"checks/gpu-operator/assert.yaml": []byte(assertContent),
			},
			componentRefs: []recipe.ComponentRef{
				{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
				{Name: "network-operator", Type: recipe.ComponentTypeHelm},
			},
			wantHealthAsserts: map[string]string{
				"gpu-operator":     assertContent,
				"network-operator": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Include registry.yaml in the data provider files
			allFiles := make(map[string][]byte, len(tt.files)+1)
			allFiles["registry.yaml"] = []byte(tt.registryYAML)
			for k, v := range tt.files {
				allFiles[k] = v
			}

			origProvider := recipe.GetDataProvider()
			recipe.SetDataProvider(&testDataProvider{files: allFiles})
			recipe.ResetComponentRegistryForTesting()
			t.Cleanup(func() {
				recipe.SetDataProvider(origProvider)
				recipe.ResetComponentRegistryForTesting()
			})

			recipeResult := &recipe.RecipeResult{
				ComponentRefs: tt.componentRefs,
			}

			resolveHealthCheckAsserts(t.Context(), recipeResult)

			for _, ref := range recipeResult.ComponentRefs {
				want, ok := tt.wantHealthAsserts[ref.Name]
				if !ok {
					continue
				}
				if ref.HealthCheckAsserts != want {
					t.Errorf("component %s: HealthCheckAsserts = %q, want %q",
						ref.Name, ref.HealthCheckAsserts, want)
				}
			}
		})
	}
}

func TestResolveExpectedResources_SkipsChainsawComponents(t *testing.T) {
	// Components with HealthCheckAsserts should skip auto-discovery entirely.
	orig := recipe.GetDataProvider()
	recipe.SetDataProvider(&testDataProvider{
		files: map[string][]byte{
			"manifests/deploy.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: should-not-appear\n  namespace: ns1\n"),
		},
	})
	t.Cleanup(func() { recipe.SetDataProvider(orig) })

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:               "chainsaw-comp",
				Namespace:          "ns1",
				Type:               recipe.ComponentTypeHelm,
				ManifestFiles:      []string{"manifests/deploy.yaml"},
				HealthCheckAsserts: "apiVersion: v1\nkind: Namespace\n",
			},
		},
	}

	err := resolveExpectedResources(t.Context(), recipeResult, "")
	if err != nil {
		t.Fatalf("resolveExpectedResources() error = %v", err)
	}

	// Component with HealthCheckAsserts should NOT have auto-discovered resources.
	if len(recipeResult.ComponentRefs[0].ExpectedResources) != 0 {
		t.Errorf("expected 0 auto-discovered resources for chainsaw component, got %d",
			len(recipeResult.ComponentRefs[0].ExpectedResources))
	}
}

func TestBuildKustomizeURL(t *testing.T) {
	tests := []struct {
		name   string
		source string
		path   string
		tag    string
		want   string
	}{
		{
			name:   "full URL with source, path, and tag",
			source: "https://github.com/org/repo",
			path:   "deploy/production",
			tag:    "v1.0.0",
			want:   "https://github.com/org/repo//deploy/production?ref=v1.0.0",
		},
		{
			name:   "source and tag, no path",
			source: "https://github.com/org/repo",
			path:   "",
			tag:    "v2.0.0",
			want:   "https://github.com/org/repo?ref=v2.0.0",
		},
		{
			name:   "source and path, no tag",
			source: "https://github.com/org/repo",
			path:   "config/base",
			tag:    "",
			want:   "https://github.com/org/repo//config/base",
		},
		{
			name:   "source only",
			source: "https://github.com/org/repo",
			path:   "",
			tag:    "",
			want:   "https://github.com/org/repo",
		},
		{
			name:   "ssh-style source",
			source: "git@github.com:org/repo.git",
			path:   "overlays/prod",
			tag:    "main",
			want:   "git@github.com:org/repo.git//overlays/prod?ref=main",
		},
		{
			name:   "trailing slash in source is trimmed",
			source: "https://github.com/org/repo/",
			path:   "deploy/production",
			tag:    "v1.0.0",
			want:   "https://github.com/org/repo//deploy/production?ref=v1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildKustomizeURL(tt.source, tt.path, tt.tag)
			if got != tt.want {
				t.Errorf("buildKustomizeURL(%q, %q, %q) = %q, want %q",
					tt.source, tt.path, tt.tag, got, tt.want)
			}
		})
	}
}

func TestRenderKustomizeTemplate_LocalDirectory(t *testing.T) {
	// Build a minimal kustomization in a temp directory to test the krusty
	// rendering path without network access.
	kustDir := t.TempDir()

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - daemonset.yaml
`
	if err := os.WriteFile(filepath.Join(kustDir, "kustomization.yaml"), []byte(kustomizationYAML), 0o644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	deploymentYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
  namespace: my-ns
spec:
  replicas: 1
  selector:
    matchLabels:
      app: controller
  template:
    metadata:
      labels:
        app: controller
    spec:
      containers:
        - name: controller
          image: nginx:latest
`
	if err := os.WriteFile(filepath.Join(kustDir, "deployment.yaml"), []byte(deploymentYAML), 0o644); err != nil {
		t.Fatalf("failed to write deployment.yaml: %v", err)
	}

	daemonsetYAML := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: agent
  namespace: my-ns
spec:
  selector:
    matchLabels:
      app: agent
  template:
    metadata:
      labels:
        app: agent
    spec:
      containers:
        - name: agent
          image: busybox:latest
`
	if err := os.WriteFile(filepath.Join(kustDir, "daemonset.yaml"), []byte(daemonsetYAML), 0o644); err != nil {
		t.Fatalf("failed to write daemonset.yaml: %v", err)
	}

	ref := recipe.ComponentRef{
		Name:      "kust-comp",
		Namespace: "my-ns",
		Type:      recipe.ComponentTypeKustomize,
		Source:    kustDir,
	}

	resources, err := renderKustomizeTemplate(t.Context(), ref)
	if err != nil {
		t.Fatalf("renderKustomizeTemplate() error = %v", err)
	}

	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d: %v", len(resources), resources)
	}

	foundDeployment := false
	foundDaemonSet := false
	for _, r := range resources {
		switch {
		case r.Kind == kindDeployment && r.Name == "controller" && r.Namespace == "my-ns":
			foundDeployment = true
		case r.Kind == kindDaemonSet && r.Name == "agent" && r.Namespace == "my-ns":
			foundDaemonSet = true
		}
	}

	if !foundDeployment {
		t.Errorf("expected Deployment controller in my-ns, got %v", resources)
	}
	if !foundDaemonSet {
		t.Errorf("expected DaemonSet agent in my-ns, got %v", resources)
	}
}

func TestRenderKustomizeTemplate_NonWorkloadFiltered(t *testing.T) {
	// Verify that non-workload resources (Service, ConfigMap) are filtered out.
	kustDir := t.TempDir()

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - resources.yaml
`
	if err := os.WriteFile(filepath.Join(kustDir, "kustomization.yaml"), []byte(kustomizationYAML), 0o644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	resourcesYAML := `apiVersion: v1
kind: Service
metadata:
  name: my-svc
  namespace: ns1
spec:
  ports:
    - port: 80
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: ns1
data:
  key: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: only-workload
  namespace: ns1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
        - name: test
          image: nginx:latest
`
	if err := os.WriteFile(filepath.Join(kustDir, "resources.yaml"), []byte(resourcesYAML), 0o644); err != nil {
		t.Fatalf("failed to write resources.yaml: %v", err)
	}

	ref := recipe.ComponentRef{
		Name:      "filtered-comp",
		Namespace: "ns1",
		Type:      recipe.ComponentTypeKustomize,
		Source:    kustDir,
	}

	resources, err := renderKustomizeTemplate(t.Context(), ref)
	if err != nil {
		t.Fatalf("renderKustomizeTemplate() error = %v", err)
	}

	if len(resources) != 1 {
		t.Fatalf("expected 1 workload resource, got %d: %v", len(resources), resources)
	}
	if resources[0].Kind != kindDeployment || resources[0].Name != "only-workload" {
		t.Errorf("expected Deployment only-workload, got %v", resources[0])
	}
}

func TestResolveExpectedResources_KustomizeComponent(t *testing.T) {
	// Verify that kustomize components get auto-discovered resources.
	kustDir := t.TempDir()

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`
	if err := os.WriteFile(filepath.Join(kustDir, "kustomization.yaml"), []byte(kustomizationYAML), 0o644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}

	deploymentYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: kust-deploy
  namespace: kust-ns
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
        - name: test
          image: nginx:latest
`
	if err := os.WriteFile(filepath.Join(kustDir, "deployment.yaml"), []byte(deploymentYAML), 0o644); err != nil {
		t.Fatalf("failed to write deployment.yaml: %v", err)
	}

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "kust-comp",
				Namespace: "kust-ns",
				Type:      recipe.ComponentTypeKustomize,
				Source:    kustDir,
			},
		},
	}

	err := resolveExpectedResources(t.Context(), recipeResult, "")
	if err != nil {
		t.Fatalf("resolveExpectedResources() error = %v", err)
	}

	got := recipeResult.ComponentRefs[0].ExpectedResources
	if len(got) != 1 {
		t.Fatalf("expected 1 resource, got %d: %v", len(got), got)
	}
	if got[0].Kind != kindDeployment || got[0].Name != "kust-deploy" || got[0].Namespace != "kust-ns" {
		t.Errorf("expected Deployment kust-deploy in kust-ns, got %v", got[0])
	}
}

func TestRenderKustomizeTemplate_InvalidSource(t *testing.T) {
	// Verify that an invalid source produces a wrapped error, not a panic.
	ref := recipe.ComponentRef{
		Name:      "bad-comp",
		Namespace: "ns1",
		Type:      recipe.ComponentTypeKustomize,
		Source:    "/nonexistent/path/to/kustomization",
	}

	resources, err := renderKustomizeTemplate(t.Context(), ref)
	if err == nil {
		t.Fatalf("expected error for invalid source, got %d resources", len(resources))
	}
	if resources != nil {
		t.Errorf("expected nil resources on error, got %v", resources)
	}
}

func TestRenderKustomizeTemplate_CancelledContext(t *testing.T) {
	// Use a valid temp kustomization directory so the only failure path
	// is the canceled context, not a missing filesystem path.
	kustDir := t.TempDir()
	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`
	if err := os.WriteFile(filepath.Join(kustDir, "kustomization.yaml"), []byte(kustomizationYAML), 0o644); err != nil {
		t.Fatalf("failed to write kustomization.yaml: %v", err)
	}
	deploymentYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
  namespace: ns1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
        - name: test
          image: nginx:latest
`
	if err := os.WriteFile(filepath.Join(kustDir, "deployment.yaml"), []byte(deploymentYAML), 0o644); err != nil {
		t.Fatalf("failed to write deployment.yaml: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	ref := recipe.ComponentRef{
		Name:      "cancelled-comp",
		Namespace: "ns1",
		Type:      recipe.ComponentTypeKustomize,
		Source:    kustDir,
	}

	_, err := renderKustomizeTemplate(ctx, ref)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestResolveExpectedResources_KustomizeSkipsEmptySource(t *testing.T) {
	// Kustomize components without Source should skip discovery without error.
	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name: "no-source",
				Type: recipe.ComponentTypeKustomize,
				// Source is empty — skips kustomize build
			},
		},
	}

	err := resolveExpectedResources(t.Context(), recipeResult, "")
	if err != nil {
		t.Errorf("resolveExpectedResources() error = %v", err)
	}

	if len(recipeResult.ComponentRefs[0].ExpectedResources) != 0 {
		t.Errorf("expected no resources for kustomize component without source, got %d",
			len(recipeResult.ComponentRefs[0].ExpectedResources))
	}
}

func TestResolveExpectedResources_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "some-chart",
				Type:      recipe.ComponentTypeHelm,
				Source:    "https://charts.example.com",
				Chart:     "my-chart",
				Namespace: "default",
			},
		},
	}

	err := resolveExpectedResources(ctx, recipeResult, "")
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

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

package k8s

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/stretchr/testify/assert"
)

func TestMapArgocdApplication(t *testing.T) {
	tests := []struct {
		name     string
		obj      map[string]any
		expected map[string]string
	}{
		{
			name:     "nil object",
			obj:      nil,
			expected: map[string]string{},
		},
		{
			name: "missing name",
			obj: map[string]any{
				"metadata": map[string]any{},
			},
			expected: map[string]string{},
		},
		{
			name: "single-source app with full metadata",
			obj: map[string]any{
				"apiVersion": "argoproj.io/v1alpha1",
				"kind":       "Application",
				"metadata": map[string]any{
					"name":      "gpu-operator",
					"namespace": "argocd",
				},
				"spec": map[string]any{
					"project": "default",
					"destination": map[string]any{
						"namespace": "gpu-operator",
						"server":    "https://kubernetes.default.svc",
					},
					"source": map[string]any{
						"repoURL":        "https://helm.ngc.nvidia.com/nvidia",
						"chart":          "gpu-operator",
						"targetRevision": "v25.3.0",
						"helm": map[string]any{
							"parameters": []any{
								map[string]any{"name": "driver.version", "value": "570.86.16"},
								map[string]any{"name": "driver.enabled", "value": "true"},
							},
						},
					},
				},
				"status": map[string]any{
					"sync": map[string]any{
						"status": "Synced",
					},
					"health": map[string]any{
						"status": "Healthy",
					},
				},
			},
			expected: map[string]string{
				"gpu-operator.namespace":                             "argocd",
				"gpu-operator.project":                               "default",
				"gpu-operator.targetNamespace":                       "gpu-operator",
				"gpu-operator.destination.server":                    "https://kubernetes.default.svc",
				"gpu-operator.source.repoURL":                        "https://helm.ngc.nvidia.com/nvidia",
				"gpu-operator.source.chart":                          "gpu-operator",
				"gpu-operator.source.targetRevision":                 "v25.3.0",
				"gpu-operator.source.helm.parameters.driver.version": "570.86.16",
				"gpu-operator.source.helm.parameters.driver.enabled": "true",
				"gpu-operator.syncStatus":                            "Synced",
				"gpu-operator.healthStatus":                          "Healthy",
			},
		},
		{
			name: "multi-source app",
			obj: map[string]any{
				"apiVersion": "argoproj.io/v1alpha1",
				"kind":       "Application",
				"metadata": map[string]any{
					"name":      "my-app",
					"namespace": "argocd",
				},
				"spec": map[string]any{
					"project": "infra",
					"destination": map[string]any{
						"namespace": "default",
					},
					"sources": []any{
						map[string]any{
							"repoURL":        "https://charts.example.com",
							"chart":          "frontend",
							"targetRevision": "1.0.0",
						},
						map[string]any{
							"repoURL":        "https://github.com/example/backend",
							"path":           "deploy/",
							"targetRevision": "main",
						},
					},
				},
				"status": map[string]any{
					"sync": map[string]any{
						"status": "OutOfSync",
					},
					"health": map[string]any{
						"status": "Degraded",
					},
				},
			},
			expected: map[string]string{
				"my-app.namespace":                "argocd",
				"my-app.project":                  "infra",
				"my-app.targetNamespace":          "default",
				"my-app.sources.0.repoURL":        "https://charts.example.com",
				"my-app.sources.0.chart":          "frontend",
				"my-app.sources.0.targetRevision": "1.0.0",
				"my-app.sources.1.repoURL":        "https://github.com/example/backend",
				"my-app.sources.1.path":           "deploy/",
				"my-app.sources.1.targetRevision": "main",
				"my-app.syncStatus":               "OutOfSync",
				"my-app.healthStatus":             "Degraded",
			},
		},
		{
			name: "minimal app - name only",
			obj: map[string]any{
				"metadata": map[string]any{
					"name": "minimal-app",
				},
				"spec": map[string]any{},
			},
			expected: map[string]string{},
		},
		{
			name: "app with no status",
			obj: map[string]any{
				"metadata": map[string]any{
					"name":      "no-status",
					"namespace": "argocd",
				},
				"spec": map[string]any{
					"project": "default",
					"source": map[string]any{
						"repoURL":        "https://github.com/example/app",
						"path":           "manifests/",
						"targetRevision": "HEAD",
					},
				},
			},
			expected: map[string]string{
				"no-status.namespace":             "argocd",
				"no-status.project":               "default",
				"no-status.source.repoURL":        "https://github.com/example/app",
				"no-status.source.path":           "manifests/",
				"no-status.source.targetRevision": "HEAD",
			},
		},
		{
			name: "app with helm values string",
			obj: map[string]any{
				"metadata": map[string]any{
					"name":      "values-app",
					"namespace": "argocd",
				},
				"spec": map[string]any{
					"source": map[string]any{
						"repoURL":        "https://charts.example.com",
						"chart":          "my-chart",
						"targetRevision": "2.0.0",
						"helm": map[string]any{
							"values": "driver:\n  enabled: true\n  version: 570.86.16",
						},
					},
				},
			},
			expected: map[string]string{
				"values-app.namespace":             "argocd",
				"values-app.source.repoURL":        "https://charts.example.com",
				"values-app.source.chart":          "my-chart",
				"values-app.source.targetRevision": "2.0.0",
				"values-app.source.helm.values":    "driver:\n  enabled: true\n  version: 570.86.16",
			},
		},
		{
			name: "app with helm valueFiles",
			obj: map[string]any{
				"metadata": map[string]any{
					"name":      "vf-app",
					"namespace": "argocd",
				},
				"spec": map[string]any{
					"source": map[string]any{
						"repoURL": "https://github.com/example/app",
						"path":    "charts/",
						"helm": map[string]any{
							"valueFiles": []any{"values.yaml", "values-prod.yaml"},
						},
					},
				},
			},
			expected: map[string]string{
				"vf-app.namespace":              "argocd",
				"vf-app.source.repoURL":         "https://github.com/example/app",
				"vf-app.source.path":            "charts/",
				"vf-app.source.helm.valueFiles": "values.yaml,values-prod.yaml",
			},
		},
		{
			name: "app with no spec",
			obj: map[string]any{
				"metadata": map[string]any{
					"name":      "empty-spec",
					"namespace": "argocd",
				},
			},
			expected: map[string]string{
				"empty-spec.namespace": "argocd",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make(map[string]measurement.Reading)
			mapArgocdApplication(tt.obj, data)

			if len(data) != len(tt.expected) {
				t.Fatalf("got %d readings, want %d\ngot: %v", len(data), len(tt.expected), readingKeys(data))
			}

			for key, wantVal := range tt.expected {
				got, exists := data[key]
				if !exists {
					t.Errorf("missing key %q", key)
					continue
				}
				if got.Any() != wantVal {
					t.Errorf("key %q = %v, want %q", key, got.Any(), wantVal)
				}
			}
		})
	}
}

func TestMapArgocdSource(t *testing.T) {
	tests := []struct {
		name     string
		source   map[string]any
		prefix   string
		expected map[string]string
	}{
		{
			name:     "empty source",
			source:   map[string]any{},
			prefix:   "app.source",
			expected: map[string]string{},
		},
		{
			name: "helm chart source",
			source: map[string]any{
				"repoURL":        "https://helm.ngc.nvidia.com/nvidia",
				"chart":          "gpu-operator",
				"targetRevision": "v25.3.0",
			},
			prefix: "app.source",
			expected: map[string]string{
				"app.source.repoURL":        "https://helm.ngc.nvidia.com/nvidia",
				"app.source.chart":          "gpu-operator",
				"app.source.targetRevision": "v25.3.0",
			},
		},
		{
			name: "git path source",
			source: map[string]any{
				"repoURL":        "https://github.com/example/repo",
				"path":           "deploy/production",
				"targetRevision": "main",
			},
			prefix: "app.sources.0",
			expected: map[string]string{
				"app.sources.0.repoURL":        "https://github.com/example/repo",
				"app.sources.0.path":           "deploy/production",
				"app.sources.0.targetRevision": "main",
			},
		},
		{
			name: "source with helm parameters",
			source: map[string]any{
				"repoURL": "https://charts.example.com",
				"chart":   "my-chart",
				"helm": map[string]any{
					"parameters": []any{
						map[string]any{"name": "image.tag", "value": "v1.0"},
						map[string]any{"name": "replicas", "value": "3"},
					},
				},
			},
			prefix: "app.source",
			expected: map[string]string{
				"app.source.repoURL":                   "https://charts.example.com",
				"app.source.chart":                     "my-chart",
				"app.source.helm.parameters.image.tag": "v1.0",
				"app.source.helm.parameters.replicas":  "3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make(map[string]measurement.Reading)
			mapArgocdSource(tt.source, tt.prefix, data)

			if len(data) != len(tt.expected) {
				t.Fatalf("got %d readings, want %d\ngot: %v", len(data), len(tt.expected), readingKeys(data))
			}

			for key, wantVal := range tt.expected {
				got, exists := data[key]
				if !exists {
					t.Errorf("missing key %q", key)
					continue
				}
				if got.Any() != wantVal {
					t.Errorf("key %q = %v, want %q", key, got.Any(), wantVal)
				}
			}
		})
	}
}

func TestCollectArgocdApplications_EmptyCluster(t *testing.T) {
	t.Setenv("NODE_NAME", testNodeName)

	collector := createTestCollector()
	ctx := context.TODO()

	data := collector.collectArgocdApplications(ctx)
	assert.NotNil(t, data)
	assert.Empty(t, data)
}

func TestCollectArgocdApplications_CancelledContext(t *testing.T) {
	collector := createTestCollector()

	ctx, cancel := context.WithCancel(context.TODO())
	cancel()

	data := collector.collectArgocdApplications(ctx)
	assert.NotNil(t, data)
	assert.Empty(t, data)
}

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
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	v2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/release/common"
	v1release "helm.sh/helm/v4/pkg/release/v1"
)

func TestMapRelease(t *testing.T) {
	tests := []struct {
		name     string
		release  release.Releaser
		expected map[string]string
	}{
		{
			name:     "nil release",
			release:  nil,
			expected: map[string]string{},
		},
		{
			name: "full release with metadata and values",
			release: &v1release.Release{
				Name:      "gpu-operator",
				Namespace: "gpu-operator",
				Version:   3,
				Info: &v1release.Info{
					Status: common.StatusDeployed,
				},
				Chart: &v2.Chart{
					Metadata: &v2.Metadata{
						Name:       "gpu-operator",
						Version:    "25.3.0",
						AppVersion: "25.3.0",
					},
				},
				Config: map[string]any{
					"driver": map[string]any{
						"enabled": true,
						"version": "570.86.16",
					},
					"replicas": float64(1),
				},
			},
			expected: map[string]string{
				"gpu-operator.namespace":             "gpu-operator",
				"gpu-operator.revision":              "3",
				"gpu-operator.status":                "deployed",
				"gpu-operator.chart":                 "gpu-operator",
				"gpu-operator.version":               "25.3.0",
				"gpu-operator.appVersion":            "25.3.0",
				"gpu-operator.values.driver.enabled": "true",
				"gpu-operator.values.driver.version": "570.86.16",
				"gpu-operator.values.replicas":       "1",
			},
		},
		{
			name: "release with nil chart",
			release: &v1release.Release{
				Name:      "my-release",
				Namespace: "default",
				Version:   1,
				Info: &v1release.Info{
					Status: common.StatusDeployed,
				},
			},
			expected: map[string]string{
				"my-release.namespace": "default",
				"my-release.revision":  "1",
				"my-release.status":    "deployed",
			},
		},
		{
			name: "release with nil chart metadata",
			release: &v1release.Release{
				Name:      "my-release",
				Namespace: "default",
				Version:   1,
				Info: &v1release.Info{
					Status: common.StatusDeployed,
				},
				Chart: &v2.Chart{},
			},
			expected: map[string]string{
				"my-release.namespace": "default",
				"my-release.revision":  "1",
				"my-release.status":    "deployed",
			},
		},
		{
			name: "release with nil info",
			release: &v1release.Release{
				Name:      "my-release",
				Namespace: "default",
				Version:   1,
			},
			expected: map[string]string{
				"my-release.namespace": "default",
				"my-release.revision":  "1",
			},
		},
		{
			name: "release with empty config",
			release: &v1release.Release{
				Name:      "my-release",
				Namespace: "default",
				Version:   1,
				Info: &v1release.Info{
					Status: common.StatusDeployed,
				},
				Config: map[string]any{},
			},
			expected: map[string]string{
				"my-release.namespace": "default",
				"my-release.revision":  "1",
				"my-release.status":    "deployed",
			},
		},
		{
			name: "release with deeply nested values",
			release: &v1release.Release{
				Name:      "network-operator",
				Namespace: "network-operator",
				Version:   2,
				Info: &v1release.Info{
					Status: common.StatusDeployed,
				},
				Chart: &v2.Chart{
					Metadata: &v2.Metadata{
						Name:    "network-operator",
						Version: "24.7.0",
					},
				},
				Config: map[string]any{
					"operator": map[string]any{
						"resources": map[string]any{
							"limits": map[string]any{
								"cpu":    "500m",
								"memory": "256Mi",
							},
						},
					},
				},
			},
			expected: map[string]string{
				"network-operator.namespace":                               "network-operator",
				"network-operator.revision":                                "2",
				"network-operator.status":                                  "deployed",
				"network-operator.chart":                                   "network-operator",
				"network-operator.version":                                 "24.7.0",
				"network-operator.values.operator.resources.limits.cpu":    "500m",
				"network-operator.values.operator.resources.limits.memory": "256Mi",
			},
		},
		{
			name: "release with array values",
			release: &v1release.Release{
				Name:      "prometheus",
				Namespace: "monitoring",
				Version:   1,
				Info: &v1release.Info{
					Status: common.StatusDeployed,
				},
				Chart: &v2.Chart{
					Metadata: &v2.Metadata{
						Name:    "prometheus",
						Version: "2.0.0",
					},
				},
				Config: map[string]any{
					"tolerations": []any{
						map[string]any{
							"key":      "nvidia.com/gpu",
							"operator": "Exists",
							"effect":   "NoSchedule",
						},
					},
				},
			},
			expected: map[string]string{
				"prometheus.namespace":          "monitoring",
				"prometheus.revision":           "1",
				"prometheus.status":             "deployed",
				"prometheus.chart":              "prometheus",
				"prometheus.version":            "2.0.0",
				"prometheus.values.tolerations": `[{"effect":"NoSchedule","key":"nvidia.com/gpu","operator":"Exists"}]`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make(map[string]measurement.Reading)
			mapRelease(tt.release, data)

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

func TestLatestReleases(t *testing.T) {
	tests := []struct {
		name     string
		releases []release.Releaser
		wantLen  int
		wantName string
		wantVer  int
	}{
		{
			name:     "nil releases",
			releases: nil,
			wantLen:  0,
		},
		{
			name:     "empty releases",
			releases: []release.Releaser{},
			wantLen:  0,
		},
		{
			name: "single release",
			releases: []release.Releaser{
				&v1release.Release{Name: "gpu-operator", Namespace: "gpu-operator", Version: 1},
			},
			wantLen:  1,
			wantName: "gpu-operator",
			wantVer:  1,
		},
		{
			name: "deduplicates same release different versions",
			releases: []release.Releaser{
				&v1release.Release{Name: "gpu-operator", Namespace: "gpu-operator", Version: 1},
				&v1release.Release{Name: "gpu-operator", Namespace: "gpu-operator", Version: 3},
				&v1release.Release{Name: "gpu-operator", Namespace: "gpu-operator", Version: 2},
			},
			wantLen:  1,
			wantName: "gpu-operator",
			wantVer:  3,
		},
		{
			name: "same name different namespaces kept separate",
			releases: []release.Releaser{
				&v1release.Release{Name: "app", Namespace: "staging", Version: 1},
				&v1release.Release{Name: "app", Namespace: "production", Version: 1},
			},
			wantLen: 2,
		},
		{
			name: "multiple distinct releases",
			releases: []release.Releaser{
				&v1release.Release{Name: "gpu-operator", Namespace: "gpu-operator", Version: 5},
				&v1release.Release{Name: "network-operator", Namespace: "network-operator", Version: 2},
				&v1release.Release{Name: "prometheus", Namespace: "monitoring", Version: 1},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := latestReleases(tt.releases)

			if len(result) != tt.wantLen {
				t.Fatalf("got %d releases, want %d", len(result), tt.wantLen)
			}

			if tt.wantName != "" && tt.wantLen == 1 {
				r, ok := result[0].(*v1release.Release)
				if !ok {
					t.Fatal("expected *v1release.Release type")
				}
				if r.Name != tt.wantName {
					t.Errorf("got name %q, want %q", r.Name, tt.wantName)
				}
				if r.Version != tt.wantVer {
					t.Errorf("got version %d, want %d", r.Version, tt.wantVer)
				}
			}
		})
	}
}

// readingKeys returns the keys of a readings map for debug output.
func readingKeys(data map[string]measurement.Reading) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	return keys
}

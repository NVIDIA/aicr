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

package constraints

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestParseConstraintPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		wantType    measurement.Type
		wantSubtype string
		wantKey     string
		expectError bool
	}{
		// Valid paths
		{
			name:        "k8s server version",
			path:        "K8s.server.version",
			wantType:    measurement.TypeK8s,
			wantSubtype: "server",
			wantKey:     "version",
		},
		{
			name:        "os release id",
			path:        "OS.release.ID",
			wantType:    measurement.TypeOS,
			wantSubtype: "release",
			wantKey:     "ID",
		},
		{
			name:        "os release version",
			path:        "OS.release.VERSION_ID",
			wantType:    measurement.TypeOS,
			wantSubtype: "release",
			wantKey:     "VERSION_ID",
		},
		{
			name:        "os sysctl kernel osrelease",
			path:        "OS.sysctl./proc/sys/kernel/osrelease",
			wantType:    measurement.TypeOS,
			wantSubtype: "sysctl",
			wantKey:     "/proc/sys/kernel/osrelease",
		},
		{
			name:        "gpu info type",
			path:        "GPU.info.type",
			wantType:    measurement.TypeGPU,
			wantSubtype: "info",
			wantKey:     "type",
		},
		{
			name:        "systemd containerd service",
			path:        "SystemD.containerd.service.ActiveState",
			wantType:    measurement.TypeSystemD,
			wantSubtype: "containerd",
			wantKey:     "service.ActiveState",
		},

		// Error cases
		{name: "empty path", path: "", expectError: true},
		{name: "single part", path: "K8s", expectError: true},
		{name: "two parts", path: "K8s.server", expectError: true},
		{name: "invalid type", path: "InvalidType.subtype.key", expectError: true},
		// Note: Type matching is case-sensitive
		{name: "lowercase k8s", path: "k8s.server.version", expectError: true},
		{name: "lowercase os", path: "os.release.ID", expectError: true},
		{name: "lowercase gpu", path: "gpu.info.type", expectError: true},
		{name: "lowercase systemd", path: "systemd.containerd.service.ActiveState", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ParseConstraintPath(tt.path)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Type != tt.wantType {
				t.Errorf("Type = %v, want %v", result.Type, tt.wantType)
			}
			if result.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", result.Subtype, tt.wantSubtype)
			}
			if result.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", result.Key, tt.wantKey)
			}
		})
	}
}

func TestConstraintPath_ExtractValue(t *testing.T) {
	t.Parallel()

	// Create a test snapshot with sample measurements
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5-eks-3025e55"),
						},
					},
					{
						Name: "images",
						Data: map[string]measurement.Reading{
							"count": measurement.Str("42"),
						},
					},
				},
			},
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID":         measurement.Str("ubuntu"),
							"VERSION_ID": measurement.Str("24.04"),
						},
					},
					{
						Name: "sysctl",
						Data: map[string]measurement.Reading{
							"/proc/sys/kernel/osrelease": measurement.Str("6.8.0-1028-aws"),
						},
					},
				},
			},
			{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{
					{
						Name: "info",
						Data: map[string]measurement.Reading{
							"type":   measurement.Str("H100"),
							"driver": measurement.Str("550.107.02"),
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name        string
		path        ConstraintPath
		want        string
		expectError bool
	}{
		// Valid extractions
		{
			name: "k8s server version",
			path: ConstraintPath{
				Type:    measurement.TypeK8s,
				Subtype: "server",
				Key:     "version",
			},
			want: "v1.33.5-eks-3025e55",
		},
		{
			name: "os release id",
			path: ConstraintPath{
				Type:    measurement.TypeOS,
				Subtype: "release",
				Key:     "ID",
			},
			want: "ubuntu",
		},
		{
			name: "os release version",
			path: ConstraintPath{
				Type:    measurement.TypeOS,
				Subtype: "release",
				Key:     "VERSION_ID",
			},
			want: "24.04",
		},
		{
			name: "kernel version",
			path: ConstraintPath{
				Type:    measurement.TypeOS,
				Subtype: "sysctl",
				Key:     "/proc/sys/kernel/osrelease",
			},
			want: "6.8.0-1028-aws",
		},
		{
			name: "gpu type",
			path: ConstraintPath{
				Type:    measurement.TypeGPU,
				Subtype: "info",
				Key:     "type",
			},
			want: "H100",
		},

		// Error cases - not found
		{
			name: "measurement type not found",
			path: ConstraintPath{
				Type:    measurement.TypeSystemD,
				Subtype: "containerd.service",
				Key:     "ActiveState",
			},
			expectError: true,
		},
		{
			name: "subtype not found",
			path: ConstraintPath{
				Type:    measurement.TypeK8s,
				Subtype: "nonexistent",
				Key:     "version",
			},
			expectError: true,
		},
		{
			name: "key not found",
			path: ConstraintPath{
				Type:    measurement.TypeK8s,
				Subtype: "server",
				Key:     "nonexistent",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := tt.path.ExtractValue(snapshot)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("ExtractValue() = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestParseConstraintPath_ItemSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		wantType     measurement.Type
		wantSubtype  string
		wantKey      string
		wantSelector string // empty means no selector expected
		wantIndex    *int   // non-nil when index selector expected
		wantPredKey  string // non-empty when predicate expected
		wantPredVal  string
		expectError  bool
	}{
		{
			name:         "index selector",
			path:         "NetworkTopology.pfs[0].rail",
			wantType:     measurement.TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "rail",
			wantSelector: "0",
			wantIndex:    intPtr(0),
		},
		{
			name:         "index selector larger",
			path:         "NetworkTopology.pfs[7].pciAddress",
			wantType:     measurement.TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "pciAddress",
			wantSelector: "7",
			wantIndex:    intPtr(7),
		},
		{
			name:         "predicate selector numeric value",
			path:         "NetworkTopology.pfs[rail=3].pciAddress",
			wantType:     measurement.TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "pciAddress",
			wantSelector: "rail=3",
			wantPredKey:  "rail",
			wantPredVal:  "3",
		},
		{
			name:         "predicate selector string value",
			path:         "NetworkTopology.pfs[traffic=east-west].rdmaDevice",
			wantType:     measurement.TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "rdmaDevice",
			wantSelector: "traffic=east-west",
			wantPredKey:  "traffic",
			wantPredVal:  "east-west",
		},
		{
			name:         "selector with dotted key after",
			path:         "NetworkTopology.pfs[0]./some/dotted/key",
			wantType:     measurement.TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "/some/dotted/key",
			wantSelector: "0",
			wantIndex:    intPtr(0),
		},

		// Error cases
		{name: "unclosed bracket", path: "NetworkTopology.pfs[0.rail", expectError: true},
		{name: "missing dot after selector", path: "NetworkTopology.pfs[0]rail", expectError: true},
		{name: "missing key after selector", path: "NetworkTopology.pfs[0]", expectError: true},
		{name: "missing key after selector with dot", path: "NetworkTopology.pfs[0].", expectError: true},
		{name: "empty selector", path: "NetworkTopology.pfs[].rail", expectError: true},
		{name: "empty subtype before bracket", path: "NetworkTopology.[0].rail", expectError: true},
		{name: "negative index", path: "NetworkTopology.pfs[-1].rail", expectError: true},
		{name: "non-integer non-predicate", path: "NetworkTopology.pfs[notnumber].rail", expectError: true},
		{name: "predicate empty key", path: "NetworkTopology.pfs[=3].rail", expectError: true},
		{name: "predicate empty value", path: "NetworkTopology.pfs[rail=].pciAddress", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ParseConstraintPath(tt.path)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil; result=%+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Type != tt.wantType {
				t.Errorf("Type = %v, want %v", result.Type, tt.wantType)
			}
			if result.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", result.Subtype, tt.wantSubtype)
			}
			if result.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", result.Key, tt.wantKey)
			}
			if result.Selector == nil {
				t.Fatalf("Selector is nil; want %q", tt.wantSelector)
			}
			if result.Selector.Raw != tt.wantSelector {
				t.Errorf("Selector.Raw = %q, want %q", result.Selector.Raw, tt.wantSelector)
			}
			if tt.wantIndex != nil {
				if result.Selector.Index == nil {
					t.Errorf("Selector.Index = nil, want %d", *tt.wantIndex)
				} else if *result.Selector.Index != *tt.wantIndex {
					t.Errorf("Selector.Index = %d, want %d", *result.Selector.Index, *tt.wantIndex)
				}
				if result.Selector.Predicate != nil {
					t.Errorf("Selector.Predicate = %+v, want nil", result.Selector.Predicate)
				}
			}
			if tt.wantPredKey != "" {
				if result.Selector.Predicate == nil {
					t.Errorf("Selector.Predicate = nil, want key=%s value=%s", tt.wantPredKey, tt.wantPredVal)
				} else {
					if result.Selector.Predicate.Key != tt.wantPredKey {
						t.Errorf("Selector.Predicate.Key = %q, want %q", result.Selector.Predicate.Key, tt.wantPredKey)
					}
					if result.Selector.Predicate.Value != tt.wantPredVal {
						t.Errorf("Selector.Predicate.Value = %q, want %q", result.Selector.Predicate.Value, tt.wantPredVal)
					}
				}
				if result.Selector.Index != nil {
					t.Errorf("Selector.Index = %d, want nil", *result.Selector.Index)
				}
			}
		})
	}
}

func intPtr(i int) *int { return &i }

func TestConstraintPath_ExtractValue_ItemSelector(t *testing.T) {
	t.Parallel()

	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeNetworkTopology,
				Subtypes: []measurement.Subtype{
					{
						Name: "pfs",
						Items: []measurement.ItemEntry{
							{
								Context: map[string]string{
									"pciAddress": "0000:03:00.0",
									"rdmaDevice": "mlx5_0",
								},
								Data: map[string]measurement.Reading{
									"rail":    measurement.Int(0),
									"traffic": measurement.Str("east-west"),
								},
							},
							{
								Context: map[string]string{
									"pciAddress": "0000:03:00.1",
									"rdmaDevice": "mlx5_1",
								},
								Data: map[string]measurement.Reading{
									"rail":    measurement.Int(1),
									"traffic": measurement.Str("east-west"),
								},
							},
							{
								Context: map[string]string{
									"pciAddress": "0000:03:00.2",
									"rdmaDevice": "mlx5_2",
								},
								Data: map[string]measurement.Reading{
									"rail":    measurement.Int(2),
									"traffic": measurement.Str("east-west"),
								},
							},
						},
					},
					{
						Name: "capabilities",
						Data: map[string]measurement.Reading{
							"sriov": measurement.Bool(true),
						},
					},
				},
			},
		},
	}

	mustParse := func(t *testing.T, path string) *ConstraintPath {
		t.Helper()
		p, err := ParseConstraintPath(path)
		if err != nil {
			t.Fatalf("ParseConstraintPath(%q) error = %v", path, err)
		}
		return p
	}

	tests := []struct {
		name        string
		path        string
		want        string
		expectError bool
	}{
		// Index selectors
		{name: "index 0 data field", path: "NetworkTopology.pfs[0].rail", want: "0"},
		{name: "index 1 data field", path: "NetworkTopology.pfs[1].rail", want: "1"},
		{name: "index 0 context field", path: "NetworkTopology.pfs[0].pciAddress", want: "0000:03:00.0"},
		{name: "index 2 rdmaDevice", path: "NetworkTopology.pfs[2].rdmaDevice", want: "mlx5_2"},
		// Predicate selectors
		{name: "predicate by data field", path: "NetworkTopology.pfs[rail=1].pciAddress", want: "0000:03:00.1"},
		{name: "predicate by context field", path: "NetworkTopology.pfs[pciAddress=0000:03:00.2].rail", want: "2"},
		// Backward compat: non-item path still works
		{name: "non-item path data", path: "NetworkTopology.capabilities.sriov", want: "true"},

		// Errors
		{name: "index out of bounds", path: "NetworkTopology.pfs[99].rail", expectError: true},
		{name: "predicate no match", path: "NetworkTopology.pfs[rail=99].pciAddress", expectError: true},
		{name: "key not in item", path: "NetworkTopology.pfs[0].nonexistent", expectError: true},
		{name: "selector on subtype with no items", path: "NetworkTopology.capabilities[0].sriov", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := mustParse(t, tt.path)
			got, err := path.ExtractValue(snap)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil; result=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ExtractValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConstraintPath_ExtractValue_PredicateAmbiguous(t *testing.T) {
	t.Parallel()

	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeNetworkTopology,
				Subtypes: []measurement.Subtype{
					{
						Name: "pfs",
						Items: []measurement.ItemEntry{
							{Data: map[string]measurement.Reading{"traffic": measurement.Str("east-west"), "rail": measurement.Int(0)}},
							{Data: map[string]measurement.Reading{"traffic": measurement.Str("east-west"), "rail": measurement.Int(1)}},
						},
					},
				},
			},
		},
	}

	path, err := ParseConstraintPath("NetworkTopology.pfs[traffic=east-west].rail")
	if err != nil {
		t.Fatalf("ParseConstraintPath() error = %v", err)
	}
	_, err = path.ExtractValue(snap)
	if err == nil {
		t.Fatal("expected ambiguous-match error, got nil")
	}
}

func TestConstraintPath_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path ConstraintPath
		want string
	}{
		{
			name: "simple path",
			path: ConstraintPath{
				Type:    measurement.TypeK8s,
				Subtype: "server",
				Key:     "version",
			},
			want: "K8s.server.version",
		},
		{
			name: "path with special chars",
			path: ConstraintPath{
				Type:    measurement.TypeOS,
				Subtype: "sysctl",
				Key:     "/proc/sys/kernel/osrelease",
			},
			want: "OS.sysctl./proc/sys/kernel/osrelease",
		},
		{
			name: "index selector",
			path: ConstraintPath{
				Type:     measurement.TypeNetworkTopology,
				Subtype:  "pfs",
				Key:      "rail",
				Selector: &itemSelector{Raw: "0", Index: intPtr(0)},
			},
			want: "NetworkTopology.pfs[0].rail",
		},
		{
			name: "predicate selector",
			path: ConstraintPath{
				Type:     measurement.TypeNetworkTopology,
				Subtype:  "pfs",
				Key:      "pciAddress",
				Selector: &itemSelector{Raw: "rail=3", Predicate: &itemPredicate{Key: "rail", Value: "3"}},
			},
			want: "NetworkTopology.pfs[rail=3].pciAddress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := tt.path.String()
			if result != tt.want {
				t.Errorf("String() = %q, want %q", result, tt.want)
			}
		})
	}
}

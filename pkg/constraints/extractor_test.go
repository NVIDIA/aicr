// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

func TestConstraintPath_String(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.path.String()
			if result != tt.want {
				t.Errorf("String() = %q, want %q", result, tt.want)
			}
		})
	}
}

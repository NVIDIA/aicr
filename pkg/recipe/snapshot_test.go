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

package recipe

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestExtractCriteriaFromSnapshot(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *snapshotter.Snapshot
		validate func(*testing.T, *Criteria)
	}{
		{
			name:     "nil snapshot",
			snapshot: nil,
			validate: func(t *testing.T, c *Criteria) {
				if c == nil {
					t.Error("expected non-nil criteria")
				}
			},
		},
		{
			name: "empty snapshot",
			snapshot: &snapshotter.Snapshot{
				Measurements: nil,
			},
			validate: func(t *testing.T, c *Criteria) {
				if c == nil {
					t.Error("expected non-nil criteria")
				}
			},
		},
		{
			name: "nil measurement skipped",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{nil},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c == nil {
					t.Error("expected non-nil criteria")
				}
			},
		},
		{
			name: "K8s service from service field",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"service": measurement.Str("eks"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceEKS {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceEKS)
				}
			},
		},
		{
			name: "K8s service from version string EKS",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("v1.28.2-eks-a0123b4"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceEKS {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceEKS)
				}
			},
		},
		{
			name: "K8s service from version string GKE",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("v1.28.3-gke.1234"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceGKE {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceGKE)
				}
			},
		},
		{
			name: "K8s service from version string AKS",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("v1.28.3-aks-1234"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceAKS {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceAKS)
				}
			},
		},
		{
			name: "K8s service from version string LKE",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("v1.31.9+lke7"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceLKE {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceLKE)
				}
			},
		},
		{
			name: "GPU RTX PRO 6000 from model field",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA RTX PRO 6000 Blackwell"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorRTXPro6000 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorRTXPro6000)
				}
			},
		},
		{
			name: "GPU H100 from model field",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA H100 80GB HBM3"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorH100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorH100)
				}
			},
		},
		{
			name: "GPU GB200",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA GB200"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorGB200 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorGB200)
				}
			},
		},
		{
			name: "GPU B200",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA-B200"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorB200 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorB200)
				}
			},
		},
		{
			name: "GPU A100",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("A100-SXM4-80GB"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorA100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorA100)
				}
			},
		},
		{
			name: "GPU L40 from gpu.model field",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "smi",
								Data: map[string]measurement.Reading{
									"gpu.model": measurement.Str("NVIDIA L40S"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorL40 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorL40)
				}
			},
		},
		{
			name: "OS ubuntu",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeOS,
						Subtypes: []measurement.Subtype{
							{
								Name: "release",
								Data: map[string]measurement.Reading{
									"ID": measurement.Str("ubuntu"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.OS != CriteriaOSUbuntu {
					t.Errorf("OS = %v, want %v", c.OS, CriteriaOSUbuntu)
				}
			},
		},
		{
			name: "complete snapshot",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"service": measurement.Str("gke"),
								},
							},
						},
					},
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("A100-SXM4-80GB"),
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
									"ID": measurement.Str("rhel"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceGKE {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceGKE)
				}
				if c.Accelerator != CriteriaAcceleratorA100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, CriteriaAcceleratorA100)
				}
				if c.OS != CriteriaOSRHEL {
					t.Errorf("OS = %v, want %v", c.OS, CriteriaOSRHEL)
				}
			},
		},
		{
			name: "unknown GPU preserves default accelerator",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA T4"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Accelerator != CriteriaAcceleratorAny {
					t.Errorf("Accelerator = %v, want %v (default preserved)", c.Accelerator, CriteriaAcceleratorAny)
				}
			},
		},
		{
			name: "systemd measurement skipped",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeSystemD,
						Subtypes: []measurement.Subtype{
							{
								Name: "containerd",
								Data: map[string]measurement.Reading{
									"state": measurement.Str("running"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *Criteria) {
				if c.Service != CriteriaServiceAny {
					t.Errorf("Service = %v, want %v", c.Service, CriteriaServiceAny)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria := ExtractCriteriaFromSnapshot(tt.snapshot)
			if tt.validate != nil {
				tt.validate(t, criteria)
			}
		})
	}
}

func TestMatchAccelerator(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  CriteriaAcceleratorType
	}{
		{"H100 uppercase", "NVIDIA H100 80GB HBM3", CriteriaAcceleratorH100},
		{"H100 lowercase", "h100-sxm", CriteriaAcceleratorH100},
		{"A100", "A100-SXM4-80GB", CriteriaAcceleratorA100},
		{"GB200", "NVIDIA GB200", CriteriaAcceleratorGB200},
		{"B200", "NVIDIA-B200", CriteriaAcceleratorB200},
		{"L40", "NVIDIA L40S", CriteriaAcceleratorL40},
		{"RTX PRO 6000", "RTX PRO 6000", CriteriaAcceleratorRTXPro6000},
		{"RTX PRO 6000 Blackwell", "NVIDIA RTX PRO 6000 Blackwell", CriteriaAcceleratorRTXPro6000},
		{"unknown model", "NVIDIA T4", ""},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchAccelerator(tt.model)
			if got != tt.want {
				t.Errorf("matchAccelerator(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

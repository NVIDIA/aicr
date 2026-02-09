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

package readiness

import (
	"context"
	"testing"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func TestCheckGPUHardwareDetection(t *testing.T) {
	tests := []struct {
		name        string
		snapshot    *snapshotter.Snapshot
		wantErr     bool
		errContains string
	}{
		{
			name: "valid GPU measurement with data",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "nvidia-smi",
								Data: map[string]measurement.Reading{
									"driver_version": measurement.Str("560.35.03"),
									"cuda_version":   measurement.Str("12.6"),
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "multiple measurements including GPU",
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
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "gpu-info",
								Data: map[string]measurement.Reading{
									"count": measurement.Int(8),
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "multiple GPU subtypes",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "nvidia-smi",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("560.35.03"),
								},
							},
							{
								Name: "topology",
								Data: map[string]measurement.Reading{
									"nvlink": measurement.Bool(true),
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:        "nil snapshot",
			snapshot:    nil,
			wantErr:     true,
			errContains: "snapshot is nil",
		},
		{
			name: "no GPU measurement",
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
			wantErr:     true,
			errContains: "no GPU measurement found",
		},
		{
			name: "GPU measurement with no subtypes",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type:     measurement.TypeGPU,
						Subtypes: []measurement.Subtype{},
					},
				},
			},
			wantErr:     true,
			errContains: "has no subtypes",
		},
		{
			name: "GPU measurement with empty subtype data",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "nvidia-smi",
								Data: map[string]measurement.Reading{},
							},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "has no data",
		},
		{
			name: "GPU measurement with nil subtypes",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type:     measurement.TypeGPU,
						Subtypes: nil,
					},
				},
			},
			wantErr:     true,
			errContains: "has no subtypes",
		},
		{
			name: "empty measurements list",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{},
			},
			wantErr:     true,
			errContains: "no GPU measurement found",
		},
		{
			name: "mixed subtypes - some with data, some without",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "empty",
								Data: map[string]measurement.Reading{},
							},
							{
								Name: "with-data",
								Data: map[string]measurement.Reading{
									"gpu_count": measurement.Int(8),
								},
							},
						},
					},
				},
			},
			wantErr: false, // At least one subtype has data
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &checks.ValidationContext{
				Context:  context.Background(),
				Snapshot: tt.snapshot,
			}

			err := CheckGPUHardwareDetection(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckGPUHardwareDetection() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !contains(err.Error(), tt.errContains) {
					t.Errorf("CheckGPUHardwareDetection() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckGPUHardwareDetectionRegistration(t *testing.T) {
	// Verify the check is registered
	check, ok := checks.GetCheck("gpu-hardware-detection")
	if !ok {
		t.Fatal("gpu-hardware-detection check not registered")
	}

	if check.Name != "gpu-hardware-detection" {
		t.Errorf("Name = %v, want gpu-hardware-detection", check.Name)
	}

	if check.Phase != "readiness" {
		t.Errorf("Phase = %v, want readiness", check.Phase)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

func TestCheckGPUHardwareDetectionWithDifferentMeasurementTypes(t *testing.T) {
	// Test that only GPU measurement type is checked
	measurementTypes := []measurement.Type{
		measurement.TypeOS,
		measurement.TypeK8s,
		// TypeGPU is tested separately
	}

	for _, mType := range measurementTypes {
		t.Run(string(mType), func(t *testing.T) {
			snapshot := &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: mType,
						Subtypes: []measurement.Subtype{
							{
								Name: "test",
								Data: map[string]measurement.Reading{
									"key": measurement.Str("value"),
								},
							},
						},
					},
				},
			}

			ctx := &checks.ValidationContext{
				Context:  context.Background(),
				Snapshot: snapshot,
			}

			err := CheckGPUHardwareDetection(ctx)
			if err == nil {
				t.Error("CheckGPUHardwareDetection() should fail without GPU measurement")
			}

			if !contains(err.Error(), "no GPU measurement found") {
				t.Errorf("CheckGPUHardwareDetection() error = %v, should contain 'no GPU measurement found'", err)
			}
		})
	}
}

// TestGPUHardwareDetection is the test wrapper for the gpu-hardware-detection check.
// This function is executed by go test inside Kubernetes validation Jobs.
// It loads the validation context from the Job environment and runs the registered check.
//
// This is distinct from TestCheckGPUHardwareDetection which is a unit test with mocked context.
//
// When run outside Kubernetes (e.g., during local development), this test is skipped.
func TestGPUHardwareDetection(t *testing.T) {
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		// Skip if not running in Kubernetes (expected during local test runs)
		t.Skipf("Skipping integration test (not in Kubernetes): %v", err)
		return
	}
	defer runner.Cancel() // Clean up context when test completes

	runner.RunCheck("gpu-hardware-detection")
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			len(s) > len(substr)+1 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

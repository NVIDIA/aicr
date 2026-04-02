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

package gpu

import (
	"context"
	"testing"
)

// mockHardwareDetector is a test double for the HardwareDetector interface.
type mockHardwareDetector struct {
	info *HardwareInfo
	err  error
}

func (m *mockHardwareDetector) Detect(_ context.Context) (*HardwareInfo, error) {
	return m.info, m.err
}

func TestHardwareDetectorInterface(t *testing.T) {
	tests := []struct {
		name        string
		detector    HardwareDetector
		wantPresent bool
		wantCount   int
		wantDriver  bool
		wantErr     bool
	}{
		{
			name: "GPU present with driver",
			detector: &mockHardwareDetector{
				info: &HardwareInfo{
					GPUPresent:   true,
					GPUCount:     2,
					DriverLoaded: true,
				},
			},
			wantPresent: true,
			wantCount:   2,
			wantDriver:  true,
		},
		{
			name: "no GPU hardware",
			detector: &mockHardwareDetector{
				info: &HardwareInfo{
					GPUPresent:   false,
					GPUCount:     0,
					DriverLoaded: false,
				},
			},
			wantPresent: false,
			wantCount:   0,
			wantDriver:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := tt.detector.Detect(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Detect() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if info.GPUPresent != tt.wantPresent {
				t.Errorf("GPUPresent = %v, want %v", info.GPUPresent, tt.wantPresent)
			}
			if info.GPUCount != tt.wantCount {
				t.Errorf("GPUCount = %v, want %v", info.GPUCount, tt.wantCount)
			}
			if info.DriverLoaded != tt.wantDriver {
				t.Errorf("DriverLoaded = %v, want %v", info.DriverLoaded, tt.wantDriver)
			}
		})
	}
}

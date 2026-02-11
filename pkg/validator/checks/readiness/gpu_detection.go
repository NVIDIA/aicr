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
	"fmt"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "gpu-hardware-detection",
		Description: "Verify GPU hardware is detected and accessible via nvidia-smi",
		Phase:       "readiness",
		Func:        CheckGPUHardwareDetection,
		TestName:    "TestGPUHardwareDetection",
	})
}

// CheckGPUHardwareDetection validates that GPU hardware is properly detected.
func CheckGPUHardwareDetection(ctx *checks.ValidationContext) error {
	if ctx.Snapshot == nil {
		return fmt.Errorf("snapshot is nil")
	}

	// Find GPU measurement in snapshot
	var gpuMeasurement *measurement.Measurement
	for _, m := range ctx.Snapshot.Measurements {
		if m.Type == measurement.TypeGPU {
			gpuMeasurement = m
			break
		}
	}

	if gpuMeasurement == nil {
		return fmt.Errorf("no GPU measurement found in snapshot")
	}

	// Validate that GPU measurement has subtypes with data
	if len(gpuMeasurement.Subtypes) == 0 {
		return fmt.Errorf("GPU measurement has no subtypes")
	}

	// Check that at least one subtype has GPU data
	hasGPUData := false
	for _, subtype := range gpuMeasurement.Subtypes {
		if len(subtype.Data) > 0 {
			hasGPUData = true
			break
		}
	}

	if !hasGPUData {
		return fmt.Errorf("GPU measurement has no data in any subtype")
	}

	return nil
}

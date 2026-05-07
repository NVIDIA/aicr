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

package gpu

import "context"

// HardwareDetector abstracts GPU hardware detection for testability.
// Implementations enumerate PCI devices and kernel module state without
// requiring GPU drivers to be installed.
type HardwareDetector interface {
	// Detect discovers GPU hardware and driver module state.
	// Returns HardwareInfo describing what was found, or an error if
	// detection could not be performed (e.g., sysfs not available).
	Detect(ctx context.Context) (*HardwareInfo, error)
}

// HardwareInfo describes the GPU hardware state detected without drivers.
type HardwareInfo struct {
	// GPUPresent is true if at least one NVIDIA GPU was found via PCI enumeration.
	GPUPresent bool

	// GPUCount is the number of NVIDIA GPUs detected via PCI enumeration.
	GPUCount int

	// DriverLoaded is true if the nvidia kernel module is currently loaded.
	DriverLoaded bool

	// DetectionSource identifies which detection method produced this result
	// (e.g., "nfd", "sysfs").
	DetectionSource string
}

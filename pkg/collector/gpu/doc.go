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

// Package gpu collects GPU hardware data via driver-free NFD/PCI enumeration.
//
// # Detection
//
// The collector uses a single, driver-free detector backed by NFD source
// packages: it enumerates PCI devices from sysfs, counts NVIDIA GPUs by
// vendor/class, resolves the accelerator SKU from each device ID (see
// device_ids.go), and checks the nvidia kernel-module state. It requires
// neither the NVIDIA driver nor nvidia-smi — only Linux with sysfs mounted —
// which is what makes day-0 detection on freshly provisioned nodes possible.
//
// (Historically a second "smi" phase shelled out to nvidia-smi for the SKU and
// extra telemetry; it was removed once the PCI device ID could name the SKU,
// eliminating the CUDA-image dependency. The accelerator SKU now also comes
// from the GPU-operator nvidia.com/gpu.product label during fingerprinting.)
//
// # Graceful Degradation
//
// Detection degrades to an empty result rather than an error:
//
//   - Detector failure (e.g., no sysfs on macOS): logged as a warning; the GPU
//     measurement is returned with no subtypes.
//   - No HardwareDetector configured: the GPU measurement is returned with no
//     subtypes.
//
// # Measurement Structure
//
//	Measurement{
//	    Type: "GPU",
//	    Subtypes: [
//	        {Name: "hardware", Data: {gpu-present, gpu-count, driver-loaded, detection-source, model}},
//	    ],
//	}
//
// The "hardware" subtype keys are defined in pkg/measurement:
//   - KeyGPUPresent: bool — true if at least one NVIDIA GPU found via PCI
//   - KeyGPUCount: int — number of NVIDIA GPUs detected
//   - KeyGPUDriverLoaded: bool — true if nvidia kernel module is loaded
//   - KeyGPUDetectionSource: string — detection method (e.g., "nfd")
//   - KeyGPUModel: string — accelerator SKU resolved from the PCI device ID
//     (omitted when the device ID is unknown). This is a descriptive discovery
//     vocabulary broader than the recipe accelerator enum.
//
// # Usage
//
// The collector is created by the factory with NFD wiring:
//
//	collector := gpu.NewCollector(
//	    gpu.WithHardwareDetector(&gpu.NFDHardwareDetector{}),
//	)
//	m, err := collector.Collect(ctx)
//
// Without WithHardwareDetector, Collect returns a GPU measurement with no
// subtypes.
//
// # Context and Timeouts
//
// The collector respects context cancellation and applies a bounded timeout
// (defaults.CollectorTimeout). NFD detection has its own sub-timeout
// (defaults.NFDDetectionTimeout).
package gpu

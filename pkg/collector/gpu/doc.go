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

// Package gpu collects GPU hardware and driver configuration data using a
// two-phase detection model.
//
// # Two-Phase Collection
//
// The collector runs two independent detection phases, each producing a
// separate measurement subtype:
//
//	Phase 1 ("hardware"): NFD-based PCI enumeration — detects NVIDIA GPUs
//	    via sysfs PCI device scan and checks nvidia kernel module state.
//	    No GPU drivers required. Requires Linux with sysfs mounted.
//
//	Phase 2 ("smi"): nvidia-smi XML query — collects driver version, CUDA
//	    version, per-GPU hardware specs, and runtime settings. Requires
//	    nvidia-smi in PATH with a loaded NVIDIA driver.
//
// Phase 1 enables day-0 GPU detection on freshly provisioned nodes where
// drivers have not yet been installed. Phase 2 provides the full telemetry
// used for recipe generation and validation.
//
// # Graceful Degradation
//
// Each phase degrades independently:
//
//   - Phase 1 failure (e.g., no sysfs on macOS): logged as warning, skipped.
//     Only the "smi" subtype is returned.
//   - Phase 2 failure (e.g., nvidia-smi not installed): logged as warning.
//     A zero-GPU "smi" subtype is returned with gpu-count=0.
//   - Both phases fail: measurement contains only the zero-GPU "smi" subtype.
//   - Phase 1 nil (no HardwareDetector configured): Phase 1 is skipped entirely,
//     preserving the pre-NFD single-phase behavior.
//
// # Measurement Structure
//
// A successful two-phase collection produces:
//
//	Measurement{
//	    Type: "GPU",
//	    Subtypes: [
//	        {Name: "hardware", Data: {gpu-present, gpu-count, driver-loaded, detection-source}},
//	        {Name: "smi",      Data: {gpu-count, driver, cuda-version, gpu.model, ...}},
//	    ],
//	}
//
// The "hardware" subtype keys are defined in pkg/measurement:
//   - KeyGPUPresent: bool — true if at least one NVIDIA GPU found via PCI
//   - KeyGPUCount: int — number of NVIDIA GPUs detected
//   - KeyGPUDriverLoaded: bool — true if nvidia kernel module is loaded
//   - KeyGPUDetectionSource: string — detection method (e.g., "nfd")
//
// The "smi" subtype contains driver telemetry and per-GPU hardware details.
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
// Without WithHardwareDetector, Phase 1 is skipped (backward compatible).
//
// # Context and Timeouts
//
// The collector respects context cancellation and applies a bounded timeout
// (defaults.CollectorTimeout). NFD detection has its own sub-timeout
// (defaults.NFDDetectionTimeout = 5s). The context is passed to each phase,
// so cancellation is respected within each phase's I/O operations.
//
// # Platform Support
//
//   - Linux with sysfs: Both phases run (full two-phase detection)
//   - macOS / containers without /sys: Phase 1 fails gracefully, Phase 2 only
//   - No nvidia-smi: Phase 2 returns zero-GPU subtype
package gpu

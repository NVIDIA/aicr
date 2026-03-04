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

// Package gpu collects GPU hardware and driver configuration data.
//
// This collector gathers comprehensive GPU information from NVIDIA GPUs using
// nvidia-smi, including hardware specifications, driver details, and GPU-specific
// runtime settings.
//
// # Collected Data
//
// The collector returns a measurement with multiple subtypes, one per GPU:
//
// GPU Hardware:
//   - model: GPU model name (H100, A100, L40, etc.)
//   - uuid: GPU UUID for unique identification
//   - architecture: GPU architecture (Hopper, Ampere, Ada, etc.)
//   - computeCapability: CUDA compute capability (9.0, 8.0, etc.)
//   - memory: Total GPU memory in MB
//   - bandwidth: Memory bandwidth in GB/s
//
// Driver Information:
//   - driverVersion: NVIDIA driver version (570.158.01, etc.)
//   - cudaVersion: Maximum supported CUDA version
//   - vbios: GPU firmware/VBIOS version
//
// Runtime Settings:
//   - persistenceMode: Whether persistence mode is enabled
//   - computeMode: Compute mode (Default, Exclusive, Prohibited)
//   - migMode: MIG mode (Enabled, Disabled) for supported GPUs
//   - addressingMode: GPU addressing mode
//   - powerLimit: Current power limit in watts
//   - powerState: Current power state (P0-P12)
//
// # Usage
//
// Create and use the collector:
//
//	collector := gpu.NewCollector()
//	measurements, err := collector.Collect(ctx)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	for _, m := range measurements {
//	    for _, subtype := range m.Subtypes {
//	        fmt.Printf("GPU %s: %s\n", subtype.Name, subtype.Data["model"])
//	    }
//	}
//
// # nvidia-smi Dependency
//
// The collector requires nvidia-smi to be installed and in the system PATH:
//
//	which nvidia-smi
//	# Output: /usr/bin/nvidia-smi
//
// nvidia-smi must be executable and properly configured to communicate with
// the NVIDIA driver.
//
// # Query Format
//
// The collector uses nvidia-smi's query mode for reliable, machine-readable output:
//
//	nvidia-smi --query-gpu=name,uuid,driver_version,... --format=csv,noheader
//
// This provides consistent output across driver versions and GPU models.
//
// # Multi-GPU Support
//
// The collector automatically detects and collects data from all available GPUs:
//
//	measurements, _ := collector.Collect(ctx)
//	for i, subtype := range measurements[0].Subtypes {
//	    fmt.Printf("GPU %d: %s\n", i, subtype.Data["model"])
//	}
//
// Each GPU becomes a separate subtype named by its index (0, 1, 2, etc.).
//
// # Context Support
//
// The collector respects context cancellation and timeouts:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//
//	measurements, err := collector.Collect(ctx)
//
// nvidia-smi execution is bounded by the context deadline.
//
// # Error Handling
//
// Common error scenarios:
//   - nvidia-smi not found: Returns error with installation instructions
//   - No GPUs detected: Returns error
//   - Driver not loaded: Returns error with troubleshooting guidance
//   - nvidia-smi timeout: Returns context deadline exceeded error
//
// The collector does not continue on errors since GPU data is critical for
// GPU-accelerated cluster configuration.
//
// # MIG Support
//
// For GPUs with MIG enabled, the collector:
//   - Reports MIG mode status
//   - Includes MIG-related settings
//   - Works with both MIG-enabled and disabled GPUs
//
// MIG instances are not individually queried (that requires additional nvidia-smi flags).
//
// # Containerized Collection
//
// When running in containers, ensure:
//   - NVIDIA Container Toolkit is installed
//   - Container has GPU access (--gpus all or device requests)
//   - nvidia-smi is available in the container
//
// For Kubernetes pods:
//
//	spec:
//	  containers:
//	  - name: collector
//	    image: nvidia/cuda:12.7-base
//	    resources:
//	      limits:
//	        nvidia.com/gpu: "1"
//
// # Use in Recipes
//
// Recipe generation uses GPU data for:
//   - GPU-specific driver version recommendations
//   - Model-specific optimizations (H100 vs A100)
//   - MIG configuration recommendations
//   - Power and thermal management settings
//   - Memory-based workload sizing
package gpu

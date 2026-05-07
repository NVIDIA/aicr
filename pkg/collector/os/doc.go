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

// Package os collects operating system configuration data.
//
// This collector gathers detailed OS-level configuration that affects GPU
// performance and Kubernetes operation. It captures kernel parameters,
// loaded modules, GRUB boot settings, and OS release information.
//
// # Collected Data
//
// The collector returns a measurement with 4 subtypes:
//
// 1. grub - Boot loader configuration:
//   - intel_iommu, amd_iommu: IOMMU settings for device passthrough
//   - iommu: IOMMU mode (pt, off, etc.)
//   - hugepages: Huge pages configuration
//   - numa_balancing: NUMA balancing settings
//   - security settings (selinux, apparmor)
//
// 2. sysctl - Kernel runtime parameters:
//   - All parameters from /proc/sys tree
//   - Network settings (tcp, udp, ip)
//   - Memory management (vm)
//   - Filesystem settings (fs)
//   - Kernel core settings
//
// 3. kmod - Loaded kernel modules:
//   - Module names
//   - Module parameters
//   - Dependency information
//
// 4. release - OS identification:
//   - ID: OS identifier (ubuntu, rhel, etc.)
//   - VERSION_ID: OS version (24.04, 9, etc.)
//   - PRETTY_NAME: Human-readable name
//   - VERSION_CODENAME: Release codename
//
// # Usage
//
// Create and use the collector:
//
//	collector := os.NewCollector()
//	measurements, err := collector.Collect(ctx)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	for _, m := range measurements {
//	    fmt.Printf("Type: %s\n", m.Type)
//	    for _, subtype := range m.Subtypes {
//	        fmt.Printf("  %s: %d settings\n", subtype.Name, len(subtype.Data))
//	    }
//	}
//
// # Data Sources
//
// The collector reads from:
//   - /etc/default/grub: GRUB configuration
//   - /proc/cmdline: Kernel boot parameters (overrides GRUB)
//   - /proc/sys: Runtime kernel parameters (recursively)
//   - /proc/modules: Loaded kernel modules
//   - /etc/os-release: Operating system identification
//
// # Context Support
//
// The collector respects context cancellation for graceful shutdown:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	measurements, err := collector.Collect(ctx)
//
// # Error Handling
//
// The collector continues on non-critical errors (missing files, permission
// denied) and returns partial results. Only systemic failures return errors.
//
// Example scenarios:
//   - Missing /etc/default/grub: Skips GRUB settings, continues
//   - Permission denied on /proc/sys: Returns available parameters
//   - Missing /etc/os-release: Returns error (critical file)
//
// # Performance
//
// The collector performs file I/O operations synchronously. Typical execution
// time is < 100ms on modern systems. For optimal performance, use appropriate
// context timeouts.
//
// # Use in Recipes
//
// Recipe generation uses OS collector data for:
//   - Kernel parameter validation and tuning
//   - Module dependency verification
//   - OS version-specific optimizations
//   - Boot parameter recommendations
package os

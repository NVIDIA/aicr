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

// Package systemd collects systemd service configuration data.
//
// This collector gathers configuration and runtime status for critical system
// services that affect GPU and Kubernetes operation, such as containerd,
// docker, and kubelet.
//
// # Collected Data
//
// For each configured service, the collector captures:
//   - Service state (active, inactive, failed)
//   - Startup configuration (enabled, disabled)
//   - Resource limits (CPU, Memory, Tasks)
//   - Execution settings (User, Group, WorkingDirectory)
//   - Dependencies (Wants, Requires, After, Before)
//   - Security settings (ProtectSystem, PrivateTmp, NoNewPrivileges)
//
// # Usage
//
// Create with specific services to monitor:
//
//	collector := systemd.NewCollector([]string{
//	    "containerd.service",
//	    "kubelet.service",
//	    "docker.service",
//	})
//
//	measurements, err := collector.Collect(ctx)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	for _, m := range measurements {
//	    for _, subtype := range m.Subtypes {
//	        fmt.Printf("Service: %s\n", subtype.Name)
//	        if state, ok := subtype.Data["ActiveState"]; ok {
//	            fmt.Printf("  State: %s\n", state)
//	        }
//	    }
//	}
//
// # Service Configuration
//
// Services to monitor are specified during collector creation:
//
//	services := []string{
//	    "containerd.service",
//	    "kubelet.service",
//	}
//	collector := systemd.NewCollector(services)
//
// Common services for GPU clusters:
//   - containerd.service: Container runtime
//   - docker.service: Docker daemon (alternative runtime)
//   - kubelet.service: Kubernetes node agent
//   - nvidia-dcgm.service: NVIDIA DCGM monitoring
//   - nvidia-persistenced.service: GPU persistence daemon
//
// # Data Format
//
// Each service becomes a subtype with configuration key-value pairs:
//
//	{
//	    Type: "systemd",
//	    Subtypes: [
//	        {
//	            Name: "containerd.service",
//	            Data: {
//	                "ActiveState": "active",
//	                "UnitFileState": "enabled",
//	                "CPUAccounting": "yes",
//	                "MemoryAccounting": "yes",
//	                "MemoryLimit": "infinity",
//	                ...
//	            }
//	        }
//	    ]
//	}
//
// # Context Support
//
// The collector respects context cancellation:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//
//	measurements, err := collector.Collect(ctx)
//
// # Error Handling
//
// The collector continues on non-critical errors:
//   - Service not found: Includes note in subtype data
//   - Service not loaded: Captures available properties
//   - systemctl not available: Returns error
//
// Critical errors (systemd not available) cause the entire collection to fail.
//
// # systemd Integration
//
// The collector uses `systemctl show` to query service properties:
//
//	systemctl show containerd.service --property=ActiveState --property=UnitFileState
//
// This provides reliable, machine-readable output that works across all
// systemd-based distributions.
//
// # Use in Recipes
//
// Recipe generation uses systemd data for:
//   - Service dependency verification
//   - Resource limit recommendations
//   - Security policy validation
//   - Startup configuration tuning
//   - Service state troubleshooting
package systemd

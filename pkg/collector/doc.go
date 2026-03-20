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

// Package collector provides interfaces and implementations for collecting system configuration data.
//
// # Overview
//
// This package defines a unified interface for gathering measurements from various system
// sources including Kubernetes clusters, GPU hardware, operating system configuration, and
// systemd services. Collectors run concurrently and return structured measurement data that
// can be serialized for analysis or recommendation generation.
//
// # Core Interface
//
// The Collector interface defines a single method for gathering data:
//
//	type Collector interface {
//	    Collect(ctx context.Context) (*measurement.Measurement, error)
//	}
//
// All collectors support context-based cancellation for graceful shutdown and timeout handling.
//
// # Factory Pattern
//
// The Factory interface enables dependency injection and testing by abstracting collector creation:
//
//	type Factory interface {
//	    CreateSystemDCollector() Collector
//	    CreateOSCollector() Collector
//	    CreateKubernetesCollector() Collector
//	    CreateGPUCollector() Collector
//	}
//
// The DefaultFactory provides production implementations with configurable options:
//
//	factory := collector.NewDefaultFactory(
//	    collector.WithSystemDServices([]string{"containerd.service", "kubelet.service"}),
//	)
//
// # Available Collectors
//
// Kubernetes (k8s): Collects cluster configuration including:
//   - Node information (provider, kernel, container runtime)
//   - Server version and platform details
//   - Deployed container images
//   - GPU Operator ClusterPolicy configuration
//
// GPU: Gathers GPU hardware and driver information including:
//   - GPU model, architecture, and compute capability
//   - Driver version, CUDA version, and firmware
//   - GPU-specific settings (MIG mode, persistence mode)
//
// Operating System (os): Captures OS-level configuration:
//   - GRUB boot parameters and kernel arguments
//   - Sysctl kernel parameters
//   - Loaded kernel modules
//   - OS release information
//
// SystemD: Monitors systemd service states:
//   - Service status and configuration
//   - Active state and startup settings
//   - Resource limits and dependencies
//
// # Usage Example
//
// Using the default factory:
//
//	factory := collector.NewDefaultFactory()
//	k8sCollector := factory.CreateKubernetesCollector()
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	measurement, err := k8sCollector.Collect(ctx)
//	if err != nil {
//	    log.Fatalf("collection failed: %v", err)
//	}
//
// Running multiple collectors in parallel:
//
//	factory := collector.NewDefaultFactory()
//	g, ctx := errgroup.WithContext(context.Background())
//	var measurements []*measurement.Measurement
//	var mu sync.Mutex
//
//	collectors := []struct {
//	    name string
//	    c    collector.Collector
//	}{
//	    {"k8s", factory.CreateKubernetesCollector()},
//	    {"gpu", factory.CreateGPUCollector()},
//	    {"os", factory.CreateOSCollector()},
//	    {"systemd", factory.CreateSystemDCollector()},
//	}
//
//	for _, col := range collectors {
//	    col := col
//	    g.Go(func() error {
//	        m, err := col.c.Collect(ctx)
//	        if err != nil {
//	            return fmt.Errorf("%s collection failed: %w", col.name, err)
//	        }
//	        mu.Lock()
//	        measurements = append(measurements, m)
//	        mu.Unlock()
//	        return nil
//	    })
//	}
//
//	if err := g.Wait(); err != nil {
//	    log.Fatalf("collection error: %v", err)
//	}
//
// # Subpackages
//
// The collector package is organized into subpackages by data source:
//   - collector/k8s - Kubernetes API collectors
//   - collector/gpu - GPU hardware collectors
//   - collector/os - Operating system collectors
//   - collector/systemd - SystemD service collectors
//   - collector/file - File-based configuration collectors
//
// # Error Handling
//
// Collectors return errors when:
//   - Required resources are unavailable (e.g., no Kubernetes cluster)
//   - Permissions are insufficient
//   - Context is canceled or times out
//   - Data parsing fails
//
// Callers should handle these errors appropriately based on their use case.
package collector

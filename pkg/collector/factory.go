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

package collector

import (
	"github.com/NVIDIA/aicr/pkg/collector/gpu"
	"github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/collector/os"
	"github.com/NVIDIA/aicr/pkg/collector/systemd"
	"github.com/NVIDIA/aicr/pkg/collector/talos"
	"github.com/NVIDIA/aicr/pkg/collector/topology"
	"github.com/NVIDIA/aicr/pkg/recipe/oskind"
)

// Factory defines the interface for creating collector instances.
// Implementations of Factory provide configured collectors for various system components.
// This interface enables dependency injection and facilitates testing by allowing mock collectors.
type Factory interface {
	CreateSystemDCollector() Collector
	CreateOSCollector() Collector
	CreateKubernetesCollector() Collector
	CreateGPUCollector() Collector
	CreateNodeTopologyCollector() Collector
}

// Option defines a configuration option for DefaultFactory.
type Option func(*DefaultFactory)

// WithSystemDServices configures the systemd services to monitor.
func WithSystemDServices(services []string) Option {
	return func(f *DefaultFactory) {
		f.SystemDServices = services
	}
}

// WithMaxNodesPerEntry configures the maximum number of node names stored per
// taint/label entry in the topology collector. 0 = no limit.
func WithMaxNodesPerEntry(max int) Option {
	return func(f *DefaultFactory) {
		f.MaxNodesPerEntry = max
	}
}

// WithOS configures the OS criteria value, used to select the appropriate
// collector backend (e.g., systemd vs. Talos for service collection).
// Empty string preserves the systemd default for backwards compatibility.
func WithOS(os string) Option {
	return func(f *DefaultFactory) {
		f.OS = os
	}
}

// DefaultFactory is the standard implementation of Factory that creates collectors
// with production dependencies. It configures default systemd services to monitor.
type DefaultFactory struct {
	SystemDServices  []string
	MaxNodesPerEntry int
	// OS routes service and OS collection to the OS-appropriate backend.
	// When set to oskind.Talos, CreateSystemDCollector and CreateOSCollector
	// return Talos-aware collectors that read state from the Kubernetes API.
	// Empty preserves the existing systemd D-Bus / /proc-based defaults.
	OS string

	// Lazily-initialized Talos collector pair sharing one config so a
	// single snapshot performs exactly one Node API fetch regardless of
	// how many Talos collectors run in parallel.
	talosService *talos.ServiceCollector
	talosOS      *talos.OSCollector
}

// NewDefaultFactory creates a new DefaultFactory with default configuration.
// By default, it monitors containerd, docker, and kubelet systemd services.
// Additional configuration can be provided via functional options.
func NewDefaultFactory(opts ...Option) *DefaultFactory {
	f := &DefaultFactory{
		SystemDServices: []string{
			"containerd.service",
			"docker.service",
			"kubelet.service",
		},
	}

	// Apply options
	for _, opt := range opts {
		opt(f)
	}

	return f
}

// CreateGPUCollector creates a GPU collector that gathers GPU hardware and driver information.
func (f *DefaultFactory) CreateGPUCollector() Collector {
	return gpu.NewCollector(
		gpu.WithHardwareDetector(&gpu.NFDHardwareDetector{}),
	)
}

// ensureTalosCollectors lazily constructs the Talos service+OS collector
// pair against a single shared config so they make exactly one Node API
// fetch even when the snapshotter runs them in parallel.
func (f *DefaultFactory) ensureTalosCollectors() {
	if f.talosService == nil {
		f.talosService, f.talosOS = talos.NewCollectors()
	}
}

// CreateSystemDCollector creates a service collector. The backend is selected
// from the OS criteria: os: talos routes to the Kubernetes-API-backed Talos
// service collector (which emits the same SystemD measurement type for
// schema compatibility); any other value uses the systemd D-Bus collector.
func (f *DefaultFactory) CreateSystemDCollector() Collector {
	if f.OS == oskind.Talos {
		f.ensureTalosCollectors()
		return f.talosService
	}
	return &systemd.Collector{
		Services: f.SystemDServices,
	}
}

// CreateOSCollector creates an OS collector. The backend is selected from
// the OS criteria: os: talos routes to a Kubernetes-API-backed collector
// that emits release + extensions subtypes derived from Node.Status.NodeInfo
// and `extensions.talos.dev/*` labels (Talos has no /etc/os-release on
// the host filesystem accessible to unprivileged pods); any other value
// uses the standard /proc-based collector.
func (f *DefaultFactory) CreateOSCollector() Collector {
	if f.OS == oskind.Talos {
		f.ensureTalosCollectors()
		return f.talosOS
	}
	return &os.Collector{}
}

// CreateKubernetesCollector creates a Kubernetes API collector.
func (f *DefaultFactory) CreateKubernetesCollector() Collector {
	return &k8s.Collector{}
}

// CreateNodeTopologyCollector creates a node topology collector that gathers
// taint and label information across all cluster nodes.
func (f *DefaultFactory) CreateNodeTopologyCollector() Collector {
	return &topology.Collector{
		MaxNodesPerEntry: f.MaxNodesPerEntry,
	}
}

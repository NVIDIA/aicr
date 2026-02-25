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

package collector

import (
	"github.com/NVIDIA/aicr/pkg/collector/gpu"
	"github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/collector/os"
	"github.com/NVIDIA/aicr/pkg/collector/systemd"
)

// Factory defines the interface for creating collector instances.
// Implementations of Factory provide configured collectors for various system components.
// This interface enables dependency injection and facilitates testing by allowing mock collectors.
type Factory interface {
	CreateSystemDCollector() Collector
	CreateOSCollector() Collector
	CreateKubernetesCollector() Collector
	CreateGPUCollector() Collector
}

// Option defines a configuration option for DefaultFactory.
type Option func(*DefaultFactory)

// WithSystemDServices configures the systemd services to monitor.
func WithSystemDServices(services []string) Option {
	return func(f *DefaultFactory) {
		f.SystemDServices = services
	}
}

// WithHelmNamespaces configures the namespaces for Helm release collection.
// nil/empty = skip, ["*"] = all namespaces, ["ns1","ns2"] = scoped.
func WithHelmNamespaces(namespaces []string) Option {
	return func(f *DefaultFactory) {
		f.HelmNamespaces = namespaces
	}
}

// DefaultFactory is the standard implementation of Factory that creates collectors
// with production dependencies. It configures default systemd services to monitor.
type DefaultFactory struct {
	SystemDServices []string
	HelmNamespaces  []string
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
	return &gpu.Collector{}
}

// CreateSystemDCollector creates a systemd collector that monitors the configured services.
func (f *DefaultFactory) CreateSystemDCollector() Collector {
	return &systemd.Collector{
		Services: f.SystemDServices,
	}
}

// CreateOSCollector creates an OS collector.
func (f *DefaultFactory) CreateOSCollector() Collector {
	return &os.Collector{}
}

// CreateKubernetesCollector creates a Kubernetes API collector.
func (f *DefaultFactory) CreateKubernetesCollector() Collector {
	return &k8s.Collector{
		HelmNamespaces: f.HelmNamespaces,
	}
}

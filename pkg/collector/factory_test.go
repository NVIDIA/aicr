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
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/collector/gpu"
	"github.com/NVIDIA/aicr/pkg/collector/systemd"
	"github.com/NVIDIA/aicr/pkg/collector/talos"
	"github.com/NVIDIA/aicr/pkg/recipe/oskind"
)

func TestDefaultCollectorFactory_CreateSystemDCollector(t *testing.T) {
	factory := NewDefaultFactory()
	factory.SystemDServices = []string{"test.service"}

	col := factory.CreateSystemDCollector()
	if col == nil {
		t.Fatal("Expected non-nil collector")
	}

	// Verify it's configured correctly
	systemdCollector, ok := col.(*systemd.Collector)
	if !ok {
		t.Fatal("Expected *systemd.SystemDCollector")
	}

	if len(systemdCollector.Services) != 1 || systemdCollector.Services[0] != "test.service" {
		t.Errorf("Expected [test.service], got %v", systemdCollector.Services)
	}
}

func TestDefaultCollectorFactory_CreateSystemDCollector_TalosBackend(t *testing.T) {
	factory := NewDefaultFactory(WithOS(oskind.Talos))

	col := factory.CreateSystemDCollector()
	if col == nil {
		t.Fatal("expected non-nil collector for OS=talos")
	}

	if _, ok := col.(*talos.ServiceCollector); !ok {
		t.Errorf("expected *talos.ServiceCollector for OS=talos, got %T", col)
	}
}

func TestDefaultCollectorFactory_CreateOSCollector_TalosBackend(t *testing.T) {
	factory := NewDefaultFactory(WithOS(oskind.Talos))

	col := factory.CreateOSCollector()
	if col == nil {
		t.Fatal("expected non-nil collector for OS=talos")
	}

	if _, ok := col.(*talos.OSCollector); !ok {
		t.Errorf("expected *talos.OSCollector for OS=talos, got %T", col)
	}
}

func TestDefaultCollectorFactory_CreateSystemDCollector_DefaultBackend(t *testing.T) {
	tests := []struct {
		name string
		os   string
	}{
		{"empty OS preserves systemd default", ""},
		{"ubuntu uses systemd", "ubuntu"},
		{"rhel uses systemd", "rhel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := NewDefaultFactory(WithOS(tt.os))
			col := factory.CreateSystemDCollector()
			if _, ok := col.(*systemd.Collector); !ok {
				t.Errorf("expected *systemd.Collector for OS=%q, got %T", tt.os, col)
			}
		})
	}
}

func TestWithOS(t *testing.T) {
	factory := NewDefaultFactory(WithOS("talos"))
	if factory.OS != "talos" {
		t.Errorf("expected OS=talos, got %q", factory.OS)
	}
}

func TestDefaultCollectorFactory_CreateOSCollector(t *testing.T) {
	factory := NewDefaultFactory()

	collector := factory.CreateOSCollector()
	if collector == nil {
		t.Fatal("Expected non-nil collector")
	}

	ctx := context.TODO()
	_, err := collector.Collect(ctx)
	if err != nil {
		t.Logf("Collect returned error (acceptable): %v", err)
	}
}

func TestDefaultCollectorFactory_AllCollectors(t *testing.T) {
	factory := NewDefaultFactory()

	collectorFuncs := []func() Collector{
		factory.CreateSystemDCollector,
		factory.CreateOSCollector,
		factory.CreateGPUCollector,
		factory.CreateKubernetesCollector,
		factory.CreateNodeTopologyCollector,
	}

	for i, createFunc := range collectorFuncs {
		collector := createFunc()
		if collector == nil {
			t.Errorf("Collector %d returned nil", i)
		}
	}
}

func TestWithSystemDServices(t *testing.T) {
	services := []string{"custom1.service", "custom2.service"}
	factory := NewDefaultFactory(WithSystemDServices(services))

	if len(factory.SystemDServices) != 2 {
		t.Errorf("expected 2 services, got %d", len(factory.SystemDServices))
	}

	if factory.SystemDServices[0] != "custom1.service" {
		t.Errorf("expected custom1.service, got %s", factory.SystemDServices[0])
	}
}

func TestDefaultCollectorFactory_CreateGPUCollector_ReturnsConfiguredCollector(t *testing.T) {
	factory := NewDefaultFactory()
	col := factory.CreateGPUCollector()
	if col == nil {
		t.Fatal("expected non-nil GPU collector")
	}

	// Verify the factory returns a *gpu.Collector (not a generic mock or wrapper).
	// The actual NFDHardwareDetector wiring is an internal detail tested by
	// the gpu package's two-phase tests.
	if _, ok := col.(*gpu.Collector); !ok {
		t.Errorf("expected *gpu.Collector, got %T", col)
	}
}

func TestNewDefaultFactory_Defaults(t *testing.T) {
	factory := NewDefaultFactory()

	// Check default services
	expectedServices := []string{"containerd.service", "docker.service", "kubelet.service"}
	if len(factory.SystemDServices) != len(expectedServices) {
		t.Errorf("expected %d services, got %d", len(expectedServices), len(factory.SystemDServices))
	}

	for i, svc := range expectedServices {
		if factory.SystemDServices[i] != svc {
			t.Errorf("expected service %s at index %d, got %s", svc, i, factory.SystemDServices[i])
		}
	}
}

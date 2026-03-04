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
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/collector/systemd"
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

func TestWithHelmNamespaces(t *testing.T) {
	tests := []struct {
		name       string
		namespaces []string
		wantLen    int
	}{
		{
			name:       "nil namespaces",
			namespaces: nil,
			wantLen:    0,
		},
		{
			name:       "all namespaces",
			namespaces: []string{"*"},
			wantLen:    1,
		},
		{
			name:       "scoped namespaces",
			namespaces: []string{"gpu-operator", "network-operator"},
			wantLen:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := NewDefaultFactory(WithHelmNamespaces(tt.namespaces))

			if len(factory.HelmNamespaces) != tt.wantLen {
				t.Errorf("expected %d namespaces, got %d", tt.wantLen, len(factory.HelmNamespaces))
			}

			// Verify K8s collector gets the namespaces
			col := factory.CreateKubernetesCollector()
			k8sCol, ok := col.(*k8s.Collector)
			if !ok {
				t.Fatal("expected *k8s.Collector")
			}
			if len(k8sCol.HelmNamespaces) != tt.wantLen {
				t.Errorf("K8s collector expected %d namespaces, got %d", tt.wantLen, len(k8sCol.HelmNamespaces))
			}
		})
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

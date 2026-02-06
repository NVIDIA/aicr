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

package snapshotter

import (
	"context"
	"fmt"
	"testing"

	"github.com/NVIDIA/eidos/pkg/collector"
	"github.com/NVIDIA/eidos/pkg/header"
	"github.com/NVIDIA/eidos/pkg/measurement"
)

func TestNewSnapshot(t *testing.T) {
	snap := NewSnapshot()

	if snap == nil {
		t.Fatal("NewSnapshot() returned nil")
		return
	}

	if snap.Measurements == nil {
		t.Error("Measurements should be initialized")
	}

	if len(snap.Measurements) != 0 {
		t.Errorf("Measurements length = %d, want 0", len(snap.Measurements))
	}
}

func TestNodeSnapshotter_Measure(t *testing.T) {
	t.Run("with nil factory uses default", func(t *testing.T) {
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    nil, // Will use default
			Serializer: &mockSerializer{},
		}

		ctx := context.Background()
		err := snapshotter.Measure(ctx)

		// This will fail because default factory requires actual system resources
		// But we verify that Factory is set
		if snapshotter.Factory == nil {
			t.Error("Factory should be set to default when nil")
		}

		// Error is expected since we don't have real collectors
		if err == nil {
			t.Log("Measure succeeded (unexpected in test environment)")
		}
	})

	t.Run("with mock factory", func(t *testing.T) {
		factory := &mockFactory{}
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    factory,
			Serializer: &mockSerializer{},
		}

		ctx := context.Background()
		err := snapshotter.Measure(ctx)

		if err != nil {
			t.Errorf("Measure() error = %v, want nil", err)
		}

		if !factory.k8sCalled {
			t.Error("Kubernetes collector not called")
		}

		if !factory.systemdCalled {
			t.Error("SystemD collector not called")
		}

		if !factory.osCalled {
			t.Error("OS collector not called")
		}
	})

	t.Run("handles collector errors", func(t *testing.T) {
		factory := &mockFactory{
			k8sError: fmt.Errorf("k8s error"),
		}
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    factory,
			Serializer: &mockSerializer{},
		}

		ctx := context.Background()
		err := snapshotter.Measure(ctx)

		if err == nil {
			t.Error("Measure() should return error when collector fails")
		}
	})
}

func TestSnapshot_Init(t *testing.T) {
	snap := NewSnapshot()
	snap.Init(header.KindSnapshot, FullAPIVersion, "1.0.0")

	if snap.Kind != header.KindSnapshot {
		t.Errorf("Kind = %s, want %s", snap.Kind, header.KindSnapshot)
	}

	if snap.Metadata == nil {
		t.Error("Metadata should be initialized")
	}
}

// Mock implementations for testing

type mockSerializer struct {
	serialized bool
	data       any
}

func (m *mockSerializer) Serialize(ctx context.Context, data any) error {
	m.serialized = true
	m.data = data
	return nil
}

type mockFactory struct {
	k8sCalled     bool
	systemdCalled bool
	osCalled      bool
	gpuCalled     bool

	k8sError     error
	systemdError error
	osError      error
	gpuError     error
}

func (m *mockFactory) CreateKubernetesCollector() collector.Collector {
	m.k8sCalled = true
	return &mockCollector{err: m.k8sError}
}

func (m *mockFactory) CreateSystemDCollector() collector.Collector {
	m.systemdCalled = true
	return &mockCollector{err: m.systemdError}
}

func (m *mockFactory) CreateOSCollector() collector.Collector {
	m.osCalled = true
	return &mockCollector{err: m.osError}
}

func (m *mockFactory) CreateGPUCollector() collector.Collector {
	m.gpuCalled = true
	return &mockCollector{err: m.gpuError}
}

type mockCollector struct {
	err error
}

func (m *mockCollector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &measurement.Measurement{
		Type:     measurement.TypeK8s,
		Subtypes: []measurement.Subtype{},
	}, nil
}

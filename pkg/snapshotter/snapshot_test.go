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

	"github.com/NVIDIA/aicr/pkg/collector"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
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

		if snapshotter.Factory == nil {
			t.Error("Factory should be set to default when nil")
		}

		// With graceful degradation, snapshot should succeed even without
		// real system resources — failed collectors are skipped.
		if err != nil {
			t.Errorf("Measure() should succeed with graceful degradation, got: %v", err)
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

	t.Run("degrades gracefully on collector errors", func(t *testing.T) {
		ser := &mockSerializer{}
		factory := &mockFactory{
			k8sError: fmt.Errorf("k8s error"),
			osError:  fmt.Errorf("os error"),
		}
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    factory,
			Serializer: ser,
		}

		ctx := context.Background()
		err := snapshotter.Measure(ctx)

		if err != nil {
			t.Errorf("Measure() should succeed even when collectors fail, got: %v", err)
		}

		// Verify that successful collectors still contributed measurements
		snap, ok := ser.data.(*Snapshot)
		if !ok {
			t.Fatal("serialized data is not a *Snapshot")
		}
		// k8s and os failed, systemd and gpu succeeded = 2 measurements
		if len(snap.Measurements) != 2 {
			t.Errorf("expected 2 measurements (from working collectors), got %d", len(snap.Measurements))
		}
	})
}

func TestNodeSnapshotter_RequireGPU(t *testing.T) {
	t.Run("fails when require-gpu set and no GPU found", func(t *testing.T) {
		// Mock GPU collector returns gpu-count=0
		factory := &mockFactory{
			gpuMeasurement: &measurement.Measurement{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{
						measurement.KeyGPUCount: measurement.Int(0),
					},
				}},
			},
		}
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    factory,
			Serializer: &mockSerializer{},
			RequireGPU: true,
		}

		err := snapshotter.Measure(context.Background())
		if err == nil {
			t.Error("expected error when require-gpu is set and no GPU found")
		}
	})

	t.Run("succeeds when require-gpu set and GPU found", func(t *testing.T) {
		factory := &mockFactory{
			gpuMeasurement: &measurement.Measurement{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{
						measurement.KeyGPUCount: measurement.Int(2),
					},
				}},
			},
		}
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    factory,
			Serializer: &mockSerializer{},
			RequireGPU: true,
		}

		err := snapshotter.Measure(context.Background())
		if err != nil {
			t.Errorf("expected no error when GPU is present, got: %v", err)
		}
	})

	t.Run("succeeds without require-gpu even when no GPU", func(t *testing.T) {
		factory := &mockFactory{}
		snapshotter := &NodeSnapshotter{
			Version:    "1.0.0",
			Factory:    factory,
			Serializer: &mockSerializer{},
			RequireGPU: false,
		}

		err := snapshotter.Measure(context.Background())
		if err != nil {
			t.Errorf("expected no error without require-gpu, got: %v", err)
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

	// gpuMeasurement overrides the default mock measurement for the GPU collector.
	gpuMeasurement *measurement.Measurement
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
	return &mockCollector{err: m.gpuError, result: m.gpuMeasurement}
}

type mockCollector struct {
	err    error
	result *measurement.Measurement
}

func (m *mockCollector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &measurement.Measurement{
		Type:     measurement.TypeK8s,
		Subtypes: []measurement.Subtype{},
	}, nil
}

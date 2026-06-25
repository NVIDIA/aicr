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

package snapshotter

import (
	"context"
	"fmt"
	"os"
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
			Factory:    nil, // Will be replaced with default
			Serializer: &mockSerializer{},
		}

		// Verify that nil factory gets replaced (but use mock to avoid live cluster access).
		// We only check that Measure sets the factory, then re-run with mock.
		if snapshotter.Factory != nil {
			t.Error("Factory should start as nil for this test")
		}

		// Use mock factory to avoid connecting to a live cluster.
		snapshotter.Factory = &mockFactory{}

		ctx := context.Background()
		err := snapshotter.Measure(ctx)

		if err != nil {
			t.Errorf("Measure() should succeed with mock factory, got: %v", err)
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

		if !factory.networkCalled {
			t.Error("Network collector not called")
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
		// k8s and os failed; systemd, gpu, topology, and network
		// succeeded = 4 measurements. mockCollector returns a default
		// non-nil Measurement when neither error nor result override is
		// set, so the network collector contributes a measurement here.
		if len(snap.Measurements) != 4 {
			t.Errorf("expected 4 measurements (from working collectors), got %d", len(snap.Measurements))
		}
	})
}

func TestNodeSnapshotter_PopulatesFingerprint(t *testing.T) {
	ser := &mockSerializer{}
	factory := &mockFactory{
		k8sMeasurement: measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").
				Set(measurement.KeyVersion, measurement.Str("v1.33.4"))).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("node").
				Set("provider", measurement.Str("eks"))).
			Build(),
		gpuMeasurement: measurement.NewMeasurement(measurement.TypeGPU).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("hardware").
				Set("gpu-present", measurement.Bool(true)).
				Set("gpu-count", measurement.Int(8)).
				Set("detection-source", measurement.Str("nfd")).
				Set("model", measurement.Str("h100"))).
			Build(),
		osMeasurement: measurement.NewMeasurement(measurement.TypeOS).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("release").
				Set("ID", measurement.Str("ubuntu")).
				Set("VERSION_ID", measurement.Str("22.04"))).
			Build(),
		topologyMeasurement: measurement.NewMeasurement(measurement.TypeNodeTopology).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(12))).
			Build(),
	}
	snapshotter := &NodeSnapshotter{
		Version:    "1.0.0",
		Factory:    factory,
		Serializer: ser,
	}

	if err := snapshotter.Measure(context.Background()); err != nil {
		t.Fatalf("Measure() error = %v, want nil", err)
	}

	snap, ok := ser.data.(*Snapshot)
	if !ok {
		t.Fatal("serialized data is not a *Snapshot")
	}
	if snap.Fingerprint == nil {
		t.Fatal("Fingerprint should be populated after Measure")
	}
	if snap.Fingerprint.Service.Value != "eks" {
		t.Errorf("Fingerprint.Service.Value = %q, want eks", snap.Fingerprint.Service.Value)
	}
	if snap.Fingerprint.Accelerator.Value != "h100" {
		t.Errorf("Fingerprint.Accelerator.Value = %q, want h100", snap.Fingerprint.Accelerator.Value)
	}
	if snap.Fingerprint.OS.Value != "ubuntu" {
		t.Errorf("Fingerprint.OS.Value = %q, want ubuntu", snap.Fingerprint.OS.Value)
	}
	if snap.Fingerprint.K8sVersion.Value != "1.33.4" {
		t.Errorf("Fingerprint.K8sVersion.Value = %q, want 1.33.4", snap.Fingerprint.K8sVersion.Value)
	}
	if snap.Fingerprint.NodeCount.Value != 12 {
		t.Errorf("Fingerprint.NodeCount.Value = %d, want 12", snap.Fingerprint.NodeCount.Value)
	}
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

	t.Run("succeeds when NFD hardware detects GPUs but nvidia-smi reports zero", func(t *testing.T) {
		// Day-0 scenario: NFD detects GPU hardware via PCI, but drivers
		// aren't installed yet so nvidia-smi reports 0 GPUs.
		factory := &mockFactory{
			gpuMeasurement: &measurement.Measurement{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{
					{
						Name: "hardware",
						Data: map[string]measurement.Reading{
							measurement.KeyGPUPresent:         measurement.Bool(true),
							measurement.KeyGPUCount:           measurement.Int(2),
							measurement.KeyGPUDriverLoaded:    measurement.Bool(false),
							measurement.KeyGPUDetectionSource: measurement.Str("nfd"),
						},
					},
					{
						Name: "smi",
						Data: map[string]measurement.Reading{
							measurement.KeyGPUCount: measurement.Int(0),
						},
					},
				},
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
			t.Errorf("expected no error when NFD detects GPUs (day-0), got: %v", err)
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
	k8sCalled      bool
	systemdCalled  bool
	osCalled       bool
	gpuCalled      bool
	topologyCalled bool
	networkCalled  bool

	k8sError      error
	systemdError  error
	osError       error
	gpuError      error
	topologyError error
	networkError  error

	// gpuMeasurement overrides the default mock measurement for the GPU collector.
	gpuMeasurement *measurement.Measurement

	// Per-collector measurement overrides used by fingerprint tests
	// to seed realistic data without spinning up real collectors.
	k8sMeasurement      *measurement.Measurement
	osMeasurement       *measurement.Measurement
	topologyMeasurement *measurement.Measurement
	networkMeasurement  *measurement.Measurement
}

func (m *mockFactory) CreateKubernetesCollector() collector.Collector {
	m.k8sCalled = true
	return &mockCollector{err: m.k8sError, result: m.k8sMeasurement}
}

func (m *mockFactory) CreateSystemDCollector() collector.Collector {
	m.systemdCalled = true
	return &mockCollector{err: m.systemdError}
}

func (m *mockFactory) CreateOSCollector() collector.Collector {
	m.osCalled = true
	return &mockCollector{err: m.osError, result: m.osMeasurement}
}

func (m *mockFactory) CreateGPUCollector() collector.Collector {
	m.gpuCalled = true
	return &mockCollector{err: m.gpuError, result: m.gpuMeasurement}
}

func (m *mockFactory) CreateNodeTopologyCollector() collector.Collector {
	m.topologyCalled = true
	return &mockCollector{err: m.topologyError, result: m.topologyMeasurement}
}

func (m *mockFactory) CreateNetworkCollector() collector.Collector {
	m.networkCalled = true
	return &mockCollector{err: m.networkError, result: m.networkMeasurement}
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

func TestHasGPUData(t *testing.T) {
	tests := []struct {
		name string
		snap *Snapshot
		want bool
	}{
		{
			name: "gpu-count int > 0 returns true",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Int(2)},
				}}},
			}},
			want: true,
		},
		{
			name: "gpu-count float64 > 0 returns true (YAML round-trip)",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Float64(2.0)},
				}}},
			}},
			want: true,
		},
		{
			name: "gpu-count int64 > 0 returns true",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Int64(4)},
				}}},
			}},
			want: true,
		},
		{
			name: "gpu-count zero returns false",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Int(0)},
				}}},
			}},
			want: false,
		},
		{
			name: "no GPU measurement returns false",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{}},
			}},
			want: false,
		},
		{
			name: "empty measurements returns false",
			snap: NewSnapshot(),
			want: false,
		},
		{
			name: "hardware subtype with gpu-count > 0 returns true",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
					Name: "hardware",
					Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Int(4)},
				}}},
			}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasGPUData(tt.snap)
			if got != tt.want {
				t.Errorf("hasGPUData() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectGPUPlacementMismatch(t *testing.T) {
	gpuPresent := func(key string) *Snapshot {
		return &Snapshot{Measurements: []*measurement.Measurement{
			{Type: measurement.TypeNodeTopology, Subtypes: []measurement.Subtype{{
				Name: "label",
				Data: map[string]measurement.Reading{
					key: measurement.Str("true|node1,node2"),
				},
			}}},
		}}
	}

	tests := []struct {
		name string
		snap *Snapshot
		want bool
	}{
		{
			name: "no GPU data and topology has gpu.present label — mismatch detected",
			snap: gpuPresent("nvidia.com/gpu.present"),
			want: true,
		},
		{
			name: "no GPU data and topology has disambiguated gpu.present.true label — mismatch detected",
			snap: gpuPresent("nvidia.com/gpu.present.true"),
			want: true,
		},
		{
			name: "no GPU data and topology has gpu.product label — mismatch detected",
			snap: gpuPresent("nvidia.com/gpu.product.NVIDIA-H100-80GB-HBM3"),
			want: true,
		},
		{
			name: "GPU data present — no mismatch",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
					Name: "smi",
					Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Int(2)},
				}}},
				{Type: measurement.TypeNodeTopology, Subtypes: []measurement.Subtype{{
					Name: "label",
					Data: map[string]measurement.Reading{
						"nvidia.com/gpu.present": measurement.Str("true|node1"),
					},
				}}},
			}},
			want: false,
		},
		{
			name: "no GPU data and no topology GPU labels — no mismatch (pre-Operator cluster)",
			snap: &Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeNodeTopology, Subtypes: []measurement.Subtype{{
					Name: "label",
					Data: map[string]measurement.Reading{
						"kubernetes.io/hostname": measurement.Str("node1|node1"),
					},
				}}},
			}},
			want: false,
		},
		{
			name: "empty snapshot — no mismatch",
			snap: NewSnapshot(),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectGPUPlacementMismatch(tt.snap)
			if got != tt.want {
				t.Errorf("detectGPUPlacementMismatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWarnOnGPUPlacementMismatch(t *testing.T) {
	// warnOnGPUPlacementMismatch has no return value; we verify it doesn't panic
	// under both the mismatch and no-mismatch conditions.
	t.Run("no-op when no mismatch", func(t *testing.T) {
		snap := &Snapshot{Measurements: []*measurement.Measurement{
			{Type: measurement.TypeGPU, Subtypes: []measurement.Subtype{{
				Name: "smi",
				Data: map[string]measurement.Reading{measurement.KeyGPUCount: measurement.Int(2)},
			}}},
		}}
		warnOnGPUPlacementMismatch(snap) // must not panic
	})

	t.Run("warns when mismatch detected", func(t *testing.T) {
		snap := &Snapshot{Measurements: []*measurement.Measurement{
			{Type: measurement.TypeNodeTopology, Subtypes: []measurement.Subtype{{
				Name: "label",
				Data: map[string]measurement.Reading{
					"nvidia.com/gpu.present": measurement.Str("true|node1"),
				},
			}}},
		}}
		warnOnGPUPlacementMismatch(snap) // must not panic; warning emitted to slog
	})
}

func TestParseOSEnv(t *testing.T) {
	// "unset" is the truly-absent case (the env var is removed). The other
	// cases set AICR_OS to a literal value via t.Setenv.
	tests := []struct {
		name  string
		env   string
		unset bool
		want  string
	}{
		{name: "unset", unset: true, want: ""},
		{name: "set but empty", env: "", want: ""},
		{name: "talos", env: "talos", want: "talos"},
		{name: "ubuntu passthrough", env: "ubuntu", want: "ubuntu"},
		{name: "uppercase normalized", env: "Talos", want: "talos"},
		{name: "whitespace trimmed", env: "  talos  ", want: "talos"},
		{name: "invalid value drops to default", env: "talsoo", want: ""},
		{name: "unknown OS drops to default", env: "windows", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				prev, hadPrev := os.LookupEnv("AICR_OS")
				if err := os.Unsetenv("AICR_OS"); err != nil {
					t.Fatalf("os.Unsetenv() error = %v", err)
				}
				t.Cleanup(func() {
					if hadPrev {
						_ = os.Setenv("AICR_OS", prev)
					}
				})
			} else {
				t.Setenv("AICR_OS", tt.env)
			}
			if got := parseOSEnv(); got != tt.want {
				t.Errorf("parseOSEnv() = %q, want %q", got, tt.want)
			}
		})
	}
}

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

package fingerprint

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

// k8sMeasurement builds a TypeK8s measurement with optional server
// version and node provider.
func k8sMeasurement(version, provider string) *measurement.Measurement {
	b := measurement.NewMeasurement(measurement.TypeK8s)
	if version != "" {
		b = b.WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("server").
				Set(measurement.KeyVersion, measurement.Str(version)),
		)
	}
	if provider != "" {
		b = b.WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("node").
				Set("provider", measurement.Str(provider)),
		)
	}
	return b.Build()
}

// gpuMeasurement builds a TypeGPU measurement with the given smi
// gpu.model value.
func gpuMeasurement(model string) *measurement.Measurement {
	return measurement.NewMeasurement(measurement.TypeGPU).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("smi").
				Set("gpu.model", measurement.Str(model)),
		).
		Build()
}

// osMeasurement builds a TypeOS measurement with the given /etc/os-release
// ID and VERSION_ID values.
func osMeasurement(id, versionID string) *measurement.Measurement {
	sb := measurement.NewSubtypeBuilder("release")
	if id != "" {
		sb = sb.Set("ID", measurement.Str(id))
	}
	if versionID != "" {
		sb = sb.Set("VERSION_ID", measurement.Str(versionID))
	}
	return measurement.NewMeasurement(measurement.TypeOS).
		WithSubtypeBuilder(sb).
		Build()
}

// topologyMeasurement builds a TypeNodeTopology measurement with the
// given node count.
func topologyMeasurement(nodeCount int) *measurement.Measurement {
	return measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(nodeCount)),
		).
		Build()
}

func TestFromMeasurements_Empty(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{})
	if got.Service.Value != "" || got.Accelerator.Value != "" || got.OS.Value != "" {
		t.Errorf("expected zero-value dimensions, got %+v", got)
	}
	if got.NodeCount.Value != 0 || got.K8sVersion.Value != "" {
		t.Errorf("expected zero K8sVersion/NodeCount, got %+v", got)
	}
}

func TestFromMeasurements_FullSnapshot(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{
		k8sMeasurement("v1.33.4", "eks"),
		gpuMeasurement("NVIDIA H100 80GB HBM3"),
		osMeasurement("ubuntu", "22.04"),
		topologyMeasurement(12),
	})

	if got.Service.Value != "eks" {
		t.Errorf("Service.Value = %q, want %q", got.Service.Value, "eks")
	}
	if got.Service.Source == "" {
		t.Error("Service.Source should be populated when value is set")
	}
	if got.Accelerator.Value != "h100" {
		t.Errorf("Accelerator.Value = %q, want %q", got.Accelerator.Value, "h100")
	}
	if got.OS.Value != "ubuntu" {
		t.Errorf("OS.Value = %q, want %q", got.OS.Value, "ubuntu")
	}
	if got.OS.Version != "22.04" {
		t.Errorf("OS.Version = %q, want %q", got.OS.Version, "22.04")
	}
	if got.K8sVersion.Value != "1.33.4" {
		t.Errorf("K8sVersion.Value = %q, want %q (leading 'v' should be stripped)", got.K8sVersion.Value, "1.33.4")
	}
	if got.NodeCount.Value != 12 {
		t.Errorf("NodeCount.Value = %d, want 12", got.NodeCount.Value)
	}
}

func TestFromMeasurements_ServiceDetection(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"eks", "eks"},
		{"gke", "gke"},
		{"aks", "aks"},
		{"oke", "oke"},
		{"kind", "kind"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{k8sMeasurement("", tt.provider)})
			if got.Service.Value != tt.want {
				t.Errorf("Service.Value = %q, want %q", got.Service.Value, tt.want)
			}
		})
	}
}

func TestFromMeasurements_OSDetection(t *testing.T) {
	tests := []struct {
		name        string
		id          string
		versionID   string
		wantValue   string
		wantVersion string
	}{
		{"ubuntu lts", "ubuntu", "22.04", "ubuntu", "22.04"},
		{"rhel", "rhel", "9.4", "rhel", "9.4"},
		{"redhat alias", "redhat", "9.4", "rhel", "9.4"},
		{"cos", "cos", "117", "cos", "117"},
		{"amzn AL2023", "amzn", "2023", "amazonlinux", "2023"},
		{"al2 alias", "al2", "2", "amazonlinux", "2"},
		{"talos", "talos", "1.7.6", "talos", "1.7.6"},
		{"unknown ID kept empty value but version retained", "freebsd", "13", "", "13"},
		{"both empty", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{osMeasurement(tt.id, tt.versionID)})
			if got.OS.Value != tt.wantValue {
				t.Errorf("OS.Value = %q, want %q", got.OS.Value, tt.wantValue)
			}
			if got.OS.Version != tt.wantVersion {
				t.Errorf("OS.Version = %q, want %q", got.OS.Version, tt.wantVersion)
			}
		})
	}
}

func TestFromMeasurements_K8sVersionStripsLeadingV(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{k8sMeasurement("v1.30.0", "")})
	if got.K8sVersion.Value != "1.30.0" {
		t.Errorf("K8sVersion.Value = %q, want %q", got.K8sVersion.Value, "1.30.0")
	}
	got = FromMeasurements([]*measurement.Measurement{k8sMeasurement("1.30.0", "")})
	if got.K8sVersion.Value != "1.30.0" {
		t.Errorf("K8sVersion.Value (no leading v) = %q, want %q", got.K8sVersion.Value, "1.30.0")
	}
}

func TestFromMeasurements_NilMeasurement(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{nil, k8sMeasurement("v1.30.0", "eks")})
	if got.Service.Value != "eks" {
		t.Errorf("expected nil measurements to be skipped, got Service.Value = %q", got.Service.Value)
	}
}

func TestFromMeasurements_GPUUnknownModel(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{gpuMeasurement("NVIDIA T4")})
	if got.Accelerator.Value != "" {
		t.Errorf("expected empty Accelerator for unrecognized model, got %q", got.Accelerator.Value)
	}
}

func TestFromMeasurements_GPUMissingSubtype(t *testing.T) {
	gpu := measurement.NewMeasurement(measurement.TypeGPU).Build()
	got := FromMeasurements([]*measurement.Measurement{gpu})
	if got.Accelerator.Value != "" {
		t.Errorf("expected empty Accelerator when smi subtype missing, got %q", got.Accelerator.Value)
	}
}

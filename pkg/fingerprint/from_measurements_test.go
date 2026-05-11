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
// given node count and an optional set of label-subtype entries
// encoded as the topology collector encodes them (value|node-list).
func topologyMeasurement(nodeCount int, labels map[string]string) *measurement.Measurement {
	b := measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(nodeCount)),
		)
	if len(labels) > 0 {
		labelSubtype := measurement.NewSubtypeBuilder("label")
		for k, v := range labels {
			labelSubtype = labelSubtype.Set(k, measurement.Str(v))
		}
		b = b.WithSubtypeBuilder(labelSubtype)
	}
	return b.Build()
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
		topologyMeasurement(12, map[string]string{
			"topology.kubernetes.io/region": "us-west-2|node1,node2",
		}),
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
	if got.Region.Value != "us-west-2" {
		t.Errorf("Region.Value = %q, want %q", got.Region.Value, "us-west-2")
	}
}

func TestFromMeasurements_GPUNodeCount(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{
			name: "all 3 nodes have gpu label",
			labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3|n1,n2,n3",
			},
			want: 3,
		},
		{
			name: "2 of 5 nodes have gpu (workers only)",
			labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3|worker1,worker2",
			},
			want: 2,
		},
		{
			name: "heterogeneous: union across disambiguated keys",
			labels: map[string]string{
				"nvidia.com/gpu.product.NVIDIA-H100-80GB-HBM3": "NVIDIA-H100-80GB-HBM3|n1,n2",
				"nvidia.com/gpu.product.NVIDIA-L40":            "NVIDIA-L40|n3",
			},
			want: 3,
		},
		{
			name:   "no gpu label: zero",
			labels: map[string]string{"kubernetes.io/arch": "amd64|n1,n2"},
			want:   0,
		},
		{
			name:   "no label subtype: zero",
			labels: nil,
			want:   0,
		},
		{
			name: "duplicate node names across keys deduped",
			labels: map[string]string{
				"nvidia.com/gpu.product.NVIDIA-H100-80GB-HBM3": "NVIDIA-H100-80GB-HBM3|n1,n2",
				"nvidia.com/gpu.product.NVIDIA-L40":            "NVIDIA-L40|n2,n3",
			},
			want: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{topologyMeasurement(5, tt.labels)})
			if got.GPUNodeCount.Value != tt.want {
				t.Errorf("GPUNodeCount.Value = %d, want %d", got.GPUNodeCount.Value, tt.want)
			}
		})
	}
}

func TestFromMeasurements_AcceleratorReconciliation(t *testing.T) {
	tests := []struct {
		name      string
		smiModel  string
		labels    map[string]string
		wantValue string
		wantNote  string
	}{
		{
			name:     "homogeneous: smi + matching single topology label",
			smiModel: "NVIDIA H100 80GB HBM3",
			labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3|node1,node2",
			},
			wantValue: "h100",
		},
		{
			name:     "heterogeneous: topology disambiguated keys override smi",
			smiModel: "NVIDIA H100 80GB HBM3",
			labels: map[string]string{
				"nvidia.com/gpu.product.NVIDIA-H100-80GB-HBM3": "NVIDIA-H100-80GB-HBM3|node1",
				"nvidia.com/gpu.product.NVIDIA-L40":            "NVIDIA-L40|node2",
			},
			wantValue: "",
			wantNote:  "multi-gpu",
		},
		{
			name:     "smi empty, single topology label backfills accelerator",
			smiModel: "",
			labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-GB200|node1,node2",
			},
			wantValue: "gb200",
		},
		{
			name:      "no topology labels: smi result preserved",
			smiModel:  "NVIDIA H100 80GB HBM3",
			labels:    nil,
			wantValue: "h100",
		},
		{
			name:      "no GPU anywhere: empty accelerator",
			smiModel:  "",
			labels:    map[string]string{"kubernetes.io/arch": "amd64|node1"},
			wantValue: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := []*measurement.Measurement{topologyMeasurement(2, tt.labels)}
			if tt.smiModel != "" {
				ms = append(ms, gpuMeasurement(tt.smiModel))
			}
			got := FromMeasurements(ms)
			if got.Accelerator.Value != tt.wantValue {
				t.Errorf("Accelerator.Value = %q, want %q", got.Accelerator.Value, tt.wantValue)
			}
			if got.Accelerator.Note != tt.wantNote {
				t.Errorf("Accelerator.Note = %q, want %q", got.Accelerator.Note, tt.wantNote)
			}
		})
	}
}

func TestFromMeasurements_RegionDetection(t *testing.T) {
	tests := []struct {
		name      string
		labels    map[string]string
		wantValue string
		wantNote  string
	}{
		{
			name:      "single region",
			labels:    map[string]string{"topology.kubernetes.io/region": "us-west-2|node1,node2"},
			wantValue: "us-west-2",
		},
		{
			name: "multi region disambiguated keys records note",
			labels: map[string]string{
				"topology.kubernetes.io/region.us-west-2": "us-west-2|node1",
				"topology.kubernetes.io/region.us-east-1": "us-east-1|node2",
			},
			wantValue: "",
			wantNote:  "multi-region",
		},
		{
			name:   "no region label",
			labels: map[string]string{"kubernetes.io/arch": "amd64|node1"},
		},
		{
			name:   "no label subtype",
			labels: nil,
		},
		{
			name:      "single-node single-region without pipe is tolerated",
			labels:    map[string]string{"topology.kubernetes.io/region": "us-west-2"},
			wantValue: "us-west-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{topologyMeasurement(1, tt.labels)})
			if got.Region.Value != tt.wantValue {
				t.Errorf("Region.Value = %q, want %q", got.Region.Value, tt.wantValue)
			}
			if got.Region.Note != tt.wantNote {
				t.Errorf("Region.Note = %q, want %q", got.Region.Note, tt.wantNote)
			}
			if tt.wantValue != "" && got.Region.Source == "" {
				t.Error("Region.Source should be populated when value is set")
			}
		})
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
		{"unknown ID drops both value and version", "freebsd", "13", "", ""},
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
	if got.Accelerator.Note != "unknown-sku" {
		t.Errorf("expected Accelerator.Note=unknown-sku for unrecognized model, got %q", got.Accelerator.Note)
	}
	if got.Accelerator.Source != "gpu.smi.gpu.model" {
		t.Errorf("expected smi source, got %q", got.Accelerator.Source)
	}
}

func TestFromMeasurements_GPUMissingSubtype(t *testing.T) {
	gpu := measurement.NewMeasurement(measurement.TypeGPU).Build()
	got := FromMeasurements([]*measurement.Measurement{gpu})
	if got.Accelerator.Value != "" {
		t.Errorf("expected empty Accelerator when smi subtype missing, got %q", got.Accelerator.Value)
	}
	if got.Accelerator.Note != "" {
		t.Errorf("expected empty Accelerator.Note when no GPU signal exists, got %q", got.Accelerator.Note)
	}
}

// TestFromMeasurements_GPUUnknownModelFromTopology exercises the
// topology-label backfill path: when smi did not run (e.g. agent
// landed on a non-GPU node) but the GPU operator labels nodes, the
// reconciliation pass parses the label's product string through the
// same ParseGPUSKU registry — an unrecognized model surfaces
// unknown-sku via the topology source so registry staleness is
// visible in the snapshot.
func TestFromMeasurements_GPUUnknownModelFromTopology(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{topologyMeasurement(1, map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-T4|node1",
	})})
	if got.Accelerator.Value != "" {
		t.Errorf("expected empty Accelerator for unrecognized topology product, got %q", got.Accelerator.Value)
	}
	if got.Accelerator.Note != "unknown-sku" {
		t.Errorf("expected Accelerator.Note=unknown-sku for unrecognized topology product, got %q", got.Accelerator.Note)
	}
	if got.Accelerator.Source != "nodeTopology.label.nvidia.com/gpu.product" {
		t.Errorf("expected topology source, got %q", got.Accelerator.Source)
	}
}

// TestFromMeasurements_SMIUnknownPlusTopologyRecognized covers the
// reconcile path where smi reported an unrecognized product (note:
// unknown-sku, value empty) but the topology label resolves to a
// known SKU. Topology is the more authoritative cluster-wide signal
// and must win — Value gets backfilled and the unknown-sku note is
// cleared so reviewers see the resolved SKU, not the stale signal.
func TestFromMeasurements_SMIUnknownPlusTopologyRecognized(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{
		gpuMeasurement("NVIDIA T4"), // smi: unrecognized
		topologyMeasurement(1, map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3|node1",
		}),
	})
	if got.Accelerator.Value != "h100" {
		t.Errorf("expected topology to backfill h100, got %q", got.Accelerator.Value)
	}
	if got.Accelerator.Note != "" {
		t.Errorf("expected unknown-sku note cleared after topology recognized SKU, got %q", got.Accelerator.Note)
	}
}

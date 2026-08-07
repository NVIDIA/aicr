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
	"fmt"
	"strings"
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

// gpuHardwareMeasurement builds a TypeGPU measurement with a "hardware"
// subtype carrying a PCI-derived SKU (the only GPU subtype after the SMI
// collector was removed).
func gpuHardwareMeasurement(sku string) *measurement.Measurement {
	hw := measurement.NewSubtypeBuilder("hardware").
		Set("gpu-present", measurement.Bool(true)).
		Set("gpu-count", measurement.Int(1)).
		Set("detection-source", measurement.Str("nfd"))
	if sku != "" {
		hw = hw.Set("model", measurement.Str(sku))
	}
	return measurement.NewMeasurement(measurement.TypeGPU).
		WithSubtypeBuilder(hw).
		Build()
}

func TestFromMeasurements_PCIBackfill(t *testing.T) {
	t.Run("supported SKU backfills Accelerator and GPUModel when nvidia-smi absent and no label", func(t *testing.T) {
		got := FromMeasurements([]*measurement.Measurement{gpuHardwareMeasurement("h100")})
		if got.Accelerator.Value != "h100" || got.Accelerator.Source != sourceAcceleratorPCI {
			t.Errorf("Accelerator = %+v, want value h100 from PCI", got.Accelerator)
		}
		if got.GPUModel.Value != "h100" {
			t.Errorf("GPUModel.Value = %q, want %q", got.GPUModel.Value, "h100")
		}
	})

	t.Run("unsupported SKU populates GPUModel + unknown-sku note, never the matching Accelerator value", func(t *testing.T) {
		// a10 is a known PCI SKU (device_ids.go) but is not a recipe-supported
		// accelerator enum, so it exercises the unsupported-SKU path.
		got := FromMeasurements([]*measurement.Measurement{gpuHardwareMeasurement("a10")})
		if got.Accelerator.Value != "" {
			t.Errorf("Accelerator.Value = %q, want empty (a10 is not a recipe-supported enum)", got.Accelerator.Value)
		}
		if got.Accelerator.Note != noteUnknownSKU || got.Accelerator.Source != sourceAcceleratorPCI {
			t.Errorf("Accelerator = %+v, want unknown-sku note from PCI (GPU present but unsupported)", got.Accelerator)
		}
		if got.GPUModel.Value != "a10" || got.GPUModel.Source != sourceAcceleratorPCI {
			t.Errorf("GPUModel = %+v, want value a10 from PCI", got.GPUModel)
		}
	})

	t.Run("no PCI SKU leaves both empty", func(t *testing.T) {
		got := FromMeasurements([]*measurement.Measurement{gpuHardwareMeasurement("")})
		if got.Accelerator.Value != "" || got.GPUModel.Value != "" {
			t.Errorf("Accelerator=%+v GPUModel=%+v, want both empty", got.Accelerator, got.GPUModel)
		}
	})

	t.Run("GFD label takes precedence over PCI for Accelerator; GPUModel still from PCI", func(t *testing.T) {
		got := FromMeasurements([]*measurement.Measurement{
			gpuHardwareMeasurement("h100"),
			topologyMeasurement(1, map[string]string{"nvidia.com/gpu.product": "NVIDIA L40"}),
		})
		if got.Accelerator.Value != "l40" || got.Accelerator.Source != sourceTopologyGPU {
			t.Errorf("Accelerator = %+v, want l40 from label (primary)", got.Accelerator)
		}
		if got.GPUModel.Value != "h100" {
			t.Errorf("GPUModel.Value = %q, want %q (PCI discovery independent of label)", got.GPUModel.Value, "h100")
		}
	})
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
		gpuHardwareMeasurement("h100"),
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
		labels    map[string]string
		wantValue string
		wantNote  string
	}{
		{
			name: "homogeneous: single topology label resolves SKU",
			labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3|node1,node2",
			},
			wantValue: "h100",
		},
		{
			name: "heterogeneous: disambiguated keys record multi-gpu",
			labels: map[string]string{
				"nvidia.com/gpu.product.NVIDIA-H100-80GB-HBM3": "NVIDIA-H100-80GB-HBM3|node1",
				"nvidia.com/gpu.product.NVIDIA-L40":            "NVIDIA-L40|node2",
			},
			wantValue: "",
			wantNote:  "multi-gpu",
		},
		{
			name: "single topology label backfills accelerator",
			labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-GB200|node1,node2",
			},
			wantValue: "gb200",
		},
		{
			name:      "no GPU label: empty accelerator",
			labels:    map[string]string{"kubernetes.io/arch": "amd64|node1"},
			wantValue: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{topologyMeasurement(2, tt.labels)})
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
		{"oci", "oke"},
		{"oci://ocid1.instance.oc1.us-chicago-1.example", "oke"},
		{"ocid1.instance.oc1.us-chicago-1.anxxeljsaqwjupqcb4pa5kzxy4hef5dtclbkqsnmu6kedbkrne3s2bz5nwzq", "oke"},
		{"lke", "lke"},
		{"linode", "lke"},
		{"linode://58291", "lke"},
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
		{"oracle linux", "ol", "8.10", "ol", "8.10"},
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

func TestFromMeasurements_GPUMissingSubtype(t *testing.T) {
	gpu := measurement.NewMeasurement(measurement.TypeGPU).Build()
	got := FromMeasurements([]*measurement.Measurement{gpu})
	if got.Accelerator.Value != "" {
		t.Errorf("expected empty Accelerator when hardware subtype missing, got %q", got.Accelerator.Value)
	}
	if got.Accelerator.Note != "" {
		t.Errorf("expected empty Accelerator.Note when no GPU signal exists, got %q", got.Accelerator.Note)
	}
	if got.GPUModel.Value != "" {
		t.Errorf("expected empty GPUModel when hardware subtype missing, got %q", got.GPUModel.Value)
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

// TestFromMeasurements_LabelRecognizedWithUnknownPCI covers a node whose GPU
// "hardware" subtype reports a SKU outside the recipe enum (so it cannot
// backfill the matching dimension) while the GPU-operator label resolves to a
// supported SKU. The label is primary and must win for Accelerator; the PCI
// SKU still surfaces descriptively via GPUModel.
func TestFromMeasurements_LabelRecognizedWithUnknownPCI(t *testing.T) {
	got := FromMeasurements([]*measurement.Measurement{
		gpuHardwareMeasurement("a10"), // PCI: unsupported-for-matching SKU (a10 is not in the recipe enum)
		topologyMeasurement(1, map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3|node1",
		}),
	})
	if got.Accelerator.Value != "h100" || got.Accelerator.Source != "nodeTopology.label.nvidia.com/gpu.product" {
		t.Errorf("Accelerator = %+v, want h100 from label (primary)", got.Accelerator)
	}
	if got.GPUModel.Value != "a10" {
		t.Errorf("GPUModel.Value = %q, want a10 (PCI discovery)", got.GPUModel.Value)
	}
}

// topologyItemsMeasurement builds a TypeNodeTopology measurement carrying the
// collector's lossless label encoding. readings maps a label key to the values
// it carries; each value's node count is taken from counts (default 1), so a
// truncated reading can be expressed without a node list.
func topologyItemsMeasurement(nodeCount int, readings map[string][]string, counts map[string]int) *measurement.Measurement {
	var items []measurement.ItemEntry
	for key, values := range readings {
		for _, v := range values {
			n := 1
			if c, ok := counts[key+"="+v]; ok {
				n = c
			}
			names := make([]string, 0, n)
			for i := 0; i < n && i < 3; i++ {
				names = append(names, fmt.Sprintf("node-%d", i))
			}
			// Render the list the way formatNodeList does — marker included —
			// so the fixture stays consistent with the truncated flag.
			list := strings.Join(names, ",")
			if n > len(names) {
				list += fmt.Sprintf(" (+%d more)", n-len(names))
			}
			items = append(items, measurement.ItemEntry{
				Context: map[string]string{"key": key, "value": v},
				Data: map[string]measurement.Reading{
					"node-count": measurement.Int(n),
					"node-list":  measurement.Str(list),
					"truncated":  measurement.Bool(n > len(names)),
				},
			})
		}
	}
	return measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtypeBuilder(
			measurement.NewSubtypeBuilder("summary").
				Set("node-count", measurement.Int(nodeCount)),
		).
		WithSubtype(measurement.Subtype{Name: "label", Items: items}).
		Build()
}

// TestFromMeasurements_ItemsAccelerator pins the item path for the accelerator
// dimension, including the false positive the folded encoding cannot avoid:
// two distinct labels sharing the nvidia.com/gpu.product prefix are counted as
// two values by hasMultiValueKeys, clearing the accelerator with a spurious
// multi-gpu note. Accelerator drives overlay selection, so that matters more
// than any count.
func TestFromMeasurements_ItemsAccelerator(t *testing.T) {
	tests := []struct {
		name      string
		readings  map[string][]string
		wantValue string
		wantNote  string
	}{
		{
			name:      "single recognized SKU",
			readings:  map[string][]string{labelKeyGPUProduct: {"NVIDIA-H100-80GB-HBM3"}},
			wantValue: "h100",
		},
		{
			name:     "two SKUs is genuinely multi-gpu",
			readings: map[string][]string{labelKeyGPUProduct: {"NVIDIA-H100-80GB-HBM3", "NVIDIA-B200"}},
			wantNote: noteMultiGPU,
		},
		{
			name:     "unrecognized SKU",
			readings: map[string][]string{labelKeyGPUProduct: {"NVIDIA-SOMETHING-NEW"}},
			wantNote: noteUnknownSKU,
		},
		{
			// Prefix siblings, not values of gpu.product.
			name: "distinct labels sharing the prefix are not extra values",
			readings: map[string][]string{
				labelKeyGPUProduct:             {"NVIDIA-H100-80GB-HBM3"},
				labelKeyGPUProduct + ".tier":   {"premium"},
				labelKeyGPUProduct + ".vendor": {"nvidia"},
			},
			wantValue: "h100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{
				topologyItemsMeasurement(2, tt.readings, nil),
			})
			if got.Accelerator.Value != tt.wantValue {
				t.Errorf("Accelerator.Value = %q, want %q", got.Accelerator.Value, tt.wantValue)
			}
			if got.Accelerator.Note != tt.wantNote {
				t.Errorf("Accelerator.Note = %q, want %q", got.Accelerator.Note, tt.wantNote)
			}
			if got.Accelerator.Source != sourceTopologyGPU {
				t.Errorf("Accelerator.Source = %q, want %q — source strings are pinned",
					got.Accelerator.Source, sourceTopologyGPU)
			}
		})
	}
}

// TestFromMeasurements_ItemsGPUNodeCountUnderTruncation pins the correction:
// the folded encoding derives the count by splitting the rendered node list on
// commas, so under --max-nodes-per-entry it reports the cap. node-count is the
// true pre-truncation total.
func TestFromMeasurements_ItemsGPUNodeCountUnderTruncation(t *testing.T) {
	m := topologyItemsMeasurement(40,
		map[string][]string{labelKeyGPUProduct: {"NVIDIA-H100-80GB-HBM3"}},
		map[string]int{labelKeyGPUProduct + "=NVIDIA-H100-80GB-HBM3": 40},
	)

	got := FromMeasurements([]*measurement.Measurement{m})
	if got.GPUNodeCount.Value != 40 {
		t.Errorf("GPUNodeCount = %d, want 40 (the true total, not the rendered list length)",
			got.GPUNodeCount.Value)
	}
	if got.GPUNodeCount.Source != sourceTopologyGPU {
		t.Errorf("GPUNodeCount.Source = %q, want %q", got.GPUNodeCount.Source, sourceTopologyGPU)
	}
}

// TestFromMeasurements_ItemsRegion pins the item path for region detection,
// including the prefix-sibling false positive that today yields multi-region
// on a single-region cluster.
func TestFromMeasurements_ItemsRegion(t *testing.T) {
	tests := []struct {
		name       string
		readings   map[string][]string
		wantRegion string
		wantNote   string
	}{
		{
			name:       "single region",
			readings:   map[string][]string{labelKeyRegion: {"us-west-2"}},
			wantRegion: "us-west-2",
		},
		{
			name:     "two regions",
			readings: map[string][]string{labelKeyRegion: {"us-west-2", "us-east-1"}},
			wantNote: noteMultiRegion,
		},
		{
			name: "distinct labels sharing the prefix are not extra regions",
			readings: map[string][]string{
				labelKeyRegion:             {"us-west-2"},
				labelKeyRegion + ".legacy": {"true"},
				labelKeyRegion + ".zone":   {"a"},
			},
			wantRegion: "us-west-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMeasurements([]*measurement.Measurement{
				topologyItemsMeasurement(2, tt.readings, nil),
			})
			if got.Region.Value != tt.wantRegion {
				t.Errorf("Region.Value = %q, want %q", got.Region.Value, tt.wantRegion)
			}
			if got.Region.Note != tt.wantNote {
				t.Errorf("Region.Note = %q, want %q", got.Region.Note, tt.wantNote)
			}
		})
	}
}

// malformedItemSubtype returns a label subtype whose single item omits a
// required field, so the shared accessor rejects the whole subtype.
func malformedItemSubtype() *measurement.Subtype {
	return &measurement.Subtype{
		Name: "label",
		Items: []measurement.ItemEntry{{
			Context: map[string]string{"key": labelKeyGPUProduct, "value": "NVIDIA-H100-80GB-HBM3"},
			Data:    map[string]measurement.Reading{"node-list": measurement.Str("gpu-a")},
		}},
	}
}

// TestItemReadersDegradeOnDecodeError pins that a rejected subtype yields an
// empty dimension rather than a partial one. Both readers deliberately swallow
// the error — the fingerprint is advisory — so nothing else would notice if
// they started returning half-decoded data instead.
func TestItemReadersDegradeOnDecodeError(t *testing.T) {
	st := malformedItemSubtype()

	if got := distinctLabelValues(st, labelKeyGPUProduct); got != nil {
		t.Errorf("distinctLabelValues() = %v, want nil on a rejected subtype", got)
	}
	if got := countGPUNodesFromItems(st); got != 0 {
		t.Errorf("countGPUNodesFromItems() = %d, want 0 on a rejected subtype", got)
	}
}

// TestDistinctLabelValuesDedupes pins that two readings sharing a key and a
// value collapse to one. The collector cannot emit that pair, but a hand-built
// snapshot can, and counting it twice would read as a heterogeneous cluster.
func TestDistinctLabelValuesDedupes(t *testing.T) {
	item := func(nodes string) measurement.ItemEntry {
		return measurement.ItemEntry{
			Context: map[string]string{"key": labelKeyGPUProduct, "value": "NVIDIA-H100-80GB-HBM3"},
			Data: map[string]measurement.Reading{
				"node-count": measurement.Int(1),
				"node-list":  measurement.Str(nodes),
				"truncated":  measurement.Bool(false),
			},
		}
	}
	st := &measurement.Subtype{Name: "label", Items: []measurement.ItemEntry{item("gpu-a"), item("gpu-b")}}

	if got := distinctLabelValues(st, labelKeyGPUProduct); len(got) != 1 {
		t.Errorf("distinctLabelValues() = %v, want one distinct value", got)
	}
}

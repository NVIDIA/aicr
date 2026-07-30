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
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	k8scollector "github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

func writePoolsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pools.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing pools file: %v", err)
	}
	return path
}

func findAKSGPUPools(snap *Snapshot) *measurement.Subtype {
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeK8s {
			continue
		}
		for i := range m.Subtypes {
			if m.Subtypes[i].Name == k8scollector.SubtypeAKSGPUPools {
				return &m.Subtypes[i]
			}
		}
	}
	return nil
}

// TestMeasureAttachesAKSGPUPools pins the local-mode orchestration path:
// the explicit pool projection lands on the K8s measurement of the
// serialized snapshot without touching any collector.
func TestMeasureAttachesAKSGPUPools(t *testing.T) {
	ser := &mockSerializer{}
	ns := &NodeSnapshotter{
		Version:    "1.0.0",
		Factory:    &mockFactory{},
		Serializer: ser,
		AKSGPUPoolsPath: writePoolsFile(t,
			`[{"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install"}}]`),
	}

	if err := ns.Measure(t.Context()); err != nil {
		t.Fatalf("Measure() error = %v", err)
	}
	snap, ok := ser.data.(*Snapshot)
	if !ok {
		t.Fatalf("serialized %T, want *Snapshot", ser.data)
	}
	subtype := findAKSGPUPools(snap)
	if subtype == nil {
		t.Fatal("snapshot is missing the aks-gpu-pools subtype")
	}
	if got, _ := subtype.Data["gpu-driver"].Any().(string); got != "Install" {
		t.Fatalf("gpu-driver = %v, want Install", subtype.Data["gpu-driver"].Any())
	}
}

// TestMeasureFailsLoudOnBadPoolsFile pins the fail-loud contract at the
// orchestration boundary: explicit operator input never rides the
// collectSafe degrade-to-warning policy, and the run fails before any
// collector executes.
func TestMeasureFailsLoudOnBadPoolsFile(t *testing.T) {
	factory := &mockFactory{}
	ns := &NodeSnapshotter{
		Version:         "1.0.0",
		Factory:         factory,
		Serializer:      &mockSerializer{},
		AKSGPUPoolsPath: filepath.Join(t.TempDir(), "absent.json"),
	}

	if err := ns.Measure(t.Context()); err == nil {
		t.Fatal("Measure() error = nil, want failure on a missing pools file")
	}
	if factory.k8sCalled {
		t.Error("collectors ran despite the pools pre-projection failing")
	}
}

// TestAttachAKSGPUPoolsCreatesK8sMeasurement pins the degraded-collection
// case: explicit input survives even when no K8s measurement was collected.
func TestAttachAKSGPUPoolsCreatesK8sMeasurement(t *testing.T) {
	snap := NewSnapshot()
	attachAKSGPUPools(snap, measurement.Subtype{
		Name: k8scollector.SubtypeAKSGPUPools,
		Data: map[string]measurement.Reading{"gpu-driver": measurement.Str("None")},
	})

	subtype := findAKSGPUPools(snap)
	if subtype == nil {
		t.Fatal("attach did not create a K8s measurement for the subtype")
	}
	if got, _ := subtype.Data["gpu-driver"].Any().(string); got != "None" {
		t.Fatalf("gpu-driver = %v, want None", subtype.Data["gpu-driver"].Any())
	}
}

// TestMergeAKSGPUPools pins the agent Job-mode path: the controller-side
// projection merges into the YAML the Job returned, on the existing K8s
// measurement.
func TestMergeAKSGPUPools(t *testing.T) {
	snap := NewSnapshot()
	snap.Measurements = append(snap.Measurements,
		measurement.NewMeasurement(measurement.TypeK8s).Build())
	raw, err := yaml.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	merged, err := mergeAKSGPUPools(raw, measurement.Subtype{
		Name: k8scollector.SubtypeAKSGPUPools,
		Data: map[string]measurement.Reading{"gpu-driver": measurement.Str("Install")},
	})
	if err != nil {
		t.Fatalf("mergeAKSGPUPools() error = %v", err)
	}

	var got Snapshot
	if err := yaml.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	subtype := findAKSGPUPools(&got)
	if subtype == nil {
		t.Fatal("merged snapshot is missing the aks-gpu-pools subtype")
	}
	if got, _ := subtype.Data["gpu-driver"].Any().(string); got != "Install" {
		t.Fatalf("gpu-driver = %v, want Install", subtype.Data["gpu-driver"].Any())
	}

	if _, err := mergeAKSGPUPools([]byte("{not yaml"), measurement.Subtype{}); err == nil {
		t.Fatal("mergeAKSGPUPools(garbage) = nil, want parse error")
	}
}

// TestRewriteSnapshotConfigMapRejectsBadInput pins the guard branches; the
// live write path requires a cluster and is covered by e2e.
func TestRewriteSnapshotConfigMapRejectsBadInput(t *testing.T) {
	if err := rewriteSnapshotConfigMap(t.Context(), "not-a-cm-uri", "", []byte("{}")); err == nil {
		t.Fatal("rewriteSnapshotConfigMap(bad URI) = nil, want error")
	}
	if err := rewriteSnapshotConfigMap(t.Context(), "cm://ns/name", "", []byte("{not yaml")); err == nil {
		t.Fatal("rewriteSnapshotConfigMap(garbage snapshot) = nil, want parse error")
	}
}

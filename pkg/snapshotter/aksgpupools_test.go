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
	"reflect"
	"strings"
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

	// An empty or `null` agent document (malfunctioning agent image)
	// unmarshals to a nil map without error; the merge must produce a
	// document carrying the subtype instead of panicking.
	for _, degenerate := range [][]byte{nil, []byte(""), []byte("null" + "\n")} {
		merged, err := mergeAKSGPUPools(degenerate, measurement.Subtype{
			Name: k8scollector.SubtypeAKSGPUPools,
			Data: map[string]measurement.Reading{"gpu-driver": measurement.Str("Install")},
		})
		if err != nil {
			t.Fatalf("mergeAKSGPUPools(degenerate %q) error = %v", degenerate, err)
		}
		var got Snapshot
		if err := yaml.Unmarshal(merged, &got); err != nil {
			t.Fatalf("unmarshal degenerate-merged: %v", err)
		}
		if findAKSGPUPools(&got) == nil {
			t.Fatalf("degenerate %q merge dropped the subtype", degenerate)
		}
	}
}

// TestMergeAKSGPUPoolsPreservesUnknownFields pins the version-skew contract:
// fields a newer agent image emits that this binary's Snapshot struct does
// not declare must survive the merge byte path.
func TestMergeAKSGPUPoolsPreservesUnknownFields(t *testing.T) {
	raw := []byte(`kind: Snapshot
futureTopLevel: keep-me
measurements:
    - type: K8s
      futureMeasurementField: keep-me-too
      subtypes:
        - subtype: server
          data:
            version: v1.35.0
          futureSubtypeField: keep-me-three
`)
	merged, err := mergeAKSGPUPools(raw, measurement.Subtype{
		Name: k8scollector.SubtypeAKSGPUPools,
		Data: map[string]measurement.Reading{"gpu-driver": measurement.Str("None")},
	})
	if err != nil {
		t.Fatalf("mergeAKSGPUPools() error = %v", err)
	}
	for _, want := range []string{"futureTopLevel: keep-me", "futureMeasurementField: keep-me-too",
		"futureSubtypeField: keep-me-three", "gpu-driver: None"} {
		if !strings.Contains(string(merged), want) {
			t.Fatalf("merged output missing %q:\n%s", want, merged)
		}
	}
}

// TestWriteSnapshotConfigMapRejectsBadInput pins the guard branches; the
// live write path requires a cluster and is covered by e2e.
func TestWriteSnapshotConfigMapRejectsBadInput(t *testing.T) {
	if err := writeSnapshotConfigMap(t.Context(), "not-a-cm-uri", "", []byte("{}")); err == nil {
		t.Fatal("writeSnapshotConfigMap(bad URI) = nil, want error")
	}
	if err := writeSnapshotConfigMap(t.Context(), "cm://ns/name", "", []byte("{not yaml")); err == nil {
		t.Fatal("writeSnapshotConfigMap(garbage snapshot) = nil, want parse error")
	}
}

// TestRawSnapshotDocRoundTrip pins the header/marshal contract of the
// generic-document wrapper the ConfigMap rewrite serializes through: kind
// and string metadata must come from the document itself (version-skew
// preservation — no typed round trip), non-string metadata values are
// skipped, and MarshalYAML must emit the raw document unchanged.
func TestRawSnapshotDocRoundTrip(t *testing.T) {
	doc := map[string]any{
		"kind": "Snapshot",
		"metadata": map[string]any{
			"version":        "9.9.9",
			"source-node":    "node-a",
			"futureIntField": 7, // non-string: must be skipped, not stringified
		},
		"futureTopLevel": "keep-me",
	}
	r := rawSnapshotDoc{doc: doc}

	if got := r.GetKind(); string(got) != "Snapshot" {
		t.Errorf("GetKind() = %q, want Snapshot", got)
	}
	meta := r.GetMetadata()
	if meta["version"] != "9.9.9" || meta["source-node"] != "node-a" {
		t.Errorf("GetMetadata() = %v, want version/source-node preserved", meta)
	}
	if _, ok := meta["futureIntField"]; ok {
		t.Errorf("GetMetadata() stringified a non-string value: %v", meta)
	}
	out, err := r.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML() error = %v", err)
	}
	if !reflect.DeepEqual(out, doc) {
		t.Errorf("MarshalYAML() = %v, want the raw document unchanged", out)
	}

	// Absent/mistyped fields degrade to empty, never panic.
	empty := rawSnapshotDoc{doc: map[string]any{"kind": 42, "metadata": "not-a-map"}}
	if got := empty.GetKind(); got != "" {
		t.Errorf("GetKind(mistyped) = %q, want empty", got)
	}
	if got := empty.GetMetadata(); len(got) != 0 {
		t.Errorf("GetMetadata(mistyped) = %v, want empty", got)
	}
}

// TestRewriteMergedSnapshotConfigMapDeliveryContract pins the branch that
// resolved the review's delivery-abort finding: a rewrite failure is fatal
// only when the ConfigMap is the delivery vehicle; otherwise the run
// continues (warn) because the returned bytes already carry the merged
// reading. Failure is injected via an invalid ConfigMap URI, which fails
// rewriteSnapshotConfigMap before any cluster access — same branch, no
// fake clientset needed.
func TestRewriteMergedSnapshotConfigMapDeliveryContract(t *testing.T) {
	if err := rewriteMergedSnapshotConfigMap(t.Context(), "not-a-cm-uri", "", []byte("{}"), true); err == nil {
		t.Fatal("deliverViaConfigMap=true with failing rewrite = nil, want the error to propagate")
	}
	if err := rewriteMergedSnapshotConfigMap(t.Context(), "not-a-cm-uri", "", []byte("{}"), false); err != nil {
		t.Fatalf("deliverViaConfigMap=false with failing rewrite = %v, want nil (warn and continue)", err)
	}
}

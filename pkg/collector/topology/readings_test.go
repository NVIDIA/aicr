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

package topology

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	corev1 "k8s.io/api/core/v1"
)

// collectSubtypes runs the real collector over a fake cluster and returns its
// label and taint subtypes. Readings are asserted against collector output
// rather than hand-built fixtures so the encoder and the decoder cannot drift.
func collectSubtypes(t *testing.T, maxNodes int, nodes ...*corev1.Node) (labelSt, taintSt *measurement.Subtype) {
	t.Helper()

	m, err := newFakeCollector(nodes, maxNodes).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	return m.GetSubtype("label"), m.GetSubtype("taint")
}

// dataOnly strips Items, producing the shape a snapshot captured before the
// lossless encoding carries. This is the legacy-compatibility fixture.
func dataOnly(st *measurement.Subtype) *measurement.Subtype {
	return &measurement.Subtype{Name: st.Name, Data: st.Data}
}

// TestLabelReadingsRecoversEveryReading is the core guarantee: every label
// present on a node appears exactly once in the decoded readings, including
// the #2003 case where a synthesized "<key>.<value>" key collides with a label
// literally named that.
func TestLabelReadingsRecoversEveryReading(t *testing.T) {
	labelSt, _ := collectSubtypes(t, 0,
		makeNode("gpu-a", nil, map[string]string{"zone": "us-west", "zone.us-west": "true"}),
		makeNode("gpu-b", nil, map[string]string{"zone": "us-east", "zone.us-west": "true"}),
	)

	readings, err := LabelReadings(labelSt)
	if err != nil {
		t.Fatalf("LabelReadings() error: %v", err)
	}

	want := []LabelReading{
		{Key: "zone", Value: "us-east", Nodes: []string{"gpu-b"}, NodeCount: 1},
		{Key: "zone", Value: "us-west", Nodes: []string{"gpu-a"}, NodeCount: 1},
		{Key: "zone.us-west", Value: "true", Nodes: []string{"gpu-a", "gpu-b"}, NodeCount: 2},
	}
	if len(readings) != len(want) {
		t.Fatalf("got %d readings, want %d: %+v", len(readings), len(want), readings)
	}
	for i, w := range want {
		got := readings[i]
		if got.Key != w.Key || got.Value != w.Value || got.NodeCount != w.NodeCount {
			t.Errorf("reading %d = {%q %q n=%d}, want {%q %q n=%d}",
				i, got.Key, got.Value, got.NodeCount, w.Key, w.Value, w.NodeCount)
		}
		if len(got.Nodes) != len(w.Nodes) {
			t.Errorf("reading %d nodes = %v, want %v", i, got.Nodes, w.Nodes)
		}
	}

	// The same subtype without Items loses one reading — the defect this
	// encoding exists to fix. Pinned so a regression in the Items path cannot
	// pass by quietly falling back.
	legacy, err := LabelReadings(dataOnly(labelSt))
	if err != nil {
		t.Fatalf("legacy LabelReadings() error: %v", err)
	}
	if len(legacy) >= len(readings) {
		t.Errorf("legacy Data decode returned %d readings; expected fewer than the %d "+
			"in the lossless form (the collision is what #2003 reports)", len(legacy), len(readings))
	}
}

// TestTaintReadingsRecoversEveryReading covers the taint-side collision, which
// needs no dotted key: encodeTaints counts per key but disambiguates with
// effect, so two taints sharing both collapse into one Data entry.
func TestTaintReadingsRecoversEveryReading(t *testing.T) {
	_, taintSt := collectSubtypes(t, 0,
		makeNode("node-1", []corev1.Taint{
			{Key: "dedicated", Value: "team-a", Effect: corev1.TaintEffectNoSchedule},
		}, nil),
		makeNode("node-2", []corev1.Taint{
			{Key: "dedicated", Value: "team-b", Effect: corev1.TaintEffectNoSchedule},
		}, nil),
	)

	readings, err := TaintReadings(taintSt)
	if err != nil {
		t.Fatalf("TaintReadings() error: %v", err)
	}
	if len(readings) != 2 {
		t.Fatalf("got %d taint readings, want 2: %+v", len(readings), readings)
	}
	for i, want := range []struct{ value, node string }{{"team-a", "node-1"}, {"team-b", "node-2"}} {
		got := readings[i]
		if got.Key != "dedicated" || got.Effect != "NoSchedule" || got.Value != want.value {
			t.Errorf("reading %d = {%q %q %q}, want {dedicated NoSchedule %q}",
				i, got.Key, got.Effect, got.Value, want.value)
		}
		if len(got.Nodes) != 1 || got.Nodes[0] != want.node {
			t.Errorf("reading %d nodes = %v, want [%s]", i, got.Nodes, want.node)
		}
	}

	legacy, err := TaintReadings(dataOnly(taintSt))
	if err != nil {
		t.Fatalf("legacy TaintReadings() error: %v", err)
	}
	if len(legacy) != 1 {
		t.Errorf("legacy Data decode returned %d taint readings, want 1 "+
			"(both collapse onto dedicated.NoSchedule)", len(legacy))
	}
}

// TestReadingsAgreeWhenEncodingIsUnambiguous pins that the two paths return
// the same thing whenever the Data encoding is capable of representing the
// readings — i.e. every case except a collision. Without this, a divergence
// introduced on either path would only surface as a consumer bug.
func TestReadingsAgreeWhenEncodingIsUnambiguous(t *testing.T) {
	labelSt, taintSt := collectSubtypes(t, 0,
		makeNode("node-1",
			[]corev1.Taint{{Key: "dedicated", Value: "sys", Effect: corev1.TaintEffectNoSchedule}},
			map[string]string{"kubernetes.io/arch": "arm64", "zone": "us-west"},
		),
		makeNode("node-2", nil,
			map[string]string{"kubernetes.io/arch": "arm64", "zone": "us-east"},
		),
	)

	items, err := LabelReadings(labelSt)
	if err != nil {
		t.Fatalf("items path: %v", err)
	}
	legacy, err := LabelReadings(dataOnly(labelSt))
	if err != nil {
		t.Fatalf("data path: %v", err)
	}
	if len(items) != len(legacy) {
		t.Fatalf("label reading counts differ: items=%d data=%d", len(items), len(legacy))
	}
	for i := range items {
		// The Data path cannot recover a disambiguated key's true name, so
		// compare on RawKey — the identity both encodings do agree on.
		if items[i].RawKey != legacy[i].RawKey {
			t.Errorf("label %d RawKey: items=%q data=%q", i, items[i].RawKey, legacy[i].RawKey)
		}
		if items[i].Value != legacy[i].Value {
			t.Errorf("label %d value: items=%q data=%q", i, items[i].Value, legacy[i].Value)
		}
	}

	taintItems, err := TaintReadings(taintSt)
	if err != nil {
		t.Fatalf("taint items path: %v", err)
	}
	taintLegacy, err := TaintReadings(dataOnly(taintSt))
	if err != nil {
		t.Fatalf("taint data path: %v", err)
	}
	if len(taintItems) != len(taintLegacy) {
		t.Fatalf("taint reading counts differ: items=%d data=%d", len(taintItems), len(taintLegacy))
	}
	for i := range taintItems {
		if taintItems[i].Key != taintLegacy[i].Key || taintItems[i].Effect != taintLegacy[i].Effect {
			t.Errorf("taint %d: items={%q %q} data={%q %q}",
				i, taintItems[i].Key, taintItems[i].Effect, taintLegacy[i].Key, taintLegacy[i].Effect)
		}
	}
}

// TestRawKeyMatchesDataMapKey pins that RawKey reproduces the legacy map key,
// so a consumer quoting it in a diagnostic emits the same bytes on both paths.
func TestRawKeyMatchesDataMapKey(t *testing.T) {
	labelSt, taintSt := collectSubtypes(t, 0,
		makeNode("node-1",
			[]corev1.Taint{
				{Key: "dedicated", Value: "sys", Effect: corev1.TaintEffectNoSchedule},
				{Key: "dedicated", Value: "sys", Effect: corev1.TaintEffectNoExecute},
			},
			map[string]string{"zone": "us-west", "kubernetes.io/arch": "arm64"},
		),
		makeNode("node-2", nil, map[string]string{"zone": "us-east"}),
	)

	readings, err := LabelReadings(labelSt)
	if err != nil {
		t.Fatalf("LabelReadings(): %v", err)
	}
	for _, r := range readings {
		if _, ok := labelSt.Data[r.RawKey]; !ok {
			t.Errorf("label RawKey %q is not a key in the Data map (keys: %v)",
				r.RawKey, sortedKeysOf(labelSt.Data))
		}
	}

	taints, err := TaintReadings(taintSt)
	if err != nil {
		t.Fatalf("TaintReadings(): %v", err)
	}
	for _, r := range taints {
		if _, ok := taintSt.Data[r.RawKey]; !ok {
			t.Errorf("taint RawKey %q is not a key in the Data map (keys: %v)",
				r.RawKey, sortedKeysOf(taintSt.Data))
		}
	}
}

// TestReadingsTruncation pins that a capped node list reports the true total
// and says it is truncated, on both paths. Consumers that must not act on
// partial membership (issue #1755) depend on this.
func TestReadingsTruncation(t *testing.T) {
	labelSt, _ := collectSubtypes(t, 1,
		makeNode("node-1", nil, map[string]string{"shared": "yes"}),
		makeNode("node-2", nil, map[string]string{"shared": "yes"}),
		makeNode("node-3", nil, map[string]string{"shared": "yes"}),
	)

	readings, err := LabelReadings(labelSt)
	if err != nil {
		t.Fatalf("LabelReadings(): %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("got %d readings, want 1", len(readings))
	}
	if !readings[0].Truncated {
		t.Error("Truncated = false, want true under --max-nodes-per-entry=1")
	}
	if readings[0].NodeCount != 3 {
		t.Errorf("NodeCount = %d, want 3 (the pre-truncation total)", readings[0].NodeCount)
	}

	// The legacy path can only infer truncation from the rendered suffix, and
	// cannot recover the true count — it reports what it can see.
	legacy, err := LabelReadings(dataOnly(labelSt))
	if err != nil {
		t.Fatalf("legacy LabelReadings(): %v", err)
	}
	if !legacy[0].Truncated {
		t.Error("legacy Truncated = false, want true (suffix detection)")
	}
}

// validItem is a well-formed item for both decoders. The tables below mutate
// one field of it, so each case fails for the reason it names.
func validItem() measurement.ItemEntry {
	return measurement.ItemEntry{
		Context: map[string]string{
			itemCtxKey:    "k",
			itemCtxValue:  "v",
			itemCtxEffect: "NoSchedule",
		},
		Data: map[string]measurement.Reading{
			itemDataNodeCount: measurement.Int(2),
			itemDataNodeList:  measurement.Str("n1,n2"),
			itemDataTruncated: measurement.Bool(false),
		},
	}
}

// TestReadingsMalformedItems pins that a structurally broken item is rejected
// rather than decoded into a partial reading a caller might trust.
func TestReadingsMalformedItems(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*measurement.ItemEntry)
	}{
		{
			name:   "missing key",
			mutate: func(i *measurement.ItemEntry) { delete(i.Context, itemCtxKey) },
		},
		{
			name:   "empty key",
			mutate: func(i *measurement.ItemEntry) { i.Context[itemCtxKey] = "" },
		},
		{
			name:   "missing node-list",
			mutate: func(i *measurement.ItemEntry) { delete(i.Data, itemDataNodeList) },
		},
		{
			name:   "node-list is not a string",
			mutate: func(i *measurement.ItemEntry) { i.Data[itemDataNodeList] = measurement.Int(3) },
		},
		{
			name:   "missing node-count",
			mutate: func(i *measurement.ItemEntry) { delete(i.Data, itemDataNodeCount) },
		},
		{
			name:   "node-count is not an integer",
			mutate: func(i *measurement.ItemEntry) { i.Data[itemDataNodeCount] = measurement.Str("2") },
		},
		{
			name:   "node-count is negative",
			mutate: func(i *measurement.ItemEntry) { i.Data[itemDataNodeCount] = measurement.Int(-1) },
		},
		{
			name:   "node-count is below the named nodes",
			mutate: func(i *measurement.ItemEntry) { i.Data[itemDataNodeCount] = measurement.Int(1) },
		},
		{
			name:   "node-count exceeds a complete list",
			mutate: func(i *measurement.ItemEntry) { i.Data[itemDataNodeCount] = measurement.Int(40) },
		},
		{
			name: "truncated list whose count does not exceed the names",
			mutate: func(i *measurement.ItemEntry) {
				i.Data[itemDataNodeList] = measurement.Str("n1,n2 (+3 more)")
				i.Data[itemDataTruncated] = measurement.Bool(true)
			},
		},
		{
			name:   "missing truncated",
			mutate: func(i *measurement.ItemEntry) { delete(i.Data, itemDataTruncated) },
		},
		{
			name:   "truncated is not a boolean",
			mutate: func(i *measurement.ItemEntry) { i.Data[itemDataTruncated] = measurement.Str("false") },
		},
		{
			name: "truncated is false against a truncated list",
			mutate: func(i *measurement.ItemEntry) {
				i.Data[itemDataNodeList] = measurement.Str("n1,n2 (+3 more)")
				i.Data[itemDataNodeCount] = measurement.Int(5)
			},
		},
		{
			name: "truncated is true against a complete list",
			mutate: func(i *measurement.ItemEntry) {
				i.Data[itemDataTruncated] = measurement.Bool(true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := validItem()
			tt.mutate(&item)
			st := &measurement.Subtype{Name: "label", Items: []measurement.ItemEntry{item}}
			if _, err := LabelReadings(st); err == nil {
				t.Error("LabelReadings() error = nil, want a decode error")
			}
			if _, err := TaintReadings(st); err == nil {
				t.Error("TaintReadings() error = nil, want a decode error")
			}
		})
	}
}

// TestTaintReadingsRequireEffect pins that effect is mandatory for taints and
// ignored for labels.
func TestTaintReadingsRequireEffect(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*measurement.ItemEntry)
	}{
		{"missing effect", func(i *measurement.ItemEntry) { delete(i.Context, itemCtxEffect) }},
		{"empty effect", func(i *measurement.ItemEntry) { i.Context[itemCtxEffect] = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := validItem()
			tt.mutate(&item)
			st := &measurement.Subtype{Name: "taint", Items: []measurement.ItemEntry{item}}
			if _, err := TaintReadings(st); err == nil {
				t.Error("TaintReadings() error = nil, want a decode error")
			}
			if _, err := LabelReadings(st); err != nil {
				t.Errorf("LabelReadings() error = %v, want nil — labels have no effect", err)
			}
		})
	}
}

// TestTaintReadingsAcceptUnknownEffect pins that the decoder does not gate on
// the effects Kubernetes defines today, so a newer cluster stays readable.
func TestTaintReadingsAcceptUnknownEffect(t *testing.T) {
	item := validItem()
	item.Context[itemCtxEffect] = "SomeFutureEffect"

	readings, err := TaintReadings(&measurement.Subtype{
		Name:  "taint",
		Items: []measurement.ItemEntry{item},
	})
	if err != nil {
		t.Fatalf("TaintReadings() error = %v, want nil", err)
	}
	if readings[0].Effect != "SomeFutureEffect" {
		t.Errorf("Effect = %q, want it preserved verbatim", readings[0].Effect)
	}
}

// TestReadingsNilAndEmpty pins that absent topology data is not a decode
// failure — callers distinguish "no readings" from "bad readings".
func TestReadingsNilAndEmpty(t *testing.T) {
	for _, st := range []*measurement.Subtype{
		nil,
		{Name: "label"},
		{Name: "label", Data: map[string]measurement.Reading{}},
	} {
		labels, err := LabelReadings(st)
		if err != nil {
			t.Errorf("LabelReadings(%v) error = %v, want nil", st, err)
		}
		if len(labels) != 0 {
			t.Errorf("LabelReadings(%v) = %d readings, want 0", st, len(labels))
		}
		taints, err := TaintReadings(st)
		if err != nil {
			t.Errorf("TaintReadings(%v) error = %v, want nil", st, err)
		}
		if len(taints) != 0 {
			t.Errorf("TaintReadings(%v) = %d readings, want 0", st, len(taints))
		}
	}
}

// TestHasLosslessReadings pins the discriminator consumers use to decide
// whether a legacy-only heuristic still applies.
func TestHasLosslessReadings(t *testing.T) {
	labelSt, _ := collectSubtypes(t, 0,
		makeNode("node-1", nil, map[string]string{"zone": "us-west"}))

	if !HasLosslessReadings(labelSt) {
		t.Error("HasLosslessReadings(collector output) = false, want true")
	}
	if HasLosslessReadings(dataOnly(labelSt)) {
		t.Error("HasLosslessReadings(legacy Data-only) = true, want false")
	}
	if HasLosslessReadings(nil) {
		t.Error("HasLosslessReadings(nil) = true, want false")
	}
}

// TestItemsAreDeterministic pins the emission order. pkg/diff compares Items
// positionally, so an unstable order would surface as phantom drift between
// two runs against an unchanged cluster.
func TestItemsAreDeterministic(t *testing.T) {
	nodes := []*corev1.Node{
		makeNode("node-1",
			[]corev1.Taint{
				{Key: "b", Value: "1", Effect: corev1.TaintEffectNoSchedule},
				{Key: "a", Value: "2", Effect: corev1.TaintEffectNoExecute},
			},
			map[string]string{"z": "1", "a": "2", "m": "3"},
		),
		makeNode("node-2", nil, map[string]string{"z": "9", "a": "2"}),
	}

	var first string
	for i := 0; i < 50; i++ {
		labelSt, taintSt := collectSubtypes(t, 0, nodes...)
		got := fmt.Sprintf("%v|%v", labelSt.Items, taintSt.Items)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("Items order is not deterministic on run %d:\n first: %s\n got:   %s", i, first, got)
		}
	}
}

func sortedKeysOf(data map[string]measurement.Reading) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
	stderrors "errors"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	corev1 "k8s.io/api/core/v1"
)

// assertDecodeRejected fails unless err carries the code the exported
// accessors promise for a structurally invalid item. Callers branch on the
// code, not the message, so asserting only err != nil would let the contract
// change silently.
func assertDecodeRejected(t *testing.T, err error, accessor string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s error = nil, want a decode error", accessor)
		return
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("%s error = %v, want code %s", accessor, err, errors.ErrCodeInvalidRequest)
	}
}

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
		// Names, not just the count: a collision's characteristic failure is
		// the right number of nodes attributed to the wrong reading.
		if !slices.Equal(got.Nodes, w.Nodes) {
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

	// The marker states how many names were dropped, so the legacy path
	// recovers the same total: NodeCount means one thing regardless of which
	// encoding a snapshot carries.
	legacy, err := LabelReadings(dataOnly(labelSt))
	if err != nil {
		t.Fatalf("legacy LabelReadings(): %v", err)
	}
	if !legacy[0].Truncated {
		t.Error("legacy Truncated = false, want true (suffix detection)")
	}
	if legacy[0].NodeCount != readings[0].NodeCount {
		t.Errorf("legacy NodeCount = %d, want %d — the two encodings must agree",
			legacy[0].NodeCount, readings[0].NodeCount)
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

// refSubtype is a well-formed subtype whose single item references a data entry
// instead of carrying node names inline.
func refSubtype(name, key, value, entry string, count int) *measurement.Subtype {
	ctx := map[string]string{itemCtxKey: key, itemCtxValue: value}
	if name == "taint" {
		ctx[itemCtxEffect] = "NoSchedule"
	}
	return &measurement.Subtype{
		Name: name,
		Data: map[string]measurement.Reading{key: measurement.Str(entry)},
		Items: []measurement.ItemEntry{{
			Context: ctx,
			Data: map[string]measurement.Reading{
				itemDataNodeCount: measurement.Int(count),
				itemDataNodeRef:   measurement.Str(key),
				itemDataTruncated: measurement.Bool(false),
			},
		}},
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
			// "n1,n2 (+3 more)" states 2 + 3 = 5. A count above the names but
			// below that total passes a direction check and fails an equality
			// one — the case a ">" rule cannot see.
			name: "truncated node-count below the total the suffix states",
			mutate: func(i *measurement.ItemEntry) {
				i.Data[itemDataNodeList] = measurement.Str("n1,n2 (+3 more)")
				i.Data[itemDataTruncated] = measurement.Bool(true)
				i.Data[itemDataNodeCount] = measurement.Int(3)
			},
		},
		{
			name: "truncated node-count above the total the suffix states",
			mutate: func(i *measurement.ItemEntry) {
				i.Data[itemDataNodeList] = measurement.Str("n1,n2 (+3 more)")
				i.Data[itemDataTruncated] = measurement.Bool(true)
				i.Data[itemDataNodeCount] = measurement.Int(900)
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
			_, labelErr := LabelReadings(st)
			assertDecodeRejected(t, labelErr, "LabelReadings()")
			_, taintErr := TaintReadings(st)
			assertDecodeRejected(t, taintErr, "TaintReadings()")
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
			_, taintErr := TaintReadings(st)
			assertDecodeRejected(t, taintErr, "TaintReadings()")
			if _, err := LabelReadings(st); err != nil {
				t.Errorf("LabelReadings() error = %v, want nil — labels have no effect", err)
			}
		})
	}
}

// TestTaintDataPathRejectsUnrecoverableEffect is the folded-encoding half of
// TestTaintReadingsRequireEffect. encodeTaints writes the two-field form only
// under a "<key>.<effect>" map key and the three-field form otherwise, so none
// of these can come from this collector — but a snapshot is a file a caller
// supplies, and each shape below leaves no effect to recover. Decoding one
// would hand back Effect:"" and satisfy a caller matching on one.
func TestTaintDataPathRejectsUnrecoverableEffect(t *testing.T) {
	for _, tt := range []struct {
		name, key, entry string
	}{
		{"one field, no separator", "dedicated", "NoSchedule"},
		{"one field, empty", "dedicated", ""},
		{"two fields, key carries no effect suffix", "dedicated", "sys|gpu-a"},
		{"two fields, key ends in the separator", "dedicated.", "sys|gpu-a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TaintReadings(&measurement.Subtype{
				Name: "taint",
				Data: map[string]measurement.Reading{tt.key: measurement.Str(tt.entry)},
			})
			assertDecodeRejected(t, err, "TaintReadings() [data path]")
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

// TestReadingsResolveReference pins the cross-reference round trip: an item
// naming a data entry decodes to the same reading an inline copy would.
func TestReadingsResolveReference(t *testing.T) {
	st := refSubtype("label", "zone", "us-west", "us-west|gpu-a,gpu-b", 2)
	readings, err := LabelReadings(st)
	if err != nil {
		t.Fatalf("LabelReadings() error = %v", err)
	}
	if len(readings) != 1 {
		t.Fatalf("got %d readings, want 1", len(readings))
	}
	if got := readings[0].Nodes; !slices.Equal(got, []string{"gpu-a", "gpu-b"}) {
		t.Errorf("Nodes = %v, want [gpu-a gpu-b] resolved from the data entry", got)
	}
	if readings[0].NodeCount != 2 {
		t.Errorf("NodeCount = %d, want 2", readings[0].NodeCount)
	}
}

// TestReadingsRejectMalformedReference pins the guards on the reference form.
// A reference that resolves to another reading's nodes would be well formed
// and wrong, which is the failure this encoding exists to remove.
func TestReadingsRejectMalformedReference(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*measurement.Subtype)
	}{
		{
			name: "reference names no data entry",
			mutate: func(st *measurement.Subtype) {
				st.Data = map[string]measurement.Reading{}
			},
		},
		{
			name: "reference names an entry this reading does not fold onto",
			mutate: func(st *measurement.Subtype) {
				st.Data["elsewhere"] = measurement.Str("other|gpu-z")
				st.Items[0].Data[itemDataNodeRef] = measurement.Str("elsewhere")
			},
		},
		{
			name: "reference is not a string",
			mutate: func(st *measurement.Subtype) {
				st.Items[0].Data[itemDataNodeRef] = measurement.Int(3)
			},
		},
		{
			name: "carries both a list and a reference",
			mutate: func(st *measurement.Subtype) {
				st.Items[0].Data[itemDataNodeList] = measurement.Str("gpu-a,gpu-b")
			},
		},
		{
			name: "carries neither a list nor a reference",
			mutate: func(st *measurement.Subtype) {
				delete(st.Items[0].Data, itemDataNodeRef)
			},
		},
		{
			name: "node-count disagrees with the referenced entry",
			mutate: func(st *measurement.Subtype) {
				st.Items[0].Data[itemDataNodeCount] = measurement.Int(9)
			},
		},
		{
			// Two readings produce the same fold key (zone=us-west with a sibling
			// zone=us-east makes applyLabelRawKeys suffix both to zone.us-west /
			// zone.us-east; the label literally named zone.us-west also gets
			// rawKey=zone.us-west). That makes folds["zone.us-west"]==2, so the
			// reference on item[0] is rejected even though ref==rawKey.
			name: "reference fold key is shared with another reading",
			mutate: func(st *measurement.Subtype) {
				// Three items: zone=us-west (ref), zone=us-east (inline),
				// zone.us-west=true (inline). applyLabelRawKeys sees two zone
				// readings, so zone=us-west → rawKey "zone.us-west", same as
				// the literal key. folds["zone.us-west"]==2 → rejected.
				*st = measurement.Subtype{
					Name: "label",
					Data: map[string]measurement.Reading{
						"zone.us-west": measurement.Str("us-west|gpu-a,gpu-b"),
						"zone.us-east": measurement.Str("us-east|gpu-b"),
					},
					Items: []measurement.ItemEntry{
						{
							Context: map[string]string{itemCtxKey: "zone", itemCtxValue: "us-west"},
							Data: map[string]measurement.Reading{
								itemDataNodeCount: measurement.Int(2),
								itemDataNodeRef:   measurement.Str("zone.us-west"),
								itemDataTruncated: measurement.Bool(false),
							},
						},
						{
							Context: map[string]string{itemCtxKey: "zone", itemCtxValue: "us-east"},
							Data: map[string]measurement.Reading{
								itemDataNodeCount: measurement.Int(1),
								itemDataNodeList:  measurement.Str("gpu-b"),
								itemDataTruncated: measurement.Bool(false),
							},
						},
						{
							Context: map[string]string{itemCtxKey: "zone.us-west", itemCtxValue: "true"},
							Data: map[string]measurement.Reading{
								itemDataNodeCount: measurement.Int(2),
								itemDataNodeList:  measurement.Str("gpu-a,gpu-b"),
								itemDataTruncated: measurement.Bool(false),
							},
						},
					},
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := refSubtype("label", "zone", "us-west", "us-west|gpu-a,gpu-b", 2)
			tt.mutate(st)
			_, err := LabelReadings(st)
			assertDecodeRejected(t, err, "LabelReadings()")
		})
	}
}

// TestReadingsRejectUnusableTruncationMarker pins that a marker whose count
// does not fit an int fails the decode. Reporting "no marker" would let a
// visibly truncated list read as complete.
func TestReadingsRejectUnusableTruncationMarker(t *testing.T) {
	item := validItem()
	item.Data[itemDataNodeList] = measurement.Str("n1 (+9223372036854775808 more)")
	item.Data[itemDataNodeCount] = measurement.Int(1)
	item.Data[itemDataTruncated] = measurement.Bool(false)

	st := &measurement.Subtype{Name: "label", Items: []measurement.ItemEntry{item}}
	_, err := LabelReadings(st)
	assertDecodeRejected(t, err, "LabelReadings()")

	// The same shape on the folded path must fail too.
	_, err = LabelReadings(&measurement.Subtype{
		Name: "label",
		Data: map[string]measurement.Reading{"zone": measurement.Str("us-west|n1 (+9223372036854775808 more)")},
	})
	assertDecodeRejected(t, err, "LabelReadings() [data path]")
}

// TestTaintDataPathReportsLogicalKey pins that the folded taint decoder
// reports the taint's real key rather than the synthesized "<key>.<effect>".
// The item path reports the logical key, and two decoders disagreeing on
// identity would surface as a phantom diff or a missed constraint match.
func TestTaintDataPathReportsLogicalKey(t *testing.T) {
	_, taintSt := collectSubtypes(t, 0,
		makeNode("gpu-a", []corev1.Taint{
			{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoSchedule},
			{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute},
		}, nil),
	)

	legacy, err := TaintReadings(dataOnly(taintSt))
	if err != nil {
		t.Fatalf("TaintReadings() error = %v", err)
	}
	for _, r := range legacy {
		if r.Key != "node.kubernetes.io/unreachable" {
			t.Errorf("Key = %q, want the logical key without the effect suffix", r.Key)
		}
		if r.RawKey == r.Key {
			t.Errorf("RawKey = %q, want the folded map key", r.RawKey)
		}
	}
}

// TestHydrateItems pins the helper pkg/diff uses to compare membership across
// encodings: referenced lists are resolved inline so a caller sees node names
// regardless of how the snapshot stored them, and keys carrying more than one
// reading are reported as unusable for comparison.
func TestHydrateItems(t *testing.T) {
	t.Run("resolves a reference into an inline list", func(t *testing.T) {
		st := refSubtype("label", "zone", "us-west", "us-west|gpu-a,gpu-b", 2)
		items, ambiguous, err := HydrateItems(st)
		if err != nil {
			t.Fatalf("HydrateItems() error = %v", err)
		}
		if got := items[0].Data[itemDataNodeList].String(); got != "gpu-a,gpu-b" {
			t.Errorf("node-list = %q, want the resolved names", got)
		}
		if _, still := items[0].Data[itemDataNodeRef]; still {
			t.Error("hydrated item still carries the reference")
		}
		if len(ambiguous) != 0 {
			t.Errorf("ambiguous = %v, want none", ambiguous)
		}
	})

	t.Run("reports keys carrying more than one reading", func(t *testing.T) {
		labelSt, _ := collectSubtypes(t, 0,
			makeNode("gpu-a", nil, map[string]string{"zone": "us-west", "zone.us-west": "true"}),
			makeNode("gpu-b", nil, map[string]string{"zone": "us-east", "zone.us-west": "true"}),
		)
		_, ambiguous, err := HydrateItems(labelSt)
		if err != nil {
			t.Fatalf("HydrateItems() error = %v", err)
		}
		if !ambiguous["zone.us-west"] {
			t.Errorf("ambiguous = %v, want zone.us-west — two readings fold onto it", ambiguous)
		}
	})

	t.Run("round-trips a truncated list", func(t *testing.T) {
		labelSt, _ := collectSubtypes(t, 1,
			makeNode("n1", nil, map[string]string{"shared": "yes"}),
			makeNode("n2", nil, map[string]string{"shared": "yes"}),
			makeNode("n3", nil, map[string]string{"shared": "yes"}),
		)
		items, _, err := HydrateItems(labelSt)
		if err != nil {
			t.Fatalf("HydrateItems() error = %v", err)
		}
		if got := items[0].Data[itemDataNodeList].String(); got != "n1 (+2 more)" {
			t.Errorf("node-list = %q, want the marker preserved", got)
		}
	})

	t.Run("taints hydrate too", func(t *testing.T) {
		st := refSubtype("taint", "dedicated", "sys", "NoSchedule|sys|gpu-a", 1)
		items, _, err := HydrateItems(st)
		if err != nil {
			t.Fatalf("HydrateItems() error = %v", err)
		}
		if got := items[0].Data[itemDataNodeList].String(); got != "gpu-a" {
			t.Errorf("node-list = %q, want gpu-a", got)
		}
	})

	t.Run("absent, foreign and malformed inputs", func(t *testing.T) {
		if items, _, err := HydrateItems(nil); items != nil || err != nil {
			t.Errorf("nil subtype = (%v, %v), want (nil, nil)", items, err)
		}
		if items, _, err := HydrateItems(&measurement.Subtype{Name: "label"}); items != nil || err != nil {
			t.Errorf("no items = (%v, %v), want (nil, nil)", items, err)
		}
		other := &measurement.Subtype{Name: "summary", Items: []measurement.ItemEntry{validItem()}}
		if items, _, err := HydrateItems(other); err != nil || len(items) != 1 {
			t.Errorf("foreign subtype = (%v, %v), want its items unchanged", items, err)
		}
		bad := &measurement.Subtype{Name: "label", Items: []measurement.ItemEntry{{
			Context: map[string]string{itemCtxKey: "k"},
			Data:    map[string]measurement.Reading{itemDataNodeList: measurement.Str("n1")},
		}}}
		if _, _, err := HydrateItems(bad); err == nil {
			t.Error("malformed items decoded without error")
		}
	})
}

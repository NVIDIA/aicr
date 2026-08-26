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

package diff

import (
	"maps"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// topologySnap builds a NodeTopology measurement in one of the two encodings.
// withItems mirrors what a current build writes — both halves, with the
// summary count sized off the items. Without it the shape is what a build
// predating the item encoding wrote: the folded map only, counted by its size.
func topologySnap(labels map[string]string, items []measurement.ItemEntry, withItems bool) *snapshotter.Snapshot {
	data := make(map[string]measurement.Reading, len(labels))
	for k, v := range labels {
		data[k] = measurement.Str(v)
	}
	labelSt := measurement.Subtype{Name: "label", Data: data}
	count := len(data)
	if withItems {
		labelSt.Items = items
		count = len(items)
	}
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{{
			Type: measurement.TypeNodeTopology,
			Subtypes: []measurement.Subtype{
				{Name: "summary", Data: map[string]measurement.Reading{
					"node-count":  measurement.Int(2),
					"label-count": measurement.Int(count),
				}},
				labelSt,
			},
		}},
	}
}

func labelItem(key, value, nodes string) measurement.ItemEntry {
	names := 0
	if nodes != "" {
		names = len(strings.Split(nodes, ","))
	}
	return measurement.ItemEntry{
		Context: map[string]string{"key": key, "value": value},
		Data: map[string]measurement.Reading{
			"node-count": measurement.Int(names),
			"node-list":  measurement.Str(nodes),
			"truncated":  measurement.Bool(false),
		},
	}
}

// refLabelItem builds a label item that references its node list from the data
// map rather than carrying it inline. dataKey must be the folded key that
// encodeLabels would produce for this reading, and must not be shared with any
// other reading (folds == 1), or the decoder will reject the reference.
func refLabelItem(key, value, dataKey string, count int) measurement.ItemEntry {
	return measurement.ItemEntry{
		Context: map[string]string{"key": key, "value": value},
		Data: map[string]measurement.Reading{
			"node-count":    measurement.Int(count),
			"node-list-ref": measurement.Str(dataKey),
			"truncated":     measurement.Bool(false),
		},
	}
}

// A cluster whose "zone" label carries two values, plus a distinct label named
// zone.us-west. The folded encoding collides those onto one key and reports
// two entries where there are three readings, so both halves differ between
// the vintages — the count as well as the presence of items.
func collidingCluster() (map[string]string, []measurement.ItemEntry) {
	folded := map[string]string{
		"zone.us-west": "true|gpu-a,gpu-b",
		"zone.us-east": "us-east|gpu-b",
	}
	items := []measurement.ItemEntry{
		labelItem("zone", "us-east", "gpu-b"),
		labelItem("zone", "us-west", "gpu-a"),
		labelItem("zone.us-west", "true", "gpu-a,gpu-b"),
	}
	return folded, items
}

// TestSnapshots_UpgradeIsNotDrift pins that capturing a baseline with an older
// aicr and a target with a current one reports no change for an unchanged
// cluster. Without this, every drift gate in CI fails the morning after an
// upgrade: compareItems sees zero items against N and reports every field of
// every item as added, and the summary counts disagree because the older build
// sized them off the folded map.
func TestSnapshots_UpgradeIsNotDrift(t *testing.T) {
	labels, items := collidingCluster()

	for _, tt := range []struct {
		name            string
		labels          map[string]string
		items           []measurement.ItemEntry
		baseHas, tgtHas bool
	}{
		{"upgrade", labels, items, false, true},
		// A rollback is the same problem mirrored, and must be as quiet.
		{"rollback", labels, items, true, false},
		{"same old version", labels, items, false, false},
		{"same new version", labels, items, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := Snapshots(
				topologySnap(tt.labels, tt.items, tt.baseHas),
				topologySnap(tt.labels, tt.items, tt.tgtHas),
			)
			if result.HasDrift() {
				t.Errorf("unchanged cluster reports drift across encodings: %v", paths(result))
			}
		})
	}
}

// TestSnapshots_RealChangeSurvivesAlignment is the other half: suppressing the
// encoding difference must not suppress the cluster difference. A gate that
// never fires is worse than one that fires spuriously.
func TestSnapshots_RealChangeSurvivesAlignment(t *testing.T) {
	labels, items := collidingCluster()

	changedLabels := map[string]string{}
	maps.Copy(changedLabels, labels)
	changedLabels["accelerator"] = "h100|gpu-a,gpu-b"
	changedItems := append(append([]measurement.ItemEntry{}, items...),
		labelItem("accelerator", "h100", "gpu-a,gpu-b"))

	for _, tt := range []struct {
		name            string
		baseHas, tgtHas bool
	}{
		{"both current", true, true},
		{"baseline predates items", false, true},
		{"target predates items", true, false},
		{"both predate items", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := Snapshots(
				topologySnap(labels, items, tt.baseHas),
				topologySnap(changedLabels, changedItems, tt.tgtHas),
			)
			if !result.HasDrift() {
				t.Error("a new label was not reported as drift")
			}
		})
	}
}

// TestSnapshots_AlignmentDoesNotMutateInputs pins that diffing is read-only.
// Callers reuse snapshots after comparing them — aicr diff renders the target
// afterwards — so dropping a half for comparison must not drop it for them.
func TestSnapshots_AlignmentDoesNotMutateInputs(t *testing.T) {
	labels, items := collidingCluster()
	base := topologySnap(labels, items, false)
	target := topologySnap(labels, items, true)

	_ = Snapshots(base, target)

	tgtLabel := target.Measurements[0].Subtypes[1]
	if len(tgtLabel.Items) != len(items) {
		t.Errorf("target lost %d items to the comparison", len(items)-len(tgtLabel.Items))
	}
	if len(tgtLabel.Data) != len(labels) {
		t.Errorf("target lost data entries to the comparison")
	}
	if got := target.Measurements[0].Subtypes[0].Data["label-count"]; got.String() != "3" {
		t.Errorf("target label-count = %s, want 3 — the restated value leaked into the input", got.String())
	}
}

// TestSnapshots_AlignmentIgnoresOtherMeasurements pins that the rule is scoped
// to NodeTopology. Other measurement types also carry items, and a one-sided
// item list there is a real difference rather than an encoding artifact.
func TestSnapshots_AlignmentIgnoresOtherMeasurements(t *testing.T) {
	withItems := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{{
			Type: measurement.Type("Network"),
			Subtypes: []measurement.Subtype{{
				Name:  "PF",
				Items: []measurement.ItemEntry{labelItem("name", "pf0", "")},
			}},
		}},
	}
	without := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{{
			Type:     measurement.Type("Network"),
			Subtypes: []measurement.Subtype{{Name: "PF"}},
		}},
	}
	if !Snapshots(without, withItems).HasDrift() {
		t.Error("a one-sided item list outside NodeTopology must still report drift")
	}
}

// TestSnapshots_LegacyPairCountIsNotRestated pins that two snapshots of one
// vintage are compared as written. There is no encoding mismatch to reconcile,
// so a summary count that disagrees with the map is a real difference and must
// be reported rather than normalized away.
func TestSnapshots_LegacyPairCountIsNotRestated(t *testing.T) {
	labels, items := collidingCluster()
	base := topologySnap(labels, items, false)
	target := topologySnap(labels, items, false)
	target.Measurements[0].Subtypes[0].Data["label-count"] = measurement.Int(99)

	result := Snapshots(base, target)
	if !result.HasDrift() {
		t.Error("a corrupted label-count between two legacy snapshots was not reported")
	}
}

// TestSnapshots_ItemsAuthoritativeOverData pins the other half of the rule:
// when both sides carry items, the folded map is not compared at all. It is
// not merely redundant — encodeLabels resolves a key collision by Go map
// iteration order, so two runs of one build against an unchanged cluster can
// write different maps. Comparing it would report drift that no cluster
// change caused.
func TestSnapshots_ItemsAuthoritativeOverData(t *testing.T) {
	_, items := collidingCluster()

	withData := func(data map[string]string) *snapshotter.Snapshot {
		readings := make(map[string]measurement.Reading, len(data))
		for k, v := range data {
			readings[k] = measurement.Str(v)
		}
		return &snapshotter.Snapshot{
			Measurements: []*measurement.Measurement{{
				Type: measurement.TypeNodeTopology,
				Subtypes: []measurement.Subtype{
					{Name: "summary", Data: map[string]measurement.Reading{
						"label-count": measurement.Int(len(items)),
					}},
					{Name: "label", Data: readings, Items: items},
				},
			}},
		}
	}

	// Same three readings both times; only which one won the folded key differs.
	base := withData(map[string]string{
		"zone.us-west": "true|gpu-a,gpu-b",
		"zone.us-east": "us-east|gpu-b",
	})
	target := withData(map[string]string{
		"zone.us-west": "us-west|gpu-a",
		"zone.us-east": "us-east|gpu-b",
	})

	if result := Snapshots(base, target); result.HasDrift() {
		t.Errorf("collision flapping in the folded map was reported as drift: %v", paths(result))
	}

	// The converse: a genuine item difference must still surface.
	changed := withData(map[string]string{
		"zone.us-west": "true|gpu-a,gpu-b",
		"zone.us-east": "us-east|gpu-b",
	})
	changed.Measurements[0].Subtypes[1].Items = append(
		append([]measurement.ItemEntry{}, items...),
		labelItem("accelerator", "h100", "gpu-a"))
	if !Snapshots(base, changed).HasDrift() {
		t.Error("an added item was not reported as drift")
	}
}

// TestSnapshots_MixedVintageCollisionWinnerIsNotDrift pins that an upgrade or
// rollback of an unchanged collision cluster is silent even when the two
// snapshots captured different collision winners for the same key.
func TestSnapshots_MixedVintageCollisionWinnerIsNotDrift(t *testing.T) {
	labels, items := collidingCluster()

	// Same cluster; the other reading won the contested zone.us-west key.
	otherWinner := make(map[string]string, len(labels))
	maps.Copy(otherWinner, labels)
	otherWinner["zone.us-west"] = "us-west|gpu-a"

	for _, tt := range []struct {
		name            string
		baseHas, tgtHas bool
	}{
		{"upgrade", false, true},
		{"rollback", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := Snapshots(
				topologySnap(otherWinner, items, tt.baseHas),
				topologySnap(labels, items, tt.tgtHas),
			)
			if result.HasDrift() {
				t.Errorf("collision winner differing across vintages reported as drift: %v", paths(result))
			}
		})
	}
}

// TestSnapshots_HydratedItemsCompareMembership pins that node membership is
// still compared when both sides carry items. Referenced lists must be
// resolved first: comparing raw items would compare a reference string, so a
// node moving between labels would go unreported.
func TestSnapshots_HydratedItemsCompareMembership(t *testing.T) {
	labels, items := collidingCluster()

	moved := make([]measurement.ItemEntry, len(items))
	copy(moved, items)
	for i := range moved {
		if moved[i].Context["value"] == "us-east" {
			moved[i] = labelItem("zone", "us-east", "gpu-c")
		}
	}

	result := Snapshots(topologySnap(labels, items, true), topologySnap(labels, moved, true))
	if !result.HasDrift() {
		t.Error("a node moving between readings was not reported as drift")
	}
}

// TestSnapshots_HydrationResolvesReferences pins that node-list-ref items are
// resolved before comparison so that node membership changes are detected.
func TestSnapshots_HydrationResolvesReferences(t *testing.T) {
	// Single-value key: fold key == key, folds["accelerator"]==1, so the
	// encoder emits node-list-ref rather than node-list.
	dataKey := "accelerator"
	data := map[string]string{dataKey: "h100|gpu-a,gpu-b"}

	refItem := refLabelItem("accelerator", "h100", dataKey, 2)
	unchanged := []measurement.ItemEntry{refItem}

	changedData := map[string]string{dataKey: "h100|gpu-a"}
	changedItem := refLabelItem("accelerator", "h100", dataKey, 1)
	changed := []measurement.ItemEntry{changedItem}

	buildSnap := func(d map[string]string, its []measurement.ItemEntry) *snapshotter.Snapshot {
		readings := make(map[string]measurement.Reading, len(d))
		for k, v := range d {
			readings[k] = measurement.Str(v)
		}
		return &snapshotter.Snapshot{
			Measurements: []*measurement.Measurement{{
				Type: measurement.TypeNodeTopology,
				Subtypes: []measurement.Subtype{
					{Name: "summary", Data: map[string]measurement.Reading{
						"label-count": measurement.Int(len(its)),
					}},
					{Name: "label", Data: readings, Items: its},
				},
			}},
		}
	}

	if result := Snapshots(buildSnap(data, unchanged), buildSnap(data, unchanged)); result.HasDrift() {
		t.Errorf("identical reference items reported as drift: %v", paths(result))
	}
	if result := Snapshots(buildSnap(data, unchanged), buildSnap(changedData, changed)); !result.HasDrift() {
		t.Error("a node leaving a reference item was not reported as drift")
	}
}

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
	return measurement.ItemEntry{
		Context: map[string]string{"key": key, "value": value},
		Data:    map[string]measurement.Reading{"node-list": measurement.Str(nodes)},
	}
}

// A cluster whose "zone" label carries two values, plus a distinct label named
// zone.us-west. The folded encoding collides those onto one key and reports
// two entries where there are three readings, so both halves differ between
// the vintages — the count as well as the presence of items.
func collidingCluster() (map[string]string, []measurement.ItemEntry) {
	return map[string]string{
			"zone.us-west": "true|gpu-a,gpu-b",
			"zone.us-east": "us-east|gpu-b",
		}, []measurement.ItemEntry{
			labelItem("zone", "us-east", "gpu-b"),
			labelItem("zone", "us-west", "gpu-a"),
			labelItem("zone.us-west", "true", "gpu-a,gpu-b"),
		}
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
	for k, v := range labels {
		changedLabels[k] = v
	}
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

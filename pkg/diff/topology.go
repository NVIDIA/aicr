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
	"github.com/NVIDIA/aicr/pkg/collector/topology"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// NodeTopology carries every label and taint reading twice: as items, and as
// the legacy folded data map. Diffing both halves makes an aicr upgrade look
// like cluster drift, because a snapshot captured by an older build has no
// items at all and every field of every item reads as added.
const (
	topologySummarySubtype = "summary"
	topologyLabelSubtype   = "label"
	topologyTaintSubtype   = "taint"

	topologyLabelCountKey = "label-count"
	topologyTaintCountKey = "taint-count"
)

// alignTopologyEncoding returns copies of base and target reduced to the one
// representation both carry, so a comparison reports cluster change rather
// than encoding change. Inputs are not modified.
//
// Per subtype: when both sides carry items, data is dropped — items are the
// authoritative record, and the folded map is not even stable between runs of
// one build, since a key collision resolves by Go map iteration order. When
// either side predates items, items are dropped instead and the summary count
// is restated over the folded map, which is the basis the older build used.
//
// The cost is that a mixed-vintage comparison sees only what the folded
// encoding could express, which is the accuracy the older snapshot was
// captured with. Once both sides carry items the comparison is exact.
func alignTopologyEncoding(base, target *measurement.Measurement) (*measurement.Measurement, *measurement.Measurement) {
	if base == nil || target == nil {
		return base, target
	}
	if base.Type != measurement.TypeNodeTopology || target.Type != measurement.TypeNodeTopology {
		return base, target
	}

	baseIdx := indexSubtypes(base.Subtypes)
	targetIdx := indexSubtypes(target.Subtypes)

	plan := map[string]subtypePlan{}
	for subtype, countKey := range map[string]string{
		topologyLabelSubtype: topologyLabelCountKey,
		topologyTaintSubtype: topologyTaintCountKey,
	} {
		b, t := baseIdx[subtype], targetIdx[subtype]
		if b == nil || t == nil {
			continue
		}
		baseHas, targetHas := len(b.Items) > 0, len(t.Items) > 0
		switch {
		case baseHas && targetHas:
			// Items are the richer record and, once hydrated, carry node
			// membership on both sides. The folded map adds nothing and is not
			// stable even within one build, since a collision resolves by map
			// iteration order.
			plan[subtype] = subtypePlan{hydrate: true, dropData: true}
		case baseHas != targetHas:
			// One side predates items. Drop items and compare the folded map.
			// Exclude collision-ambiguous keys: Go map iteration decides the
			// winner, so an unchanged cluster can write different values on each
			// side across an upgrade/rollback.
			_, ambiguous, err := topology.HydrateItems(itemSide(b, t))
			if err != nil {
				plan[subtype] = subtypePlan{dropItems: true, countKey: countKey}
				continue
			}
			plan[subtype] = subtypePlan{dropItems: true, countKey: countKey, skipKeys: ambiguous}
		}
		// Neither carries items: one vintage, nothing to reconcile. Restating
		// the count here would mask a corrupted one rather than report it.
	}
	if len(plan) == 0 {
		return base, target
	}

	return alignMeasurement(base, plan), alignMeasurement(target, plan)
}

// subtypePlan is how one subtype is reduced before comparison.
type subtypePlan struct {
	hydrate   bool            // resolve referenced node lists into the items
	dropData  bool            // compare items only
	dropItems bool            // compare the folded map only
	countKey  string          // summary count to restate over the folded map
	skipKeys  map[string]bool // folded keys too ambiguous to compare safely
}

// itemSide returns whichever subtype carries items.
func itemSide(a, b *measurement.Subtype) *measurement.Subtype {
	if len(a.Items) > 0 {
		return a
	}
	return b
}

// alignMeasurement copies m with the topology subtypes reduced as directed.
func alignMeasurement(m *measurement.Measurement, plan map[string]subtypePlan) *measurement.Measurement {
	out := *m
	out.Subtypes = make([]measurement.Subtype, len(m.Subtypes))
	copy(out.Subtypes, m.Subtypes)

	folded := map[string]int{}
	for i := range out.Subtypes {
		st := &out.Subtypes[i]
		p, ok := plan[st.Name]
		if !ok {
			continue
		}
		if p.hydrate {
			if items, _, err := topology.HydrateItems(st); err == nil {
				st.Items = items
			} else {
				p.dropData = false // keep data when hydration fails
			}
		}
		if p.dropData {
			st.Data = nil
		}
		if p.dropItems {
			st.Items = nil
		}
		if p.countKey != "" {
			folded[p.countKey] = len(st.Data)
		}
		if len(p.skipKeys) > 0 && st.Data != nil {
			data := make(map[string]measurement.Reading, len(st.Data))
			for k, v := range st.Data {
				if !p.skipKeys[k] {
					data[k] = v
				}
			}
			st.Data = data
		}
	}
	if len(folded) == 0 {
		return &out
	}

	for i := range out.Subtypes {
		st := &out.Subtypes[i]
		if st.Name != topologySummarySubtype || st.Data == nil {
			continue
		}
		data := make(map[string]measurement.Reading, len(st.Data))
		for k, v := range st.Data {
			data[k] = v
		}
		for key, count := range folded {
			if _, ok := data[key]; ok {
				data[key] = measurement.Int(count)
			}
		}
		st.Data = data
	}
	return &out
}

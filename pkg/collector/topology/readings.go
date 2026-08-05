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
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// LabelReading is one aggregated label reading.
type LabelReading struct {
	Key   string
	Value string
	// Nodes carry Key=Value. When Truncated, the list is incomplete and its
	// last element holds the "(+N more)" marker.
	Nodes []string
	// NodeCount is the true total, including nodes omitted by truncation.
	NodeCount int
	Truncated bool
	// RawKey is the Data map key this reading corresponds to, so callers can
	// keep diagnostics identical across both encodings. Not an identity.
	RawKey string
}

// TaintReading is one aggregated taint reading.
type TaintReading struct {
	Key       string
	Effect    string
	Value     string
	Nodes     []string
	NodeCount int
	Truncated bool
	RawKey    string
}

// LabelReadings returns the readings in a NodeTopology "label" subtype,
// preferring the lossless Items form and falling back to decoding Data for
// snapshots captured before it existed.
//
// The paths are not equivalent. Data folds key and value into one map key when
// a label carries multiple values, which collides with a label literally named
// "<key>.<value>" (#2003); on that path a reading's Key is whatever the map
// key says. The ambiguity is inherent to the old encoding and cannot be undone
// here.
//
// Validating node names is caller policy — see pkg/constraints, which fails
// closed on malformed tokens.
func LabelReadings(st *measurement.Subtype) ([]LabelReading, error) {
	if st == nil {
		return nil, nil
	}
	if len(st.Items) > 0 {
		return labelReadingsFromItems(st.Items)
	}
	return labelReadingsFromData(st.Data), nil
}

// TaintReadings returns the readings in a NodeTopology "taint" subtype. See
// LabelReadings for the Items-preferred contract.
//
// Data is doubly ambiguous for taints: encodeTaints counts entries per key but
// disambiguates with effect, so two taints sharing both collapse into one
// entry.
func TaintReadings(st *measurement.Subtype) ([]TaintReading, error) {
	if st == nil {
		return nil, nil
	}
	if len(st.Items) > 0 {
		return taintReadingsFromItems(st.Items)
	}
	return taintReadingsFromData(st.Data), nil
}

// HasLosslessReadings reports whether a subtype carries the Items form.
// Callers that keep a heuristic meaningful only against folded Data keys use
// this rather than inspecting Items directly.
func HasLosslessReadings(st *measurement.Subtype) bool {
	return st != nil && len(st.Items) > 0
}

func labelReadingsFromItems(items []measurement.ItemEntry) ([]LabelReading, error) {
	out := make([]LabelReading, 0, len(items))
	for i := range items {
		key, err := itemContext(items[i], itemCtxKey, i)
		if err != nil {
			return nil, err
		}
		nodes, count, truncated, err := itemNodes(items[i], i)
		if err != nil {
			return nil, err
		}
		out = append(out, LabelReading{
			// An empty label value is legal, so a missing value key and an
			// empty one both decode to "".
			Key:       key,
			Value:     items[i].Context[itemCtxValue],
			Nodes:     nodes,
			NodeCount: count,
			Truncated: truncated,
			RawKey:    key,
		})
	}
	applyLabelRawKeys(out)
	return out, nil
}

// applyLabelRawKeys replays encodeLabels' rule over a decoded set: a key
// carrying more than one value renders as "<key>.<value>". Disambiguation
// depends on the whole set, not one reading.
func applyLabelRawKeys(readings []LabelReading) {
	values := make(map[string]int, len(readings))
	for i := range readings {
		values[readings[i].Key]++
	}
	for i := range readings {
		if values[readings[i].Key] > 1 {
			readings[i].RawKey = readings[i].Key + "." + readings[i].Value
		}
	}
}

func taintReadingsFromItems(items []measurement.ItemEntry) ([]TaintReading, error) {
	out := make([]TaintReading, 0, len(items))
	for i := range items {
		key, err := itemContext(items[i], itemCtxKey, i)
		if err != nil {
			return nil, err
		}
		nodes, count, truncated, err := itemNodes(items[i], i)
		if err != nil {
			return nil, err
		}
		out = append(out, TaintReading{
			Key:       key,
			Effect:    items[i].Context[itemCtxEffect],
			Value:     items[i].Context[itemCtxValue],
			Nodes:     nodes,
			NodeCount: count,
			Truncated: truncated,
			RawKey:    key,
		})
	}
	applyTaintRawKeys(out)
	return out, nil
}

// applyTaintRawKeys replays encodeTaints' rule. It keys on entries sharing a
// key rather than on distinct effects because that is what encodeTaints does —
// RawKey mirrors the old output rather than correcting it.
func applyTaintRawKeys(readings []TaintReading) {
	seen := make(map[string]int, len(readings))
	for i := range readings {
		seen[readings[i].Key]++
	}
	for i := range readings {
		if seen[readings[i].Key] > 1 {
			readings[i].RawKey = readings[i].Key + "." + readings[i].Effect
		}
	}
}

func itemContext(item measurement.ItemEntry, field string, idx int) (string, error) {
	v, ok := item.Context[field]
	if !ok || v == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: missing required context field %q", idx, field))
	}
	return v, nil
}

func itemNodes(item measurement.ItemEntry, idx int) (nodes []string, count int, truncated bool, err error) {
	raw, ok := item.Data[itemDataNodeList]
	if !ok {
		return nil, 0, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: missing required data field %q", idx, itemDataNodeList))
	}
	list, ok := raw.Any().(string)
	if !ok {
		return nil, 0, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: data field %q is not a string", idx, itemDataNodeList))
	}
	nodes = splitNodeList(list)

	count = len(nodes)
	if r, ok := item.Data[itemDataNodeCount]; ok {
		if n, ok := readingInt(r); ok {
			count = n
		}
	}
	// Default to the rendered suffix — the only signal Data ever carried — so an
	// item that omits or mistypes the field still reads as truncated. A declared
	// bool wins when it is one.
	truncated = IsTruncatedNodeList(list)
	if r, ok := item.Data[itemDataTruncated]; ok {
		if b, ok := r.Any().(bool); ok {
			truncated = b
		}
	}
	return nodes, count, truncated, nil
}

// readingInt accepts the integer shapes a Reading can hold; JSON decoders
// deliver integers as float64.
func readingInt(r measurement.Reading) (int, bool) {
	switch v := r.Any().(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v != float64(int64(v)) {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

// labelReadingsFromData decodes the legacy "<value>|<nodes>" encoding. Key is
// the map key verbatim: whether it is a true label name or a synthesized
// "<key>.<value>" cannot be determined from the encoding alone.
func labelReadingsFromData(data map[string]measurement.Reading) []LabelReading {
	if len(data) == 0 {
		return nil
	}
	out := make([]LabelReading, 0, len(data))
	for key, reading := range data {
		value, list, _ := strings.Cut(reading.String(), "|")
		nodes := splitNodeList(list)
		out = append(out, LabelReading{
			Key:       key,
			Value:     value,
			Nodes:     nodes,
			NodeCount: len(nodes),
			Truncated: IsTruncatedNodeList(list),
			RawKey:    key,
		})
	}
	sortLabelReadings(out)
	return out
}

// taintReadingsFromData decodes the legacy taint encoding, whose two shapes
// are told apart by field count: "<effect>|<value>|<nodes>" for a plain key,
// "<value>|<nodes>" for a disambiguated one where the effect is the key suffix.
func taintReadingsFromData(data map[string]measurement.Reading) []TaintReading {
	if len(data) == 0 {
		return nil
	}
	out := make([]TaintReading, 0, len(data))
	for key, reading := range data {
		var effect, value, list string
		parts := strings.SplitN(reading.String(), "|", 3)
		switch len(parts) {
		case 3:
			effect, value, list = parts[0], parts[1], parts[2]
		case 2:
			// Splitting the effect back off the key is ambiguous for a taint
			// key that legitimately contains a dot — the defect Items removes.
			value, list = parts[0], parts[1]
			if i := strings.LastIndex(key, "."); i >= 0 {
				effect = key[i+1:]
			}
		default:
			value = parts[0]
		}
		nodes := splitNodeList(list)
		out = append(out, TaintReading{
			Key:       key,
			Effect:    effect,
			Value:     value,
			Nodes:     nodes,
			NodeCount: len(nodes),
			Truncated: IsTruncatedNodeList(list),
			RawKey:    key,
		})
	}
	sortTaintReadings(out)
	return out
}

// sortLabelReadings matches the collector's emission order so callers see the
// same order from either encoding; the Data path iterates a Go map.
func sortLabelReadings(readings []LabelReading) {
	sort.Slice(readings, func(i, j int) bool {
		if readings[i].Key != readings[j].Key {
			return readings[i].Key < readings[j].Key
		}
		return readings[i].Value < readings[j].Value
	})
}

func sortTaintReadings(readings []TaintReading) {
	sort.Slice(readings, func(i, j int) bool {
		if readings[i].Key != readings[j].Key {
			return readings[i].Key < readings[j].Key
		}
		if readings[i].Effect != readings[j].Effect {
			return readings[i].Effect < readings[j].Effect
		}
		return readings[i].Value < readings[j].Value
	})
}

// splitNodeList splits a rendered node list. A truncated list keeps its
// "(+N more)" marker in the final element; callers consult Truncated.
func splitNodeList(list string) []string {
	if list == "" {
		return nil
	}
	return strings.Split(list, ",")
}

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
		return labelReadingsFromItems(st)
	}
	return labelReadingsFromData(st.Data)
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
		return taintReadingsFromItems(st)
	}
	return taintReadingsFromData(st.Data)
}

// HasLosslessReadings reports whether a subtype carries the Items form.
// Callers that keep a heuristic meaningful only against folded Data keys use
// this rather than inspecting Items directly.
func HasLosslessReadings(st *measurement.Subtype) bool {
	return st != nil && len(st.Items) > 0
}

// labelReadingsFromItems preserves item order: readings[i] always describes
// items[i]. HydrateItems relies on this to pair node lists with items by index.
func labelReadingsFromItems(st *measurement.Subtype) ([]LabelReading, error) {
	items := st.Items
	out := make([]LabelReading, 0, len(items))
	for i := range items {
		key, err := itemContext(items[i], itemCtxKey, i)
		if err != nil {
			return nil, err
		}
		out = append(out, LabelReading{
			// An empty label value is legal, so a missing value key and an
			// empty one both decode to "".
			Key:    key,
			Value:  items[i].Context[itemCtxValue],
			RawKey: key,
		})
	}
	// RawKey is the data entry this reading folds onto, and membership can
	// only be resolved once the whole set is known — folding depends on how
	// many values a key carries.
	applyLabelRawKeys(out)
	folds := foldCounts(rawKeysOf(out))
	for i := range items {
		nodes, count, truncated, err := itemNodes(items[i], i, st.Data, out[i].RawKey, folds)
		if err != nil {
			return nil, err
		}
		out[i].Nodes, out[i].NodeCount, out[i].Truncated = nodes, count, truncated
	}
	return out, nil
}

// rawKeysOf and foldCounts report how many readings fold onto each data entry.
// An entry claimed by more than one reading describes only one of them, so no
// reading may reference it.
func rawKeysOf[T interface{ raw() string }](readings []T) []string {
	out := make([]string, 0, len(readings))
	for _, r := range readings {
		out = append(out, r.raw())
	}
	return out
}

func foldCounts(rawKeys []string) map[string]int {
	out := make(map[string]int, len(rawKeys))
	for _, k := range rawKeys {
		out[k]++
	}
	return out
}

func (r LabelReading) raw() string { return r.RawKey }
func (r TaintReading) raw() string { return r.RawKey }

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

// taintReadingsFromItems preserves item order: readings[i] always describes
// items[i]. HydrateItems relies on this to pair node lists with items by index.
func taintReadingsFromItems(st *measurement.Subtype) ([]TaintReading, error) {
	items := st.Items
	out := make([]TaintReading, 0, len(items))
	for i := range items {
		key, err := itemContext(items[i], itemCtxKey, i)
		if err != nil {
			return nil, err
		}
		// Required so an empty Effect cannot satisfy a caller matching on one.
		// Not checked against the known effects — a newer Kubernetes must
		// still decode.
		effect, err := itemContext(items[i], itemCtxEffect, i)
		if err != nil {
			return nil, err
		}
		out = append(out, TaintReading{
			Key:    key,
			Effect: effect,
			Value:  items[i].Context[itemCtxValue],
			RawKey: key,
		})
	}
	applyTaintRawKeys(out)
	folds := foldCounts(rawKeysOf(out))
	for i := range items {
		nodes, count, truncated, err := itemNodes(items[i], i, st.Data, out[i].RawKey, folds)
		if err != nil {
			return nil, err
		}
		out[i].Nodes, out[i].NodeCount, out[i].Truncated = nodes, count, truncated
	}
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

// itemRef reads the optional reference to a data entry, reporting whether one
// is present.
func itemRef(item measurement.ItemEntry, idx int) (string, bool, error) {
	r, ok := item.Data[itemDataNodeRef]
	if !ok {
		return "", false, nil
	}
	ref, ok := r.Any().(string)
	if !ok || ref == "" {
		return "", false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: data field %q is not a non-empty string", idx, itemDataNodeRef))
	}
	return ref, true, nil
}

// resolveNodeList returns the encoded node list for an item: the inline copy
// when it carries one, or the data entry it names.
//
// An item carries exactly one of the two. Inferring the reference from an
// absent node-list would make a producer's omission indistinguishable from a
// deliberate reference, so the reference is explicit and its absence alongside
// an absent list is a decode failure.
//
// A reference is only honored when it names this reading's own fold key and
// no other reading folds onto that key — the condition under which the entry
// describes this reading and no other. Anything else would resolve to another
// reading's nodes: well formed and wrong, the failure this encoding removes.
func resolveNodeList(item measurement.ItemEntry, idx int, data map[string]measurement.Reading, rawKey string, folds map[string]int) (string, error) {
	inline, hasList := item.Data[itemDataNodeList]
	ref, hasRef, err := itemRef(item, idx)
	if err != nil {
		return "", err
	}

	switch {
	case hasList && hasRef:
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: carries both %q and %q; membership has one source",
				idx, itemDataNodeList, itemDataNodeRef))
	case !hasList && !hasRef:
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: carries neither %q nor %q", idx, itemDataNodeList, itemDataNodeRef))
	case hasList:
		list, ok := inline.Any().(string)
		if !ok {
			return "", errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("topology item %d: data field %q is not a string", idx, itemDataNodeList))
		}
		return list, nil
	}

	if ref != rawKey {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: %q names %q but this reading folds onto %q",
				idx, itemDataNodeRef, ref, rawKey))
	}
	if folds[ref] != 1 {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: %d readings fold onto %q, so it cannot be referenced",
				idx, folds[ref], ref))
	}
	entry, ok := data[ref]
	if !ok {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: %q %q names no data entry", idx, itemDataNodeRef, ref))
	}
	// The node list is the final field of every folded encoding: "<value>|
	// <nodes>" for labels and disambiguated taints, "<effect>|<value>|<nodes>"
	// otherwise.
	parts := strings.Split(entry.String(), "|")
	return parts[len(parts)-1], nil
}

// itemNodes resolves and validates one item's node membership. The three
// fields describe the same node set from different angles, so a disagreement
// makes the item unreadable rather than imprecise.
func itemNodes(item measurement.ItemEntry, idx int, data map[string]measurement.Reading, rawKey string, folds map[string]int) (nodes []string, count int, truncated bool, err error) {
	list, err := resolveNodeList(item, idx, data, rawKey, folds)
	if err != nil {
		return nil, 0, false, err
	}
	nodes = splitNodeList(list)
	hidden, marker, err := truncatedNodeListRemainder(list)
	if err != nil {
		return nil, 0, false, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d", idx), err)
	}

	count, err = itemInt(item, itemDataNodeCount, idx)
	if err != nil {
		return nil, 0, false, err
	}
	truncated, err = itemBool(item, itemDataTruncated, idx)
	if err != nil {
		return nil, 0, false, err
	}

	// The list states the pre-truncation total exactly — the names it renders
	// plus the N its marker withholds — so node-count is checked for equality
	// rather than for a direction. A complete list has hidden == 0, which
	// collapses to "count equals the names".
	switch {
	case marker != truncated:
		return nil, 0, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: %q is %v but the node list %s the truncation marker",
				idx, itemDataTruncated, truncated,
				map[bool]string{true: "carries", false: "does not carry"}[marker]))
	case count != len(nodes)+hidden:
		return nil, 0, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: %q is %d but the node list names %d and withholds %d",
				idx, itemDataNodeCount, count, len(nodes), hidden))
	}
	return nodes, count, truncated, nil
}

// itemInt reads a required non-negative integer data field.
func itemInt(item measurement.ItemEntry, field string, idx int) (int, error) {
	r, ok := item.Data[field]
	if !ok {
		return 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: missing required data field %q", idx, field))
	}
	v, ok := readingInt(r)
	if !ok {
		return 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: data field %q is not an integer", idx, field))
	}
	if v < 0 {
		return 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: data field %q is negative (%d)", idx, field, v))
	}
	return v, nil
}

// itemBool reads a required boolean data field.
func itemBool(item measurement.ItemEntry, field string, idx int) (bool, error) {
	r, ok := item.Data[field]
	if !ok {
		return false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: missing required data field %q", idx, field))
	}
	v, ok := r.Any().(bool)
	if !ok {
		return false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("topology item %d: data field %q is not a boolean", idx, field))
	}
	return v, nil
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
func labelReadingsFromData(data map[string]measurement.Reading) ([]LabelReading, error) {
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]LabelReading, 0, len(data))
	for key, reading := range data {
		value, list, _ := strings.Cut(reading.String(), "|")
		nodes := splitNodeList(list)
		hidden, truncated, err := truncatedNodeListRemainder(list)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("label reading %q", key), err)
		}
		out = append(out, LabelReading{
			Key:       key,
			Value:     value,
			Nodes:     nodes,
			NodeCount: len(nodes) + hidden,
			Truncated: truncated,
			RawKey:    key,
		})
	}
	sortLabelReadings(out)
	return out, nil
}

// taintReadingsFromData decodes the legacy taint encoding, whose two shapes
// are told apart by field count: "<effect>|<value>|<nodes>" for a plain key,
// "<value>|<nodes>" for a disambiguated one where the effect is the key suffix.
func taintReadingsFromData(data map[string]measurement.Reading) ([]TaintReading, error) {
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]TaintReading, 0, len(data))
	for rawKey, reading := range data {
		key := rawKey
		var effect, value, list string
		parts := strings.SplitN(reading.String(), "|", 3)
		switch len(parts) {
		case 3:
			effect, value, list = parts[0], parts[1], parts[2]
		case 2:
			// Two fields exist only because encodeTaints moved the effect into
			// the key, so the final segment is the effect and the prefix is the
			// key — the same identity the item path reports. Which is still a
			// guess for a taint key that legitimately contains a dot: the
			// ambiguity the item encoding removes.
			// A key with no separator, or nothing after it, carries no effect
			// to recover — decoding it would hand back Effect:"".
			i := strings.LastIndex(rawKey, ".")
			if i < 0 || i == len(rawKey)-1 {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("taint reading %q: two-field form requires an effect suffix in the key", rawKey))
			}
			value, list = parts[0], parts[1]
			effect, key = rawKey[i+1:], rawKey[:i]
		default:
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("taint reading %q: expected 2 or 3 pipe-separated fields, got 1", rawKey))
		}
		nodes := splitNodeList(list)
		hidden, truncated, err := truncatedNodeListRemainder(list)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("taint reading %q", rawKey), err)
		}
		out = append(out, TaintReading{
			Key:       key,
			Effect:    effect,
			Value:     value,
			Nodes:     nodes,
			NodeCount: len(nodes) + hidden,
			Truncated: truncated,
			RawKey:    rawKey,
		})
	}
	sortTaintReadings(out)
	return out, nil
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

// Subtype names carrying a folded counterpart.
const (
	subtypeLabel = "label"
	subtypeTaint = "taint"
)

// HydrateItems returns st's items with every referenced node list resolved
// inline, so a consumer comparing items sees node membership regardless of how
// the snapshot chose to encode it. It also reports the folded keys that more
// than one reading maps onto: entries the folded encoding cannot describe
// unambiguously, and whose value therefore depends on map iteration order.
//
// Returns nil when st carries no items. Subtypes other than "label" and
// "taint" are returned unchanged, having no folded counterpart.
func HydrateItems(st *measurement.Subtype) (items []measurement.ItemEntry, ambiguous map[string]bool, err error) {
	if st == nil || len(st.Items) == 0 {
		return nil, nil, nil
	}

	var nodes [][]string
	var rawKeys []string
	switch st.Name {
	case subtypeLabel:
		readings, err := LabelReadings(st)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range readings {
			nodes, rawKeys = append(nodes, r.Nodes), append(rawKeys, r.RawKey)
		}
	case subtypeTaint:
		readings, err := TaintReadings(st)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range readings {
			nodes, rawKeys = append(nodes, r.Nodes), append(rawKeys, r.RawKey)
		}
	default:
		return st.Items, nil, nil
	}

	folds := foldCounts(rawKeys)
	ambiguous = make(map[string]bool)
	for k, n := range folds {
		if n > 1 {
			ambiguous[k] = true
		}
	}

	if len(nodes) != len(st.Items) {
		return nil, nil, errors.New(errors.ErrCodeInternal,
			"hydration produced a different number of readings than items")
	}
	items = make([]measurement.ItemEntry, len(st.Items))
	for i := range st.Items {
		data := make(map[string]measurement.Reading, len(st.Items[i].Data))
		for k, v := range st.Items[i].Data {
			if k == itemDataNodeRef {
				continue
			}
			data[k] = v
		}
		// Joining reproduces the encoded list exactly, marker included: the
		// suffix is space-joined onto the final name rather than being an
		// element of its own.
		data[itemDataNodeList] = measurement.Str(strings.Join(nodes[i], ","))
		items[i] = measurement.ItemEntry{Context: st.Items[i].Context, Data: data}
	}
	return items, ambiguous, nil
}

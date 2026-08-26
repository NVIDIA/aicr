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

package measurement

import (
	"fmt"
	"strconv"
	"strings"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// Structured-error context keys used by path parsing and extraction.
const (
	keyType         = "type"
	keyPath         = "path"
	keyKey          = "key"
	keySubtype      = "subtype"
	keySelector     = "selector"
	keyPredicateKey = "predicateKey"
	keySuggestion   = "suggestion"
)

// PathGPUNodesLabel is the node-set constraint form from issue #1755. Unlike
// a scalar path it does not name a reading any producer emits: the evaluator
// in pkg/constraints synthesizes the GPU-node set from the snapshot's
// NodeTopology.label readings and quantifies a label predicate over it.
//
// It lives here rather than in pkg/constraints so the catalog can accept it
// without an import cycle (pkg/constraints imports pkg/recipe, which imports
// this package).
const PathGPUNodesLabel = "NodeTopology.gpu-nodes.label"

// itemSelector represents an addressable element of a Subtype.Items list.
// It is either an integer index (Index != nil) or a key=value predicate
// (Predicate != nil); never both, never neither.
type itemSelector struct {
	Raw       string
	Index     *int
	Predicate *itemPredicate
}

type itemPredicate struct {
	Key   string
	Value string
}

// Path represents a parsed fully qualified constraint path.
//
// Without item selector: "{Type}.{Subtype}.{Key}"
//
//	Example: "K8s.server.version" -> Type="K8s", Subtype="server", Key="version"
//
// With item selector: "{Type}.{Subtype}[<selector>].{Key}"
//
//	Index form:     "NetworkTopology.pfs[0].rail"
//	Predicate form: "NetworkTopology.pfs[rail=3].pciAddress"
//
// The selector targets an entry in Subtype.Items; Key is then resolved against
// that ItemEntry's Data (preferred) or Context. Paths without a selector keep
// the legacy behavior of looking up Key in Subtype.Data.
//
// The key portion may contain dots (e.g., "/proc/sys/kernel/osrelease").
//
// The selector is deliberately unexported: the selector grammar is an
// implementation detail of the path syntax, not a public shape.
type Path struct {
	Type     Type
	Subtype  string
	Key      string
	selector *itemSelector
}

// ParsePath parses a fully qualified constraint path.
func ParsePath(path string) (*Path, error) {
	if path == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "constraint path cannot be empty")
	}

	before, after, ok := strings.Cut(path, ".")
	if !ok {
		return nil, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid constraint path: expected format {Type}.{Subtype}[selector].{Key}",
			map[string]any{keyPath: path})
	}

	typeStr := before
	rest := after

	measurementType, valid := ParseType(typeStr)
	if !valid {
		return nil, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid measurement type in constraint path",
			map[string]any{keyType: typeStr, keyPath: path, "validTypes": Types})
	}

	subtype, selector, key, err := parseSubtypeSelectorKey(rest, path, measurementType)
	if err != nil {
		return nil, err
	}

	return &Path{
		Type:     measurementType,
		Subtype:  subtype,
		Key:      key,
		selector: selector,
	}, nil
}

// parseSubtypeSelectorKey parses the portion of the path after "{Type}." into
// (subtype, optional selector, key).
//
// The Key may contain dots; everything after the separating dot (or after the
// closing `]` and its `.`) belongs to Key. Which dot separates depends on the
// Type: subtype names normally carry no dot, so the FIRST one separates and a
// dotted key like "/proc/sys/kernel/osrelease" resolves. Types whose subtype
// names do carry dots — SystemD unit names — separate at the LAST dot instead.
// See splitsOnLastDot.
//
// The bracket form needs no rule: `[` delimits the subtype explicitly, so a
// dotted subtype is unambiguous there.
func parseSubtypeSelectorKey(rest, fullPath string, t Type) (string, *itemSelector, string, error) {
	bracketStart := strings.Index(rest, "[")
	dotIdx := strings.Index(rest, ".")

	if bracketStart >= 0 && (dotIdx < 0 || bracketStart < dotIdx) {
		// Subtype[selector].Key form.
		subtype := rest[:bracketStart]
		if subtype == "" {
			return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
				"invalid constraint path: subtype before '[' is empty",
				map[string]any{keyPath: fullPath})
		}

		bracketEnd := strings.Index(rest[bracketStart:], "]")
		if bracketEnd < 0 {
			return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
				"invalid constraint path: unclosed item selector bracket",
				map[string]any{keyPath: fullPath})
		}
		bracketEnd += bracketStart

		sel, err := parseItemSelector(rest[bracketStart+1:bracketEnd], fullPath)
		if err != nil {
			return "", nil, "", err
		}

		after := rest[bracketEnd+1:]
		if after == "" {
			return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
				"invalid constraint path: missing key after item selector",
				map[string]any{keyPath: fullPath})
		}
		if after[0] != '.' {
			return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
				"invalid constraint path: expected '.' after item selector",
				map[string]any{keyPath: fullPath})
		}
		key := after[1:]
		if key == "" {
			return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
				"invalid constraint path: missing key after item selector",
				map[string]any{keyPath: fullPath})
		}
		return subtype, sel, key, nil
	}

	// Subtype.Key form (no selector).
	if dotIdx < 0 {
		return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid constraint path: expected format {Type}.{Subtype}.{Key}",
			map[string]any{keyPath: fullPath})
	}
	if splitsOnLastDot(t) {
		dotIdx = strings.LastIndex(rest, ".")
	}
	subtype := rest[:dotIdx]
	key := rest[dotIdx+1:]
	if subtype == "" || key == "" {
		return "", nil, "", aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid constraint path: subtype or key is empty",
			map[string]any{keyPath: fullPath})
	}
	return subtype, nil, key, nil
}

// parseItemSelector parses the contents of `[ ... ]` into an itemSelector.
// Forms accepted:
//   - integer (no equals sign) -> index selector
//   - key=value                -> predicate selector (LHS and RHS non-empty)
func parseItemSelector(raw, fullPath string) (*itemSelector, error) {
	if raw == "" {
		return nil, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid constraint path: item selector is empty",
			map[string]any{keyPath: fullPath})
	}
	if before, after, ok := strings.Cut(raw, "="); ok {
		k := before
		v := after
		if k == "" || v == "" {
			return nil, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
				"invalid constraint path: predicate selector requires non-empty key and value",
				map[string]any{keyPath: fullPath, keySelector: raw})
		}
		return &itemSelector{Raw: raw, Predicate: &itemPredicate{Key: k, Value: v}}, nil
	}
	idx, err := strconv.Atoi(raw)
	if err != nil {
		return nil, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid constraint path: item selector must be an integer index or 'key=value' predicate",
			map[string]any{keyPath: fullPath, keySelector: raw})
	}
	if idx < 0 {
		return nil, aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
			"invalid constraint path: item index cannot be negative",
			map[string]any{keyPath: fullPath, keySelector: raw})
	}
	return &itemSelector{Raw: raw, Index: &idx}, nil
}

// String returns the fully qualified path string.
func (p *Path) String() string {
	if p.selector != nil {
		return fmt.Sprintf("%s.%s[%s].%s", p.Type, p.Subtype, p.selector.Raw, p.Key)
	}
	return fmt.Sprintf("%s.%s.%s", p.Type, p.Subtype, p.Key)
}

// Extract extracts the value at this path from a set of measurements.
// Returns the value as a string, or an error if the path doesn't exist.
//
// Callers hold a *snapshotter.Snapshot pass snap.Measurements; the nil-snapshot
// guard belongs at that call site, not here — a nil measurement slice is a
// legitimate "nothing collected" input, distinct from "no snapshot supplied".
func (p *Path) Extract(ms []*Measurement) (string, error) {
	if p == nil {
		return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "constraint path is nil")
	}

	// Find the measurement with matching type
	var targetMeasurement *Measurement
	for _, m := range ms {
		if m != nil && m.Type == p.Type {
			targetMeasurement = m
			break
		}
	}

	if targetMeasurement == nil {
		return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
			"measurement type not found in snapshot",
			map[string]any{keyType: p.Type})
	}

	// Find the subtype
	var targetSubtype *Subtype
	for i := range targetMeasurement.Subtypes {
		if targetMeasurement.Subtypes[i].Name == p.Subtype {
			targetSubtype = &targetMeasurement.Subtypes[i]
			break
		}
	}

	if targetSubtype == nil {
		return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
			"subtype not found in measurement",
			map[string]any{keySubtype: p.Subtype, keyType: p.Type})
	}

	if p.selector != nil {
		return extractFromItems(targetSubtype, p)
	}

	// Find the key in data (legacy path).
	reading, exists := targetSubtype.Data[p.Key]
	if !exists {
		return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
			"key not found in subtype",
			map[string]any{keyKey: p.Key, keySubtype: p.Subtype, keyType: p.Type})
	}

	// Convert reading to string
	return reading.String(), nil
}

// extractFromItems resolves a path with an item selector against
// targetSubtype.Items, then looks up p.Key in the chosen ItemEntry.
func extractFromItems(st *Subtype, p *Path) (string, error) {
	if len(st.Items) == 0 {
		return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
			"subtype has no items but path uses an item selector",
			map[string]any{keySubtype: p.Subtype, keyType: p.Type, keySelector: p.selector.Raw})
	}

	if p.selector.Index != nil {
		idx := *p.selector.Index
		if idx >= len(st.Items) {
			return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
				"item index out of bounds",
				map[string]any{keySubtype: p.Subtype, keyType: p.Type, "index": idx, "itemCount": len(st.Items)})
		}
		return lookupInItem(&st.Items[idx], p)
	}

	// Predicate match.
	pred := p.selector.Predicate
	var matchIdx = -1
	for i := range st.Items {
		if itemMatchesPredicate(&st.Items[i], pred) {
			if matchIdx >= 0 {
				return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeConflict,
					"item predicate matches multiple entries",
					map[string]any{
						keySubtype:  p.Subtype,
						keyType:     p.Type,
						"predicate": p.selector.Raw,
						"matchedAt": []int{matchIdx, i},
					})
			}
			matchIdx = i
		}
	}
	if matchIdx < 0 {
		return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
			"no item matches predicate",
			map[string]any{keySubtype: p.Subtype, keyType: p.Type, "predicate": p.selector.Raw})
	}
	return lookupInItem(&st.Items[matchIdx], p)
}

// itemMatchesPredicate reports whether an ItemEntry has a field (in Data or
// Context) named pred.Key whose stringified value equals pred.Value.
func itemMatchesPredicate(item *ItemEntry, pred *itemPredicate) bool {
	if r, ok := item.Data[pred.Key]; ok && r != nil {
		return r.String() == pred.Value
	}
	if v, ok := item.Context[pred.Key]; ok {
		return v == pred.Value
	}
	return false
}

// lookupInItem looks up p.Key in the ItemEntry: Data (Reading.String()) first,
// then Context (string), then errors.
func lookupInItem(item *ItemEntry, p *Path) (string, error) {
	if r, ok := item.Data[p.Key]; ok && r != nil {
		return r.String(), nil
	}
	if v, ok := item.Context[p.Key]; ok {
		return v, nil
	}
	return "", aicrerrors.NewWithContext(aicrerrors.ErrCodeNotFound,
		"key not found in item",
		map[string]any{keyKey: p.Key, keySubtype: p.Subtype, keyType: p.Type, keySelector: p.selector.Raw})
}

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

package constraints

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// Structured-error context keys used by this package.
const (
	keyType     = "type"
	keyPath     = "path"
	keySubtype  = "subtype"
	keySelector = "selector"
)

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

// ConstraintPath represents a parsed fully qualified constraint path.
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
type ConstraintPath struct {
	Type     measurement.Type
	Subtype  string
	Key      string
	Selector *itemSelector
}

// ParseConstraintPath parses a fully qualified constraint path.
func ParseConstraintPath(path string) (*ConstraintPath, error) {
	if path == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "constraint path cannot be empty")
	}

	typeDot := strings.Index(path, ".")
	if typeDot < 0 {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid constraint path: expected format {Type}.{Subtype}[selector].{Key}",
			map[string]any{keyPath: path})
	}

	typeStr := path[:typeDot]
	rest := path[typeDot+1:]

	measurementType, valid := measurement.ParseType(typeStr)
	if !valid {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid measurement type in constraint path",
			map[string]any{keyType: typeStr, keyPath: path, "validTypes": measurement.Types})
	}

	subtype, selector, key, err := parseSubtypeSelectorKey(rest, path)
	if err != nil {
		return nil, err
	}

	return &ConstraintPath{
		Type:     measurementType,
		Subtype:  subtype,
		Key:      key,
		Selector: selector,
	}, nil
}

// parseSubtypeSelectorKey parses the portion of the path after "{Type}." into
// (subtype, optional selector, key). The Key may contain dots; everything
// after the first separating dot (or after the closing `]` and its `.`)
// belongs to Key.
func parseSubtypeSelectorKey(rest, fullPath string) (string, *itemSelector, string, error) {
	bracketStart := strings.Index(rest, "[")
	dotIdx := strings.Index(rest, ".")

	if bracketStart >= 0 && (dotIdx < 0 || bracketStart < dotIdx) {
		// Subtype[selector].Key form.
		subtype := rest[:bracketStart]
		if subtype == "" {
			return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
				"invalid constraint path: subtype before '[' is empty",
				map[string]any{keyPath: fullPath})
		}

		bracketEnd := strings.Index(rest[bracketStart:], "]")
		if bracketEnd < 0 {
			return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
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
			return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
				"invalid constraint path: missing key after item selector",
				map[string]any{keyPath: fullPath})
		}
		if after[0] != '.' {
			return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
				"invalid constraint path: expected '.' after item selector",
				map[string]any{keyPath: fullPath})
		}
		key := after[1:]
		if key == "" {
			return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
				"invalid constraint path: missing key after item selector",
				map[string]any{keyPath: fullPath})
		}
		return subtype, sel, key, nil
	}

	// Subtype.Key form (no selector).
	if dotIdx < 0 {
		return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid constraint path: expected format {Type}.{Subtype}.{Key}",
			map[string]any{keyPath: fullPath})
	}
	subtype := rest[:dotIdx]
	key := rest[dotIdx+1:]
	if subtype == "" || key == "" {
		return "", nil, "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
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
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid constraint path: item selector is empty",
			map[string]any{keyPath: fullPath})
	}
	if eq := strings.Index(raw, "="); eq >= 0 {
		k := raw[:eq]
		v := raw[eq+1:]
		if k == "" || v == "" {
			return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
				"invalid constraint path: predicate selector requires non-empty key and value",
				map[string]any{keyPath: fullPath, keySelector: raw})
		}
		return &itemSelector{Raw: raw, Predicate: &itemPredicate{Key: k, Value: v}}, nil
	}
	idx, err := strconv.Atoi(raw)
	if err != nil {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid constraint path: item selector must be an integer index or 'key=value' predicate",
			map[string]any{keyPath: fullPath, keySelector: raw})
	}
	if idx < 0 {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid constraint path: item index cannot be negative",
			map[string]any{keyPath: fullPath, keySelector: raw})
	}
	return &itemSelector{Raw: raw, Index: &idx}, nil
}

// String returns the fully qualified path string.
func (cp *ConstraintPath) String() string {
	if cp.Selector != nil {
		return fmt.Sprintf("%s.%s[%s].%s", cp.Type, cp.Subtype, cp.Selector.Raw, cp.Key)
	}
	return fmt.Sprintf("%s.%s.%s", cp.Type, cp.Subtype, cp.Key)
}

// ExtractValue extracts the value at this path from a snapshot.
// Returns the value as a string, or an error if the path doesn't exist.
func (cp *ConstraintPath) ExtractValue(snap *snapshotter.Snapshot) (string, error) {
	if snap == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "snapshot is nil")
	}

	// Find the measurement with matching type
	var targetMeasurement *measurement.Measurement
	for _, m := range snap.Measurements {
		if m.Type == cp.Type {
			targetMeasurement = m
			break
		}
	}

	if targetMeasurement == nil {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"measurement type not found in snapshot",
			map[string]any{keyType: cp.Type})
	}

	// Find the subtype
	var targetSubtype *measurement.Subtype
	for i := range targetMeasurement.Subtypes {
		if targetMeasurement.Subtypes[i].Name == cp.Subtype {
			targetSubtype = &targetMeasurement.Subtypes[i]
			break
		}
	}

	if targetSubtype == nil {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"subtype not found in measurement",
			map[string]any{keySubtype: cp.Subtype, keyType: cp.Type})
	}

	if cp.Selector != nil {
		return extractFromItems(targetSubtype, cp)
	}

	// Find the key in data (legacy path).
	reading, exists := targetSubtype.Data[cp.Key]
	if !exists {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"key not found in subtype",
			map[string]any{"key": cp.Key, keySubtype: cp.Subtype, keyType: cp.Type})
	}

	// Convert reading to string
	return reading.String(), nil
}

// extractFromItems resolves a path with an item selector against
// targetSubtype.Items, then looks up cp.Key in the chosen ItemEntry.
func extractFromItems(st *measurement.Subtype, cp *ConstraintPath) (string, error) {
	if len(st.Items) == 0 {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"subtype has no items but path uses an item selector",
			map[string]any{keySubtype: cp.Subtype, keyType: cp.Type, keySelector: cp.Selector.Raw})
	}

	if cp.Selector.Index != nil {
		idx := *cp.Selector.Index
		if idx >= len(st.Items) {
			return "", errors.NewWithContext(errors.ErrCodeNotFound,
				"item index out of bounds",
				map[string]any{keySubtype: cp.Subtype, keyType: cp.Type, "index": idx, "itemCount": len(st.Items)})
		}
		return lookupInItem(&st.Items[idx], cp)
	}

	// Predicate match.
	pred := cp.Selector.Predicate
	var matchIdx = -1
	for i := range st.Items {
		if itemMatchesPredicate(&st.Items[i], pred) {
			if matchIdx >= 0 {
				return "", errors.NewWithContext(errors.ErrCodeConflict,
					"item predicate matches multiple entries",
					map[string]any{
						keySubtype:  cp.Subtype,
						keyType:     cp.Type,
						"predicate": cp.Selector.Raw,
						"matchedAt": []int{matchIdx, i},
					})
			}
			matchIdx = i
		}
	}
	if matchIdx < 0 {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"no item matches predicate",
			map[string]any{keySubtype: cp.Subtype, keyType: cp.Type, "predicate": cp.Selector.Raw})
	}
	return lookupInItem(&st.Items[matchIdx], cp)
}

// itemMatchesPredicate reports whether an ItemEntry has a field (in Data or
// Context) named pred.Key whose stringified value equals pred.Value.
func itemMatchesPredicate(item *measurement.ItemEntry, pred *itemPredicate) bool {
	if r, ok := item.Data[pred.Key]; ok && r != nil {
		return r.String() == pred.Value
	}
	if v, ok := item.Context[pred.Key]; ok {
		return v == pred.Value
	}
	return false
}

// lookupInItem looks up cp.Key in the ItemEntry: Data (Reading.String()) first,
// then Context (string), then errors.
func lookupInItem(item *measurement.ItemEntry, cp *ConstraintPath) (string, error) {
	if r, ok := item.Data[cp.Key]; ok && r != nil {
		return r.String(), nil
	}
	if v, ok := item.Context[cp.Key]; ok {
		return v, nil
	}
	return "", errors.NewWithContext(errors.ErrCodeNotFound,
		"key not found in item",
		map[string]any{"key": cp.Key, keySubtype: cp.Subtype, keyType: cp.Type, keySelector: cp.Selector.Raw})
}

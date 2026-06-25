// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// DeepEqualValues returns the diff between two helm values trees as a list
// of Mismatch entries (empty when the trees are semantically equal). nil
// inputs are normalised to empty maps so callers can pass action.Get's
// Config field directly.
//
// Argument order is (actual, expected) — `actual` is the deployed release's
// values, `expected` is what l8k would install now. Each returned Mismatch
// has Actual set from `actual` and Expected from `expected`.
//
// Used both by the Phase 0 helm install/upgrade conflict gate and by the
// preflight CheckHelmValues check — one source of truth so the two flows
// can't disagree about whether the deployed release matches what l8k
// would install now.
//
// Comparison semantics: identical JSON-shape trees are equal regardless of
// map-key ordering. Slices compare by index (helm user-supplied values
// rarely have set-semantics arrays). Scalars use reflect.DeepEqual — a
// type mismatch (e.g. `enabled: "true"` string vs `enabled: true` bool)
// counts as a diff.
func DeepEqualValues(a, b map[string]interface{}) []Mismatch {
	if a == nil {
		a = map[string]interface{}{}
	}
	if b == nil {
		b = map[string]interface{}{}
	}
	var diffs []Mismatch
	walkValues("", a, b, &diffs)
	return diffs
}

func walkValues(path string, a, b interface{}, diffs *[]Mismatch) {
	// Map vs map: recurse on union of keys.
	if am, aok := a.(map[string]interface{}); aok {
		bm, bok := b.(map[string]interface{})
		if !bok {
			*diffs = append(*diffs, Mismatch{Path: path, Actual: a, Expected: b})
			return
		}
		keys := unionKeys(am, bm)
		sort.Strings(keys)
		for _, k := range keys {
			walkValues(appendPath(path, k), am[k], bm[k], diffs)
		}
		return
	}
	// Slice vs slice: compare by index.
	if as, aok := a.([]interface{}); aok {
		bs, bok := b.([]interface{})
		if !bok || len(as) != len(bs) {
			*diffs = append(*diffs, Mismatch{Path: path, Actual: a, Expected: b})
			return
		}
		for i := range as {
			walkValues(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i], diffs)
		}
		return
	}
	// Scalar comparison.
	if !reflect.DeepEqual(a, b) {
		*diffs = append(*diffs, Mismatch{Path: path, Actual: a, Expected: b})
	}
}

func unionKeys(a, b map[string]interface{}) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	return keys
}

func appendPath(parent, child string) string {
	if parent == "" {
		return child
	}
	if strings.ContainsAny(child, ".[]") {
		return parent + "." + fmt.Sprintf("%q", child)
	}
	return parent + "." + child
}

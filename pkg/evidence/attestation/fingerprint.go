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

package attestation

import (
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// FingerprintFromSnapshot derives a basic FingerprintBlock from a
// snapshot's measurements. This is a V1 stub: it reads what the
// existing collectors already populate and emits resolved values for
// any dimension that can be derived without inference. The replacement
// (issue #752) will surface a richer Fingerprint type with per-signal
// provenance and a Match() helper directly on snapshotter.Snapshot.
//
// Dimensions that cannot be derived from the snapshot are left nil so
// the predicate's fingerprint block omits them rather than emitting
// confusing empty strings.
//
// This stub deliberately does *not* try to derive intent or platform.
// Those are recipe-level concepts, not cluster facts; populating them
// from heuristics would be a guess, not a measurement. They remain
// tracked in CriteriaMatch.PerDimension via the recipe's declared
// criteria.
func FingerprintFromSnapshot(snap *snapshotter.Snapshot) FingerprintBlock {
	fp := FingerprintBlock{}
	if snap == nil {
		return fp
	}

	for _, m := range snap.Measurements {
		if m == nil {
			continue
		}
		switch m.Type {
		case measurement.TypeK8s:
			if v := firstReadingValue(m, measurement.KeyVersion); v != "" {
				fp.K8sVersion = &FingerprintDimension{Value: v}
			}
			if v := firstSubtypeContext(m, "service"); v != "" {
				fp.Service = &FingerprintDimension{Value: v}
			}
		case measurement.TypeOS:
			if v := firstSubtypeName(m); v != "" {
				fp.OS = &FingerprintDimension{Value: v}
			}
		case measurement.TypeGPU:
			if v := firstSubtypeName(m); v != "" {
				fp.Accelerator = &FingerprintDimension{Value: v}
			}
		case measurement.TypeSystemD, measurement.TypeNodeTopology:
			// SystemD and NodeTopology don't contribute to V1
			// fingerprint dimensions; they're recipe-criteria-irrelevant.
		}
	}

	return fp
}

// criteriaWildcard is the recipe-criteria value that means "match any".
const criteriaWildcard = "any"

// MatchCriteria evaluates a recipe's criteria block against a
// fingerprint and returns a CriteriaMatch. Empty / "any" criteria are
// treated as wildcard matches. The function is total: missing
// fingerprint dimensions against a non-wildcard criterion yield
// FingerprintProvides == "" and Match == false.
func MatchCriteria(fp FingerprintBlock, c *recipe.Criteria) CriteriaMatch {
	if c == nil {
		return CriteriaMatch{Matched: true, PerDimension: map[string]CriteriaDimensionMatch{}}
	}

	per := map[string]CriteriaDimensionMatch{}
	addDim(per, "service", string(c.Service), valueOf(fp.Service))
	addDim(per, "accelerator", string(c.Accelerator), valueOf(fp.Accelerator))
	addDim(per, "os", string(c.OS), valueOf(fp.OS))
	addDim(per, "intent", string(c.Intent), valueOf(fp.Intent))
	addDim(per, "platform", string(c.Platform), valueOf(fp.Platform))

	matched := true
	for _, dim := range per {
		if !dim.Match {
			matched = false
			break
		}
	}
	return CriteriaMatch{Matched: matched, PerDimension: per}
}

func addDim(out map[string]CriteriaDimensionMatch, name, requires, provides string) {
	// Empty / "any" criteria are wildcards.
	if requires == "" || requires == criteriaWildcard {
		out[name] = CriteriaDimensionMatch{
			RecipeRequires:      requires,
			FingerprintProvides: provides,
			Match:               true,
		}
		return
	}
	out[name] = CriteriaDimensionMatch{
		RecipeRequires:      requires,
		FingerprintProvides: provides,
		Match:               provides == requires,
	}
}

func valueOf(d *FingerprintDimension) string {
	if d == nil {
		return ""
	}
	return d.Value
}

// firstReadingValue returns the first reading value matching key
// across all subtypes of a measurement, or "" if not present.
// Reading is a runtime interface; we use String() for the resolved
// scalar form, which round-trips for the string-typed measurement keys
// fingerprinting cares about (k8s version, etc.).
func firstReadingValue(m *measurement.Measurement, key string) string {
	for _, st := range m.Subtypes {
		if r, ok := st.Data[key]; ok && r != nil {
			return r.String()
		}
	}
	return ""
}

// firstSubtypeName returns the Name of the first subtype, or "".
// For OS measurements the subtype name is the OS distribution
// (e.g., "ubuntu"); for GPU measurements it's the SKU (e.g., "h100").
func firstSubtypeName(m *measurement.Measurement) string {
	if len(m.Subtypes) == 0 {
		return ""
	}
	return m.Subtypes[0].Name
}

// firstSubtypeContext walks a measurement's subtype context maps and
// returns the first non-empty value for the given key.
func firstSubtypeContext(m *measurement.Measurement, key string) string {
	for _, st := range m.Subtypes {
		if v, ok := st.Context[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

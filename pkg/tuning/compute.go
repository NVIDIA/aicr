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

package tuning

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// SchemaVersion is the version of the Report schema.
const SchemaVersion = "1.0.0"

// nwcComponent is the registry name of the nodewright customizations component
// whose selected manifest carries the tuning/setup package pins.
const nwcComponent = "nodewright-customizations"

// anyValue renders an empty criteria dimension.
const anyValue = "*"

// NotApplicable is the marker for a Profile equal to the accelerator, or for an
// absent Setup/Tuning package pin. Exported so the renderer (tools/tuning)
// reuses one definition instead of duplicating the literal.
const NotApplicable = "-"

// nwcManifestsDir is the data-relative directory holding the
// nodewright-customizations component's manifests. Any manifest a resolved
// componentRef references from here is treated as its tuning/setup manifest —
// there is deliberately no hard-coded manifest allowlist, so a newly added
// manifest is picked up automatically rather than silently skipped.
const nwcManifestsDir = "components/nodewright-customizations/manifests/"

// Options configures a Compute run.
type Options struct {
	// Provider is the recipe DataProvider to enumerate and resolve against.
	// Nil selects the package-global embedded catalog (hermetic).
	Provider recipe.DataProvider
	// Version stamps the recipe builder version used during resolution.
	Version string
	// Filter narrows enumeration to leaves matching every set criteria dimension.
	Filter *recipe.Criteria
}

// Row is one (service, accelerator) tuning-status entry.
type Row struct {
	Service     string
	Accelerator string
	Profile     string
	Setup       PackagePin
	Tuning      PackagePin
}

// Report is the catalog-wide tuning-status snapshot.
type Report struct {
	SchemaVersion string
	Rows          []Row
}

// Compute resolves every leaf recipe, extracts the nodewright tuning/setup pins
// applied for its (service, accelerator), and returns a deterministic Report.
// Rows are collapsed by (service, accelerator) with a consistency check: if two
// leaves in the same group disagree on the applied pins/profile, Compute fails
// loud rather than silently picking one.
func Compute(ctx context.Context, opts Options) (*Report, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.TuningComputeTimeout)
	defer cancel()

	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{
		Provider: opts.Provider,
		Version:  opts.Version,
		Filter:   opts.Filter,
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err,
			errors.ErrCodeInternal, "failed to resolve recipe catalog for tuning status")
	}

	type groupKey struct{ service, accelerator string }
	groups := map[groupKey]Row{}

	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result == nil {
			continue
		}
		ref, ok := findComponentRef(leaf.Result.ComponentRefs, nwcComponent)
		if !ok {
			continue
		}
		manifestPath := selectTuningManifest(ref.ManifestFiles)
		if manifestPath == "" {
			continue
		}
		content, cerr := recipe.GetManifestContentWithContext(ctx, leaf.Result.DataProvider(), manifestPath)
		if cerr != nil {
			return nil, errors.PropagateOrWrap(cerr,
				errors.ErrCodeInternal, "failed to read tuning manifest "+manifestPath)
		}
		pins, perr := extractPackagePins(content)
		if perr != nil {
			return nil, errors.PropagateOrWrap(perr,
				errors.ErrCodeInternal, "failed to extract pins from "+manifestPath)
		}
		setup, tuning, clsErr := classifyPins(pins)
		if clsErr != nil {
			return nil, errors.PropagateOrWrap(clsErr,
				errors.ErrCodeInternal, "failed to classify pins from "+manifestPath)
		}

		accelCell := valueOrAny(string(leaf.Entry.Criteria.Accelerator))
		row := Row{
			Service:     valueOrAny(string(leaf.Entry.Criteria.Service)),
			Accelerator: accelCell,
			Profile:     profileCell(ref.Overrides, accelCell),
			Setup:       setup,
			Tuning:      tuning,
		}

		key := groupKey{row.Service, row.Accelerator}
		if existing, seen := groups[key]; seen {
			if existing != row {
				return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"inconsistent tuning for service=%s accelerator=%s across recipes (leaf %q): %+v vs %+v",
					key.service, key.accelerator, leaf.Entry.Name, existing, row))
			}
			continue
		}
		groups[key] = row
	}

	rows := make([]Row, 0, len(groups))
	for _, r := range groups {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Service != rows[j].Service {
			return rows[i].Service < rows[j].Service
		}
		return rows[i].Accelerator < rows[j].Accelerator
	})

	return &Report{SchemaVersion: SchemaVersion, Rows: rows}, nil
}

// findComponentRef returns the ref with the given name.
func findComponentRef(refs []recipe.ComponentRef, name string) (recipe.ComponentRef, bool) {
	for _, r := range refs {
		if r.Name == name {
			return r, true
		}
	}
	return recipe.ComponentRef{}, false
}

// selectTuningManifest returns the first manifest the componentRef references
// from the nodewright-customizations manifests directory, or "" if none is
// referenced. The ref is already the nodewright-customizations component, so any
// manifest it carries from that directory is its tuning/setup manifest — no
// per-manifest allowlist is needed, and a newly added manifest is picked up
// automatically.
func selectTuningManifest(manifestFiles []string) string {
	for _, m := range manifestFiles {
		if strings.HasPrefix(m, nwcManifestsDir) {
			return m
		}
	}
	return ""
}

// valueOrAny renders an empty or "any" criteria dimension as the wildcard.
func valueOrAny(v string) string {
	if v == "" || v == recipe.CriteriaAnyValue {
		return anyValue
	}
	return v
}

// profileCell renders the tuning profile accelerator (the componentRef override)
// when it differs from the Accelerator column value, else NotApplicable.
func profileCell(overrides map[string]any, accelCell string) string {
	p := overrideString(overrides, "accelerator")
	if p == "" || p == accelCell {
		return NotApplicable
	}
	return p
}

// overrideString reads a string-valued override, coercing non-strings via Sprint.
func overrideString(overrides map[string]any, key string) string {
	if overrides == nil {
		return ""
	}
	v, ok := overrides[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return fmt.Sprint(v)
}

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

package uatbroker

import (
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ExpandSchedule builds the ordered per-reservation nightly run schedule.
// For each reservation it emits, in order:
//
//  1. the tip-of-main cell (when includeMain is true), then
//  2. up to previousN of the newest STABLE releases, in DESCENDING semver
//     order.
//
// Each cell carries the subset of the reservation's nightly intents ELIGIBLE
// at that cell's version (Cell.Intents) and the intents a harness-compat
// floor dropped (Cell.Skipped), both computed by EligibleNightlyIntents
// against compat (nil means no floors). The controller iterates a cell's own
// Intents, so a gated intent never dispatches for that release, and announces
// each Skipped entry.
//
// rawTags are unsorted tag strings (e.g. the output of `git tag -l 'v*'`);
// pre-release tags (those with a semver pre-release segment) and tags that
// do not parse as semver are dropped. Cells are ordered newest-first so the
// nightly controller, when its time-box closes, simply stops at the cursor —
// which drops the OLDEST releases first, as DC1 requires. A negative
// previousN is treated as zero.
func ExpandSchedule(reservations []Reservation, compat *Compat, rawTags []string, includeMain bool, previousN int) map[string][]Cell {
	if previousN < 0 {
		previousN = 0
	}
	stable := sortedStableDescending(rawTags)
	if previousN < len(stable) {
		stable = stable[:previousN]
	}

	out := make(map[string][]Cell, len(reservations))
	for i := range reservations {
		res := &reservations[i]
		cells := make([]Cell, 0, len(stable)+1)
		if includeMain {
			intents, skipped := res.EligibleNightlyIntents(compat, "", true)
			cells = append(cells, Cell{
				Reservation: res.Name, AICRVersion: "", IsMain: true,
				Intents: intents, Skipped: skipped,
			})
		}
		for _, tag := range stable {
			intents, skipped := res.EligibleNightlyIntents(compat, tag, false)
			cells = append(cells, Cell{
				Reservation: res.Name, AICRVersion: tag, IsMain: false,
				Intents: intents, Skipped: skipped,
			})
		}
		out[res.Name] = cells
	}
	return out
}

// EligibleNightlyIntents splits the reservation's nightly intents
// (NightlyIntentsOrDefault) into those that run at aicrVersion and those a
// harness-compat floor skips. The tip-of-main cell (isMain) runs every listed
// intent: it is built from source against the fixtures it ships with. A
// release cell skips an intent whose compat.FloorFor(r.Cloud, intent) is
// NEWER than aicrVersion (a tag >= the floor runs, a tag below it is skipped).
//
// Fail-OPEN on an unparseable cell version (or a floor that bypassed
// Validate): the intent stays eligible rather than being silently dropped.
// Silently skipping a cell is the dangerous direction (hidden coverage loss);
// running a spurious cell is self-announcing.
func (r *Reservation) EligibleNightlyIntents(compat *Compat, aicrVersion string, isMain bool) ([]string, []SkippedIntent) {
	intents := r.NightlyIntentsOrDefault()
	if isMain || compat == nil || len(compat.Floors) == 0 {
		return intents, nil
	}
	cellV, err := semver.NewVersion(aicrVersion)
	if err != nil {
		return intents, nil // fail open — see doc comment
	}
	out := make([]string, 0, len(intents))
	var skipped []SkippedIntent
	for _, intent := range intents {
		floor, gated := compat.FloorFor(r.Cloud, intent)
		if !gated {
			out = append(out, intent)
			continue
		}
		floorV, err := semver.NewVersion(floor.MinRelease)
		if err != nil || !cellV.LessThan(floorV) {
			out = append(out, intent) // tag >= floor (or unparseable floor: fail open)
			continue
		}
		skipped = append(skipped, SkippedIntent{
			Intent: intent, Floor: floor.MinRelease, Lane: floor.Lane, Reason: floor.Reason,
		})
	}
	return out, skipped
}

// sortedStableDescending parses rawTags, drops unparseable and pre-release
// tags, and returns the remaining stable tags' ORIGINAL strings (e.g.
// "v1.2.3") in descending semver order.
func sortedStableDescending(rawTags []string) []string {
	versions := stableVersionsDescending(rawTags)
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.Original())
	}
	return out
}

// stableVersionsDescending parses rawTags, drops unparseable, pre-release,
// and normalized-duplicate tags, and returns the rest in descending order.
func stableVersionsDescending(rawTags []string) []*semver.Version {
	versions := make([]*semver.Version, 0, len(rawTags))
	seen := make(map[string]bool, len(rawTags))
	for _, t := range rawTags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		v, err := semver.NewVersion(t)
		if err != nil {
			continue // not semver — drop
		}
		if v.Prerelease() != "" {
			continue // pre-release — drop
		}
		if seen[v.String()] {
			continue // normalized duplicate (e.g. "v1.2" and "v1.2.0") — drop
		}
		seen[v.String()] = true
		versions = append(versions, v)
	}
	sort.Sort(sort.Reverse(semver.Collection(versions)))
	return versions
}

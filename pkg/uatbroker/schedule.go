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
// at that cell's version (Cell.Intents), computed by EligibleNightlyIntents:
// the main cell runs every listed intent, while a release cell drops any
// intent whose nightly-intent-min-versions gate is newer than the tag. The
// controller iterates a cell's own Intents, so a gated intent simply never
// dispatches for that release.
//
// rawTags are unsorted tag strings (e.g. the output of `git tag -l 'v*'`);
// pre-release tags (those with a semver pre-release segment) and tags that
// do not parse as semver are dropped. Cells are ordered newest-first so the
// nightly controller, when its time-box closes, simply stops at the cursor —
// which drops the OLDEST releases first, as DC1 requires. A negative
// previousN is treated as zero.
func ExpandSchedule(reservations []Reservation, rawTags []string, includeMain bool, previousN int) map[string][]Cell {
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
			cells = append(cells, Cell{
				Reservation: res.Name, AICRVersion: "", IsMain: true,
				Intents: res.EligibleNightlyIntents("", true),
			})
		}
		for _, tag := range stable {
			cells = append(cells, Cell{
				Reservation: res.Name, AICRVersion: tag, IsMain: false,
				Intents: res.EligibleNightlyIntents(tag, false),
			})
		}
		out[res.Name] = cells
	}
	return out
}

// EligibleNightlyIntents returns the subset of the reservation's nightly
// intents (NightlyIntentsOrDefault) that should run at aicrVersion. The
// tip-of-main cell (isMain) runs every listed intent — it is built from source
// and carries the newest fixes. A release cell drops any intent whose
// nightly-intent-min-versions entry is NEWER than aicrVersion (semver
// comparison; a tag >= the min runs, a tag below it is dropped).
//
// Fail-OPEN on the (should-not-happen) unparseable version: an intent stays
// eligible rather than being silently dropped, because the schedule only ever
// feeds this valid semver release tags and Validate rejects unparseable
// min-versions at parse time. Silently skipping a cell is the dangerous
// direction (hidden coverage loss); running a spurious cell is self-announcing.
func (r *Reservation) EligibleNightlyIntents(aicrVersion string, isMain bool) []string {
	intents := r.NightlyIntentsOrDefault()
	if isMain || len(r.NightlyIntentMinVersions) == 0 {
		return intents
	}
	cellV, err := semver.NewVersion(aicrVersion)
	if err != nil {
		return intents // fail open — see doc comment
	}
	out := make([]string, 0, len(intents))
	for _, intent := range intents {
		minStr, gated := r.NightlyIntentMinVersions[intent]
		if !gated {
			out = append(out, intent)
			continue
		}
		minV, err := semver.NewVersion(minStr)
		if err != nil {
			out = append(out, intent) // fail open — Validate should have caught this
			continue
		}
		if !cellV.LessThan(minV) {
			out = append(out, intent) // tag >= min: eligible
		}
	}
	return out
}

// sortedStableDescending parses rawTags, drops unparseable and pre-release
// tags, and returns the remaining stable tags' ORIGINAL strings (e.g.
// "v1.2.3") in descending semver order.
func sortedStableDescending(rawTags []string) []string {
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

	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.Original())
	}
	return out
}

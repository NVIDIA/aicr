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
// rawTags are unsorted tag strings (e.g. the output of `git tag -l 'v*'`);
// pre-release tags (those with a semver pre-release segment) and tags that
// do not parse as semver are dropped. Cells are ordered newest-first so the
// nightly controller, when its time-box closes, simply stops at the cursor —
// which drops the OLDEST releases first, as DC1 requires. A negative
// previousN is treated as zero.
func ExpandSchedule(reservations, rawTags []string, includeMain bool, previousN int) map[string][]Cell {
	if previousN < 0 {
		previousN = 0
	}
	stable := sortedStableDescending(rawTags)
	if previousN < len(stable) {
		stable = stable[:previousN]
	}

	out := make(map[string][]Cell, len(reservations))
	for _, res := range reservations {
		cells := make([]Cell, 0, len(stable)+1)
		if includeMain {
			cells = append(cells, Cell{Reservation: res, AICRVersion: "", IsMain: true})
		}
		for _, tag := range stable {
			cells = append(cells, Cell{Reservation: res, AICRVersion: tag, IsMain: false})
		}
		out[res] = cells
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

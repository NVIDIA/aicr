// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// publishedSlack is how much earlier than the pinned version a candidate must
// have been published to be withheld. A batch-regenerated Helm index stamps
// every version within one second in arbitrary order (all 459 eks-charts
// entries carry 2026-10-05T21:16:09Z), so ordering that close is noise; the
// lines this screen exists for predate their pins by months (kai-scheduler
// v0.20.1 by 259 days, agentgateway v2.2.1 by 191).
const publishedSlack = 24 * time.Hour

// screening is what screen decided about one lookup's candidates. offered and
// predating are safest first; undated names offered versions that had no
// publish date to check.
type screening struct {
	offered        []Candidate
	predating      []Candidate
	undated        []string
	currentUndated bool
}

// screen splits a lookup's candidates into the versions the report offers and
// the ones it withholds for having been published before the pinned version.
//
// Renovate offers the highest version per bucket, and a registry can carry a
// frozen or retracted line numbered above the one upstream maintains: the
// agentgateway v2.2.x tags are the kgateway lineage, and kai-scheduler
// retracted v0.20.1 while v0.18.x is its Latest (#2791). Both were published
// months before the pins they would replace. Publish order is a signal, not a
// verdict -- a maintained parallel line is withheld too when its newest release
// is older than the pin, as when the pin is itself a recent patch on an older
// line -- which is why withheld versions stay in the report for a reviewer.
//
// Each update type is walked highest version first: predating versions are
// withheld until the first that is not, which is that type's offer, and every
// version below it is dropped unexamined. With separateMultipleMajor and
// separateMultipleMinor Renovate offers one candidate per release line, so this
// is what turns a frozen top line back into the maintained one beneath it while
// keeping the one-per-type shape the report has always had.
//
// A version without a publish date cannot be shown to predate anything, so it
// is offered, as it was before this screen existed, and named in undated.
func screen(l Lookup) screening {
	s := screening{currentUndated: l.CurrentPublished.IsZero()}
	cutoff := l.CurrentPublished.Add(-publishedSlack)

	byType := map[string][]Candidate{}
	for _, c := range l.Candidates {
		byType[c.UpdateType] = append(byType[c.UpdateType], c)
	}
	for _, cands := range byType {
		sort.Slice(cands, func(i, j int) bool { return versionLess(cands[j].Version, cands[i].Version) })
		for _, c := range cands {
			dated := !s.currentUndated && !c.Published.IsZero()
			if dated && c.Published.Before(cutoff) {
				s.predating = append(s.predating, c)
				continue
			}
			if !s.currentUndated && !dated {
				s.undated = append(s.undated, c.Version)
			}
			s.offered = append(s.offered, c)
			break
		}
	}

	sort.Slice(s.offered, func(i, j int) bool { return candidateLess(s.offered[i], s.offered[j]) })
	sort.Slice(s.predating, func(i, j int) bool { return candidateLess(s.predating[i], s.predating[j]) })
	sort.Slice(s.undated, func(i, j int) bool { return versionLess(s.undated[i], s.undated[j]) })
	return s
}

// unscreened says why some offered version was not checked against the pin,
// or returns empty when every one was.
func (s screening) unscreened(current string) string {
	switch {
	case s.currentUndated:
		return fmt.Sprintf("no publish date for current %s; no candidate was checked against it", current)
	case len(s.undated) > 0:
		return fmt.Sprintf("no publish date for %s; offered without the check", strings.Join(s.undated, ", "))
	}
	return ""
}

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
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// schemaVersion is the contract between this tool and the
// aicr-reviewing-component-drift skill. Bump it on any incompatible change to
// the emitted JSON. 2 added the publish-date screen: Latest stopped being
// Renovate's highest version, and a chart whose every candidate was withheld
// moved from Drift to Withheld, which a version-1 consumer would silently miss.
const schemaVersion = 2

// Row is one chart's drift. Components lists every AICR component sharing the
// chart pin — the OpenShift twins (prometheus-adapter, nvidia-dra-driver-gpu,
// k8s-nim-operator) always move together, so they collapse into one row.
//
// Latest is the highest-ranked candidate screen offered, and Alternatives the
// other offers, smaller step first, omitted when there are none. Latest is
// never repeated there: listing it twice would read as two distinct options.
// Predating lists what screen withheld for having been published before
// Current. Unscreened is non-empty when an offered version went unchecked for
// want of a publish date, so its absence means every offer was checked. A row
// in Report.Withheld has Predating and no Latest.
type Row struct {
	Components       []string    `json:"components"`
	Chart            string      `json:"chart"`
	Registry         string      `json:"registry"`
	Datasource       string      `json:"datasource"`
	Current          string      `json:"current"`
	CurrentPublished time.Time   `json:"currentPublished,omitzero"`
	Latest           string      `json:"latest,omitempty"`
	UpdateType       string      `json:"updateType,omitempty"`
	LatestPublished  time.Time   `json:"latestPublished,omitzero"`
	Alternatives     []Candidate `json:"alternatives,omitempty"`
	Predating        []Candidate `json:"predating,omitempty"`
	Unscreened       string      `json:"unscreened,omitempty"`
}

// Unresolved is a pin Renovate could not look up. It is reported as unknown and
// deliberately never folded into Current: a drift report that reads clean
// because a lookup failed is the failure mode worth engineering against.
type Unresolved struct {
	Components []string `json:"components"`
	Chart      string   `json:"chart"`
	Reason     string   `json:"reason"`
}

// Summary counts pins in Tracked and chart rows in the rest. Every tracked pin
// lands in exactly one of Drift, Current, Unresolved, or Withheld. Unscreened
// counts the Drift rows whose Unscreened is set, a subset of Behind; a Withheld
// row is never unscreened, since withholding needs every date.
type Summary struct {
	Tracked    int `json:"tracked"`
	Behind     int `json:"behind"`
	Unresolved int `json:"unresolved"`
	Withheld   int `json:"withheld"`
	Unscreened int `json:"unscreened"`
}

// Report is the drift-report.json artifact. Withheld holds charts whose every
// candidate was published before the pinned version: nothing is proposed, and a
// reviewer decides explicitly whether any of them is a real upgrade.
type Report struct {
	SchemaVersion int          `json:"schemaVersion"`
	GeneratedAt   string       `json:"generatedAt,omitempty"`
	Commit        string       `json:"commit,omitempty"`
	RunURL        string       `json:"runUrl,omitempty"`
	Summary       Summary      `json:"summary"`
	Drift         []Row        `json:"drift"`
	Withheld      []Row        `json:"withheld"`
	Current       []string     `json:"current"`
	Unresolved    []Unresolved `json:"unresolved"`
}

// Meta carries run identity. GeneratedAt is injected rather than read from the
// clock so the tool is reproducible under test.
type Meta struct {
	GeneratedAt string
	Commit      string
	RunURL      string
}

type groupKey struct{ chart, registry, current string }

// newRow builds a chart's row from what screen decided. The last offer is the
// highest-ranked, since screen returns offers safest first.
func newRow(p Pin, l Lookup, s screening) *Row {
	row := &Row{
		Components:       []string{p.Component},
		Chart:            p.Chart,
		Registry:         p.Repository,
		Datasource:       p.Datasource,
		Current:          p.Version,
		CurrentPublished: l.CurrentPublished,
		Predating:        s.predating,
		Unscreened:       s.unscreened(p.Version),
	}
	if n := len(s.offered); n > 0 {
		latest := s.offered[n-1]
		row.Latest, row.UpdateType, row.LatestPublished = latest.Version, latest.UpdateType, latest.Published
		if n > 1 {
			row.Alternatives = s.offered[:n-1]
		}
	}
	return row
}

// BuildReport joins the registry's annotated pins against Renovate's lookups.
func BuildReport(pins []Pin, lookups map[string]Lookup, meta Meta) (Report, error) {
	r := Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   meta.GeneratedAt,
		Commit:        meta.Commit,
		RunURL:        meta.RunURL,
		Drift:         []Row{},
		Withheld:      []Row{},
		Current:       []string{},
		Unresolved:    []Unresolved{},
	}

	drift := map[groupKey]*Row{}
	withheld := map[groupKey]*Row{}
	unresolved := map[groupKey]*Unresolved{}
	// resolved counts pins whose DepName appeared in lookups — including
	// Problem-bearing ones — so the guard below only fires on total join
	// failure (Renovate's report shape changed or the run failed outright),
	// not on any single dep's lookup failure.
	resolved := 0

	for _, p := range pins {
		if p.Repository == "" {
			continue // manifest-only: nothing upstream to track
		}
		r.Summary.Tracked++
		key := groupKey{chart: p.Chart, registry: p.Repository, current: p.Version}
		l, ok := lookups[p.DepName]
		switch {
		case !ok:
			appendUnresolved(unresolved, key, p, "absent from Renovate report")
		case l.Problem != "":
			resolved++
			appendUnresolved(unresolved, key, p, l.Problem)
		case l.Current == "":
			// dep.Updates present but dep.CurrentValue absent (e.g. an
			// empty `updates` array) parses to Current, Latest, and Problem
			// all "". Nothing else below distinguishes that from "current"
			// — catch it here, before the mismatch and update branches, so
			// a pin Renovate never actually resolved is never folded into
			// Current.
			resolved++
			appendUnresolved(unresolved, key, p, fmt.Sprintf(
				"Renovate reported no currentValue for depName %q; the lookup did not resolve this pin", p.DepName))
		case l.Current != p.Version:
			// A depName shared by two pins (the OpenShift twins) but reported by
			// Renovate at a currentValue that disagrees with this pin: the map in
			// ParseRenovateReport kept a different dep entry than the one this pin
			// expected. Report it as unknown rather than silently joining this pin
			// to a lookup that was not run against it.
			resolved++
			appendUnresolved(unresolved, key, p, fmt.Sprintf(
				"Renovate reported currentValue %q for depName %q; this pin is %q (duplicate depName)",
				l.Current, p.DepName, p.Version))
		case len(l.Candidates) == 0:
			resolved++
			r.Current = append(r.Current, p.Component)
		default:
			resolved++
			s := screen(l)
			rows := drift
			if len(s.offered) == 0 {
				rows = withheld
			}
			if row, seen := rows[key]; seen {
				row.Components = append(row.Components, p.Component)
				continue
			}
			rows[key] = newRow(p, l, s)
		}
	}

	if r.Summary.Tracked == 0 || resolved == 0 {
		return Report{}, errors.New(errors.ErrCodeInternal,
			"no registry-chart dep resolved: the Renovate run failed or its report shape changed — "+
				"refusing to report a clean fleet")
	}

	r.Drift = sortedRows(drift)
	r.Withheld = sortedRows(withheld)
	for _, u := range unresolved {
		sort.Strings(u.Components)
		r.Unresolved = append(r.Unresolved, *u)
	}
	sort.Slice(r.Unresolved, func(i, j int) bool {
		return r.Unresolved[i].Components[0] < r.Unresolved[j].Components[0]
	})
	sort.Strings(r.Current)

	r.Summary.Behind = len(r.Drift)
	r.Summary.Unresolved = len(r.Unresolved)
	r.Summary.Withheld = len(r.Withheld)
	for _, row := range r.Drift {
		if row.Unscreened != "" {
			r.Summary.Unscreened++
		}
	}
	return r, nil
}

func sortedRows(m map[groupKey]*Row) []Row {
	out := make([]Row, 0, len(m))
	for _, row := range m {
		sort.Strings(row.Components)
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Components[0] < out[j].Components[0] })
	return out
}

func appendUnresolved(m map[groupKey]*Unresolved, key groupKey, p Pin, reason string) {
	if u, seen := m[key]; seen {
		u.Components = append(u.Components, p.Component)
		return
	}
	m[key] = &Unresolved{Components: []string{p.Component}, Chart: p.Chart, Reason: reason}
}

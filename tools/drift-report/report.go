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
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// schemaVersion is the contract between this tool and the
// aicr-reviewing-component-drift skill. Bump it on any incompatible change to
// the emitted JSON.
const schemaVersion = 1

// Row is one chart's drift. Components lists every AICR component sharing the
// chart pin — the OpenShift twins (prometheus-adapter, nvidia-dra-driver-gpu,
// k8s-nim-operator) always move together, so they collapse into one row.
type Row struct {
	Components []string `json:"components"`
	Chart      string   `json:"chart"`
	Registry   string   `json:"registry"`
	Datasource string   `json:"datasource"`
	Current    string   `json:"current"`
	Latest     string   `json:"latest"`
	UpdateType string   `json:"updateType"`
}

// Unresolved is a pin Renovate could not look up. It is reported as unknown and
// deliberately never folded into Current: a drift report that reads clean
// because a lookup failed is the failure mode worth engineering against.
type Unresolved struct {
	Components []string `json:"components"`
	Chart      string   `json:"chart"`
	Reason     string   `json:"reason"`
}

type Summary struct {
	Tracked    int `json:"tracked"`
	Behind     int `json:"behind"`
	Unresolved int `json:"unresolved"`
}

type Report struct {
	SchemaVersion int          `json:"schemaVersion"`
	GeneratedAt   string       `json:"generatedAt,omitempty"`
	Commit        string       `json:"commit,omitempty"`
	RunURL        string       `json:"runUrl,omitempty"`
	Summary       Summary      `json:"summary"`
	Drift         []Row        `json:"drift"`
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

// BuildReport joins the registry's annotated pins against Renovate's lookups.
func BuildReport(pins []Pin, lookups map[string]Lookup, meta Meta) (Report, error) {
	r := Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   meta.GeneratedAt,
		Commit:        meta.Commit,
		RunURL:        meta.RunURL,
		Drift:         []Row{},
		Current:       []string{},
		Unresolved:    []Unresolved{},
	}

	drift := map[groupKey]*Row{}
	unresolved := map[groupKey]*Unresolved{}
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
		case l.Latest == "":
			resolved++
			r.Current = append(r.Current, p.Component)
		default:
			resolved++
			if row, seen := drift[key]; seen {
				row.Components = append(row.Components, p.Component)
				continue
			}
			drift[key] = &Row{
				Components: []string{p.Component},
				Chart:      p.Chart,
				Registry:   p.Repository,
				Datasource: p.Datasource,
				Current:    p.Version,
				Latest:     l.Latest,
				UpdateType: l.UpdateType,
			}
		}
	}

	if r.Summary.Tracked > 0 && resolved == 0 {
		return Report{}, errors.New(errors.ErrCodeInternal,
			"no registry-chart dep resolved: the Renovate run failed or its report shape changed — "+
				"refusing to report a clean fleet")
	}

	for _, row := range drift {
		sort.Strings(row.Components)
		r.Drift = append(r.Drift, *row)
	}
	sort.Slice(r.Drift, func(i, j int) bool { return r.Drift[i].Components[0] < r.Drift[j].Components[0] })
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
	return r, nil
}

func appendUnresolved(m map[groupKey]*Unresolved, key groupKey, p Pin, reason string) {
	if u, seen := m[key]; seen {
		u.Components = append(u.Components, p.Component)
		return
	}
	m[key] = &Unresolved{Components: []string{p.Component}, Chart: p.Chart, Reason: reason}
}

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
	"encoding/json"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Candidate is one version Renovate offers for a pin. Renovate emits at most
// one per update type.
type Candidate struct {
	Version    string `json:"version"`
	UpdateType string `json:"updateType"`
}

// Lookup is what Renovate found for one annotated pin. An empty Latest means
// the pin is current; a non-empty Problem means Renovate could not resolve it,
// which is reported as unknown and never as current.
//
// Candidates carries every recognized update, not just the winning one.
// Reporting only Latest hid the safer step whenever both existed: in the
// 2026-09-28 run kube-prometheus-stack offered minor 84.5.0 next to major
// 91.5.2, and a reviewer reading the digest saw only the major (#2791). Latest
// still names the highest-ranked candidate, because Summary.Behind counts rows
// by it and changing the winner would move the headline counts.
type Lookup struct {
	DepName    string
	Current    string
	Latest     string
	UpdateType string
	Candidates []Candidate
	Problem    string
}

// registryChartDepType is the depTypeTemplate set by the custom manager in
// .github/renovate.json5. Deps of any other type belong to another manager.
const registryChartDepType = "registry-chart"

// renovateUpdate is one entry in a dep's `updates` array.
type renovateUpdate struct {
	NewValue   string `json:"newValue"`
	UpdateType string `json:"updateType"`
}

type rawReport struct {
	Repositories map[string]struct {
		PackageFiles map[string][]struct {
			PackageFile string `json:"packageFile"`
			Deps        []struct {
				DepName      string `json:"depName"`
				DepType      string `json:"depType"`
				Datasource   string `json:"datasource"`
				CurrentValue string `json:"currentValue"`
				SkipReason   string `json:"skipReason"`
				Warnings     []struct {
					Message string `json:"message"`
				} `json:"warnings"`
				// Pointer so an absent `updates` key (Renovate lookup never ran)
				// is distinguishable from a present, empty array (lookup ran,
				// dep is current) — a nil slice decodes identically to both.
				Updates *[]renovateUpdate `json:"updates"`
			} `json:"deps"`
		} `json:"packageFiles"`
	} `json:"repositories"`
}

// updateRank orders update types by distance traveled, so a dep offering both
// a patch and a minor reports the minor. Renovate emits one entry per type.
// Unknown types (rollback, replacement, lockFileMaintenance, bump, pinDigest)
// are not selectable — a rollback is not forward drift and must never be
// reported as Latest.
var updateRank = map[string]int{"digest": 1, "pin": 2, "patch": 3, "minor": 4, "major": 5}

// ParseRenovateReport extracts the registry-chart deps from a Renovate report
// written with RENOVATE_REPORT_TYPE=file.
func ParseRenovateReport(data []byte) (map[string]Lookup, error) {
	var rr rawReport
	if err := json.Unmarshal(data, &rr); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse Renovate report", err)
	}
	out := make(map[string]Lookup)
	for _, repo := range rr.Repositories {
		for _, files := range repo.PackageFiles {
			for _, file := range files {
				for _, dep := range file.Deps {
					if dep.DepType != registryChartDepType {
						continue
					}
					l := Lookup{DepName: dep.DepName, Current: dep.CurrentValue}
					var msgs []string
					if dep.SkipReason != "" {
						msgs = append(msgs, dep.SkipReason)
					}
					for _, w := range dep.Warnings {
						msgs = append(msgs, w.Message)
					}
					if dep.Updates == nil {
						// No `updates` key at all: the Renovate lookup did not run
						// for this dep. Report it as unresolved, never as current.
						msgs = append(msgs, "no updates array: Renovate lookup did not run for this dep")
					}
					var updates []renovateUpdate
					if dep.Updates != nil {
						updates = *dep.Updates
					}
					best := 0
					hasRecognizedUpdate := false
					var unsupportedType string
					var malformed []string
					for _, u := range updates {
						if u.NewValue == "" {
							// Missing newValue: the update can never be selected, and
							// it must not be silently dropped either — an unresolved
							// pin is reported as unknown, never as current.
							malformed = append(malformed, "update missing newValue")
							continue
						}
						if u.UpdateType == "" {
							malformed = append(malformed, "update missing updateType")
							continue
						}
						r, ok := updateRank[u.UpdateType]
						if !ok {
							// Unrecognized type; remember it but don't select it.
							if unsupportedType == "" {
								unsupportedType = u.UpdateType
							}
							continue
						}
						hasRecognizedUpdate = true
						l.Candidates = append(l.Candidates, Candidate{Version: u.NewValue, UpdateType: u.UpdateType})
						if r >= best {
							best, l.Latest, l.UpdateType = r, u.NewValue, u.UpdateType
						}
					}
					// Safest-first, by the same rank that picks Latest. Renovate's
					// array order is not contractual, and an unstable order here
					// would churn the committed artifact between identical runs --
					// the reproducibility the report is read for.
					//
					// Version breaks a rank tie so the order is total. Renovate
					// emits at most one update per type, which would make ties
					// unreachable, but that is its behavior rather than a
					// guarantee -- and falling back to array order on a tie is
					// exactly the non-contractual ordering this sort exists to
					// remove.
					sort.Slice(l.Candidates, func(i, j int) bool {
						a, b := l.Candidates[i], l.Candidates[j]
						if ra, rb := updateRank[a.UpdateType], updateRank[b.UpdateType]; ra != rb {
							return ra < rb
						}
						return a.Version < b.Version
					})
					// If no recognized updates exist, surface the unsupported type and
					// any malformed entries as diagnostics instead of leaving the pin
					// looking current.
					if !hasRecognizedUpdate {
						if unsupportedType != "" {
							msgs = append(msgs, "unsupported update type: "+unsupportedType)
						}
						msgs = append(msgs, malformed...)
						l.Latest = ""
						l.UpdateType = ""
					}
					l.Problem = strings.Join(msgs, ": ")
					out[dep.DepName] = l
				}
			}
		}
	}
	return out, nil
}

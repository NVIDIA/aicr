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
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Lookup is what Renovate found for one annotated pin. An empty Latest means
// the pin is current; a non-empty Problem means Renovate could not resolve it,
// which is reported as unknown and never as current.
type Lookup struct {
	DepName    string
	Current    string
	Latest     string
	UpdateType string
	Problem    string
}

// registryChartDepType is the depTypeTemplate set by the custom manager in
// .github/renovate.json5. Deps of any other type belong to another manager.
const registryChartDepType = "registry-chart"

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
				Updates []struct {
					NewValue   string `json:"newValue"`
					UpdateType string `json:"updateType"`
				} `json:"updates"`
			} `json:"deps"`
		} `json:"packageFiles"`
	} `json:"repositories"`
}

// updateRank orders update types by distance traveled, so a dep offering both
// a patch and a minor reports the minor. Renovate emits one entry per type.
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
					l.Problem = strings.Join(msgs, ": ")
					best := 0
					for _, u := range dep.Updates {
						if u.NewValue == "" {
							continue
						}
						if r := updateRank[u.UpdateType]; r >= best {
							best, l.Latest, l.UpdateType = r, u.NewValue, u.UpdateType
						}
					}
					out[dep.DepName] = l
				}
			}
		}
	}
	return out, nil
}

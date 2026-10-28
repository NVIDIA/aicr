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

package aicrjob

import (
	"encoding/json"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

// UpgradeReport is the subset of `aicr upgrade-check --format json` the
// operator reads. AICR publishes no schema for it; the golden
// fixtures in testdata pin its v1.0.0 shape. The operator runs upgrade-check
// with --fail-on-error=false: a component that needs attention is a normal
// verdict, not a failed Job.
type UpgradeReport struct {
	Deployer            string            `json:"deployer"`
	ObjectNamesCompared bool              `json:"objectNamesCompared"`
	Components          []ComponentChange `json:"components"`
	Summary             UpgradeSummary    `json:"summary"`
}

// ComponentChange is one component's assessed change.
type ComponentChange struct {
	Component string `json:"component"`
	// Change is "version", "added" or "removed" at v1.0.0.
	Change string `json:"change"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	// Verdict is set for version changes: safe, manual, blocked, unknown or
	// unversioned (NVIDIA/aicr pkg/upgrade/record.go).
	Verdict     string `json:"verdict,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Explanation string `json:"explanation,omitempty"`
	Breaking    bool   `json:"breaking"`
	Downgrade   bool   `json:"downgrade"`
	FailsRun    bool   `json:"failsRun"`
	StoppedAt   string `json:"stoppedAt,omitempty"`
	// Summary, Precondition and Steps are the transition record's
	// instructions for the assessed deployer. The operator performs only the
	// apply; approving takes on the rest.
	Summary      string        `json:"summary,omitempty"`
	Precondition string        `json:"precondition,omitempty"`
	Steps        []UpgradeStep `json:"steps,omitempty"`
}

// UpgradeStep is one step of a transition record.
type UpgradeStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Reason      string `json:"reason,omitempty"`
}

// UpgradeSummary counts components and those needing attention.
type UpgradeSummary struct {
	Components int `json:"components"`
	Failing    int `json:"failing"`
}

// upgradeRequired are the report fields the operator cannot work without.
var upgradeRequired = []string{"deployer", "components", "summary"}

// ParseUpgradeReport decodes an upgrade-check report produced for deployer.
// Unknown fields are ignored; a missing required field, a report for another
// deployer, or a summary that disagrees with the component list is rejected.
func ParseUpgradeReport(data []byte, deployer string) (*UpgradeReport, error) {
	if err := requireFields(data, "upgrade-check report", upgradeRequired); err != nil {
		return nil, err
	}
	r := &UpgradeReport{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, unsupported("decode upgrade-check report: %v", err)
	}
	if r.Deployer != deployer {
		return nil, unsupported("upgrade-check report is for deployer %q, want %q", r.Deployer, deployer)
	}
	if r.Summary.Components != len(r.Components) {
		return nil, unsupported("upgrade-check summary counts %d components, report lists %d", r.Summary.Components, len(r.Components))
	}
	for i := range r.Components {
		if r.Components[i].Component == "" {
			return nil, unsupported("upgrade-check component %d has no name", i)
		}
	}
	return r, nil
}

// Assessment maps a change onto the rollout step assessment. An
// added or removed component has no verdict and is NotApplicable; its
// approval need comes from the step operation. A version change without a
// verdict, or with one this operator does not recognize, is Unknown, which
// requires approval: unknown is not safe.
func (c *ComponentChange) Assessment() v1alpha1.Assessment {
	switch c.Verdict {
	case "":
		if c.Change == "version" {
			return v1alpha1.AssessmentUnknown
		}
		return v1alpha1.AssessmentNotApplicable
	case "safe":
		return v1alpha1.AssessmentSafe
	case "manual":
		return v1alpha1.AssessmentManual
	case "blocked":
		return v1alpha1.AssessmentBlocked
	case "unversioned":
		return v1alpha1.AssessmentUnversioned
	default:
		return v1alpha1.AssessmentUnknown
	}
}

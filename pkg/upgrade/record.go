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

package upgrade

// ComponentUpgradesKind is the kind expected on a ComponentUpgrades document.
const ComponentUpgradesKind = "ComponentUpgrades"

// Verdict describes whether a version transition can be performed in place.
//
// Only safe, manual, and blocked are authorable. Unknown and unversioned are
// computed by the matcher and MUST NOT appear in a record: unknown is a gap in
// the data, fixed by authoring a record, while unversioned is a gap in the
// inputs, fixed by pinning something comparable.
type Verdict string

const (
	VerdictSafe        Verdict = "safe"
	VerdictManual      Verdict = "manual"
	VerdictBlocked     Verdict = "blocked"
	VerdictUnknown     Verdict = "unknown"
	VerdictUnversioned Verdict = "unversioned"
)

// Authorable reports whether v may appear in a record file.
func (v Verdict) Authorable() bool {
	switch v { //nolint:exhaustive // unknown and unversioned are computed, never authored; see the Verdict doc comment
	case VerdictSafe, VerdictManual, VerdictBlocked:
		return true
	default:
		return false
	}
}

// ComponentUpgrades is one component's transition records.
type ComponentUpgrades struct {
	APIVersion  string       `yaml:"apiVersion"`
	Kind        string       `yaml:"kind"`
	Component   string       `yaml:"component"`
	Transitions []Transition `yaml:"transitions,omitempty"`
	Replaces    *Replaces    `yaml:"replaces,omitempty"`
}

// Transition describes one version boundary and what crossing it requires.
type Transition struct {
	From              string             `yaml:"from"`
	To                string             `yaml:"to"`
	Verdict           Verdict            `yaml:"verdict"`
	VerifiedBy        string             `yaml:"verifiedBy,omitempty"`
	Summary           string             `yaml:"summary"`
	Precondition      string             `yaml:"precondition,omitempty"`
	Reversible        *bool              `yaml:"reversible,omitempty"`
	ReversibleNotes   string             `yaml:"reversibleNotes,omitempty"`
	StepsByDeployer   []StepGroup        `yaml:"stepsByDeployer,omitempty"`
	Hooks             []Hook             `yaml:"hooks,omitempty"`
	AffectedResources []AffectedResource `yaml:"affectedResources,omitempty"`
	References        []string           `yaml:"references,omitempty"`
}

// Replaces names the component this one supersedes, joining the removed and
// added registry rows into one migration.
type Replaces struct {
	Component       string      `yaml:"component"`
	Verdict         Verdict     `yaml:"verdict"`
	VerifiedBy      string      `yaml:"verifiedBy,omitempty"`
	Summary         string      `yaml:"summary"`
	StepsByDeployer []StepGroup `yaml:"stepsByDeployer,omitempty"`
}

// StepGroup is one ordered instruction sequence for a set of deployers. A
// group omitting Deployers covers exactly those deployers no explicit group
// claims.
type StepGroup struct {
	Deployers []string `yaml:"deployers,omitempty"`
	Steps     []Step   `yaml:"steps"`
}

// Step is one operator action. ID is unique within its group; the same logical
// action may reuse an id across groups, since a consumer addresses a step as
// (deployer, id).
type Step struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	Reason      string `yaml:"reason,omitempty"`
}

// Hook references an AICR-authored migration manifest. Allowed on any verdict,
// including safe: a hook is AICR doing the work rather than the operator.
type Hook struct {
	File  string `yaml:"file"`
	Phase string `yaml:"phase"`
}

// AffectedResource drives the at-risk scan in online mode.
type AffectedResource struct {
	Group string   `yaml:"group"`
	Kinds []string `yaml:"kinds"`
}

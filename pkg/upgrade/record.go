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

import "github.com/Masterminds/semver/v3"

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
	switch v {
	case VerdictSafe, VerdictManual, VerdictBlocked:
		return true
	case VerdictUnknown, VerdictUnversioned:
		return false
	default:
		return false
	}
}

// StepLimitMinor is the one StepLimit value.
const StepLimitMinor = "minor"

// ComponentUpgrades is one component's transition records.
type ComponentUpgrades struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Component  string `yaml:"component"`

	// StepLimit, when StepLimitMinor, limits each move to the next
	// major.minor, ignoring patch versions. Empty means no limit. Under the
	// limit:
	//
	//   - a move into a new major needs a record whose `to` is in that major's
	//     first minor and whose `from` names the old major's last minor
	//   - a manual or blocked `to` may omit its ceiling, which is then the end
	//     of its floor's minor
	StepLimit string `yaml:"stepLimit,omitempty"`

	Transitions []Transition `yaml:"transitions,omitempty"`
	Replaces    *Replaces    `yaml:"replaces,omitempty"`
}

// Covers reports whether some transition's `to` range contains version. No
// record assesses an upgrade into a version outside every `to`: the matcher
// reports unknown, or blocked when the version lies past the highest ceiling.
// ADR-021 Decision 10 gates on exactly this. A version that is not semver,
// like a `to` that does not parse, covers nothing.
func (u *ComponentUpgrades) Covers(version string) bool {
	if u == nil {
		return false
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	for i := range u.Transitions {
		b, err := toBounds(u, &u.Transitions[i])
		if err == nil && b.contains(v) {
			return true
		}
	}
	return false
}

// AheadOf reports whether every transition's `to` range starts above version,
// so the record describes only boundaries version has not reached: guidance
// written ahead of the bump it describes. It fails toward false, the reading
// that holds version to Covers: on a nil or empty record, a version that is
// not semver, and a `to` that does not parse or names no floor.
func (u *ComponentUpgrades) AheadOf(version string) bool {
	if u == nil || len(u.Transitions) == 0 {
		return false
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	for i := range u.Transitions {
		b, err := parseBounds(u.Transitions[i].To)
		if err != nil || b.lower.unbounded || b.lower.ver == nil {
			return false
		}
		cmp := v.Compare(b.lower.ver)
		if cmp > 0 || (cmp == 0 && b.lower.inclusive) {
			return false
		}
	}
	return true
}

// Transition describes one version boundary, as the one-step moves from any
// version in From to any in To, and what crossing it requires.
type Transition struct {
	From       string  `yaml:"from"`
	To         string  `yaml:"to"`
	Verdict    Verdict `yaml:"verdict"`
	VerifiedBy string  `yaml:"verifiedBy,omitempty"`

	// Skips lists other transitions' floors whose guidance a cluster taking
	// this transition may skip. It must list each floor this one's moves pass
	// that is:
	//
	//   - inside To, above this transition's own floor
	//   - a manual or blocked transition's, whose From admits a version the
	//     move goes by
	//   - a RequiredStop's, whose From overlaps this one's
	//
	// A blocked transition sharing this one's floor counts unless this one is
	// blocked too. A move passing a floor it must list but does not is not
	// described by this transition.
	Skips []string `yaml:"skips,omitempty"`

	// RequiredStop reports whether a cluster taking this transition must stop
	// inside To and re-run the check, as for an automatic migration. It has
	// effect only on a safe transition, since a manual or blocked one already
	// stops a route.
	RequiredStop bool `yaml:"requiredStop,omitempty"`

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

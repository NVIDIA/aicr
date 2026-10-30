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

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// newConstraint is the only place IncludePrerelease is set. Masterminds
// otherwise derives prerelease inclusion per AND-group from whether the
// constraint text contains a prerelease, which makes Check disagree with
// numeric bounds. Every constraint this package builds goes through here.
func newConstraint(s string) (*semver.Constraints, error) {
	c, err := semver.NewConstraint(s)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid semver constraint %q", s), err)
	}
	c.IncludePrerelease = true
	return c, nil
}

// rangeEnd is one side of a range. An unbounded side has no limit in that
// direction, and its ver is nil. A bounded side with a nil ver is the zero
// value a rejected parseRange call returns, and is unsatisfiable.
type rangeEnd struct {
	ver       *semver.Version
	inclusive bool
	unbounded bool

	// family reports whether this exclusive upper side also leaves out the
	// prereleases of ver, a release, so it compares at X.Y.Z-0. parseRange
	// sets it only for a release named by <, so "<1.25.0" leaves out
	// 1.25.0-rc.1. Every other operator compares in plain semver order.
	family bool
}

// comparedAt returns the version b compares at.
func (b rangeEnd) comparedAt() *semver.Version {
	if !b.family || b.ver == nil {
		return b.ver
	}
	return semver.New(b.ver.Major(), b.ver.Minor(), b.ver.Patch(), "0", "")
}

// inclusiveEnd returns the inclusive range end at v.
func inclusiveEnd(v *semver.Version) rangeEnd {
	return rangeEnd{ver: v, inclusive: true}
}

// exclusiveEnd returns the exclusive upper end at v, which also leaves out v's
// prereleases when v is a release.
func exclusiveEnd(v *semver.Version) rangeEnd {
	return rangeEnd{ver: v, family: v.Prerelease() == ""}
}

// endsEqual reports whether a and b are the same range end.
func endsEqual(a, b rangeEnd) bool {
	if a.unbounded || b.unbounded {
		return a.unbounded == b.unbounded
	}
	if a.ver == nil || b.ver == nil {
		return a.ver == nil && b.ver == nil && a.inclusive == b.inclusive
	}
	return a.inclusive == b.inclusive && a.comparedAt().Equal(b.comparedAt())
}

// lowerBefore orders lower bounds, unbounded first.
func lowerBefore(a, b rangeEnd) bool {
	if a.unbounded != b.unbounded {
		return a.unbounded
	}
	if a.unbounded {
		return false
	}
	if cmp := a.comparedAt().Compare(b.comparedAt()); cmp != 0 {
		return cmp < 0
	}
	return a.inclusive && !b.inclusive
}

// upperAfter reports whether a extends coverage beyond b.
func upperAfter(a, b rangeEnd) bool {
	if a.unbounded {
		return true
	}
	if b.unbounded {
		return false
	}
	if cmp := a.comparedAt().Compare(b.comparedAt()); cmp != 0 {
		return cmp > 0
	}
	return a.inclusive && !b.inclusive
}

// lowerAtOrBelow reports whether a lower bound sits at or below an upper bound,
// so the pair admits at least one version. Unlike contiguous, which asks
// whether coverage continues across a shared point and accepts either side
// including it, sharing a version requires both sides to include it.
func lowerAtOrBelow(lo, hi rangeEnd) bool {
	if lo.unbounded || hi.unbounded {
		return true
	}
	cmp := lo.comparedAt().Compare(hi.comparedAt())
	if cmp != 0 {
		return cmp < 0
	}
	return lo.inclusive && hi.inclusive
}

// contiguous reports whether coverage continues from upper into the next lower
// bound, lower, comparing versions as written. Special cases are:
//
//   - "<0.18.0" then ">0.18.0" is not contiguous, since 0.18.0 is uncovered
//   - "<0.18.0" then ">=0.18.0" is contiguous, though it leaves out 0.18.0's
//     prereleases
func contiguous(upper, lower rangeEnd) bool {
	if upper.unbounded || lower.unbounded {
		return true
	}
	// No record need describe the prereleases of a release named by <, and a
	// cluster on one matches no transition.
	cmp := lower.ver.Compare(upper.ver)
	if cmp < 0 {
		return true
	}
	if cmp > 0 {
		return false
	}
	return upper.inclusive || lower.inclusive
}

// pastCeiling reports whether v sits above a bounded ceiling. A version at an
// inclusive ceiling is not past it, and one at an exclusive ceiling is.
func pastCeiling(ceiling rangeEnd, v *semver.Version) bool {
	cmp := v.Compare(ceiling.comparedAt())
	return cmp > 0 || (cmp == 0 && !ceiling.inclusive)
}

// versionRange is a half-open or closed version interval.
type versionRange struct {
	lower rangeEnd
	upper rangeEnd
}

// contains reports whether b holds v. The zero versionRange, which parseRange
// returns with every error, contains nothing.
func (b versionRange) contains(v *semver.Version) bool {
	if !b.lower.unbounded {
		if b.lower.ver == nil {
			return false
		}
		cmp := v.Compare(b.lower.comparedAt())
		if cmp < 0 || (cmp == 0 && !b.lower.inclusive) {
			return false
		}
	}
	if !b.upper.unbounded {
		if b.upper.ver == nil {
			return false
		}
		cmp := v.Compare(b.upper.comparedAt())
		if cmp > 0 || (cmp == 0 && !b.upper.inclusive) {
			return false
		}
	}
	return true
}

// admits reports whether b holds at. A non-inclusive at stands just above its
// version.
func admits(b versionRange, at rangeEnd) bool {
	if at.inclusive {
		return b.contains(at.comparedAt())
	}
	if !b.lower.unbounded && (b.lower.ver == nil || at.comparedAt().Compare(b.lower.comparedAt()) < 0) {
		return false
	}
	if !b.upper.unbounded && (b.upper.ver == nil || at.comparedAt().Compare(b.upper.comparedAt()) >= 0) {
		return false
	}
	return true
}

// onlyVersion returns the interval holding v and nothing else.
func onlyVersion(v *semver.Version) versionRange {
	return versionRange{lower: inclusiveEnd(v), upper: inclusiveEnd(v)}
}

// overlaps reports whether a and b share at least one version.
func overlaps(a, b versionRange) bool {
	return lowerAtOrBelow(a.lower, b.upper) && lowerAtOrBelow(b.lower, a.upper)
}

// overlap returns the interval a and b share. ok is false when they share no
// version.
func overlap(a, b versionRange) (out versionRange, ok bool) {
	out = a
	if lowerBefore(a.lower, b.lower) {
		out.lower = b.lower
	}
	if upperAfter(a.upper, b.upper) {
		out.upper = b.upper
	}
	return out, lowerAtOrBelow(out.lower, out.upper)
}

// rangeUpTo returns the versions from lower up to, but not including, the first
// version floor admits.
func rangeUpTo(lower, floor rangeEnd) versionRange {
	return versionRange{lower: lower, upper: rangeEnd{ver: floor.ver, inclusive: !floor.inclusive, family: floor.family}}
}

// rangeText returns b in the record range grammar.
func rangeText(b versionRange) string {
	if !b.lower.unbounded && !b.upper.unbounded && b.lower.inclusive && b.upper.inclusive &&
		b.lower.ver.Equal(b.upper.ver) {

		return "=" + b.lower.ver.String()
	}
	var parts []string
	if !b.lower.unbounded {
		op := ">"
		if b.lower.inclusive {
			op = ">="
		}
		parts = append(parts, op+b.lower.ver.String())
	}
	if !b.upper.unbounded {
		op := "<"
		if b.upper.inclusive {
			op = "<="
		}
		parts = append(parts, op+b.upper.ver.String())
	}
	return strings.Join(parts, " ")
}

// parseRange returns the interval of constraint, which must be a single
// AND-group of simple comparators. It rejects anything whose bounds are
// ambiguous, and every error names constraint in full.
func parseRange(constraint string) (versionRange, error) {
	// Ambiguity is rejected rather than approximated, because a wrong
	// structural check is worse than a rejected record.
	if strings.Contains(constraint, "||") {
		return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q uses ||; only a single AND-group of simple comparators is allowed", constraint))
	}
	if strings.Contains(constraint, " - ") {
		return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q is a hyphen range; write explicit >= and <= comparators instead", constraint))
	}

	fields := strings.FieldsFunc(constraint, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})
	if len(fields) == 0 {
		return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q is empty", constraint))
	}

	var b versionRange
	b.lower.unbounded = true
	b.upper.unbounded = true
	exactSet := false

	for i, f := range fields {
		op, verStr := splitComparator(f)
		if op == "" {
			return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("range %q contains unsupported comparator %q; allowed: <, <=, >, >=, = or a bare version", constraint, f))
		}
		if verStr == "" {
			// A space between the operator and the version (">= 0.18.0")
			// splits into two fields; the operator alone reaches here with
			// no version attached. This is not a grammar we widen to accept
			// (Masterminds does, but parseRange does not). The message
			// shows the join an author almost certainly meant.
			suggestion := f
			if i+1 < len(fields) {
				suggestion = f + fields[i+1]
			}
			return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("range %q: comparator %q is missing a version; the operator and version must not be separated by a space (write %q)",
					constraint, f, suggestion))
		}
		v, err := parseRangeVersion(constraint, verStr)
		if err != nil {
			return versionRange{}, err
		}
		switch op {
		case ">", ">=":
			if exactSet {
				return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q combines an exact version with another comparator", constraint))
			}
			if !b.lower.unbounded {
				return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q sets a lower bound twice", constraint))
			}
			b.lower = rangeEnd{ver: v, inclusive: op == ">="}
		case "<", "<=":
			if exactSet {
				return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q combines an exact version with another comparator", constraint))
			}
			if !b.upper.unbounded {
				return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q sets an upper bound twice", constraint))
			}
			b.upper = rangeEnd{ver: v, inclusive: true}
			if op == "<" {
				b.upper = exclusiveEnd(v)
			}
		case "=":
			if !b.lower.unbounded || !b.upper.unbounded {
				return versionRange{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q combines an exact version with another comparator", constraint))
			}
			b.lower = rangeEnd{ver: v, inclusive: true}
			b.upper = rangeEnd{ver: v, inclusive: true}
			exactSet = true
		}
	}
	return b, nil
}

// splitComparator returns the operator and version text of one token. A bare
// version is reported as "=". An unsupported operator (^, ~, !=) returns "".
func splitComparator(tok string) (op, ver string) {
	switch {
	case strings.HasPrefix(tok, ">="), strings.HasPrefix(tok, "<="):
		return tok[:2], tok[2:]
	case strings.HasPrefix(tok, "!="):
		return "", ""
	case strings.HasPrefix(tok, ">"), strings.HasPrefix(tok, "<"), strings.HasPrefix(tok, "="):
		return tok[:1], tok[1:]
	case strings.HasPrefix(tok, "^"), strings.HasPrefix(tok, "~"):
		return "", ""
	default:
		return "=", tok
	}
}

// parseRangeVersion parses s, one bound's version in constraint. s must be a
// full X.Y.Z version with no wildcard or build metadata, and may carry a
// prerelease, which compares exactly. Every error names constraint.
func parseRangeVersion(constraint, s string) (*semver.Version, error) {
	// Prereleases are legal because a component pinned at one, like grove at
	// v0.1.0-alpha.12, cannot otherwise have an honest record.
	if strings.Contains(s, "+") {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q: version %q carries build metadata, which semver orders as equal; "+
				"a bump that changes only build metadata would move past no ceiling", constraint, s))
	}
	// The wildcard check runs on core (the release segment only), not the
	// raw token: a legal prerelease tag may itself contain an x or X (e.g.
	// "1.2.3-hotfix.1"), which is not a wildcard.
	core := strings.TrimPrefix(s, "v")
	if idx := strings.IndexAny(core, "-+"); idx >= 0 {
		core = core[:idx]
	}
	if strings.ContainsAny(core, "*xX") {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q: version %q uses a wildcard; write explicit comparators instead", constraint, s))
	}
	if strings.Count(core, ".") != 2 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q: version %q is not a full X.Y.Z version; "+
				"a partial version silently expands and reads as approximate", constraint, s))
	}
	v, err := semver.NewVersion(s)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q: invalid version %q", constraint, s), err)
	}
	return v, nil
}

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

// prereleasePolicy controls whether a range bound may name a prerelease.
// Forbidden in `from`; allowed in `to`, because a component pinned at a
// prerelease (grove, v0.1.0-alpha.12) otherwise cannot have a record whose
// ceiling reaches its own pin.
type prereleasePolicy int

const (
	prereleaseForbidden prereleasePolicy = iota
	prereleaseAllowed
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

// bound is one side of a range. An unbounded side has no limit in that
// direction; ver is nil there. A bounded side with a nil ver is the
// zero-value result of a rejected parseBounds call and is treated as
// unsatisfiable rather than dereferenced.
type bound struct {
	ver       *semver.Version
	inclusive bool
	unbounded bool
}

// bounds is a half-open or closed version interval.
type bounds struct {
	lower bound
	upper bound
}

// contains reports whether v falls inside the interval. The zero value of
// bounds (both sides bounded with a nil ver, the shape returned alongside
// every parseBounds error) contains nothing, so a caller that logs a
// parseBounds error and calls contains anyway gets a safe "no match" instead
// of a nil-pointer panic.
func (b bounds) contains(v *semver.Version) bool {
	if !b.lower.unbounded {
		if b.lower.ver == nil {
			return false
		}
		cmp := v.Compare(b.lower.ver)
		if cmp < 0 || (cmp == 0 && !b.lower.inclusive) {
			return false
		}
	}
	if !b.upper.unbounded {
		if b.upper.ver == nil {
			return false
		}
		cmp := v.Compare(b.upper.ver)
		if cmp > 0 || (cmp == 0 && !b.upper.inclusive) {
			return false
		}
	}
	return true
}

// parseBounds extracts the interval of a constraint written in the restricted
// grammar: a single AND-group of simple comparators. Anything whose bounds are
// ambiguous is rejected rather than approximated, because a wrong structural
// check is worse than a rejected record. Every rejection names the full
// constraint text, since callers (Task 5's aggregation) surface these
// messages without the surrounding record context.
func parseBounds(constraint string, pre prereleasePolicy) (bounds, error) {
	if strings.Contains(constraint, "||") {
		return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q uses ||; only a single AND-group of simple comparators is allowed", constraint))
	}
	if strings.Contains(constraint, " - ") {
		return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q is a hyphen range; write explicit >= and <= comparators instead", constraint))
	}

	fields := strings.FieldsFunc(constraint, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})
	if len(fields) == 0 {
		return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q is empty", constraint))
	}

	var b bounds
	b.lower.unbounded = true
	b.upper.unbounded = true
	exactSet := false

	for i, f := range fields {
		op, verStr := splitComparator(f)
		if op == "" {
			return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("range %q contains unsupported comparator %q; allowed: <, <=, >, >=, = or a bare version", constraint, f))
		}
		if verStr == "" {
			// A space between the operator and the version (">= 0.18.0")
			// splits into two fields; the operator alone reaches here with
			// no version attached. This is not a grammar we widen to accept
			// (Masterminds does, but bounds does not) — instead the message
			// shows the join an author almost certainly meant.
			suggestion := f
			if i+1 < len(fields) {
				suggestion = f + fields[i+1]
			}
			return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("range %q: comparator %q is missing a version; the operator and version must not be separated by a space (write %q)",
					constraint, f, suggestion))
		}
		v, err := parseRangeVersion(constraint, verStr, pre)
		if err != nil {
			return bounds{}, err
		}
		switch op {
		case ">", ">=":
			if exactSet {
				return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q combines an exact version with another comparator", constraint))
			}
			if !b.lower.unbounded {
				return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q sets a lower bound twice", constraint))
			}
			b.lower = bound{ver: v, inclusive: op == ">="}
		case "<", "<=":
			if exactSet {
				return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q combines an exact version with another comparator", constraint))
			}
			if !b.upper.unbounded {
				return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q sets an upper bound twice", constraint))
			}
			b.upper = bound{ver: v, inclusive: op == "<="}
		case "=":
			if !b.lower.unbounded || !b.upper.unbounded {
				return bounds{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("range %q combines an exact version with another comparator", constraint))
			}
			b.lower = bound{ver: v, inclusive: true}
			b.upper = bound{ver: v, inclusive: true}
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

// parseRangeVersion parses one bound's version under the grammar's rules:
// full X.Y.Z, no wildcard, no build metadata, and a prerelease only where the
// policy allows one. constraint is the full range text the version came
// from, carried only so rejections can name it.
func parseRangeVersion(constraint, s string, pre prereleasePolicy) (*semver.Version, error) {
	if strings.Contains(s, "+") {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q: version %q carries build metadata, which semver orders as equal; "+
				"a bump that changes only build metadata would move past no ceiling", constraint, s))
	}
	// The wildcard check runs on core (the release segment only), not the
	// raw token: a prerelease tag legal under pre may itself contain an x or
	// X (e.g. "1.2.3-hotfix.1"), which is not a wildcard.
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
	if v.Prerelease() != "" && pre == prereleaseForbidden {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("range %q: version %q names a prerelease, which is not allowed here", constraint, s))
	}
	return v, nil
}

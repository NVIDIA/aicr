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

package corroborate

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// Class is a corroboration source class. A signer's class is derived from its
// verified OIDC identity against the allowlist — never a free-text field.
type Class string

const (
	// ClassFirstParty is the project's own UAT signer (GH-Actions OIDC pinned
	// to NVIDIA/aicr).
	ClassFirstParty Class = "first-party"

	// ClassCommunity is an allowlisted community signer (and the fallback class
	// for a verified-but-unallowlisted "reported" signer).
	ClassCommunity Class = "community"

	// ClassPartner is an allowlisted partner signer.
	ClassPartner Class = "partner"
)

// maxAllowlistBytes bounds the allowlist file read. The allowlist is a small,
// hand-curated, PR-reviewed file; anything larger is malformed or hostile.
const maxAllowlistBytes = 1 << 20 // 1 MiB

// SupportedAllowlistSchemaVersion is the allowlist schema (GP1-owned, Contract
// 2) this classifier understands. The loader fails closed on any other value:
// a future GP1 schema bump may carry semantics these classification rules do
// not enforce, so GP4 must be updated rather than silently classifying under
// stale assumptions.
const SupportedAllowlistSchemaVersion = "1.0.0"

// AllowlistEntry pins one verified signer: an exact issuer and an identity that
// is either an exact string or a tightly-bounded regex (recognized by a leading
// "^"). Over-broad identities are rejected by Allowlist.Validate.
type AllowlistEntry struct {
	Issuer   string `yaml:"issuer" json:"issuer"`
	Identity string `yaml:"identity" json:"identity"`
}

// Allowlist is the in-tree, PR-reviewed signer allowlist
// (recipes/evidence/allowlist.yaml, owned by GP1; this is the consumer-side
// loader/classifier). The three class sections are disjoint and non-overlapping.
type Allowlist struct {
	SchemaVersion string           `yaml:"schemaVersion" json:"schemaVersion"`
	FirstParty    []AllowlistEntry `yaml:"firstParty" json:"firstParty"`
	Community     []AllowlistEntry `yaml:"community" json:"community"`
	Partner       []AllowlistEntry `yaml:"partner" json:"partner"`
}

// classEntry pairs an entry with its class for whole-list iteration.
type classEntry struct {
	class Class
	entry AllowlistEntry
}

// entries returns every entry tagged with its class, in class order
// (first-party, community, partner) then file order.
func (a *Allowlist) entries() []classEntry {
	out := make([]classEntry, 0, len(a.FirstParty)+len(a.Community)+len(a.Partner))
	for _, e := range a.FirstParty {
		out = append(out, classEntry{ClassFirstParty, e})
	}
	for _, e := range a.Community {
		out = append(out, classEntry{ClassCommunity, e})
	}
	for _, e := range a.Partner {
		out = append(out, classEntry{ClassPartner, e})
	}
	return out
}

// LoadAllowlist reads and validates the allowlist at path. The read is bounded
// (maxAllowlistBytes) before parse so an attacker-influenced path cannot OOM
// the generator.
func LoadAllowlist(path string) (*Allowlist, error) {
	data, err := readBoundedFile(path, maxAllowlistBytes)
	if err != nil {
		return nil, err
	}

	var al Allowlist
	if err := yaml.Unmarshal(data, &al); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse allowlist "+path, err)
	}
	// Fail closed on an unsupported schema before any classification: an
	// unrecognized version may change the trust semantics this loader enforces.
	if al.SchemaVersion != SupportedAllowlistSchemaVersion {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("allowlist %s schemaVersion %q is unsupported (want %q)",
				path, al.SchemaVersion, SupportedAllowlistSchemaVersion))
	}
	if err := al.Validate(); err != nil {
		return nil, err
	}
	return &al, nil
}

// Validate enforces the anti-sybil invariants on the allowlist so it is not
// itself an attack surface:
//
//   - every entry has a non-empty issuer and identity;
//   - no identity is over-broad (no unbounded wildcard org/repo segment);
//   - the classes are disjoint and no two entries overlap (one verified
//     identity matches at most one entry).
func (a *Allowlist) Validate() error {
	all := a.entries()

	// Per-entry shape + over-broad lint.
	for _, ce := range all {
		if strings.TrimSpace(ce.entry.Issuer) == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("allowlist %s entry has empty issuer", ce.class))
		}
		if strings.TrimSpace(ce.entry.Identity) == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("allowlist %s entry has empty identity", ce.class))
		}
		if reason, broad := overBroadIdentity(ce.entry.Identity); broad {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("allowlist %s entry identity %q is over-broad: %s",
					ce.class, ce.entry.Identity, reason))
		}
		if _, err := compileIdentity(ce.entry.Identity); err != nil {
			return err
		}
	}

	// Disjoint + non-overlapping across the whole list.
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if overlaps(all[i].entry, all[j].entry) {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("allowlist entries overlap: %s %q and %s %q match a common identity",
						all[i].class, all[i].entry.Identity, all[j].class, all[j].entry.Identity))
			}
		}
	}
	return nil
}

// Classify resolves a verified (issuer, identity) to its class and whether it
// counts toward corroboration. A signer matching no entry is admitted as a
// zero-weight reported dot: class community, allowlisted false.
func (a *Allowlist) Classify(issuer, identity string) (Class, bool) {
	for _, ce := range a.entries() {
		if ce.entry.Issuer != issuer {
			continue
		}
		m, err := compileIdentity(ce.entry.Identity)
		if err != nil {
			continue // a malformed entry can never grant weight
		}
		if m(identity) {
			return ce.class, true
		}
	}
	return ClassCommunity, false
}

// identityMatcher reports whether a verified identity matches an allowlist
// entry's identity pattern.
type identityMatcher func(identity string) bool

// compileIdentity turns an entry identity into a matcher. A leading "^" marks a
// regex (compiled anchored at both ends); anything else is an exact-string
// match.
func compileIdentity(pattern string) (identityMatcher, error) {
	if !strings.HasPrefix(pattern, "^") {
		want := pattern
		return func(identity string) bool { return identity == want }, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "compile allowlist identity "+pattern, err)
	}
	return func(identity string) bool {
		loc := re.FindStringIndex(identity)
		// Require a full-string match: the regex is anchored with ^ and should
		// also anchor the end, but enforce full coverage defensively.
		return loc != nil && loc[0] == 0 && loc[1] == len(identity)
	}, nil
}

// overBroadIdentity reports whether an identity pattern is over-broad. Exact
// (non-regex) identities are inherently specific. A regex is over-broad when it
// contains UNBOUNDED repetition (*, +, or {n,}) anywhere in its AST — that is
// what lets a single entry match many distinct orgs/repos and manufacture a
// CONFIRMED (e.g. "^https://github.com/.+/.+/..." or "^.../[^/]+/attest$" or
// "^.../\\w+$"). Bounded quantifiers ({n,m}, ?) and fixed alternations
// ("(aws|gcp)") cannot span an arbitrary segment, so they are allowed.
//
// Walking the parsed AST catches every unbounded construct (\w+, [^/]+, .{2,},
// nested groups, a wildcard alternation branch), which a substring scan for a
// fixed set of tokens cannot.
func overBroadIdentity(pattern string) (reason string, broad bool) {
	if !strings.HasPrefix(pattern, "^") {
		return "", false // exact identities are inherently specific
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		// A malformed regex is rejected separately by compileIdentity; it is
		// not "over-broad", so let that path produce the precise compile error.
		return "", false
	}
	if q := unboundedRepetition(re); q != "" {
		return "contains unbounded repetition " + q + " that can span an org/repo segment", true
	}
	return "", false
}

// unboundedRepetition returns a token for the first unbounded repetition in the
// regex AST, or "" if none. Unbounded means *, +, or {n,} (open-ended) —
// repetitions that can match an arbitrary number of characters and therefore an
// arbitrary org/repo segment.
func unboundedRepetition(re *syntax.Regexp) string {
	// if/else rather than a switch on syntax.Op: bounded quantifiers ({n,m}, ?),
	// literals, char classes, anchors, captures, concats, and alternations are
	// not themselves unbounded, so they simply fall through to the recursion.
	switch {
	case re.Op == syntax.OpStar:
		return "*"
	case re.Op == syntax.OpPlus:
		return "+"
	case re.Op == syntax.OpRepeat && re.Max == -1: // {n,} has no upper bound
		return "{n,}"
	}
	for _, sub := range re.Sub {
		if q := unboundedRepetition(sub); q != "" {
			return q
		}
	}
	return ""
}

// overlaps reports whether two allowlist entries could both match one verified
// identity. Different issuers never overlap (issuer is an exact match). For a
// shared issuer it cross-applies each entry's matcher to the other's
// representative identity, which catches exact duplicates, an exact identity
// also covered by a foreign regex, and alternation-equivalent regexes.
//
// Sound regex-intersection emptiness is undecidable in general, so this is a
// best-effort SECONDARY guard. The load-bearing anti-sybil controls are the
// over-broad lint (overBroadIdentity, which forbids the unbounded repetition a
// truly broad pattern would need) and the anchored full-string match in
// compileIdentity; this check primarily stops the same identity being listed in
// two classes.
func overlaps(x, y AllowlistEntry) bool {
	if x.Issuer != y.Issuer {
		return false
	}
	mx, errX := compileIdentity(x.Identity)
	my, errY := compileIdentity(y.Identity)
	if errX != nil || errY != nil {
		// Malformed patterns are rejected elsewhere; conservatively report no
		// overlap so the precise compile error surfaces instead.
		return false
	}
	return mx(representativeIdentity(y.Identity)) || my(representativeIdentity(x.Identity))
}

// representativeIdentity returns a concrete identity string that pattern
// matches, so two patterns can be cross-tested for overlap. For an exact
// identity it is the identity itself; for a regex it strips the anchors,
// unescapes "\.", and resolves a leading alternation to its first option —
// enough to expose duplicate or nested tightly-bounded patterns.
func representativeIdentity(pattern string) string {
	if !strings.HasPrefix(pattern, "^") {
		return pattern
	}
	s := strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	s = strings.ReplaceAll(s, `\.`, ".")
	// Resolve "(a|b|c)" alternations to their first option.
	for {
		open := strings.IndexByte(s, '(')
		if open < 0 {
			break
		}
		closeIdx := strings.IndexByte(s[open:], ')')
		if closeIdx < 0 {
			break
		}
		closeIdx += open
		group := s[open+1 : closeIdx]
		first := group
		if bar := strings.IndexByte(group, '|'); bar >= 0 {
			first = group[:bar]
		}
		s = s[:open] + first + s[closeIdx+1:]
	}
	return s
}

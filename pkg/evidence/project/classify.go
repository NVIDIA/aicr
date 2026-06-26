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

package project

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// Class is the trust classification recorded for a verified signer. It
// drives how the GP4 consensus model weighs a source: only allowlisted
// sources count toward consensus; everything else is reported but
// contributes zero weight.
//
// The schema and matching semantics here intentionally mirror the
// consumer-side loader in pkg/corroborate so the producer and the GP4
// generator read the identical recipes/evidence/allowlist.yaml (owned by
// GP1) and classify a verified signer identically.
type Class string

const (
	// ClassFirstParty marks evidence produced by AICR's own UAT pipeline.
	ClassFirstParty Class = "first-party"

	// ClassCommunity marks evidence from an allowlisted community signer,
	// and is also the fail-closed fallback for a verified-but-unallowlisted
	// signer (reported, but never counted).
	ClassCommunity Class = "community"

	// ClassPartner marks evidence from an allowlisted partner signer.
	ClassPartner Class = "partner"
)

// firstPartyIssuer is the GitHub Actions OIDC token issuer used by the
// interim first-party heuristic (see Classify).
const firstPartyIssuer = "https://token.actions.githubusercontent.com"

// firstPartyIdentity matches the SubjectAlternativeName of AICR's own UAT
// workflows (uat-aws.yaml / uat-gcp.yaml on a branch ref) — the only
// identity the interim heuristic may admit as first-party. Fully anchored
// (^…$) so neither a look-alike host nor an arbitrary other path/ref under
// NVIDIA/aicr (e.g. a fork-PR workflow or a non-UAT workflow) can satisfy
// it. Mirrors FIRST_PARTY_IDENTITY in evidence-ingest.yaml and the
// documented firstParty allowlist entry that replaces this heuristic once
// recipes/evidence/allowlist.yaml ships. Used only when no allowlist file
// is present.
var firstPartyIdentity = regexp.MustCompile(
	`^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-(aws|gcp)\.yaml@refs/heads/.+$`)

// AllowlistEntry pins one verified signer: an exact issuer and an
// identity that is either an exact string or a tightly-bounded regex
// (recognized by a leading "^"). Over-broad identities are rejected by
// Allowlist.Validate.
type AllowlistEntry struct {
	Issuer   string `yaml:"issuer" json:"issuer"`
	Identity string `yaml:"identity" json:"identity"`
}

// Allowlist is the in-tree, PR-reviewed signer allowlist
// (recipes/evidence/allowlist.yaml, owned by GP1). The three class
// sections are disjoint and non-overlapping. A nil *Allowlist is valid
// and applies only the interim first-party heuristic plus the community
// fail-closed default.
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
	if a == nil {
		return nil
	}
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

// LoadAllowlist reads, parses, and validates the allowlist at path. The
// read is size-bounded before parse so an attacker-influenced path
// cannot OOM the process. Returns ErrCodeInvalidRequest on a bad file.
func LoadAllowlist(path string) (*Allowlist, error) {
	data, err := readBoundedFile(path, "allowlist "+path, defaults.HTTPResponseBodyLimit)
	if err != nil {
		return nil, err
	}

	var al Allowlist
	if err := yaml.Unmarshal(data, &al); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse allowlist "+path, err)
	}
	if err := al.Validate(); err != nil {
		return nil, err
	}
	return &al, nil
}

// Validate enforces the anti-sybil invariants so the allowlist is not
// itself an attack surface: every entry has a non-empty issuer and
// identity; no identity is over-broad (no unbounded wildcard segment);
// and the classes are disjoint (one verified identity matches at most
// one entry). The producer re-checks these (defense in depth) even
// though GP1 lints the file in its own repo CI.
func (a *Allowlist) Validate() error {
	all := a.entries()
	for _, ce := range all {
		if strings.TrimSpace(ce.entry.Issuer) == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("allowlist %s entry has empty issuer", ce.class))
		}
		if strings.TrimSpace(ce.entry.Identity) == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("allowlist %s entry has empty identity", ce.class))
		}
		if reason, unsafe := unsafeIdentityConstruct(ce.entry.Identity); unsafe {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("allowlist %s entry identity %q is unsafe: %s",
					ce.class, ce.entry.Identity, reason))
		}
		if _, err := compileIdentity(ce.entry.Identity); err != nil {
			return err
		}
	}
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

// Classify resolves a verified (issuer, identity) to its class and
// whether it counts toward corroboration. Resolution order:
//
//  1. The first matching allowlist entry wins (allowlisted=true).
//  2. When no allowlist is loaded (nil receiver, the interim state
//     before GP1 ships recipes/evidence/allowlist.yaml), the built-in
//     first-party heuristic admits AICR's own UAT identity so it is not
//     mislabeled community. Once the allowlist file exists this branch
//     is never reached — classification then matches the GP4 consumer
//     exactly.
//  3. Otherwise community, not allowlisted — the fail-closed default.
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
	if a == nil && issuer == firstPartyIssuer && firstPartyIdentity.MatchString(identity) {
		return ClassFirstParty, true
	}
	return ClassCommunity, false
}

// identityMatcher reports whether a verified identity matches an entry's
// identity pattern.
type identityMatcher func(identity string) bool

// compileIdentity turns an entry identity into a matcher. A leading "^"
// marks a regex (full-string match); anything else is an exact-string
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
		return loc != nil && loc[0] == 0 && loc[1] == len(identity)
	}, nil
}

// unsafeIdentityConstruct reports whether a regex identity pattern uses a
// construct that makes it either over-broad or impossible to overlap-check
// soundly. An exact (non-regex) identity is always safe. For a regex, only
// literals, concatenation, capture groups, alternation, and the ^/$ anchors
// are permitted. That keeps every accepted pattern finitely enumerable down
// its first alternation branch — the precondition for the single-sample
// cross-test in overlaps() (via representativeIdentity) to be exact rather
// than a heuristic that can miss intersecting entries. Any repetition
// (*, +, ?, {n,m}), character class, or '.' is rejected: repetition can
// span an org/repo segment and manufacture a confirmed source, while a
// character class or '.' lets two distinct entries intersect on an input
// that representativeIdentity never samples.
func unsafeIdentityConstruct(pattern string) (reason string, unsafe bool) {
	if !strings.HasPrefix(pattern, "^") {
		return "", false
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", false // a malformed regex is rejected by compileIdentity
	}
	return walkIdentityAST(re)
}

// walkIdentityAST returns a human-readable reason for the first unsafe node
// in a parsed identity regex, or ("", false) when every node is supported.
func walkIdentityAST(re *syntax.Regexp) (reason string, unsafe bool) {
	switch re.Op {
	case syntax.OpStar:
		return "contains unbounded repetition '*' that can span an org/repo segment", true
	case syntax.OpPlus:
		return "contains unbounded repetition '+' that can span an org/repo segment", true
	case syntax.OpRepeat:
		return "contains repetition '{n,m}' that can span an org/repo segment", true
	case syntax.OpQuest:
		return "contains optional '?' which the overlap check cannot sample", true
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return "contains a character class or '.' which the overlap check cannot sample", true
	case syntax.OpNoMatch, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return "contains an unsupported regex construct", true
	case syntax.OpEmptyMatch, syntax.OpLiteral, syntax.OpConcat,
		syntax.OpAlternate, syntax.OpCapture,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpBeginLine, syntax.OpEndLine:
		for _, sub := range re.Sub {
			if r, u := walkIdentityAST(sub); u {
				return r, u
			}
		}
		return "", false
	default:
		return "contains an unsupported regex construct", true
	}
}

// overlaps reports whether two entries could both match one verified
// identity. Different issuers never overlap. For a shared issuer it
// cross-applies each entry's matcher to the other's representative
// identity — catching exact duplicates, an exact identity also covered
// by a foreign regex, and alternation-equivalent regexes.
func overlaps(x, y AllowlistEntry) bool {
	if x.Issuer != y.Issuer {
		return false
	}
	mx, errX := compileIdentity(x.Identity)
	my, errY := compileIdentity(y.Identity)
	if errX != nil || errY != nil {
		return false
	}
	return mx(representativeIdentity(y.Identity)) || my(representativeIdentity(x.Identity))
}

// representativeIdentity returns a concrete identity string that pattern
// matches, so two patterns can be cross-tested for overlap.
func representativeIdentity(pattern string) string {
	if !strings.HasPrefix(pattern, "^") {
		return pattern
	}
	s := strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	s = strings.ReplaceAll(s, `\.`, ".")
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

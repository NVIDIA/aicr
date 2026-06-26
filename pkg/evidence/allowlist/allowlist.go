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

// Package allowlist loads and validates the maintained signer allowlist at
// recipes/evidence/allowlist.yaml. The allowlist is the trust root for the
// interim evidence-corroboration dashboard (epic #1400, contract #1347): it
// pins, per class, the signers that may contribute corroborating evidence.
// GP2's verify step and GP4's consensus model read it to classify a verified
// signer and weight its corroboration; a verified-but-unlisted signer is
// admitted as "reported," never corroborating.
//
// Privacy: community/partner entries are keyed by the one-way source slug
// (attestation.SourceSlug of the signer's issuer+identity) and never store
// the cleartext identity, so a contributor's personal email is not committed
// to the repo. First-party entries pin their CI workflow identity as a
// tightly-bounded regex — a workflow URL, not personal PII.
//
// The allowlist must not itself become a sybil surface, so Validate enforces
// that every entry is anchored (a content-addressed slug, or a tightly-bounded
// regex with no wildcard in the issuer/org/repo/workflow segment), that the
// three classes are disjoint, and that no two entries overlap.
package allowlist

import (
	"bytes"
	stderrors "errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// SchemaVersion is the on-disk allowlist schema version. Validate accepts
// the 1.0.x line.
const SchemaVersion = "1.0.0"

// slugLenStr is attestation.SourceSlugLength rendered for error messages.
var slugLenStr = strconv.Itoa(attestation.SourceSlugLength)

// sizeCeiling caps the bytes read from the allowlist file. The allowlist is
// small; anything past this is a bug or hostile input. Mirrors the pointer
// loader's 1 MiB ceiling.
var sizeCeiling = defaults.MaxRecipePOSTBytes

// Class names a contributor trust tier. Classes are disjoint.
type Class string

// Trust classes. first-party is the AICR project's own CI; community and
// partner are external contributors with progressively vetted standing.
const (
	ClassFirstParty Class = "first-party"
	ClassCommunity  Class = "community"
	ClassPartner    Class = "partner"
)

// Allowlist is the parsed recipes/evidence/allowlist.yaml document.
type Allowlist struct {
	SchemaVersion string  `yaml:"schemaVersion"`
	FirstParty    []Entry `yaml:"firstParty"`
	Community     []Entry `yaml:"community"`
	Partner       []Entry `yaml:"partner"`
}

// Entry pins one contributing signer. Exactly one of Source (the one-way
// per-source slug, for community/partner parties that commit pointers) or
// IdentityPattern (a tightly-bounded anchored regex, for branch-varying
// first-party CI that ingests directly) must be set.
//
// Source is attestation.SourceSlug(issuer, identity) of the verified signer.
// The cleartext identity is intentionally NOT stored — the slug is the stable
// key and is recomputed from the verified cert at ingest time. Label is an
// optional, non-PII display string (e.g. a GitHub handle or org name)
// surfaced by the dashboard; omit it to keep the entry fully pseudonymous.
type Entry struct {
	Label           string `yaml:"label,omitempty"`
	Issuer          string `yaml:"issuer"`
	Source          string `yaml:"source,omitempty"`
	IdentityPattern string `yaml:"identityPattern,omitempty"`
}

// id returns a stable human-facing identifier for the entry, used only in
// error messages: the label if set, else the slug, else the pattern.
func (e Entry) id() string {
	switch {
	case e.Label != "":
		return e.Label
	case e.Source != "":
		return "source " + e.Source
	default:
		return "pattern " + e.IdentityPattern
	}
}

// classified pairs an Entry with the class it was declared in, plus the
// compiled pattern (nil for slug entries) so Classify need not recompile.
type classified struct {
	entry   Entry
	class   Class
	pattern *regexp.Regexp
}

// Load reads, parses, and fully validates the allowlist at path.
func Load(path string) (*Allowlist, error) {
	f, err := os.Open(path) //nolint:gosec // repo-relative path, CI-controlled
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to open allowlist file", err)
	}
	defer func() { _ = f.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(f, sizeCeiling+1))
	if readErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read allowlist file", readErr)
	}
	if int64(len(body)) > sizeCeiling {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "allowlist file exceeds size limit (1 MiB)")
	}

	var al Allowlist
	if uErr := unmarshalStrict(body, &al); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "allowlist file is not valid YAML", uErr)
	}
	if vErr := al.Validate(); vErr != nil {
		return nil, vErr
	}
	return &al, nil
}

// Validate checks the allowlist is well-formed and not a sybil surface:
// supported schema; every entry anchored and well-shaped; slug entries carry
// a well-formed slug; pattern entries are tightly bounded (no wildcard
// org/repo); classes disjoint; no two entries overlap.
func (a *Allowlist) Validate() error {
	if !strings.HasPrefix(a.SchemaVersion, "1.0.") && a.SchemaVersion != "1.0" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"unsupported allowlist schemaVersion "+a.SchemaVersion+" (supports 1.0.x)")
	}

	all, err := a.classifyAll()
	if err != nil {
		return err
	}
	return checkDisjoint(all)
}

// classifyAll validates each entry in isolation and returns the flattened,
// class-tagged, pattern-compiled view used by Classify and the overlap check.
func (a *Allowlist) classifyAll() ([]classified, error) {
	groups := []struct {
		class   Class
		entries []Entry
	}{
		{ClassFirstParty, a.FirstParty},
		{ClassCommunity, a.Community},
		{ClassPartner, a.Partner},
	}

	var out []classified
	for _, g := range groups {
		for i := range g.entries {
			e := g.entries[i]
			pat, err := validateEntry(e, g.class)
			if err != nil {
				return nil, err
			}
			out = append(out, classified{entry: e, class: g.class, pattern: pat})
		}
	}
	return out, nil
}

// validateEntry checks a single entry's shape and returns its compiled
// pattern (nil for slug entries).
func validateEntry(e Entry, class Class) (*regexp.Regexp, error) {
	if e.Issuer == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"allowlist entry "+e.id()+" in "+string(class)+" is missing issuer")
	}
	if (e.Source == "") == (e.IdentityPattern == "") {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"allowlist entry "+e.id()+" must set exactly one of source or identityPattern")
	}

	// Anchor each class to its key kind: community/partner contributors commit
	// pointers and are keyed by the one-way source slug (no cleartext PII in
	// the repo); first-party CI ingests directly and is pinned by a bounded
	// identityPattern. A first-party `source` or a community/partner
	// `identityPattern` would commit cleartext identity to the wrong tier and
	// break the per-source privacy contract, so reject it at load time.
	switch class {
	case ClassFirstParty:
		if e.IdentityPattern == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"allowlist entry "+e.id()+" in "+string(class)+
					" must use identityPattern, not source")
		}
	case ClassCommunity, ClassPartner:
		if e.Source == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"allowlist entry "+e.id()+" in "+string(class)+
					" must use source, not identityPattern")
		}
	}

	if e.Source != "" {
		return nil, validateSlugEntry(e)
	}
	return validatePatternEntry(e)
}

// validateSlugEntry enforces that a slug entry carries a well-formed source
// slug (the only thing we can check, since the cleartext identity is by
// design absent).
func validateSlugEntry(e Entry) error {
	if !isHexSlug(e.Source) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"allowlist entry "+e.id()+" source must be a "+slugLenStr+
				"-character lowercase-hex slug")
	}
	return nil
}

// validatePatternEntry enforces that a pattern entry is fully anchored,
// compiles, carries no source, and is not over-broad.
func validatePatternEntry(e Entry) (*regexp.Regexp, error) {
	if err := checkNotOverBroad(e.id(), e.IdentityPattern); err != nil {
		return nil, err
	}
	pat, err := regexp.Compile(e.IdentityPattern)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"allowlist entry "+e.id()+" has an invalid identityPattern", err)
	}
	return pat, nil
}

// checkNotOverBroad rejects identity patterns whose issuer/org/repo/workflow
// segment is not fully literal. A tightly-bounded entry may use a wildcard
// only in the ref portion (right of the OIDC subject's '@'); a wildcard
// anywhere left of it could match an arbitrary org or repo, which is exactly
// the sybil surface the allowlist exists to prevent.
func checkNotOverBroad(id, pattern string) error {
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		return errors.New(errors.ErrCodeInvalidRequest,
			"allowlist entry "+id+" identityPattern must be fully anchored with ^ and $")
	}
	body := pattern[1 : len(pattern)-1]
	at := indexUnescaped(body, '@')
	if at < 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"allowlist entry "+id+" identityPattern must contain an OIDC subject of the "+
				"form <issuer-path>@<ref>")
	}
	if i := firstUnescapedMeta(body[:at]); i >= 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"allowlist entry "+id+" identityPattern has a wildcard in the issuer/org/repo/"+
				"workflow segment (over-broad): only the ref (right of '@') may be bounded-wildcard")
	}
	return nil
}

// literalPrefix returns the issuer/org/repo/workflow segment (left of the
// first unescaped '@') of an already-validated pattern, with regex escapes
// removed, for overlap comparison. Two patterns sharing this prefix and
// issuer target the same workflow and are treated as overlapping.
func literalPrefix(pattern string) string {
	body := strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	at := indexUnescaped(body, '@')
	if at < 0 {
		return body
	}
	return unescapeRegex(body[:at])
}

// checkDisjoint rejects duplicate labels, duplicate source slugs, and
// overlapping patterns (same issuer + literal prefix), so no verified signer
// can be classified two ways.
func checkDisjoint(all []classified) error {
	if err := checkDuplicateLabels(all); err != nil {
		return err
	}

	seenSource := map[string]string{}
	type patKey struct{ issuer, prefix string }
	seenPattern := map[patKey]string{}

	for _, c := range all {
		if c.entry.Source != "" {
			if prev, ok := seenSource[c.entry.Source]; ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					"allowlist entries "+prev+" and "+c.entry.id()+" share the same source slug")
			}
			seenSource[c.entry.Source] = c.entry.id()
			continue
		}
		k := patKey{c.entry.Issuer, literalPrefix(c.entry.IdentityPattern)}
		if prev, ok := seenPattern[k]; ok {
			return errors.New(errors.ErrCodeInvalidRequest,
				"allowlist entries "+prev+" and "+c.entry.id()+" have overlapping identity patterns")
		}
		seenPattern[k] = c.entry.id()
	}
	return nil
}

// checkDuplicateLabels rejects two entries sharing a non-empty label (empty
// labels are allowed to repeat — a fully-pseudonymous entry has none).
func checkDuplicateLabels(all []classified) error {
	seen := map[string]struct{}{}
	for _, c := range all {
		if c.entry.Label == "" {
			continue
		}
		if _, ok := seen[c.entry.Label]; ok {
			return errors.New(errors.ErrCodeInvalidRequest,
				"allowlist has a duplicate entry label "+c.entry.Label)
		}
		seen[c.entry.Label] = struct{}{}
	}
	return nil
}

// unmarshalStrict decodes YAML rejecting unknown fields. This fails closed on
// a stray `identity:` (the cleartext field deliberately removed for privacy)
// or any typo'd key, rather than silently ignoring it.
func unmarshalStrict(body []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if stderrors.Is(err, io.EOF) {
			return nil // empty document is an empty allowlist
		}
		return err
	}
	return nil
}

// Classify resolves a verified signer to its allowlist class and entry. The
// bool is false when the signer is not listed — callers admit such a signer
// as "reported" (never corroborating). Slug matches (community/partner) take
// precedence over pattern matches (first-party); Validate keeps the classes
// administratively disjoint.
func (a *Allowlist) Classify(issuer, identity string) (Class, *Entry, bool) {
	all, err := a.classifyAll()
	if err != nil {
		return "", nil, false
	}
	if slug, slugErr := attestation.SourceSlug(issuer, identity); slugErr == nil {
		for i := range all {
			// Match both the slug and the issuer. The slug derives from
			// (issuer, identity), but a slug-only match would let a different
			// issuer that engineered a colliding slug inherit the entry — so
			// re-anchor on the entry's pinned issuer here as well.
			if all[i].entry.Source == slug && all[i].entry.Issuer == issuer {
				e := all[i].entry
				return all[i].class, &e, true
			}
		}
	}
	for i := range all {
		if all[i].pattern == nil || all[i].entry.Issuer != issuer {
			continue
		}
		if all[i].pattern.MatchString(identity) {
			e := all[i].entry
			return all[i].class, &e, true
		}
	}
	return "", nil, false
}

// isHexSlug reports whether s is a SourceSlugLength-long lowercase-hex string.
func isHexSlug(s string) bool {
	if len(s) != attestation.SourceSlugLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// indexUnescaped returns the index of the first occurrence of c in s that is
// not preceded by an odd number of backslashes, or -1.
func indexUnescaped(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // skip the escaped character
			continue
		}
		if s[i] == c {
			return i
		}
	}
	return -1
}

// firstUnescapedMeta returns the index of the first unescaped regex
// metacharacter in s, or -1 if s is a pure literal (escapes aside).
func firstUnescapedMeta(s string) int {
	const meta = ".*+?()[]{}|^$"
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // an escaped metacharacter is a literal
			continue
		}
		if strings.IndexByte(meta, s[i]) >= 0 {
			return i
		}
	}
	return -1
}

// unescapeRegex drops backslash escapes so two equivalent literal prefixes
// compare equal regardless of which characters each chose to escape.
func unescapeRegex(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

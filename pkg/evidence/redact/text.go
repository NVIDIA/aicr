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

package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// TextScrubber redacts free text the CNCF submission directory ships, the
// rendered evidence pages, README.md, PRODUCT.yaml notes, junit.xml and
// e2e.log, where the structural CTRF and snapshot policies above do not
// reach: that text is the check output itself, so it is kept, and the
// identifiers inside it are aliased instead. One scrubber serves one
// submission so an identifier gets the same alias in every file, which
// keeps the relations the evidence shows (the same node in two checks, the
// same GPU in two lines) while removing the value.
//
// Rules, in the order applied:
//
//   - a token the caller lists in Preserve is kept verbatim where it stands
//     as a whole token (not inside a longer run of URL characters), so a
//     vendor-supplied URL carrying a UUID is not rewritten while the same
//     value embedded in another identifier is;
//   - a node name the caller lists is aliased node-N, longest name first so
//     a name that prefixes another is not split;
//   - container runtime IDs (containerd://<64 hex> and the docker and cri-o
//     forms) are aliased container-N, with the scheme kept; a bare 64-hex
//     digest (sha256:...) is an image identity and is kept;
//   - UUIDs are aliased uuid-N, which covers Kubernetes object UIDs, GPU
//     UUIDs (the GPU- prefix is kept) and cloud subscription IDs;
//   - IPv4 addresses, IPv6 addresses (the eight-group and the :: forms) and
//     MAC addresses are aliased ipv4-N, ipv6-N and mac-N;
//   - the account field of an AWS ARN is aliased account-N;
//   - a JWT is replaced by jwt-redacted without an alias.
//
// The scrubber is not a guarantee that nothing sensitive remains: a free-form
// hostname or a site name that is not a node name passes through. The
// operator reviews the directory before submitting it, and Report says what
// the pass did so the README can state it.
type TextScrubber struct {
	// Preserve lists tokens left untouched wherever they appear.
	Preserve []string

	// NodeNames lists the cluster's node names, aliased node-N.
	NodeNames []string

	// aliases is the alias of each value per class; used records the
	// values a Scrub call actually rewrote, which is what Report counts.
	aliases map[string]map[string]string
	used    map[string]map[string]bool
	jwts    int

	// nodeRE is the compiled alternation of NodeNames, rebuilt when the
	// names it was built from change.
	nodeRE      *regexp.Regexp
	nodeREnames string
}

// Alias classes, which are also the alias prefixes.
const (
	aliasNode      = "node"
	aliasContainer = "container"
	aliasUUID      = "uuid"
	aliasIPv4      = "ipv4"
	aliasIPv6      = "ipv6"
	aliasMAC       = "mac"
	aliasAccount   = "account"
	jwtRedacted    = "jwt-redacted"
)

// textAppliedRules is the static, sorted description of the text scrub.
var textAppliedRules = []string{
	"text.alias:account",
	"text.alias:container",
	"text.alias:ipv4",
	"text.alias:ipv6",
	"text.alias:mac",
	"text.alias:node",
	"text.alias:uuid",
	"text.omit:jwt",
}

var (
	textContainerID = regexp.MustCompile(`\b(containerd|docker|cri-o)://([0-9a-f]{64})\b`)
	textUUID        = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	textIPv4        = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\b`)
	// Eight groups, or a :: compression with at least one hex group on one
	// side; a bare "::" is left alone and so is a time such as 18:14:14.
	textIPv6   = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}\b|(?i)\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6}::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6}\b)?|(?i)::[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6}\b`)
	textMAC    = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}:){5}[0-9a-f]{2}\b`)
	textAWSARN = regexp.MustCompile(`\b(arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:)([0-9]{12})(:)`)
	textJWT    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
)

// Scrub returns in with every rule applied. Aliases are stable across calls
// on the same receiver.
func (s *TextScrubber) Scrub(in string) string {
	if s.aliases == nil {
		s.aliases = make(map[string]map[string]string)
		s.used = make(map[string]map[string]bool)
		// Node aliases are assigned up front in name order, so node-N does
		// not depend on which file mentions a node first.
		for _, name := range s.sortedNodeNames() {
			s.alias(aliasNode, name)
		}
		delete(s.used, aliasNode)
	}
	// Longest token first, so a token that is a prefix of another (a docs
	// URL and a page beneath it) does not split the longer one and expose
	// its tail to the rules.
	tokens := append([]string(nil), s.Preserve...)
	sort.SliceStable(tokens, func(i, j int) bool { return len(tokens[i]) > len(tokens[j]) })
	preserved := make([]string, 0, len(tokens))
	for i, token := range tokens {
		if token == "" || !strings.Contains(in, token) {
			continue
		}
		placeholder := fmt.Sprintf("\x00preserve-%d\x00", i)
		replaced := false
		in = replaceWholeToken(in, token, placeholder, &replaced)
		if replaced {
			preserved = append(preserved, placeholder, token)
		}
	}

	in = s.aliasNodeNames(in)
	in = textContainerID.ReplaceAllStringFunc(in, func(m string) string {
		parts := textContainerID.FindStringSubmatch(m)
		return parts[1] + "://" + s.alias(aliasContainer, parts[2])
	})
	in = textUUID.ReplaceAllStringFunc(in, func(m string) string { return s.alias(aliasUUID, strings.ToLower(m)) })
	in = textIPv6.ReplaceAllStringFunc(in, func(m string) string { return s.alias(aliasIPv6, strings.ToLower(m)) })
	in = textIPv4.ReplaceAllStringFunc(in, func(m string) string { return s.alias(aliasIPv4, m) })
	in = textMAC.ReplaceAllStringFunc(in, func(m string) string { return s.alias(aliasMAC, strings.ToLower(m)) })
	in = textAWSARN.ReplaceAllStringFunc(in, func(m string) string {
		parts := textAWSARN.FindStringSubmatch(m)
		return parts[1] + s.alias(aliasAccount, parts[2]) + parts[3]
	})
	in = textJWT.ReplaceAllStringFunc(in, func(string) string {
		s.jwts++
		return jwtRedacted
	})

	if len(preserved) > 0 {
		in = strings.NewReplacer(preserved...).Replace(in)
	}
	return in
}

// replaceWholeToken substitutes token where it is not embedded in a longer
// run of URL or identifier characters, and reports whether it did.
func replaceWholeToken(in, token, placeholder string, replaced *bool) string {
	var sb strings.Builder
	rest := in
	for {
		i := strings.Index(rest, token)
		if i < 0 {
			sb.WriteString(rest)
			return sb.String()
		}
		end := i + len(token)
		bounded := (i == 0 || !isTokenByte(rest[i-1])) && (end == len(rest) || !isTokenByte(rest[end]))
		sb.WriteString(rest[:i])
		if bounded {
			sb.WriteString(placeholder)
			*replaced = true
		} else {
			sb.WriteString(token)
		}
		rest = rest[end:]
	}
}

// isTokenByte reports whether b continues a URL or identifier token: a
// preserved value followed by one of these is part of something longer.
func isTokenByte(b byte) bool {
	return isNameByte(b) || b == '-' || b == '.' || b == '/' || b == '@' || b == ':' || b == '%' || b == '?' || b == '=' || b == '&' || b == '#'
}

// sortedNodeNames returns the non-blank node names in name order, which
// fixes their alias numbers.
func (s *TextScrubber) sortedNodeNames() []string {
	names := make([]string, 0, len(s.NodeNames))
	for _, n := range s.NodeNames {
		if strings.TrimSpace(n) != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// aliasNodeNames aliases each listed node name in one pass over the input:
// an alternation of every name, longest first so a name that contains
// another is matched whole, scanned once with the boundaries checked per
// match. Inserted aliases are never re-scanned, so a cluster whose nodes are
// literally named node-1, node-2 cannot collide with the alias of another.
func (s *TextScrubber) aliasNodeNames(in string) string {
	names := s.sortedNodeNames()
	if len(names) == 0 {
		return in
	}
	sort.SliceStable(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	re := s.nodeRegexp(names)
	var sb strings.Builder
	pos := 0
	for pos < len(in) {
		m := re.FindStringIndex(in[pos:])
		if m == nil {
			break
		}
		start := pos + m[0]
		sb.WriteString(in[pos:start])
		// The longest name at this offset may run into a longer token
		// (gpu-1-b in gpu-1-bx); a shorter name at the same offset can still
		// stand whole (gpu-1 in gpu-1-bx), so each is tried, longest first,
		// with the boundaries read from the original input.
		matched := ""
		leftBounded := start == 0 || !isNameByte(in[start-1])
		for _, name := range names {
			if !strings.HasPrefix(in[start:], name) {
				continue
			}
			end := start + len(name)
			if leftBounded && (end == len(in) || !isNameByte(in[end])) {
				matched = name
				break
			}
		}
		if matched == "" {
			// Not a whole node name here: emit one character and look again
			// from the next, so a bounded name inside the rejected span is
			// still found.
			_, size := utf8.DecodeRuneInString(in[start:])
			sb.WriteString(in[start : start+size])
			pos = start + size
			continue
		}
		sb.WriteString(s.alias(aliasNode, matched))
		pos = start + len(matched)
	}
	sb.WriteString(in[pos:])
	return sb.String()
}

// nodeRegexp returns the alternation of names, longest first, compiled once
// per distinct name list.
func (s *TextScrubber) nodeRegexp(names []string) *regexp.Regexp {
	key := strings.Join(names, "\x00")
	if s.nodeRE == nil || s.nodeREnames != key {
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = regexp.QuoteMeta(n)
		}
		s.nodeRE, s.nodeREnames = regexp.MustCompile(strings.Join(quoted, "|")), key
	}
	return s.nodeRE
}

// isNameByte reports whether b continues a node name token, so a name found
// inside a longer word is not aliased. A hyphen is a boundary: Kubernetes
// derives object names as <node>-<suffix> (ResourceSlices, generated pod
// names), and the node inside them must be aliased too.
func isNameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// alias returns the stable alias of value within class, minting one on first
// sight, and records the value as rewritten. Node aliases are minted before
// any text is scrubbed, so a node's number is fixed by the name list.
func (s *TextScrubber) alias(class, value string) string {
	byValue := s.aliases[class]
	if byValue == nil {
		byValue = make(map[string]string)
		s.aliases[class] = byValue
	}
	if s.used[class] == nil {
		s.used[class] = make(map[string]bool)
	}
	s.used[class][value] = true
	if a, ok := byValue[value]; ok {
		return a
	}
	a := fmt.Sprintf("%s-%d", class, len(byValue)+1)
	byValue[value] = a
	return a
}

// AppliedRules returns the sorted rule identifiers of the text scrub,
// independent of what any input contained.
func (s *TextScrubber) AppliedRules() []string {
	return append([]string(nil), textAppliedRules...)
}

// Report returns one line per alias class that minted at least one alias
// ("ipv4: 3 distinct values aliased"), plus the JWT count, sorted. Empty
// when nothing was rewritten.
func (s *TextScrubber) Report() []string {
	if s == nil {
		return nil
	}
	var out []string
	for class, values := range s.used {
		if len(values) > 0 {
			out = append(out, fmt.Sprintf("%s: %d distinct value(s) aliased", class, len(values)))
		}
	}
	if s.jwts > 0 {
		out = append(out, fmt.Sprintf("jwt: %d token(s) removed", s.jwts))
	}
	sort.Strings(out)
	return out
}

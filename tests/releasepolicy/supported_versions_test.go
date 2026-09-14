// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package releasepolicy

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The supported-version policy is published twice on purpose. SECURITY.md is
// what a reporter reads, and RELEASE.md is what release tooling reads: the
// NVIDIA OSS Scorecard's release dimension fetches RELEASE.md and CHANGELOG.md
// and nothing else, so a policy stated only in SECURITY.md is invisible to it.
//
// Duplication nothing checks is duplication that drifts, and the failure is
// silent: a release bumps one table and leaves the other advertising a minor
// that stopped receiving patches. This file is that check.

const supportedVersionsHeading = "## Supported Versions"

// supportedMinorCell matches the supported row's version cell, e.g. `0.19.x`.
var supportedMinorCell = regexp.MustCompile("`([0-9]+\\.[0-9]+)\\.x`")

// endOfLifeCell matches the end-of-life row's version cell, e.g. `< 0.19`.
var endOfLifeCell = regexp.MustCompile("`< *([0-9]+\\.[0-9]+)`")

// fixTypePhrase captures what a supported version is promised, e.g.
// "security fixes" out of "receives security fixes". Both the prose sentence
// and the table's status cell are phrased around "receives", which is what
// makes one pattern enough for both.
var fixTypePhrase = regexp.MustCompile(`(?i)receives\s+([a-z][a-z ]*?)\s*(?:[.;,|]|$)`)

func TestSupportedVersionsMatchSecurityPolicy(t *testing.T) {
	releaseSupported, releaseEndOfLife := supportedVersionPolicy(t, "RELEASE.md")
	securitySupported, securityEndOfLife := supportedVersionPolicy(t, "SECURITY.md")

	if releaseSupported != securitySupported {
		t.Errorf("supported minor has drifted: RELEASE.md says %q, SECURITY.md says %q. "+
			"Both files publish this policy and must name the same minor",
			releaseSupported+".x", securitySupported+".x")
	}
	if releaseEndOfLife != securityEndOfLife {
		t.Errorf("end-of-life threshold has drifted: RELEASE.md says %q, SECURITY.md says %q. "+
			"Both files publish this policy and must name the same threshold",
			"< "+releaseEndOfLife, "< "+securityEndOfLife)
	}

	// Within one file the two rows describe one boundary: everything below the
	// supported minor is end-of-life. A half-finished bump moves the supported
	// row and forgets the threshold, leaving a table that calls the same minor
	// both supported and end-of-life. Catching that needs no cross-file
	// comparison, so assert it per file.
	for _, doc := range []struct {
		path      string
		supported string
		endOfLife string
	}{
		{"RELEASE.md", releaseSupported, releaseEndOfLife},
		{"SECURITY.md", securitySupported, securityEndOfLife},
	} {
		if doc.supported != doc.endOfLife {
			t.Errorf("%s supported-versions table is self-contradictory: supported minor is "+
				"%q but the end-of-life threshold is %q, so %s is listed as both",
				doc.path, doc.supported+".x", "< "+doc.endOfLife, doc.endOfLife+".x")
		}
	}

	// Agreeing on the version number is not the same as agreeing on the
	// promise. RELEASE.md shipped saying a supported minor "receives bug fixes
	// and security patches" while SECURITY.md said "receives security fixes" —
	// a wider public commitment than the security policy makes, and the
	// number-only comparison above sailed straight past it.
	releaseFixes := supportedFixTypes(t, "RELEASE.md")
	securityFixes := supportedFixTypes(t, "SECURITY.md")

	if strings.Join(releaseFixes, ", ") != strings.Join(securityFixes, ", ") {
		t.Errorf("supported-versions promise has drifted: RELEASE.md promises %q, "+
			"SECURITY.md promises %q. RELEASE.md must not publish a wider commitment "+
			"than SECURITY.md, which is authoritative on what a supported version receives",
			strings.Join(releaseFixes, ", "), strings.Join(securityFixes, ", "))
	}

	for _, doc := range []struct {
		path  string
		fixes []string
	}{
		{"RELEASE.md", releaseFixes},
		{"SECURITY.md", securityFixes},
	} {
		if len(doc.fixes) > 1 {
			t.Errorf("%s supported-versions section promises %d different fix types (%s); "+
				"its prose and its table must say the same thing",
				doc.path, len(doc.fixes), strings.Join(doc.fixes, ", "))
		}
	}
}

// supportedFixTypes returns the distinct fix types path's supported-versions
// section promises, lowercased and sorted. Every "receives ..." in the section
// is collected, not just the first, so a table cell that disagrees with the
// prose above it shows up as two entries rather than being silently dropped.
func supportedFixTypes(t *testing.T, path string) []string {
	t.Helper()

	// Fold the section onto one line first: the prose wraps, and a promise
	// split across a newline would otherwise not match.
	section := strings.Join(strings.Fields(supportedVersionsSection(t, path)), " ")

	matches := fixTypePhrase.FindAllStringSubmatch(section, -1)
	if matches == nil {
		t.Fatalf("%s %s section never says what a supported version receives; "+
			"without that phrase this comparison would pass vacuously",
			path, supportedVersionsHeading)
	}

	seen := make(map[string]bool, len(matches))
	unique := make([]string, 0, len(matches))
	for _, match := range matches {
		phrase := strings.ToLower(match[1])
		if !seen[phrase] {
			seen[phrase] = true
			unique = append(unique, phrase)
		}
	}
	sort.Strings(unique)
	return unique
}

// supportedVersionPolicy returns the supported minor and the end-of-life
// threshold declared by path's "## Supported Versions" section, as bare
// `MAJOR.MINOR` strings.
func supportedVersionPolicy(t *testing.T, path string) (supported, endOfLife string) {
	t.Helper()

	section := supportedVersionsSection(t, path)
	return firstSubmatch(t, path, "supported minor", supportedMinorCell, section),
		firstSubmatch(t, path, "end-of-life threshold", endOfLifeCell, section)
}

// supportedVersionsSection returns the body of path's supported-versions
// section, from its heading to the next level-2 heading. Scoping the match to
// the section keeps a version number written elsewhere in the file (a
// deprecation example, a sample tag) out of the comparison.
func supportedVersionsSection(t *testing.T, path string) string {
	t.Helper()

	document := string(readFile(t, path))
	_, body, found := strings.Cut(document, supportedVersionsHeading+"\n")
	if !found {
		t.Fatalf("%s has no %q section; both files must state which versions "+
			"receive fixes", path, supportedVersionsHeading)
	}
	if next := strings.Index(body, "\n## "); next >= 0 {
		body = body[:next]
	}
	return body
}

// firstSubmatch returns pattern's first capture in section, failing when the
// section does not declare the value at all. Returning an empty string instead
// would let two documents that both dropped their table compare equal.
func firstSubmatch(t *testing.T, path, what string, pattern *regexp.Regexp, section string) string {
	t.Helper()

	match := pattern.FindStringSubmatch(section)
	if match == nil {
		t.Fatalf("%s %s section declares no %s (no %s cell)", path, supportedVersionsHeading, what, pattern)
	}
	return match[1]
}

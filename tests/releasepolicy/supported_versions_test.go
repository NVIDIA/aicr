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

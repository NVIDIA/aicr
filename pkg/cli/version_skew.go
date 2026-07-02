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

package cli

import (
	"log/slog"
	"strings"
)

// isReleaseVersion reports whether v is a release build version string.
// Dev builds ("dev") and snapshot builds ("...-next") carry non-release
// strings whose skew is expected, so version-skew detection ignores them to
// avoid noise in local and CI flows. An empty version (an older artifact that
// predates the embedded-version field) is likewise treated as unknown.
func isReleaseVersion(v string) bool {
	return v != "" && v != versionDefault && !strings.Contains(v, "-next")
}

// versionSkewDetected reports whether the inputs contain two or more distinct
// release versions. Only release versions are compared (see isReleaseVersion);
// a single known version mixed with unknown/dev ones is not treated as skew.
func versionSkewDetected(versions ...string) bool {
	distinct := make(map[string]struct{})
	for _, v := range versions {
		if isReleaseVersion(v) {
			distinct[v] = struct{}{}
		}
	}
	return len(distinct) >= 2
}

// warnVersionSkew emits a single advisory warning when the running binary, the
// recipe-producing binary, and the snapshot-producing binary report different
// release versions. Mixing artifacts produced by different aicr versions can
// surface as confusing validation failures with no obvious cause, so this is a
// debugging breadcrumb pointing at version skew.
//
// The embedded versions are unsigned, advisory metadata — this deliberately
// does not fail the command. See the schema-versioned compatibility gate
// tracked as follow-up work for a stronger guarantee.
func warnVersionSkew(binaryVersion, recipeVersion, snapshotVersion string) {
	if !versionSkewDetected(binaryVersion, recipeVersion, snapshotVersion) {
		return
	}
	slog.Warn("version skew detected across validate inputs; mixing artifacts from different aicr versions can cause hard-to-debug validation failures",
		"binaryVersion", displayVersion(binaryVersion),
		"recipeVersion", displayVersion(recipeVersion),
		"snapshotVersion", displayVersion(snapshotVersion))
}

// displayVersion renders a version for logging, substituting a placeholder for
// the empty string so the warning never shows a blank field.
func displayVersion(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

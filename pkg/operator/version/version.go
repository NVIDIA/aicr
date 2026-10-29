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

// Package version reports the build identity stamped into the operator
// binaries at link time.
package version

import (
	"regexp"
	"runtime"
)

// Set at link time with -ldflags "-X <module>/pkg/operator/version.Version=...".
var (
	// Version is the semantic version the build was cut from, or the
	// development default when built without a release stamp.
	Version = DevVersion
	// Commit is the full source commit the build was produced from.
	Commit = "unknown"
	// Date is the build timestamp in RFC 3339 form.
	Date = "unknown"
)

// DevVersion marks a build that was not stamped by the release pipeline.
const DevVersion = "v0.0.0-dev"

// releasePattern matches the versions the release pipeline stamps: a semantic
// version tag, optionally with an alpha, beta or rc pre-release.
var releasePattern = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-(alpha|beta|rc)\.(0|[1-9]\d*))?$`)

// Info is the build identity in a form suitable for logs and metrics.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Release   bool   `json:"release"`
}

// Get returns the identity of the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Release:   IsRelease(Version),
	}
}

// IsRelease reports whether v is a version the release pipeline stamps.
// Development-only behavior, such as the approval bypass, is refused
// by any build for which this returns true.
func IsRelease(v string) bool {
	return releasePattern.MatchString(v)
}

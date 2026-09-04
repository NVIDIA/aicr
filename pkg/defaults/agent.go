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

package defaults

import "strings"

// Defaults for the snapshot-collection agent Job.
//
// These live here rather than in pkg/cli because both the CLI and the
// pkg/client/v1 facade deploy the same Job. They used to exist only as CLI flag
// defaults, which is why an SDK caller who omitted them got an apiserver
// rejection from inside Deploy instead of a usable error (#2256). One
// definition means the two paths cannot drift into deploying different images.
const (
	// AgentName is the Job and ServiceAccount name for the snapshot agent.
	// Both share it: the ServiceAccount exists only to run this Job.
	AgentName = "aicr"

	// AgentImageRepository is the registry path of the agent image.
	AgentImageRepository = "ghcr.io/nvidia/aicr"

	// AgentImageDevTag is the tag used for builds that have no released
	// image to match: development builds and pre-release snapshots.
	AgentImageDevTag = "latest"

	// DevVersion is the version string of an unstamped build — the value
	// left in place when the release ldflags did not run.
	DevVersion = "dev"

	// DevChartVersion is what DevVersion becomes when it has to be written
	// into a Helm Chart.yaml `version:`. Helm validates that field as
	// SemVer 2 and rejects "dev" outright, so every generated chart whose
	// version tracks the AICR build needs a valid stand-in. The
	// "0.0.0-<pre-release>" shape sorts below every real release, which is
	// the correct ordering for an unstamped build.
	DevChartVersion = "0.0.0-dev"
)

// AgentImageForVersion returns the agent container image matching a build
// version, so the agent deployed into the cluster is the same generation as the
// binary deploying it.
//
// Release builds map to their own tag, normalizing the "v" prefix that release
// versions carry inconsistently ("0.8.10" and "v0.8.10" both yield
// ":v0.8.10"). Development builds and "-next" snapshots have no published image
// of their own and fall back to AgentImageDevTag. An empty version is treated as
// a development build: a Client constructed without WithVersion has no release
// to match, and pinning it to a tag that does not exist would fail the pull.
func AgentImageForVersion(version string) string {
	v := strings.TrimSpace(version)

	if v == "" || v == DevVersion || strings.Contains(v, "-next") {
		return AgentImageRepository + ":" + AgentImageDevTag
	}
	if strings.HasPrefix(v, "v") {
		return AgentImageRepository + ":" + v
	}
	return AgentImageRepository + ":v" + v
}

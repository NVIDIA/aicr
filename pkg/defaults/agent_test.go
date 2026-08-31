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

import "testing"

func TestAgentImageForVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		// The "v" prefix is inconsistent across release version strings, so
		// both spellings must land on the one published tag.
		{"release without v prefix", "0.8.10", AgentImageRepository + ":v0.8.10"},
		{"release with v prefix", "v0.8.10", AgentImageRepository + ":v0.8.10"},

		// No published image exists for these, so they take the dev tag.
		{"dev build", DevVersion, AgentImageRepository + ":" + AgentImageDevTag},
		{"snapshot with v prefix", "v0.8.10-next", AgentImageRepository + ":" + AgentImageDevTag},
		{"snapshot without v prefix", "0.8.10-next", AgentImageRepository + ":" + AgentImageDevTag},

		// A Client constructed without WithVersion. Pinning it to a tag that
		// does not exist would fail the image pull rather than run.
		{"unset version", "", AgentImageRepository + ":" + AgentImageDevTag},
		{"whitespace version", "  ", AgentImageRepository + ":" + AgentImageDevTag},

		// Surrounding whitespace must not produce an unpullable tag.
		{"padded release", " 0.8.10 ", AgentImageRepository + ":v0.8.10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AgentImageForVersion(tt.version); got != tt.want {
				t.Errorf("AgentImageForVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

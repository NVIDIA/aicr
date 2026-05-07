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

import "testing"

func TestDefaultAgentImage(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected string
	}{
		{"dev build", "dev", agentImageBase + ":latest"},
		{"snapshot with v prefix", "v0.8.10-next", agentImageBase + ":latest"},
		{"snapshot without v prefix", "0.8.10-next", agentImageBase + ":latest"},
		{"release without v prefix", "0.8.10", agentImageBase + ":v0.8.10"},
		{"release with v prefix", "v0.8.10", agentImageBase + ":v0.8.10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := version
			version = tt.version
			t.Cleanup(func() { version = original })

			got := defaultAgentImage()
			if got != tt.expected {
				t.Errorf("defaultAgentImage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

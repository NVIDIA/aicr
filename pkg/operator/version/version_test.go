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

package version

import "testing"

func TestIsRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{"v1.0.0", true},
		{"v0.1.0", true},
		{"v10.20.30", true},
		{"v1.2.0-rc.1", true},
		{"v1.3.0-rc1", true},
		{"v1.3.0-rc12", true},
		{"v1.3.0-rc01", false},
		{"v1.2.0-beta.0", true},
		{"v1.2.0-alpha.3", true},
		{DevVersion, false},
		{"", false},
		{"1.0.0", false},
		{"v1.0", false},
		{"v01.0.0", false},
		{"v1.0.0-dirty", false},
		{"v1.0.0-3-gabcdef0", false},
		{"v1.0.0-rc", false},
		{"v1.0.0+meta", false},
		{"latest", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			if got := IsRelease(tt.version); got != tt.want {
				t.Fatalf("IsRelease(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestGetReportsStampedValues(t *testing.T) {
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	Version, Commit, Date = "v1.2.3", "abc123", "2026-10-14T00:00:00Z"
	got := Get()
	if got.Version != "v1.2.3" || got.Commit != "abc123" || got.Date != "2026-10-14T00:00:00Z" {
		t.Fatalf("Get() = %+v, want stamped values", got)
	}
	if !got.Release {
		t.Fatal("Get().Release = false for a stamped release version")
	}
	if got.GoVersion == "" {
		t.Fatal("Get().GoVersion is empty")
	}

	Version = DevVersion
	if Get().Release {
		t.Fatal("Get().Release = true for the development version")
	}
}

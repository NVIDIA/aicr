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

package oskind

import (
	"sort"
	"testing"
)

func TestAll_SortedAndComplete(t *testing.T) {
	got := All()
	want := []string{AmazonLinux, COS, RHEL, Talos, Ubuntu}
	if len(got) != len(want) {
		t.Fatalf("All() returned %d values, want %d", len(got), len(want))
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			t.Errorf("All() not sorted: got[%d]=%q, sorted[%d]=%q", i, got[i], i, sorted[i])
		}
		if got[i] != want[i] {
			t.Errorf("All()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsKnown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"any", "any", true},
		{"ubuntu", "ubuntu", true},
		{"rhel", "rhel", true},
		{"cos", "cos", true},
		{"amazonlinux", "amazonlinux", true},
		{"talos", "talos", true},
		{"Talos uppercase", "Talos", true},
		{"talos with whitespace", "  talos  ", true},
		{"empty", "", false},
		{"alias al2 not recognized", "al2", false},
		{"unknown", "windows", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsKnown(tt.in); got != tt.want {
				t.Errorf("IsKnown(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

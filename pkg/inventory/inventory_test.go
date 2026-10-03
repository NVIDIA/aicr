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

package inventory

import "testing"

// TestCompareInstallOrder asserts the namespace tiebreak directly.
//
// Through either reader's sort it cannot be: the comparator is a total order
// over identities that are unique on (name, namespace), so dropping the
// tiebreak leaves an order that depends on Go map iteration and a test for it
// passes or fails by chance. Here the claim is exact.
func TestCompareInstallOrder(t *testing.T) {
	tests := []struct {
		name              string
		nameA, namespaceA string
		nameB, namespaceB string
		want              int
	}{
		{"name orders first", "alpha", "z-ns", "beta", "a-ns", -1},
		{"name orders first, reversed", "beta", "a-ns", "alpha", "z-ns", 1},
		{"equal names fall to the namespace", "gpu-operator", "tenant-a", "gpu-operator", "tenant-b", -1},
		{
			"equal names fall to the namespace, reversed",
			"gpu-operator", "tenant-b", "gpu-operator", "tenant-a", 1,
		},
		{"identical is equal", "gpu-operator", "tenant-a", "gpu-operator", "tenant-a", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareInstallOrder(tt.nameA, tt.namespaceA, tt.nameB, tt.namespaceB)
			if (got < 0) != (tt.want < 0) || (got > 0) != (tt.want > 0) {
				t.Errorf("compareInstallOrder(%q/%q, %q/%q) = %d, want sign of %d",
					tt.nameA, tt.namespaceA, tt.nameB, tt.namespaceB, got, tt.want)
			}
		})
	}
}

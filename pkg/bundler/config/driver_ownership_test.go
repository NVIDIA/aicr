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

package config

import "testing"

func TestDriverOwnershipFamilies(t *testing.T) {
	families := DriverOwnershipFamilies()
	if len(families) != 2 {
		t.Fatalf("DriverOwnershipFamilies() returned %d families, want 2", len(families))
	}

	seen := map[string]bool{}
	for _, family := range families {
		if len(family.ComponentNames) == 0 || len(family.Paths) == 0 {
			t.Fatalf("family %+v must name components and paths", family)
		}
		for _, name := range family.ComponentNames {
			if seen[name] {
				t.Errorf("component %q appears in more than one family or twice", name)
			}
			seen[name] = true
		}
	}
}

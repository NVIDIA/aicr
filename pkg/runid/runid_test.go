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

package runid

import (
	"regexp"
	"testing"
)

var runIDPattern = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{16}$`)

func TestGenerateFormat(t *testing.T) {
	id := Generate()
	if !runIDPattern.MatchString(id) {
		t.Errorf("Generate() = %q, want match %s", id, runIDPattern)
	}
	if len(id) != 32 {
		t.Errorf("len(Generate()) = %d, want 32", len(id))
	}
}

func TestGenerateUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id := Generate()
		if _, dup := seen[id]; dup {
			t.Fatalf("Generate() returned duplicate %q", id)
		}
		seen[id] = struct{}{}
	}
}

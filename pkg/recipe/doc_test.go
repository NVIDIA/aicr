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

package recipe

import (
	"sort"
	"testing"
)

// TestCriteriaPlatformConstantsMatchGetter guards against drift between the
// CriteriaPlatform* constants and the slice returned by
// GetCriteriaPlatformTypes(). Adding a new constant without registering it in
// the getter (or vice versa) is exactly the class of bug that left earlier
// platform-enum doc surfaces stale before this test existed.
func TestCriteriaPlatformConstantsMatchGetter(t *testing.T) {
	declared := []string{
		string(CriteriaPlatformDynamo),
		string(CriteriaPlatformKubeflow),
		string(CriteriaPlatformNIM),
		string(CriteriaPlatformRunai),
		string(CriteriaPlatformSlurm),
	}
	sort.Strings(declared)

	got := GetCriteriaPlatformTypes()
	if len(got) != len(declared) {
		t.Fatalf("len(GetCriteriaPlatformTypes())=%d, declared constants=%d", len(got), len(declared))
	}
	for i, want := range declared {
		if got[i] != want {
			t.Errorf("GetCriteriaPlatformTypes()[%d] = %q, want %q (declared)", i, got[i], want)
		}
	}
}

// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package architecture

import (
	"testing"
)

// TestFacadeBoundary is the #2028 architecture gate. It fails when pkg/cli or
// pkg/server gains a business-logic reference that facade-policy.yaml does not
// record, when a recorded symbol changes class, or when a recorded symbol falls
// out of use.
//
// There is deliberately no testing.Short() skip: `make test` runs with -short,
// so a skip would disable this gate in the only place it runs.
func TestFacadeBoundary(t *testing.T) {
	t.Parallel()

	p := loadPolicy(t, "facade-policy.yaml")
	if problems := p.validate(); len(problems) != 0 {
		for _, problem := range problems {
			t.Errorf("policy: %s", problem)
		}
		t.FailNow()
	}

	violations := checkAgainstPolicy(observedReferences(t), p)
	for _, v := range violations {
		t.Errorf("%s: %s.%s — %s", v.Kind, v.Package, v.Symbol, v.Detail)
	}
	if len(violations) != 0 {
		t.Logf("%d violation(s). pkg/cli and pkg/server must reach business logic "+
			"through pkg/client/v1; if an exception is genuinely warranted, record it "+
			"in tests/architecture/facade-policy.yaml with a reason.", len(violations))
	}
}

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
	"sort"
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

// TestInfrastructureAllowlistIsClosed pins the infrastructure bucket's
// package SET in the committed facade-policy.yaml to a sorted literal.
//
// Infrastructure packages are exempt from symbol/class/staleness tracking
// entirely: unclassified, class-changed, and stale can never fire for them,
// because checkAgainstPolicy treats any reference into an infrastructure
// package as clean without ever consulting a symbols map. That makes
// infrastructure membership the one boundary this gate does not mechanically
// defend -- if business logic were later added to an infrastructure package,
// or a business package relocated into this bucket, checkAgainstPolicy would
// stay green with no other signal. This test exists so growing the
// allowlist requires a deliberate change to this test (and the reviewer
// sign-off that comes with reviewing it), not a silent policy-file edit that
// sails through unremarked.
func TestInfrastructureAllowlistIsClosed(t *testing.T) {
	t.Parallel()

	p := loadPolicy(t, "facade-policy.yaml")

	want := []string{
		"pkg/defaults",
		"pkg/deprecation",
		"pkg/errors",
		"pkg/header",
		"pkg/logging",
		"pkg/serializer",
	}

	got := make([]string, 0, len(p.Infrastructure))
	for name := range p.Infrastructure {
		got = append(got, name)
	}
	sort.Strings(got)

	same := len(got) == len(want)
	if same {
		for i := range want {
			if got[i] != want[i] {
				same = false
				break
			}
		}
	}
	if !same {
		t.Fatalf("infrastructure bucket = %v, want exactly %v -- infrastructure membership is "+
			"reviewer-gated, not mechanically enforced by this gate (see TestInfrastructureAllowlistIsClosed "+
			"doc comment); if this change is deliberate, update the expected set here", got, want)
	}
}

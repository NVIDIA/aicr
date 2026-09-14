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

func TestCheckAgainstPolicy(t *testing.T) {
	t.Parallel()

	base := policy{
		Version: 1,
		Facade:  []string{"pkg/client/v1"},
		Infrastructure: map[string]string{
			"pkg/errors": "structured error codes",
		},
		Constrained: map[string]constrainedPackage{
			"pkg/recipe": {
				Reason:    "wire types",
				Permanent: true,
				Symbols: map[string]symbolClass{
					"Recipe":          classType,
					"Recipe.Resolved": classBehavioral,
				},
			},
		},
	}

	tests := []struct {
		name      string
		refs      []reference
		wantKind  string
		wantCount int // defaults to 1 when wantKind is set; use to assert more than one violation
	}{
		{
			name: "facade use is always clean",
			refs: []reference{
				{Package: "pkg/client/v1", Symbol: "NewClient", Class: classBehavioral},
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classType},
				{Package: "pkg/recipe", Symbol: "Recipe.Resolved", Class: classBehavioral},
			},
		},
		{
			name: "infrastructure needs no symbol entry",
			refs: []reference{
				{Package: "pkg/errors", Symbol: "Wrap", Class: classBehavioral},
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classType},
				{Package: "pkg/recipe", Symbol: "Recipe.Resolved", Class: classBehavioral},
			},
		},
		{
			name: "recorded symbol at recorded class is clean",
			refs: []reference{
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classType},
				{Package: "pkg/recipe", Symbol: "Recipe.Resolved", Class: classBehavioral},
			},
			wantKind: "",
		},
		{
			name: "new symbol in a constrained package",
			refs: []reference{
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classType},
				{Package: "pkg/recipe", Symbol: "Recipe.Resolved", Class: classBehavioral},
				{Package: "pkg/recipe", Symbol: "Hydrate", Class: classBehavioral},
			},
			wantKind: "unclassified",
		},
		{
			name: "symbol changed class",
			refs: []reference{
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classBehavioral},
				{Package: "pkg/recipe", Symbol: "Recipe.Resolved", Class: classBehavioral},
			},
			wantKind: "class-changed",
		},
		{
			name: "recorded symbol no longer used",
			refs: []reference{
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classType},
			},
			wantKind: "stale",
		},
		{
			name: "package in no bucket",
			refs: []reference{
				{Package: "pkg/recipe", Symbol: "Recipe", Class: classType},
				{Package: "pkg/recipe", Symbol: "Recipe.Resolved", Class: classBehavioral},
				{Package: "pkg/collector", Symbol: "New", Class: classBehavioral},
			},
			wantKind: "unknown-package",
		},
		{
			// Regression guard: a constrained package referenced nowhere in this
			// pass must still surface every one of its recorded symbols as stale.
			// A guard that skips untouched packages would make this case, the
			// largest form of "stale exception protects nothing", unreportable.
			name: "unreferenced constrained package is fully stale",
			refs: []reference{
				{Package: "pkg/client/v1", Symbol: "NewClient", Class: classBehavioral},
			},
			wantKind:  "stale",
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refs := make(map[reference]bool, len(tt.refs))
			for _, r := range tt.refs {
				refs[r] = true
			}
			got := checkAgainstPolicy(refs, base)
			if tt.wantKind == "" {
				if len(got) != 0 {
					t.Fatalf("checkAgainstPolicy() = %+v, want none", got)
				}
				return
			}
			wantCount := tt.wantCount
			if wantCount == 0 {
				wantCount = 1
			}
			if len(got) != wantCount {
				t.Fatalf("checkAgainstPolicy() = %+v, want exactly %d %s violation(s)", got, wantCount, tt.wantKind)
			}
			for _, v := range got {
				if v.Kind != tt.wantKind {
					t.Errorf("Kind = %q, want %q", v.Kind, tt.wantKind)
				}
			}
		})
	}
}

type violation struct {
	Kind    string
	Package string
	Symbol  string
	Detail  string
}

// checkAgainstPolicy reports every way the observed reference set disagrees
// with the recorded policy. Results are sorted so failure output is stable.
func checkAgainstPolicy(refs map[reference]bool, p policy) []violation {
	facade := make(map[string]bool, len(p.Facade))
	for _, name := range p.Facade {
		facade[name] = true
	}

	var violations []violation
	seen := make(map[string]map[string]bool) // constrained package -> symbols observed

	for ref := range refs {
		switch {
		case facade[ref.Package]:
			continue
		case p.Infrastructure[ref.Package] != "":
			continue
		}
		entry, ok := p.Constrained[ref.Package]
		if !ok {
			violations = append(violations, violation{
				Kind:    "unknown-package",
				Package: ref.Package,
				Symbol:  ref.Symbol,
				Detail:  "package is in no policy bucket; add it to infrastructure or constrained with a reason",
			})
			continue
		}
		if seen[ref.Package] == nil {
			seen[ref.Package] = make(map[string]bool)
		}
		seen[ref.Package][ref.Symbol] = true

		recorded, ok := entry.Symbols[ref.Symbol]
		if !ok {
			violations = append(violations, violation{
				Kind:    "unclassified",
				Package: ref.Package,
				Symbol:  ref.Symbol,
				Detail:  "new " + string(ref.Class) + " use; record it in the policy or route it through pkg/client/v1",
			})
			continue
		}
		if recorded != ref.Class {
			violations = append(violations, violation{
				Kind:    "class-changed",
				Package: ref.Package,
				Symbol:  ref.Symbol,
				Detail:  "recorded " + string(recorded) + ", observed " + string(ref.Class),
			})
		}
	}

	for name, entry := range p.Constrained {
		for symbol := range entry.Symbols {
			if seen[name][symbol] {
				continue
			}
			violations = append(violations, violation{
				Kind:    "stale",
				Package: name,
				Symbol:  symbol,
				Detail:  "policy records this symbol but nothing uses it; remove the entry",
			})
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Package != violations[j].Package {
			return violations[i].Package < violations[j].Package
		}
		if violations[i].Symbol != violations[j].Symbol {
			return violations[i].Symbol < violations[j].Symbol
		}
		return violations[i].Kind < violations[j].Kind
	})
	return violations
}

// TestGateCatchesBehavioralBypass is the regression fixture required by #2028.
//
// The bypass being simulated is the subtle one: a package that is ALREADY
// allowlisted, and a type that is ALREADY permitted, gaining a new behavioral
// method call. No new import appears, so an import-level gate sees nothing.
func TestGateCatchesBehavioralBypass(t *testing.T) {
	t.Parallel()

	// A stand-in business package. Self-contained so the fixture needs no
	// export data and stays fast.
	const businessSrc = `package business

type Recipe struct{ Name string }

func (r *Recipe) Resolve() string { return r.Name }
`
	// The consumer holds a Recipe (permitted as a type) and calls Resolve on
	// it. There is no "business." selector on that call anywhere.
	const consumerSrc = `package consumer

import "fixture/business"

func Render(r *business.Recipe) string {
	return r.Resolve()
}
`
	lp := checkTwoPackages(t, businessSrc, consumerSrc)

	refs := make(map[reference]bool)
	for ref := range packageQualifiedRefs(lp, "fixture/") {
		refs[ref] = true
	}
	for ref := range foreignMethodRefs(lp, "fixture/") {
		refs[ref] = true
	}

	// The policy permits the type and nothing else.
	p := policy{
		Version: 1,
		Constrained: map[string]constrainedPackage{
			"business": {
				Reason:    "wire type rendered by the consumer",
				Permanent: true,
				Symbols:   map[string]symbolClass{"Recipe": classType},
			},
		},
	}

	violations := checkAgainstPolicy(refs, p)

	var caught bool
	for _, v := range violations {
		if v.Kind == "unclassified" && v.Symbol == "Recipe.Resolve" {
			caught = true
		}
	}
	if !caught {
		t.Fatalf("gate did not catch the behavioral bypass; violations = %+v", violations)
	}
}

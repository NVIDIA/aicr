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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// facadePackage is pkg/client/v1's module-relative import path, the one
// package every reference into is always clean. validate requires it be
// listed in the facade bucket so the generator's raw output -- which dumps
// every observed package, including the facade itself, into constrained --
// cannot be committed as-is.
const facadePackage = "pkg/client/v1"

type symbolClass string

const (
	classType       symbolClass = "type"
	classBehavioral symbolClass = "behavioral"
	classConst      symbolClass = "const"
	classVar        symbolClass = "var"
)

type constrainedPackage struct {
	Reason    string                 `yaml:"reason"`
	Tracking  string                 `yaml:"tracking,omitempty"`
	Permanent bool                   `yaml:"permanent,omitempty"`
	Symbols   map[string]symbolClass `yaml:"symbols"`
}

type policy struct {
	Version        int                           `yaml:"version"`
	Facade         []string                      `yaml:"facade"`
	Infrastructure map[string]string             `yaml:"infrastructure"`
	Constrained    map[string]constrainedPackage `yaml:"constrained"`
}

func loadPolicy(t *testing.T, path string) policy {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read policy %s: %v", path, err)
	}
	var p policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		t.Fatalf("parse policy %s: %v", path, err)
	}
	return p
}

func TestLoadPolicy(t *testing.T) {
	t.Parallel()

	p := loadPolicy(t, filepath.Join("testdata", "policy-valid.yaml"))

	if p.Version != 1 {
		t.Errorf("Version = %d, want 1", p.Version)
	}
	if got := p.Constrained["pkg/recipe"].Symbols["Recipe"]; got != classType {
		t.Errorf("pkg/recipe.Recipe class = %q, want %q", got, classType)
	}
	if got := p.Constrained["pkg/recipe"].Symbols["Recipe.Resolved"]; got != classBehavioral {
		t.Errorf("pkg/recipe.Recipe.Resolved class = %q, want %q", got, classBehavioral)
	}
	if got := p.Infrastructure["pkg/errors"]; got == "" {
		t.Error("pkg/errors carries no infrastructure reason")
	}
}

// trackingPattern is the issue-reference shape required of a constrained
// entry's tracking field: "#" followed by one or more digits. Anything else
// -- a placeholder, a bare number, a URL -- fails to name a real issue whose
// closure removes the exception.
var trackingPattern = regexp.MustCompile(`^#\d+$`)

// validate reports human-readable problems with the policy's own shape. Every
// constrained package needs a reason and exactly one of tracking/permanent, so
// an exception can never be silently permanent by omission.
func (p policy) validate() []string {
	var problems []string

	// A package listed in more than one bucket must be reported here, by
	// name, before anything else. checkAgainstPolicy checks facade, then
	// infrastructure, then constrained, and `continue`s on the first match,
	// so a package in both infrastructure and constrained never reaches the
	// constrained loop's `seen` bookkeeping -- every one of its recorded
	// symbols then surfaces as "stale ... remove the entry", which
	// misdirects the author toward deleting live entries instead of
	// pointing at the actual fault: the duplicate bucket listing.
	problems = append(problems, duplicateBucketProblems(p)...)

	for name, entry := range p.Constrained {
		if entry.Reason == "" {
			problems = append(problems, name+": missing reason")
		}
		if strings.Contains(entry.Reason, "TODO") {
			problems = append(problems, name+": reason still contains a TODO placeholder")
		}
		hasTracking := entry.Tracking != ""
		if hasTracking == entry.Permanent {
			problems = append(problems, name+": set exactly one of tracking or permanent")
		}
		if hasTracking && !trackingPattern.MatchString(entry.Tracking) {
			problems = append(problems, name+": tracking must be an issue reference like \"#1234\", got \""+entry.Tracking+"\"")
		}
		for symbol, class := range entry.Symbols {
			switch class {
			case classType, classBehavioral, classConst, classVar:
			default:
				problems = append(problems, name+"."+symbol+": unknown class "+string(class))
			}
		}
	}
	for name, reason := range p.Infrastructure {
		if reason == "" {
			problems = append(problems, name+": missing infrastructure reason")
		}
	}
	if len(p.Facade) != 1 || p.Facade[0] != facadePackage {
		problems = append(problems, "the facade bucket must contain only "+facadePackage)
	}
	return problems
}

// duplicateBucketProblems reports every package name that appears in more
// than one of the three buckets (facade, infrastructure, constrained).
// Results are sorted so validate's output is stable.
func duplicateBucketProblems(p policy) []string {
	counts := make(map[string]int)
	for _, name := range p.Facade {
		counts[name]++
	}
	for name := range p.Infrastructure {
		counts[name]++
	}
	for name := range p.Constrained {
		counts[name]++
	}

	var dups []string
	for name, count := range counts {
		if count > 1 {
			dups = append(dups, name)
		}
	}
	sort.Strings(dups)

	problems := make([]string, 0, len(dups))
	for _, name := range dups {
		problems = append(problems, name+": listed in multiple policy buckets")
	}
	return problems
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()

	validFacade := []string{facadePackage}

	tests := []struct {
		name    string
		entry   constrainedPackage
		facade  []string
		infra   map[string]string
		wantSub string
	}{
		{"valid permanent", constrainedPackage{Reason: "r", Permanent: true}, validFacade, nil, ""},
		{"valid tracking", constrainedPackage{Reason: "r", Tracking: "#2025"}, validFacade, nil, ""},
		{"missing reason", constrainedPackage{Permanent: true}, validFacade, nil, "missing reason"},
		{"neither", constrainedPackage{Reason: "r"}, validFacade, nil, "exactly one"},
		{"both", constrainedPackage{Reason: "r", Tracking: "#1", Permanent: true}, validFacade, nil, "exactly one"},
		{"bad class", constrainedPackage{Reason: "r", Permanent: true, Symbols: map[string]symbolClass{"X": "nope"}}, validFacade, nil, "unknown class"},
		// Regression guard for the generator's raw output: every constrained
		// reason it emits is the literal placeholder below, and validate must
		// reject it rather than let an unfinished policy pass green.
		{"todo reason", constrainedPackage{Reason: "TODO: state why this is not a facade gap", Permanent: true}, validFacade, nil, "TODO"},
		// Regression guard for the generator's other mistake: it sorts every
		// observed package -- including the facade itself -- into constrained
		// and writes no facade bucket at all.
		{"missing facade", constrainedPackage{Reason: "r", Permanent: true}, nil, nil, "facade bucket"},
		// Regression guard for a real hole: checkAgainstPolicy treats every
		// p.Facade entry as clean, so a second facade package would exempt
		// that package from the gate entirely while validate stayed green.
		{"second facade package", constrainedPackage{Reason: "r", Permanent: true}, []string{facadePackage, "pkg/recipe"}, nil, "facade bucket"},
		// A malformed tracking value (a typo, a placeholder) satisfies the old
		// "non-empty string" check but doesn't name an issue whose closure
		// removes the exception.
		{"malformed tracking", constrainedPackage{Reason: "r", Tracking: "TODO"}, validFacade, nil, "issue reference"},
		{"well-formed tracking", constrainedPackage{Reason: "r", Tracking: "#2561"}, validFacade, nil, ""},
		// Coverage for the untested infrastructure empty-reason branch: without
		// this row, dropping the guard from validate would pass every other
		// case in this table.
		{"infrastructure empty reason", constrainedPackage{Reason: "r", Permanent: true}, validFacade, map[string]string{"pkg/y": ""}, "missing infrastructure reason"},
		// Regression guard for the misdirected-stale-error bug: pkg/x listed in
		// both infrastructure and constrained must be reported as a duplicate
		// bucket listing, not (via checkAgainstPolicy's infrastructure
		// short-circuit skipping the constrained `seen` bookkeeping) as every
		// one of its constrained symbols going stale.
		{"duplicate bucket", constrainedPackage{Reason: "r", Permanent: true}, validFacade, map[string]string{"pkg/x": "infra reason"}, "listed in multiple policy buckets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := policy{Facade: tt.facade, Infrastructure: tt.infra, Constrained: map[string]constrainedPackage{"pkg/x": tt.entry}}
			problems := p.validate()
			if tt.wantSub == "" {
				if len(problems) != 0 {
					t.Errorf("validate() = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("validate() returned no problems, want one containing %q", tt.wantSub)
			}
			if !strings.Contains(problems[0], tt.wantSub) {
				t.Errorf("validate() = %q, want substring %q", problems[0], tt.wantSub)
			}
		})
	}
}

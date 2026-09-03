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
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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

// validate reports human-readable problems with the policy's own shape. Every
// constrained package needs a reason and exactly one of tracking/permanent, so
// an exception can never be silently permanent by omission.
func (p policy) validate() []string {
	var problems []string
	for name, entry := range p.Constrained {
		if entry.Reason == "" {
			problems = append(problems, name+": missing reason")
		}
		hasTracking := entry.Tracking != ""
		if hasTracking == entry.Permanent {
			problems = append(problems, name+": set exactly one of tracking or permanent")
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
	return problems
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entry   constrainedPackage
		wantSub string
	}{
		{"valid permanent", constrainedPackage{Reason: "r", Permanent: true}, ""},
		{"valid tracking", constrainedPackage{Reason: "r", Tracking: "#2025"}, ""},
		{"missing reason", constrainedPackage{Permanent: true}, "missing reason"},
		{"neither", constrainedPackage{Reason: "r"}, "exactly one"},
		{"both", constrainedPackage{Reason: "r", Tracking: "#1", Permanent: true}, "exactly one"},
		{"bad class", constrainedPackage{Reason: "r", Permanent: true, Symbols: map[string]symbolClass{"X": "nope"}}, "unknown class"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := policy{Constrained: map[string]constrainedPackage{"pkg/x": tt.entry}}
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

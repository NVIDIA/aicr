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

package uatbroker

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

var errInvalid = errors.New(errors.ErrCodeInvalidRequest, "")

// compatRegistry is a minimal registry with gcp and azure lanes.
func compatRegistry() *Registry {
	return &Registry{Reservations: []Reservation{
		{Name: "gcp-h100", Cloud: CloudGCP},
		{Name: "azure-h100", Cloud: CloudAzure},
	}}
}

const validCompat = `floors:
  - lane: gcp
    intents: [training]
    min-release: v0.22.0
    reason: >-
      tcpxo
      runtime
  - lane: azure
    intents: [training, inference]
    min-release: v0.18.0
    reason: driver flip
`

func TestParseCompat(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"valid rows", validCompat, false},
		{"explicit empty floors", "floors: []\n", false},
		{"empty document", "", true},
		{"missing floors key", "# nothing\n{}\n", true},
		{"unknown key", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: v0.22.0\n    reason: r\n    accelerator: h100\n", true},
		{"empty reason", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: v0.22.0\n    reason: '  '\n", true},
		{"empty lane", "floors:\n  - lane: ''\n    intents: [training]\n    min-release: v0.22.0\n    reason: r\n", true},
		{"empty intents", "floors:\n  - lane: gcp\n    intents: []\n    min-release: v0.22.0\n    reason: r\n", true},
		{"duplicate intents", "floors:\n  - lane: gcp\n    intents: [training, training]\n    min-release: v0.22.0\n    reason: r\n", true},
		{"invalid intent", "floors:\n  - lane: gcp\n    intents: [serving]\n    min-release: v0.22.0\n    reason: r\n", true},
		{"bad semver", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: vlatest\n    reason: r\n", true},
		{"partial semver", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: v0.22\n    reason: r\n", true},
		{"pre-release", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: v0.22.0-rc1\n    reason: r\n", true},
		{"build metadata", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: v0.22.0+abc\n    reason: r\n", true},
		{"missing v prefix", "floors:\n  - lane: gcp\n    intents: [training]\n    min-release: 0.22.0\n    reason: r\n", true},
		{
			"duplicate lane/intent across rows",
			"floors:\n  - lane: gcp\n    intents: [training]\n    min-release: v0.22.0\n    reason: r\n" +
				"  - lane: gcp\n    intents: [inference, training]\n    min-release: v0.21.0\n    reason: r\n",
			true,
		},
		{"malformed yaml", "floors: [\n", true},
		{
			"min-release via merge key has no line",
			"floors:\n  - &base\n    lane: gcp\n    intents: [training]\n    min-release: v0.22.0\n    reason: r\n" +
				"  - <<: *base\n    lane: azure\n",
			true,
		},
		{
			"floor row via alias has no line",
			"floors:\n  - &row\n    lane: gcp\n    intents: [training]\n    min-release: v0.22.0\n    reason: r\n" +
				"  - *row\n",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCompat([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCompat error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !stderrors.Is(err, errInvalid) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestParseCompatRecordsLines(t *testing.T) {
	c, err := ParseCompat([]byte(validCompat))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, 0, len(c.Floors))
	for _, f := range c.Floors {
		got = append(got, f.Line)
	}
	if want := []int{4, 10}; !reflect.DeepEqual(got, want) {
		t.Errorf("min-release lines = %v, want %v", got, want)
	}
	if c.Floors[0].Reason != "tcpxo runtime" {
		t.Errorf("folded reason = %q", c.Floors[0].Reason)
	}
}

func TestCompatValidate(t *testing.T) {
	tests := []struct {
		name    string
		lane    string
		reg     *Registry
		wantErr bool
	}{
		{"lane is a registry cloud", CloudGCP, compatRegistry(), false},
		{"lane not a registry cloud", CloudAWS, compatRegistry(), true},
		{"unknown lane", "moon", compatRegistry(), true},
		{"nil registry", CloudGCP, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Compat{Floors: []Floor{{Lane: tt.lane, Intents: []string{IntentTraining}, MinRelease: "v1.0.0", Reason: "r"}}}
			if err := c.Validate(tt.reg); (err != nil) != tt.wantErr {
				t.Errorf("Validate error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	// Validate also re-runs the schema rules.
	if err := (&Compat{}).Validate(compatRegistry()); err == nil {
		t.Error("Validate(nil floors) = nil, want error")
	}
}

func TestFloorFor(t *testing.T) {
	c, err := ParseCompat([]byte(validCompat))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		c         *Compat
		cloud     string
		intent    string
		wantFloor string
		wantOK    bool
	}{
		{"hit", c, CloudAzure, IntentInference, "v0.18.0", true},
		{"lane with one intent gated: gated intent", c, CloudGCP, IntentTraining, "v0.22.0", true},
		{"lane with one intent gated: other intent", c, CloudGCP, IntentInference, "", false},
		{"miss", c, CloudAWS, IntentTraining, "", false},
		{"nil compat", nil, CloudGCP, IntentTraining, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := tt.c.FloorFor(tt.cloud, tt.intent)
			if ok != tt.wantOK || f.MinRelease != tt.wantFloor {
				t.Errorf("FloorFor = (%q, %v), want (%q, %v)", f.MinRelease, ok, tt.wantFloor, tt.wantOK)
			}
		})
	}
}

func TestWithoutLines(t *testing.T) {
	c, err := ParseCompat([]byte(validCompat))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		lines     []int
		wantLanes []string
		wantErr   bool
	}{
		{"none", nil, []string{CloudGCP, CloudAzure}, false},
		{"drop gcp", []int{4}, []string{CloudAzure}, false},
		{"drop both", []int{4, 10}, []string{}, false},
		{"unknown line", []int{5}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.WithoutLines(tt.lines)
			if (err != nil) != tt.wantErr {
				t.Fatalf("WithoutLines error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			lanes := []string{}
			for _, f := range got.Floors {
				lanes = append(lanes, f.Lane)
			}
			if !reflect.DeepEqual(lanes, tt.wantLanes) {
				t.Errorf("lanes = %v, want %v", lanes, tt.wantLanes)
			}
		})
	}
	if len(c.Floors) != 2 {
		t.Errorf("WithoutLines mutated the receiver: %d floors", len(c.Floors))
	}
}

func TestCheckFloors(t *testing.T) {
	floor := func(v string) *Compat {
		return &Compat{Floors: []Floor{{Lane: CloudGCP, Intents: []string{IntentTraining}, MinRelease: v, Reason: "r", Line: 7}}}
	}
	tags := []string{"v0.20.0", "v0.21.0", "v0.21.1", "v0.22.0", "v0.23.0-rc1", "junk"}
	tests := []struct {
		name         string
		c            *Compat
		tags         []string
		containing   map[int][]string
		previousN    int
		wantKind     string // "" = no problem
		wantExpected string
	}{
		{"floor equals lowest containing tag", floor("v0.22.0"), tags, map[int][]string{7: {"v0.22.0"}}, -1, "", ""},
		{
			"lower tag contains commit: over-high, suggests lowest",
			floor("v0.22.0"), tags, map[int][]string{7: {"v0.22.0", "v0.21.0", "v0.21.1"}}, -1,
			FloorProblemOverHigh, "v0.21.0",
		},
		{"under-low floor is not flagged", floor("v0.20.0"), tags, map[int][]string{7: {"v0.22.0"}}, -1, "", ""},
		{
			"pre-release containing tags are ignored",
			floor("v0.23.0"), tags, map[int][]string{7: {"v0.23.0-rc1"}}, -1, "", "",
		},
		{"untagged commit, next patch", floor("v0.22.1"), tags, map[int][]string{7: {}}, -1, "", ""},
		{"untagged commit, next minor", floor("v0.23.0"), tags, map[int][]string{7: {}}, -1, "", ""},
		{"untagged commit, next major", floor("v1.0.0"), tags, map[int][]string{7: {}}, -1, "", ""},
		{"untagged commit, existing tag", floor("v0.21.1"), tags, map[int][]string{7: {}}, -1, "", ""},
		{
			"untagged commit, beyond next minor",
			floor("v0.24.0"), tags, map[int][]string{7: {}}, -1, FloorProblemOverHigh, "v0.23.0",
		},
		{
			"untagged commit, skipped patch",
			floor("v0.22.2"), tags, map[int][]string{7: {}}, -1, FloorProblemOverHigh, "v0.23.0",
		},
		{"no stable tags: inconclusive", floor("v0.22.0"), []string{"v0.22.0-rc1"}, map[int][]string{7: {}}, -1, FloorProblemInconclusive, ""},
		{"line missing from containing: inconclusive", floor("v0.22.0"), tags, map[int][]string{8: {}}, -1, FloorProblemInconclusive, ""},
		{"unparseable floor: inconclusive", floor("latest"), tags, map[int][]string{7: {}}, -1, FloorProblemInconclusive, ""},
		{"inert within window", floor("v0.21.1"), tags, map[int][]string{7: {"v0.21.1"}}, 2, FloorProblemInert, ""},
		{"not inert when window reaches below", floor("v0.21.1"), tags, map[int][]string{7: {"v0.21.1"}}, 3, "", ""},
		{"inert not reported without window", floor("v0.20.0"), tags, map[int][]string{7: {"v0.20.0"}}, -1, "", ""},
		{"nil compat", nil, tags, nil, -1, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckFloors(tt.c, tt.tags, tt.containing, tt.previousN)
			if got == nil {
				t.Fatal("CheckFloors returned nil, want a non-nil slice (encodes as [])")
			}
			if tt.wantKind == "" {
				if len(got) != 0 {
					t.Errorf("problems = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("problems = %+v, want exactly one", got)
			}
			p := got[0]
			if p.Kind != tt.wantKind || p.Expected != tt.wantExpected {
				t.Errorf("problem = %+v, want kind %q expected %q", p, tt.wantKind, tt.wantExpected)
			}
			wantSev := SeverityError
			if tt.wantKind == FloorProblemInert {
				wantSev = SeverityNotice
			}
			if p.Severity != wantSev || p.Line != 7 || p.Lane != CloudGCP || p.Message == "" {
				t.Errorf("problem = %+v, want severity %q line 7 lane gcp and a message", p, wantSev)
			}
			if strings.Contains(p.Message, "\n") {
				t.Errorf("message is not one line: %q", p.Message)
			}
			if tt.wantExpected != "" && !strings.Contains(p.Message, tt.wantExpected) {
				t.Errorf("message %q does not name expected floor %s", p.Message, tt.wantExpected)
			}
		})
	}
}

func TestParseContaining(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[int][]string
		wantErr bool
	}{
		{"tab separated", "4\tv0.22.0\tv0.22.1\n11\n", map[int][]string{4: {"v0.22.0", "v0.22.1"}, 11: {}}, false},
		{"space separated, blank lines", "\n4 v0.22.0\n\n", map[int][]string{4: {"v0.22.0"}}, false},
		{"repeated lines are unioned", "4\tv1\n4\tv2\n", map[int][]string{4: {"v1", "v2"}}, false},
		{"empty", "", map[int][]string{}, false},
		{"non-numeric line", "abc\tv1\n", nil, true},
		{"zero line", "0\n", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseContaining([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompatGate(t *testing.T) {
	c, err := ParseCompat([]byte(validCompat))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		c           *Compat
		cloud       string
		intent      string
		version     string
		wantErr     bool
		wantMessage string
	}{
		{"below floor refused", c, CloudGCP, IntentTraining, "v0.21.1", true, "floor v0.22.0"},
		{"pre-release of floor refused", c, CloudGCP, IntentTraining, "v0.22.0-rc2", true, "floor v0.22.0"},
		{"at floor allowed", c, CloudGCP, IntentTraining, "v0.22.0", false, "meets"},
		{"above floor allowed", c, CloudGCP, IntentTraining, "v0.23.0", false, "meets"},
		{"no floor allowed", c, CloudGCP, IntentInference, "v0.1.0", false, "no harness-compat floor"},
		{"nil compat allowed", nil, CloudGCP, IntentTraining, "v0.1.0", false, "no harness-compat floor"},
		{"bad version", c, CloudGCP, IntentTraining, "latest", true, "not semver"},
		{
			"unvalidated bad floor",
			&Compat{Floors: []Floor{{Lane: CloudGCP, Intents: []string{IntentTraining}, MinRelease: "x"}}},
			CloudGCP, IntentTraining, "v1.0.0", true, "not semver",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := tt.c.Gate(tt.cloud, tt.intent, tt.version)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Gate error = %v, wantErr %v", err, tt.wantErr)
			}
			text := msg
			if err != nil {
				text = err.Error()
				if !stderrors.Is(err, errInvalid) {
					t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
				}
			}
			if !strings.Contains(text, tt.wantMessage) {
				t.Errorf("Gate output %q does not contain %q", text, tt.wantMessage)
			}
		})
	}
}

func TestLoadCompatFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid", write("ok.yaml", validCompat), false},
		{"missing file", filepath.Join(dir, "nope.yaml"), true},
		{"oversized", write("big.yaml", "# "+strings.Repeat("x", int(maxCompatBytes))+"\n"), true},
		{"schema error", write("bad.yaml", "floors:\n  - lane: gcp\n"), true},
		{"lane not in registry", write("lane.yaml", strings.ReplaceAll(validCompat, "lane: gcp", "lane: kind")), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCompatFile(tt.path, compatRegistry())
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadCompatFile error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestCommittedCompatFile asserts only the structure of tests/uat/compat.yaml
// (parses, validates against the committed registry, every lane has a fixture
// directory), never specific floor values. The gcp training row is the one
// exception: it proves the #2705 gate migrated out of the registry.
func TestCommittedCompatFile(t *testing.T) {
	root := filepath.Join("..", "..")
	reg, err := LoadRegistryFile(filepath.Join(root, "infra", "uat", "reservations.yaml"))
	if err != nil {
		t.Fatalf("load committed registry: %v", err)
	}
	c, err := LoadCompatFile(filepath.Join(root, "tests", "uat", "compat.yaml"), reg)
	if err != nil {
		t.Fatalf("load committed compat file: %v", err)
	}
	for _, f := range c.Floors {
		if st, statErr := os.Stat(filepath.Join(root, "tests", "uat", f.Lane)); statErr != nil || !st.IsDir() {
			t.Errorf("compat lane %q has no tests/uat/%s/ directory", f.Lane, f.Lane)
		}
	}
	if _, ok := c.FloorFor(CloudGCP, IntentTraining); !ok {
		t.Error("committed compat file has no gcp/training floor (the migrated #2705 gate)")
	}
}

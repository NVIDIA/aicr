// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"strings"
	"testing"
)

const sampleReport = `{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "ghcr.io/nvidia/nvsentinel",
                "depType": "registry-chart",
                "datasource": "docker",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "v1.20.3", "updateType": "patch"},
                  {"newValue": "v1.23.0", "updateType": "minor"}
                ]
              },
              {
                "depName": "cert-manager",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.2",
                "updates": []
              },
              {
                "depName": "dynamo-platform",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "1.4.2",
                "skipReason": "invalid-value",
                "warnings": [{"message": "Failed to look up helm package"}],
                "updates": []
              },
              {
                "depName": "no-lookup-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v2.0.0"
              },
              {
                "depName": "golang.org/x/net",
                "depType": "require",
                "datasource": "go",
                "currentValue": "v0.1.0",
                "updates": [{"newValue": "v0.2.0", "updateType": "minor"}]
              }
            ]
          }
        ]
      }
    }
  }
}`

func TestParseRenovateReport(t *testing.T) {
	got, err := ParseRenovateReport([]byte(sampleReport))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d registry-chart deps, want 4 (non-registry depTypes must be dropped)", len(got))
	}

	tests := []struct {
		name                           string
		dep                            string
		wantLatest, wantType, wantProb string
	}{
		{"highest update wins", "ghcr.io/nvidia/nvsentinel", "v1.23.0", "minor", ""},
		{"empty updates array means current", "cert-manager", "", "", ""},
		{"skipReason surfaces as a problem", "dynamo-platform", "", "", "invalid-value: Failed to look up helm package"},
		{"absent updates key means unresolved, not current", "no-lookup-chart", "", "",
			"no updates array: Renovate lookup did not run for this dep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, ok := got[tt.dep]
			if !ok {
				t.Fatalf("dep %q missing from result", tt.dep)
			}
			if l.Latest != tt.wantLatest || l.UpdateType != tt.wantType || l.Problem != tt.wantProb {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					l.Latest, l.UpdateType, l.Problem, tt.wantLatest, tt.wantType, tt.wantProb)
			}
		})
	}
}

func TestParseRenovateReportRejectsGarbage(t *testing.T) {
	if _, err := ParseRenovateReport([]byte(`{"repositories":`)); err == nil {
		t.Fatal("want error on truncated JSON, got nil")
	}
}

func TestParseRenovateReportIncompleteUpdates(t *testing.T) {
	tests := []struct {
		name                           string
		report                         string
		wantLatest, wantType, wantProb string
	}{
		{
			"sole update missing newValue",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "no-newvalue-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "", "updateType": "minor"}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"",
			"",
			"update missing newValue",
		},
		{
			"sole update missing updateType",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "no-updatetype-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "v1.23.0", "updateType": ""}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"",
			"",
			"update missing updateType",
		},
		{
			"malformed entry alongside a valid recognized update",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "mixed-malformed-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "", "updateType": "minor"},
                  {"newValue": "v1.23.0", "updateType": "minor"}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"v1.23.0",
			"minor",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRenovateReport([]byte(tt.report))
			if err != nil {
				t.Fatalf("ParseRenovateReport: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d deps, want 1", len(got))
			}
			var l Lookup
			for _, dep := range got {
				l = dep
				break
			}
			if l.Latest != tt.wantLatest || l.UpdateType != tt.wantType || l.Problem != tt.wantProb {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					l.Latest, l.UpdateType, l.Problem, tt.wantLatest, tt.wantType, tt.wantProb)
			}
		})
	}
}

func TestParseRenovateReportUnsupportedUpdateTypes(t *testing.T) {
	tests := []struct {
		name                           string
		report                         string
		wantLatest, wantType, wantProb string
	}{
		{
			"only rollback update",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "ahead-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v2.0.0",
                "updates": [
                  {"newValue": "v1.19.0", "updateType": "rollback"}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"",
			"",
			"unsupported update type: rollback",
		},
		{
			"mixed rollback and minor update",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "mixed-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "v1.19.0", "updateType": "rollback"},
                  {"newValue": "v1.23.0", "updateType": "minor"}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"v1.23.0",
			"minor",
			"",
		},
		{
			"same-rank tie between two patches",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "tied-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "v1.20.1", "updateType": "patch"},
                  {"newValue": "v1.20.2", "updateType": "patch"}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"v1.20.2",
			"patch",
			"",
		},
		{
			"skipReason with only unrecognized update",
			`{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "deprecated-chart",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.5.0",
                "skipReason": "package-renamed",
                "updates": [
                  {"newValue": "v1.6.0", "updateType": "replacement"}
                ]
              }
            ]
          }
        ]
      }
    }
  }
}`,
			"",
			"",
			"package-renamed: unsupported update type: replacement",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRenovateReport([]byte(tt.report))
			if err != nil {
				t.Fatalf("ParseRenovateReport: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d deps, want 1", len(got))
			}
			var l Lookup
			for _, dep := range got {
				l = dep
				break
			}
			if l.Latest != tt.wantLatest || l.UpdateType != tt.wantType || l.Problem != tt.wantProb {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					l.Latest, l.UpdateType, l.Problem, tt.wantLatest, tt.wantType, tt.wantProb)
			}
		})
	}
}

// Renovate emits one entry per update type, and the report used to keep only
// the highest-ranked one. That discarded the safer step when both existed: in
// the 2026-09-28 run kube-prometheus-stack offered minor 84.5.0 alongside major
// 91.5.2, and only the major reached the digest (#2791). Candidates keeps all
// of them, ordered safest-first so the ordering is stable across runs.
func TestParseRenovateReportCollectsAllCandidates(t *testing.T) {
	got, err := ParseRenovateReport([]byte(sampleReport))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}

	tests := []struct {
		name string
		dep  string
		want []Candidate
	}{
		{
			name: "every recognized update is kept, ranked safest first",
			dep:  "ghcr.io/nvidia/nvsentinel",
			want: []Candidate{
				{Version: "v1.20.3", UpdateType: "patch"},
				{Version: "v1.23.0", UpdateType: "minor"},
			},
		},
		{"a current pin has no candidates", "cert-manager", nil},
		{"an unresolved pin has no candidates", "no-lookup-chart", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, ok := got[tt.dep]
			if !ok {
				t.Fatalf("dep %q missing from result", tt.dep)
			}
			if len(l.Candidates) != len(tt.want) {
				t.Fatalf("got %d candidates %v, want %d %v",
					len(l.Candidates), l.Candidates, len(tt.want), tt.want)
			}
			for i := range tt.want {
				if l.Candidates[i] != tt.want[i] {
					t.Errorf("candidate %d: got %+v, want %+v", i, l.Candidates[i], tt.want[i])
				}
			}
		})
	}
}

// Latest stays the highest-ranked candidate. Summary.Behind counts rows whose
// Latest is non-empty, so changing which candidate wins would silently change
// the headline counts this report has always published.
func TestParseRenovateReportLatestStillWins(t *testing.T) {
	got, err := ParseRenovateReport([]byte(sampleReport))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	l := got["ghcr.io/nvidia/nvsentinel"]
	if l.Latest != "v1.23.0" || l.UpdateType != "minor" {
		t.Errorf("got Latest=%q UpdateType=%q, want v1.23.0/minor", l.Latest, l.UpdateType)
	}
}

// Renovate emits at most one update per type, so a rank tie should be
// unreachable -- but that is its behavior, not a guarantee, and the sort must
// not fall back to the array order it exists to normalize. Feeding two entries
// of the same type in both orders must produce the same Candidates.
func TestParseRenovateReportCandidateOrderIsTotal(t *testing.T) {
	report := func(first, second string) string {
		return `{"repositories":{"r":{"packageFiles":{"custom.regex":[{"packageFile":"recipes/registry.yaml","deps":[
          {"depName":"dup","depType":"registry-chart","datasource":"docker","currentValue":"1.0.0",
           "updates":[{"newValue":"` + first + `","updateType":"minor"},
                      {"newValue":"` + second + `","updateType":"minor"}]}]}]}}}}`
	}

	// 1.9.0 against 1.10.0: lexically "1.10.0" sorts first, which would put the
	// larger step ahead of the smaller one and contradict the safest-first
	// ordering Alternatives is documented to have.
	ascending, err := ParseRenovateReport([]byte(report("1.9.0", "1.10.0")))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	descending, err := ParseRenovateReport([]byte(report("1.10.0", "1.9.0")))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}

	got, rev := ascending["dup"].Candidates, descending["dup"].Candidates
	if len(got) != 2 || len(rev) != 2 {
		t.Fatalf("got %d and %d candidates, want 2 each", len(got), len(rev))
	}
	for i := range got {
		if got[i] != rev[i] {
			t.Fatalf("input order changed the result: %+v vs %+v", got, rev)
		}
	}
	if got[0].Version != "1.9.0" {
		t.Errorf("tie not broken by semver precedence: got %+v", got)
	}
}

// Not every chart version parses as SemVer, and an unparseable pair still has
// to order deterministically or the artifact churns between identical runs.
func TestParseRenovateReportCandidateOrderHandlesNonSemver(t *testing.T) {
	report := func(first, second string) string {
		return `{"repositories":{"r":{"packageFiles":{"custom.regex":[{"packageFile":"recipes/registry.yaml","deps":[
          {"depName":"dup","depType":"registry-chart","datasource":"docker","currentValue":"1.0.0",
           "updates":[{"newValue":"` + first + `","updateType":"minor"},
                      {"newValue":"` + second + `","updateType":"minor"}]}]}]}}}}`
	}
	a, err := ParseRenovateReport([]byte(report("not-a-version", "also-not")))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	b, err := ParseRenovateReport([]byte(report("also-not", "not-a-version")))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	if a["dup"].Candidates[0] != b["dup"].Candidates[0] {
		t.Errorf("unparseable versions did not order deterministically: %+v vs %+v",
			a["dup"].Candidates, b["dup"].Candidates)
	}
}

// versionLess has to be a strict weak order, not merely deterministic per
// pair: sort.Slice is undefined on a comparator that cycles, so a mixed set of
// parseable and unparseable versions could order differently per input order.
// Grouping parseable ahead of unparseable is what removes the cycle
// "1.9.0" < "1.10.0" < "1.5.0_invalid" < "1.9.0" that comparing each pair on
// its own terms produced.
func TestVersionLessIsTransitive(t *testing.T) {
	vs := []string{"1.9.0", "1.10.0", "1.5.0_invalid", "2.0.0", "not-a-version", "v1.9.0"}
	for _, a := range vs {
		for _, b := range vs {
			for _, c := range vs {
				if versionLess(a, b) && versionLess(b, c) && !versionLess(a, c) {
					t.Errorf("not transitive: %q<%q and %q<%q but not %q<%q", a, b, b, c, a, c)
				}
			}
		}
	}
	// Asymmetry: a<b and b<a cannot both hold.
	for _, a := range vs {
		for _, b := range vs {
			if versionLess(a, b) && versionLess(b, a) {
				t.Errorf("not asymmetric: %q and %q each compare less than the other", a, b)
			}
		}
	}
}

// The mixed set must land the same way regardless of the order Renovate
// happened to emit it in.
func TestParseRenovateReportMixedVersionOrderIsStable(t *testing.T) {
	build := func(vs ...string) string {
		ups := make([]string, 0, len(vs))
		for _, v := range vs {
			ups = append(ups, `{"newValue":"`+v+`","updateType":"minor"}`)
		}
		return `{"repositories":{"r":{"packageFiles":{"custom.regex":[{"packageFile":"recipes/registry.yaml","deps":[
          {"depName":"mixed","depType":"registry-chart","datasource":"docker","currentValue":"1.0.0",
           "updates":[` + strings.Join(ups, ",") + `]}]}]}}}}`
	}
	forward, err := ParseRenovateReport([]byte(build("1.9.0", "1.10.0", "1.5.0_invalid")))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	reverse, err := ParseRenovateReport([]byte(build("1.5.0_invalid", "1.10.0", "1.9.0")))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	got, rev := forward["mixed"].Candidates, reverse["mixed"].Candidates
	if len(got) != 3 || len(rev) != 3 {
		t.Fatalf("got %d and %d candidates, want 3 each", len(got), len(rev))
	}
	for i := range got {
		if got[i] != rev[i] {
			t.Fatalf("input order changed the result:\n  %+v\n  %+v", got, rev)
		}
	}
	// Parseable versions first, in SemVer order; unparseable last.
	want := []string{"1.9.0", "1.10.0", "1.5.0_invalid"}
	for i, w := range want {
		if got[i].Version != w {
			t.Errorf("position %d: got %q, want %q (full: %+v)", i, got[i].Version, w, got)
		}
	}
}

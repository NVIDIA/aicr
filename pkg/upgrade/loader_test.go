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

package upgrade

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

type mapSource map[string][]byte

func (m mapSource) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, ok := m[path]
	if !ok {
		return nil, errors.New(errors.ErrCodeNotFound, "no such file: "+path)
	}
	return b, nil
}

func validRecord(component string) string {
	return "apiVersion: " + header.GroupVersionV1Beta1 + "\n" +
		"kind: " + ComponentUpgradesKind + "\n" +
		"component: " + component + "\n" +
		"transitions:\n" +
		"  - from: \"<0.18.0\"\n" +
		"    to: \">=0.18.0 <=0.18.0\"\n" +
		"    verdict: manual\n" +
		"    summary: the rename\n" +
		"    stepsByDeployer:\n" +
		"      - steps:\n" +
		"          - id: rename\n" +
		"            description: do the thing\n"
}

func TestLoadSuccess(t *testing.T) {
	src := mapSource{"upgrades/nw.yaml": []byte(validRecord("nw"))}
	comps := []Component{{Name: "nw", File: "upgrades/nw.yaml", PinnedVersion: "v0.18.0"}}

	set, err := Load(context.Background(), src, comps)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if len(set) != 1 || set["nw"] == nil {
		t.Fatalf("set = %v, want one entry keyed nw", set)
	}
	if set["nw"].Transitions[0].Verdict != VerdictManual {
		t.Errorf("verdict = %q, want manual", set["nw"].Transitions[0].Verdict)
	}
}

// A component with no record is not an error here — that is #2535's gate.
func TestLoadSkipsComponentsWithoutRecords(t *testing.T) {
	set, err := Load(context.Background(), mapSource{}, []Component{{Name: "nw"}})
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if len(set) != 0 {
		t.Errorf("set = %v, want empty", set)
	}
}

func TestLoadRejectsBadHeaders(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantText []string
	}{
		{
			name:     "unrecognized apiVersion names the one expected value",
			body:     strings.Replace(validRecord("nw"), header.GroupVersionV1Beta1, "aicr.run/v9", 1),
			wantText: []string{"aicr.run/v9", header.GroupVersionV1Beta1},
		},
		{
			// ADR-021:149 and ADR-022:103 both put this kind on the beta track
			// from the start: there is no alpha version to emit and later retire.
			name:     "the alpha authoring version is not accepted",
			body:     strings.Replace(validRecord("nw"), header.GroupVersionV1Beta1, header.AuthoringGroupVersion, 1),
			wantText: []string{header.AuthoringGroupVersion, header.GroupVersionV1Beta1},
		},
		{
			name:     "empty apiVersion is not tolerated",
			body:     strings.Replace(validRecord("nw"), "apiVersion: "+header.GroupVersionV1Beta1, "apiVersion: \"\"", 1),
			wantText: []string{"apiVersion"},
		},
		{
			name:     "wrong kind",
			body:     strings.Replace(validRecord("nw"), ComponentUpgradesKind, "RecipeMetadata", 1),
			wantText: []string{"RecipeMetadata", ComponentUpgradesKind},
		},
		{
			name:     "authored unknown verdict is rejected, not degraded",
			body:     strings.Replace(validRecord("nw"), "verdict: manual", "verdict: unknown", 1),
			wantText: []string{"unknown"},
		},
		{
			name:     "authored unversioned verdict is rejected",
			body:     strings.Replace(validRecord("nw"), "verdict: manual", "verdict: unversioned", 1),
			wantText: []string{"unversioned"},
		},
		{
			name:     "component does not match the registry entry",
			body:     validRecord("wrong-name"),
			wantText: []string{"wrong-name", "nw"},
		},
		{
			name:     "unparseable from range",
			body:     strings.Replace(validRecord("nw"), `from: "<0.18.0"`, `from: "^0.18.0"`, 1),
			wantText: []string{"from"},
		},
		{
			name:     "prerelease in from is rejected",
			body:     strings.Replace(validRecord("nw"), `from: "<0.18.0"`, `from: "<0.18.0-rc.1"`, 1),
			wantText: []string{"prerelease"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := mapSource{"upgrades/nw.yaml": []byte(tt.body)}
			comps := []Component{{Name: "nw", File: "upgrades/nw.yaml", PinnedVersion: "v0.18.0"}}

			_, err := Load(context.Background(), src, comps)
			if err == nil {
				t.Fatal("Load = nil error, want rejection")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// An unknown or misspelled field must be an error, not silently dropped: a
// typo'd hooks: or verifedBy: would otherwise weaken a record with no rule
// firing at all.
func TestLoadRejectsUnknownFields(t *testing.T) {
	body := strings.Replace(validRecord("nw"), "    summary: the rename",
		"    verifedBy: a uat lane\n    summary: the rename", 1)
	src := mapSource{"upgrades/nw.yaml": []byte(body)}
	comps := []Component{{Name: "nw", File: "upgrades/nw.yaml", PinnedVersion: "v0.18.0"}}

	_, err := Load(context.Background(), src, comps)
	if err == nil {
		t.Fatal("Load = nil error, want rejection of the unknown field")
	}
	if !strings.Contains(err.Error(), "verifedBy") {
		t.Errorf("error %q does not name the offending field", err.Error())
	}
}

// A record asserting nothing must not read as well-formed.
func TestLoadRejectsRecordWithNoAssertions(t *testing.T) {
	body := "apiVersion: " + header.GroupVersionV1Beta1 + "\n" +
		"kind: " + ComponentUpgradesKind + "\n" +
		"component: nw\n" +
		"transitions: []\n"
	src := mapSource{"upgrades/nw.yaml": []byte(body)}
	comps := []Component{{Name: "nw", File: "upgrades/nw.yaml", PinnedVersion: "v0.18.0"}}

	_, err := Load(context.Background(), src, comps)
	if err == nil {
		t.Fatal("Load = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "neither transitions nor a replaces") {
		t.Errorf("error %q does not explain the problem", err.Error())
	}
}

// Last-wins on a duplicate name would silently never validate the first record.
func TestLoadRejectsDuplicateComponentNames(t *testing.T) {
	src := mapSource{
		"upgrades/a.yaml": []byte(validRecord("nw")),
		"upgrades/b.yaml": []byte(validRecord("nw")),
	}
	comps := []Component{
		{Name: "nw", File: "upgrades/a.yaml", PinnedVersion: "v0.18.0"},
		{Name: "nw", File: "upgrades/b.yaml", PinnedVersion: "v0.18.0"},
	}
	if _, err := Load(context.Background(), src, comps); err == nil {
		t.Fatal("Load = nil error, want rejection of the duplicate name")
	}
}

// A prerelease ceiling in `to` must load. Real components are pinned at
// prereleases, and such a pin otherwise cannot have a record reaching it.
func TestLoadAllowsPrereleaseInTo(t *testing.T) {
	body := strings.Replace(validRecord("pre"),
		`to: ">=0.18.0 <=0.18.0"`, `to: ">=0.1.0-alpha.1 <=0.1.0-alpha.12"`, 1)
	body = strings.Replace(body, `from: "<0.18.0"`, `from: "<0.1.0"`, 1)
	src := mapSource{"upgrades/pre.yaml": []byte(body)}
	comps := []Component{{Name: "pre", File: "upgrades/pre.yaml", PinnedVersion: "v0.1.0-alpha.12"}}

	if _, err := Load(context.Background(), src, comps); err != nil {
		t.Fatalf("Load error = %v", err)
	}
}

// A structured ReadFile error keeps its own code rather than flattening.
func TestLoadPropagatesReadError(t *testing.T) {
	comps := []Component{{Name: "nw", File: "upgrades/missing.yaml", PinnedVersion: "v0.18.0"}}
	_, err := Load(context.Background(), mapSource{}, comps)
	if err == nil {
		t.Fatal("Load = nil error, want failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("error = %v, want inner ErrCodeNotFound preserved", err)
	}
}

// A nil Source with a record to read is a caller bug, but panicking on it
// turns a misconfiguration into a crash in whatever process called Load.
func TestLoadRejectsNilSource(t *testing.T) {
	comps := []Component{{Name: "nw", File: "upgrades/nw.yaml", PinnedVersion: "v0.18.0"}}
	_, err := Load(context.Background(), nil, comps)
	if err == nil {
		t.Fatal("Load = nil error, want rejection of the nil source")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "upgrades/nw.yaml") {
		t.Errorf("error %q does not name the file it could not read", err.Error())
	}
}

// A component with no record needs no source at all.
func TestLoadAllowsNilSourceWhenNothingReferencesAFile(t *testing.T) {
	set, err := Load(context.Background(), nil, []Component{{Name: "nw"}})
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if len(set) != 0 {
		t.Errorf("set = %v, want empty", set)
	}
}

// No other test asserts that a realistic record satisfies all nine rules at
// once, so nothing proved they are jointly satisfiable — and `replaces` was
// never exercised through a YAML decode at all. Every clause here is load
// bearing: rule 3 needs the two `from` domains to meet and to reach the pin,
// rule 7 needs each `from` to stop at its own `to` floor, rule 8 needs the two
// floors to differ, and rule 6 needs the explicit group plus the remainder to
// cover all five deployers.
func TestLoadAndValidateAcceptAWellFormedRecord(t *testing.T) {
	const body = `apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: widget-operator
transitions:
  - from: "<0.20.0"
    to: ">=0.20.0 <=0.20.0"
    verdict: manual
    summary: widget.example.invalid moves to gadget.example.invalid
    precondition: no Widget is mid-rollout
    reversible: true
    reversibleNotes: only until the legacy CRD is removed in 0.22.0
    stepsByDeployer:
      - deployers: [argocd, argocd-helm, flux]
        steps:
          - id: rewrite-crs
            description: rewrite apiVersion and kind in one commit
            reason: splitting it lets auto-sync recreate what you deleted
      - steps:
          - id: rewrite-crs
            description: rewrite apiVersion and kind, then apply
    hooks:
      - file: manifests/migrations/adopt-gadgets.yaml
        phase: pre-upgrade
    affectedResources:
      - group: widget.example.invalid
        kinds: [Widget, DeploymentPolicy]
    references:
      - https://example.invalid/migration
  - from: ">=0.20.0 <0.22.0"
    to: ">=0.22.0 <=0.22.0"
    verdict: safe
    verifiedBy: uat lane eks-h100-training
    summary: the legacy CRD is removed with no operator action
replaces:
  component: old-widget-operator
  verdict: manual
  summary: supersedes old-widget-operator, whose registry row is gone
  stepsByDeployer:
    - steps:
        - id: uninstall-old
          description: uninstall old-widget-operator before installing this one
`
	src := mapSource{"upgrades/widget-operator.yaml": []byte(body)}
	comps := []Component{{
		Name:          "widget-operator",
		File:          "upgrades/widget-operator.yaml",
		PinnedVersion: "v0.22.0",
	}}

	set, err := Load(context.Background(), src, comps)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if err := set.Validate(comps); err != nil {
		t.Fatalf("Validate error = %v, want nil for a well-formed record", err)
	}
	u := set["widget-operator"]
	if len(u.Transitions) != 2 {
		t.Fatalf("transitions = %d, want 2", len(u.Transitions))
	}
	if u.Replaces == nil || u.Replaces.Component != "old-widget-operator" {
		t.Fatalf("replaces = %+v, want the superseded component decoded", u.Replaces)
	}
	if len(u.Transitions[0].Hooks) != 1 {
		t.Errorf("hooks = %v, want one decoded hook", u.Transitions[0].Hooks)
	}
}

func TestLoadHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := mapSource{"upgrades/nw.yaml": []byte(validRecord("nw"))}
	comps := []Component{{Name: "nw", File: "upgrades/nw.yaml", PinnedVersion: "v0.18.0"}}

	if _, err := Load(ctx, src, comps); err == nil {
		t.Fatal("Load = nil error on a canceled context, want failure")
	}
}

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
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// Every record below is synthetic, for the same reason as syntheticSet.
func bundleSet() Set {
	return Set{
		"gamma-operator": {
			Component: "gamma-operator",
			Transitions: []Transition{
				{From: "<2.0.0", To: ">=2.0.0 <=2.1.0", Verdict: VerdictManual, Summary: "rename"},
				{From: ">=2.0.0 <2.1.0", To: "=2.1.0", Verdict: VerdictSafe, VerifiedBy: "synthetic", Summary: "patch"},
			},
		},
		"delta-operator": {
			Component: "delta-operator",
			Transitions: []Transition{
				{From: "<3.0.0", To: ">=3.0.0", Verdict: VerdictBlocked, Summary: "stop"},
			},
		},
	}
}

func TestBundleNotes(t *testing.T) {
	tests := []struct {
		name string
		pins []BundlePin
		want []string
	}{
		{
			"manual to contains pin, safe to containing the same pin skipped",
			[]BundlePin{{"gamma-operator", "v2.1.0"}},
			[]string{"gamma-operator:>=2.0.0 <=2.1.0"},
		},
		{"pin above every to", []BundlePin{{"gamma-operator", "2.2.0"}}, nil},
		{"pin below every to", []BundlePin{{"gamma-operator", "1.9.0"}}, nil},
		{"pin is not semver", []BundlePin{{"gamma-operator", "main"}}, nil},
		{"component has no record", []BundlePin{{"epsilon-operator", "1.0.0"}}, nil},
		{"blocked to contains pin", []BundlePin{{"delta-operator", "3.4.0"}}, []string{"delta-operator:>=3.0.0"}},
		{
			"pin order preserved",
			[]BundlePin{{"delta-operator", "3.0.0"}, {"gamma-operator", "2.0.0"}},
			[]string{"delta-operator:>=3.0.0", "gamma-operator:>=2.0.0 <=2.1.0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notes := BundleNotes(bundleSet(), tt.pins)
			var got []string
			for _, n := range notes {
				got = append(got, n.Component+":"+n.Transition.To)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BundleNotes() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBundleNotesPointIntoSet(t *testing.T) {
	set := bundleSet()
	notes := BundleNotes(set, []BundlePin{{"gamma-operator", "2.1.0"}})
	if len(notes) != 1 {
		t.Fatalf("got %d notes, want 1", len(notes))
	}
	if notes[0].Transition != &set["gamma-operator"].Transitions[0] {
		t.Error("BundleNote.Transition does not point into the Set it was selected from")
	}
	if notes[0].Pin != "2.1.0" {
		t.Errorf("Pin = %q, want 2.1.0", notes[0].Pin)
	}
}

// guideSet exercises every block the guide renders: a manual record with a
// GitOps group and a remainder group, a precondition, reversibility and
// references. The blocked record and the second manual one cover helm alone,
// which Validate would reject, so that every other deployer reaches the
// no-steps fallback in both its wordings: with references and without.
func guideSet() Set {
	irreversible := false
	return Set{
		"gamma-operator": {
			Component: "gamma-operator",
			Transitions: []Transition{{
				From:    "<2.0.0",
				To:      ">=2.0.0 <=2.1.0",
				Verdict: VerdictManual,
				// Folded the way a YAML block scalar arrives, so the golden
				// shows the break collapsed rather than splitting the paragraph.
				Summary:         "The legacy API group is renamed.\nA mirror controller copies existing objects.",
				Precondition:    "No object is mid-rollout.",
				Reversible:      &irreversible,
				ReversibleNotes: "Objects under the new group are not read by 1.x controllers.",
				StepsByDeployer: []StepGroup{
					{
						Deployers: []string{"argocd", "argocd-helm", "flux"},
						Steps: []Step{
							{
								ID:          "upgrade-operator",
								Description: "Bump the operator to the pinned version.",
								Reason:      "the mirror controller ships with it.",
							},
							{
								ID:          "rename-in-one-commit",
								Description: "In a single commit, remove the legacy manifests and add their replacements.",
								Reason:      "one commit lets the controller prune and adopt in a single sync.",
							},
						},
					},
					{
						Steps: []Step{
							{ID: "upgrade-operator", Description: "Upgrade the operator release."},
							{ID: "rename", Description: "Rewrite apiVersion and kind, then apply."},
							{
								ID:          "delete-legacy",
								Description: "Delete the legacy objects once their replacements are reconciling.",
								Reason:      "the mirror controller does not prune them.",
							},
						},
					},
				},
				References: []string{"https://example.com/gamma/2.0-migration"},
			}},
		},
		"delta-operator": {
			Component: "delta-operator",
			Transitions: []Transition{{
				From:    "<3.0.0",
				To:      ">=3.0.0",
				Verdict: VerdictBlocked,
				Summary: "3.0.0 drops in-place conversion; the store has to be exported first.",
				StepsByDeployer: []StepGroup{{
					Deployers: []string{"helm"},
					Steps: []Step{{
						ID:          "export-store",
						Description: "Export the store with the 2.x tooling.",
					}},
				}},
			}},
		},
		"epsilon-operator": {
			Component: "epsilon-operator",
			Transitions: []Transition{{
				From:    "<0.5.0",
				To:      ">=0.5.0 <0.6.0",
				Verdict: VerdictManual,
				Summary: "The webhook certificate moves to a new Secret.",
				StepsByDeployer: []StepGroup{{
					Deployers: []string{"helm"},
					Steps: []Step{{
						ID:          "delete-old-secret",
						Description: "Delete the old webhook Secret after the upgrade.",
					}},
				}},
				References: []string{"https://example.com/epsilon/0.5-notes"},
			}},
		},
	}
}

func guideNotes(t *testing.T) []BundleNote {
	t.Helper()
	notes := BundleNotes(guideSet(), []BundlePin{
		{"gamma-operator", "v2.1.0"},
		{"delta-operator", "v3.0.0"},
		{"epsilon-operator", "v0.5.2"},
	})
	if len(notes) != 3 {
		t.Fatalf("guideSet selected %d notes, want 3", len(notes))
	}
	return notes
}

func TestWriteGuideGolden(t *testing.T) {
	for _, d := range []string{"helm", "argocd", "argocd-helm", "flux", "helmfile"} {
		t.Run(d, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteGuide(&buf, guideNotes(t), d); err != nil {
				t.Fatalf("WriteGuide: %v", err)
			}
			compareGolden(t, "upgrading-"+d+".md.golden", buf.Bytes())
		})
	}
}

func TestWriteGuideDeployerFiltering(t *testing.T) {
	tests := []struct {
		deployer string
		want     string
		notWant  string
	}{
		{"argocd", "rename-in-one-commit", "delete-legacy"},
		{"helm", "delete-legacy", "rename-in-one-commit"},
	}
	for _, tt := range tests {
		t.Run(tt.deployer, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteGuide(&buf, guideNotes(t), tt.deployer); err != nil {
				t.Fatalf("WriteGuide: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, tt.want) {
				t.Errorf("%s guide lacks step %q:\n%s", tt.deployer, tt.want, out)
			}
			if strings.Contains(out, tt.notWant) {
				t.Errorf("%s guide renders another deployer's step %q:\n%s", tt.deployer, tt.notWant, out)
			}
		})
	}
}

func TestWriteGuideReversible(t *testing.T) {
	no, yes := false, true
	tests := []struct {
		name       string
		reversible *bool
		notes      string
		want       string
	}{
		{"false with notes", &no, "cannot be rolled back", "\n**Reversible:** no. cannot be rolled back\n"},
		{"true with notes", &yes, "downgrade the chart", "\n**Reversible:** yes. downgrade the chart\n"},
		{"false without notes", &no, "", "\n**Reversible:** no.\n"},
		{"nil", nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &Transition{
				From: "<1.0.0", To: ">=1.0.0", Verdict: VerdictManual, Summary: "s",
				Reversible: tt.reversible, ReversibleNotes: tt.notes,
			}
			var buf bytes.Buffer
			notes := []BundleNote{{Component: "c", Pin: "1.0.0", Transition: tr}}
			if err := WriteGuide(&buf, notes, "helm"); err != nil {
				t.Fatalf("WriteGuide: %v", err)
			}
			out := buf.String()
			if tt.want == "" {
				if strings.Contains(out, "**Reversible:**") {
					t.Errorf("unset reversibility rendered:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("guide lacks %q:\n%s", tt.want, out)
			}
		})
	}
}

func TestWriteNoticeGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNotice(&buf, guideNotes(t), "argocd"); err != nil {
		t.Fatalf("WriteNotice: %v", err)
	}
	compareGolden(t, "upgrade-notice.golden", buf.Bytes())
}

func TestBundleRenderersEmpty(t *testing.T) {
	var guide, notice bytes.Buffer
	if err := WriteGuide(&guide, nil, "helm"); err != nil {
		t.Fatalf("WriteGuide: %v", err)
	}
	if err := WriteNotice(&notice, nil, "helm"); err != nil {
		t.Fatalf("WriteNotice: %v", err)
	}
	if guide.Len() != 0 {
		t.Errorf("guide for no notes = %q, want empty", guide.String())
	}
	if notice.Len() != 0 {
		t.Errorf("notice for no notes = %q, want empty", notice.String())
	}
	if lines := NoteLines(nil); lines != nil {
		t.Errorf("NoteLines(nil) = %q, want nil", lines)
	}
}

func TestNoteLines(t *testing.T) {
	got := NoteLines(guideNotes(t))
	want := []string{
		"gamma-operator requires manual steps when upgrading from <2.0.0; " +
			"read UPGRADING.md before applying this bundle over an existing installation",
		"delta-operator cannot be upgraded from <3.0.0 in one step; " +
			"read UPGRADING.md before applying this bundle over an existing installation",
		"epsilon-operator requires manual steps when upgrading from <0.5.0; " +
			"read UPGRADING.md before applying this bundle over an existing installation",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NoteLines() =\n%q\nwant\n%q", got, want)
	}
}

// Record prose is artifact-derived: a raw newline would end a list item or a
// table row early, and a raw ESC would reach whoever cats the file.
func TestBundleRenderersSanitize(t *testing.T) {
	const (
		hostile   = "a\nb\x1b[31m"
		collapsed = `a b\x1b[31m`
	)
	tr := &Transition{
		From: "<1.0.0", To: ">=1.0.0", Verdict: VerdictManual,
		Summary:      hostile,
		Precondition: hostile,
		StepsByDeployer: []StepGroup{{Steps: []Step{
			{ID: hostile, Description: hostile, Reason: hostile},
		}}},
	}
	notes := []BundleNote{{Component: hostile, Pin: "1.0.0", Transition: tr}}

	var guide, notice bytes.Buffer
	if err := WriteGuide(&guide, notes, "helm"); err != nil {
		t.Fatalf("WriteGuide: %v", err)
	}
	if err := WriteNotice(&notice, notes, "helm"); err != nil {
		t.Fatalf("WriteNotice: %v", err)
	}
	outputs := map[string]string{"guide": guide.String(), "notice": notice.String()}
	for i, l := range NoteLines(notes) {
		outputs[fmt.Sprintf("line %d", i)] = l
	}
	for name, out := range outputs {
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("%s carries a raw ESC byte: %q", name, out)
		}
		if strings.Contains(out, "a\nb") {
			t.Errorf("%s split record prose across lines: %q", name, out)
		}
		if !strings.Contains(out, collapsed) {
			t.Errorf("%s does not render the collapsed, escaped text %q: %q", name, collapsed, out)
		}
	}
}

func TestBundleRenderersRejectMalformedCalls(t *testing.T) {
	notes := BundleNotes(bundleSet(), []BundlePin{{"gamma-operator", "2.1.0"}})
	renderers := []struct {
		name   string
		render func(io.Writer, []BundleNote, string) error
	}{
		{"guide", WriteGuide},
		{"notice", WriteNotice},
	}
	tests := []struct {
		name     string
		writer   io.Writer
		notes    []BundleNote
		deployer string
		wantText string
	}{
		{"nil writer", nil, notes, "helm", "writer"},
		{"nil writer, no notes", nil, nil, "helm", "writer"},
		{"no deployer", &bytes.Buffer{}, notes, "", `""`},
		{"no deployer, no notes", &bytes.Buffer{}, nil, "", `""`},
		{"unknown deployer", &bytes.Buffer{}, notes, "kustomize", `"kustomize"`},
	}
	for _, r := range renderers {
		for _, tt := range tests {
			t.Run(r.name+"/"+tt.name, func(t *testing.T) {
				err := r.render(tt.writer, tt.notes, tt.deployer)
				if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
					t.Fatalf("error = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantText) {
					t.Errorf("error = %v, want it to name %s", err, tt.wantText)
				}
			})
		}
	}
}

func TestBundleRenderersPropagateWriteFailure(t *testing.T) {
	notes := guideNotes(t)
	if err := WriteGuide(failingWriter{}, notes, "helm"); err == nil {
		t.Error("WriteGuide on a failing writer returned nil")
	}
	if err := WriteNotice(failingWriter{}, notes, "helm"); err == nil {
		t.Error("WriteNotice on a failing writer returned nil")
	}
}

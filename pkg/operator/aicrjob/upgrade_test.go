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

package aicrjob

import (
	"encoding/json"
	"maps"
	"testing"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// The v0.22.0 -> v1.0.0 fixture records a safe and a blocked verdict.
func TestParseUpgradeReportFixture(t *testing.T) {
	t.Parallel()
	r, err := ParseUpgradeReport(fixture(t, "upgrade-check-v0.22.0-to-v1.0.0-eks.json"), "argocd")
	if err != nil {
		t.Fatalf("ParseUpgradeReport() = %v", err)
	}
	if r.ObjectNamesCompared {
		t.Error("recipe-to-recipe reports do not compare object names")
	}
	got := map[string]v1alpha1.Assessment{}
	for i := range r.Components {
		got[r.Components[i].Component] = r.Components[i].Assessment()
	}
	if got["gpu-operator"] != v1alpha1.AssessmentSafe || got["nvsentinel"] != v1alpha1.AssessmentBlocked {
		t.Fatalf("assessments = %v", got)
	}
	if r.Summary.Failing != 1 {
		t.Errorf("failing = %d, want 1", r.Summary.Failing)
	}

	added, err := ParseUpgradeReport(fixture(t, "upgrade-check-eks-training-to-kubeflow.json"), "argocd")
	if err != nil {
		t.Fatalf("ParseUpgradeReport(added) = %v", err)
	}
	if c := added.Components[0]; c.Change != "added" || c.Assessment() != v1alpha1.AssessmentNotApplicable {
		t.Fatalf("added component = %+v", c)
	}
}

// The v1.0.0 -> v1.1.2 EKS lane transition from AICR v1.1.2 records two
// safe verdicts and a manual one, which needs approval.
func TestParseUpgradeReportV112(t *testing.T) {
	t.Parallel()
	r, err := ParseUpgradeReport(fixtureAt(t, "v1.1.2", "upgrade-check-v1.0.0-to-v1.1.2-eks.json"), "argocd")
	if err != nil {
		t.Fatalf("ParseUpgradeReport() = %v", err)
	}
	got := map[string]v1alpha1.Assessment{}
	for i := range r.Components {
		got[r.Components[i].Component] = r.Components[i].Assessment()
	}
	want := map[string]v1alpha1.Assessment{
		"gpu-operator": v1alpha1.AssessmentSafe, "nodewright-operator": v1alpha1.AssessmentSafe, "nvsentinel": v1alpha1.AssessmentManual,
	}
	if !maps.Equal(got, want) || r.Summary.Failing != 1 {
		t.Fatalf("assessments = %v, failing = %d; want %v, 1", got, r.Summary.Failing, want)
	}
	// The manual record carries what approving takes on: a precondition and
	// the steps beyond the apply.
	for _, c := range r.Components {
		if c.Component == "nvsentinel" && (c.Precondition == "" || len(c.Steps) != 3 || c.Steps[0].ID != "apply" || c.Steps[2].Description == "") {
			t.Fatalf("nvsentinel record = %+v", c)
		}
	}
	added, err := ParseUpgradeReport(fixtureAt(t, "v1.1.2", "upgrade-check-eks-training-to-kubeflow.json"), "argocd")
	if err != nil {
		t.Fatalf("ParseUpgradeReport(added) = %v", err)
	}
	if c := added.Components[0]; c.Change != "added" || c.Assessment() != v1alpha1.AssessmentNotApplicable {
		t.Fatalf("added component = %+v", c)
	}
}

func TestParseUpgradeReportRejects(t *testing.T) {
	t.Parallel()
	base := fixture(t, "upgrade-check-v0.22.0-to-v1.0.0-eks.json")
	edit := func(f func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	tests := []struct {
		name     string
		data     []byte
		deployer string
	}{
		{"other deployer", base, "flux"},
		{"missing components", edit(func(m map[string]any) { delete(m, "components") }), "argocd"},
		{"missing summary", edit(func(m map[string]any) { delete(m, "summary") }), "argocd"},
		{"summary mismatch", edit(func(m map[string]any) { m["summary"].(map[string]any)["components"] = 5 }), "argocd"},
		{"unnamed component", edit(func(m map[string]any) { m["components"].([]any)[0].(map[string]any)["component"] = "" }), "argocd"},
		{"not JSON", []byte("components: []"), "argocd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseUpgradeReport(tt.data, tt.deployer); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("ParseUpgradeReport() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

// Unknown is not safe: an unrecognized future verdict requires approval.
func TestAssessmentMapping(t *testing.T) {
	t.Parallel()
	tests := map[string]v1alpha1.Assessment{
		"":            v1alpha1.AssessmentNotApplicable,
		"safe":        v1alpha1.AssessmentSafe,
		"manual":      v1alpha1.AssessmentManual,
		"blocked":     v1alpha1.AssessmentBlocked,
		"unknown":     v1alpha1.AssessmentUnknown,
		"unversioned": v1alpha1.AssessmentUnversioned,
		"probably":    v1alpha1.AssessmentUnknown,
	}
	for verdict, want := range tests {
		c := ComponentChange{Verdict: verdict}
		if got := c.Assessment(); got != want {
			t.Errorf("verdict %q -> %s, want %s", verdict, got, want)
		}
	}
	// A version change AICR did not assess is not safe; an added or removed
	// component's approval comes from its step operation.
	for change, want := range map[string]v1alpha1.Assessment{
		"version": v1alpha1.AssessmentUnknown,
		"added":   v1alpha1.AssessmentNotApplicable,
		"removed": v1alpha1.AssessmentNotApplicable,
	} {
		c := ComponentChange{Change: change}
		if got := c.Assessment(); got != want {
			t.Errorf("a %s change without a verdict -> %s, want %s", change, got, want)
		}
	}
}

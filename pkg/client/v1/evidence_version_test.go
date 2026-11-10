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

package aicr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestRenderCNCFEvidenceWithOptions(t *testing.T) {
	t.Parallel()

	report := &ctrf.Report{}
	report.Results.Tests = []ctrf.TestResult{
		{Name: "dra-support", Status: ctrf.StatusPassed, Duration: 1000},
		{Name: "gang-scheduling", Status: ctrf.StatusPassed, Duration: 1000},
	}

	tests := []struct {
		name          string
		opts          RenderCNCFEvidenceOptions
		wantChecklist string
		wantDRA       string // how index.md lists dra_support
	}{
		{"zero options render the latest", RenderCNCFEvidenceOptions{}, "**Checklist:** " + string(cncf.LatestVersion), "## Supporting Evidence"},
		{"explicit v1.35 keeps dra_support a requirement", RenderCNCFEvidenceOptions{ConformanceVersion: "v1.35"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"explicit version accepts the bare form", RenderCNCFEvidenceOptions{ConformanceVersion: "1.36"}, "**Checklist:** v1.36", "## Supporting Evidence"},
		{"kubernetes version selects its minor", RenderCNCFEvidenceOptions{KubernetesVersion: "1.35.4-eks-abc"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"explicit version wins over kubernetes version", RenderCNCFEvidenceOptions{ConformanceVersion: "v1.37", KubernetesVersion: "v1.35.0"}, "**Checklist:** v1.37", "## Supporting Evidence"},
		{"kubernetes minor newer than any carried checklist renders the latest", RenderCNCFEvidenceOptions{KubernetesVersion: "v1.40.0"}, "**Checklist:** " + string(cncf.LatestVersion), "## Supporting Evidence"},
		{"kubernetes minor older than any carried checklist renders the oldest, not the latest", RenderCNCFEvidenceOptions{KubernetesVersion: "v1.34.9"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"kubernetes minor at the recipe floor renders v1.35", RenderCNCFEvidenceOptions{KubernetesVersion: "v1.32.4"}, "**Checklist:** v1.35", "| `dra_support` | MUST |"},
		{"unparseable kubernetes version falls back to the latest", RenderCNCFEvidenceOptions{KubernetesVersion: "unknown"}, "**Checklist:** " + string(cncf.LatestVersion), "## Supporting Evidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := (&Client{}).RenderCNCFEvidenceWithOptions(context.Background(), report, dir, tt.opts); err != nil {
				t.Fatalf("RenderCNCFEvidenceWithOptions: %v", err)
			}
			index, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{tt.wantChecklist, tt.wantDRA} {
				if !strings.Contains(string(index), want) {
					t.Errorf("index.md missing %q:\n%s", want, index)
				}
			}
		})
	}

	t.Run("unsupported explicit version is refused before writing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		err := (&Client{}).RenderCNCFEvidenceWithOptions(context.Background(), report, dir,
			RenderCNCFEvidenceOptions{ConformanceVersion: "v1.38"})
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("evidence dir not left empty: %v", entries)
		}
	})
}

func TestSnapshotKubernetesVersion(t *testing.T) {
	t.Parallel()

	withServer := func(version string) *Snapshot {
		m := measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").
				Set(measurement.KeyVersion, measurement.Str(version))).
			Build()
		return WrapSnapshot(&snapshotter.Snapshot{Measurements: []*measurement.Measurement{m}})
	}

	tests := []struct {
		name string
		snap *Snapshot
		want string
	}{
		{"nil receiver", nil, ""},
		{"no measurements", &Snapshot{}, ""},
		{"server version with v prefix", withServer("v1.36.3"), "1.36.3"},
		{"server version with build metadata", withServer("v1.35.2-eks-1f2a3b"), "1.35.2-eks-1f2a3b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.snap.KubernetesVersion(); got != tt.want {
				t.Errorf("KubernetesVersion() = %q, want %q", got, tt.want)
			}
		})
	}

	// The facade value feeds the renderer option end to end.
	if v, ok := cncf.VersionForKubernetes(withServer("v1.36.3").KubernetesVersion()); !ok || v != cncf.Version136 {
		t.Errorf("VersionForKubernetes(KubernetesVersion()) = (%q, %v)", v, ok)
	}
}

// RenderCNCFEvidenceWithResult returns the aggregate the pages were rendered
// from, carries NotApplicable into it, and refuses an id that may not be
// declared not applicable before writing anything.
func TestRenderCNCFEvidenceWithResult(t *testing.T) {
	t.Parallel()

	report := &ctrf.Report{}
	report.Results.Tests = []ctrf.TestResult{
		{Name: "gang-scheduling", Status: ctrf.StatusPassed, Duration: 1000},
		{Name: "cluster-autoscaling", Status: ctrf.StatusFailed, Message: "no scale event observed"},
		{Name: "pod-autoscaling", Status: ctrf.StatusSkipped, Extra: map[string]string{"skipReason": "no-metrics-api"}},
	}

	t.Run("aggregate reflects the run and the declaration", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		agg, err := (&Client{}).RenderCNCFEvidenceWithResult(context.Background(), report, dir, RenderCNCFEvidenceOptions{
			ConformanceVersion: "v1.37",
			NotApplicable:      map[string]string{"cluster_autoscaling": "fixed-size cluster"},
		})
		if err != nil {
			t.Fatalf("RenderCNCFEvidenceWithResult: %v", err)
		}
		if agg == nil || agg.Version != cncf.Version137 {
			t.Fatalf("aggregate = %+v, want v1.37", agg)
		}
		if agg.Passed() {
			t.Error("Passed() = true with MUSTs skipped and not run")
		}
		want := map[string]cncf.RequirementStatus{
			"gang_scheduling":     cncf.StatusPassed,
			"cluster_autoscaling": cncf.StatusNotApplicable,
			"pod_autoscaling":     cncf.StatusNotTested,
			"robust_controller":   cncf.StatusNotTested,
			"workload_sandboxing": cncf.StatusNotTested,
		}
		for id, status := range want {
			if r := agg.Requirement(id); r == nil || r.Status != status {
				t.Errorf("%s = %+v, want %q", id, r, status)
			}
		}
		for _, r := range agg.IncompleteMusts() {
			if r.ID == "cluster_autoscaling" || r.Level != cncf.LevelMust {
				t.Errorf("IncompleteMusts lists %s (%s)", r.ID, r.Level)
			}
		}
		index, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"**Applicable MUSTs:** FAILED",
			"NOT APPLICABLE (fixed-size cluster)",
			"NOT TESTED (pod-autoscaling skipped: no-metrics-api)",
		} {
			if !strings.Contains(string(index), want) {
				t.Errorf("index.md missing %q:\n%s", want, index)
			}
		}
	})

	t.Run("a non-conditional id is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		agg, err := (&Client{}).RenderCNCFEvidenceWithResult(context.Background(), report, dir, RenderCNCFEvidenceOptions{
			NotApplicable: map[string]string{"gang_scheduling": "we do not gang"},
		})
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		if agg != nil {
			t.Errorf("aggregate = %+v, want nil on error", agg)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("evidence dir not left empty: %v", entries)
		}
	})

	t.Run("ValidateCNCFEvidenceOptions refuses the same inputs without rendering", func(t *testing.T) {
		t.Parallel()
		c := &Client{}
		if err := c.ValidateCNCFEvidenceOptions(RenderCNCFEvidenceOptions{
			KubernetesVersion: "v1.37.2",
			NotApplicable:     map[string]string{"cluster_autoscaling": "fixed-size cluster"},
		}); err != nil {
			t.Errorf("valid options refused: %v", err)
		}
		for name, opts := range map[string]RenderCNCFEvidenceOptions{
			"non-conditional id":  {NotApplicable: map[string]string{"gang_scheduling": "we do not gang"}},
			"blank justification": {NotApplicable: map[string]string{"cluster_autoscaling": " "}},
			"unsupported version": {ConformanceVersion: "v1.38"},
		} {
			t.Run(name, func(t *testing.T) {
				errorstest.WantReportedCode(t, c.ValidateCNCFEvidenceOptions(opts), aicrerrors.ErrCodeInvalidRequest)
			})
		}
		var nilClient *Client
		errorstest.WantReportedCode(t, nilClient.ValidateCNCFEvidenceOptions(RenderCNCFEvidenceOptions{}), aicrerrors.ErrCodeInvalidRequest)
	})

	t.Run("WithOptions still refuses the same way", func(t *testing.T) {
		t.Parallel()
		err := (&Client{}).RenderCNCFEvidenceWithOptions(context.Background(), report, t.TempDir(), RenderCNCFEvidenceOptions{
			NotApplicable: map[string]string{"workload_sandboxing": "no sandbox runtime"},
		})
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
	})
}

func coverageRecipe(checks ...string) *RecipeResult {
	return &RecipeResult{Name: "r", internal: &recipe.RecipeResult{
		Validation: &recipe.ValidationConfig{Conformance: &recipe.ValidationPhase{Checks: checks}},
	}}
}

// TestValidateCNCFRecipeCoverage pins the #3703 pre-flight at the facade: a
// recipe that declares no complete proof path for an applicable MUST is
// refused for the checklist the options select, before any validator runs.
func TestValidateCNCFRecipeCoverage(t *testing.T) {
	t.Parallel()
	complete := []string{"gang-scheduling", "accelerator-metrics", "ai-service-metrics", "inference-gateway",
		"pod-autoscaling", "cluster-autoscaling", "robust-controller", "secure-accelerator-access"}
	slinky := append([]string{"slinky-slurm-gpu-access"}, complete[:len(complete)-1]...)
	missingGang := complete[1:]

	tests := []struct {
		name    string
		rec     *RecipeResult
		opts    RenderCNCFEvidenceOptions
		wantErr []string // substrings of an ErrCodeInvalidRequest; nil means accepted
	}{
		{"complete recipe at the cluster's minor", coverageRecipe(complete...), RenderCNCFEvidenceOptions{KubernetesVersion: "v1.37.2"}, nil},
		{"Slinky alternative path", coverageRecipe(slinky...), RenderCNCFEvidenceOptions{KubernetesVersion: "v1.37.2"}, nil},
		{"missing gang-scheduling", coverageRecipe(missingGang...), RenderCNCFEvidenceOptions{KubernetesVersion: "v1.37.2"},
			[]string{"gang_scheduling: missing gang-scheduling", "v1.37"}},
		{"dra-support required by the v1.35 checklist the cluster selects", coverageRecipe(complete...), RenderCNCFEvidenceOptions{KubernetesVersion: "v1.35.0"},
			[]string{"dra_support: missing dra-support", "v1.35"}},
		{"notApplicable lifts a conditional MUST", coverageRecipe(complete[:5]...),
			RenderCNCFEvidenceOptions{KubernetesVersion: "v1.37.2", NotApplicable: map[string]string{"cluster_autoscaling": "fixed-size"}},
			[]string{"robust_controller: missing robust-controller", "secure_accelerator_access"}},
		{"a recipe with no conformance phase declares nothing", &RecipeResult{Name: "r", internal: &recipe.RecipeResult{}},
			RenderCNCFEvidenceOptions{ConformanceVersion: "v1.36"}, []string{"gang_scheduling", "ai_inference"}},
		{"no internal recipe state", &RecipeResult{Name: "r"}, RenderCNCFEvidenceOptions{}, []string{"recipe is required"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := (&Client{}).ValidateCNCFRecipeCoverage(tt.rec, tt.opts)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateCNCFRecipeCoverage() = %v, want nil", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			for _, want := range tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want substring %q", err, want)
				}
			}
		})
	}
	t.Run("nil client", func(t *testing.T) {
		t.Parallel()
		var c *Client
		errorstest.WantReportedCode(t, c.ValidateCNCFRecipeCoverage(coverageRecipe(complete...), RenderCNCFEvidenceOptions{}), aicrerrors.ErrCodeInvalidRequest)
	})
}

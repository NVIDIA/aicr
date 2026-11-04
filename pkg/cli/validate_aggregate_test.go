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

package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// A bad spec.validate.evidence.cncf.notApplicable key is refused before the
// recipe is loaded or any cluster is contacted, so the run fails in
// milliseconds rather than after every validator Job.
func TestValidateCmd_NotApplicablePreflight(t *testing.T) {
	tests := []struct {
		name          string
		notApplicable string
		wantErr       bool
		wantSubstr    string
	}{
		{
			name:          "non-conditional requirement is refused",
			notApplicable: "gang_scheduling: we do not gang",
			wantErr:       true,
			wantSubstr:    `notApplicable: "gang_scheduling" is not a conditional MUST`,
		},
		{
			name:          "unknown requirement is refused",
			notApplicable: "dra_support: retired",
			wantErr:       true,
			wantSubstr:    `notApplicable: "dra_support" is not a requirement`,
		},
		{
			name:          "blank justification is refused",
			notApplicable: `cluster_autoscaling: "  "`,
			wantErr:       true,
			wantSubstr:    "needs a justification",
		},
		{
			name:          "a valid declaration passes the pre-flight",
			notApplicable: "cluster_autoscaling: fixed-size cluster",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			cfgYAML := "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nmetadata:\n  name: test\nspec:\n  validate:\n" +
				"    evidence:\n      cncf:\n        notApplicable:\n          " + tt.notApplicable + "\n"
			if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			recipePath := filepath.Join(dir, "recipe.yaml")
			recipeYAML := "kind: RecipeMetadata\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: test\nspec:\n  criteria:\n" +
				"    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n"
			if err := os.WriteFile(recipePath, []byte(recipeYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			// The snapshot path does not exist and --no-cluster keeps the run
			// offline: a refusal must mention notApplicable and come before the
			// recipe is read; a pass gets past the recipe to the snapshot load,
			// which then fails on the missing file.
			missingSnapshot := filepath.Join(dir, "missing-snapshot.yaml")
			app := &cli.Command{Name: "aicr", Commands: []*cli.Command{validateCmd()}}
			err := app.Run(t.Context(), []string{"aicr", "validate", "--config", cfgPath,
				"--recipe", recipePath, "--snapshot", missingSnapshot, "--no-cluster",
				"--evidence-dir", filepath.Join(dir, "evidence")})
			if err == nil {
				t.Fatal("expected an error: the snapshot file does not exist")
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				if !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantSubstr)
				}
				return
			}
			if strings.Contains(err.Error(), "notApplicable") || !strings.Contains(err.Error(), "missing-snapshot.yaml") {
				t.Errorf("error = %v, want the missing snapshot, not a notApplicable refusal", err)
			}
		})
	}
}

// logRequirementAggregate reports the aggregate without deciding the exit
// status: every applicable MUST that did not pass is named with its reason,
// and SHOULDs and not-applicable MUSTs are not.
func TestLogRequirementAggregate(t *testing.T) {
	report := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "gang-scheduling", Status: ctrf.StatusFailed, Message: "pods not co-scheduled"},
		{Name: "pod-autoscaling", Status: ctrf.StatusSkipped, Extra: map[string]string{"skipReason": "no-metrics-api"}},
	}}}
	agg, err := (&aicr.Client{}).RenderCNCFEvidenceWithResult(t.Context(), report, t.TempDir(), aicr.RenderCNCFEvidenceOptions{
		ConformanceVersion: "v1.37",
		NotApplicable:      map[string]string{"cluster_autoscaling": "fixed-size cluster"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		agg    *aicr.CNCFRequirementAggregate
		want   []string
		absent []string
	}{
		{
			name:   "nil aggregate logs nothing",
			agg:    nil,
			absent: []string{"CNCF requirement aggregation"},
		},
		{
			name: "incomplete MUSTs are named with status and reason",
			agg:  agg,
			want: []string{
				"CNCF requirement aggregation", "checklist=v1.37", "applicableMustsPassed=false",
				"requirement=gang_scheduling", "status=failed", `reason="gang-scheduling failed"`,
				"requirement=pod_autoscaling", `status="not tested"`, "pod-autoscaling skipped: no-metrics-api",
				"requirement=robust_controller", `reason="not run"`,
			},
			absent: []string{"requirement=cluster_autoscaling", "requirement=workload_sandboxing", "requirement=gpu_sharing"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			logRequirementAggregate(tt.agg)

			out := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %q:\n%s", want, out)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(out, bad) {
					t.Errorf("log must not contain %q:\n%s", bad, out)
				}
			}
		})
	}
}

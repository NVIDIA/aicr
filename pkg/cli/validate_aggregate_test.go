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
	stderrors "errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// A bad spec.validate.evidence.cncf block (a notApplicable or documented
// key that is not allowed, a missing or malformed vendor field) is refused
// before the recipe is loaded or any cluster is contacted, so the run fails
// in milliseconds rather than after every validator Job.
func TestValidateCmd_NotApplicablePreflight(t *testing.T) {
	tests := []struct {
		name       string
		cncfYAML   string
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "non-conditional requirement is refused",
			cncfYAML:   cncfVendorYAML + "        notApplicable:\n          gang_scheduling: we do not gang\n",
			wantErr:    true,
			wantSubstr: `notApplicable: "gang_scheduling" is not a conditional MUST`,
		},
		{
			name:       "unknown requirement is refused",
			cncfYAML:   cncfVendorYAML + "        notApplicable:\n          dra_support: retired\n",
			wantErr:    true,
			wantSubstr: `notApplicable: "dra_support" is not a requirement`,
		},
		{
			name:       "blank justification is refused",
			cncfYAML:   cncfVendorYAML + "        notApplicable:\n          cluster_autoscaling: \"  \"\n",
			wantErr:    true,
			wantSubstr: "needs a justification",
		},
		{
			name:       "missing vendor is refused",
			cncfYAML:   "        notApplicable:\n          cluster_autoscaling: fixed-size cluster\n",
			wantErr:    true,
			wantSubstr: "requires spec.validate.evidence.cncf.vendor",
		},
		{
			name:       "malformed vendor URL is refused",
			cncfYAML:   strings.Replace(cncfVendorYAML, "https://vendor.example/logo.svg", "vendor.example/logo.svg", 1),
			wantErr:    true,
			wantSubstr: "vendor.productLogoUrl",
		},
		{
			name:       "documented without a URL is refused",
			cncfYAML:   cncfVendorYAML + "        documented:\n          cluster_autoscaling:\n            notes: scales\n",
			wantErr:    true,
			wantSubstr: "documented.cluster_autoscaling: at least one documentation URL",
		},
		{
			name:     "a valid declaration passes the pre-flight",
			cncfYAML: cncfVendorYAML + "        notApplicable:\n          cluster_autoscaling: fixed-size cluster\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := writeCNCFConfig(t, dir, tt.cncfYAML)
			recipePath := filepath.Join(dir, "recipe.yaml")
			recipeYAML := "kind: RecipeMetadata\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: test\nspec:\n  criteria:\n" +
				"    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n"
			if err := os.WriteFile(recipePath, []byte(recipeYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			// The snapshot path does not exist, and the file inputs keep the
			// run off the cluster up to its load: a refusal must mention
			// notApplicable and come before the recipe is read; a pass gets
			// past the recipe to the snapshot load, which then fails on the
			// missing file.
			missingSnapshot := filepath.Join(dir, "missing-snapshot.yaml")
			app := &cli.Command{Name: "aicr", Commands: []*cli.Command{validateCmd()}}
			err := app.Run(t.Context(), []string{"aicr", "validate", "--config", cfgPath,
				"--recipe", recipePath, "--snapshot", missingSnapshot, "--phase", "conformance",
				"--cncf-submission", "--evidence-dir", filepath.Join(dir, "evidence")})
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
			if strings.Contains(err.Error(), "evidence.cncf") || !strings.Contains(err.Error(), "missing-snapshot.yaml") {
				t.Errorf("error = %v, want the missing snapshot, not a config refusal", err)
			}
		})
	}
}

// logRequirementAggregate reports the aggregate without deciding the exit
// status (that is CNCFRequirementAggregate.Outcome): every applicable MUST
// that did not pass is named with its reason, SHOULDs and not-applicable
// MUSTs are not, and when no check produced a result one warning replaces
// the per-MUST lines.
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
	nothingRan, err := (&aicr.Client{}).RenderCNCFEvidenceWithResult(t.Context(), &ctrf.Report{}, t.TempDir(), aicr.RenderCNCFEvidenceOptions{
		ConformanceVersion: "v1.37",
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		agg        *aicr.CNCFRequirementAggregate
		want       []string
		absent     []string
		wantNoLogs bool
	}{
		{name: "nil aggregate logs nothing", agg: nil, wantNoLogs: true},
		{
			name: "incomplete MUSTs are named with status and reason",
			agg:  agg,
			want: []string{
				"CNCF requirement aggregation", "checklist=v1.37", "applicableMustsEstablished=false",
				"requirement=gang_scheduling", "status=failed", `reason="gang-scheduling failed"`,
				"requirement=pod_autoscaling", `status="not tested"`, "pod-autoscaling skipped: no-metrics-api",
				"requirement=robust_controller", `reason="not run"`,
			},
			absent: []string{"requirement=cluster_autoscaling", "requirement=workload_sandboxing", "requirement=gpu_sharing"},
		},
		{
			name:   "no check ran: one warning, not one per MUST",
			agg:    nothingRan,
			want:   []string{"no conformance check produced a result"},
			absent: []string{"CNCF MUST requirement not established"},
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
			if tt.wantNoLogs && out != "" {
				t.Errorf("expected no log output, got:\n%s", out)
			}
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

// runOutcome combines a per-check failure with the submission stage's error
// and keeps that error's classified code when it adds context: a render
// canceled by the operator stays CANCELED, a timed-out one stays TIMEOUT, the
// requirement verdict stays INTERNAL, and a plain error falls back to
// INTERNAL. Without a submission error the per-check rule alone decides.
func TestRunOutcome(t *testing.T) {
	verdict := errors.New(errors.ErrCodeInternal, "CNCF submission not established: 1 applicable MUST requirement(s) of the v1.37 checklist did not pass: gang_scheduling not tested (gang-scheduling not run)")
	tests := []struct {
		name         string
		checksFailed bool
		submission   error
		wantCode     errors.ErrorCode
		wantSubstrs  []string
	}{
		{name: "nothing failed"},
		{name: "checks failed, no submission error", checksFailed: true, wantCode: errors.ErrCodeInternal,
			wantSubstrs: []string{"one or more phases did not pass"}},
		{name: "verdict alone is returned as is", submission: verdict, wantCode: errors.ErrCodeInternal,
			wantSubstrs: []string{"gang_scheduling not tested"}},
		{name: "checks failed and verdict keeps INTERNAL", checksFailed: true, submission: verdict, wantCode: errors.ErrCodeInternal,
			wantSubstrs: []string{"one or more phases did not pass", "gang_scheduling not tested"}},
		{name: "canceled render with failed checks keeps CANCELED", checksFailed: true,
			submission: errors.New(errors.ErrCodeCanceled, "render canceled"), wantCode: errors.ErrCodeCanceled,
			wantSubstrs: []string{"one or more phases did not pass", "render canceled"}},
		{name: "timed-out render with failed checks keeps TIMEOUT", checksFailed: true,
			submission: errors.New(errors.ErrCodeTimeout, "render timed out"), wantCode: errors.ErrCodeTimeout,
			wantSubstrs: []string{"render timed out"}},
		{name: "plain error with failed checks falls back to INTERNAL", checksFailed: true,
			submission: stderrors.New("disk full"), wantCode: errors.ErrCodeInternal,
			wantSubstrs: []string{"disk full"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runOutcome(tt.checksFailed, tt.submission)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("runOutcome() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("runOutcome() = nil, want an error")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			for _, want := range tt.wantSubstrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not mention %q", err, want)
				}
			}
		})
	}
}

// validationExit lets Outcome decide a rendered submission run on its own:
// a failed check in such a run produces Outcome's single combined message,
// not that message wrapped again with the phase-level failure; a submission
// run whose render failed, and every non-submission run, go through
// runOutcome.
func TestValidationExit(t *testing.T) {
	outcome := errors.New(errors.ErrCodeInternal,
		"validation failed: one or more checks did not pass, and the CNCF submission is not established: gang_scheduling failed (gang-scheduling failed)")
	renderCanceled := errors.New(errors.ErrCodeCanceled, "render canceled")
	tests := []struct {
		name         string
		decided      bool
		outcome      error
		checksFailed bool
		renderErr    error
		wantErr      error
		wantCode     errors.ErrorCode
		wantAbsent   string
	}{
		{name: "rendered submission, all established", decided: true},
		{name: "rendered submission, failed check: Outcome's message once", decided: true, outcome: outcome, checksFailed: true,
			wantErr: outcome, wantAbsent: "phases did not pass"},
		{name: "render failed with failed checks keeps the render code", checksFailed: true, renderErr: renderCanceled,
			wantCode: errors.ErrCodeCanceled},
		{name: "plain run with failed checks", checksFailed: true, wantCode: errors.ErrCodeInternal},
		{name: "plain run passed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validationExit(tt.decided, tt.outcome, tt.checksFailed, tt.renderErr)
			if tt.wantErr != nil {
				if err != tt.wantErr { //nolint:errorlint // identity is the assertion: the outcome must not be re-wrapped
					t.Fatalf("validationExit() = %v, want Outcome's error returned unchanged", err)
				}
				if strings.Count(err.Error(), "validation failed") != 1 || strings.Contains(err.Error(), tt.wantAbsent) {
					t.Errorf("error %q is wrapped twice", err.Error())
				}
				return
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("validationExit() = %v, want nil", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

// A submission failure that kept its staged copy names that path and the
// removal rule; one that kept nothing says so.
func TestLogSubmissionFailure(t *testing.T) {
	kept := errors.WrapWithContext(errors.ErrCodeInternal, "submission not promoted", errors.New(errors.ErrCodeInternal, "rejected"),
		map[string]any{aicr.CNCFStagedDirContextKey: "/out/submission.incomplete/v1.37/submission"})
	if got := stagedSubmissionDir(kept); got != "/out/submission.incomplete/v1.37/submission" {
		t.Errorf("stagedSubmissionDir() = %q", got)
	}
	if got := stagedSubmissionDir(errors.New(errors.ErrCodeInternal, "write failed")); got != "" {
		t.Errorf("stagedSubmissionDir() = %q for an error without a staged copy", got)
	}
	logs := captureJSONLogs(t)
	logSubmissionFailure(kept, "/out/submission")
	logSubmissionFailure(errors.New(errors.ErrCodeInternal, "write failed"), "/out/submission")
	out := logs.String()
	for _, want := range []string{"staged copy is kept for inspection and must be removed before the next run", "/out/submission.incomplete/v1.37/submission", "nothing was kept"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
}

// TestRunValidationSubmissionVerdict drives runValidation as a submission
// run offline through CNCFRequirementAggregate.Outcome. --no-cluster is
// refused above runValidation, so the internal call is the one route without
// a cluster: every check is skipped, so every MUST reads not tested, the
// submission is staged but never reaches the requested path, and the run
// fails naming a MUST, while the same run without the flag passes.
func TestRunValidationSubmissionVerdict(t *testing.T) {
	client, rec := phaseLogRecipe(t)
	withEveryMustCheck(t, rec)
	snap := phaseLogSnapshot()

	for _, submission := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain run passes", true: "submission run fails on unestablished MUSTs"}[submission], func(t *testing.T) {
			dir := t.TempDir()
			evidenceDir := filepath.Join(dir, "evidence")
			cfg := validationConfig{
				phases:              []validator.Phase{validator.PhaseConformance},
				runID:               "20260908-000000-0123456789abcdef",
				output:              filepath.Join(dir, "out.yaml"),
				outFormat:           serializer.FormatYAML,
				validationNamespace: "aicr-validation-test",
				cleanup:             true,
				noCluster:           true,
				failOnError:         true,
			}
			if submission {
				cfg.cncfSubmission = true
				cfg.evidenceDir = evidenceDir
				cfg.cncfVendor = testCNCFVendor()
				// The fixture snapshot runs Kubernetes 1.34, which no carried
				// checklist certifies; the explicit version is the override.
				cfg.cncfConformanceVersion = "v1.37"
			}
			logs := captureJSONLogs(t)
			err := runValidation(t.Context(), client, rec, snap, cfg)
			if !submission {
				if err != nil {
					t.Fatalf("runValidation: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("submission run passed with every check skipped")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			for _, want := range []string{"CNCF submission not established", "gang_scheduling not tested"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not mention %q", err, want)
				}
			}
			if _, statErr := os.Stat(evidenceDir); statErr == nil {
				t.Errorf("%s exists; an incomplete submission must not reach the requested path", evidenceDir)
			}
			// The checklist version follows the snapshot, so the staged
			// product directory is <dir>.incomplete/vX.Y/evidence/.
			if staged, _ := filepath.Glob(filepath.Join(evidenceDir+".incomplete", "v*", "evidence", "PRODUCT.yaml")); len(staged) != 1 {
				t.Errorf("staged copy missing: %v", staged)
			}
			out := logs.String()
			if strings.Contains(out, "accepted by the upstream validator") {
				t.Errorf("logged a complete directory that does not exist:\n%s", out)
			}
			if !strings.Contains(out, "CNCF submission directory is incomplete") {
				t.Errorf("missing the incomplete warning:\n%s", out)
			}
		})
	}
}

// withEveryMustCheck adds the MUST-evidencing checks the h100-eks-training
// chain does not declare (inference-gateway, robust-controller), so a
// submission run of the test recipe passes the #3703 coverage pre-flight and
// reaches the code under test. The recipe is the test's own fixture.
func withEveryMustCheck(t *testing.T, rec *aicr.RecipeResult) {
	t.Helper()
	resolved := rec.Resolved()
	if resolved == nil || resolved.Validation == nil || resolved.Validation.Conformance == nil {
		t.Fatal("setup: the test recipe declares no conformance phase")
	}
	for _, name := range []string{"inference-gateway", "robust-controller"} {
		if !slices.Contains(resolved.Validation.Conformance.Checks, name) {
			resolved.Validation.Conformance.Checks = append(resolved.Validation.Conformance.Checks, name)
		}
	}
}

// TestRunValidationRefusesRecipeCoverageGap pins the #3703 coverage
// pre-flight at the CLI: a submission run of a recipe that cannot establish
// an applicable MUST is refused before any check runs, naming the
// requirement and the check; a plain conformance run logs the gap and
// proceeds.
func TestRunValidationRefusesRecipeCoverageGap(t *testing.T) {
	client, rec := phaseLogRecipe(t)
	withEveryMustCheck(t, rec)
	resolved := rec.Resolved()
	resolved.Validation.Conformance.Checks = slices.DeleteFunc(resolved.Validation.Conformance.Checks,
		func(name string) bool { return name == "gang-scheduling" })
	snap := phaseLogSnapshot()
	base := func(dir string) validationConfig {
		return validationConfig{
			phases:              []validator.Phase{validator.PhaseConformance},
			runID:               "20260908-000000-0123456789abcdef",
			output:              filepath.Join(dir, "out.yaml"),
			outFormat:           serializer.FormatYAML,
			validationNamespace: "aicr-validation-test",
			cleanup:             true,
			noCluster:           true,
			failOnError:         true,
		}
	}

	t.Run("submission run refused before the run", func(t *testing.T) {
		dir := t.TempDir()
		cfg := base(dir)
		cfg.cncfSubmission = true
		cfg.evidenceDir = filepath.Join(dir, "evidence")
		cfg.cncfVendor = testCNCFVendor()
		// The fixture snapshot runs Kubernetes 1.34, which no carried
		// checklist matches; the explicit version gets past that pre-flight
		// so this one is the refusal under test.
		cfg.cncfConformanceVersion = "v1.37"
		err := runValidation(t.Context(), client, rec, snap, cfg)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		for _, want := range []string{"gang_scheduling: missing gang-scheduling", "spec.validation.conformance.checks"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want substring %q", err, want)
			}
		}
		if _, statErr := os.Stat(filepath.Join(dir, "out.yaml")); statErr == nil {
			t.Error("a CTRF report was written although the run was refused before it started")
		}
	})

	t.Run("plain conformance run warns and proceeds", func(t *testing.T) {
		dir := t.TempDir()
		logs := captureJSONLogs(t)
		if err := runValidation(t.Context(), client, rec, snap, base(dir)); err != nil {
			t.Fatalf("runValidation: %v", err)
		}
		rec := findJSONLogRecord(t, logs.Bytes(), "the recipe's conformance checks would not establish every applicable MUST of the CNCF checklist; this run is not a submission, so it proceeds")
		if msg, _ := rec["error"].(string); !strings.Contains(msg, "gang_scheduling: missing gang-scheduling") {
			t.Errorf("warning error = %q, want the gap named", msg)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "out.yaml")); statErr != nil {
			t.Errorf("CTRF report not written: %v; a plain run must proceed", statErr)
		}
	})
}

// TestRunValidationRefusesUncarriedMinor pins the #3703 version pre-flight at
// the CLI: once the snapshot is in hand and before any check runs, a
// submission run of a cluster whose Kubernetes minor no carried checklist
// matches is refused, and spec.validate.evidence.cncf.conformanceVersion
// lifts the refusal.
func TestRunValidationRefusesUncarriedMinor(t *testing.T) {
	client, rec := phaseLogRecipe(t)
	withEveryMustCheck(t, rec)
	snap := phaseLogSnapshot() // Kubernetes 1.34, which no carried checklist matches
	base := func(dir string) validationConfig {
		return validationConfig{
			phases:              []validator.Phase{validator.PhaseConformance},
			runID:               "20260908-000000-0123456789abcdef",
			output:              filepath.Join(dir, "out.yaml"),
			outFormat:           serializer.FormatYAML,
			validationNamespace: "aicr-validation-test",
			cleanup:             true,
			noCluster:           true,
			failOnError:         true,
			cncfSubmission:      true,
			evidenceDir:         filepath.Join(dir, "evidence"),
			cncfVendor:          testCNCFVendor(),
		}
	}

	t.Run("refused before the run", func(t *testing.T) {
		dir := t.TempDir()
		err := runValidation(t.Context(), client, rec, snap, base(dir))
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if err == nil || !strings.Contains(err.Error(), "1.34.0") || !strings.Contains(err.Error(), "ConformanceVersion") {
			t.Errorf("error = %v, want the cluster version and the override named", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "out.yaml")); statErr == nil {
			t.Error("a CTRF report was written although the run was refused before it started")
		}
	})

	t.Run("conformanceVersion lifts the refusal", func(t *testing.T) {
		dir := t.TempDir()
		cfg := base(dir)
		cfg.cncfConformanceVersion = "v1.37"
		err := runValidation(t.Context(), client, rec, snap, cfg)
		// Under --no-cluster every check is skipped, so the run reaches the
		// verdict and fails there, as TestRunValidationSubmissionVerdict does.
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		if _, statErr := os.Stat(filepath.Join(dir, "out.yaml")); statErr != nil {
			t.Errorf("CTRF report not written: %v; the run should have proceeded", statErr)
		}
	})

	t.Run("a submission without a vendor is refused", func(t *testing.T) {
		dir := t.TempDir()
		cfg := base(dir)
		cfg.cncfVendor = nil
		err := runValidation(t.Context(), client, rec, snap, cfg)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if err == nil || !strings.Contains(err.Error(), "vendor") {
			t.Errorf("error = %v, want the vendor requirement named", err)
		}
	})
}

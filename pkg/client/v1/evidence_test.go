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
	stderrors "errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// TestCNCFEvidenceFeatures_Empty pins the removed --feature interface: the
// deprecated lister returns an empty, non-nil, unshared slice.
func TestCNCFEvidenceFeatures_Empty(t *testing.T) {
	t.Parallel()
	got := CNCFEvidenceFeatures()
	if got == nil || len(got) != 0 {
		t.Fatalf("CNCFEvidenceFeatures() = %#v, want an empty non-nil slice", got)
	}
}

func TestRenderCNCFEvidence(t *testing.T) {
	t.Parallel()

	report := &ctrf.Report{}
	report.Results.Tests = []ctrf.TestResult{
		{Name: "dra-support", Status: ctrf.StatusPassed, Duration: 1000},
		{Name: "gang-scheduling", Status: ctrf.StatusSkipped},
	}

	t.Run("writes submission evidence", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := (&Client{}).RenderCNCFEvidence(context.Background(), report, dir); err != nil {
			t.Fatalf("RenderCNCFEvidence: %v", err)
		}
		for _, f := range []string{"index.md", "dra-support.md"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("%s not written: %v", f, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "gang-scheduling.md")); !os.IsNotExist(err) {
			t.Errorf("gang-scheduling.md written for a skipped check (stat err = %v)", err)
		}
	})

	t.Run("refuses a report with a withheld check", func(t *testing.T) {
		t.Parallel()
		withheld := &ctrf.Report{}
		withheld.Results.Tests = []ctrf.TestResult{
			{Name: "dra-support", Status: ctrf.StatusPassed, Duration: 1000},
			{Name: "gang-scheduling", Status: ctrf.StatusSkipped,
				Extra: map[string]string{"skipReason": validator.SkipCheckReasonCode}},
		}
		dir := t.TempDir()
		errorstest.WantReportedCode(t, (&Client{}).RenderCNCFEvidence(context.Background(), withheld, dir), aicrerrors.ErrCodeInvalidRequest)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("evidence dir = %v (err %v), want it left empty", entries, err)
		}
	})

	tests := []struct {
		name   string
		client *Client
		ctx    context.Context
		dir    string
	}{
		{"nil client", nil, context.Background(), "out"},
		{"nil context", &Client{}, nil, "out"},
		{"empty dir", &Client{}, context.Background(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errorstest.WantReportedCode(t, tt.client.RenderCNCFEvidence(tt.ctx, report, tt.dir), aicrerrors.ErrCodeInvalidRequest)
		})
	}
}

// TestCollectCNCFEvidence covers the argument contract of the run-plus-render
// wrapper. Every case is refused before the snapshot agent is deployed, so
// nothing reaches a cluster; the run itself is exercised by the CLI's
// --cncf-submission path on a live cluster.
func TestCollectCNCFEvidence(t *testing.T) {
	t.Parallel()

	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	recipePath := filepath.Join(t.TempDir(), "recipe.yaml")
	recipeYAML := "kind: RecipeMetadata\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: test\nspec:\n  criteria:\n" +
		"    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n"
	if err = os.WriteFile(recipePath, []byte(recipeYAML), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	rec, err := client.LoadRecipe(context.Background(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}

	closed, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	closedRec, err := closed.LoadRecipe(context.Background(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	if err = closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	valid := CNCFCollectOptions{Dir: t.TempDir()}
	withFeatures := valid
	withFeatures.Features = []string{"gang-scheduling"}
	noCluster := valid
	noCluster.NoCluster = true
	withoutDir := valid
	withoutDir.Dir = ""
	badNotApplicable := valid
	badNotApplicable.NotApplicable = map[string]string{"gang_scheduling": "we do not gang"}

	tests := []struct {
		name        string
		client      *Client
		ctx         context.Context
		rec         *RecipeResult
		opts        CNCFCollectOptions
		wantMessage string
	}{
		{name: "nil recipe", client: client, ctx: context.Background(), opts: valid, wantMessage: "recipe is required"},
		{name: "features removed", client: client, ctx: context.Background(), rec: rec, opts: withFeatures, wantMessage: "--feature interface was removed"},
		{name: "no-cluster removed", client: client, ctx: context.Background(), rec: rec, opts: noCluster, wantMessage: "needs a live cluster"},
		{name: "recipe missing internal", client: client, ctx: context.Background(), rec: &RecipeResult{Name: "x"}, opts: valid, wantMessage: "no internal recipe state"},
		{name: "recipe from another client", client: newClientForBundleTest(t), ctx: context.Background(), rec: rec, opts: valid, wantMessage: "Client"},
		{name: "empty dir", client: client, ctx: context.Background(), rec: rec, opts: withoutDir, wantMessage: "directory is required"},
		{name: "non-conditional notApplicable", client: client, ctx: context.Background(), rec: rec, opts: badNotApplicable, wantMessage: `"gang_scheduling" is not a conditional MUST`},
		{name: "uncarried ConformanceVersion", client: client, ctx: context.Background(), rec: rec, opts: CNCFCollectOptions{Dir: t.TempDir(), ConformanceVersion: "v1.99"}, wantMessage: "v1.99"},
		{name: "nil client", ctx: context.Background(), rec: rec, opts: valid, wantMessage: "not initialized"},
		{name: "nil context", client: client, rec: rec, opts: valid, wantMessage: "context is required"},
		{name: "closed client", client: closed, ctx: context.Background(), rec: closedRec, opts: valid, wantMessage: "already closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.client.CollectCNCFEvidence(tt.ctx, tt.rec, tt.opts)
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMessage)
			}
		})
	}
}

func TestMergeReports(t *testing.T) {
	t.Parallel()

	c := &Client{version: "v1.2.3"}

	r1 := &ctrf.Report{}
	r1.Results.Summary = ctrf.Summary{Tests: 2, Passed: 2}
	r2 := &ctrf.Report{}
	r2.Results.Summary = ctrf.Summary{Tests: 3, Passed: 1, Failed: 2}

	results := []*PhaseResult{
		{Phase: "deployment", Report: r1},
		nil,                                 // nil result skipped
		{Phase: "performance", Report: r2},  // counts merged
		{Phase: "conformance", Report: nil}, // nil report contributes nothing
	}

	merged := c.MergeReports(results)
	if merged == nil {
		t.Fatal("MergeReports returned nil")
	}
	if merged.Results.Tool.Name != "aicr" {
		t.Errorf("tool name = %q, want aicr", merged.Results.Tool.Name)
	}
	if merged.Results.Tool.Version != "v1.2.3" {
		t.Errorf("tool version = %q, want v1.2.3", merged.Results.Tool.Version)
	}
	if got := merged.Results.Summary.Tests; got != 5 {
		t.Errorf("merged tests = %d, want 5", got)
	}
	if got := merged.Results.Summary.Passed; got != 3 {
		t.Errorf("merged passed = %d, want 3", got)
	}
	if got := merged.Results.Summary.Failed; got != 2 {
		t.Errorf("merged failed = %d, want 2", got)
	}
}

// TestMergeReports_NilReceiver locks in that MergeReports tolerates a nil
// Client (empty version), so a caller cannot panic on it.
func TestMergeReports_NilReceiver(t *testing.T) {
	t.Parallel()
	var c *Client
	merged := c.MergeReports(nil)
	if merged == nil {
		t.Fatal("MergeReports(nil) returned nil")
	}
	if merged.Results.Tool.Version != "" {
		t.Errorf("version = %q, want empty for nil client", merged.Results.Tool.Version)
	}
}

func TestEmitRecipeEvidence_RejectsBadInput(t *testing.T) {
	t.Parallel()

	validClient := newClientForBundleTest(t)
	validRecipe := newRecipeResultForBundleTest(validClient,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	validSnap := &Snapshot{}

	tests := []struct {
		name   string
		client *Client
		recipe *RecipeResult
		snap   *Snapshot
		opts   EvidenceOptions
	}{
		{"nil client", nil, validRecipe, validSnap, EvidenceOptions{OutDir: "out"}},
		{"nil recipe", validClient, nil, validSnap, EvidenceOptions{OutDir: "out"}},
		{"recipe missing internal", validClient, &RecipeResult{Name: "no-internal"}, validSnap, EvidenceOptions{OutDir: "out"}},
		{"nil snapshot", validClient, validRecipe, nil, EvidenceOptions{OutDir: "out"}},
		{"empty outdir", validClient, validRecipe, validSnap, EvidenceOptions{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.client.EmitRecipeEvidence(context.Background(), tt.recipe, tt.snap, nil, tt.opts)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var se *aicrerrors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected *aicrerrors.StructuredError, got %T: %v", err, err)
			}
			if se.Code != aicrerrors.ErrCodeInvalidRequest {
				t.Errorf("expected ErrCodeInvalidRequest, got %s", se.Code)
			}
		})
	}
}

// TestEmitRecipeEvidence_RejectsClosedClient locks in the closed-Client guard:
// after Close() clears the builder, evidence emission must fail closed rather
// than loading a catalog from a half-torn-down Client.
func TestEmitRecipeEvidence_RejectsClosedClient(t *testing.T) {
	t.Parallel()

	c := newClientForBundleTest(t)
	r := newRecipeResultForBundleTest(c,
		[]recipe.ComponentRef{{Name: "c1", Type: recipe.ComponentTypeHelm}},
		[]ComponentRef{{Name: "c1", Kind: "Helm"}},
	)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := c.EmitRecipeEvidence(context.Background(), r, &Snapshot{}, nil, EvidenceOptions{OutDir: "out"})
	if err == nil {
		t.Fatalf("expected error from closed Client, got nil")
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

// TestCollectCNCFEvidence_ReachesSnapshot passes every argument check and
// fails at the first cluster stage: the kubeconfig names a file that does not
// exist, so CollectSnapshot cannot build a client. It pins that the resolved
// inputs (absolute paths, default namespace) reach the run without a cluster.
func TestCollectCNCFEvidence_ReachesSnapshot(t *testing.T) {
	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	recipePath := filepath.Join(t.TempDir(), "recipe.yaml")
	recipeYAML := "kind: RecipeMetadata\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: test\nspec:\n  criteria:\n" +
		"    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n"
	if err = os.WriteFile(recipePath, []byte(recipeYAML), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	rec, err := client.LoadRecipe(context.Background(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	t.Chdir(t.TempDir())

	err = client.CollectCNCFEvidence(context.Background(), rec, CNCFCollectOptions{
		Dir:        "evidence",
		Kubeconfig: "kubeconfig-that-does-not-exist",
	})
	if err == nil {
		t.Fatal("CollectCNCFEvidence succeeded with a kubeconfig that does not exist")
	}
	if errorstest.ReportedCode(err) == aicrerrors.ErrCodeInvalidRequest && strings.Contains(err.Error(), "CNCFCollectOptions") {
		t.Fatalf("refused on an option instead of reaching the snapshot stage: %v", err)
	}
	if _, statErr := os.Stat("evidence"); statErr == nil {
		t.Error("evidence directory was created before any check ran")
	}
}

// fakeSubmissionRunner stands in for the Client in runCNCFSubmission so the
// composition (snapshot, conformance run, merge, render, verdict) and its
// error propagation are exercised without a cluster.
type fakeSubmissionRunner struct {
	agentCfg     *AgentConfig
	validateOpts []ValidateOption
	// preflightOpts records the ValidateCNCFSubmissionOptions call; stages
	// lists the methods in call order so a test can assert the pre-flight
	// ran before ValidateState.
	preflightOpts *CNCFSubmissionOptions
	preflightErr  error
	coverageOpts  *RenderCNCFEvidenceOptions
	coverageErr   error
	stages        []string
	// snapshotVersion is the Kubernetes version the fake snapshot reports.
	snapshotVersion string
	renderOpts      RenderCNCFEvidenceOptions
	renderDir       string
	submitOpts      *CNCFSubmissionOptions
	submitDir       string

	snapshotErr error
	validateErr error
	renderErr   error
	report      *ctrf.Report
	agg         *CNCFRequirementAggregate
}

func (f *fakeSubmissionRunner) CollectSnapshot(_ context.Context, cfg *AgentConfig) (*Snapshot, error) {
	f.stages = append(f.stages, "CollectSnapshot")
	f.agentCfg = cfg
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	if f.snapshotVersion == "" {
		return &Snapshot{}, nil
	}
	return WrapSnapshot(&snapshotter.Snapshot{Measurements: []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtypeBuilder(measurement.NewSubtypeBuilder("server").SetString("version", f.snapshotVersion)).
			Build(),
	}}), nil
}

func (f *fakeSubmissionRunner) ValidateCNCFSubmissionOptions(opts CNCFSubmissionOptions, _ string) error {
	f.stages = append(f.stages, "ValidateCNCFSubmissionOptions")
	f.preflightOpts = &opts
	return f.preflightErr
}

func (f *fakeSubmissionRunner) ValidateCNCFRecipeCoverage(_ *RecipeResult, opts RenderCNCFEvidenceOptions) error {
	f.stages = append(f.stages, "ValidateCNCFRecipeCoverage")
	f.coverageOpts = &opts
	return f.coverageErr
}

func (f *fakeSubmissionRunner) ValidateState(_ context.Context, _ *RecipeResult, _ *Snapshot, opts ...ValidateOption) ([]*PhaseResult, error) {
	f.stages = append(f.stages, "ValidateState")
	f.validateOpts = opts
	if f.validateErr != nil {
		return nil, f.validateErr
	}
	report := f.report
	if report == nil {
		report = &ctrf.Report{}
	}
	return []*PhaseResult{{Phase: PhaseConformance, Status: "passed", Report: report}}, nil
}

func (f *fakeSubmissionRunner) MergeReports(results []*PhaseResult) *ctrf.Report {
	return results[0].Report
}

func (f *fakeSubmissionRunner) RenderCNCFEvidenceWithResult(_ context.Context, _ *ctrf.Report, dir string, opts RenderCNCFEvidenceOptions) (*CNCFRequirementAggregate, error) {
	f.renderDir = dir
	f.renderOpts = opts
	if f.renderErr != nil {
		return nil, f.renderErr
	}
	return f.agg, nil
}

func (f *fakeSubmissionRunner) WriteCNCFSubmission(_ context.Context, _ *ctrf.Report, dir string, _ *Snapshot, _ *RecipeResult, opts CNCFSubmissionOptions) (*CNCFSubmissionResult, error) {
	f.submitDir = dir
	f.submitOpts = &opts
	if f.renderErr != nil {
		return nil, f.renderErr
	}
	return &CNCFSubmissionResult{Aggregate: f.agg, Complete: f.agg != nil && f.agg.Passed(), Dir: dir}, nil
}

// TestRunCNCFSubmissionExactMinorPreflight pins the #3703 pre-flight in the
// Go submission run: with a vendor, the snapshot's Kubernetes version is
// checked against the carried checklists before ValidateState, so an
// uncarried minor never costs a validator run; ConformanceVersion is carried
// into that check and into the write.
func TestRunCNCFSubmissionExactMinorPreflight(t *testing.T) {
	vendor := CNCFVendor{Name: "Test Vendor", PlatformName: "P", PlatformVersion: "1", Description: "d", ContactEmail: "o@v.example",
		WebsiteURL: "https://v.example", DocumentationURL: "https://v.example/docs", ProductLogoURL: "https://v.example/l.svg",
		K8sConformanceURL: "https://github.com/cncf/k8s-conformance/tree/master/v1.37/p"}
	passed := &CNCFRequirementAggregate{Version: cncf.LatestVersion}

	t.Run("an uncarried minor fails before ValidateState", func(t *testing.T) {
		runner := &fakeSubmissionRunner{agg: passed, snapshotVersion: "1.34.0",
			preflightErr: aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "no carried CNCF AI Conformance checklist certifies Kubernetes \"1.34.0\"")}
		err := runCNCFSubmission(context.Background(), runner, &RecipeResult{}, cncfSubmissionInputs{dir: "/tmp/out", vendor: &vendor})
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		want := []string{"CollectSnapshot", "ValidateCNCFSubmissionOptions"}
		if !slices.Equal(runner.stages, want) {
			t.Errorf("stages = %v, want %v: the refusal must land before any validator Job", runner.stages, want)
		}
		if runner.preflightOpts == nil || runner.preflightOpts.KubernetesVersion != "1.34.0" || runner.preflightOpts.Vendor != vendor {
			t.Errorf("pre-flight options = %+v, want the snapshot's version and the vendor", runner.preflightOpts)
		}
	})

	t.Run("ConformanceVersion reaches the pre-flight and the write", func(t *testing.T) {
		runner := &fakeSubmissionRunner{agg: passed, snapshotVersion: "1.34.0"}
		err := runCNCFSubmission(context.Background(), runner, &RecipeResult{}, cncfSubmissionInputs{dir: "/tmp/out", vendor: &vendor, conformanceVersion: "v1.37"})
		if err != nil {
			t.Fatalf("runCNCFSubmission() error = %v", err)
		}
		want := []string{"CollectSnapshot", "ValidateCNCFSubmissionOptions", "ValidateCNCFRecipeCoverage", "ValidateState"}
		if len(runner.stages) < 4 || !slices.Equal(runner.stages[:4], want) {
			t.Errorf("stages = %v, want prefix %v", runner.stages, want)
		}
		if runner.preflightOpts == nil || runner.preflightOpts.ConformanceVersion != "v1.37" || runner.submitOpts == nil || runner.submitOpts.ConformanceVersion != "v1.37" {
			t.Errorf("ConformanceVersion not carried: pre-flight %+v, write %+v", runner.preflightOpts, runner.submitOpts)
		}
	})

	t.Run("a recipe that cannot establish a MUST fails before ValidateState", func(t *testing.T) {
		runner := &fakeSubmissionRunner{agg: passed, snapshotVersion: "1.37.2",
			coverageErr: aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "gang_scheduling: missing gang-scheduling")}
		err := runCNCFSubmission(context.Background(), runner, &RecipeResult{}, cncfSubmissionInputs{dir: "/tmp/out", vendor: &vendor,
			notApplicable: map[string]string{"cluster_autoscaling": "fixed"}})
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		want := []string{"CollectSnapshot", "ValidateCNCFSubmissionOptions", "ValidateCNCFRecipeCoverage"}
		if !slices.Equal(runner.stages, want) {
			t.Errorf("stages = %v, want %v: the coverage refusal must land before any validator Job", runner.stages, want)
		}
		if runner.coverageOpts == nil || runner.coverageOpts.KubernetesVersion != "1.37.2" || runner.coverageOpts.NotApplicable["cluster_autoscaling"] == "" {
			t.Errorf("coverage options = %+v, want the snapshot's version and notApplicable", runner.coverageOpts)
		}
	})

	t.Run("the pages render only warns on a coverage gap", func(t *testing.T) {
		runner := &fakeSubmissionRunner{agg: passed, snapshotVersion: "1.37.2",
			coverageErr: aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "gang_scheduling: missing gang-scheduling")}
		if err := runCNCFSubmission(context.Background(), runner, &RecipeResult{}, cncfSubmissionInputs{dir: "/tmp/out"}); err != nil {
			t.Fatalf("runCNCFSubmission() error = %v, want the pages render to proceed", err)
		}
		if !slices.Contains(runner.stages, "ValidateState") {
			t.Errorf("stages = %v, want ValidateState reached", runner.stages)
		}
	})

	t.Run("the pages render runs no submission pre-flight", func(t *testing.T) {
		runner := &fakeSubmissionRunner{agg: passed, snapshotVersion: "1.34.0"}
		if err := runCNCFSubmission(context.Background(), runner, &RecipeResult{}, cncfSubmissionInputs{dir: "/tmp/out", conformanceVersion: "v1.36"}); err != nil {
			t.Fatalf("runCNCFSubmission() error = %v", err)
		}
		if slices.Contains(runner.stages, "ValidateCNCFSubmissionOptions") {
			t.Errorf("stages = %v: the pages render keeps its nearest-checklist fallback", runner.stages)
		}
		if runner.renderOpts.ConformanceVersion != "v1.36" {
			t.Errorf("render ConformanceVersion = %q, want v1.36", runner.renderOpts.ConformanceVersion)
		}
	})
}

// With a vendor, runCNCFSubmission writes the submission directory instead
// of rendering the evidence pages, carrying the same options plus the
// vendor, and still takes its exit status from the aggregate.
func TestRunCNCFSubmissionWithVendor(t *testing.T) {
	vendor := CNCFVendor{Name: "Test Vendor", PlatformName: "P", PlatformVersion: "1", Description: "d", ContactEmail: "o@v.example",
		WebsiteURL: "https://v.example", DocumentationURL: "https://v.example/docs", ProductLogoURL: "https://v.example/l.svg",
		K8sConformanceURL: "https://github.com/cncf/k8s-conformance/tree/master/v1.37/p"}
	documented := map[string]CNCFDocumentation{"cluster_autoscaling": {Evidence: []string{"https://v.example/autoscaling"}, Notes: "n"}}
	for _, tc := range []struct {
		name    string
		agg     *CNCFRequirementAggregate
		wantErr bool
	}{
		{name: "complete", agg: &CNCFRequirementAggregate{Version: cncf.LatestVersion, Requirements: []cncf.RequirementResult{
			{ID: "gang_scheduling", Level: cncf.LevelMust, Status: cncf.StatusPassed}}}},
		{name: "incomplete MUST fails the run", agg: &CNCFRequirementAggregate{Version: cncf.LatestVersion, Requirements: []cncf.RequirementResult{
			{ID: "gang_scheduling", Level: cncf.LevelMust, Status: cncf.StatusFailed, Reason: "gang-scheduling failed"}}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeSubmissionRunner{agg: tc.agg}
			err := runCNCFSubmission(context.Background(), runner, &RecipeResult{}, cncfSubmissionInputs{
				dir: "/tmp/out", notApplicable: map[string]string{}, documented: documented, vendor: &vendor,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("runCNCFSubmission() error = %v, wantErr %v", err, tc.wantErr)
			}
			if runner.renderDir != "" {
				t.Error("evidence pages were rendered although a vendor was set")
			}
			carried := runner.submitOpts != nil && runner.submitOpts.Vendor == vendor && runner.submitOpts.Documented["cluster_autoscaling"].Notes == "n"
			if runner.submitDir != "/tmp/out" || !carried {
				t.Errorf("WriteCNCFSubmission called with dir %q opts %+v", runner.submitDir, runner.submitOpts)
			}
		})
	}
}

// TestRunCNCFSubmission drives the submission run through a fake runner:
// the agent is deployed privileged in the resolved namespace, the
// conformance phase alone runs in that namespace, NotApplicable reaches the
// renderer, every stage's error propagates with its own code, and the shared
// Outcome rule (per-check failure plus the aggregate's verdict) decides.
func TestRunCNCFSubmission(t *testing.T) {
	t.Parallel()

	passed := &CNCFRequirementAggregate{Version: cncf.LatestVersion, Requirements: []cncf.RequirementResult{
		{ID: "gang_scheduling", Level: cncf.LevelMust, Status: cncf.StatusPassed},
		{ID: "cluster_autoscaling", Level: cncf.LevelMust, Status: cncf.StatusNotApplicable, Reason: "fixed-size cluster"},
	}}
	incomplete := &CNCFRequirementAggregate{Version: cncf.LatestVersion, Requirements: []cncf.RequirementResult{
		{ID: "gang_scheduling", Level: cncf.LevelMust, Status: cncf.StatusNotTested, Reason: "gang-scheduling not run"},
	}}
	in := cncfSubmissionInputs{
		dir:           "/abs/evidence",
		kubeconfig:    "/abs/kubeconfig",
		namespace:     "submission-ns",
		notApplicable: map[string]string{"cluster_autoscaling": "fixed-size cluster"},
	}

	tests := []struct {
		name     string
		runner   *fakeSubmissionRunner
		wantCode aicrerrors.ErrorCode
		wantMsg  string
	}{
		{name: "every MUST established", runner: &fakeSubmissionRunner{agg: passed}},
		{name: "snapshot error propagates", runner: &fakeSubmissionRunner{snapshotErr: aicrerrors.New(aicrerrors.ErrCodeUnavailable, "agent did not start")},
			wantCode: aicrerrors.ErrCodeUnavailable, wantMsg: "agent did not start"},
		{name: "validate error propagates", runner: &fakeSubmissionRunner{validateErr: aicrerrors.New(aicrerrors.ErrCodeTimeout, "phase timed out")},
			wantCode: aicrerrors.ErrCodeTimeout, wantMsg: "phase timed out"},
		{name: "render error propagates", runner: &fakeSubmissionRunner{renderErr: aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "bad notApplicable")},
			wantCode: aicrerrors.ErrCodeInvalidRequest, wantMsg: "bad notApplicable"},
		{name: "unestablished MUST fails the run", runner: &fakeSubmissionRunner{agg: incomplete},
			wantCode: aicrerrors.ErrCodeInternal, wantMsg: "gang_scheduling not tested (gang-scheduling not run)"},
		{name: "unmapped check crashed with every MUST established fails the run", runner: &fakeSubmissionRunner{agg: passed,
			report: &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{{Name: "platform-health", Status: ctrf.StatusOther}}}}},
			wantCode: aicrerrors.ErrCodeInternal, wantMsg: "one or more checks did not pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := runCNCFSubmission(context.Background(), tt.runner, &RecipeResult{Name: "r"}, in)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("runCNCFSubmission: %v", err)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %v, want it to mention %q", err, tt.wantMsg)
				}
			}

			cfg := tt.runner.agentCfg
			if cfg == nil {
				t.Fatal("CollectSnapshot was not called")
			}
			if cfg.Namespace != in.namespace || cfg.Kubeconfig != in.kubeconfig || !cfg.Privileged || !cfg.Cleanup {
				t.Errorf("AgentConfig = %+v, want namespace %q, kubeconfig %q, privileged, cleanup", *cfg, in.namespace, in.kubeconfig)
			}
			if tt.runner.snapshotErr != nil {
				if tt.runner.validateOpts != nil {
					t.Error("ValidateState ran after the snapshot failed")
				}
				return
			}
			vc := &validateConfig{}
			for _, opt := range tt.runner.validateOpts {
				opt(vc)
			}
			if vc.namespace == nil || *vc.namespace != in.namespace || vc.kubeconfig != in.kubeconfig {
				t.Errorf("validate options: namespace %v kubeconfig %q, want %q and %q", vc.namespace, vc.kubeconfig, in.namespace, in.kubeconfig)
			}
			if len(vc.phases) != 1 || vc.phases[0] != PhaseConformance {
				t.Errorf("validate phases = %v, want only %s", vc.phases, PhaseConformance)
			}
			if tt.runner.validateErr != nil {
				if tt.runner.renderDir != "" {
					t.Error("render ran after the conformance phase failed")
				}
				return
			}
			if tt.runner.renderDir != in.dir || !maps.Equal(tt.runner.renderOpts.NotApplicable, in.notApplicable) {
				t.Errorf("render: dir %q notApplicable %v, want %q and %v", tt.runner.renderDir, tt.runner.renderOpts.NotApplicable, in.dir, in.notApplicable)
			}
		})
	}
}

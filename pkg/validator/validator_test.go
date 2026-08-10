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

package validator

import (
	"context"
	stderrors "errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/recipes"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestNewDefaults(t *testing.T) {
	v := New()

	if v.Namespace != "aicr-validation" {
		t.Errorf("Namespace = %q, want %q", v.Namespace, "aicr-validation")
	}
	if v.RunID == "" {
		t.Error("RunID should be generated")
	}
	if !v.Cleanup {
		t.Error("Cleanup should default to true")
	}
	if v.NoCluster {
		t.Error("NoCluster should default to false")
	}
	if v.Kubeconfig != "" {
		t.Errorf("Kubeconfig = %q, want empty", v.Kubeconfig)
	}
	if len(v.Tolerations) != 1 || v.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("Tolerations should default to tolerate-all, got %v", v.Tolerations)
	}
	if v.FailFast {
		t.Error("FailFast should default to false")
	}
}

func TestNewWithOptions(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithCommit("abc1234"),
		WithKubeconfig("/path/to/kubeconfig"),
		WithNamespace("custom-ns"),
		WithRunID("test-run"),
		WithCleanup(false),
		WithNoCluster(true),
		WithImagePullSecrets([]string{"secret1"}),
		WithTolerations(nil),
	)

	if v.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", v.Version, "1.0.0")
	}
	if v.Commit != "abc1234" {
		t.Errorf("Commit = %q, want %q", v.Commit, "abc1234")
	}
	if v.Kubeconfig != "/path/to/kubeconfig" {
		t.Errorf("Kubeconfig = %q, want %q", v.Kubeconfig, "/path/to/kubeconfig")
	}
	if v.Namespace != "custom-ns" {
		t.Errorf("Namespace = %q, want %q", v.Namespace, "custom-ns")
	}
	if v.RunID != "test-run" {
		t.Errorf("RunID = %q, want %q", v.RunID, "test-run")
	}
	if v.Cleanup {
		t.Error("Cleanup should be false")
	}
	if !v.NoCluster {
		t.Error("NoCluster should be true")
	}
	if len(v.ImagePullSecrets) != 1 || v.ImagePullSecrets[0] != "secret1" {
		t.Errorf("ImagePullSecrets = %v", v.ImagePullSecrets)
	}
}

// TestPrepareClusterPropagatesCustomKubeconfig verifies the run-scoped path
// reaches cluster client creation without reading a kubeconfig file or
// contacting Kubernetes. The injected factory fails before any cluster API
// operation, keeping this regression test hermetic and fail-safe.
func TestPrepareClusterPropagatesCustomKubeconfig(t *testing.T) {
	t.Parallel()

	const wantKubeconfig = "/path/to/target-kubeconfig"
	wantErr := stderrors.New("stop before cluster access")
	v := New(WithKubeconfig("  " + wantKubeconfig + "  "))

	var gotKubeconfig string
	v.kubeClientFactory = func(kubeconfig string) (kubernetes.Interface, error) {
		gotKubeconfig = kubeconfig
		return nil, wantErr
	}

	_, err := v.prepareCluster(t.Context(), nil, nil)
	if !stderrors.Is(err, wantErr) {
		t.Fatalf("prepareCluster() error = %v, want wrapped injected error", err)
	}
	if gotKubeconfig != wantKubeconfig {
		t.Errorf("kubeconfig = %q, want %q", gotKubeconfig, wantKubeconfig)
	}
}

// TestPrepareClusterEmptyKubeconfigUsesDefaultClient verifies that empty input
// is routed through default discovery without consulting the explicit-path
// client factory. The environment is cleared so default discovery fails before
// any cluster access, keeping the test hermetic.
func TestPrepareClusterEmptyKubeconfigUsesDefaultClient(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	wantFactoryErr := stderrors.New("explicit-path factory called")
	v := New(WithKubeconfig(" \t "))
	factoryCalled := false
	v.kubeClientFactory = func(string) (kubernetes.Interface, error) {
		factoryCalled = true
		return nil, wantFactoryErr
	}

	_, err := v.prepareCluster(t.Context(), nil, nil)
	if err == nil {
		t.Fatal("prepareCluster() error = nil, want default discovery error")
	}
	if factoryCalled {
		t.Error("prepareCluster() called explicit-path factory for empty kubeconfig")
	}
	if stderrors.Is(err, wantFactoryErr) {
		t.Errorf("prepareCluster() error = %v, want default discovery error", err)
	}
}

// TestPrepareClusterRejectsMissingKubeconfig verifies that a typo in a
// caller-supplied path is classified as invalid input before Kubernetes client
// construction can relabel the filesystem error as an internal failure.
func TestPrepareClusterRejectsMissingKubeconfig(t *testing.T) {
	t.Parallel()

	kubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")
	v := New(WithKubeconfig(kubeconfig))

	_, err := v.prepareCluster(t.Context(), nil, nil)
	if err == nil {
		t.Fatal("prepareCluster() error = nil, want invalid request")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("prepareCluster() error = %v, want ErrCodeInvalidRequest", err)
	}
	if !stderrors.Is(err, fs.ErrNotExist) {
		t.Errorf("prepareCluster() error = %v, want wrapped fs.ErrNotExist", err)
	}
}

func loadEmbeddedCatalog(t *testing.T) *catalog.ValidatorCatalog {
	t.Helper()
	cat, err := catalog.LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("failed to load catalog: %v", err)
	}
	return cat
}

func TestPhasesSkipped(t *testing.T) {
	v := New(WithVersion("1.0.0"))
	cat := loadEmbeddedCatalog(t)

	results := v.phasesSkipped(cat, PhaseOrder, "test reason")
	if len(results) != len(PhaseOrder) {
		t.Fatalf("expected %d results, got %d", len(PhaseOrder), len(results))
	}

	for i, pr := range results {
		if pr.Phase != PhaseOrder[i] {
			t.Errorf("results[%d].Phase = %q, want %q", i, pr.Phase, PhaseOrder[i])
		}
		if pr.Status != ctrf.StatusSkipped {
			t.Errorf("results[%d].Status = %q, want %q", i, pr.Status, ctrf.StatusSkipped)
		}
		if pr.Report == nil {
			t.Errorf("results[%d].Report should not be nil", i)
		}
	}
}

func TestPhasesSkippedSubset(t *testing.T) {
	v := New(WithVersion("1.0.0"))
	cat := loadEmbeddedCatalog(t)

	subset := []Phase{PhaseDeployment}
	results := v.phasesSkipped(cat, subset, "test reason")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Phase != PhaseDeployment {
		t.Errorf("Phase = %q, want %q", results[0].Phase, PhaseDeployment)
	}
}

func TestPhaseSkipped(t *testing.T) {
	v := New(WithVersion("1.0.0"))
	cat := loadEmbeddedCatalog(t)

	pr := v.phaseSkipped(cat, PhaseDeployment, "no cluster")
	if pr.Phase != PhaseDeployment {
		t.Errorf("Phase = %q, want %q", pr.Phase, PhaseDeployment)
	}
	if pr.Status != ctrf.StatusSkipped {
		t.Errorf("Status = %q, want %q", pr.Status, ctrf.StatusSkipped)
	}
	if pr.Report == nil {
		t.Fatal("Report should not be nil")
	}
	if pr.Report.ReportFormat != ctrf.ReportFormatCTRF {
		t.Errorf("ReportFormat = %q, want %q", pr.Report.ReportFormat, ctrf.ReportFormatCTRF)
	}
}

func TestValidatePhasesNoClusterAll(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	results, err := v.ValidatePhases(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("ValidatePhases() failed: %v", err)
	}

	if len(results) != len(PhaseOrder) {
		t.Fatalf("expected %d results, got %d", len(PhaseOrder), len(results))
	}

	for _, pr := range results {
		if pr.Status != ctrf.StatusSkipped {
			t.Errorf("phase %q status = %q, want %q", pr.Phase, pr.Status, ctrf.StatusSkipped)
		}
	}
}

func TestValidatePhasesNoClusterSubset(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	results, err := v.ValidatePhases(context.Background(), []Phase{PhaseDeployment}, nil, nil)
	if err != nil {
		t.Fatalf("ValidatePhases() failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Phase != PhaseDeployment {
		t.Errorf("Phase = %q, want %q", results[0].Phase, PhaseDeployment)
	}
}

func TestValidatePhaseNoCluster(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	pr, err := v.ValidatePhase(context.Background(), PhaseDeployment, nil, nil)
	if err != nil {
		t.Fatalf("ValidatePhase() failed: %v", err)
	}

	if pr.Status != ctrf.StatusSkipped {
		t.Errorf("status = %q, want %q", pr.Status, ctrf.StatusSkipped)
	}
	if pr.Phase != PhaseDeployment {
		t.Errorf("phase = %q, want %q", pr.Phase, PhaseDeployment)
	}
}

// validationWithChecks builds a ValidationInput declaring the given check
// names per phase. Used by the preflight tests to exercise unmatched,
// cross-phase, and duplicate declarations.
func validationWithChecks(checksByPhase map[Phase][]string) *v1.ValidationInput {
	vi := &v1.ValidationInput{}
	for phase, checks := range checksByPhase {
		vp := &v1.ValidationPhase{Checks: checks}
		switch phase {
		case PhaseDeployment:
			vi.Config.Deployment = vp
		case PhasePerformance:
			vi.Config.Performance = vp
		case PhaseConformance:
			vi.Config.Conformance = vp
		}
	}
	return vi
}

func TestPreflightDeclaredChecks(t *testing.T) {
	cat := &catalog.ValidatorCatalog{
		Validators: []catalog.ValidatorEntry{
			{Name: "operator-health", Phase: "deployment"},
			{Name: "expected-resources", Phase: "deployment"},
			{Name: "nccl-all-reduce", Phase: "performance"},
		},
	}
	v := New(WithVersion("1.0.0"))

	tests := []struct {
		name        string
		phases      []Phase
		checks      map[Phase][]string
		wantErr     bool
		wantSubstrs []string
		notSubstrs  []string
	}{
		{
			name:   "all matched",
			phases: []Phase{PhaseDeployment},
			checks: map[Phase][]string{PhaseDeployment: {"operator-health", "expected-resources"}},
		},
		{
			name:        "typo unmatched anywhere",
			phases:      []Phase{PhaseDeployment},
			checks:      map[Phase][]string{PhaseDeployment: {"operator-health", "expected-resoures"}},
			wantErr:     true,
			wantSubstrs: []string{"expected-resoures", "matches no validator in the catalog"},
		},
		{
			name:        "declared under wrong phase names the other phase",
			phases:      []Phase{PhaseDeployment},
			checks:      map[Phase][]string{PhaseDeployment: {"nccl-all-reduce"}},
			wantErr:     true,
			wantSubstrs: []string{"nccl-all-reduce", "found under phase: performance"},
		},
		{
			name:        "mixed valid and invalid surfaces only the invalid",
			phases:      []Phase{PhaseDeployment},
			checks:      map[Phase][]string{PhaseDeployment: {"operator-health", "bogus-check"}},
			wantErr:     true,
			wantSubstrs: []string{"bogus-check"},
			// The valid check must not be reported as a problem.
			notSubstrs: []string{"operator-health"},
		},
		{
			name:        "duplicate declaration",
			phases:      []Phase{PhaseDeployment},
			checks:      map[Phase][]string{PhaseDeployment: {"operator-health", "operator-health"}},
			wantErr:     true,
			wantSubstrs: []string{"operator-health", "more than once"},
		},
		{
			name:   "aggregates offenders across all requested phases",
			phases: []Phase{PhaseDeployment, PhasePerformance},
			checks: map[Phase][]string{
				PhaseDeployment:  {"typo-a"},
				PhasePerformance: {"typo-b"},
			},
			wantErr:     true,
			wantSubstrs: []string{"typo-a", "typo-b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vi := validationWithChecks(tt.checks)
			err := v.preflightDeclaredChecks(cat, tt.phases, vi)
			if (err != nil) != tt.wantErr {
				t.Fatalf("preflightDeclaredChecks() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
			}
			for _, sub := range tt.wantSubstrs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q missing expected substring %q", err.Error(), sub)
				}
			}
			for _, sub := range tt.notSubstrs {
				if strings.Contains(err.Error(), sub) {
					t.Errorf("error %q unexpectedly contains %q", err.Error(), sub)
				}
			}
		})
	}
}

// TestPreflightDeclaredChecks_ExternalCatalogMissingCheck models an incomplete
// external (--data) catalog: a recipe declares a check that the loaded catalog
// does not supply. The preflight must fail closed rather than let the phase
// filter down to zero tests and pass spuriously.
func TestPreflightDeclaredChecks_ExternalCatalogMissingCheck(t *testing.T) {
	externalCatalog := &catalog.ValidatorCatalog{
		Validators: []catalog.ValidatorEntry{
			{Name: "operator-health", Phase: "deployment"},
			// A required gate the external catalog forgot to include.
		},
	}
	v := New(WithVersion("1.0.0"))

	vi := validationWithChecks(map[Phase][]string{
		PhaseDeployment: {"operator-health", "expected-resources"},
	})

	err := v.preflightDeclaredChecks(externalCatalog, []Phase{PhaseDeployment}, vi)
	if err == nil {
		t.Fatal("preflightDeclaredChecks() = nil error, want fail-closed on missing external-catalog check")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "expected-resources") {
		t.Errorf("error %q missing the unmatched check name", err.Error())
	}
}

// TestValidatePhaseNoClusterRejectsUnmatchedCheck proves the fail-closed gate
// runs in --no-cluster mode through the real entry point: an unmatched check
// must error, not report a spuriously passing skipped phase (issue #2121).
func TestValidatePhaseNoClusterRejectsUnmatchedCheck(t *testing.T) {
	v := New(WithVersion("1.0.0"), WithNoCluster(true))
	vi := validationWithChecks(map[Phase][]string{
		PhaseDeployment: {"this-check-does-not-exist"},
	})

	pr, err := v.ValidatePhase(context.Background(), PhaseDeployment, vi, nil)
	if err == nil {
		t.Fatalf("ValidatePhase(--no-cluster) = %+v, nil error; want fail-closed on unmatched check", pr)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}

// TestValidatePhasesNoClusterRejectsUnmatchedCheck is the plural-path twin: the
// default client validate route (ValidatePhases) must also fail closed offline.
func TestValidatePhasesNoClusterRejectsUnmatchedCheck(t *testing.T) {
	v := New(WithVersion("1.0.0"), WithNoCluster(true))
	vi := validationWithChecks(map[Phase][]string{
		PhaseDeployment: {"this-check-does-not-exist"},
	})

	results, err := v.ValidatePhases(context.Background(), []Phase{PhaseDeployment}, vi, nil)
	if err == nil {
		t.Fatalf("ValidatePhases(--no-cluster) = %+v, nil error; want fail-closed on unmatched check", results)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}

func TestValidatePhaseRunsReadinessPreflight(t *testing.T) {
	// The per-phase SDK entry point must enforce the same readiness gate as
	// ValidatePhases: a caller running a single phase must not be able to
	// bypass the recipe's readiness constraints (e.g. the GKE device-plugin
	// ownership check, issue #1755). Runs in no-cluster mode — readiness is
	// evaluated inline before the no-cluster short-circuit.
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 99.0"},
		},
	}
	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.30.0"),
						},
					},
				},
			},
		},
	}

	_, err := v.ValidatePhase(context.Background(), PhaseDeployment, v1.ToValidationInput(rec), snap)
	if err == nil {
		t.Fatal("ValidatePhase() = nil error, want readiness failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}

func TestValidatePhasesRunsReadinessPreflight(t *testing.T) {
	// The plural entry point is the DEFAULT client validate path
	// (pkg/client/v1/aicr.go routes full-phase validation through
	// ValidatePhases), so its readiness gate needs its own pin: a regression
	// that reorders checkReadiness below the NoCluster short-circuit would
	// pass every singular-path test while silently fail-opening the primary
	// path — the exact #1755 failure this gate exists to prevent. Uses a
	// non-nil snapshot so the gate must actually evaluate the constraint
	// rather than fail on snapshot absence.
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 99.0"},
		},
	}
	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.30.0"),
						},
					},
				},
			},
		},
	}

	results, err := v.ValidatePhases(context.Background(), nil, v1.ToValidationInput(rec), snap)
	if err == nil {
		t.Fatal("ValidatePhases() = nil error, want readiness failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
	if results != nil {
		t.Errorf("PhaseResults = %v, want nil: no phase may run after a readiness failure", results)
	}
}

func TestCheckReadinessNilInputs(t *testing.T) {
	tests := []struct {
		name string
		rec  *recipe.RecipeResult
		snap *snapshotter.Snapshot
	}{
		{"nil recipe", nil, &snapshotter.Snapshot{}},
		{"nil snapshot", &recipe.RecipeResult{}, nil},
		{"both nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validationInput := v1.ToValidationInput(tt.rec)
			if err := checkReadiness(validationInput, tt.snap); err != nil {
				t.Errorf("checkReadiness() = %v, want nil", err)
			}
		})
	}
}

func TestCheckReadinessNilSnapshotWithConstraintsFailsClosed(t *testing.T) {
	// Declared readiness constraints with no snapshot must error, not
	// silently skip — a direct SDK caller passing a nil snapshot must not
	// bypass the gate.
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 1.28"},
		},
	}
	err := checkReadiness(v1.ToValidationInput(rec), nil)
	if err == nil {
		t.Fatal("checkReadiness() = nil, want error for declared constraints without a snapshot")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}

func TestCheckReadinessEmptyConstraints(t *testing.T) {
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{},
	}
	snap := &snapshotter.Snapshot{}
	validationInput := v1.ToValidationInput(rec)
	if err := checkReadiness(validationInput, snap); err != nil {
		t.Errorf("checkReadiness() = %v, want nil for empty constraints", err)
	}
}

func TestCheckReadinessUnparseableConstraintFailsClosed(t *testing.T) {
	// A pre-flight gate must fail closed on evaluator errors so a malformed
	// validation YAML cannot masquerade as a passing constraint. An
	// unparseable constraint name surfaces as an error from Evaluate; the
	// caller propagates it as ErrCodeInvalidRequest.
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "invalid-path-that-will-be-skipped", Value: "anything"},
		},
	}
	snap := &snapshotter.Snapshot{}
	validationInput := v1.ToValidationInput(rec)
	if err := checkReadiness(validationInput, snap); err == nil {
		t.Errorf("checkReadiness() = nil, want error for unevaluable constraint")
	}
}

func TestCheckReadinessEvaluatesReadinessPhaseConstraints(t *testing.T) {
	// validation.readiness.constraints must be evaluated by the pre-flight
	// gate alongside the top-level constraint set (issue #1755): they exist
	// for gates that must fail `aicr validate` closed without participating
	// in generation-time overlay filtering. The failure message must carry
	// the constraint's remediation so the operator gets the diagnostic.
	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.30.0"),
						},
					},
				},
			},
		},
	}

	passC := recipe.Constraint{Name: "K8s.server.version", Value: ">= 1.28"}
	failC := recipe.Constraint{
		Name:        "K8s.server.version",
		Value:       ">= 99.0",
		Remediation: "upgrade the control plane",
	}

	tests := []struct {
		name      string
		topLevel  []recipe.Constraint
		readiness []recipe.Constraint
		wantErr   bool
		wantIn    string // substring the error must carry; "" skips
	}{
		{
			name:      "failing readiness-phase constraint carries remediation",
			readiness: []recipe.Constraint{failC},
			wantErr:   true,
			wantIn:    "upgrade the control plane",
		},
		{
			name:      "passing readiness-phase constraint",
			readiness: []recipe.Constraint{passC},
		},
		{
			name:      "passing top-level with failing readiness-phase",
			topLevel:  []recipe.Constraint{passC},
			readiness: []recipe.Constraint{failC},
			wantErr:   true,
			wantIn:    "upgrade the control plane",
		},
		{
			name:      "passing top-level with passing readiness-phase",
			topLevel:  []recipe.Constraint{passC},
			readiness: []recipe.Constraint{passC},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recipe.RecipeResult{
				Constraints: tt.topLevel,
				Validation: &recipe.ValidationConfig{
					Readiness: &recipe.ValidationPhase{Constraints: tt.readiness},
				},
			}
			vi := v1.ToValidationInput(rec)
			topBefore := len(vi.Constraints)

			err := checkReadiness(vi, snap)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkReadiness() = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantIn != "" && !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantIn)
			}
			// The combined evaluation must not grow the input's top-level
			// slice: ToValidationInput aliases the recipe's Constraints, so
			// an aliasing append would write readiness constraints into the
			// caller's recipe (the capped append in checkReadiness prevents
			// this).
			if len(vi.Constraints) != topBefore {
				t.Errorf("checkReadiness mutated Constraints: len %d -> %d", topBefore, len(vi.Constraints))
			}
		})
	}
}

func TestPhaseOrder(t *testing.T) {
	// performance runs last: its benchmark saturates all node GPUs and releases
	// DRA claims asynchronously, which would otherwise starve conformance's
	// GPU-needing checks (e.g. dra-support).
	expected := []Phase{PhaseDeployment, PhaseConformance, PhasePerformance}
	if len(PhaseOrder) != len(expected) {
		t.Fatalf("PhaseOrder length = %d, want %d", len(PhaseOrder), len(expected))
	}
	for i, p := range PhaseOrder {
		if p != expected[i] {
			t.Errorf("PhaseOrder[%d] = %q, want %q", i, p, expected[i])
		}
	}
}

// TestRecipeCheckNamesMatchCatalog verifies that every check name referenced
// in recipe overlays exists in the validator catalog for the correct phase.
// Catches typos and drift between recipes and catalog at PR time.
func TestRecipeCheckNamesMatchCatalog(t *testing.T) {
	cat, err := catalog.LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("failed to load catalog: %v", err)
	}

	// Build lookup: phase → set of valid check names.
	validChecks := map[string]map[string]bool{
		"deployment":  make(map[string]bool),
		"performance": make(map[string]bool),
		"conformance": make(map[string]bool),
	}
	for _, entry := range cat.Validators {
		if m, ok := validChecks[entry.Phase]; ok {
			m[entry.Name] = true
		}
	}

	// Walk all embedded overlay YAML files.
	err = fs.WalkDir(recipes.FS, "overlays", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}

		data, readErr := recipes.FS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("failed to read %s: %w", path, readErr)
		}

		var metadata recipe.RecipeMetadata
		if unmarshalErr := yaml.Unmarshal(data, &metadata); unmarshalErr != nil {
			return nil // skip non-recipe YAML
		}

		if metadata.Spec.Validation == nil {
			return nil
		}

		phases := map[string]*recipe.ValidationPhase{
			"deployment":  metadata.Spec.Validation.Deployment,
			"performance": metadata.Spec.Validation.Performance,
			"conformance": metadata.Spec.Validation.Conformance,
		}

		for phase, vp := range phases {
			if vp == nil {
				continue
			}
			for _, checkName := range vp.Checks {
				if !validChecks[phase][checkName] {
					t.Errorf("%s: check %q in %s phase does not exist in catalog (valid: %v)",
						path, checkName, phase, catalogNames(cat, phase))
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk overlays: %v", err)
	}
}

func catalogNames(cat *catalog.ValidatorCatalog, phase string) []string {
	entries := cat.ForPhase(Phase(phase))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

func TestExtractResultSummaries(t *testing.T) {
	tests := []struct {
		name   string
		stdout []string
		want   []string
	}{
		{
			name:   "no RESULT lines — nothing extracted",
			stdout: []string{"time=... level=INFO msg=starting", "All pods running"},
			want:   []string{},
		},
		{
			name: "extracts prefixed lines in order",
			stdout: []string{
				"time=... level=INFO msg=check running",
				"RESULT: Inference throughput: 39399.24 tokens/sec",
				"RESULT: Inference TTFT p99: 138.27 ms",
				"Throughput constraint: >= 5000 → PASS",
			},
			want: []string{
				"Inference throughput: 39399.24 tokens/sec",
				"Inference TTFT p99: 138.27 ms",
			},
		},
		{
			name: "RESULT without trailing content is skipped (no empty emission)",
			stdout: []string{
				"RESULT: ",
				"RESULT: real summary",
			},
			want: []string{"real summary"},
		},
		{
			name:   "empty stdout — empty result",
			stdout: nil,
			want:   []string{},
		},
		{
			name: "prefix must match exactly — lowercase 'result:' does not qualify",
			stdout: []string{
				"result: not-a-summary",
				"RESULT:no-space-after-colon",
				"RESULT: valid",
			},
			want: []string{"valid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractResultSummaries(tt.stdout)
			if len(got) != len(tt.want) {
				t.Fatalf("extractResultSummaries() len = %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractResultSummaries()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestWithFailFast_SetsField(t *testing.T) {
	v := New(WithFailFast(true))
	if !v.FailFast {
		t.Error("WithFailFast(true): FailFast should be true")
	}
}

// newFakeRunner returns a runner that returns the given status for every phase
// and records how many times it was called.
func newFakeRunner(status string, calls *int) func(Phase) (*PhaseResult, error) {
	return func(phase Phase) (*PhaseResult, error) {
		*calls++
		return &PhaseResult{
			Phase:  phase,
			Status: status,
			Report: ctrf.NewBuilder("aicr", "test", string(phase)).Build(),
		}, nil
	}
}

func TestRunPhases_Default_AllPhasesRun(t *testing.T) {
	v := New(WithVersion("1.0.0")) // FailFast defaults to false
	cat := loadEmbeddedCatalog(t)

	calls := 0
	results, err := v.runPhases(context.Background(), newFakeRunner(ctrf.StatusFailed, &calls), cat, PhaseOrder)
	if err != nil {
		t.Fatalf("runPhases() error = %v", err)
	}
	if calls != len(PhaseOrder) {
		t.Errorf("runner called %d times, want %d (no fail-fast)", calls, len(PhaseOrder))
	}
	if len(results) != len(PhaseOrder) {
		t.Fatalf("results len = %d, want %d", len(results), len(PhaseOrder))
	}
	for _, pr := range results {
		if pr.Status != ctrf.StatusFailed {
			t.Errorf("phase %q status = %q, want %q", pr.Phase, pr.Status, ctrf.StatusFailed)
		}
	}
}

func TestRunPhases_FailFast_StopsAfterFirstFailure(t *testing.T) {
	v := New(WithVersion("1.0.0"), WithFailFast(true))
	cat := loadEmbeddedCatalog(t)

	calls := 0
	results, err := v.runPhases(context.Background(), newFakeRunner(ctrf.StatusFailed, &calls), cat, PhaseOrder)
	if err != nil {
		t.Fatalf("runPhases() error = %v", err)
	}
	if calls != 1 {
		t.Errorf("runner called %d times, want 1 (fail-fast after first failure)", calls)
	}
	if len(results) != len(PhaseOrder) {
		t.Fatalf("results len = %d, want %d", len(results), len(PhaseOrder))
	}
	if results[0].Status != ctrf.StatusFailed {
		t.Errorf("results[0].Status = %q, want %q", results[0].Status, ctrf.StatusFailed)
	}
	for _, pr := range results[1:] {
		if pr.Status != ctrf.StatusSkipped {
			t.Errorf("phase %q status = %q, want %q (skipped by fail-fast)", pr.Phase, pr.Status, ctrf.StatusSkipped)
		}
	}
}

func TestRunPhases_FailFast_OtherPhaseGates(t *testing.T) {
	// A phase whose status is "other" (checks could not be executed to a
	// verdict — e.g. the deployment readiness gate's pods never ran during a
	// node reboot) must trip fail-fast just like "failed". Otherwise a
	// not-ready cluster masquerades as passing and later phases run against it.
	v := New(WithVersion("1.0.0"), WithFailFast(true))
	cat := loadEmbeddedCatalog(t)

	calls := 0
	results, err := v.runPhases(context.Background(), newFakeRunner(ctrf.StatusOther, &calls), cat, PhaseOrder)
	if err != nil {
		t.Fatalf("runPhases() error = %v", err)
	}
	if calls != 1 {
		t.Errorf("runner called %d times, want 1 (fail-fast after first \"other\" phase)", calls)
	}
	if results[0].Status != ctrf.StatusOther {
		t.Errorf("results[0].Status = %q, want %q", results[0].Status, ctrf.StatusOther)
	}
	for _, pr := range results[1:] {
		if pr.Status != ctrf.StatusSkipped {
			t.Errorf("phase %q status = %q, want %q (skipped by fail-fast)", pr.Phase, pr.Status, ctrf.StatusSkipped)
		}
	}
}

func TestRunPhases_FailFast_PassedPhaseDoesNotGate(t *testing.T) {
	// Phase[0]=passed, Phase[1]=failed → Phase[2] skipped; runner called twice.
	v := New(WithVersion("1.0.0"), WithFailFast(true))
	cat := loadEmbeddedCatalog(t)

	statuses := []string{ctrf.StatusPassed, ctrf.StatusFailed}
	idx := 0
	runner := func(phase Phase) (*PhaseResult, error) {
		status := statuses[idx]
		idx++
		return &PhaseResult{
			Phase:  phase,
			Status: status,
			Report: ctrf.NewBuilder("aicr", "test", string(phase)).Build(),
		}, nil
	}

	results, err := v.runPhases(context.Background(), runner, cat, PhaseOrder)
	if err != nil {
		t.Fatalf("runPhases() error = %v", err)
	}
	if idx != 2 {
		t.Errorf("runner called %d times, want 2", idx)
	}
	if results[0].Status != ctrf.StatusPassed {
		t.Errorf("results[0].Status = %q, want passed", results[0].Status)
	}
	if results[1].Status != ctrf.StatusFailed {
		t.Errorf("results[1].Status = %q, want failed", results[1].Status)
	}
	if results[2].Status != ctrf.StatusSkipped {
		t.Errorf("results[2].Status = %q, want skipped", results[2].Status)
	}
}

// newFakeClusterClient returns a fake clientset whose server-side-apply calls
// materialize the applied object in the tracker. The upstream fake client does
// not implement SSA (an Apply returns NotFound without creating anything), so
// this reactor lets ensureNamespace and job.EnsureRBAC create the per-run
// Namespace, ServiceAccount, and ClusterRoleBinding the RBAC-rollback tests
// depend on. Extra reactors may be prepended by the caller afterward.
func newFakeClusterClient() *k8sfake.Clientset {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(clienttesting.PatchAction)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil // not an apply — let the default handler run
		}
		gvr := pa.GetResource()
		name := pa.GetName()
		ns := pa.GetNamespace()
		var obj runtime.Object
		switch gvr.Resource {
		case "serviceaccounts":
			obj = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		case "clusterrolebindings":
			obj = &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
		case "namespaces":
			obj = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		default:
			return false, nil, nil
		}
		// Ignore AlreadyExists: EnsureRBAC applies each object exactly once per run.
		_ = cs.Tracker().Create(gvr, obj, ns)
		return true, obj, nil
	})
	return cs
}

// TestPrepareClusterRollsBackRBACOnConfigMapFailure guards issue #2119: when a
// preparation step AFTER job.EnsureRBAC fails (here, data-ConfigMap creation),
// prepareCluster must revoke the per-run cluster-admin ClusterRoleBinding and
// ServiceAccount before returning, rather than leaking a privileged identity.
// Reverting the in-prepareCluster rollback leaves both resources behind and
// fails this test.
func TestPrepareClusterRollsBackRBACOnConfigMapFailure(t *testing.T) {
	cs := newFakeClusterClient()

	// Fail every data-ConfigMap create so ensureDataConfigMaps errors out AFTER
	// EnsureRBAC has already created the privileged RBAC.
	cs.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("configmap backend unavailable")
	})

	v := New(WithRunID("rollback-run"), WithKubeconfig("in-memory"))
	v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return cs, nil }

	_, err := v.prepareCluster(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("prepareCluster() error = nil, want ConfigMap failure")
	}

	// No active cluster-admin subject must remain after the failed run.
	crbName := job.ClusterRoleBindingName(v.RunID)
	if _, getErr := cs.RbacV1().ClusterRoleBindings().Get(context.Background(), crbName, metav1.GetOptions{}); getErr == nil {
		t.Errorf("ClusterRoleBinding %q survived the failed run; cluster-admin leaked", crbName)
	}
	saName := job.ServiceAccountName(v.RunID)
	if _, getErr := cs.CoreV1().ServiceAccounts(v.Namespace).Get(context.Background(), saName, metav1.GetOptions{}); getErr == nil {
		t.Errorf("ServiceAccount %q survived the failed run", saName)
	}
}

// TestValidatePhasesPromotesRBACCleanupFailure proves the success path fails
// closed on privileged cleanup: when every phase passes but revoking the
// per-run ClusterRoleBinding fails, ValidatePhases must promote that failure
// into its returned error rather than reporting a clean success. Reverting the
// named-return promotion makes ValidatePhases return nil and fails this test.
func TestValidatePhasesPromotesRBACCleanupFailure(t *testing.T) {
	cs := newFakeClusterClient()
	cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver unavailable")
	})

	v := New(WithRunID("promote-phases"), WithKubeconfig("in-memory"))
	v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return cs, nil }

	// nil validationInput selects no checks, so the phases run to a clean pass
	// without deploying any Jobs — isolating the cleanup-failure promotion.
	results, err := v.ValidatePhases(context.Background(), []Phase{PhaseDeployment}, nil, nil)
	if err == nil {
		t.Fatal("ValidatePhases() error = nil, want promoted RBAC cleanup failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("ValidatePhases() error = %v, want ErrCodeInternal", err)
	}
	if len(results) == 0 {
		t.Error("ValidatePhases() returned no results; phases should still have run")
	}
}

// TestValidatePhasePromotesRBACCleanupFailure is the single-phase analog of
// TestValidatePhasesPromotesRBACCleanupFailure.
func TestValidatePhasePromotesRBACCleanupFailure(t *testing.T) {
	cs := newFakeClusterClient()
	cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver unavailable")
	})

	v := New(WithRunID("promote-phase"), WithKubeconfig("in-memory"))
	v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return cs, nil }

	result, err := v.ValidatePhase(context.Background(), PhaseDeployment, nil, nil)
	if err == nil {
		t.Fatal("ValidatePhase() error = nil, want promoted RBAC cleanup failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("ValidatePhase() error = %v, want ErrCodeInternal", err)
	}
	if result == nil {
		t.Error("ValidatePhase() result = nil; the phase should still have produced a result")
	}
}

// TestPrepareClusterSurfacesRollbackFailure guards the issue #2119 hardening:
// when a preparation step fails AFTER EnsureRBAC and the rollback's
// ClusterRoleBinding delete ALSO fails, prepareCluster must fold the rollback
// failure into its returned error — surfacing that cluster-admin may be
// orphaned rather than swallowing it. Reverting the fold in the rollback defer
// leaves the returned error carrying only the prep cause and fails the
// injected-rollback-cause assertion.
func TestPrepareClusterSurfacesRollbackFailure(t *testing.T) {
	cs := newFakeClusterClient()

	// Fail data-ConfigMap creation so prepareCluster errors after EnsureRBAC.
	cs.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("configmap backend unavailable")
	})

	// Fail the rollback's ClusterRoleBinding delete and record that it ran.
	rollbackDeleteCause := stderrors.New("apiserver unavailable during rollback")
	var crbDeleteAttempted bool
	cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		crbDeleteAttempted = true
		return true, nil, rollbackDeleteCause
	})

	v := New(WithRunID("rollback-fail"), WithKubeconfig("in-memory"))
	v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return cs, nil }

	_, err := v.prepareCluster(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("prepareCluster() error = nil, want prep + rollback failure")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("prepareCluster() error = %v, want ErrCodeInternal", err)
	}
	if !stderrors.Is(err, rollbackDeleteCause) {
		t.Errorf("prepareCluster() error = %v, want the rollback delete cause surfaced, not swallowed", err)
	}
	if !crbDeleteAttempted {
		t.Error("prepareCluster() did not attempt to delete the ClusterRoleBinding during rollback")
	}
}

// TestPrepareClusterNoRollbackWhenCleanupDisabled locks in the intentional
// --cleanup=false behavior (issue #2119 is explicitly distinct from #306, which
// owns that contract): when cleanup is disabled and preparation fails after
// EnsureRBAC, prepareCluster must NOT attempt any rollback delete, leaving the
// per-run RBAC in place for a caller debugging a failed setup. Making rollback
// unconditional would delete the binding and fail both assertions below.
func TestPrepareClusterNoRollbackWhenCleanupDisabled(t *testing.T) {
	cs := newFakeClusterClient()

	cs.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("configmap backend unavailable")
	})

	var deleted []string
	cs.PrependReactor("delete", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deleted = append(deleted, action.GetResource().Resource)
		return false, nil, nil // record only; fall through to the default tracker
	})

	v := New(WithRunID("no-rollback"), WithKubeconfig("in-memory"), WithCleanup(false))
	v.kubeClientFactory = func(string) (kubernetes.Interface, error) { return cs, nil }

	_, err := v.prepareCluster(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("prepareCluster() error = nil, want ConfigMap failure")
	}

	for _, r := range deleted {
		if r == "clusterrolebindings" || r == "serviceaccounts" {
			t.Errorf("prepareCluster() attempted RBAC delete %q with cleanup disabled; want none", r)
		}
	}

	// The per-run cluster-admin binding must remain for manual inspection.
	crbName := job.ClusterRoleBindingName(v.RunID)
	if _, getErr := cs.RbacV1().ClusterRoleBindings().Get(context.Background(), crbName, metav1.GetOptions{}); getErr != nil {
		t.Errorf("ClusterRoleBinding %q was removed despite cleanup=false: %v", crbName, getErr)
	}
}

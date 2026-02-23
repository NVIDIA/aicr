// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

func init() {
	// Register a test-only check for buildTestPattern tests.
	// Cannot import deployment checks due to import cycle, so register directly.
	checks.RegisterCheck(&checks.Check{
		Name:        "test-registered-check",
		Description: "Test-only check for buildTestPattern coverage",
		Phase:       "deployment",
		TestName:    "TestRegisteredCheck",
	})
}

func TestValidatePhase(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()
	recipeResult := createTestRecipeWithValidation()

	tests := []struct {
		name       string
		phase      ValidationPhaseName
		wantStatus ValidationStatus
		wantPhases int // Number of phases in result
	}{
		{
			name:       "readiness phase",
			phase:      PhaseReadiness,
			wantStatus: ValidationStatusPass,
			wantPhases: 1,
		},
		{
			name:       "deployment phase",
			phase:      PhaseDeployment,
			wantStatus: ValidationStatusPass,
			wantPhases: 1,
		},
		{
			name:       "performance phase",
			phase:      PhasePerformance,
			wantStatus: ValidationStatusPass,
			wantPhases: 1,
		},
		{
			name:       "conformance phase",
			phase:      PhaseConformance,
			wantStatus: ValidationStatusSkipped, // Not configured in test recipe
			wantPhases: 1,
		},
		{
			name:       "all phases",
			phase:      PhaseAll,
			wantStatus: ValidationStatusPass,
			wantPhases: 4, // All 4 phases
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"), WithNoCluster(true))
			result, err := v.ValidatePhase(context.Background(), tt.phase, recipeResult, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}

			if len(result.Phases) != tt.wantPhases {
				t.Errorf("Phases count = %d, want %d", len(result.Phases), tt.wantPhases)
			}
		})
	}
}

func TestValidateReadiness(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()

	tests := []struct {
		name            string
		constraints     []recipe.Constraint
		wantStatus      ValidationStatus
		wantConstraints int
	}{
		{
			name: "single constraint",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
			},
			wantStatus:      ValidationStatusPass,
			wantConstraints: 1,
		},
		{
			name: "multiple constraints",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
				{Name: "OS.release.ID", Value: "ubuntu"},
			},
			wantStatus:      ValidationStatusPass,
			wantConstraints: 2,
		},
		{
			name:            "no constraints",
			constraints:     []recipe.Constraint{},
			wantStatus:      ValidationStatusPass,
			wantConstraints: 0,
		},
		{
			name: "failing constraint",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 99.0"}, // Will fail
			},
			wantStatus:      ValidationStatusFail,
			wantConstraints: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"), WithNoCluster(true))
			recipeResult := &recipe.RecipeResult{
				Constraints: tt.constraints,
			}

			result, err := v.validateReadiness(context.Background(), recipeResult, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}

			phaseResult := result.Phases[string(PhaseReadiness)]
			if phaseResult == nil {
				t.Fatal("readiness phase result is nil")
			}

			if len(phaseResult.Constraints) != tt.wantConstraints {
				t.Errorf("Constraints count = %d, want %d", len(phaseResult.Constraints), tt.wantConstraints)
			}
		})
	}
}

func TestValidateDeployment(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()

	tests := []struct {
		name             string
		validationConfig *recipe.ValidationConfig
		wantStatus       ValidationStatus
		wantChecks       int
	}{
		{
			name: "with checks",
			validationConfig: &recipe.ValidationConfig{
				Deployment: &recipe.ValidationPhase{
					Checks: []string{"operator-health", "expected-resources"},
				},
			},
			wantStatus: ValidationStatusPass,
			wantChecks: 2,
		},
		{
			name:             "not configured",
			validationConfig: nil,
			wantStatus:       ValidationStatusSkipped,
			wantChecks:       0,
		},
		{
			name: "with constraints",
			validationConfig: &recipe.ValidationConfig{
				Deployment: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "gpu-operator.version", Value: "== v25.10.1"},
					},
					Checks: []string{"operator-health"},
				},
			},
			wantStatus: ValidationStatusPass,
			wantChecks: 1, // Checks count (constraints are in separate array)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"), WithNoCluster(true))
			recipeResult := &recipe.RecipeResult{
				Validation: tt.validationConfig,
			}

			result, err := v.validateDeployment(context.Background(), recipeResult, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}

			phaseResult := result.Phases[string(PhaseDeployment)]
			if phaseResult == nil {
				t.Fatal("deployment phase result is nil")
			}

			if len(phaseResult.Checks) != tt.wantChecks {
				t.Errorf("Checks count = %d, want %d", len(phaseResult.Checks), tt.wantChecks)
			}
		})
	}
}

func TestValidatePerformance(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()

	tests := []struct {
		name             string
		validationConfig *recipe.ValidationConfig
		wantStatus       ValidationStatus
		wantChecks       int
	}{
		{
			name: "with checks and infrastructure",
			validationConfig: &recipe.ValidationConfig{
				Performance: &recipe.ValidationPhase{
					Infrastructure: "nccl-doctor",
					Checks:         []string{"nccl-bandwidth-test", "fabric-health"},
				},
			},
			wantStatus: ValidationStatusPass,
			wantChecks: 2,
		},
		{
			name:             "not configured",
			validationConfig: nil,
			wantStatus:       ValidationStatusSkipped,
			wantChecks:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"), WithNoCluster(true))
			recipeResult := &recipe.RecipeResult{
				Validation: tt.validationConfig,
			}

			result, err := v.validatePerformance(context.Background(), recipeResult, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}

			phaseResult := result.Phases[string(PhasePerformance)]
			if phaseResult == nil {
				t.Fatal("performance phase result is nil")
			}

			if len(phaseResult.Checks) != tt.wantChecks {
				t.Errorf("Checks count = %d, want %d", len(phaseResult.Checks), tt.wantChecks)
			}
		})
	}
}

func TestValidateConformance(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()

	tests := []struct {
		name             string
		validationConfig *recipe.ValidationConfig
		wantStatus       ValidationStatus
		wantChecks       int
	}{
		{
			name: "with checks",
			validationConfig: &recipe.ValidationConfig{
				Conformance: &recipe.ValidationPhase{
					Checks: []string{"ai-workload-validation"},
				},
			},
			wantStatus: ValidationStatusPass,
			wantChecks: 1,
		},
		{
			name:             "not configured",
			validationConfig: nil,
			wantStatus:       ValidationStatusSkipped,
			wantChecks:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"), WithNoCluster(true))
			recipeResult := &recipe.RecipeResult{
				Validation: tt.validationConfig,
			}

			result, err := v.validateConformance(context.Background(), recipeResult, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}

			phaseResult := result.Phases[string(PhaseConformance)]
			if phaseResult == nil {
				t.Fatal("conformance phase result is nil")
			}

			if len(phaseResult.Checks) != tt.wantChecks {
				t.Errorf("Checks count = %d, want %d", len(phaseResult.Checks), tt.wantChecks)
			}
		})
	}
}

func TestValidateAll_PhaseOrder(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()
	recipeResult := createTestRecipeWithValidation()

	v := New(WithVersion("test"), WithNoCluster(true))
	result, err := v.validateAll(context.Background(), recipeResult, snapshot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all phases are present
	expectedPhases := []string{"readiness", "deployment", "performance", "conformance"}
	for _, phaseName := range expectedPhases {
		if result.Phases[phaseName] == nil {
			t.Errorf("phase %q is missing from result", phaseName)
		}
	}

	// Verify readiness has constraint results
	readinessPhase := result.Phases["readiness"]
	if len(readinessPhase.Constraints) == 0 {
		t.Error("readiness phase should have constraint results")
	}

	// Verify deployment has check results
	deployPhase := result.Phases["deployment"]
	if len(deployPhase.Checks) == 0 {
		t.Error("deployment phase should have check results")
	}
}

func TestValidateAll_PhaseDependencies(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	// This test would verify phase dependency logic (fail → skip subsequent)
	// For skeleton implementation, all phases pass, so we can't test skip logic yet
	// TODO: Add test when we have real validation that can fail

	snapshot := createTestSnapshot()
	recipeResult := createTestRecipeWithValidation()

	v := New(WithVersion("test"), WithNoCluster(true))
	result, err := v.validateAll(context.Background(), recipeResult, snapshot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// In skeleton, all phases should pass
	if result.Summary.Status != ValidationStatusPass {
		t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, ValidationStatusPass)
	}

	// No phases should be skipped (all pass in skeleton)
	for phaseName, phase := range result.Phases {
		if phaseName == "conformance" {
			// Conformance is not configured, so it should be skipped
			if phase.Status != ValidationStatusSkipped {
				t.Errorf("conformance phase Status = %v, want %v", phase.Status, ValidationStatusSkipped)
			}
		} else {
			if phase.Status != ValidationStatusPass {
				t.Errorf("%s phase Status = %v, want %v", phaseName, phase.Status, ValidationStatusPass)
			}
		}
	}
}

// Helper functions

func createTestSnapshot() *snapshotter.Snapshot {
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.35.0"),
						},
					},
				},
			},
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID":         measurement.Str("ubuntu"),
							"VERSION_ID": measurement.Str("24.04"),
						},
					},
				},
			},
		},
	}
}

func createTestRecipeWithValidation() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 1.32.4"},
			{Name: "OS.release.ID", Value: "ubuntu"},
		},
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Checks: []string{"operator-health", "expected-resources"},
			},
			Performance: &recipe.ValidationPhase{
				Infrastructure: "nccl-doctor",
				Checks:         []string{"nccl-bandwidth-test", "fabric-health"},
			},
			// Conformance not configured (will be skipped)
		},
	}
}

func TestValidatePhases(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()
	recipeResult := createTestRecipeWithValidation()

	tests := []struct {
		name       string
		phases     []ValidationPhaseName
		wantStatus ValidationStatus
		wantPhases int // Number of phases in result
	}{
		{
			name:       "empty phases defaults to readiness",
			phases:     []ValidationPhaseName{},
			wantStatus: ValidationStatusPass,
			wantPhases: 1,
		},
		{
			name:       "single readiness phase",
			phases:     []ValidationPhaseName{PhaseReadiness},
			wantStatus: ValidationStatusPass,
			wantPhases: 1,
		},
		{
			name:       "single deployment phase",
			phases:     []ValidationPhaseName{PhaseDeployment},
			wantStatus: ValidationStatusPass,
			wantPhases: 1,
		},
		{
			name:       "multiple phases - readiness and deployment",
			phases:     []ValidationPhaseName{PhaseReadiness, PhaseDeployment},
			wantStatus: ValidationStatusPass,
			wantPhases: 2,
		},
		{
			name:       "multiple phases - readiness, deployment, performance",
			phases:     []ValidationPhaseName{PhaseReadiness, PhaseDeployment, PhasePerformance},
			wantStatus: ValidationStatusPass,
			wantPhases: 3,
		},
		{
			name:       "all phases via PhaseAll in list",
			phases:     []ValidationPhaseName{PhaseAll},
			wantStatus: ValidationStatusPass,
			wantPhases: 4, // All 4 phases
		},
		{
			name:       "PhaseAll mixed with others uses all",
			phases:     []ValidationPhaseName{PhaseReadiness, PhaseAll},
			wantStatus: ValidationStatusPass,
			wantPhases: 4, // All 4 phases because PhaseAll is present
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"))
			result, err := v.ValidatePhases(context.Background(), tt.phases, recipeResult, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Summary.Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}

			if len(result.Phases) != tt.wantPhases {
				t.Errorf("Phases count = %d, want %d", len(result.Phases), tt.wantPhases)
			}
		})
	}
}

func TestValidatePhases_ContextCanceled(t *testing.T) {
	// Skip if running with -short flag (for fast unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Kubernetes cluster available (integration test requires cluster)
	if _, _, err := client.GetKubeClient(); err != nil {
		t.Skipf("Skipping integration test: no Kubernetes cluster available: %v", err)
	}

	snapshot := createTestSnapshot()
	recipeResult := createTestRecipeWithValidation()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	v := New(WithVersion("test"))
	_, err := v.ValidatePhases(ctx, []ValidationPhaseName{PhaseReadiness, PhaseDeployment}, recipeResult, snapshot)

	if err == nil {
		t.Error("expected error for canceled context, got nil")
	}
}

func TestValidatePhases_SummaryCounts(t *testing.T) {
	snapshot := createTestSnapshot()

	tests := []struct {
		name        string
		recipe      *recipe.RecipeResult
		phases      []ValidationPhaseName
		wantPassed  int
		wantFailed  int
		wantSkipped int
		wantTotal   int
	}{
		{
			name: "readiness only configured - others skipped",
			recipe: &recipe.RecipeResult{
				Constraints: []recipe.Constraint{
					{Name: "K8s.server.version", Value: ">= 1.32.4"},
					{Name: "OS.release.ID", Value: "ubuntu"},
				},
				// No Deployment/Performance/Conformance configured
			},
			phases:      []ValidationPhaseName{PhaseReadiness, PhaseDeployment, PhasePerformance, PhaseConformance},
			wantPassed:  1, // readiness
			wantSkipped: 3, // deployment, performance, conformance
			wantTotal:   4,
		},
		{
			name: "single readiness phase",
			recipe: &recipe.RecipeResult{
				Constraints: []recipe.Constraint{
					{Name: "K8s.server.version", Value: ">= 1.32.4"},
				},
			},
			phases:     []ValidationPhaseName{PhaseReadiness},
			wantPassed: 1, // readiness passes, summary reflects constraint count
			wantTotal:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"), WithNoCluster(true))
			result, err := v.ValidatePhases(context.Background(), tt.phases, tt.recipe, snapshot)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Passed != tt.wantPassed {
				t.Errorf("Summary.Passed = %d, want %d", result.Summary.Passed, tt.wantPassed)
			}
			if result.Summary.Failed != tt.wantFailed {
				t.Errorf("Summary.Failed = %d, want %d", result.Summary.Failed, tt.wantFailed)
			}
			if result.Summary.Skipped != tt.wantSkipped {
				t.Errorf("Summary.Skipped = %d, want %d", result.Summary.Skipped, tt.wantSkipped)
			}
			if result.Summary.Total != tt.wantTotal {
				t.Errorf("Summary.Total = %d, want %d", result.Summary.Total, tt.wantTotal)
			}
		})
	}
}

// TestMapTestStatusToValidationStatus tests the mapping of test status strings to validation status
func TestMapTestStatusToValidationStatus(t *testing.T) {
	tests := []struct {
		name       string
		testStatus string
		want       ValidationStatus
	}{
		{
			name:       "pass status",
			testStatus: "pass",
			want:       ValidationStatusPass,
		},
		{
			name:       "fail status",
			testStatus: "fail",
			want:       ValidationStatusFail,
		},
		{
			name:       "skip status",
			testStatus: "skip",
			want:       ValidationStatusSkipped,
		},
		{
			name:       "unknown status defaults to warning",
			testStatus: "unknown",
			want:       ValidationStatusWarning,
		},
		{
			name:       "empty status defaults to warning",
			testStatus: "",
			want:       ValidationStatusWarning,
		},
		{
			name:       "arbitrary status defaults to warning",
			testStatus: "some-random-status",
			want:       ValidationStatusWarning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapTestStatusToValidationStatus(tt.testStatus)
			if got != tt.want {
				t.Errorf("mapTestStatusToValidationStatus(%q) = %v, want %v", tt.testStatus, got, tt.want)
			}
		})
	}
}

// TestDetermineStartPhase tests the logic for determining which phase to start from
func TestDetermineStartPhase(t *testing.T) {
	tests := []struct {
		name           string
		existingResult *ValidationResult
		want           ValidationPhaseName
	}{
		{
			name: "no phases run - start from readiness",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{},
			},
			want: PhaseReadiness,
		},
		{
			name: "readiness failed - resume from readiness",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{
					string(PhaseReadiness): {
						Status: ValidationStatusFail,
					},
				},
			},
			want: PhaseReadiness,
		},
		{
			name: "readiness passed, deployment not started - resume from deployment",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{
					string(PhaseReadiness): {
						Status: ValidationStatusPass,
					},
				},
			},
			want: PhaseDeployment,
		},
		{
			name: "readiness and deployment passed, performance not started - resume from performance",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{
					string(PhaseReadiness): {
						Status: ValidationStatusPass,
					},
					string(PhaseDeployment): {
						Status: ValidationStatusPass,
					},
				},
			},
			want: PhasePerformance,
		},
		{
			name: "readiness passed, deployment failed - resume from deployment",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{
					string(PhaseReadiness): {
						Status: ValidationStatusPass,
					},
					string(PhaseDeployment): {
						Status: ValidationStatusFail,
					},
				},
			},
			want: PhaseDeployment,
		},
		{
			name: "all phases passed - start from readiness",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{
					string(PhaseReadiness): {
						Status: ValidationStatusPass,
					},
					string(PhaseDeployment): {
						Status: ValidationStatusPass,
					},
					string(PhasePerformance): {
						Status: ValidationStatusPass,
					},
					string(PhaseConformance): {
						Status: ValidationStatusPass,
					},
				},
			},
			want: PhaseReadiness,
		},
		{
			name: "readiness and deployment passed, performance failed, conformance passed - resume from performance",
			existingResult: &ValidationResult{
				Phases: map[string]*PhaseResult{
					string(PhaseReadiness): {
						Status: ValidationStatusPass,
					},
					string(PhaseDeployment): {
						Status: ValidationStatusPass,
					},
					string(PhasePerformance): {
						Status: ValidationStatusFail,
					},
					string(PhaseConformance): {
						Status: ValidationStatusPass,
					},
				},
			},
			want: PhasePerformance,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineStartPhase(tt.existingResult)
			if got != tt.want {
				t.Errorf("determineStartPhase() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateRecipeRegistrations(t *testing.T) {
	validator := New()

	tests := []struct {
		name               string
		recipe             *recipe.RecipeResult
		phase              string
		expectWarnings     bool
		expectedItemCount  int
		descriptionContain string
	}{
		{
			name: "deployment - unregistered constraint logs warning",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Constraints: []recipe.Constraint{
							{Name: "Deployment.nonexistent-app.version", Value: ">= v1.0.0"},
						},
					},
				},
			},
			phase:              "deployment",
			expectWarnings:     true,
			expectedItemCount:  1,
			descriptionContain: "unregistered constraint",
		},
		{
			name: "deployment - unregistered check logs warning",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks: []string{"nonexistent-check"},
					},
				},
			},
			phase:              "deployment",
			expectWarnings:     true,
			expectedItemCount:  1,
			descriptionContain: "unregistered check",
		},
		{
			name: "deployment - multiple unregistered",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Constraints: []recipe.Constraint{
							{Name: "Deployment.fake-app-1.version", Value: ">= v1.0.0"},
							{Name: "Deployment.fake-app-2.version", Value: ">= v1.0.0"},
						},
					},
				},
			},
			phase:             "deployment",
			expectWarnings:    true,
			expectedItemCount: 2,
		},
		{
			name: "performance - no constraints (no warnings)",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Performance: &recipe.ValidationPhase{
						Constraints: []recipe.Constraint{},
					},
				},
			},
			phase:          "performance",
			expectWarnings: false,
		},
		{
			name: "performance - unregistered constraint logs warning",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Performance: &recipe.ValidationPhase{
						Constraints: []recipe.Constraint{
							{Name: "Performance.nonexistent-metric.value", Value: ">= 100"},
						},
						Checks: []string{"nonexistent-perf-check"},
					},
				},
			},
			phase:             "performance",
			expectWarnings:    true,
			expectedItemCount: 1,
		},
		{
			name: "conformance - unregistered constraint and check logs warning",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Conformance: &recipe.ValidationPhase{
						Constraints: []recipe.Constraint{
							{Name: "Conformance.fake.value", Value: ">= 1.0"},
						},
						Checks: []string{"fake-conformance-check"},
					},
				},
			},
			phase:             "conformance",
			expectWarnings:    true,
			expectedItemCount: 1,
		},
		{
			name: "conformance - nil validation (no warnings)",
			recipe: &recipe.RecipeResult{
				Validation: nil,
			},
			phase:          "conformance",
			expectWarnings: false,
		},
		{
			name: "deployment - registered constraint no warning",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Constraints: []recipe.Constraint{
							{Name: "Deployment.gpu-operator.version", Value: ">= v24.6.0"},
						},
					},
				},
			},
			phase:          "deployment",
			expectWarnings: false,
		},
		{
			name: "readiness - not validated by registration check",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Readiness: &recipe.ValidationPhase{},
				},
			},
			phase:          "readiness",
			expectWarnings: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// validateRecipeRegistrations now logs warnings instead of returning errors
			// Just verify it doesn't panic
			validator.validateRecipeRegistrations(tt.recipe, tt.phase)

			// Note: In a real implementation, you could capture log output to verify warnings
			// For now, we just ensure the function completes without panicking
		})
	}
}

func TestCheckNameToTestName(t *testing.T) {
	tests := []struct {
		name      string
		checkName string
		want      string
	}{
		{
			name:      "simple hyphenated name",
			checkName: "operator-health",
			want:      "TestOperatorHealth",
		},
		{
			name:      "multiple hyphens",
			checkName: "gpu-device-plugin-check",
			want:      "TestGpuDevicePluginCheck",
		},
		{
			name:      "single word",
			checkName: "health",
			want:      "TestHealth",
		},
		{
			name:      "already capitalized parts",
			checkName: "GPU-health",
			want:      "TestGPUHealth",
		},
		{
			name:      "empty string",
			checkName: "",
			want:      "Test",
		},
		{
			name:      "dot separator",
			checkName: "gpu.operator.check",
			want:      "TestGpuOperatorCheck",
		},
		{
			name:      "underscore separator",
			checkName: "gpu_operator_check",
			want:      "TestGpuOperatorCheck",
		},
		{
			name:      "mixed separators",
			checkName: "gpu-operator.health_check",
			want:      "TestGpuOperatorHealthCheck",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkNameToTestName(tt.checkName)
			if got != tt.want {
				t.Errorf("checkNameToTestName(%q) = %q, want %q", tt.checkName, got, tt.want)
			}
		})
	}
}

func TestBuildTestPattern(t *testing.T) {
	v := New()

	tests := []struct {
		name              string
		recipe            *recipe.RecipeResult
		phase             string
		wantPattern       string
		wantExpectedTests int
	}{
		{
			name: "empty recipe returns empty pattern",
			recipe: &recipe.RecipeResult{
				Validation: nil,
			},
			phase:             "deployment",
			wantPattern:       "",
			wantExpectedTests: 0,
		},
		{
			name: "nil validation deployment returns empty pattern",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: nil,
				},
			},
			phase:             "deployment",
			wantPattern:       "",
			wantExpectedTests: 0,
		},
		{
			name: "empty deployment checks returns empty pattern",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks:      []string{},
						Constraints: []recipe.Constraint{},
					},
				},
			},
			phase:             "deployment",
			wantPattern:       "",
			wantExpectedTests: 0,
		},
		{
			name: "performance phase not implemented returns empty",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Performance: &recipe.ValidationPhase{
						Checks: []string{"perf-check"},
					},
				},
			},
			phase:             "performance",
			wantPattern:       "",
			wantExpectedTests: 0,
		},
		{
			name: "conformance phase builds pattern from checks",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Conformance: &recipe.ValidationPhase{
						Checks: []string{"conformance-check"},
					},
				},
			},
			phase:             "conformance",
			wantPattern:       "^(TestConformanceCheck)$",
			wantExpectedTests: 1,
		},
		{
			name: "deployment with registered check uses registry test name",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks: []string{"test-registered-check"},
					},
				},
			},
			phase:             "deployment",
			wantPattern:       "^(TestRegisteredCheck)$",
			wantExpectedTests: 1,
		},
		{
			name: "deployment with unregistered check falls back to generated name",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks: []string{"some-unknown-check"},
					},
				},
			},
			phase:             "deployment",
			wantPattern:       "^(TestSomeUnknownCheck)$",
			wantExpectedTests: 1,
		},
		{
			name: "deployment with multiple checks builds combined pattern",
			recipe: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks: []string{"test-registered-check", "some-unknown-check"},
					},
				},
			},
			phase:             "deployment",
			wantPattern:       "^(TestRegisteredCheck|TestSomeUnknownCheck)$",
			wantExpectedTests: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := v.buildTestPattern(tt.recipe, tt.phase)
			if result.Pattern != tt.wantPattern {
				t.Errorf("buildTestPattern() pattern = %q, want %q", result.Pattern, tt.wantPattern)
			}
			if result.ExpectedTests != tt.wantExpectedTests {
				t.Errorf("buildTestPattern() expectedTests = %d, want %d", result.ExpectedTests, tt.wantExpectedTests)
			}
		})
	}
}

func TestResolvePhaseTimeout(t *testing.T) {
	tests := []struct {
		name           string
		phase          *recipe.ValidationPhase
		defaultTimeout time.Duration
		want           time.Duration
	}{
		{
			name:           "nil phase uses default",
			phase:          nil,
			defaultTimeout: DefaultDeploymentTimeout,
			want:           DefaultDeploymentTimeout,
		},
		{
			name:           "empty timeout uses default",
			phase:          &recipe.ValidationPhase{},
			defaultTimeout: DefaultReadinessTimeout,
			want:           DefaultReadinessTimeout,
		},
		{
			name:           "recipe timeout overrides default",
			phase:          &recipe.ValidationPhase{Timeout: "15m"},
			defaultTimeout: DefaultDeploymentTimeout,
			want:           15 * time.Minute,
		},
		{
			name:           "recipe timeout in seconds",
			phase:          &recipe.ValidationPhase{Timeout: "300s"},
			defaultTimeout: DefaultDeploymentTimeout,
			want:           5 * time.Minute,
		},
		{
			name:           "invalid timeout falls back to default",
			phase:          &recipe.ValidationPhase{Timeout: "not-a-duration"},
			defaultTimeout: DefaultPerformanceTimeout,
			want:           DefaultPerformanceTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePhaseTimeout(tt.phase, tt.defaultTimeout)
			if got != tt.want {
				t.Errorf("resolvePhaseTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

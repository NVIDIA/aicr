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
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// buildCriteriaFromCmd is a test helper that constructs a recipe.Criteria
// from CLI command flags, resolving each enum value against the supplied
// per-provider registry. Used only by TestBuildCriteriaFromCmd to assert
// the registry-aware option set; production code builds criteria via the
// mergeCriteriaFromCmdAndConfig → applyCriteriaOverrides path which
// composes a config base with CLI overrides.
func buildCriteriaFromCmd(cmd *cli.Command, reg *recipe.CriteriaRegistry) (*recipe.Criteria, error) {
	var opts []recipe.RegistryCriteriaOption
	if s := cmd.String("service"); s != "" {
		opts = append(opts, recipe.WithServiceRegistry(s))
	}
	if s := cmd.String("accelerator"); s != "" {
		opts = append(opts, recipe.WithAcceleratorRegistry(s))
	}
	if s := cmd.String("intent"); s != "" {
		opts = append(opts, recipe.WithIntentRegistry(s))
	}
	if s := cmd.String("os"); s != "" {
		opts = append(opts, recipe.WithOSRegistry(s))
	}
	if s := cmd.String("platform"); s != "" {
		opts = append(opts, recipe.WithPlatformRegistry(s))
	}
	if n := cmd.Int("nodes"); n > 0 {
		opts = append(opts, recipe.WithNodesRegistry(n))
	}
	return recipe.BuildCriteriaWithRegistry(reg, opts...)
}

func TestBuildCriteriaFromCmd(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError bool
		errMsg    string
		validate  func(*testing.T, *recipe.Criteria)
	}{
		{
			name: "valid service",
			args: []string{"cmd", "--service", "eks"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceEKS {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceEKS)
				}
			},
		},
		{
			name:      "invalid service",
			args:      []string{"cmd", "--service", "invalid-service"},
			wantError: true,
			errMsg:    "invalid service type",
		},
		{
			name: "valid accelerator",
			args: []string{"cmd", "--accelerator", "h100"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Accelerator != recipe.CriteriaAcceleratorH100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorH100)
				}
			},
		},
		{
			name: "valid accelerator with gpu alias",
			args: []string{"cmd", "--gpu", "gb200"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Accelerator != recipe.CriteriaAcceleratorGB200 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorGB200)
				}
			},
		},
		{
			name:      "invalid accelerator",
			args:      []string{"cmd", "--accelerator", "invalid-gpu"},
			wantError: true,
			errMsg:    "invalid accelerator type",
		},
		{
			name: "valid intent",
			args: []string{"cmd", "--intent", "training"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Intent != recipe.CriteriaIntentTraining {
					t.Errorf("Intent = %v, want %v", c.Intent, recipe.CriteriaIntentTraining)
				}
			},
		},
		{
			name:      "invalid intent",
			args:      []string{"cmd", "--intent", "invalid-intent"},
			wantError: true,
			errMsg:    "invalid intent type",
		},
		{
			name: "valid os",
			args: []string{"cmd", "--os", "ubuntu"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.OS != recipe.CriteriaOSUbuntu {
					t.Errorf("OS = %v, want %v", c.OS, recipe.CriteriaOSUbuntu)
				}
			},
		},
		{
			name:      "invalid os",
			args:      []string{"cmd", "--os", "invalid-os"},
			wantError: true,
			errMsg:    "invalid os type",
		},
		{
			name: "valid nodes",
			args: []string{"cmd", "--nodes", "8"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Nodes != 8 {
					t.Errorf("Nodes = %v, want 8", c.Nodes)
				}
			},
		},
		{
			name: "complete criteria",
			args: []string{
				"cmd",
				"--service", "gke",
				"--accelerator", "a100",
				"--intent", "inference",
				"--os", "cos",
				"--nodes", "16",
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceGKE {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceGKE)
				}
				if c.Accelerator != recipe.CriteriaAcceleratorA100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorA100)
				}
				if c.Intent != recipe.CriteriaIntentInference {
					t.Errorf("Intent = %v, want %v", c.Intent, recipe.CriteriaIntentInference)
				}
				if c.OS != recipe.CriteriaOSCOS {
					t.Errorf("OS = %v, want %v", c.OS, recipe.CriteriaOSCOS)
				}
				if c.Nodes != 16 {
					t.Errorf("Nodes = %v, want 16", c.Nodes)
				}
			},
		},
		{
			name: "empty criteria is valid",
			args: []string{"cmd"},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c == nil {
					t.Error("expected non-nil criteria")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedCriteria *recipe.Criteria
			var capturedErr error

			testCmd := &cli.Command{
				Name: "test",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "service"},
					&cli.StringFlag{Name: "accelerator", Aliases: []string{"gpu"}},
					&cli.StringFlag{Name: "intent"},
					&cli.StringFlag{Name: "os"},
					&cli.IntFlag{Name: "nodes"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					capturedCriteria, capturedErr = buildCriteriaFromCmd(cmd, recipe.NewCriteriaRegistry())
					return capturedErr
				},
			}

			err := testCmd.Run(context.Background(), tt.args)

			if tt.wantError {
				if err == nil && capturedErr == nil {
					t.Error("expected error but got nil")
					return
				}
				errToCheck := err
				if capturedErr != nil {
					errToCheck = capturedErr
				}
				if tt.errMsg != "" && !strings.Contains(errToCheck.Error(), tt.errMsg) {
					t.Errorf("error = %v, want error containing %v", errToCheck, tt.errMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if capturedErr != nil {
				t.Errorf("unexpected captured error: %v", capturedErr)
				return
			}

			if capturedCriteria == nil {
				t.Error("expected non-nil criteria")
				return
			}

			if tt.validate != nil {
				tt.validate(t, capturedCriteria)
			}
		})
	}
}

func TestExtractCriteriaFromSnapshot(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *snapshotter.Snapshot
		validate func(*testing.T, *recipe.Criteria)
	}{
		{
			name:     "nil snapshot",
			snapshot: nil,
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c == nil {
					t.Error("expected non-nil criteria")
				}
			},
		},
		{
			name: "empty snapshot",
			snapshot: &snapshotter.Snapshot{
				Measurements: nil,
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c == nil {
					t.Error("expected non-nil criteria")
				}
			},
		},
		{
			name: "snapshot with K8s service",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: "K8s",
						Subtypes: []measurement.Subtype{
							{
								Name: "node",
								Data: map[string]measurement.Reading{
									"provider": measurement.Str("eks"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceEKS {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceEKS)
				}
			},
		},
		{
			name: "snapshot with GPU H100",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: "GPU",
						Subtypes: []measurement.Subtype{
							{
								Name: "smi",
								Data: map[string]measurement.Reading{
									"gpu.model": measurement.Str("NVIDIA H100 80GB HBM3"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Accelerator != recipe.CriteriaAcceleratorH100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorH100)
				}
			},
		},
		{
			name: "snapshot with GB200",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: "GPU",
						Subtypes: []measurement.Subtype{
							{
								Name: "smi",
								Data: map[string]measurement.Reading{
									"gpu.model": measurement.Str("NVIDIA GB200"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Accelerator != recipe.CriteriaAcceleratorGB200 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorGB200)
				}
			},
		},
		{
			name: "snapshot with OS ubuntu",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: "OS",
						Subtypes: []measurement.Subtype{
							{
								Name: "release",
								Data: map[string]measurement.Reading{
									"ID": measurement.Str("ubuntu"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.OS != recipe.CriteriaOSUbuntu {
					t.Errorf("OS = %v, want %v", c.OS, recipe.CriteriaOSUbuntu)
				}
			},
		},
		{
			name: "complete snapshot",
			snapshot: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: "K8s",
						Subtypes: []measurement.Subtype{
							{
								Name: "node",
								Data: map[string]measurement.Reading{
									"provider": measurement.Str("gke"),
								},
							},
						},
					},
					{
						Type: "GPU",
						Subtypes: []measurement.Subtype{
							{
								Name: "smi",
								Data: map[string]measurement.Reading{
									"gpu.model": measurement.Str("NVIDIA A100-SXM4-80GB"),
								},
							},
						},
					},
					{
						Type: "OS",
						Subtypes: []measurement.Subtype{
							{
								Name: "release",
								Data: map[string]measurement.Reading{
									"ID": measurement.Str("rhel"),
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceGKE {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceGKE)
				}
				if c.Accelerator != recipe.CriteriaAcceleratorA100 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorA100)
				}
				if c.OS != recipe.CriteriaOSRHEL {
					t.Errorf("OS = %v, want %v", c.OS, recipe.CriteriaOSRHEL)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var measurements []*measurement.Measurement
			if tt.snapshot != nil {
				measurements = tt.snapshot.Measurements
			}
			criteria := fingerprint.FromMeasurements(measurements).ToCriteria(nil)

			if tt.validate != nil {
				tt.validate(t, criteria)
			}
		})
	}
}

func TestApplyCriteriaOverrides(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		initial  *recipe.Criteria
		validate func(*testing.T, *recipe.Criteria)
		wantErr  bool
	}{
		{
			name:    "override service",
			args:    []string{"cmd", "--service", "aks"},
			initial: &recipe.Criteria{Service: recipe.CriteriaServiceEKS},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceAKS {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceAKS)
				}
			},
		},
		{
			name:    "override accelerator",
			args:    []string{"cmd", "--accelerator", "l40"},
			initial: &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Accelerator != recipe.CriteriaAcceleratorL40 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorL40)
				}
			},
		},
		{
			name:    "override intent",
			args:    []string{"cmd", "--intent", "inference"},
			initial: &recipe.Criteria{Intent: recipe.CriteriaIntentTraining},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Intent != recipe.CriteriaIntentInference {
					t.Errorf("Intent = %v, want %v", c.Intent, recipe.CriteriaIntentInference)
				}
			},
		},
		{
			name:    "override os",
			args:    []string{"cmd", "--os", "rhel"},
			initial: &recipe.Criteria{OS: recipe.CriteriaOSUbuntu},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.OS != recipe.CriteriaOSRHEL {
					t.Errorf("OS = %v, want %v", c.OS, recipe.CriteriaOSRHEL)
				}
			},
		},
		{
			name:    "override platform",
			args:    []string{"cmd", "--platform", "kubeflow"},
			initial: &recipe.Criteria{Platform: recipe.CriteriaPlatformDynamo},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Platform != recipe.CriteriaPlatformKubeflow {
					t.Errorf("Platform = %v, want %v", c.Platform, recipe.CriteriaPlatformKubeflow)
				}
			},
		},
		{
			name:    "override nodes",
			args:    []string{"cmd", "--nodes", "16"},
			initial: &recipe.Criteria{Nodes: 4},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Nodes != 16 {
					t.Errorf("Nodes = %v, want 16", c.Nodes)
				}
			},
		},
		{
			name:    "same service value no change",
			args:    []string{"cmd", "--service", "eks"},
			initial: &recipe.Criteria{Service: recipe.CriteriaServiceEKS},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceEKS {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceEKS)
				}
			},
		},
		{
			name:    "set on empty criteria",
			args:    []string{"cmd", "--intent", "training", "--os", "ubuntu", "--nodes", "8"},
			initial: &recipe.Criteria{},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Intent != recipe.CriteriaIntentTraining {
					t.Errorf("Intent = %v, want %v", c.Intent, recipe.CriteriaIntentTraining)
				}
				if c.OS != recipe.CriteriaOSUbuntu {
					t.Errorf("OS = %v, want %v", c.OS, recipe.CriteriaOSUbuntu)
				}
				if c.Nodes != 8 {
					t.Errorf("Nodes = %v, want 8", c.Nodes)
				}
			},
		},
		{
			name:    "no overrides preserves existing",
			args:    []string{"cmd"},
			initial: &recipe.Criteria{Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorGB200},
			validate: func(t *testing.T, c *recipe.Criteria) {
				if c.Service != recipe.CriteriaServiceGKE {
					t.Errorf("Service = %v, want %v", c.Service, recipe.CriteriaServiceGKE)
				}
				if c.Accelerator != recipe.CriteriaAcceleratorGB200 {
					t.Errorf("Accelerator = %v, want %v", c.Accelerator, recipe.CriteriaAcceleratorGB200)
				}
			},
		},
		{
			name:    "invalid service returns error",
			args:    []string{"cmd", "--service", "invalid"},
			initial: &recipe.Criteria{},
			wantErr: true,
		},
		{
			name:    "invalid accelerator returns error",
			args:    []string{"cmd", "--accelerator", "invalid"},
			initial: &recipe.Criteria{},
			wantErr: true,
		},
		{
			name:    "invalid intent returns error",
			args:    []string{"cmd", "--intent", "invalid"},
			initial: &recipe.Criteria{},
			wantErr: true,
		},
		{
			name:    "invalid os returns error",
			args:    []string{"cmd", "--os", "invalid"},
			initial: &recipe.Criteria{},
			wantErr: true,
		},
		{
			name:    "invalid platform returns error",
			args:    []string{"cmd", "--platform", "invalid"},
			initial: &recipe.Criteria{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCmd := &cli.Command{
				Name: "test",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "service"},
					&cli.StringFlag{Name: "accelerator", Aliases: []string{"gpu"}},
					&cli.StringFlag{Name: "intent"},
					&cli.StringFlag{Name: "os"},
					&cli.StringFlag{Name: "platform"},
					&cli.IntFlag{Name: "nodes"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return applyCriteriaOverrides(cmd, tt.initial, recipe.NewCriteriaRegistry())
				},
			}

			err := testCmd.Run(context.Background(), tt.args)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if tt.validate != nil {
				tt.validate(t, tt.initial)
			}
		})
	}
}

func TestRecipeCmd_CommandStructure(t *testing.T) {
	cmd := recipeCmd()

	if cmd.Name != "recipe" {
		t.Errorf("Name = %v, want recipe", cmd.Name)
	}

	if cmd.Usage == "" {
		t.Error("Usage should not be empty")
	}

	if cmd.Description == "" {
		t.Error("Description should not be empty")
	}

	requiredFlags := []string{"service", "accelerator", "intent", "os", "nodes", "snapshot", "output", "format"}
	for _, flagName := range requiredFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasName(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("required flag %q not found", flagName)
		}
	}

	if cmd.Action == nil {
		t.Error("Action should not be nil")
	}
}

func TestRecipeCmd_NoCriteriaValidation(t *testing.T) {
	cmd := recipeCmd()

	// Run the recipe command with no criteria flags and no snapshot
	err := cmd.Run(context.Background(), []string{"recipe"})

	if err == nil {
		t.Error("expected error when no criteria provided, got nil")
		return
	}

	expectedMsg := "no criteria provided"
	if !strings.Contains(err.Error(), expectedMsg) {
		t.Errorf("error = %v, want error containing %q", err, expectedMsg)
	}
}

func TestSnapshotCmd_CommandStructure(t *testing.T) {
	cmd := snapshotCmd()

	if cmd.Name != "snapshot" {
		t.Errorf("Name = %v, want snapshot", cmd.Name)
	}

	if cmd.Usage == "" {
		t.Error("Usage should not be empty")
	}

	if cmd.Description == "" {
		t.Error("Description should not be empty")
	}

	requiredFlags := []string{"output", "format"}
	for _, flagName := range requiredFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasName(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("required flag %q not found", flagName)
		}
	}

	if cmd.Action == nil {
		t.Error("Action should not be nil")
	}
}

func hasName(flag cli.Flag, name string) bool {
	if flag == nil {
		return false
	}
	return slices.Contains(flag.Names(), name)
}

func TestRecipeCmd_HasDataFlag(t *testing.T) {
	cmd := recipeCmd()

	found := false
	for _, flag := range cmd.Flags {
		if hasName(flag, "data") {
			found = true
			break
		}
	}

	if !found {
		t.Error("recipe command should have --data flag")
	}
}

func TestRecipeClientFromCmd_EmptyPath(t *testing.T) {
	// With no --data flag, recipeClientFromCmd constructs an embedded-source
	// Client (no error). This is the post-migration replacement for the old
	// empty-path data-provider no-op.
	testCmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client, err := recipeClientFromCmd(cmd, nil)
			if err != nil {
				return err
			}
			return client.Close()
		},
	}

	err := testCmd.Run(context.Background(), []string{"test"})
	if err != nil {
		t.Errorf("expected no error with empty --data flag, got: %v", err)
	}
}

func TestRecipeClientFromCmd_InvalidPath(t *testing.T) {
	testCmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client, err := recipeClientFromCmd(cmd, nil)
			if err == nil {
				_ = client.Close()
			}
			return err
		},
	}

	// A --data pointing at a non-existent directory must fail Client
	// construction (the layered FilesystemSource provider validates the dir).
	err := testCmd.Run(context.Background(), []string{"test", "--data", "/non/existent/path"})
	if err == nil {
		t.Error("expected error with non-existent path")
	}
}

func TestRecipeClientFromCmd_MissingRegistry(t *testing.T) {
	// A --data directory without registry.yaml must fail Client construction.
	tmpDir := t.TempDir()

	testCmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client, err := recipeClientFromCmd(cmd, nil)
			if err == nil {
				_ = client.Close()
			}
			return err
		},
	}

	err := testCmd.Run(context.Background(), []string{"test", "--data", tmpDir})
	if err == nil {
		t.Error("expected error when registry.yaml is missing")
	}
	if !strings.Contains(err.Error(), "registry.yaml") {
		t.Errorf("error should mention registry.yaml, got: %v", err)
	}
}

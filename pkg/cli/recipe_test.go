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

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
)

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
					capturedCriteria, capturedErr = buildCriteriaFromCmd(cmd)
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
								Name: "server",
								Data: map[string]measurement.Reading{
									"service": measurement.Str("eks"),
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
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA H100 80GB HBM3"),
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
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("NVIDIA GB200"),
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
								Name: "server",
								Data: map[string]measurement.Reading{
									"service": measurement.Str("gke"),
								},
							},
						},
					},
					{
						Type: "GPU",
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("A100-SXM4-80GB"),
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
			criteria := recipe.ExtractCriteriaFromSnapshot(tt.snapshot)

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
			name:    "invalid override returns error",
			args:    []string{"cmd", "--service", "invalid"},
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
					&cli.IntFlag{Name: "nodes"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return applyCriteriaOverrides(cmd, tt.initial)
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

func TestCommandLister(_ *testing.T) {
	commandLister(context.Background(), nil)

	cmd := &cli.Command{Name: "test"}
	commandLister(context.Background(), cmd)

	rootCmd := &cli.Command{
		Name: "root",
		Commands: []*cli.Command{
			{Name: "visible1", Hidden: false},
			{Name: "hidden", Hidden: true},
			{Name: "visible2", Hidden: false},
		},
	}
	commandLister(context.Background(), rootCmd)
}

func hasName(flag cli.Flag, name string) bool {
	if flag == nil {
		return false
	}
	names := flag.Names()
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
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

func TestInitDataProvider_EmptyPath(t *testing.T) {
	// Create a minimal command with just the data flag
	testCmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return initDataProvider(cmd)
		},
	}

	// Run with no --data flag (should succeed with no-op)
	err := testCmd.Run(context.Background(), []string{"test"})
	if err != nil {
		t.Errorf("expected no error with empty --data flag, got: %v", err)
	}
}

func TestInitDataProvider_InvalidPath(t *testing.T) {
	testCmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return initDataProvider(cmd)
		},
	}

	// Run with non-existent path
	err := testCmd.Run(context.Background(), []string{"test", "--data", "/non/existent/path"})
	if err == nil {
		t.Error("expected error with non-existent path")
	}
}

func TestInitDataProvider_MissingRegistry(t *testing.T) {
	// Create temp directory without registry.yaml
	tmpDir := t.TempDir()

	testCmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "data"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return initDataProvider(cmd)
		},
	}

	// Run with directory that has no registry.yaml
	err := testCmd.Run(context.Background(), []string{"test", "--data", tmpDir})
	if err == nil {
		t.Error("expected error when registry.yaml is missing")
	}
	if !strings.Contains(err.Error(), "registry.yaml") {
		t.Errorf("error should mention registry.yaml, got: %v", err)
	}
}

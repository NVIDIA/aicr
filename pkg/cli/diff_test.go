// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestDiffCmd_CommandStructure(t *testing.T) {
	cmd := diffCmd()

	if cmd.Name != "diff" {
		t.Errorf("command name = %q, want %q", cmd.Name, "diff")
	}

	if cmd.Category != functionalCategoryName {
		t.Errorf("category = %q, want %q", cmd.Category, functionalCategoryName)
	}
}

func TestDiffCmd_RecipeModeFlags(t *testing.T) {
	cmd := diffCmd()

	recipeFlags := []string{"recipe", "snapshot"}
	for _, flagName := range recipeFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing recipe mode flag: %s", flagName)
		}
	}

	// Check aliases
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "recipe") && !hasFlag(flag, "r") {
			t.Error("--recipe flag missing -r alias")
		}
		if hasFlag(flag, "snapshot") && !hasFlag(flag, "s") {
			t.Error("--snapshot flag missing -s alias")
		}
	}
}

func TestDiffCmd_SnapshotModeFlags(t *testing.T) {
	cmd := diffCmd()

	snapshotFlags := []string{"baseline", "target"}
	for _, flagName := range snapshotFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing snapshot mode flag: %s", flagName)
		}
	}
}

func TestDiffCmd_CommonFlags(t *testing.T) {
	cmd := diffCmd()

	commonFlags := []string{"fail-on-drift", "output", "format", "kubeconfig", "data"}
	for _, flagName := range commonFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing common flag: %s", flagName)
		}
	}
}

func TestDiffCmd_ModeValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		errContain string
	}{
		{
			name:       "no flags",
			args:       []string{"aicr", "diff"},
			wantErr:    true,
			errContain: "specify either",
		},
		{
			name:       "mixed modes",
			args:       []string{"aicr", "diff", "--recipe", "r.yaml", "--baseline", "b.yaml"},
			wantErr:    true,
			errContain: "cannot mix",
		},
		{
			name:       "recipe without snapshot",
			args:       []string{"aicr", "diff", "--recipe", "r.yaml"},
			wantErr:    true,
			errContain: "--snapshot is required",
		},
		{
			name:       "snapshot without recipe",
			args:       []string{"aicr", "diff", "--snapshot", "s.yaml"},
			wantErr:    true,
			errContain: "--recipe is required",
		},
		{
			name:       "baseline without target",
			args:       []string{"aicr", "diff", "--baseline", "b.yaml"},
			wantErr:    true,
			errContain: "--target is required",
		},
		{
			name:       "target without baseline",
			args:       []string{"aicr", "diff", "--target", "t.yaml"},
			wantErr:    true,
			errContain: "--baseline is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := diffCmd()
			app := &cli.Command{
				Name:     "aicr",
				Commands: []*cli.Command{cmd},
			}
			err := app.Run(t.Context(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
		})
	}
}

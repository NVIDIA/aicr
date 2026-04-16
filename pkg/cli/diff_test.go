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

func TestDiffCmd_Flags(t *testing.T) {
	cmd := diffCmd()

	requiredFlags := []string{"baseline", "target", "fail-on-drift", "output", "format", "kubeconfig", "data"}
	for _, flagName := range requiredFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing flag: %s", flagName)
		}
	}

	// Check baseline alias
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "baseline") && !hasFlag(flag, "b") {
			t.Error("--baseline flag missing -b alias")
		}
	}
}

func TestDiffCmd_Validation(t *testing.T) {
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
			errContain: "--baseline is required",
		},
		{
			name:       "baseline without target",
			args:       []string{"aicr", "diff", "--baseline", "b.yaml"},
			wantErr:    true,
			errContain: "--target is required",
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

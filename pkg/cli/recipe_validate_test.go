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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

func TestRecipeValidateJSONFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("kind: [invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := &cli.Command{Name: "aicr", Commands: []*cli.Command{recipeValidateCmd()}, Writer: &output}
	err := cmd.Run(context.Background(), []string{"aicr", "validate", "--format", "json", path})
	if err == nil {
		t.Fatal("expected validation failure")
	}
	var diagnostics []aicr.RecipeDiagnostic
	if err := json.Unmarshal(output.Bytes(), &diagnostics); err != nil {
		t.Fatalf("invalid JSON: %v; output %q", err, output.String())
	}
	if len(diagnostics) == 0 || diagnostics[0].Severity != "error" || diagnostics[0].File == "" {
		t.Fatalf("expected file error diagnostic, got %+v", diagnostics)
	}
}

func TestRecipeValidateArguments(t *testing.T) {
	for _, args := range [][]string{
		{"aicr", "validate"},
		{"aicr", "validate", "--format", "xml", "input.yaml"},
		{"aicr", "validate", "one.yaml", "two.yaml"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			cmd := &cli.Command{Name: "aicr", Commands: []*cli.Command{recipeValidateCmd()}, Writer: &bytes.Buffer{}}
			if err := cmd.Run(context.Background(), args); err == nil {
				t.Fatalf("expected argument error for %v", args)
			}
		})
	}
}

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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestWriteQueryResult(t *testing.T) {
	tests := []struct {
		name     string
		val      any
		format   serializer.Format
		contains string
	}{
		{
			name:     "string yaml",
			val:      "570.86.16",
			format:   serializer.FormatYAML,
			contains: "570.86.16",
		},
		{
			name:     "string json is valid json",
			val:      "570.86.16",
			format:   serializer.FormatJSON,
			contains: `"570.86.16"`,
		},
		{
			name:     "bool yaml",
			val:      true,
			format:   serializer.FormatYAML,
			contains: "true",
		},
		{
			name:     "bool json is valid json",
			val:      true,
			format:   serializer.FormatJSON,
			contains: "true",
		},
		{
			name:     "int yaml",
			val:      42,
			format:   serializer.FormatYAML,
			contains: "42",
		},
		{
			name:     "int json is valid json",
			val:      42,
			format:   serializer.FormatJSON,
			contains: "42",
		},
		{
			name:     "float64 yaml",
			val:      3.14,
			format:   serializer.FormatYAML,
			contains: "3.14",
		},
		{
			name:     "map yaml",
			val:      map[string]any{"key": "value"},
			format:   serializer.FormatYAML,
			contains: "key: value",
		},
		{
			name:     "map json",
			val:      map[string]any{"key": "value"},
			format:   serializer.FormatJSON,
			contains: `"key": "value"`,
		},
		{
			name:     "slice yaml",
			val:      []string{"a", "b"},
			format:   serializer.FormatYAML,
			contains: "- a",
		},
		{
			name:     "slice json",
			val:      []string{"a", "b"},
			format:   serializer.FormatJSON,
			contains: `"a"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if writeErr := writeQueryResult(&buf, tt.val, tt.format); writeErr != nil {
				t.Fatalf("writeQueryResult returned error: %v", writeErr)
			}

			output := buf.String()

			if !strings.Contains(output, tt.contains) {
				t.Errorf("output %q does not contain %q", output, tt.contains)
			}
		})
	}
}

func TestQueryCmdFlagsExcludesOutput(t *testing.T) {
	flags := queryCmdFlags()
	for _, f := range flags {
		names := f.Names()
		for _, n := range names {
			if n == "output" {
				t.Error("queryCmdFlags should not include --output flag")
			}
		}
	}
}

func TestQueryCmdFlagsIncludesSelector(t *testing.T) {
	flags := queryCmdFlags()
	found := map[string]bool{}
	for _, f := range flags {
		names := f.Names()
		for _, n := range names {
			found[n] = true
		}
	}
	for _, name := range []string{"selector", "profile"} {
		if !found[name] {
			t.Errorf("queryCmdFlags must include --%s flag", name)
		}
	}
}

func TestQueryCmdRejectsRepeatedSlurmAccountingMode(t *testing.T) {
	err := queryCmd().Run(t.Context(), []string{
		"query",
		"--service", "eks",
		"--selector", "deploymentOrder",
		"--slurm-accounting-mode", "disabled",
		"--slurm-accounting-mode", "aicr-provided",
	})
	if err == nil || !strings.Contains(err.Error(),
		"flag --slurm-accounting-mode can only be specified once") {

		t.Fatalf("query command error = %v, want repeated accounting mode rejection", err)
	}
}

func TestRecipeAndQueryCommandsRejectExplicitEmptySlurmAccountingMode(t *testing.T) {
	tests := []struct {
		name string
		cmd  func() *cli.Command
		args []string
	}{
		{
			name: "recipe",
			cmd:  recipeCmd,
			args: []string{"recipe", "--service", "eks", "--slurm-accounting-mode="},
		},
		{
			name: "query",
			cmd:  queryCmd,
			args: []string{
				"query", "--service", "eks", "--selector", "deploymentOrder",
				"--slurm-accounting-mode=",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cmd().Run(t.Context(), tt.args)
			if err == nil || !strings.Contains(err.Error(), "invalid Slurm accounting mode") {
				t.Fatalf("%s command error = %v, want explicit empty accounting mode rejection",
					tt.name, err)
			}
		})
	}
}

func TestQueryCmdCriteriaStrictRejectsExternalCriteria(t *testing.T) {
	t.Setenv("AICR_CRITERIA_STRICT", "")
	dataDir := writeQueryExternalCriteriaCatalog(t)
	configPath := filepath.Join(t.TempDir(), "aicr-config.yaml")
	config := `apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: query-strict-test
spec:
  recipe:
    criteriaStrict: true
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name        string
		extraArgs   []string
		wantErrCode errors.ErrorCode
		wantOutput  string
	}{
		{
			name:       "non-strict query accepts external criterion",
			wantOutput: "external-query-service",
		},
		{
			name:        "CLI flag rejects external criterion",
			extraArgs:   []string{"--criteria-strict"},
			wantErrCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:        "config rejects external criterion",
			extraArgs:   []string{"--config", configPath},
			wantErrCode: errors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			parent := &cli.Command{
				Name:     "aicr",
				Commands: []*cli.Command{queryCmd()},
				Writer:   &output,
			}
			args := []string{
				"aicr", "query",
				"--data", dataDir,
				"--service", "external-query-service",
				"--selector", "criteria.service",
			}
			args = append(args, tt.extraArgs...)
			err := parent.Run(t.Context(), args)
			if tt.wantErrCode != "" {
				if !stderrors.Is(err, errors.New(tt.wantErrCode, "")) {
					t.Fatalf("query error = %v, want code %s", err, tt.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("query error = %v", err)
			}
			if !strings.Contains(output.String(), tt.wantOutput) {
				t.Fatalf("query output = %q, want %q", output.String(), tt.wantOutput)
			}
		})
	}
}

func writeQueryExternalCriteriaCatalog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(registry), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	overlaysDir := filepath.Join(dir, "overlays")
	if err := os.MkdirAll(overlaysDir, 0o755); err != nil {
		t.Fatalf("create overlays directory: %v", err)
	}
	overlay := `apiVersion: aicr.run/v1alpha2
kind: RecipeMetadata
metadata:
  name: external-query
spec:
  criteria:
    service: external-query-service
  componentRefs: []
`
	if err := os.WriteFile(filepath.Join(overlaysDir, "external-query.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	return dir
}

// TestRecipeAndQueryCommandsRejectInvalidRuntimeInventoryMode exercises
// runtimeInventoryResolveOptions' flag-set branch on both commands that expose
// the flag.
//
// Without this the function is covered only by its "flag unset and no config"
// fallthrough, which returns before touching the value — so a parse or wiring
// defect in the branch operators actually use would not be observed.
func TestRecipeAndQueryCommandsRejectInvalidRuntimeInventoryMode(t *testing.T) {
	tests := []struct {
		name string
		cmd  func() *cli.Command
		args []string
	}{
		{
			name: "recipe",
			cmd:  recipeCmd,
			args: []string{"recipe", "--service", "eks", "--runtime-inventory", "off"},
		},
		{
			name: "query",
			cmd:  queryCmd,
			args: []string{
				"query", "--service", "eks", "--selector", "deploymentOrder",
				"--runtime-inventory", "off",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cmd().Run(t.Context(), tt.args)
			if err == nil {
				t.Fatal("command error = nil, want rejection of an invalid runtime inventory mode")
			}
			// Assert the code, since that is what callers branch on; a text
			// match alone would accept an unrelated error carrying similar
			// wording. The message check stays to distinguish which
			// invalid-request this is.
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("command error = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), "invalid runtime inventory mode") {
				t.Fatalf("command error = %v, want an invalid-mode rejection", err)
			}
		})
	}
}

// TestRecipeCommandRejectsRuntimeInventoryWithoutComponent covers the flag-set
// branch reaching a successful parse and then failing closed at build time,
// which is the path an operator hits after a typo in --service.
//
// The criteria must name a recipe that does not declare k8s-aibom. Inference on
// gke/h100/cos is the stock-adoption target under ADR-019's amendment and now
// declares it, so this asserts against a training recipe instead. Mirrors the
// same precondition in pkg/client/v1's TestResolveRecipeRuntimeInventoryMode.
func TestRecipeCommandRejectsRuntimeInventoryWithoutComponent(t *testing.T) {
	err := recipeCmd().Run(t.Context(), []string{
		"recipe", "--service", "gke", "--accelerator", "h100",
		"--os", "cos", "--intent", "training",
		"--runtime-inventory", "disabled",
	})
	if err == nil {
		t.Fatal("command error = nil, want rejection for a recipe that does not declare " +
			"the component; if these criteria now declare k8s-aibom, pick criteria that " +
			"do not rather than relaxing this assertion")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("command error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "requires the recipe to declare component") {
		t.Fatalf("command error = %v, want the missing-component rejection", err)
	}
}

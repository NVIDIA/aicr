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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	evidenceindex "github.com/NVIDIA/aicr/pkg/evidence/index"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
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
	for _, name := range []string{"selector", "profile", "summary", "recipe", "r"} {
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
	config := `apiVersion: aicr.run/v1beta1
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
				if errorstest.ReportedCode(err) != tt.wantErrCode {
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
	overlay := `apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata:
  name: external-query
spec:
  criteria:
    service: external-query-service
  componentRefs: []
`
	return providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":                []byte(providertest.EmptyRegistryYAML),
		"overlays/external-query.yaml": []byte(overlay),
	})
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
			if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
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
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Errorf("command error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "requires the recipe to declare component") {
		t.Fatalf("command error = %v, want the missing-component rejection", err)
	}
}

// TestRecipeAndQueryRejectRepeatedGKETCPXOInterfaces covers the single-value
// contract for the required mapping: urfave/cli keeps the last repeated value
// silently, so the flag must be in each command's validateSingleValueFlags
// list (CodeRabbit PR #2705).
func TestRecipeAndQueryRejectRepeatedGKETCPXOInterfaces(t *testing.T) {
	mapping := "eth1=gpu-nic-0,eth2=gpu-nic-1,eth3=gpu-nic-2,eth4=gpu-nic-3," +
		"eth5=gpu-nic-4,eth6=gpu-nic-5,eth7=gpu-nic-6,eth8=gpu-nic-7"

	tests := []struct {
		name string
		cmd  func() *cli.Command
		args []string
	}{
		{
			name: "recipe",
			cmd:  recipeCmd,
			args: []string{"recipe", "--service", "gke", "--accelerator", "h100",
				"--os", "cos", "--intent", "training", "--platform", "kubeflow",
				"--gke-tcpxo-interfaces", mapping, "--gke-tcpxo-interfaces", mapping},
		},
		{
			name: "query",
			cmd:  queryCmd,
			args: []string{"query", "--service", "gke", "--selector", "deploymentOrder",
				"--gke-tcpxo-interfaces", mapping, "--gke-tcpxo-interfaces", mapping},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cmd().Run(t.Context(), tt.args)
			if err == nil || !strings.Contains(err.Error(),
				"flag --gke-tcpxo-interfaces can only be specified once") {

				t.Fatalf("%s error = %v, want repeated flag rejection", tt.name, err)
			}
		})
	}
}

// priorRecipeFile writes a hydrated recipe naming one component at the
// namespace a previous resolution installed it into. The source and version
// fields also let tests distinguish full inheritance from namespace-only
// inheritance while satisfying the loader's coherence rules.
func priorRecipeFile(t *testing.T, path, component, namespace string) string {
	t.Helper()
	doc := fmt.Sprintf(`kind: RecipeResult
apiVersion: aicr.run/v1
metadata:
  version: test
componentRefs:
  - name: %s
    type: Helm
    source: https://charts.invalid/prior
    version: 1.0.0
    namespace: %s
`, component, namespace)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write prior recipe: %v", err)
	}
	return path
}

// runQueryValue runs `aicr query` and returns what it printed. The command is
// its own root here, so cmd.Writer is the cmd.Root().Writer the action writes
// the selected value to.
func runQueryValue(t *testing.T, args ...string) string {
	t.Helper()
	cmd := queryCmd()
	var out bytes.Buffer
	cmd.Writer = &out
	if err := cmd.Run(t.Context(), args); err != nil {
		t.Fatalf("query %v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

// TestQueryCmd_InheritFrom proves --inherit-from reaches the resolver on BOTH
// paths buildRecipeFromCmdWithConfig routes through: the criteria-only
// resolver and the snapshot one. The flag is declared on the recipe flag
// block, which queryCmdFlags reuses; query is the command that can read the
// result back, because it prints the selected value to cmd.Root().Writer
// while recipe writes its document straight to a file.
func TestQueryCmd_InheritFrom(t *testing.T) {
	const (
		component      = "gpu-operator"
		movedNamespace = "legacy-gpu-operator"
	)
	selector := "components." + component + ".namespace"

	dir := t.TempDir()
	prior := priorRecipeFile(t, filepath.Join(dir, "prior.yaml"), component, movedNamespace)
	snapFile := filepath.Join(dir, "snapshot.yaml")
	// v1.34.0 clears the strictest readiness constraint on the criteria below.
	writeSnapshotWithVersion(t, snapFile, "v1.34.0")

	tests := []struct {
		name  string
		extra []string
	}{
		{name: "criteria path"},
		{name: "snapshot path", extra: []string{"--snapshot", snapFile}},
		{name: "namespace criteria path", extra: []string{"--inherit=namespace"}},
		{name: "namespace snapshot path", extra: []string{"--snapshot", snapFile, "--inherit=namespace"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := slices.Concat([]string{
				"query", "--selector", selector,
				"--service", "eks", "--accelerator", "h100",
				"--intent", "training",
			}, tt.extra)

			// Baseline first: the registry default has to differ from the
			// inherited value, or the assertion below would hold either way.
			baseArgs := slices.DeleteFunc(slices.Clone(args), func(arg string) bool {
				return strings.HasPrefix(arg, "--inherit=")
			})
			base := runQueryValue(t, baseArgs...)
			if base == movedNamespace {
				t.Fatalf("setup: %s already resolves to %q, so the test would assert nothing",
					component, movedNamespace)
			}

			got := runQueryValue(t, slices.Concat(args, []string{"--inherit-from", prior})...)
			if got != movedNamespace {
				t.Errorf("%s = %q, want the inherited %q (default is %q); --inherit-from did not reach the resolver",
					selector, got, movedNamespace, base)
			}
		})
	}
}

func TestInheritanceSelectionCLIRejects(t *testing.T) {
	commands := []struct {
		name   string
		newCmd func() *cli.Command
		args   []string
	}{
		{"recipe", recipeCmd, []string{"recipe", "--service", "eks"}},
		{"query", queryCmd, []string{"query", "--service", "eks", "--selector", "deploymentOrder"}},
		{"mirror", mirrorListCmd, []string{"list", "--service", "eks"}},
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"namespace without source", []string{"--inherit=namespace"}, "requires --inherit-from"},
		{"all without source", []string{"--inherit=all"}, "requires --inherit-from"},
		{"invalid mode", []string{"--inherit-from=missing.yaml", "--inherit=namespaces"}, "invalid inherit mode"},
		{"empty mode", []string{"--inherit-from=missing.yaml", "--inherit="}, "invalid inherit mode"},
		{"repeated mode", []string{"--inherit=namespace", "--inherit=all"}, "flag --inherit can only be specified once"},
	}
	for _, command := range commands {
		for _, tt := range tests {
			t.Run(command.name+"/"+tt.name, func(t *testing.T) {
				err := command.newCmd().Run(t.Context(), slices.Concat(command.args, tt.args))
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
			})
		}
	}
}

func TestRecipeNamespaceInheritanceOutput(t *testing.T) {
	dir := t.TempDir()
	prior := priorRecipeFile(t, filepath.Join(dir, "prior.yaml"), "gpu-operator", "legacy-gpu-operator")
	output := filepath.Join(dir, "recipe.yaml")
	err := recipeCmd().Run(t.Context(), []string{"recipe", "--service", "eks", "--accelerator", "h100", "--intent", "training",
		"--inherit-from", prior, "--inherit=namespace", "--output", output})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc, []byte("namespace: legacy-gpu-operator")) ||
		bytes.Contains(doc, []byte("https://charts.invalid/prior")) {

		t.Fatalf("recipe must inherit the namespace and retain the current source:\n%s", doc)
	}
}

// TestRecipeAndQueryRejectRepeatedInheritFrom covers the single-value contract:
// urfave/cli silently keeps the last repeated value, so a second
// --inherit-from would pin namespaces from an artifact the operator did not
// think they had named.
func TestRecipeAndQueryRejectRepeatedInheritFrom(t *testing.T) {
	tests := []struct {
		name string
		cmd  func() *cli.Command
		args []string
	}{
		{
			name: "recipe",
			cmd:  recipeCmd,
			args: []string{"recipe", "--service", "eks",
				"--inherit-from", "a.yaml", "--inherit-from", "b.yaml"},
		},
		{
			name: "query",
			cmd:  queryCmd,
			args: []string{"query", "--service", "eks", "--selector", "deploymentOrder",
				"--inherit-from", "a.yaml", "--inherit-from", "b.yaml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cmd().Run(t.Context(), tt.args)
			if err == nil || !strings.Contains(err.Error(),
				"flag --inherit-from can only be specified once") {

				t.Fatalf("%s error = %v, want repeated flag rejection", tt.name, err)
			}
		})
	}
}

func TestQueryCmdSummary(t *testing.T) {
	for _, format := range []string{"", "table", "yaml", "json"} {
		t.Run("format="+format, func(t *testing.T) {
			args := []string{"query", "--service", "eks", "--accelerator", "h100", "--intent", "training", "--summary"}
			if format != "" {
				args = append(args, "--format", format)
			}
			output := runQueryValue(t, args...)
			if !strings.Contains(output, "gpu-operator") {
				t.Fatalf("summary missing component: %s", output)
			}
			if format == "" || format == "table" {
				for _, section := range []string{"RECIPE SUMMARY", "CRITERIA", "OVERLAYS", "CONSTRAINTS", "COMPONENTS", "VALIDATION"} {
					if !strings.Contains(output, section) {
						t.Errorf("summary missing %s: %s", section, output)
					}
				}
			} else if format == "json" && !json.Valid([]byte(output)) {
				t.Errorf("invalid JSON: %s", output)
			}
		})
	}
}

func TestQueryCmdFlagCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"neither selector nor summary", []string{"--service=eks"}, "one of --selector or --summary is required"},
		{"both selector and summary", []string{"--selector=criteria.service", "--summary"}, "mutually exclusive"},
		{"empty selector and summary", []string{"--selector=", "--summary"}, "mutually exclusive"},
		{"repeated recipe", []string{"--recipe=a.yaml", "--recipe=b.yaml", "--summary"}, "can only be specified once"},
		{"repeated selector", []string{"--selector=a", "--selector=b"}, "can only be specified once"},
		{"invalid format", []string{"--service=eks", "--summary", "--format=xml"}, "unknown output format"},
		{"empty recipe", []string{"--recipe=", "--summary"}, "recipe path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := queryCmd().Run(t.Context(), append([]string{"query"}, tt.args...))
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestQueryCmdRecipeFlagPartition fails when a query flag is neither rejected
// with --recipe (recipeGenerationFlags) nor listed here as valid with it, so a
// generation flag added to recipeCmdFlags cannot be silently ignored.
func TestQueryCmdRecipeFlagPartition(t *testing.T) {
	allowedWithRecipe := map[string]bool{
		cmdNameRecipe: true, flagSelector: true, flagSummary: true, flagFormat: true,
		"config": true, "data": true, "kubeconfig": true, "criteria-strict": true, flagRequireEvidence: true,
	}
	rejected := make(map[string]bool, len(recipeGenerationFlags))
	for _, name := range recipeGenerationFlags {
		rejected[name] = true
	}
	declared := map[string]bool{}
	for _, f := range queryCmdFlags() {
		name := f.Names()[0]
		declared[name] = true
		switch {
		case rejected[name] && allowedWithRecipe[name]:
			t.Errorf("--%s is both in recipeGenerationFlags and allowed with --recipe", name)
		case !rejected[name] && !allowedWithRecipe[name]:
			t.Errorf("--%s is new: add it to recipeGenerationFlags if it shapes the resolved recipe, "+
				"or to allowedWithRecipe in this test if it is valid with --recipe", name)
		}
	}
	for _, names := range []map[string]bool{rejected, allowedWithRecipe} {
		for name := range names {
			if !declared[name] {
				t.Errorf("--%s is classified but is not a query flag", name)
			}
		}
	}
}

func TestQueryCmdRecipeExcludesGenerationFlags(t *testing.T) {
	// Keep this list independent of recipeGenerationFlags so dropping an entry
	// from the implementation does not also drop its regression test.
	flags := []string{
		"--service=eks", "--accelerator=h100", "--intent=training", "--os=ubuntu", "--platform=kubeflow", "--nodes=1",
		"--snapshot=missing.yaml", "--profile=gpuStack=operator-managed", "--slurm-accounting-mode=disabled",
		"--runtime-inventory=disabled", "--gke-tcpxo-interfaces=eth1=gpu-nic-0", "--inherit-from=missing.yaml", "--inherit=namespace",
	}
	for _, flag := range flags {
		t.Run(flag, func(t *testing.T) {
			err := queryCmd().Run(t.Context(), []string{"query", "--recipe=missing.yaml", "--summary", flag})
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if err == nil || !strings.Contains(err.Error(), "cannot be combined with "+strings.Split(flag, "=")[0]) {
				t.Errorf("error = %v, want recipe/generation-input conflict", err)
			}
		})
	}
}

// queryRecipeFixture resolves with the real builder, without freezing shipped
// component versions in a fixture or test assertion.
func queryRecipeFixture(t *testing.T) (*aicr.Client, *recipe.RecipeResult, string) {
	t.Helper()
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.ResolveRecipeFromCriteria(t.Context(), aicr.WrapCriteria(&recipe.Criteria{
		Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100, Intent: recipe.CriteriaIntentTraining,
	}))
	if err != nil {
		t.Fatal(err)
	}
	resolved := result.Resolved()
	return client, resolved, writeQueryRecipeFixture(t, resolved)
}

func writeQueryRecipeFixture(t *testing.T, resolved *recipe.RecipeResult) string {
	t.Helper()
	data, err := serializer.MarshalYAMLDeterministic(resolved)
	if err != nil {
		t.Fatal(err)
	}
	return writeYAML(t, "recipe.yaml", string(data))
}

func TestQueryCmdRecipe(t *testing.T) {
	_, _, path := queryRecipeFixture(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	for _, source := range []string{path, server.URL + "/recipe.yaml"} {
		for _, selection := range [][]string{{"--summary"}, {"--selector=criteria.service"}, {"--selector="}} {
			t.Run(source+"/"+selection[0], func(t *testing.T) {
				output := runQueryValue(t, append([]string{"query", "-r", source}, selection...)...)
				want := "eks"
				switch selection[0] {
				case "--summary":
					want = "RECIPE SUMMARY"
				case "--selector=":
					want = "components:"
				}
				if !strings.Contains(output, want) {
					t.Errorf("output = %s, want %s", output, want)
				}
			})
		}
	}
	output := runQueryValue(t, "query", "--service=eks", "--selector=")
	if !strings.Contains(output, "components:") || !strings.Contains(output, "criteria:") {
		t.Errorf("empty selector must return the whole recipe: %s", output)
	}
}

func TestQueryCmdRecipeLoaderErrors(t *testing.T) {
	client, _, _ := queryRecipeFixture(t)
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.yaml"), writeYAML(t, "bad.yaml", "invalid: [")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			_, loaderErr := client.LoadRecipe(t.Context(), path, "")
			if loaderErr == nil {
				t.Fatal("setup: loader must reject the input")
			}
			err := queryCmd().Run(t.Context(), []string{"query", "--recipe", path, "--summary"})
			errorstest.WantReportedCode(t, err, errorstest.ReportedCode(loaderErr))
		})
	}
}

// TestQueryCmdRecipeRejectsSavedSummary proves a saved summary is not a
// recipe: its own kind fails the loader's kind gate before the body is
// decoded, so it neither loads as an empty recipe nor fails with a type error.
func TestQueryCmdRecipeRejectsSavedSummary(t *testing.T) {
	_, resolved, path := queryRecipeFixture(t)
	if resolved.Validation == nil {
		t.Fatal("setup: the fixture recipe must declare validation checks")
	}
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			summary := runQueryValue(t, "query", "--recipe", path, "--summary", "--format="+format)
			if !strings.Contains(summary, "validation") {
				t.Fatalf("setup: summary must carry validation phases: %s", summary)
			}
			saved := writeYAML(t, "summary."+format, summary)
			for _, selection := range []string{"--summary", "--selector=components"} {
				err := queryCmd().Run(t.Context(), []string{"query", "--recipe", saved, selection})
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				if err == nil || !strings.Contains(err.Error(), `kind "`+recipe.RecipeSummaryKind+`"`) {
					t.Errorf("%s: error = %v, want rejection naming kind %q", selection, err, recipe.RecipeSummaryKind)
				}
			}
		})
	}
}

func TestQueryCmdUnprofiledSummarySkipsSelectorHydration(t *testing.T) {
	_, resolved, _ := queryRecipeFixture(t)
	resolved.ComponentRefs[0].ValuesFile = "components/missing-values-for-summary-test.yaml"
	path := writeQueryRecipeFixture(t, resolved)
	if output := runQueryValue(t, "query", "--recipe", path, "--summary"); !strings.Contains(output, "COMPONENTS") {
		t.Errorf("summary = %s", output)
	}
	err := queryCmd().Run(t.Context(), []string{"query", "--recipe", path, "--selector=components"})
	if err == nil {
		t.Fatal("selector must hydrate values and reject the missing values file")
	}
}

func TestQueryCmdSummaryOmitsDisabledComponents(t *testing.T) {
	_, resolved, _ := queryRecipeFixture(t)
	disabled := map[string]string{"cert-manager": "enabled", "nfd": "install"}
	for name, key := range disabled {
		ref := resolved.GetComponentRef(name)
		if ref == nil {
			t.Fatalf("setup: recipe must contain %s", name)
		}
		if ref.Overrides == nil {
			ref.Overrides = map[string]any{}
		}
		ref.Overrides[key] = false
	}
	enabled := 0
	for _, ref := range resolved.ComponentRefs {
		if ref.IsEnabled() {
			enabled++
		}
	}
	path := writeQueryRecipeFixture(t, resolved)

	var summary recipe.Summary
	if err := json.Unmarshal([]byte(runQueryValue(t, "query", "--recipe", path, "--summary", "--format=json")), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if len(summary.Components) != enabled {
		t.Errorf("summary lists %d components, want the %d enabled ones", len(summary.Components), enabled)
	}
	for _, c := range summary.Components {
		if _, ok := disabled[c.Name]; ok {
			t.Errorf("summary lists disabled component %s", c.Name)
		}
		for _, dep := range c.DependsOn {
			if _, ok := disabled[dep]; ok {
				t.Errorf("%s depends on disabled component %s", c.Name, dep)
			}
		}
	}

	text := runQueryValue(t, "query", "--recipe", path, "--summary")
	if want := fmt.Sprintf("COMPONENTS (%d)", enabled); !strings.Contains(text, want) {
		t.Errorf("text summary missing %q: %s", want, text)
	}
}

func TestQueryCmdRecipeConfig(t *testing.T) {
	_, _, path := queryRecipeFixture(t)
	tests := []struct {
		name string
		body string
	}{
		{"service", "    criteria: {service: eks}\n"},
		{"explicit wildcard", "    criteria: {service: any}\n"},
		{"accelerator", "    criteria: {accelerator: h100}\n"},
		{"intent", "    criteria: {intent: training}\n"},
		{"os", "    criteria: {os: ubuntu}\n"},
		{"platform", "    criteria: {platform: kubeflow}\n"},
		{"nodes", "    criteria: {nodes: 2}\n"},
		{"snapshot", "    input: {snapshot: missing.yaml}\n"},
		{"profile", "    profile: gpuStack=operator-managed\n"},
		{"accounting", "    configuration: {slurm: {accounting: {mode: disabled}}}\n"},
		{"inventory", "    configuration: {runtimeInventory: {mode: disabled}}\n"},
		{"interfaces", "    configuration:\n      gke:\n        tcpxoInterfaces:\n" +
			"          - {interfaceName: eth1, network: gpu-nic-0}\n" +
			"          - {interfaceName: eth2, network: gpu-nic-1}\n" +
			"          - {interfaceName: eth3, network: gpu-nic-2}\n" +
			"          - {interfaceName: eth4, network: gpu-nic-3}\n" +
			"          - {interfaceName: eth5, network: gpu-nic-4}\n" +
			"          - {interfaceName: eth6, network: gpu-nic-5}\n" +
			"          - {interfaceName: eth7, network: gpu-nic-6}\n" +
			"          - {interfaceName: eth8, network: gpu-nic-7}\n"},
		{"invalid criteria", "    criteria: {service: not-a-service}\n"},
		{"invalid selection", "    configuration: {runtimeInventory: {mode: not-a-mode}}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := writeYAML(t, "config.yaml", "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  recipe:\n"+tt.body)
			err := queryCmd().Run(t.Context(), []string{"query", "--recipe", path, "--summary", "--config", config})
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.HasPrefix(tt.name, "invalid") && (err == nil || !strings.Contains(err.Error(), "cannot be combined")) {
				t.Errorf("expected generation-input conflict, got %v", err)
			}
		})
	}
	for _, format := range []string{"yaml", "json", "table"} {
		t.Run("output-only config/"+format, func(t *testing.T) {
			config := writeYAML(t, "config.yaml", "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nspec:\n  recipe:\n    output: {format: "+format+"}\n")
			output := runQueryValue(t, "query", "--recipe", path, "--summary", "--config", config)
			want := "apiVersion:"
			switch format {
			case "json":
				want = `"apiVersion":`
			case "table":
				want = "RECIPE SUMMARY"
			}
			if !strings.Contains(output, want) {
				t.Errorf("config format ignored: %s", output)
			}
			output = runQueryValue(t, "query", "--recipe", path, "--summary", "--config", config, "--format=table")
			if !strings.Contains(output, "RECIPE SUMMARY") {
				t.Errorf("explicit format must override config: %s", output)
			}
		})
	}
}

// TestQueryCmdRecipeRequiresEvidence proves --recipe fails closed under an
// evidence requirement. A loaded file only names its criteria, profile and
// leaf overlay, so an edited copy of an attested recipe would otherwise pass
// the committed-pointer check.
func TestQueryCmdRecipeRequiresEvidence(t *testing.T) {
	t.Setenv(evidenceindex.EnvVar, "")
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	result, err := client.ResolveRecipeFromCriteria(t.Context(), aicr.WrapCriteria(&recipe.Criteria{
		Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorGB200,
		OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.RequirePublishedEvidence(t.Context(), result); err != nil {
		t.Fatalf("setup: gb200-eks-ubuntu-training must carry a committed pointer: %v", err)
	}
	edited := result.Resolved()
	if len(edited.ComponentRefs) == 0 {
		t.Fatal("setup: attested recipe has no components")
	}
	edited.ComponentRefs[0].Version = "v0.0.0-edited"
	editedPath := writeQueryRecipeFixture(t, edited)
	missingPath := filepath.Join(t.TempDir(), "missing.yaml")
	config := writeRequireEvidenceConfig(t, true)

	for _, file := range []struct{ name, path string }{
		{"edited attested recipe", editedPath},
		{"unread missing file", missingPath},
	} {
		for _, selection := range []string{"--summary", "--selector=criteria.service"} {
			for _, source := range []string{"flag", "config", "environment"} {
				t.Run(file.name+"/"+selection+"/"+source, func(t *testing.T) {
					args := []string{"query", "--recipe", file.path, selection}
					switch source {
					case "flag":
						args = append(args, "--require-evidence")
					case "config":
						args = append(args, "--config", config)
					case "environment":
						t.Setenv(evidenceindex.EnvVar, "true")
					}
					cmd := queryCmd()
					var output bytes.Buffer
					cmd.Writer = &output
					runErr := cmd.Run(t.Context(), args)
					errorstest.WantReportedCode(t, runErr, errors.ErrCodeInvalidRequest)
					if runErr == nil || !strings.Contains(runErr.Error(), "--recipe cannot be used while evidence is required") {
						t.Errorf("expected loaded-recipe evidence rejection, got %v", runErr)
					}
					if output.Len() != 0 {
						t.Errorf("rejected query printed output: %s", output.String())
					}
				})
			}
		}
	}

	t.Run("edited attested recipe/no requirement", func(t *testing.T) {
		if got := runQueryValue(t, "query", "--recipe", editedPath, "--selector=criteria.accelerator"); got != "gb200" {
			t.Errorf("query without an evidence requirement = %q, want gb200", got)
		}
	})
}

func TestQueryCmdProfiledSummary(t *testing.T) {
	criteria := &recipe.Criteria{
		Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
		OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
	}
	const profile = "gpuStack=operator-managed"
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	result, err := client.ResolveRecipeFromCriteriaWithOptions(t.Context(), aicr.WrapCriteria(criteria), aicr.WithProfile(profile))
	if err != nil {
		t.Fatal(err)
	}
	path := writeQueryRecipeFixture(t, result.Resolved())
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"saved profile", []string{"query", "--recipe", path, "--summary"}},
		{"generated profile", []string{"query", "--service=aks", "--accelerator=h100", "--os=ubuntu", "--intent=training", "--profile=" + profile, "--summary"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := runQueryValue(t, tt.args...)
			if !strings.Contains(output, "Profile:") || !strings.Contains(output, profile) {
				t.Errorf("summary missing selected profile: %s", output)
			}
		})
	}

	const missingValues = "components/gpu-operator/missing-profile-values.yaml"
	ref := result.Resolved().GetComponentRef("gpu-operator")
	if ref == nil {
		t.Fatal("setup: profiled recipe must contain gpu-operator")
	}
	ref.ValuesFile = missingValues
	invalidPath := writeQueryRecipeFixture(t, result.Resolved())
	t.Run("saved profile missing locked values", func(t *testing.T) {
		_, canonicalErr := client.LoadRecipe(t.Context(), invalidPath, "")
		if canonicalErr == nil || !strings.Contains(canonicalErr.Error(), missingValues) {
			t.Fatalf("setup: expected canonical loader to reject missing locked values, got %v", canonicalErr)
		}
		cmd := queryCmd()
		var output bytes.Buffer
		cmd.Writer = &output
		err := cmd.Run(t.Context(), []string{"query", "--recipe", invalidPath, "--summary"})
		errorstest.WantReportedCode(t, err, errorstest.ReportedCode(canonicalErr))
		if err == nil || !strings.Contains(err.Error(), missingValues) || output.Len() != 0 {
			t.Errorf("summary must preserve loader rejection: error=%v, output=%s", err, output.String())
		}
	})

	t.Run("generated profile missing locked values", func(t *testing.T) {
		dir := t.TempDir()
		registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents: []\n"
		if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(registry), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "overlays"), 0o755); err != nil {
			t.Fatal(err)
		}
		overlay := `apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata:
  name: query-profile-missing-values
spec:
  base: h100-aks-ubuntu-training
  criteria:
    service: aks
    accelerator: h100
    os: ubuntu
    intent: training
  componentRefs:
    - name: gpu-operator
      valuesFile: ` + missingValues + "\n"
		if err := os.WriteFile(filepath.Join(dir, "overlays", "query-profile-missing-values.yaml"), []byte(overlay), 0o600); err != nil {
			t.Fatal(err)
		}
		external, err := aicr.NewClient(aicr.WithRecipeSource(aicr.FilesystemSource(dir)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = external.Close() }()
		_, canonicalErr := external.ResolveRecipeFromCriteriaWithOptions(t.Context(), aicr.WrapCriteria(criteria), aicr.WithProfile(profile))
		if canonicalErr == nil || !strings.Contains(canonicalErr.Error(), missingValues) {
			t.Fatalf("setup: expected canonical resolver to reject missing locked values, got %v", canonicalErr)
		}
		cmd := queryCmd()
		var output bytes.Buffer
		cmd.Writer = &output
		err = cmd.Run(t.Context(), []string{"query", "--data", dir, "--service=aks", "--accelerator=h100", "--os=ubuntu", "--intent=training", "--profile=" + profile, "--summary"})
		errorstest.WantReportedCode(t, err, errorstest.ReportedCode(canonicalErr))
		if err == nil || !strings.Contains(err.Error(), missingValues) || output.Len() != 0 {
			t.Errorf("summary must preserve resolver rejection: error=%v, output=%s", err, output.String())
		}
	})
}

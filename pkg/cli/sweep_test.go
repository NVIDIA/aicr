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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

// sweepRunID is the 128-bit token shape the GPU conformance checks generate.
const sweepRunID = "0123456789abcdef0123456789abcdef"

// sweepTestNamespace is a plan-eligible per-run test namespace: the ownership
// label reconstructs the name, and it is older than the default floor.
var sweepTestNamespace = defaults.GPUTestNamespacePrefix + sweepRunID

// sweepCluster answers as a cluster carrying one stale GPU test namespace and
// nothing else: every other list is empty, the PodGroup CRD is absent, and a
// namespace delete succeeds.
func sweepCluster(withNamespace bool) func(method, path string) (int, string) {
	nsList := `{"kind":"NamespaceList","apiVersion":"v1","items":[]}`
	if withNamespace {
		nsList = fmt.Sprintf(`{"kind":"NamespaceList","apiVersion":"v1","items":[{
			"metadata":{"name":%q,"uid":"uid-%s","creationTimestamp":%q,
			"labels":{%q:%q}},
			"spec":{"finalizers":["kubernetes"]}}]}`,
			sweepTestNamespace, sweepTestNamespace,
			time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339),
			labels.GPUTestRun, sweepRunID)
	}
	return func(method, path string) (int, string) {
		switch {
		case method == http.MethodGet && path == "/api/v1/namespaces":
			return http.StatusOK, nsList
		case method == http.MethodGet && strings.HasPrefix(path, "/apis/scheduling.run.ai/"):
			return http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		case method == http.MethodGet:
			return http.StatusOK, `{"items":[]}`
		case method == http.MethodDelete:
			return http.StatusOK, `{"kind":"Status","apiVersion":"v1","status":"Success"}`
		default:
			return http.StatusOK, `{}`
		}
	}
}

// runSweep invokes the sweep command against a fresh tree, reading
// confirmation from reader, and returns everything it wrote to the root
// writer.
func runSweep(t *testing.T, reader io.Reader, args ...string) (string, error) {
	t.Helper()
	out, _, err := runSweepCapture(t, reader, args...)
	return out, err
}

// runSweepCapture is runSweep plus everything written to the root error
// writer, which --format json|yaml routes the human display to.
func runSweepCapture(t *testing.T, reader io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	app := &cli.Command{
		Name:      "aicr",
		Writer:    &outBuf,
		ErrWriter: &errBuf,
		Reader:    reader,
		Commands:  []*cli.Command{sweepCmd()},
	}
	err = app.Run(t.Context(), append([]string{"aicr", "sweep"}, args...))
	return outBuf.String(), errBuf.String(), err
}

// nonTerminalReader is a stdin that is provably not a TTY, so tests do not
// depend on whether `go test` itself was run from an interactive shell.
func nonTerminalReader(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestSweepCmd_CommandStructure(t *testing.T) {
	cmd := sweepCmd()

	if cmd.Name != "sweep" {
		t.Errorf("command name = %q, want %q", cmd.Name, "sweep")
	}
	if cmd.Category != functionalCategoryName {
		t.Errorf("category = %q, want %q", cmd.Category, functionalCategoryName)
	}
	for _, flagName := range []string{"namespace", "older-than", "dry-run", "yes", "output", "format", "kubeconfig"} {
		found := false
		for _, f := range cmd.Flags {
			if hasFlag(f, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing flag: %s", flagName)
		}
	}

	for _, f := range cmd.Flags {
		if df, ok := f.(*cli.DurationFlag); ok && df.Name == "older-than" && df.Value != defaults.ValidationSweepMinAge {
			t.Errorf("--older-than default = %v, want %v", df.Value, defaults.ValidationSweepMinAge)
		}
		if sf, ok := f.(*cli.StringFlag); ok && sf.Name == "namespace" && sf.Value != defaults.ValidationNamespace {
			t.Errorf("--namespace default = %q, want %q", sf.Value, defaults.ValidationNamespace)
		}
	}

	// The help text must keep the safety contract honest: what is never
	// swept, and the non-interactive failure mode.
	for _, fragment := range []string{"runai/queue", "never", "--dry-run", "--yes"} {
		if !strings.Contains(cmd.Description, fragment) {
			t.Errorf("description does not mention %q", fragment)
		}
	}
}

func TestSweepCmd_RejectsNonPositiveOlderThan(t *testing.T) {
	for _, value := range []string{"0s", "-1h"} {
		t.Run(value, func(t *testing.T) {
			_, err := runSweep(t, nonTerminalReader(t), "--older-than", value)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

// TestSweepCmd_FlagParseErrorExits1: urfave/cli flag-parse errors are not
// StructuredErrors, so the root command maps them to exit 1 — distinct from
// the exit 2 of a validation error the command itself raises (non-positive
// --older-than above).
func TestSweepCmd_FlagParseErrorExits1(t *testing.T) {
	for _, args := range [][]string{
		{"--older-than", "abc"},
		{"--bogus-flag"},
	} {
		_, err := runSweep(t, nonTerminalReader(t), args...)
		if err == nil {
			t.Fatalf("args %v: expected a parse error", args)
		}
		if got := errors.ExitCodeFromError(err); got != errors.ExitError {
			t.Errorf("args %v: exit code = %d, want %d", args, got, errors.ExitError)
		}
	}
}

// TestSweepCmd_TableConfigMapOutputRejectedBeforeApply: --format table with
// a cm:// destination is rejected before the command plans or applies — a
// rejection that came after the apply would have already deleted.
func TestSweepCmd_TableConfigMapOutputRejectedBeforeApply(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())

	_, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig,
		"--yes", "--format", "table", "--output", "cm://"+defaults.ValidationNamespace+"/sweep-report")
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if got := rec.seen(); len(got) != 0 {
		t.Errorf("the command contacted the cluster before rejecting the output combination: %v", got)
	}
}

func TestSweepCmd_NonTTYWithoutYesFails(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())

	_, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if sweepIssuedDelete(rec) {
		t.Error("a non-confirmed sweep must not delete")
	}
}

func TestSweepCmd_PromptAnswers(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantCode    errors.ErrorCode // zero value means success
		wantDeletes bool
	}{
		{"yes deletes", "y\n", "", true},
		{"long yes deletes", "yes\n", "", true},
		{"no deletes nothing", "n\n", errors.ErrCodeCanceled, false},
		{"empty answer declines", "\n", errors.ErrCodeCanceled, false},
		{"EOF declines", "", errors.ErrCodeCanceled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newScriptedAPIServer(t, sweepCluster(true))
			kubeconfig := rec.writeKubeconfig(t, t.TempDir())

			out, err := runSweep(t, strings.NewReader(tt.input), "--kubeconfig", kubeconfig)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("sweep error = %v, want nil", err)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
			}
			if got := sweepIssuedDelete(rec); got != tt.wantDeletes {
				t.Errorf("DELETE issued = %v, want %v (requests %v)", got, tt.wantDeletes, rec.seen())
			}
			for _, fragment := range []string{"Cluster context: recording", "API server:", sweepTestNamespace} {
				if !strings.Contains(out, fragment) {
					t.Errorf("output does not contain %q:\n%s", fragment, out)
				}
			}
		})
	}
}

func TestSweepCmd_YesSkipsPrompt(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())

	out, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig, "--yes")
	if err != nil {
		t.Fatalf("sweep --yes error = %v", err)
	}
	if strings.Contains(out, "[y/N]") {
		t.Errorf("--yes must not prompt:\n%s", out)
	}
	if !sweepIssuedDelete(rec) {
		t.Error("--yes sweep deleted nothing")
	}
	if !strings.Contains(out, "Sweep result:") || !strings.Contains(out, "deleting") {
		t.Errorf("result table missing from output:\n%s", out)
	}
}

func TestSweepCmd_DryRunNeverPromptsOrDeletes(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())

	out, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig, "--dry-run")
	if err != nil {
		t.Fatalf("sweep --dry-run error = %v", err)
	}
	if strings.Contains(out, "[y/N]") {
		t.Errorf("--dry-run must not prompt:\n%s", out)
	}
	if sweepIssuedDelete(rec) {
		t.Errorf("--dry-run deleted: %v", rec.seen())
	}
	if !strings.Contains(out, "dry-run") || !strings.Contains(out, "would-delete") {
		t.Errorf("dry-run output missing mode or candidate:\n%s", out)
	}
}

func TestSweepCmd_NothingToSweep(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(false))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())

	// No candidates means no prompt even on a terminal stream, exit 0.
	out, err := runSweep(t, strings.NewReader(""), "--kubeconfig", kubeconfig)
	if err != nil {
		t.Fatalf("sweep error = %v", err)
	}
	if !strings.Contains(out, "Nothing to sweep") {
		t.Errorf("output missing the nothing-to-sweep line:\n%s", out)
	}
}

func TestSweepCmd_JSONOutput(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())
	outPath := filepath.Join(t.TempDir(), "report.json")

	// Serializer output honors --output; the serializer's stdout is not the
	// root writer, so a file is how a test reads the document (as the
	// upgrade-check tests do).
	out, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig, "--dry-run", "--format", "json", "--output", outPath)
	if err != nil {
		t.Fatalf("sweep error = %v", err)
	}
	// Machine output is not polluted by the human display.
	if strings.Contains(out, "[y/N]") || strings.Contains(out, "KIND") {
		t.Errorf("json mode printed the table/prompt to the writer:\n%s", out)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report struct {
		ClusterContext string `json:"clusterContext"`
		Server         string `json:"server"`
		Namespace      string `json:"namespace"`
		DryRun         bool   `json:"dryRun"`
		Objects        []struct {
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, data)
	}
	if report.ClusterContext != "recording" || report.Namespace != defaults.ValidationNamespace || !report.DryRun {
		t.Errorf("report header = %+v", report)
	}
	if len(report.Objects) != 1 {
		t.Fatalf("report objects = %+v, want one", report.Objects)
	}
	got := report.Objects[0]
	if got.Kind != "Namespace" || got.Name != sweepTestNamespace || got.Status != "would-delete" {
		t.Errorf("report object = %+v", got)
	}
}

// TestSweepCmd_JSONPromptGoesToErrWriter pins the stdout contract: with
// --format json the document owns the root writer, so the plan table and the
// confirmation prompt land on the error writer.
func TestSweepCmd_JSONPromptGoesToErrWriter(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())
	outPath := filepath.Join(t.TempDir(), "report.json")

	stdout, stderr, err := runSweepCapture(t, strings.NewReader("y\n"),
		"--kubeconfig", kubeconfig, "--format", "json", "--output", outPath)
	if err != nil {
		t.Fatalf("sweep error = %v", err)
	}
	if stdout != "" {
		t.Errorf("json mode wrote the human display to the root writer:\n%s", stdout)
	}
	for _, fragment := range []string{"KIND", "[y/N]", sweepTestNamespace} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr does not contain %q:\n%s", fragment, stderr)
		}
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report struct {
		Objects []struct {
			Status string `json:"status"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, data)
	}
	if len(report.Objects) != 1 || report.Objects[0].Status != "deleting" {
		t.Errorf("report objects = %+v, want the namespace deleting", report.Objects)
	}
}

func TestSweepCmd_YAMLOutput(t *testing.T) {
	rec := newScriptedAPIServer(t, sweepCluster(true))
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())
	outPath := filepath.Join(t.TempDir(), "report.yaml")

	_, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig, "--yes", "--format", "yaml", "--output", outPath)
	if err != nil {
		t.Fatalf("sweep error = %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report struct {
		Objects []struct {
			Kind   string `yaml:"kind"`
			Status string `yaml:"status"`
		} `yaml:"objects"`
	}
	if err := yaml.Unmarshal(data, &report); err != nil {
		t.Fatalf("report is not YAML: %v\n%s", err, data)
	}
	if len(report.Objects) != 1 || report.Objects[0].Status != "deleting" {
		t.Errorf("report objects = %+v, want the namespace deleting", report.Objects)
	}
}

// TestSweepCmd_YesStructuredPrintsPlanToErrWriter pins the Safety contract
// for an unprompted apply: with --yes and --format json|yaml there is no
// prompt, but the plan — context, server and the exact candidate list — is
// still printed (to the error writer) before anything is deleted, while the
// destination holds only the structured report.
func TestSweepCmd_YesStructuredPrintsPlanToErrWriter(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			rec := newScriptedAPIServer(t, sweepCluster(true))
			kubeconfig := rec.writeKubeconfig(t, t.TempDir())
			outPath := filepath.Join(t.TempDir(), "report."+format)

			stdout, stderr, err := runSweepCapture(t, nonTerminalReader(t),
				"--kubeconfig", kubeconfig, "--yes", "--format", format, "--output", outPath)
			if err != nil {
				t.Fatalf("sweep error = %v", err)
			}
			if stdout != "" {
				t.Errorf("structured mode wrote the human display to the root writer:\n%s", stdout)
			}
			for _, fragment := range []string{"Cluster context: recording", "KIND", sweepTestNamespace} {
				if !strings.Contains(stderr, fragment) {
					t.Errorf("stderr does not contain %q:\n%s", fragment, stderr)
				}
			}
			if strings.Contains(stderr, "[y/N]") {
				t.Errorf("--yes must not prompt:\n%s", stderr)
			}
			if !sweepIssuedDelete(rec) {
				t.Error("--yes sweep deleted nothing")
			}

			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatalf("read report: %v", err)
			}
			var report struct {
				Objects []struct {
					Status string `json:"status" yaml:"status"`
				} `json:"objects" yaml:"objects"`
			}
			switch format {
			case "json":
				if err := json.Unmarshal(data, &report); err != nil {
					t.Fatalf("report is not JSON: %v\n%s", err, data)
				}
			default:
				if err := yaml.Unmarshal(data, &report); err != nil {
					t.Fatalf("report is not YAML: %v\n%s", err, data)
				}
			}
			if len(report.Objects) != 1 || report.Objects[0].Status != "deleting" {
				t.Errorf("report objects = %+v, want the namespace deleting", report.Objects)
			}
		})
	}
}

func TestSweepCmd_ForbiddenListIsUnauthorized(t *testing.T) {
	rec := newScriptedAPIServer(t, func(method, path string) (int, string) {
		if method == http.MethodGet && path == "/api/v1/namespaces" {
			return http.StatusForbidden, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`
		}
		return http.StatusOK, `{"items":[]}`
	})
	kubeconfig := rec.writeKubeconfig(t, t.TempDir())

	_, err := runSweep(t, nonTerminalReader(t), "--kubeconfig", kubeconfig, "--yes")
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if sweepIssuedDelete(rec) {
		t.Error("a sweep that cannot list must not delete")
	}
}

// sweepIssuedDelete reports whether the cluster saw any DELETE request.
func sweepIssuedDelete(rec *recordingAPIServer) bool {
	for _, r := range rec.seen() {
		if strings.HasPrefix(r, "DELETE ") {
			return true
		}
	}
	return false
}

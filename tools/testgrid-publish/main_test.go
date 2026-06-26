// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestResultString(t *testing.T) {
	tests := []struct {
		passed bool
		want   string
	}{
		{true, "SUCCESS"},
		{false, "FAILURE"},
	}
	for _, tt := range tests {
		got := resultString(tt.passed)
		if got != tt.want {
			t.Errorf("resultString(%v) = %q, want %q", tt.passed, got, tt.want)
		}
	}
}

func TestExtractSignerNoAttestation(t *testing.T) {
	// Bundle dir with no attestation.intoto.jsonl — both should be empty.
	dir := t.TempDir()
	identity, issuer := extractSigner(dir)
	if identity != "" || issuer != "" {
		t.Errorf("extractSigner() = (%q, %q), want (%q, %q)", identity, issuer, "", "")
	}
}

// TestRunDryRun exercises the full run() pipeline using a pre-materialized
// bundle directory so no OCI registry or GCS access is needed.
func TestRunDryRun(t *testing.T) {
	dir := t.TempDir()

	// Write recipe.yaml
	recipe := `
criteria:
  service: eks
  accelerator: h100
  os: ubuntu
  intent: training
  platform: kubeflow
constraints:
  - name: K8s.server.version
    value: ">=1.29"
`
	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write ctrf/deployment.json
	report := makeReport("deployment", []ctrf.TestResult{
		{Name: "health-check", Status: ctrf.StatusPassed, Duration: 1000},
	})
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	ctrfDir := filepath.Join(dir, ctrfDirName)
	if mkErr := os.MkdirAll(ctrfDir, 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(ctrfDir, "deployment.json"), data, 0o600); wErr != nil {
		t.Fatal(wErr)
	}

	// Redirect stdout to capture dry-run output.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	runErr := run(context.Background(), runConfig{
		bundleDir:   dir,
		bucket:      "aicr-testgrid-staging",
		sourceClass: sourceClassUAT,
		dryRun:      true,
	})

	_ = w.Close()

	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read dry-run output: %v", err)
	}
	output := string(outBytes)

	if runErr != nil {
		t.Fatalf("run() error = %v, output:\n%s", runErr, output)
	}
	for _, want := range []string{"eks", "h100-ubuntu", "training-kubeflow", "aicr-testgrid-staging", "started.json", "finished.json"} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}
}

// TestRunDryRunFailedTests verifies run() reports FAILURE when CTRF has a failure.
func TestRunDryRunFailedTests(t *testing.T) {
	dir := t.TempDir()

	recipe := `
criteria:
  service: gke
  accelerator: h100
  os: cos
  intent: inference
`
	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}

	report := makeReport("deployment", []ctrf.TestResult{
		{Name: "gpu-check", Status: ctrf.StatusFailed, Message: "GPU not found"},
	})
	data, _ := json.Marshal(report)
	ctrfDir := filepath.Join(dir, ctrfDirName)
	if err := os.MkdirAll(ctrfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctrfDir, "deployment.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	runErr := run(context.Background(), runConfig{
		bundleDir:   dir,
		bucket:      "aicr-testgrid-staging",
		sourceClass: sourceClassUAT,
		dryRun:      true,
	})

	_ = w.Close()

	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read dry-run output: %v", err)
	}
	output := string(outBytes)

	if runErr != nil {
		t.Fatalf("run() returned unexpected error: %v\noutput:\n%s", runErr, output)
	}
	if !strings.Contains(output, "passed=false") {
		t.Errorf("dry-run output should show passed=false for failed CTRF:\n%s", output)
	}
}

// TestRunDryRunMissingCTRF verifies run() errors when the bundle has no CTRF files.
func TestRunDryRunMissingCTRF(t *testing.T) {
	dir := t.TempDir()

	recipe := `
criteria:
  service: eks
  accelerator: h100
  os: ubuntu
  intent: training
`
	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}
	// No ctrf/ directory — convertCTRF should fail.

	err := run(context.Background(), runConfig{
		bundleDir:   dir,
		bucket:      "aicr-testgrid-staging",
		sourceClass: sourceClassUAT,
		dryRun:      true,
	})
	if err == nil {
		t.Fatal("run() expected error for missing CTRF files, got nil")
	}
}

// TestRunDryRunInvalidRecipe verifies run() errors on a bad recipe.
func TestRunDryRunInvalidRecipe(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), []byte("not: valid: yaml:"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run(context.Background(), runConfig{
		bundleDir:   dir,
		bucket:      "aicr-testgrid-staging",
		sourceClass: sourceClassUAT,
		dryRun:      true,
	})
	if err == nil {
		t.Fatal("run() expected error for invalid recipe, got nil")
	}
}

// TestRunDryRunSummaryBundleAutoResolve verifies that --bundle-dir pointing at
// a bundle parent directory auto-resolves to the nested summary-bundle/ subdir.
func TestRunDryRunSummaryBundleAutoResolve(t *testing.T) {
	parent := t.TempDir()
	bundleDir := filepath.Join(parent, attestation.SummaryBundleDirName)
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}

	recipe := `
criteria:
  service: gke
  accelerator: h100
  os: cos
  intent: inference
`
	if err := os.WriteFile(filepath.Join(bundleDir, attestation.RecipeFilename), []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}

	report := makeReport("deployment", []ctrf.TestResult{
		{Name: "ok", Status: ctrf.StatusPassed, Duration: 500},
	})
	data, _ := json.Marshal(report)
	ctrfDir := filepath.Join(bundleDir, ctrfDirName)
	if err := os.MkdirAll(ctrfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctrfDir, "deployment.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	oldStdout := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	// Point at the *parent* dir — run() should auto-resolve to summary-bundle/.
	err := run(context.Background(), runConfig{
		bundleDir:   parent,
		bucket:      "aicr-testgrid-staging",
		sourceClass: sourceClassUAT,
		dryRun:      true,
	})
	_ = w.Close()
	if err != nil {
		t.Fatalf("run() expected success with auto-resolved summary-bundle, got: %v", err)
	}
}

// TestWriteGCSGcloudNotFound verifies writeGCS returns a clear error when
// gcloud is not found on PATH.
func TestWriteGCSGcloudNotFound(t *testing.T) {
	// Set PATH to an empty dir so gcloud is not found.
	t.Setenv("PATH", t.TempDir())

	started := startedJSON{Timestamp: 1, Metadata: map[string]string{}}
	finished := finishedJSON{Timestamp: 1, Metadata: map[string]string{}}

	err := writeGCS(context.Background(), "bucket", "prefix",
		started, finished, []byte("<testsuites/>"))
	if err == nil {
		t.Fatal("writeGCS() expected error when gcloud not in PATH")
	}
}

func TestPrintDryRun(t *testing.T) {
	// Capture stdout by redirecting os.Stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	started := startedJSON{
		Timestamp: 1749600000,
		Metadata: map[string]string{
			metaKeyAICRVersion: "v0.1.0",
			metaKeySourceClass: sourceClassUAT,
		},
	}
	finished := finishedJSON{
		Timestamp: 1749600060,
		Passed:    true,
		Result:    "SUCCESS",
		Metadata:  started.Metadata,
	}

	printDryRun("my-bucket", "groups/eks/h100-ubuntu/training/1749600000-abc12345",
		started, finished, []byte("<testsuites/>"))

	_ = w.Close()

	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read dry-run output: %v", err)
	}
	output := string(outBytes)
	if !strings.Contains(output, "my-bucket") {
		t.Errorf("output missing bucket name:\n%s", output)
	}
	if !strings.Contains(output, "started.json") {
		t.Errorf("output missing started.json:\n%s", output)
	}
	if !strings.Contains(output, "finished.json") {
		t.Errorf("output missing finished.json:\n%s", output)
	}
	if !strings.Contains(output, "v0.1.0") {
		t.Errorf("output missing aicr_version:\n%s", output)
	}
}

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

func TestMetaKeys(t *testing.T) {
	keys := MetaKeys()

	// Verify length matches the declared constants.
	const wantLen = 7
	if len(keys) != wantLen {
		t.Errorf("MetaKeys() length = %d, want %d", len(keys), wantLen)
	}

	// Verify all constants are present.
	required := map[string]bool{
		metaKeyAICRVersion:    false,
		metaKeyK8sVersion:     false,
		metaKeyK8sConstraint:  false,
		metaKeySignerIdentity: false,
		metaKeySignerIssuer:   false,
		metaKeySourceClass:    false,
		metaKeyEvidenceDigest: false,
	}
	seen := make(map[string]bool)
	for _, k := range keys {
		if k == "" {
			t.Error("MetaKeys() contains empty string")
		}
		if seen[k] {
			t.Errorf("duplicate key %q in MetaKeys()", k)
		}
		seen[k] = true
		if _, ok := required[k]; !ok {
			t.Errorf("unexpected key %q in MetaKeys()", k)
		}
		required[k] = true
	}
	for k, found := range required {
		if !found {
			t.Errorf("MetaKeys() missing required key %q", k)
		}
	}

	// Verify stability: two calls return the same slice in the same order.
	keys2 := MetaKeys()
	for i := range keys {
		if i >= len(keys2) || keys[i] != keys2[i] {
			t.Errorf("MetaKeys() not stable at index %d: %q vs %q", i, keys[i], keys2[i])
		}
	}
}

// TestMetaKeysMatchEmittedMetadata verifies that every key declared by
// MetaKeys() actually appears in the started.json metadata emitted by run().
func TestMetaKeysMatchEmittedMetadata(t *testing.T) {
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

	report := ctrf.Report{
		ReportFormat: ctrf.ReportFormatCTRF,
		SpecVersion:  ctrf.SpecVersion,
		Results: ctrf.Results{
			Tool:    ctrf.Tool{Name: "deployment"},
			Summary: ctrf.Summary{Tests: 1, Passed: 1},
			Tests:   []ctrf.TestResult{{Name: "ok", Status: ctrf.StatusPassed, Duration: 100}},
		},
	}
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

	// Capture dry-run stdout.
	oldStdout := os.Stdout
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	if runErr := run(context.Background(), runConfig{
		bundleDir:   dir,
		bucket:      "test-bucket",
		sourceClass: sourceClassUAT,
		dryRun:      true,
	}); runErr != nil {
		_ = w.Close()
		t.Fatalf("run() error = %v", runErr)
	}
	_ = w.Close()

	outBytes, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatal(readErr)
	}
	output := string(outBytes)

	// Parse emitted key names from lines like "  key_name             = ..."
	emitted := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) == 2 {
			emitted[strings.TrimSpace(parts[0])] = true
		}
	}

	for _, k := range MetaKeys() {
		if !emitted[k] {
			t.Errorf("MetaKeys() key %q not found in dry-run output:\n%s", k, output)
		}
	}
}

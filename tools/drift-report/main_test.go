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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// runFixtureReport resolves one real registry-chart pin (nvsentinel) as
// current, so BuildReport sees resolved > 0 and does not fail closed.
// currentValue below must track the live nvsentinel pin in
// recipes/registry.yaml; a drifted fixture pin routes this dep to the
// mismatch branch, and the symptom is an off-by-one in Summary.Unresolved
// far from this line.
const runFixtureReport = `{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "ghcr.io/nvidia/nvsentinel",
                "depType": "registry-chart",
                "datasource": "docker",
                "currentValue": "v1.20.0",
                "updates": []
              }
            ]
          }
        ]
      }
    }
  }
}`

// TestRunCreatesOutputDirectories confirms the write path creates -out and
// -slack-out's parent directories on demand. The Makefile target's own
// defaults (dist/drift-report.json, dist/slack-payload.json) point at a
// gitignored directory absent from a clean checkout, so run must not assume
// the parent exists.
func TestRunCreatesOutputDirectories(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "raw.json")
	if err := os.WriteFile(reportPath, []byte(runFixtureReport), 0o600); err != nil {
		t.Fatalf("write fixture renovate report: %v", err)
	}

	out := filepath.Join(dir, "nested", "drift-report.json")
	slackOut := filepath.Join(dir, "nested2", "slack-payload.json")

	if err := run(testRepoRoot(t), reportPath, out, slackOut, Meta{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	outData, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read -out: %v", err)
	}
	var report Report
	if err = json.Unmarshal(outData, &report); err != nil {
		t.Fatalf("-out is not valid JSON: %v", err)
	}
	if report.SchemaVersion != schemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", report.SchemaVersion, schemaVersion)
	}
	// runFixtureReport resolves only nvsentinel; every other tracked pin in the
	// real registry.yaml is absent from the Renovate report and must land as
	// unresolved, never as current. 30, not 33: three OpenShift twin pairs
	// (prometheus-adapter, k8s-nim-operator, nvidia-dra-driver-gpu) share their
	// non-OCP sibling's chart/registry/version and collapse into one row apiece.
	const wantUnresolved = 30
	if report.Summary.Unresolved != wantUnresolved {
		t.Errorf("Summary.Unresolved = %d, want %d", report.Summary.Unresolved, wantUnresolved)
	}
	if report.Summary.Behind != 0 {
		t.Errorf("Summary.Behind = %d, want 0", report.Summary.Behind)
	}
	if len(report.Current) != 1 {
		t.Errorf("len(Current) = %d, want 1 (nvsentinel)", len(report.Current))
	}

	slackData, err := os.ReadFile(slackOut) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read -slack-out: %v", err)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err = json.Unmarshal(slackData, &payload); err != nil {
		t.Fatalf("-slack-out is not valid JSON: %v", err)
	}
	if payload.Text == "" {
		t.Error("-slack-out text is empty")
	}
}

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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowPath is the RQ2 link-check workflow relative to this package.
const workflowPath = "../../.github/workflows/testgrid-link-check.yaml"

// linkCheckWorkflow is the subset of the workflow YAML the warning-only
// contract is asserted against. actionlint (merge-gate.yaml) covers syntax;
// this test pins the invariants the linter cannot express: read-only token, no
// pull-request write, weekly schedule + dispatch, and the report-only run.
type linkCheckWorkflow struct {
	On          map[string]yaml.Node   `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
}

func loadLinkCheckWorkflow(t *testing.T) linkCheckWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(workflowPath))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var wf linkCheckWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	return wf
}

func TestWorkflowIsWarningOnlyAndReadOnly(t *testing.T) {
	wf := loadLinkCheckWorkflow(t)

	// Top-level token is read-only: contents is read, and no scope is write —
	// a future `pull-requests: write` (or any other write) must fail this test.
	if got := wf.Permissions["contents"]; got != "read" {
		t.Errorf("top-level contents permission = %q, want read", got)
	}
	for scope, level := range wf.Permissions {
		if level == "write" {
			t.Errorf("top-level permission %q = write; the link-check bot must stay read-only", scope)
		}
	}

	// Triggers: weekly schedule + manual dispatch (clone of bom-refresh).
	if _, ok := wf.On["schedule"]; !ok {
		t.Error("workflow is missing the weekly `schedule` trigger")
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Error("workflow is missing the `workflow_dispatch` trigger")
	}

	// Exactly one job, and it must not escalate to any write permission — the
	// bot never opens a PR or edits the doc.
	for name, job := range wf.Jobs {
		for perm, level := range job.Permissions {
			if level == "write" {
				t.Errorf("job %q grants %s: write; the link-check bot must stay read-only", name, perm)
			}
		}
		for _, step := range job.Steps {
			if strings.Contains(step.Uses, "create-pull-request") {
				t.Errorf("job %q uses create-pull-request; the link-check bot must never open a PR", name)
			}
		}
	}
}

func TestWorkflowRunsLinkCheckToStepSummary(t *testing.T) {
	wf := loadLinkCheckWorkflow(t)
	var found bool
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "tools/testgrid-link-check") {
				found = true
				// Require the exact flag pairing, not merely both strings present:
				// a step that wrote to stdout and mentioned GITHUB_STEP_SUMMARY
				// elsewhere would otherwise pass while breaking report delivery.
				if !strings.Contains(step.Run, `-report-out "$GITHUB_STEP_SUMMARY"`) {
					t.Errorf(`link-check step must pass -report-out "$GITHUB_STEP_SUMMARY", got: %q`, step.Run)
				}
			}
		}
	}
	if !found {
		t.Error("workflow never invokes tools/testgrid-link-check")
	}
}

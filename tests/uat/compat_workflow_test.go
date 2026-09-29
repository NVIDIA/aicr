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

package uat

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/uatbroker"
)

const (
	compatFile        = "compat.yaml"
	registryFile      = "../../infra/uat/reservations.yaml"
	nightlyWorkflow   = "uat-nightly-batch.yaml"
	runWorkflow       = "uat-run.yaml"
	compatPRWorkflow  = "compat-floor-check.yaml"
	compatStepName    = "Check harness-compat floors"
	driveStepName     = "Run version-matrix cells (sequential, time-boxed)"
	gateStepName      = "Harness-compat floor gate"
	resolveStepName   = "Resolve reservation row"
	skipNoticeLiteral = "::notice title=UAT release cell skipped (compat floor)::"
	legacyMinVersions = "nightly-intent-min-versions"
)

// compatWorkflow is the subset of a workflow document the compat-floor
// wiring tests read; distinct from workflowDocument because they also need
// triggers, step conditions, and `with:` inputs.
type compatWorkflow struct {
	On   compatTriggers               `yaml:"on"`
	Jobs map[string]compatWorkflowJob `yaml:"jobs"`
}

type compatTriggers struct {
	WorkflowDispatch *compatTrigger `yaml:"workflow_dispatch"`
	WorkflowCall     *compatTrigger `yaml:"workflow_call"`
	PullRequest      *struct {
		Paths []string `yaml:"paths"`
	} `yaml:"pull_request"`
	Schedule []map[string]string `yaml:"schedule"`
}

type compatTrigger struct {
	Inputs map[string]struct {
		Type    string `yaml:"type"`
		Default any    `yaml:"default"`
	} `yaml:"inputs"`
}

type compatWorkflowJob struct {
	Steps []compatWorkflowStep `yaml:"steps"`
}

type compatWorkflowStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]any    `yaml:"with"`
}

func decodeCompatWorkflow(t *testing.T, file string) compatWorkflow {
	t.Helper()
	var w compatWorkflow
	if err := loadWorkflow(t, file).Decode(&w); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	return w
}

// compatStepIndex returns the index of the unique step named name in job.
func compatStepIndex(t *testing.T, job compatWorkflowJob, name string) int {
	t.Helper()
	idx := -1
	for i, s := range job.Steps {
		if s.Name != name {
			continue
		}
		if idx >= 0 {
			t.Fatalf("found more than one step named %q", name)
		}
		idx = i
	}
	if idx < 0 {
		t.Fatalf("no step named %q", name)
	}
	return idx
}

func compatJob(t *testing.T, w compatWorkflow, file, name string) compatWorkflowJob {
	t.Helper()
	job, ok := w.Jobs[name]
	if !ok {
		t.Fatalf("%s has no job %q", file, name)
	}
	return job
}

func assertContainsAll(t *testing.T, what, text string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if !strings.Contains(text, m) {
			t.Errorf("%s does not contain %q", what, m)
		}
	}
}

// TestCommittedCompatFile asserts only the STRUCTURE of tests/uat/compat.yaml,
// never specific floors (so the suite never pressures anyone to keep a stale
// floor). The gcp row is the one exception: its presence, together with the
// absence of the legacy registry key, proves the #2860 migration.
func TestCommittedCompatFile(t *testing.T) {
	reg, err := uatbroker.LoadRegistryFile(registryFile)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	compat, err := uatbroker.LoadCompatFile(compatFile, reg)
	if err != nil {
		t.Fatalf("load %s: %v", compatFile, err)
	}
	for _, f := range compat.Floors {
		info, statErr := os.Stat(f.Lane)
		if statErr != nil || !info.IsDir() {
			t.Errorf("compat lane %q has no tests/uat/%s/ directory (stat error: %v)", f.Lane, f.Lane, statErr)
		}
	}
	if _, ok := compat.FloorFor("gcp", "training"); !ok {
		t.Error("compat.yaml has no gcp/training floor; the #2705 gate must live here after the migration")
	}

	data, err := os.ReadFile(registryFile)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if strings.Contains(string(data), legacyMinVersions) {
		t.Errorf("%s still mentions %s; floors live in tests/uat/%s", registryFile, legacyMinVersions, compatFile)
	}
}

func TestNoWorkflowReferencesLegacyMinVersions(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(workflowsDir, "*.y*ml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no workflows found")
	}
	for _, p := range paths {
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		if strings.Contains(string(data), legacyMinVersions) {
			t.Errorf("%s still references %s", filepath.Base(p), legacyMinVersions)
		}
	}
}

func TestNightlyBatchCompatWiring(t *testing.T) {
	w := decodeCompatWorkflow(t, nightlyWorkflow)
	drive := compatJob(t, w, nightlyWorkflow, "drive")
	compatIdx := compatStepIndex(t, drive, compatStepName)
	driveIdx := compatStepIndex(t, drive, driveStepName)
	if compatIdx >= driveIdx {
		t.Fatalf("%q (step %d) must run before %q (step %d)", compatStepName, compatIdx, driveStepName, driveIdx)
	}
	compat := drive.Steps[compatIdx]
	if compat.ID != "compat" {
		t.Errorf("%q id = %q, want compat (the drive step reads steps.compat.outputs)", compatStepName, compat.ID)
	}
	assertContainsAll(t, "compat step", compat.Run,
		"uat-broker compat list",
		"uat-broker compat check",
		"--containing",
		"git blame --porcelain -L",
		"git tag -l --contains",
		"ignore_lines=",
		"rejections_file=",
	)

	run := drive.Steps[driveIdx]
	assertContainsAll(t, "drive step", run.Run,
		`--compat "$COMPAT_FILE"`,
		"--ignore-floor-lines",
		".skipped",
		skipNoticeLiteral,
		"GITHUB_STEP_SUMMARY",
		"### Release cells skipped by harness-compat floor",
		"::error title=UAT compat floor rejected::",
	)
	for _, key := range []string{"IGNORE_FLOOR_LINES", "COMPAT_REJECTIONS_FILE"} {
		if !strings.Contains(run.Env[key], "steps.compat.outputs.") {
			t.Errorf("drive step env %s = %q, want it wired from steps.compat.outputs", key, run.Env[key])
		}
	}
}

// TestNightlyFullyGatedCellIsNotSilent guards the #2860 regression: a release
// cell with no eligible intents used to be dropped by a bare `continue`.
func TestNightlyFullyGatedCellIsNotSilent(t *testing.T) {
	w := decodeCompatWorkflow(t, nightlyWorkflow)
	drive := compatJob(t, w, nightlyWorkflow, "drive")
	script := drive.Steps[compatStepIndex(t, drive, driveStepName)].Run
	lines := strings.Split(script, "\n")

	skipNoticeAt, emptyCheckAt := -1, -1
	for i, l := range lines {
		if strings.Contains(l, skipNoticeLiteral) && skipNoticeAt < 0 {
			skipNoticeAt = i
		}
		if strings.Contains(l, "${#cell_intents[@]} == 0") {
			if emptyCheckAt >= 0 {
				t.Fatal("more than one empty-intents check in the drive step")
			}
			emptyCheckAt = i
		}
	}
	if emptyCheckAt < 0 {
		t.Fatal("drive step has no empty-intents check")
	}
	if skipNoticeAt < 0 || skipNoticeAt > emptyCheckAt {
		t.Errorf("per-intent skip notice (line %d) must be emitted before the empty-intents check (line %d)", skipNoticeAt, emptyCheckAt)
	}
	if strings.Contains(lines[emptyCheckAt], "continue") {
		t.Errorf("empty-intents check skips silently: %q", strings.TrimSpace(lines[emptyCheckAt]))
	}
	noticed := false
	for _, l := range lines[emptyCheckAt+1:] {
		trimmed := strings.TrimSpace(l)
		if strings.Contains(trimmed, "::notice") {
			noticed = true
		}
		if trimmed == "continue" {
			if !noticed {
				t.Error("fully-gated cell `continue`s without a preceding ::notice")
			}
			return
		}
		if trimmed == "fi" {
			break
		}
	}
	t.Error("empty-intents block does not `continue` past the fully-gated cell")
}

func TestUATRunCompatGate(t *testing.T) {
	w := decodeCompatWorkflow(t, runWorkflow)
	triggers := map[string]*compatTrigger{
		"workflow_dispatch": w.On.WorkflowDispatch,
		"workflow_call":     w.On.WorkflowCall,
	}
	for name, trig := range triggers {
		if trig == nil {
			t.Fatalf("%s has no %s trigger", runWorkflow, name)
		}
		in, ok := trig.Inputs["allow_below_compat_floor"]
		if !ok {
			t.Errorf("%s %s does not declare allow_below_compat_floor", runWorkflow, name)
			continue
		}
		if in.Type != "boolean" || in.Default != false {
			t.Errorf("%s %s allow_below_compat_floor = {type: %q, default: %v}, want {boolean, false}",
				runWorkflow, name, in.Type, in.Default)
		}
	}

	resolve := compatJob(t, w, runWorkflow, "resolve")
	gateIdx := compatStepIndex(t, resolve, gateStepName)
	// The run-<cloud> jobs are always()-gated on the resolved cloud output,
	// so the refusal must fail the job before that output is written.
	if rowIdx := compatStepIndex(t, resolve, resolveStepName); gateIdx >= rowIdx {
		t.Errorf("%q (step %d) must run before %q (step %d)", gateStepName, gateIdx, resolveStepName, rowIdx)
	}
	gate := resolve.Steps[gateIdx]
	if strings.ReplaceAll(gate.If, " ", "") != "inputs.aicr_version!=''" {
		t.Errorf("%q if = %q, want inputs.aicr_version != '' (main is never gated)", gateStepName, gate.If)
	}
	// The override must apply only to the broker's below-floor refusal (see
	// TestCompatGate in tools/uat-broker), not to every exit-2 failure.
	assertContainsAll(t, "gate step", gate.Run, "uat-broker compat gate", "--cloud", "--intent", "--version",
		"is below the harness-compat floor")
	wantEnv := map[string]string{
		"AICR_VERSION":             "${{ inputs.aicr_version }}",
		"INTENT":                   "${{ inputs.intent }}",
		"ALLOW_BELOW_COMPAT_FLOOR": "${{ inputs.allow_below_compat_floor }}",
	}
	for k, v := range wantEnv {
		if gate.Env[k] != v {
			t.Errorf("%q env %s = %q, want %q", gateStepName, k, gate.Env[k], v)
		}
	}
}

// TestCloudPipelinesOnlyReachableThroughUATRun pins why the per-cloud
// pipelines carry no compat warning step of their own: they have no dispatch
// or schedule trigger, so every run passes uat-run.yaml's compat gate. Adding
// a direct trigger must come with its own compat check.
func TestCloudPipelinesOnlyReachableThroughUATRun(t *testing.T) {
	for _, file := range []string{"uat-aws.yaml", "uat-gcp.yaml", "uat-azure.yaml", "uat-kind.yaml"} {
		t.Run(file, func(t *testing.T) {
			w := decodeCompatWorkflow(t, file)
			if w.On.WorkflowCall == nil {
				t.Errorf("%s is not a reusable (workflow_call) pipeline", file)
			}
			if w.On.WorkflowDispatch != nil || w.On.PullRequest != nil || len(w.On.Schedule) > 0 {
				t.Errorf("%s is directly triggerable; route it through uat-run.yaml or add a compat gate", file)
			}
		})
	}
}

func TestCompatFloorPRCheck(t *testing.T) {
	w := decodeCompatWorkflow(t, compatPRWorkflow)
	if w.On.PullRequest == nil {
		t.Fatalf("%s has no pull_request trigger", compatPRWorkflow)
	}
	for _, p := range []string{"tests/uat/compat.yaml", "infra/uat/reservations.yaml", ".github/workflows/" + compatPRWorkflow} {
		if !slices.Contains(w.On.PullRequest.Paths, p) {
			t.Errorf("%s pull_request.paths missing %q", compatPRWorkflow, p)
		}
	}
	job := compatJob(t, w, compatPRWorkflow, "compat-check")
	checkout := job.Steps[compatStepIndex(t, job, "Checkout")]
	if depth, ok := checkout.With["fetch-depth"].(int); !ok || depth != 0 {
		t.Errorf("checkout fetch-depth = %v, want 0 (blame + tag --contains need full history)", checkout.With["fetch-depth"])
	}
	assertContainsAll(t, "PR compat step", job.Steps[compatStepIndex(t, job, compatStepName)].Run,
		"uat-broker compat list",
		"uat-broker compat check",
		"git blame --porcelain -L",
		"git tag -l --contains",
		"exit 1",
	)
}

// TestCompatStepsNoInlineExpressions keeps workflow inputs and repo-derived
// values out of the compat run blocks: they must arrive via env.
func TestCompatStepsNoInlineExpressions(t *testing.T) {
	// failClosed marks the steps written for #2860; the drive step predates it
	// and deliberately tolerates `gh run watch`/`gh run list` failures.
	type target struct {
		file, job, step string
		failClosed      bool
	}
	targets := []target{
		{nightlyWorkflow, "drive", compatStepName, true},
		{nightlyWorkflow, "drive", driveStepName, false},
		{runWorkflow, "resolve", gateStepName, true},
		{compatPRWorkflow, "compat-check", compatStepName, true},
	}
	for _, tg := range targets {
		w := decodeCompatWorkflow(t, tg.file)
		job := compatJob(t, w, tg.file, tg.job)
		run := job.Steps[compatStepIndex(t, job, tg.step)].Run
		if strings.Contains(run, "${{") {
			t.Errorf("%s %q interpolates a ${{ }} expression into run:; pass it via env", tg.file, tg.step)
		}
		if tg.failClosed && strings.Contains(run, "|| true") {
			t.Errorf("%s %q swallows an exit status with `|| true`; capture rc and fail closed", tg.file, tg.step)
		}
	}
}

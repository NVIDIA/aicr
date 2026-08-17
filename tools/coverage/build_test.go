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
	"slices"
	"strings"
	"testing"
)

// writeFixture lays down a minimal signal tree under root.
func writeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

// buildMatrix builds the matrix for root and fails the test on error.
func buildMatrix(t *testing.T, root string) Matrix {
	t.Helper()
	m, err := BuildMatrix(root)
	if err != nil {
		t.Fatalf("BuildMatrix(%s): %v", root, err)
	}
	return m
}

func rowByItem(m Matrix, item string) (Row, bool) {
	for _, r := range m.Rows {
		if r.Item == item {
			return r, true
		}
	}
	return Row{}, false
}

// reservationRow renders one reservation registry row. nightlyIntents is
// emitted verbatim, so a caller can pass "[training]", an explicit opt-out
// "[]", or "" to omit the key entirely (which defaults to [training]).
func reservationRow(name, slug, cloud, nightlyIntents string) string {
	row := "  - name: " + name + "\n" +
		"    slug: " + slug + "\n" +
		"    cloud: " + cloud + "\n" +
		"    accelerator: h100\n" +
		"    gpu-count: 8\n" +
		"    cluster-config-path: tests/uat/" + cloud + "/cluster-config.yaml\n" +
		"    test-config-dir: tests/uat/" + cloud + "/tests\n"
	if nightlyIntents != "" {
		row += "    nightly-intents: " + nightlyIntents + "\n"
	}
	return row
}

// nightlyBatchFixture renders a uat-nightly-batch.yaml carrying the previous_n
// input default and the matching schedule || 'N' fallback the version axis
// requires to agree.
func nightlyBatchFixture(previousN string) string {
	return "name: UAT Nightly Batch\n" +
		"on:\n" +
		"  schedule:\n" +
		"    - cron: '0 4 * * *'\n" +
		"  workflow_dispatch:\n" +
		"    inputs:\n" +
		"      previous_n:\n" +
		"        type: string\n" +
		"        default: '" + previousN + "'\n" +
		"jobs:\n" +
		"  drive:\n" +
		"    steps:\n" +
		"      - env:\n" +
		"          PREVIOUS_N: ${{ inputs.previous_n || '" + previousN + "' }}\n" +
		"        run: echo ok\n"
}

// cloudPipelineFixture renders a per-cloud uat-<cloud>.yaml running the given
// phases as enabled steps, and any disabledPhases as commented-out ones — the
// shape the real pipelines use for a phase that is present but not executed.
func cloudPipelineFixture(cloud string, phases []string, disabledPhases ...string) string {
	wf := "name: UAT " + cloud + "\non:\n  workflow_call: {}\njobs:\n  uat:\n    steps:\n"
	for _, phase := range phases {
		wf += "      - name: UAT - " + phase + "\n" +
			"        run: ./tests/uat/" + cloud + "/run " + phase + " \"${TEST_CONFIG}\"\n"
	}
	for _, phase := range disabledPhases {
		wf += "      # TEMPORARILY DISABLED\n" +
			"      # - name: UAT - " + phase + "\n" +
			"      #   run: ./tests/uat/" + cloud + "/run " + phase + " \"${TEST_CONFIG}\"\n"
	}
	return wf
}

// wiredTrainingFixture sets up a repo where one reservation (aws) is enrolled in
// the nightly batch for training only, the per-cloud runner is a thin shim that
// sources the shared phase library holding the real invocations, chainsaw
// exercises recipe/validate, inference UAT assets exist but no reservation runs
// that intent, and demos document `query`.
func wiredTrainingFixture(t *testing.T, root string) {
	writeFixture(t, root, map[string]string{
		"infra/uat/reservations.yaml": "reservations:\n" +
			reservationRow("aws-h100", "ah1", "aws", "[training]"),
		".github/workflows/uat-nightly-batch.yaml": nightlyBatchFixture("1"),
		".github/workflows/uat-aws.yaml": cloudPipelineFixture("aws",
			[]string{"prep", "install", "conformance", "train", "verify"}, "serve"),
		// The runner is a shim: it carries no `aicr` invocation of its own.
		"tests/uat/aws/run": "#!/usr/bin/env bash\n" +
			"source \"$(dirname \"${BASH_SOURCE[0]}\")/../lib/phases.sh\"\n" +
			"uat_main \"$@\"\n",
		// The shared phase library holds the real nightly invocations, including
		// the argv-array form.
		"tests/uat/lib/phases.sh": "# shellcheck shell=bash\n" +
			"\"${AICR_BIN}\" snapshot --config \"${config}\"\n" +
			"\"${AICR_BIN}\" bundle --config \"${config}\"\n" +
			"args=(evidence verify ./evidence/pointer.yaml)\n" +
			"\"${AICR_BIN}\" \"${args[@]}\"\n",
		// chainsaw exercises recipe + validate (per-PR).
		"tests/chainsaw/cli/recipe-gen/chainsaw-test.yaml": "run: aicr recipe --service eks\nrun: ${AICR_BIN} validate -r r.yaml\n",
		// Inference UAT assets exist but no enrolled reservation runs that intent.
		"tests/uat/aws/tests/cuj2-inference/test.yaml": "script: ${AICR_BIN} bundle\n",
		// demos document query only (not executable).
		"demos/cuj1-eks.md": "Run `aicr query --selector x` to inspect.\n",
	})
}

func TestBuildMatrixStatusFromSignals(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)

	m := buildMatrix(t, root)

	tests := []struct {
		item       string
		wantStatus Status
		wantNote   bool
	}{
		{"recipe", StatusCovered, false},                 // chainsaw
		{"validate", StatusCovered, false},               // chainsaw
		{"bundle", StatusCovered, false},                 // enrolled UAT lane
		{"evidence verify", StatusCovered, false},        // argv-array in the shared phase library
		{"query", StatusNotYetCovered, true},             // demo-only → note
		{"diff", StatusNotYetCovered, false},             // no signal anywhere
		{"cuj1-training-kubeflow", StatusCovered, false}, // enrolled training intent + demo
		{"cuj2-inference-dynamo", StatusStubbed, true},   // assets present but intent unenrolled
	}
	for _, tt := range tests {
		t.Run(tt.item, func(t *testing.T) {
			r, ok := rowByItem(m, tt.item)
			if !ok {
				t.Fatalf("row %q not in matrix", tt.item)
			}
			if r.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (harnesses=%v)", r.Status, tt.wantStatus, r.Harnesses)
			}
			if (r.Note != "") != tt.wantNote {
				t.Errorf("note presence = %v (%q), want %v", r.Note != "", r.Note, tt.wantNote)
			}
		})
	}
}

// TestUATVerbsResolveThroughSharedPhaseLibrary is the #1977 regression guard:
// the per-cloud runners are shims that source tests/uat/lib/phases.sh, so a
// scanner that reads only the shims sees zero `aicr` invocations and silently
// downgrades every UAT row to uncovered.
func TestUATVerbsResolveThroughSharedPhaseLibrary(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	m := buildMatrix(t, root)

	// Only the shared library invokes these; the runner shim carries none of them.
	for _, verb := range []string{"snapshot", "bundle", "evidence verify"} {
		t.Run(verb, func(t *testing.T) {
			r, ok := rowByItem(m, verb)
			if !ok {
				t.Fatalf("row %q not in matrix", verb)
			}
			if !r.Harnesses[HarnessUAT] {
				t.Errorf("verb %q invoked from the shared phase library must carry the uat harness; got %v", verb, r.Harnesses)
			}
			if r.Cadence != "nightly" {
				t.Errorf("cadence = %q, want nightly", r.Cadence)
			}
		})
	}
}

// TestOptedOutReservationIsNotLive guards the inverse: an explicit
// `nightly-intents: []` opts a reservation out of the batch, so its runner and
// the shared phase library must contribute no live coverage even though both
// are still on disk.
func TestOptedOutReservationIsNotLive(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	writeFixture(t, root, map[string]string{
		"infra/uat/reservations.yaml": "reservations:\n" +
			reservationRow("aws-h100", "ah1", "aws", "[]"),
	})

	wired, err := scanWiredUAT(root)
	if err != nil {
		t.Fatalf("scanWiredUAT: %v", err)
	}
	if len(wired.lanes) != 0 {
		t.Fatalf("opted-out registry must yield no wired lanes; got %+v", wired.lanes)
	}

	m := buildMatrix(t, root)
	for _, verb := range []string{"snapshot", "bundle", "evidence verify"} {
		r, ok := rowByItem(m, verb)
		if !ok {
			t.Fatalf("row %q not in matrix", verb)
		}
		if r.Harnesses[HarnessUAT] {
			t.Errorf("verb %q must not claim UAT coverage from an opted-out reservation", verb)
		}
	}
}

// TestScanWiredUATFromRegistry covers the registry → lanes mapping: the
// absent-field default, the explicit opt-out, and rows sharing a cloud collapsing
// into one lane whose intents are the union.
func TestScanWiredUATFromRegistry(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]string{
		"infra/uat/reservations.yaml": "reservations:\n" +
			reservationRow("aws-h100", "ah1", "aws", "[inference]") +
			reservationRow("aws-gb200", "ag2", "aws", "[training]") + // same cloud → one lane
			reservationRow("gcp-h100", "gh1", "gcp", "") + // absent → defaults to [training]
			reservationRow("azure-h100", "zh1", "azure", "[]"), // explicit opt-out
		".github/workflows/uat-aws.yaml": cloudPipelineFixture("aws", []string{"train"}, "serve"),
		".github/workflows/uat-gcp.yaml": cloudPipelineFixture("gcp", []string{"train", "serve"}),
	})

	wired, err := scanWiredUAT(root)
	if err != nil {
		t.Fatalf("scanWiredUAT: %v", err)
	}

	wantRunners := []string{
		filepath.Join("tests", "uat", "aws", "run"),
		filepath.Join("tests", "uat", "gcp", "run"),
	}
	if got := wired.runners(); !slices.Equal(got, wantRunners) {
		t.Fatalf("runners() = %v, want %v (azure opted out)", got, wantRunners)
	}

	// Both intents run somewhere, so both stacks are stood up nightly...
	for _, intent := range []string{"training", "inference"} {
		if !wired.runsIntent(intent) {
			t.Errorf("runsIntent(%q) = false, want true", intent)
		}
	}
	// ...but a journey is live only where its phase is an enabled step on a lane
	// enrolled for that intent. gcp enables serve but runs training only; aws
	// runs inference but has serve disabled — so the inference journey is dead.
	if !wired.runsJourney("training", "train") {
		t.Error("training/train must be live (aws and gcp both run it)")
	}
	if wired.runsJourney("inference", "serve") {
		t.Error("inference/serve must not be live: the only inference lane has serve disabled")
	}
}

// TestJourneyStepDisabledIsNotCovered guards the second half of the honesty
// contract: enrolling an intent proves the stack is deployed nightly, not that
// the journey runs. A commented-out CUJ step must leave the row stubbed, with a
// note pointing at the pipeline rather than the registry.
func TestJourneyStepDisabledIsNotCovered(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	writeFixture(t, root, map[string]string{
		// Enroll inference too, but leave the pipeline's serve step commented out.
		"infra/uat/reservations.yaml": "reservations:\n" +
			reservationRow("aws-h100", "ah1", "aws", "[training, inference]"),
	})

	m := buildMatrix(t, root)
	r, ok := rowByItem(m, "cuj2-inference-dynamo")
	if !ok {
		t.Fatal("cuj2-inference-dynamo row missing")
	}
	if r.Status != StatusStubbed {
		t.Errorf("status = %q, want %q (harnesses=%v)", r.Status, StatusStubbed, r.Harnesses)
	}
	if r.Harnesses[HarnessUAT] {
		t.Error("a disabled serve step must not claim live UAT coverage")
	}
	if r.Note != noteJourneyStepDisabled {
		t.Errorf("note = %q, want the disabled-step note", r.Note)
	}

	// Enabling the step flips it to live — the note is not a permanent verdict.
	write(t, root, ".github/workflows/uat-aws.yaml",
		cloudPipelineFixture("aws", []string{"prep", "install", "conformance", "train", "serve", "verify"}))
	r, _ = rowByItem(buildMatrix(t, root), "cuj2-inference-dynamo")
	if !r.Harnesses[HarnessUAT] || r.Status != StatusCovered {
		t.Errorf("enabling the serve step must yield live UAT coverage; got status=%q harnesses=%v", r.Status, r.Harnesses)
	}
}

// TestShellCommentedPhaseIsNotExecuted covers the gap YAML decoding alone leaves:
// a commented-out *step* vanishes from the parsed document, but a `run:` block is
// an opaque scalar, so a full-line or trailing shell comment inside one survives
// into step.Run and must not count as an executed phase.
func TestShellCommentedPhaseIsNotExecuted(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	writeFixture(t, root, map[string]string{
		"infra/uat/reservations.yaml": "reservations:\n" +
			reservationRow("aws-h100", "ah1", "aws", "[training, inference]"),
		// One live step, one full-line-commented runner call, and one trailing
		// inline comment that only mentions the serve runner.
		".github/workflows/uat-aws.yaml": "on:\n  workflow_call: {}\njobs:\n  uat:\n    steps:\n" +
			"      - run: ./tests/uat/aws/run train \"${TEST_CONFIG}\"\n" +
			"      - run: |\n" +
			"          echo skipping\n" +
			"          # ./tests/uat/aws/run serve \"${TEST_CONFIG}\"\n" +
			"      - run: echo done # ./tests/uat/aws/run serve \"${TEST_CONFIG}\"\n",
	})

	phases, err := scanLanePhases(root, "aws")
	if err != nil {
		t.Fatalf("scanLanePhases: %v", err)
	}
	if !phases["train"] {
		t.Error("live train step must be detected")
	}
	if phases["serve"] {
		t.Error("shell-commented serve call must not count as an executed phase")
	}

	r, ok := rowByItem(buildMatrix(t, root), "cuj2-inference-dynamo")
	if !ok {
		t.Fatal("cuj2-inference-dynamo row missing")
	}
	if r.Harnesses[HarnessUAT] {
		t.Error("a shell-commented CUJ step must not produce UAT coverage")
	}
}

func TestStripShellComment(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`./tests/uat/aws/run train`, `./tests/uat/aws/run train`},
		{`echo done # ./tests/uat/aws/run serve`, `echo done `},
		{`echo 'a\#b' # tail`, `echo 'a\#b' `},       // single-quoted \ is literal; real comment still cuts
		{`echo \#notacomment`, `echo \#notacomment`}, // unquoted \# is a literal #
		{`echo "x\#y" # tail`, `echo "x\#y" `},
	}
	for _, tt := range tests {
		if got := stripShellComment(tt.in); got != tt.want {
			t.Errorf("stripShellComment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestScanLanePhasesRejectsNonLocalCloud(t *testing.T) {
	// Enough .. segments that Clean climbs out of .github/workflows and leaves
	// a non-local relative path — the case filepath.IsLocal is meant to catch.
	_, err := scanLanePhases(t.TempDir(), "../../../../../../../etc/passwd")
	if err == nil {
		t.Fatal("expected invalid cloud path error")
	}
	if !strings.Contains(err.Error(), "invalid UAT cloud path") {
		t.Errorf("error = %q, want invalid UAT cloud path", err)
	}
}

// TestPhaseTokenIsWholeArgument pins the capture to the whole phase argument.
// A letters-only class would clip `serve-v2` to `serve` and credit cuj2 with a
// journey the pipeline never ran.
func TestPhaseTokenIsWholeArgument(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	write(t, root, ".github/workflows/uat-aws.yaml",
		cloudPipelineFixture("aws", []string{"train", "serve-v2"}))

	phases, err := scanLanePhases(root, "aws")
	if err != nil {
		t.Fatalf("scanLanePhases: %v", err)
	}
	if !phases["serve-v2"] {
		t.Errorf("phase token must span the whole argument; got %v", phases)
	}
	if phases["serve"] {
		t.Error("a truncated token must not register as the serve phase")
	}
}

// TestUnenrolledIntentUsesRegistryNote distinguishes the other stub cause: the
// intent is not in the nightly batch at all, so the fix is a registry edit.
func TestUnenrolledIntentUsesRegistryNote(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root) // aws enrolled for training only
	r, ok := rowByItem(buildMatrix(t, root), "cuj2-inference-dynamo")
	if !ok {
		t.Fatal("cuj2-inference-dynamo row missing")
	}
	if r.Note != noteIntentNotEnrolled {
		t.Errorf("note = %q, want the unenrolled-intent note", r.Note)
	}
}

// TestBuildMatrixFailsClosed asserts a missing or unusable input is an error
// rather than a matrix that quietly reports the lanes as uncovered.
func TestBuildMatrixFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		wantErr string
	}{
		{
			name:    "missing reservation registry",
			mutate:  func(t *testing.T, root string) { rm(t, root, registryRelPath) },
			wantErr: registryRelPath,
		},
		{
			name:    "invalid reservation registry",
			mutate:  func(t *testing.T, root string) { write(t, root, registryRelPath, "reservations: []\n") },
			wantErr: "no reservations",
		},
		{
			name:    "missing pipeline for an enrolled cloud",
			mutate:  func(t *testing.T, root string) { rm(t, root, ".github/workflows/uat-aws.yaml") },
			wantErr: "uat-aws.yaml",
		},
		{
			name:    "missing runner for an enrolled cloud",
			mutate:  func(t *testing.T, root string) { rm(t, root, "tests/uat/aws/run") },
			wantErr: "UAT signal path is missing",
		},
		{
			name: "missing shared phase library",
			mutate: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, "tests", "uat", "lib")); err != nil {
					t.Fatalf("remove phase library: %v", err)
				}
			},
			wantErr: "UAT signal path is missing",
		},
		{
			name: "enrolled pipeline with no enabled runner step",
			mutate: func(t *testing.T, root string) {
				write(t, root, ".github/workflows/uat-aws.yaml", cloudPipelineFixture("aws", nil, "train", "serve"))
			},
			wantErr: "nightly-intents: []",
		},
		{
			name:    "missing nightly batch workflow",
			mutate:  func(t *testing.T, root string) { rm(t, root, nightlyBatchRelPath) },
			wantErr: nightlyBatchRelPath,
		},
		{
			name: "nightly batch without previous_n input",
			mutate: func(t *testing.T, root string) {
				write(t, root, nightlyBatchRelPath, "on:\n  workflow_dispatch:\n    inputs: {}\njobs: {}\n")
			},
			wantErr: previousNInput,
		},
		{
			name: "nightly batch without schedule previous_n fallback",
			mutate: func(t *testing.T, root string) {
				write(t, root, nightlyBatchRelPath, "on:\n  workflow_dispatch:\n    inputs:\n"+
					"      previous_n:\n        type: string\n        default: '1'\njobs: {}\n")
			},
			wantErr: "schedule fallback",
		},
		{
			name: "previous_n default disagrees with schedule fallback",
			mutate: func(t *testing.T, root string) {
				write(t, root, nightlyBatchRelPath, "name: UAT Nightly Batch\n"+
					"on:\n  workflow_dispatch:\n    inputs:\n"+
					"      previous_n:\n        type: string\n        default: '1'\n"+
					"jobs:\n  drive:\n    steps:\n"+
					"      - env:\n"+
					"          PREVIOUS_N: ${{ inputs.previous_n || '2' }}\n"+
					"        run: echo ok\n")
			},
			wantErr: "disagrees",
		},
		{
			name:    "non-integer previous_n literals",
			mutate:  func(t *testing.T, root string) { write(t, root, nightlyBatchRelPath, nightlyBatchFixture("many")) },
			wantErr: previousNInput,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			wiredTrainingFixture(t, root)
			tt.mutate(t, root)

			if _, err := BuildMatrix(root); err == nil {
				t.Fatal("BuildMatrix must fail closed, got nil error")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestVersionAxisFromNightlyBatch(t *testing.T) {
	tests := []struct {
		previousN string
		want      int
		wantProse string
	}{
		{"0", 0, "built from source. Each version"},
		{"1", 1, "plus the previous stable release"},
		{"3", 3, "plus the previous 3 stable releases"},
	}
	for _, tt := range tests {
		t.Run("previous_n="+tt.previousN, func(t *testing.T) {
			root := t.TempDir()
			wiredTrainingFixture(t, root)
			write(t, root, nightlyBatchRelPath, nightlyBatchFixture(tt.previousN))

			m := buildMatrix(t, root)
			if m.VersionAxis.PreviousReleases != tt.want {
				t.Errorf("PreviousReleases = %d, want %d", m.VersionAxis.PreviousReleases, tt.want)
			}
			if out := Render(m, true, false); !strings.Contains(out, tt.wantProse) {
				t.Errorf("rendered axis prose missing %q", tt.wantProse)
			}
		})
	}
}

func TestRenderDeterministic(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	// Two independent builds must render byte-identical output despite the
	// map-backed harness sets and verb scan.
	a := Render(buildMatrix(t, root), true, false)
	b := Render(buildMatrix(t, root), true, false)
	if a != b {
		t.Fatal("Render output is not deterministic across runs")
	}
}

func TestRenderMDXSafe(t *testing.T) {
	// The page must stay inside the check-docs-mdx gate: no HTML comments, no
	// bare braces, no autolinks in the generated body.
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	out := Render(buildMatrix(t, root), true, false)
	for _, bad := range []string{"<!--", "{", "<http://", "<https://"} {
		if strings.Contains(out, bad) {
			t.Errorf("generated body contains MDX-unsafe token %q", bad)
		}
	}
}

func TestNoTitleOmitsH1(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	m := buildMatrix(t, root)
	with := Render(m, true, false)
	without := Render(m, true, true)
	if !strings.Contains(with, "# Recipe & CLI Coverage Matrix") {
		t.Error("expected H1 when noTitle=false")
	}
	if strings.Contains(without, "# Recipe & CLI Coverage Matrix") {
		t.Error("H1 must be omitted when noTitle=true")
	}
}

// TestRepoUATLanesAreVisible runs the scanner against the real repository. It is
// the standing anti-rot guard for #1977: if the reservation registry moves, or
// the phase commands migrate out of tests/uat/lib again, the nightly lanes go
// invisible — and that must fail here rather than silently downgrade the
// committed matrix on the next regeneration.
func TestRepoUATLanesAreVisible(t *testing.T) {
	root := filepath.Join("..", "..")

	wired, err := scanWiredUAT(root)
	if err != nil {
		t.Fatalf("scanWiredUAT against the repo: %v", err)
	}
	runners := wired.runners()
	if len(runners) == 0 {
		t.Fatal("no nightly-enrolled UAT runners resolved from " + registryRelPath)
	}
	for _, runner := range runners {
		if _, statErr := os.Stat(filepath.Join(root, runner)); statErr != nil {
			t.Errorf("enrolled runner %s does not exist: %v", runner, statErr)
		}
	}

	// These verbs are invoked only from the shared phase library, so each one
	// proves the composite layer is still being scanned.
	m := buildMatrix(t, root)
	for _, verb := range []string{"snapshot", "bundle", "evidence verify"} {
		r, ok := rowByItem(m, verb)
		if !ok {
			t.Fatalf("row %q not in matrix", verb)
		}
		if !r.Harnesses[HarnessUAT] {
			t.Errorf("verb %q lost its nightly UAT signal; the wiring scanner is blind to the enrolled lanes again (#1977)", verb)
		}
	}
}

// rm removes a repo-relative path under root.
func rm(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
}

// write overwrites a repo-relative path under root.
func write(t *testing.T, root, rel, content string) {
	t.Helper()
	writeFixture(t, root, map[string]string{rel: content})
}

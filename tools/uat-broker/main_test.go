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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/uatbroker"
)

const testRegistry = `
reservations:
  - name: aws-h100
    slug: ah1
    cloud: aws
    reservation-id: cr-0cbe491320188dfa6
    accelerator: h100
    gpu-count: 8
    cluster-config-path: tests/uat/aws/cluster-config.yaml
    test-config-dir: tests/uat/aws/tests
    nightly-intents: [training, inference]
    daytime-intent: training
  - name: gcp-h100
    slug: gh1
    cloud: gcp
    reservation-id: projects/p/reservations/r
    accelerator: h100
    gpu-count: 8
    cluster-config-path: tests/uat/gcp/cluster-config.yaml
    test-config-dir: tests/uat/gcp/tests
    nightly-intents: [training, inference]
    daytime-intent: inference
`

func writeRegistry(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "reservations.yaml")
	if err := os.WriteFile(p, []byte(testRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testCompat floors gcp training at v2.0.0; its min-release is on line 4.
const testCompat = `floors:
  - lane: gcp
    intents: [training]
    min-release: v2.0.0
    reason: >-
      new fixture
      field
`

func writeCompat(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "compat.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func invoke(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = parseAndRun(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestReservationsResolve(t *testing.T) {
	reg := writeRegistry(t)
	code, stdout, stderr := invoke("", "reservations", "--file", reg, "--name", "aws-h100")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		// slug feeds needs.resolve.outputs.slug — the daytime cluster name's
		// discovery key (ADR-017) — so the resolver must emit it.
		"slug=ah1",
		"cloud=aws",
		"reservation-id=cr-0cbe491320188dfa6",
		"accelerator=h100",
		"gpu-count=8",
		"cluster-config-path=tests/uat/aws/cluster-config.yaml",
		"test-config-dir=tests/uat/aws/tests",
		"nightly-intents=training,inference",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// TestReservationsResolveNightlyIntentsDefaults verifies the resolved output
// reports the training default when a row omits nightly-intents, so the nightly
// batch's intent loop is never handed an empty list.
func TestReservationsResolveNightlyIntentsDefaults(t *testing.T) {
	const reg = `
reservations:
  - name: aws-h100
    slug: ah1
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`
	p := filepath.Join(t.TempDir(), "reservations.yaml")
	if err := os.WriteFile(p, []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invoke("", "reservations", "--file", p, "--name", "aws-h100")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "nightly-intents=training") {
		t.Errorf("stdout missing resolved default nightly-intents=training\ngot:\n%s", stdout)
	}
}

// TestReservationsResolveNightlyIntentsOptOut asserts an explicit empty
// nightly-intents list resolves to an EMPTY value (nightly opt-out), while
// remaining a present key — the nightly batch skips the leg on an empty
// value but fails closed when the key is missing entirely.
func TestReservationsResolveNightlyIntentsOptOut(t *testing.T) {
	const reg = `
reservations:
  - name: azure-h100
    slug: zh1
    cloud: azure
    accelerator: h100
    gpu-count: 8
    cluster-config-path: tests/uat/azure/cluster-config.yaml
    test-config-dir: tests/uat/azure/tests
    nightly-intents: []
`
	p := filepath.Join(t.TempDir(), "reservations.yaml")
	if err := os.WriteFile(p, []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invoke("", "reservations", "--file", p, "--name", "azure-h100")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "nightly-intents=\n") {
		t.Errorf("stdout missing empty nightly-intents= line (opt-out)\ngot:\n%s", stdout)
	}
	if !strings.Contains(stdout, "cloud=azure\n") {
		t.Errorf("stdout missing cloud=azure\ngot:\n%s", stdout)
	}
	if !strings.Contains(stdout, "reservation-id=\n") {
		t.Errorf("stdout missing empty reservation-id= line\ngot:\n%s", stdout)
	}
}

func TestReservationsList(t *testing.T) {
	reg := writeRegistry(t)
	code, stdout, _ := invoke("", "reservations", "--file", reg, "--list")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	got := strings.Fields(stdout)
	if len(got) != 2 || got[0] != "aws-h100" || got[1] != "gcp-h100" {
		t.Errorf("--list = %v, want [aws-h100 gcp-h100]", got)
	}
}

func TestReservationsDaytime(t *testing.T) {
	reg := writeRegistry(t)
	code, stdout, stderr := invoke("", "reservations", "--file", reg, "--daytime")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var got []uatbroker.DaytimeAssignment
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--daytime output is not valid JSON: %v\ngot:\n%s", err, stdout)
	}
	want := []uatbroker.DaytimeAssignment{
		{Reservation: "aws-h100", Intent: "training"},
		{Reservation: "gcp-h100", Intent: "inference"},
	}
	if len(got) != len(want) {
		t.Fatalf("--daytime = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("--daytime[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReservationsExitCodes(t *testing.T) {
	reg := writeRegistry(t)
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no name, no list", []string{"reservations", "--file", reg}, errors.ExitInvalidInput},
		{"missing reservation", []string{"reservations", "--file", reg, "--name", "nope"}, errors.ExitNotFound},
		{"unknown registry file", []string{"reservations", "--file", "/no/such.yaml", "--name", "aws-h100"}, errors.ExitInvalidInput},
		{"bad flag", []string{"reservations", "--bogus"}, errors.ExitInvalidInput},
		{"list and name conflict", []string{"reservations", "--file", reg, "--list", "--name", "aws-h100"}, errors.ExitInvalidInput},
		{"daytime and name conflict", []string{"reservations", "--file", reg, "--daytime", "--name", "aws-h100"}, errors.ExitInvalidInput},
		{"daytime and list conflict", []string{"reservations", "--file", reg, "--daytime", "--list"}, errors.ExitInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := invoke("", tt.args...)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestScheduleJSON(t *testing.T) {
	reg := writeRegistry(t)
	stdin := "v1.0.0\nv2.0.0\nv1.5.0-rc1\nv1.5.0\n"
	code, stdout, stderr := invoke(stdin, "schedule", "--file", reg, "--compat", writeCompat(t, "floors: []\n"), "--previous-n", "2")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}

	var schedule map[string][]uatbroker.Cell
	if err := json.Unmarshal([]byte(stdout), &schedule); err != nil {
		t.Fatalf("schedule output is not valid JSON: %v\ngot:\n%s", err, stdout)
	}
	for _, res := range []string{"aws-h100", "gcp-h100"} {
		cells := schedule[res]
		if len(cells) != 3 {
			t.Fatalf("%s has %d cells, want 3 (main + 2)", res, len(cells))
		}
		if !cells[0].IsMain {
			t.Errorf("%s cell[0] is not main", res)
		}
		if cells[1].AICRVersion != "v2.0.0" || cells[2].AICRVersion != "v1.5.0" {
			t.Errorf("%s release order = [%s %s], want [v2.0.0 v1.5.0]", res, cells[1].AICRVersion, cells[2].AICRVersion)
		}
	}
}

func TestScheduleReservationsOverride(t *testing.T) {
	// --reservations selects a subset of the registry (still loaded, so each
	// cell can carry its eligible nightly intents); a name must exist in --file.
	reg := writeRegistry(t)
	code, stdout, stderr := invoke("v1.0.0\n", "schedule", "--file", reg, "--compat", writeCompat(t, testCompat), "--reservations", "aws-h100", "--previous-n", "1")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var schedule map[string][]uatbroker.Cell
	if err := json.Unmarshal([]byte(stdout), &schedule); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := schedule["aws-h100"]; !ok || len(schedule) != 1 {
		t.Errorf("schedule keys = %v, want only aws-h100", schedule)
	}
}

func TestScheduleRejectsDuplicateReservations(t *testing.T) {
	// Duplicate --reservations must fail loudly rather than silently collapse.
	reg := writeRegistry(t)
	code, _, _ := invoke("v1.0.0\n", "schedule", "--file", reg, "--compat", writeCompat(t, testCompat), "--reservations", "aws-h100,aws-h100")
	if code != errors.ExitInvalidInput {
		t.Errorf("exit = %d, want %d for duplicate --reservations", code, errors.ExitInvalidInput)
	}
}

func TestScheduleRejectsUnknownReservation(t *testing.T) {
	// A --reservations name absent from the registry fails loudly (NOT_FOUND)
	// rather than scheduling an intent-less row.
	reg := writeRegistry(t)
	code, _, _ := invoke("v1.0.0\n", "schedule", "--file", reg, "--reservations", "nope-h100")
	if code != errors.ExitNotFound {
		t.Errorf("exit = %d, want %d for unknown --reservations name", code, errors.ExitNotFound)
	}
}

// TestScheduleEmitsPerCellIntents verifies the schedule attaches each cell's
// eligible intents and applies the compat floor: with gcp training floored at
// v2.0.0, main carries both intents, the v1.0.0 release carries only inference
// and reports the skip, and --ignore-floor-lines un-gates the row.
func TestScheduleEmitsPerCellIntents(t *testing.T) {
	reg := writeRegistry(t)
	compat := writeCompat(t, testCompat)
	tests := []struct {
		name        string
		extra       []string
		wantRelease []string
		wantSkipped []uatbroker.SkippedIntent
	}{
		{
			name:        "floor honored",
			wantRelease: []string{uatbroker.IntentInference},
			wantSkipped: []uatbroker.SkippedIntent{{Intent: "training", Floor: "v2.0.0", Lane: "gcp", Reason: "new fixture field"}},
		},
		{
			name:        "ignored floor line runs the cell",
			extra:       []string{"--ignore-floor-lines", "4"},
			wantRelease: []string{uatbroker.IntentTraining, uatbroker.IntentInference},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"schedule", "--file", reg, "--compat", compat, "--reservations", "gcp-h100", "--previous-n", "1"}, tt.extra...)
			code, stdout, stderr := invoke("v1.0.0\n", args...)
			if code != 0 {
				t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
			}
			var schedule map[string][]uatbroker.Cell
			if err := json.Unmarshal([]byte(stdout), &schedule); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			cells := schedule["gcp-h100"]
			if len(cells) != 2 {
				t.Fatalf("got %d cells, want 2 (main + v1.0.0)", len(cells))
			}
			if !cells[0].IsMain || !reflect.DeepEqual(cells[0].Intents, []string{uatbroker.IntentTraining, uatbroker.IntentInference}) || cells[0].Skipped != nil {
				t.Errorf("main cell = %+v, want both intents and no skips", cells[0])
			}
			if cells[1].AICRVersion != "v1.0.0" || !reflect.DeepEqual(cells[1].Intents, tt.wantRelease) {
				t.Errorf("v1.0.0 cell intents = %v, want %v", cells[1].Intents, tt.wantRelease)
			}
			if !reflect.DeepEqual(cells[1].Skipped, tt.wantSkipped) {
				t.Errorf("v1.0.0 cell skipped = %+v, want %+v", cells[1].Skipped, tt.wantSkipped)
			}
		})
	}
	// The JSON key is lowercase "skipped" and omitted when empty.
	_, stdout, _ := invoke("v1.0.0\n", "schedule", "--file", reg, "--compat", compat, "--reservations", "gcp-h100", "--previous-n", "1")
	if !strings.Contains(stdout, `"skipped": [`) || strings.Count(stdout, `"skipped"`) != 1 {
		t.Errorf("want exactly one skipped key in schedule JSON, got:\n%s", stdout)
	}
}

func TestScheduleCompatErrors(t *testing.T) {
	reg := writeRegistry(t)
	compat := writeCompat(t, testCompat)
	tests := []struct {
		name string
		args []string
	}{
		{"missing compat file", []string{"--compat", "/no/such/compat.yaml"}},
		{"compat lane not in registry", []string{"--compat", writeCompat(t, strings.ReplaceAll(testCompat, "lane: gcp", "lane: kind"))}},
		{"ignore line matches no floor", []string{"--compat", compat, "--ignore-floor-lines", "5"}},
		{"ignore line not a number", []string{"--compat", compat, "--ignore-floor-lines", "four"}},
		{"ignore lines empty after trim", []string{"--compat", compat, "--ignore-floor-lines", " , "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := invoke("v1.0.0\n", append([]string{"schedule", "--file", reg}, tt.args...)...)
			if code != errors.ExitInvalidInput {
				t.Errorf("exit = %d, want %d", code, errors.ExitInvalidInput)
			}
		})
	}
}

func TestCompatList(t *testing.T) {
	reg := writeRegistry(t)
	body := testCompat + "  - lane: aws\n    intents: [training, inference]\n    min-release: v1.0.0\n    reason: r\n"
	code, stdout, stderr := invoke("", "compat", "list", "--registry", reg, "--file", writeCompat(t, body))
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	want := "4\tgcp\ttraining\tv2.0.0\n10\taws\ttraining\tv1.0.0\n10\taws\tinference\tv1.0.0\n"
	if stdout != want {
		t.Errorf("compat list =\n%q\nwant\n%q", stdout, want)
	}
}

func TestCompatCheck(t *testing.T) {
	reg := writeRegistry(t)
	compat := writeCompat(t, testCompat)
	writeContaining := func(body string) string {
		p := filepath.Join(t.TempDir(), "containing.tsv")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name       string
		tags       string
		containing string
		extra      []string
		wantKinds  []string
	}{
		{"sound floor", "v1.0.0\nv2.0.0\n", "4\tv2.0.0\n", nil, []string{}},
		{"over-high floor", "v1.0.0\nv2.0.0\n", "4\tv1.0.0\tv2.0.0\n", nil, []string{uatbroker.FloorProblemOverHigh}},
		{"missing containing record", "v1.0.0\nv2.0.0\n", "", nil, []string{uatbroker.FloorProblemInconclusive}},
		{"no stable tags", "v2.0.0-rc1\n", "4\n", nil, []string{uatbroker.FloorProblemInconclusive}},
		{"inert with window", "v2.0.0\nv3.0.0\n", "4\tv2.0.0\n", []string{"--previous-n", "2"}, []string{uatbroker.FloorProblemInert}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"compat", "check", "--registry", reg, "--file", compat, "--containing", writeContaining(tt.containing)}, tt.extra...)
			code, stdout, stderr := invoke(tt.tags, args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 even with problems (stderr: %s)", code, stderr)
			}
			var problems []uatbroker.FloorProblem
			if err := json.Unmarshal([]byte(stdout), &problems); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, stdout)
			}
			kinds := make([]string, 0, len(problems))
			for _, p := range problems {
				kinds = append(kinds, p.Kind)
				if p.Line != 4 || p.Lane != "gcp" || p.Message == "" {
					t.Errorf("problem = %+v, want line 4, lane gcp, a message", p)
				}
			}
			if !reflect.DeepEqual(kinds, tt.wantKinds) {
				t.Errorf("kinds = %v, want %v", kinds, tt.wantKinds)
			}
		})
	}
	// An empty result is the JSON array [], never null.
	_, stdout, _ := invoke("v2.0.0\n", "compat", "check", "--registry", reg, "--file", compat, "--containing", writeContaining("4\tv2.0.0\n"))
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty check output = %q, want []", stdout)
	}
}

func TestCompatGate(t *testing.T) {
	reg := writeRegistry(t)
	compat := writeCompat(t, testCompat)
	base := []string{"compat", "gate", "--registry", reg, "--file", compat}
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		// uat-run.yaml keys its allow_below_compat_floor override on this
		// phrase, so it is part of the CLI contract.
		{"below floor", []string{"--cloud", "gcp", "--intent", "training", "--version", "v1.0.0"}, errors.ExitInvalidInput, "", "is below the harness-compat floor v2.0.0"},
		{"at floor", []string{"--cloud", "gcp", "--intent", "training", "--version", "v2.0.0"}, 0, "ok:", ""},
		{"no floor", []string{"--cloud", "aws", "--intent", "training", "--version", "v0.1.0"}, 0, "ok: no harness-compat floor", ""},
		{"missing version", []string{"--cloud", "gcp", "--intent", "training"}, errors.ExitInvalidInput, "", "--version is required"},
		{"bad version", []string{"--cloud", "gcp", "--intent", "training", "--version", "main"}, errors.ExitInvalidInput, "", "not semver"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := invoke("", append(append([]string{}, base...), tt.args...)...)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stdout, tt.wantStdout) || !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stdout %q / stderr %q, want %q / %q", stdout, stderr, tt.wantStdout, tt.wantStderr)
			}
		})
	}
}

func TestCompatExitCodes(t *testing.T) {
	reg := writeRegistry(t)
	compat := writeCompat(t, testCompat)
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no subcommand", []string{"compat"}, errors.ExitInvalidInput},
		{"unknown subcommand", []string{"compat", "frob"}, errors.ExitInvalidInput},
		{"list help", []string{"compat", "list", "-h"}, 0},
		{"list bad flag", []string{"compat", "list", "--bogus"}, errors.ExitInvalidInput},
		{"list positional", []string{"compat", "list", "--registry", reg, "--file", compat, "extra"}, errors.ExitInvalidInput},
		{"list missing registry", []string{"compat", "list", "--registry", "/no/such.yaml", "--file", compat}, errors.ExitInvalidInput},
		{"list missing compat", []string{"compat", "list", "--registry", reg, "--file", "/no/such.yaml"}, errors.ExitInvalidInput},
		{"check without containing", []string{"compat", "check", "--registry", reg, "--file", compat}, errors.ExitInvalidInput},
		{"check missing containing file", []string{"compat", "check", "--registry", reg, "--file", compat, "--containing", "/no/such.tsv"}, errors.ExitInvalidInput},
		{"check bad compat", []string{"compat", "check", "--registry", reg, "--file", "/no/such.yaml", "--containing", compat}, errors.ExitInvalidInput},
		{"check malformed containing", []string{"compat", "check", "--registry", reg, "--file", compat, "--containing", reg}, errors.ExitInvalidInput},
		{"check help", []string{"compat", "check", "-h"}, 0},
		{"gate help", []string{"compat", "gate", "-h"}, 0},
		{"gate bad compat", []string{"compat", "gate", "--registry", reg, "--file", "/no/such.yaml", "--cloud", "gcp", "--intent", "training", "--version", "v1.0.0"}, errors.ExitInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := invoke("", tt.args...)
			if code != tt.want {
				t.Errorf("exit = %d, want %d", code, tt.want)
			}
		})
	}
}

func TestCompatPropagatesWriteError(t *testing.T) {
	reg := writeRegistry(t)
	compat := writeCompat(t, testCompat)
	containing := filepath.Join(t.TempDir(), "c.tsv")
	if err := os.WriteFile(containing, []byte("4\tv2.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"compat", "list", "--registry", reg, "--file", compat},
		{"compat", "check", "--registry", reg, "--file", compat, "--containing", containing},
		{"compat", "gate", "--registry", reg, "--file", compat, "--cloud", "gcp", "--intent", "training", "--version", "v2.0.0"},
	} {
		t.Run(args[1], func(t *testing.T) {
			if code := parseAndRun(context.Background(), args, strings.NewReader("v2.0.0\n"), failingWriter{}, io.Discard); code == 0 {
				t.Error("expected a non-zero exit when the stdout write fails")
			}
		})
	}
}

func TestReadTagsRejectsOversizedInput(t *testing.T) {
	// Exercise the OOM-safeguard branch: input larger than maxTagsBytes.
	big := strings.Repeat("v1.0.0\n", int(maxTagsBytes/7)+1)
	if _, err := readTags(context.Background(), strings.NewReader(big)); err == nil {
		t.Fatal("expected an error for a tag list exceeding the size limit")
	}
}

func TestReadTagsCanceledContext(t *testing.T) {
	// A canceled context must interrupt a blocking stdin read rather than hang.
	pr, pw := io.Pipe() // never written → Read blocks until ctx cancellation
	defer pw.Close()    // release the reader goroutine when the test ends
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTags(ctx, pr); err == nil {
		t.Fatal("expected an error when the context is canceled during the stdin read")
	}
}

func TestScheduleWarnsOnNoReleases(t *testing.T) {
	// previous-n>0 with no usable tags must warn (but still succeed main-only).
	reg := writeRegistry(t)
	code, _, stderr := invoke("not-a-tag\n", "schedule", "--file", reg, "--compat", writeCompat(t, testCompat), "--reservations", "aws-h100", "--previous-n", "2")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr, "main-only") {
		t.Errorf("expected a main-only warning on stderr, got: %s", stderr)
	}
}

func TestScheduleEmptyWarning(t *testing.T) {
	// --include-main=false with no usable tags yields an empty schedule, which
	// must be reported as empty/nothing-to-run, NOT as "main-only".
	reg := writeRegistry(t)
	code, _, stderr := invoke("not-a-tag\n", "schedule", "--file", reg, "--compat", writeCompat(t, testCompat), "--reservations", "aws-h100", "--include-main=false", "--previous-n", "2")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr, "empty schedule") {
		t.Errorf("expected an empty-schedule warning, got: %s", stderr)
	}
	if strings.Contains(stderr, "main-only") {
		t.Errorf("must not claim main-only when --include-main=false, got: %s", stderr)
	}
}

func TestRejectsPositionalArgs(t *testing.T) {
	reg := writeRegistry(t)
	tests := [][]string{
		{"reservations", "--file", reg, "--list", "extra"},
		{"reservations", "--file", reg, "--name", "aws-h100", "stray"},
		{"schedule", "--reservations", "aws-h100", "bogus"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, _ := invoke("", args...)
			if code != errors.ExitInvalidInput {
				t.Errorf("exit = %d, want %d for trailing positional args", code, errors.ExitInvalidInput)
			}
		})
	}
}

// failingWriter fails every Write, simulating a broken pipe or an unwritable
// $GITHUB_OUTPUT target.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReservationsPropagatesWriteError(t *testing.T) {
	reg := writeRegistry(t)
	tests := []struct {
		name string
		args []string
	}{
		{"resolve", []string{"reservations", "--file", reg, "--name", "aws-h100"}},
		{"list", []string{"reservations", "--file", reg, "--list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := parseAndRun(context.Background(), tt.args, strings.NewReader(""), failingWriter{}, io.Discard)
			if code == 0 {
				t.Error("expected a non-zero exit when the stdout write fails")
			}
		})
	}
}

func TestTopLevelDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, errors.ExitInvalidInput},
		{"unknown subcommand", []string{"frobnicate"}, errors.ExitInvalidInput},
		{"help", []string{"help"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := invoke("", tt.args...)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
		})
	}
}

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

package validator

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The two phase summaries runPhase emits are the only place an operator is
// told how many checks never ran. Both counts are guarded here, because the
// number is the entire value of the line: a summary that prints phase,
// status and a validator total while withholding the skipped count reads as
// a complete accounting of a suite that was in fact narrowed, and --skip-check
// makes that narrowing routine rather than exceptional.
//
// Fixture arithmetic, chosen so `skipped` is the ONLY quantity in reach with
// its value at each site (an assertion that could be satisfied by a sibling
// count cannot tell you which field it read):
//
//	conformance catalog entries      5   (+1 deployment entry, off-phase)
//	declared for the phase           5
//	skipChecks entries               4   (3 conformance + 1 deployment-only)
//	  -> withheld from THIS phase    3   = Summary.Skipped
//	  -> actually selected to run    2
//	both selected entries fail to deploy (the fake clientset refuses the
//	Job apply) -> ExitCode -1 -> CTRF "other"
//
//	"running validation phase":  catalog=5  selected=2  skipped=4
//	"phase completed":           validators=5 passed=0 failed=0 skipped=3
//
// At the "phase completed" site every other integer available to a mistyped
// or mutated expression differs from 3: Tests=5, Passed=0, Failed=0, Other=2,
// Pending=0, len(SkipChecks)=4, len(entries)=2. The off-phase skip name is
// what pulls len(SkipChecks) off Summary.Skipped; without it the two counts
// would both be 3 and the site-674 assertion could not distinguish them.
func TestRunPhaseLogsSkippedCount(t *testing.T) {
	cat := &catalog.ValidatorCatalog{
		Validators: []catalog.ValidatorEntry{
			{Name: "gpu-operator-health", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "dra-support", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "slinky-slurm-health", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "node-feature-discovery", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "gpu-nodes-ready", Phase: "conformance", Image: "example.com/aicr/check:v1"},
			{Name: "operator-health", Phase: "deployment", Image: "example.com/aicr/check:v1"},
		},
	}
	declared := map[Phase][]string{
		PhaseConformance: {
			"gpu-operator-health", "dra-support", "slinky-slurm-health",
			"node-feature-discovery", "gpu-nodes-ready",
		},
	}

	v := New(
		WithVersion("1.0.0"),
		WithCleanup(false),
		WithSkipChecks(
			// Three withheld from the conformance phase under test...
			"gpu-operator-health", "dra-support", "slinky-slurm-health",
			// ...and one that names a deployment check, so it counts toward
			// len(v.SkipChecks) without touching this phase's Summary.Skipped.
			"operator-health",
		),
	)

	logs := captureSlog(t)
	_, err := v.runPhase(
		t.Context(),
		refusingJobApplyClient(),
		informers.NewSharedInformerFactory(k8sfake.NewSimpleClientset(), 0),
		cat,
		PhaseConformance,
		validationWithChecks(declared),
	)
	if err != nil {
		t.Fatalf("runPhase() error = %v, want nil", err)
	}

	// validator.go:674 -- the count on the phase's closing summary. This is
	// the site --no-cluster never reaches, so nothing else in the suite
	// covers it.
	completed := findLogRecord(t, logs.Bytes(), "phase completed")
	assertLogInt(t, completed, "phase completed", "skipped", 3)
	// Pinned alongside so a mutant that redirects "skipped" at one of them
	// has to disagree with the value it copies, not merely with 3.
	assertLogInt(t, completed, "phase completed", "validators", 5)
	assertLogInt(t, completed, "phase completed", "passed", 0)
	assertLogInt(t, completed, "phase completed", "failed", 0)

	// validator.go:531 -- the opening summary counts the skip LIST, not this
	// phase's withheld checks, which is why 4 and not 3.
	running := findLogRecord(t, logs.Bytes(), "running validation phase")
	assertLogInt(t, running, "running validation phase", "skipped", 4)
	assertLogInt(t, running, "running validation phase", "catalog", 5)
	assertLogInt(t, running, "running validation phase", "selected", 2)
}

// refusingJobApplyClient returns a fake clientset whose Job server-side-apply
// always fails. Every selected entry then takes runPhase's deploy-failure
// branch, which records an "other" result and moves on: the phase completes
// immediately with no informer sync and no Job wait, so the summary counts
// are reached deterministically and the test cannot hang on a timeout.
func refusingJobApplyClient() *k8sfake.Clientset {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("patch", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(stderrors.New("fake clientset: Job apply refused"))
	})
	return cs
}

// captureSlog redirects the default logger into a buffer of JSON records for
// the duration of the test. JSON rather than text so an attribute is read by
// name and compared as a number, instead of substring-matching "skipped=3"
// out of a line where several counts are printed side by side.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// findLogRecord returns the first captured record whose msg is want, failing
// the test when no record carries it.
func findLogRecord(t *testing.T, logs []byte, want string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(logs))
	dec.UseNumber()
	for {
		var rec map[string]any
		switch err := dec.Decode(&rec); {
		case stderrors.Is(err, io.EOF):
			t.Fatalf("no log record with msg %q; captured:\n%s", want, logs)
		case err != nil:
			t.Fatalf("decoding captured log: %v; captured:\n%s", err, logs)
		}
		if msg, ok := rec["msg"].(string); ok && msg == want {
			return rec
		}
	}
}

// assertLogInt asserts the record carries key with exactly want. A missing
// key is reported distinctly from a wrong value, so deleting an attribute and
// mis-sourcing one are not confusable failures.
func assertLogInt(t *testing.T, rec map[string]any, msg, key string, want int64) {
	t.Helper()
	raw, ok := rec[key]
	if !ok {
		t.Fatalf("%q record has no %q attribute; record = %v", msg, key, rec)
	}
	num, ok := raw.(json.Number)
	if !ok {
		t.Fatalf("%q record %q = %v (%T), want a number", msg, key, raw, raw)
	}
	got, err := num.Int64()
	if err != nil {
		t.Fatalf("%q record %q = %v, not an integer: %v", msg, key, raw, err)
	}
	if got != want {
		t.Errorf("%q record %q = %d, want %d", msg, key, got, want)
	}
}

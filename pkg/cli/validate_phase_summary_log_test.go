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
	stderrors "errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// phaseLogOverlayYAML is the smallest leaf overlay that hydrates against the
// embedded recipe data, so the run reaches the phase loop instead of failing
// catalog load. Two phases of the resulting recipe declare different numbers
// of checks, which is what lets the assertion below tell a per-phase count
// from a constant.
const phaseLogOverlayYAML = `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: aicr-phase-log-test
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    intent: training
  componentRefs: []
`

// TestRunValidationLogsSkippedCountPerPhase guards validate.go's per-phase
// summary. The line's whole reason to exist is the count: --skip-check lets a
// phase report a green status having withheld most of its checks, and an
// operator reading a summary of phase, status and duration alone has no way
// to see that. Delete the "skipped" pair and this test fails.
//
// The expected value is derived from a DIFFERENT artifact than the one the log
// reads: the summary line prints report.Results.Summary.Skipped, while the
// expectation here counts the skipped entries in the CTRF document the same
// run serialized to disk. Deriving it rather than pinning 5 and 13 keeps an
// unrelated recipe edit from failing a log-format guard, and the two phases
// are asserted to disagree so a constant cannot satisfy both.
//
// KNOWN LIMIT, and the reason this is the weaker of the two guards on the
// skipped count (pkg/validator's TestRunPhaseLogsSkippedCount is the strong
// one): --no-cluster is the only route to this line that needs no live
// cluster, and it reports every declared check as skipped, so Summary.Tests
// and Summary.Skipped are necessarily equal here. A mutation that swapped
// Skipped for Tests would survive this test. Nothing available offline
// separates them, because separating them requires a check that actually ran.
func TestRunValidationLogsSkippedCountPerPhase(t *testing.T) {
	client, rec := phaseLogRecipe(t)
	snap := phaseLogSnapshot()
	dir := t.TempDir()

	// One single-phase run each: MergeReports returns a lone phase report
	// unmerged, so each output file is exactly the phase's own document and
	// the count can be attributed to the phase that produced it.
	counts := map[validator.Phase]int64{}
	for _, phase := range []validator.Phase{validator.PhaseDeployment, validator.PhaseConformance} {
		out := filepath.Join(dir, string(phase)+".yaml")

		logs := captureJSONLogs(t)
		err := runValidation(t.Context(), client, rec, snap, validationConfig{
			phases:              []validator.Phase{phase},
			runID:               "20260908-000000-0123456789abcdef",
			output:              out,
			outFormat:           serializer.FormatYAML,
			validationNamespace: "aicr-validation-test",
			cleanup:             true,
			noCluster:           true,
		})
		if err != nil {
			t.Fatalf("runValidation(%s): %v", phase, err)
		}

		want := countSkippedTests(t, out)
		if want == 0 {
			t.Fatalf("phase %s reported no skipped checks; the fixture cannot "+
				"tell a reported count from an absent one", phase)
		}

		rec := findJSONLogRecord(t, logs.Bytes(), "phase result")
		if got, _ := rec["phase"].(string); got != string(phase) {
			t.Fatalf("phase result record is for %q, want %q", got, phase)
		}
		assertJSONLogInt(t, rec, "phase result", "skipped", want)
		counts[phase] = want
	}

	// Guard against the vacuous pass: if both phases withheld the same number
	// of checks, a hardcoded constant would satisfy every assertion above.
	if counts[validator.PhaseDeployment] == counts[validator.PhaseConformance] {
		t.Errorf("both phases reported %d skipped checks; the fixture no longer "+
			"distinguishes a per-phase count from a constant",
			counts[validator.PhaseDeployment])
	}
}

// phaseLogRecipe returns a Client and a hydrated recipe from the embedded
// recipe data: no cluster and no network.
func phaseLogRecipe(t *testing.T) (*aicr.Client, *aicr.RecipeResult) {
	t.Helper()
	overlayPath := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(overlayPath, []byte(phaseLogOverlayYAML), 0o600); err != nil {
		t.Fatalf("setup: write overlay: %v", err)
	}
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("setup: NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	rec, err := client.LoadRecipe(t.Context(), overlayPath, "")
	if err != nil {
		t.Fatalf("setup: LoadRecipe: %v", err)
	}
	return client, rec
}

// phaseLogSnapshot satisfies the h100-eks-training chain's readiness
// constraint (K8s.server.version >= 1.32.4) so the run reaches the phases
// rather than stopping at the pre-flight.
func phaseLogSnapshot() *aicr.Snapshot {
	return aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			measurement.NewMeasurement(measurement.TypeK8s).
				WithSubtypeBuilder(
					measurement.NewSubtypeBuilder("server").SetString("version", "v1.34.0"),
				).
				Build(),
		},
	})
}

// countSkippedTests counts the skipped entries in a serialized CTRF report by
// walking Results.Tests. Deliberately not Results.Summary.Skipped: that is the
// field the log line prints, and reading it here would make the assertion
// compare the value to itself.
func countSkippedTests(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // Test-controlled path under t.TempDir().
	if err != nil {
		t.Fatalf("reading CTRF report %s: %v", path, err)
	}
	var report ctrf.Report
	if err := yaml.Unmarshal(raw, &report); err != nil {
		t.Fatalf("parsing CTRF report %s: %v", path, err)
	}
	if len(report.Results.Tests) == 0 {
		t.Fatalf("CTRF report %s lists no tests; the expectation would be "+
			"derived from an empty document", path)
	}
	var skipped int64
	for _, test := range report.Results.Tests {
		if test.Status == ctrf.StatusSkipped {
			skipped++
		}
	}
	return skipped
}

// captureJSONLogs redirects the default logger into a buffer of JSON records
// for the duration of the test. JSON rather than text so an attribute is read
// by name and compared as a number, instead of substring-matching "skipped=5"
// out of a line that prints several values side by side.
func captureJSONLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// findJSONLogRecord returns the first captured record whose msg is want,
// failing the test when no record carries it.
func findJSONLogRecord(t *testing.T, logs []byte, want string) map[string]any {
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

// assertJSONLogInt asserts the record carries key with exactly want. A missing
// key is reported distinctly from a wrong value, so deleting an attribute and
// mis-sourcing one are not confusable failures.
func assertJSONLogInt(t *testing.T, rec map[string]any, msg, key string, want int64) {
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

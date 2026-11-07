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

package cncf

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf/upstreamvalidator"
	"github.com/NVIDIA/aicr/pkg/evidence/redact"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// landscapeURL is the CNCF landscape the vendored validator fetches.
const landscapeURL = "https://raw.githubusercontent.com/cncf/landscape/master/landscape.yml"

// fakeTransport answers the vendored validator's requests in memory: the
// landscape with the given members, every other URL with 200 (or a status
// per URL). It replaces http.DefaultTransport, which the validator's
// zero-Transport clients use, so the gate runs without a network or a
// listener.
type fakeTransport struct {
	members         []string
	landscapeStatus int
	statuses        map[string]int

	mu       sync.Mutex
	requests []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	f.mu.Lock()
	f.requests = append(f.requests, u)
	f.mu.Unlock()
	status, body := http.StatusOK, ""
	switch {
	case u == landscapeURL:
		status = f.landscapeStatus
		var sb strings.Builder
		sb.WriteString("landscape:\n  - name: CNCF Members\n    subcategories:\n      - name: Platinum\n        items:\n")
		for _, m := range f.members {
			sb.WriteString("          - name: " + m + " (member)\n")
		}
		body = sb.String()
	case f.statuses[u] != 0:
		status = f.statuses[u]
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func installTransport(t *testing.T, f *fakeTransport) *fakeTransport {
	t.Helper()
	if f.landscapeStatus == 0 {
		f.landscapeStatus = http.StatusOK
	}
	prev := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = prev })
	return f
}

var fixedNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// submissionFixture is the acceptance fixture: every v1.37 MUST passed
// (cluster-autoscaling as an observation, which submissionInput documents,
// since no real run demonstrates it), the driver-runtime-management SHOULD
// failed, dra-support (supporting evidence at v1.37) and platform-health (no
// requirement) passed. Check output carries a node name and an IP the
// redaction pass must alias.
func submissionFixture() []ctrf.TestResult {
	tests := append([]ctrf.TestResult(nil), allMustsPassed...)
	for i := range tests {
		tests[i].Duration = 1000 * (i + 1)
		if tests[i].Name == "cluster-autoscaling" {
			tests[i].Extra = observationOnly
			tests[i].Stdout = []string{"agentpool gpuworker1 enableAutoScaling=true min=1 max=4"}
		}
	}
	tests[0].Stdout = []string{"PodGroup members scheduled on gpu-node-1 and gpu-node-2", "probe reached 10.244.3.7:8080"}
	return append(tests,
		ctrf.TestResult{Name: "driver-runtime-management", Status: "failed", Duration: 9000,
			Message: "driver version mismatch on gpu-node-2: probe 580.65.06, advertised 580.82.07"},
		ctrf.TestResult{Name: "dra-support", Status: "passed", Duration: 500, Stdout: []string{"ResourceSlice gpu.nvidia.com present"}},
		ctrf.TestResult{Name: "platform-health", Status: "passed", Duration: 250},
	)
}

// autoscalerDocumentation is the manual attestation submissionInput carries
// for the observed cluster-autoscaling result of submissionFixture.
var autoscalerDocumentation = Documentation{
	Evidence: []string{"https://vendor.example/docs/cluster-autoscaler"},
	Notes:    "The managed cluster autoscaler scales the GPU node pool on pending accelerator pods.",
}

func submissionInput(tests []ctrf.TestResult) SubmissionInput {
	return SubmissionInput{
		Report:  reportOf(tests...),
		Version: Version137,
		Options: AggregateOptions{Documented: map[string]Documentation{"cluster_autoscaling": autoscalerDocumentation}},
		Vendor:  testVendor(),
		Platform: Platform{
			KubernetesVersion: "1.37.2", Service: "aks", Accelerator: "h100", GPUModel: "h100", OS: "ubuntu", OSVersion: "24.04",
			NodeCount: 6, GPUNodeCount: 2, NodeNames: []string{"gpu-node-1", "gpu-node-2", "cpu-node-1"},
			Recipe:     RecipeInfo{Name: "h100-aks-ubuntu-inference-dynamo", Version: "v1.3.0"},
			Components: []Component{{Name: "gpu-operator", Version: "v26.7.2"}, {Name: "kai-scheduler", Version: "v0.16.9"}},
		},
		Tool: Tool{Version: "v1.3.0", Commit: "0123456789abcdef0123456789abcdef01234567"},
		Now:  fixedNow,
	}
}

// goldenFiles are the submission files pinned per case under
// testdata/submission/<case>/. Regenerate deliberately with
//
//	AICR_UPDATE_GOLDEN=1 go test ./pkg/evidence/cncf/ -run TestWriteSubmission
//
// and review the diff: these are the bytes a vendor submits.
var goldenFiles = []string{productFile, readmeFile, junitFile, e2eLogFile}

func compareGolden(t *testing.T, caseName, dir string) {
	t.Helper()
	goldenDir := filepath.Join("testdata", "submission", caseName)
	if os.Getenv("AICR_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range goldenFiles {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(goldenDir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("golden updated: %s", goldenDir)
		return
	}
	for _, name := range goldenFiles {
		want, err := os.ReadFile(filepath.Join(goldenDir, name))
		if err != nil {
			t.Fatalf("read golden (run with AICR_UPDATE_GOLDEN=1 to create): %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from golden %s; regenerate with AICR_UPDATE_GOLDEN=1 if intended\n--- got ---\n%s", name, goldenDir, got)
		}
	}
}

func readProduct(t *testing.T, dir string) map[string]productRequirement {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, productFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec map[string][]productRequirement `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("PRODUCT.yaml does not parse: %v", err)
	}
	out := map[string]productRequirement{}
	for _, rows := range doc.Spec {
		for _, r := range rows {
			out[r.ID] = r
		}
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestWriteSubmissionComplete is acceptance bullet 1 of #3605: every MUST
// passing and one SHOULD whose check failed renders that SHOULD Not
// Implemented, exports its testcase as skipped, and passes the pinned
// upstream validator without edits. It also pins the redaction pass and the
// four files' bytes.
func TestWriteSubmissionComplete(t *testing.T) {
	tr := installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, submissionInput(submissionFixture()))
	if err != nil {
		t.Fatalf("WriteSubmission() = %v", err)
	}
	if !res.Complete || res.Dir != dir {
		t.Fatalf("result = %+v", res)
	}
	if _, statErr := os.Stat(dir + IncompleteSuffix); !os.IsNotExist(statErr) {
		t.Errorf("staging tree still present: %v", statErr)
	}
	if !strings.Contains(res.ValidatorOutput, "Validation successful") {
		t.Errorf("validator output:\n%s", res.ValidatorOutput)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if want := []string{"PRODUCT.yaml", "README.md", "e2e.log", "evidence", "junit.xml"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("directory = %v, want %v", names, want)
	}
	if _, err := os.Stat(filepath.Join(dir, evidenceDirName, "index.md")); !os.IsNotExist(err) {
		t.Error("index.md must not be written into the submission directory")
	}

	product := readProduct(t, dir)
	if got := product["driver_runtime_management"]; got.Status != productNotImplemented || len(got.Evidence) != 0 || !strings.Contains(got.Notes, "driver-runtime-management failed") {
		t.Errorf("driver_runtime_management = %+v", got)
	}
	gang := product["gang_scheduling"]
	if gang.Status != productImplemented || strings.Join(gang.Evidence, " ") != "evidence/gang-scheduling.md junit.xml#gang-scheduling e2e.log#gang-scheduling" {
		t.Errorf("gang_scheduling = %+v", gang)
	}
	// workload-sandboxing has a check since #3697 but the fixture does not
	// run it, so the SHOULD is not claimed for want of a result.
	if got := product["workload_sandboxing"]; got.Status != productNotImplemented || !strings.Contains(got.Notes, "Not claimed by this run: not run") {
		t.Errorf("workload_sandboxing = %+v", got)
	}
	if got := product["cluster_autoscaling"]; got.Status != productImplemented || len(got.Evidence) != 2 || got.Evidence[0] != autoscalerDocumentation.Evidence[0] {
		t.Errorf("cluster_autoscaling = %+v, want Implemented by documentation and the page", got)
	}
	for _, r := range product {
		for _, ref := range r.Evidence {
			if strings.Contains(ref, "driver-runtime-management") || strings.Contains(ref, "dra-support") || strings.Contains(ref, "platform-health") {
				t.Errorf("%s cites %s, which is not automated proof of a requirement", r.ID, ref)
			}
		}
	}

	junit := readFile(t, filepath.Join(dir, junitFile))
	rewritten := strings.Contains(junit, `<testcase name="driver-runtime-management"`) && strings.Contains(junit, "original verdict failed: driver version mismatch")
	if !rewritten || strings.Contains(junit, "<failure") {
		t.Errorf("junit.xml export rule not applied:\n%s", junit)
	}
	e2e := readFile(t, filepath.Join(dir, e2eLogFile))
	if !strings.Contains(e2e, "--- SKIP: driver-runtime-management") || strings.Contains(e2e, "--- FAIL") {
		t.Errorf("e2e.log export rule not applied:\n%s", e2e)
	}

	// Redaction: the node names and the IP in check output are aliased in
	// every file, the vendor's URLs are kept.
	for _, path := range []string{filepath.Join(dir, junitFile), filepath.Join(dir, e2eLogFile), filepath.Join(dir, evidenceDirName, "gang-scheduling.md"), filepath.Join(dir, readmeFile)} {
		content := readFile(t, path)
		for _, leaked := range []string{"gpu-node-1", "gpu-node-2", "10.244.3.7"} {
			if strings.Contains(content, leaked) {
				t.Errorf("%s leaks %q", filepath.Base(path), leaked)
			}
		}
	}
	page := readFile(t, filepath.Join(dir, evidenceDirName, "gang-scheduling.md"))
	// cpu-node-1 sorts first and takes node-1 although no output names it.
	if !strings.Contains(page, "node-2 and node-3") || !strings.Contains(page, "ipv4-1:8080") {
		t.Errorf("aliases missing from the page:\n%s", page)
	}
	readme := readFile(t, filepath.Join(dir, readmeFile))
	for _, want := range []string{testVendor().K8sConformanceURL, "ipv4: 1 distinct value(s) aliased", "node: 2 distinct value(s) aliased", "**Outcome:** accepted",
		"`driver-runtime-management` | failed | skipped", upstreamvalidator.UpstreamCommit, "| Kubernetes version | 1.37.2 |", "`gpu-operator` | v26.7.2"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	if got := strings.Join(res.Redaction, ";"); got != "ipv4: 1 distinct value(s) aliased;node: 2 distinct value(s) aliased" {
		t.Errorf("Redaction = %q", got)
	}
	// The validator saw the landscape and every vendor URL.
	var sawLandscape bool
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, u := range tr.requests {
		sawLandscape = sawLandscape || u == landscapeURL
	}
	if !sawLandscape {
		t.Errorf("validator did not fetch the landscape: %v", tr.requests)
	}
	compareGolden(t, "complete", dir)
}

// The same fixture with a failed MUST exits non-zero and no complete
// directory exists: the staging tree is kept and the validator is not run.
func TestWriteSubmissionFailedMust(t *testing.T) {
	tr := installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	tests := withResult(submissionFixture(), "gang-scheduling",
		&ctrf.TestResult{Name: "gang-scheduling", Status: "failed", Duration: 3000, Message: "one member bound during the negative window"})
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, submissionInput(tests))
	if err != nil {
		t.Fatalf("WriteSubmission() = %v", err)
	}
	if res.Complete {
		t.Fatal("Complete = true for a failed MUST")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("requested directory exists for an incomplete run: %v", err)
	}
	if want := filepath.Join(dir+IncompleteSuffix, "v1.37", "submission"); res.Dir != want {
		t.Errorf("Dir = %s, want %s", res.Dir, want)
	}
	if len(tr.requests) != 0 {
		t.Errorf("validator ran on an incomplete directory: %v", tr.requests)
	}
	errorstest.WantReportedCode(t, res.Aggregate.Outcome(reportOf(tests...), true), errors.ErrCodeInternal)
	if got := readProduct(t, res.Dir)["gang_scheduling"]; got.Status != productNotImplemented || !strings.Contains(got.Notes, "gang-scheduling failed") {
		t.Errorf("gang_scheduling = %+v", got)
	}
	if readme := readFile(t, filepath.Join(res.Dir, readmeFile)); !strings.Contains(readme, "**Outcome:** not run; 1 applicable MUST requirement(s) not established (gang_scheduling)") {
		t.Errorf("README does not say the validator was not run:\n%s", readme)
	}
	junit := readFile(t, filepath.Join(res.Dir, junitFile))
	if !strings.Contains(junit, `<failure message="failed: one member bound`) {
		t.Errorf("a failed applicable MUST must be exported as a failure:\n%s", junit)
	}
	if _, err := os.Stat(filepath.Join(dir+IncompleteSuffix, "docs", "AIConformance-1.37.yaml")); err != nil {
		t.Errorf("staging tree lacks the schema the validator reads: %v", err)
	}
}

// A conditional MUST overridden to N/A: its failed check is exported
// skipped with the justification, PRODUCT.yaml renders N/A citing only the
// page, and the validator accepts the directory with its reviewer warning.
func TestWriteSubmissionNotApplicableExport(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	tests := withResult(submissionFixture(), "cluster-autoscaling",
		&ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Duration: 2000, Message: "no autoscaler confirmed on EKS"})
	in := submissionInput(tests)
	in.Options = AggregateOptions{NotApplicable: map[string]string{"cluster_autoscaling": "Fixed-size node pools; no cluster autoscaler or Karpenter is deployed."}}
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, in)
	if err != nil {
		t.Fatalf("WriteSubmission() = %v", err)
	}
	if !res.Complete {
		t.Fatal("Complete = false")
	}
	if !strings.Contains(res.ValidatorOutput, "is MUST level and marked N/A") {
		t.Errorf("validator output lacks the N/A reviewer warning:\n%s", res.ValidatorOutput)
	}
	got := readProduct(t, dir)["cluster_autoscaling"]
	if got.Status != productNotApplicable || strings.Join(got.Evidence, " ") != "evidence/cluster-autoscaling.md" || !strings.HasPrefix(got.Notes, "Fixed-size node pools") {
		t.Errorf("cluster_autoscaling = %+v", got)
	}
	junit := readFile(t, filepath.Join(dir, junitFile))
	if !strings.Contains(junit, "not applicable: Fixed-size node pools") || !strings.Contains(junit, "original verdict failed: no autoscaler confirmed") || strings.Contains(junit, "<failure") {
		t.Errorf("junit.xml:\n%s", junit)
	}
	compareGolden(t, "not-applicable", dir)
}

// The manual-attestation rule: an OBSERVED result with vendor documentation
// renders Implemented citing the documentation and the page, never the
// testcase; without it the run is incomplete.
func TestWriteSubmissionObservedDocumented(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	tests := withResult(submissionFixture(), "cluster-autoscaling",
		&ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Duration: 2000, Extra: observationOnly,
			Stdout: []string{"agentpool gpuworker1 enableAutoScaling=true min=1 max=4"}})
	t.Run("documented", func(t *testing.T) {
		in := submissionInput(tests)
		dir := filepath.Join(t.TempDir(), "submission")
		res, err := WriteSubmission(context.Background(), dir, in)
		if err != nil {
			t.Fatalf("WriteSubmission() = %v", err)
		}
		if !res.Complete {
			t.Fatal("Complete = false")
		}
		got := readProduct(t, dir)["cluster_autoscaling"]
		wantEvidence := "https://vendor.example/docs/cluster-autoscaler evidence/cluster-autoscaling.md"
		notesOK := strings.Contains(got.Notes, "The managed cluster autoscaler") && strings.Contains(got.Notes, "without a behavioral test")
		if got.Status != productImplemented || strings.Join(got.Evidence, " ") != wantEvidence || !notesOK {
			t.Errorf("cluster_autoscaling = %+v", got)
		}
		readme := readFile(t, filepath.Join(dir, readmeFile))
		if !strings.Contains(readme, "| `cluster_autoscaling` | MUST | Implemented | OBSERVED (cluster-autoscaling observed, not demonstrated) |") {
			t.Errorf("README row:\n%s", readme)
		}
		compareGolden(t, "observed-documented", dir)
	})
	t.Run("undocumented", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "submission")
		in := submissionInput(tests)
		in.Options.Documented = nil
		res, err := WriteSubmission(context.Background(), dir, in)
		if err != nil {
			t.Fatalf("WriteSubmission() = %v", err)
		}
		if res.Complete {
			t.Fatal("Complete = true for an observed, undocumented MUST")
		}
		if outcome := res.Aggregate.Outcome(reportOf(tests...), true); outcome == nil || !strings.Contains(outcome.Error(), "cluster_autoscaling observed") {
			t.Errorf("Outcome = %v", outcome)
		}
	})
}

// The gate: a validator that cannot start is unavailable, one that rejects
// the directory is an internal error, and either keeps the staging tree.
func TestWriteSubmissionValidatorGate(t *testing.T) {
	tests := []struct {
		name      string
		transport *fakeTransport
		wantCode  errors.ErrorCode
		wantText  string
	}{
		{
			name:      "landscape unreachable",
			transport: &fakeTransport{members: []string{"Test Vendor"}, landscapeStatus: http.StatusServiceUnavailable},
			wantCode:  errors.ErrCodeUnavailable,
			wantText:  "could not start",
		},
		{
			name:      "vendor not a member",
			transport: &fakeTransport{members: []string{"Someone Else"}},
			wantCode:  errors.ErrCodeInternal,
			wantText:  "does not match any CNCF member",
		},
		{
			name:      "documentation url does not answer",
			transport: &fakeTransport{members: []string{"Test Vendor"}, statuses: map[string]int{"https://vendor.example/docs": http.StatusNotFound}},
			wantCode:  errors.ErrCodeInternal,
			wantText:  "Invalid URL in metadata documentationUrl",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installTransport(t, tt.transport)
			dir := filepath.Join(t.TempDir(), "submission")
			_, err := WriteSubmission(context.Background(), dir, submissionInput(submissionFixture()))
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q lacks %q", err.Error(), tt.wantText)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Error("requested directory exists after a validator failure")
			}
			if _, statErr := os.Stat(filepath.Join(dir+IncompleteSuffix, "v1.37", "submission", productFile)); statErr != nil {
				t.Errorf("staging tree not kept: %v", statErr)
			}
			se, ok := stderrors.AsType[*errors.StructuredError](err)
			if !ok || se.Context[StagedDirContextKey] != filepath.Join(dir+IncompleteSuffix, "v1.37", "submission") {
				t.Errorf("error does not carry the staged directory: %+v", err)
			}
			readme := readFile(t, filepath.Join(dir+IncompleteSuffix, "v1.37", "submission", readmeFile))
			wantOutcome := "**Outcome:** rejected;"
			if tt.wantCode == errors.ErrCodeUnavailable {
				wantOutcome = "**Outcome:** not run; the validator could not start"
			}
			if !strings.Contains(readme, wantOutcome) {
				t.Errorf("README lacks %q:\n%s", wantOutcome, readme)
			}
		})
	}
}

func TestWriteSubmissionRefusals(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	base := t.TempDir()
	nonEmpty := filepath.Join(base, "taken")
	if err := os.MkdirAll(nonEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "stale")+IncompleteSuffix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "v1.2"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		dir      string
		mutate   func(*SubmissionInput)
		wantCode errors.ErrorCode
		wantText string
	}{
		{name: "nil report", dir: filepath.Join(base, "a"), mutate: func(in *SubmissionInput) { in.Report = nil }, wantCode: errors.ErrCodeInvalidRequest, wantText: "CTRF report is required"},
		{name: "malformed vendor", dir: filepath.Join(base, "b"), mutate: func(in *SubmissionInput) { in.Vendor.Name = "" }, wantCode: errors.ErrCodeInvalidRequest, wantText: "vendor.name is required"},
		{name: "empty dir", dir: "", wantCode: errors.ErrCodeInvalidRequest, wantText: "directory is required"},
		{name: "non-empty dir", dir: nonEmpty, wantCode: errors.ErrCodeInvalidRequest, wantText: "is not empty"},
		{name: "stale incomplete tree", dir: filepath.Join(base, "stale"), wantCode: errors.ErrCodeInvalidRequest, wantText: "inspect it, then remove it; every run exits 2 while it exists"},
		{name: "version-like parent segment", dir: filepath.Join(base, "v1.2", "out"), wantCode: errors.ErrCodeInvalidRequest, wantText: "vX.Y"},
		{name: "unsupported version", dir: filepath.Join(base, "c"), mutate: func(in *SubmissionInput) { in.Version = "v1.30" }, wantCode: errors.ErrCodeInvalidRequest, wantText: "unsupported"},
		{name: "malformed documented", dir: filepath.Join(base, "d"), mutate: func(in *SubmissionInput) {
			in.Options.Documented = map[string]Documentation{"gpu_sharing": {Notes: "x"}}
		}, wantCode: errors.ErrCodeInvalidRequest, wantText: "documentation URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := submissionInput(submissionFixture())
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			_, err := WriteSubmission(context.Background(), tt.dir, in)
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q lacks %q", err.Error(), tt.wantText)
			}
		})
	}
	// An existing empty directory is acceptable and is replaced in place.
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := WriteSubmission(context.Background(), empty, submissionInput(submissionFixture()))
	if err != nil || !res.Complete {
		t.Fatalf("empty dir: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(empty, productFile)); err != nil {
		t.Errorf("PRODUCT.yaml not at the requested path: %v", err)
	}
}

// Vendor fields pass through the redaction untouched: a dotted-quad platform
// version is not an IPv4 address, and a rewritten vendor name would fail the
// validator's landscape match.
func TestWriteSubmissionPreservesVendorFields(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Vendor 10.0.0.1"}})
	in := submissionInput(submissionFixture())
	in.Vendor.Name = "Vendor 10.0.0.1"
	in.Vendor.PlatformVersion = "1.0.0.1"
	in.Vendor.Description = "Build 0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0 of the platform."
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, in)
	if err != nil || !res.Complete {
		t.Fatalf("WriteSubmission() = %v %+v", err, res)
	}
	product := readFile(t, filepath.Join(dir, productFile))
	for _, want := range []string{"platformVersion: 1.0.0.1", "vendorName: Vendor 10.0.0.1", "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0 of the platform"} {
		if !strings.Contains(product, want) {
			t.Errorf("PRODUCT.yaml lacks %q:\n%s", want, product)
		}
	}
}

// Options are validated against the resolved checklist version before the
// staging tree exists, so a refusal leaves nothing behind.
func TestWriteSubmissionRefusesOptionsBeforeStaging(t *testing.T) {
	in := submissionInput(submissionFixture())
	in.Version = Version136
	in.Options.Documented["workload_sandboxing"] = autoscalerDocumentation
	dir := filepath.Join(t.TempDir(), "submission")
	_, err := WriteSubmission(context.Background(), dir, in)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if !strings.Contains(err.Error(), "workload_sandboxing") {
		t.Errorf("error %q does not name the requirement", err.Error())
	}
	if _, statErr := os.Stat(dir + IncompleteSuffix); !os.IsNotExist(statErr) {
		t.Error("a refused run left a staging tree behind")
	}
}

// A run that ends before the redaction pass completed leaves no staging
// tree at all, so no raw node name or address can be found under it.
func TestWriteSubmissionCanceled(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "submission")
	_, err := WriteSubmission(ctx, dir, submissionInput(submissionFixture()))
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if _, statErr := os.Stat(dir + IncompleteSuffix); !os.IsNotExist(statErr) {
		t.Errorf("staging tree kept before the redaction pass completed: %v", statErr)
	}
}

// Vendor-authored text in PRODUCT.yaml (the N/A justification, the
// documented notes) is kept verbatim while run-derived text is scrubbed, and
// a non-UTC Now renders in UTC.
func TestWriteSubmissionKeepsVendorNotes(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	tests := withResult(submissionFixture(), "cluster-autoscaling",
		&ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Duration: 2000, Message: "no autoscaler at 10.0.0.9"})
	in := submissionInput(tests)
	in.Options = AggregateOptions{NotApplicable: map[string]string{"cluster_autoscaling": "Fixed pools at 10.0.0.1 and release 1.2.3.4; no autoscaler."}}
	in.Now = time.Date(2026, 11, 6, 14, 0, 0, 0, time.FixedZone("plus2", 2*3600))
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, in)
	if err != nil || !res.Complete {
		t.Fatalf("WriteSubmission() = %v %+v", err, res)
	}
	product := readFile(t, filepath.Join(dir, productFile))
	if !strings.Contains(product, "Fixed pools at 10.0.0.1 and release 1.2.3.4; no autoscaler.") {
		t.Errorf("justification was scrubbed:\n%s", product)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, junitFile)), "no autoscaler at ipv4-") {
		t.Error("run-derived message was not scrubbed")
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, readmeFile)), "**Generated:** 2026-11-06T12:00:00Z") {
		t.Error("Generated is not rendered in UTC")
	}
}

// Vendor fields are not global preserve tokens: a platform version of "1"
// protects nothing else, and the run output is still redacted.
func TestWriteSubmissionVendorValueDoesNotShieldOutput(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	tests := submissionFixture()
	tests[0].Stdout = []string{"members on worker-1 reached 10.0.0.1 and 10.0.0.1:8080"}
	in := submissionInput(tests)
	in.Vendor.PlatformVersion = "1"
	in.Platform.NodeNames = []string{"worker-1"}
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, in)
	if err != nil || !res.Complete {
		t.Fatalf("WriteSubmission() = %v %+v", err, res)
	}
	page := readFile(t, filepath.Join(dir, evidenceDirName, "gang-scheduling.md"))
	if strings.Contains(page, "worker-1") || strings.Contains(page, "10.0.0.1") || !strings.Contains(page, "node-1 reached ipv4-1 and ipv4-1:8080") {
		t.Errorf("run output not redacted:\n%s", page)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, productFile)), "platformVersion: \"1\"") {
		t.Error("platformVersion not carried verbatim")
	}
}

// The default version is the latest carried checklist, and the wall clock
// is used when Now is zero.
func TestWriteSubmissionDefaults(t *testing.T) {
	installTransport(t, &fakeTransport{members: []string{"Test Vendor"}})
	in := submissionInput(submissionFixture())
	in.Version, in.Now = "", time.Time{}
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := WriteSubmission(context.Background(), dir, in)
	if err != nil || !res.Complete || res.Aggregate.Version != LatestVersion {
		t.Fatalf("defaults: %v %+v", err, res)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, productFile)), "kubernetesVersion: "+string(LatestVersion)) {
		t.Error("PRODUCT.yaml does not carry the latest version")
	}
}

// Only a verdict that rejected the directory renders as rejected; a
// validator that could not start or was interrupted did not run.
func TestValidatorFailureState(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome string
		wantDetail  string
	}{
		{"rejected", errors.New(errors.ErrCodeInternal, "upstream validator rejected the submission"), validationRejected, "its report is in the run log"},
		{"unavailable", errors.New(errors.ErrCodeUnavailable, "upstream validator could not start: landscape"), validationNotRun, "could not start"},
		{"canceled", errors.Wrap(errors.ErrCodeCanceled, "upstream validator did not finish", context.Canceled), validationNotRun, "interrupted before it reached a verdict"},
		{"timed out", errors.New(errors.ErrCodeTimeout, "upstream validator did not finish"), validationNotRun, "interrupted before it reached a verdict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validatorFailureState(tt.err)
			if got.Outcome != tt.wantOutcome || !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("validatorFailureState() = %+v, want outcome %q detail containing %q", got, tt.wantOutcome, tt.wantDetail)
			}
		})
	}
}

// renderProduct refuses an aggregate that does not cover the checklist, and
// a checklist row in a category it does not know; both are build defects.
func TestRenderProductInternalErrors(t *testing.T) {
	cl, err := ChecklistFor(Version137)
	if err != nil {
		t.Fatal(err)
	}
	_, err = renderProduct(Version137, testVendor(), cl, nil)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)

	agg, err := AggregateReport(Version137, reportOf(allMustsPassed...), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := buildProductRows(cl, agg, map[string]bool{})
	odd := &Checklist{Version: Version137, Requirements: append([]Requirement(nil), cl.Requirements...)}
	odd.Requirements[0].Category = "unknown"
	_, err = renderProduct(Version137, testVendor(), odd, rows)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

// A staging tree that cannot be created (its parent is a file) is an
// internal error, as is a directory path that cannot be listed.
func TestWriteSubmissionStagingErrors(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := WriteSubmission(context.Background(), filepath.Join(blocker, "submission"), submissionInput(submissionFixture()))
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	_, err = newStaging(filepath.Join(blocker, "submission"), Version137)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	// A root that already exists is a conflict and is left alone, whatever
	// it holds.
	taken := filepath.Join(base, "taken-root")
	if mkErr := os.MkdirAll(taken+IncompleteSuffix, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wrErr := os.WriteFile(filepath.Join(taken+IncompleteSuffix, "docs"), []byte("x"), 0o644); wrErr != nil {
		t.Fatal(wrErr)
	}
	_, err = newStaging(taken, Version137)
	errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
	if _, statErr := os.Stat(filepath.Join(taken+IncompleteSuffix, "docs")); statErr != nil {
		t.Errorf("a pre-existing staging tree was touched: %v", statErr)
	}
	// A failure after this run claimed the root removes it again: the
	// parent is made read-only so the child directories cannot be created.
	locked := filepath.Join(base, "locked", "submission")
	if mkErr := os.MkdirAll(filepath.Dir(locked), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if err = os.Chmod(filepath.Dir(locked), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(locked), 0o755) })
	if _, err = newStaging(locked, Version137); err == nil {
		t.Skip("directory permissions are not enforced here")
	}
	if _, statErr := os.Stat(locked + IncompleteSuffix); !os.IsNotExist(statErr) {
		t.Error("staging tree left behind after an initialization failure")
	}
	if _, err := listPages(filepath.Join(base, "missing")); err == nil {
		t.Error("listPages() on a missing directory must fail")
	}
	errorstest.WantReportedCode(t, writeFile(filepath.Join(base, "missing", "x"), nil), errors.ErrCodeInternal)
	errorstest.WantReportedCode(t, scrubPages(filepath.Join(base, "missing"), map[string]bool{"x.md": true}, &redact.TextScrubber{}), errors.ErrCodeInternal)
}

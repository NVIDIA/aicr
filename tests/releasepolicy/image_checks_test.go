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

package releasepolicy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// compositeStep returns the run script of the named step of a composite action.
func compositeStep(t *testing.T, action, name string) string {
	t.Helper()
	steps := sliceValue(t, mapValue(t, loadYAML(t, action), "runs"), "steps")
	index := stepIndex(steps, name)
	if index < 0 {
		t.Fatalf("%s has no step %q", action, name)
	}
	return stringValue(t, steps[index].(map[string]any), "run")
}

// fakeBin writes an executable named name holding body into a fresh directory
// and returns a PATH that finds it first.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// runStep runs a step script in dir with env and returns its outputs and error.
func runStep(t *testing.T, script, dir string, env ...string) (map[string]string, string, error) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "outputs")
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(env, "GITHUB_OUTPUT="+output)...)
	log, err := cmd.CombinedOutput()
	outputs := map[string]string{}
	if data, readErr := os.ReadFile(output); readErr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				outputs[key] = value
			}
		}
	}
	return outputs, string(log), err
}

// The malware scan passes only when ClamAV demonstrably read what it was
// given: a summary, at least one file, the expected count, and no limit hit.
func TestMalwareScanFailsClosed(t *testing.T) {
	t.Parallel()
	script := compositeStep(t, ".github/actions/malware-scan/action.yml", "Run ClamAV scan")
	tests := []struct {
		name     string
		report   string
		exit     int
		expected string
		want     string
	}{
		{name: "clean", report: "Scanned files: 10\nInfected files: 0\n", want: "clean"},
		{name: "clean with expected count", report: "Scanned files: 10\nInfected files: 0\n", expected: "10", want: "clean"},
		{name: "infected", report: "/x: Eicar-Test-Signature FOUND\nScanned files: 10\nInfected files: 1\n", exit: 1, want: "infected"},
		{name: "no summary", report: "LibClamAV Error: out of memory\n", exit: 2, want: "error"},
		{name: "summary without the infected line", report: "Scanned files: 10\n", want: "error"},
		{name: "nothing scanned", report: "Scanned files: 0\nInfected files: 0\n", want: "error"},
		{name: "fewer files than expected", report: "Scanned files: 9\nInfected files: 0\n", expected: "10", want: "error"},
		{name: "limit exceeded", report: "/x: Heuristics.Limits.Exceeded.MaxFileSize FOUND\nScanned files: 9\nInfected files: 0\nTotal errors: 1\n", exit: 2, want: "error"},
		{name: "exit 1 without an infected file", report: "Scanned files: 10\nInfected files: 0\n", exit: 1, want: "error"},
		{name: "malformed expected count", report: "Scanned files: 10\nInfected files: 0\n", expected: "ten", want: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			report := filepath.Join(dir, "report.txt")
			if err := os.WriteFile(report, []byte(tt.report), 0o600); err != nil {
				t.Fatal(err)
			}
			path := fakeBin(t, "clamscan", fmt.Sprintf(`for arg in "$@"; do [[ "$arg" == --version ]] && { echo "ClamAV 1.5.5/28189/today"; exit 0; }; done
cat %q
exit %d
`, report, tt.exit))
			outputs, log, err := runStep(t, script, dir, "PATH="+path, "SCAN_PATH=layers", "DB_DIR=db", "EXPECTED_FILES="+tt.expected)
			if err != nil {
				t.Fatalf("the scan step must not fail itself (the enforce step does): %v\n%s", err, log)
			}
			if outputs["outcome"] != tt.want {
				t.Errorf("outcome = %q, want %q\n%s", outputs["outcome"], tt.want, log)
			}
		})
	}

	condition := ""
	for _, raw := range sliceValue(t, mapValue(t, loadYAML(t, ".github/actions/malware-scan/action.yml"), "runs"), "steps") {
		if step := raw.(map[string]any); fmt.Sprint(step["name"]) == "Enforce scan result" {
			condition = fmt.Sprint(step["if"])
		}
	}
	if condition != "steps.scan.outputs.outcome != 'clean' && (steps.scan.outputs.outcome != 'infected' || inputs.fail_on_finding == 'true')" {
		t.Errorf("the enforce step must fail every outcome but clean, unless only a finding is waived: %q", condition)
	}
}

func TestImageChecksVulnerabilityScanFailsClosed(t *testing.T) {
	t.Parallel()
	script := compositeStep(t, ".github/actions/image-checks/action.yml", "Vulnerability scan")
	const manifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const index = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	tests := []struct {
		name    string
		exit    int
		scanned string
		noJSON  bool
		source  string
		want    string
	}{
		{name: "passed", scanned: manifest, want: "passed"},
		{name: "passed from the registry", scanned: manifest, source: "registry", want: "passed"},
		{name: "finding at the threshold", exit: 2, scanned: manifest, want: "failed"},
		{name: "grype error", exit: 1, scanned: manifest, want: "error"},
		{name: "no report", noJSON: true, want: "error"},
		{name: "a different manifest", scanned: "sha256:2222222222222222222222222222222222222222222222222222222222222222", want: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			reports := filepath.Join(dir, "reports")
			if err := os.MkdirAll(reports, 0o750); err != nil {
				t.Fatal(err)
			}
			write := ""
			if !tt.noJSON {
				write = fmt.Sprintf(`printf '{"source":{"target":{"manifestDigest":"%s"}},"matches":[]}' > %q
printf '{}' > %q
`, tt.scanned, filepath.Join(reports, "grype.json"), filepath.Join(reports, "grype.sarif"))
			}
			args := filepath.Join(dir, "args")
			path := fakeBin(t, "grype", fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\n", args)+write+fmt.Sprintf("exit %d\n", tt.exit))
			source := tt.source
			if source == "" {
				source = "layout"
			}
			outputs, log, err := runStep(t, script, dir, "PATH="+path, "LAYOUT=layout", "PLATFORM=linux/amd64",
				"IMAGE=ghcr.io/nvidia/aicr", "INDEX="+index, "SOURCE="+source, "MANIFEST="+manifest, "REPORTS="+reports)
			target := strings.SplitN(string(readFileAt(t, args)), "\n", 2)[0]
			wantTarget := map[string]string{"layout": "oci-dir:layout", "registry": "registry:ghcr.io/nvidia/aicr@" + index}[source]
			if target != wantTarget {
				t.Errorf("grype read %q, want %q", target, wantTarget)
			}
			if outputs["result"] != tt.want {
				t.Errorf("result = %q, want %q\n%s", outputs["result"], tt.want, log)
			}
			if (err == nil) != (tt.want == "passed") {
				t.Errorf("step error = %v for result %s", err, tt.want)
			}
		})
	}
}

func TestImageChecksSecretScanFailsClosed(t *testing.T) {
	t.Parallel()
	script := compositeStep(t, ".github/actions/image-checks/action.yml", "Secret scan")
	tests := []struct {
		name   string
		report string
		exit   int
		log    string
		want   string
	}{
		{name: "passed", report: "[]", want: "passed"},
		{name: "a file gitleaks could not read", report: "[]", log: "2:58PM WRN skipping file: permission denied path=layers/000/x", want: "error"},
		{name: "a read error", report: "[]", log: "2:58PM ERR could not read file", want: "error"},
		{name: "findings summary alone", report: `[{"RuleID":"github-pat"}]`, exit: 3, log: "2:58PM WRN leaks found: 1", want: "failed"},
		{name: "a warning that only mentions the summary", report: "[]", log: "2:58PM WRN skipping file: permission denied path=leaks found: 1/x", want: "error"},
		{name: "findings", report: `[{"RuleID":"github-pat"},{"RuleID":"aws-access-token"}]`, exit: 3, want: "failed"},
		{name: "error without a report", exit: 1, want: "error"},
		{name: "findings without the leak exit code", report: `[{"RuleID":"github-pat"}]`, want: "error"},
		{name: "leak exit code without findings", report: "[]", exit: 3, want: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			reports := filepath.Join(dir, "reports")
			if err := os.MkdirAll(reports, 0o750); err != nil {
				t.Fatal(err)
			}
			tree := filepath.Join(dir, "tree")
			if err := os.MkdirAll(tree, 0o750); err != nil {
				t.Fatal(err)
			}
			write := ""
			if tt.report != "" {
				write = fmt.Sprintf("printf '%%s' %q > %q\n", tt.report, filepath.Join(reports, "gitleaks.json"))
			}
			if tt.log != "" {
				write += fmt.Sprintf("echo %q >&2\n", tt.log)
			}
			args := filepath.Join(dir, "args")
			path := fakeBin(t, "gitleaks", fmt.Sprintf(`printf '%%s\n' "$@" > %q
[[ "$PWD" == */tree ]] || { echo "not run from the tree root" >&2; exit 9; }
`, args)+write+fmt.Sprintf("exit %d\n", tt.exit))
			outputs, log, err := runStep(t, script, dir, "PATH="+path, "TREE="+tree, "REPORTS="+reports)
			// Relative paths are what the anchored allowlists match, and the
			// warning check reads uncolored level names.
			sent := strings.Split(strings.TrimSpace(string(readFileAt(t, args))), "\n")
			config := ""
			if i := slices.Index(sent, "--config"); i >= 0 && i+1 < len(sent) {
				config = sent[i+1]
			}
			if len(sent) < 2 || sent[0] != "dir" || sent[1] != "." || !slices.Contains(sent, "--no-color") ||
				!filepath.IsAbs(config) || filepath.Base(config) != ".gitleaks.toml" {

				t.Errorf("gitleaks arguments = %q", sent)
			}
			if outputs["result"] != tt.want {
				t.Errorf("result = %q, want %q\n%s", outputs["result"], tt.want, log)
			}
			if (err == nil) != (tt.want == "passed") {
				t.Errorf("step error = %v for result %s", err, tt.want)
			}
		})
	}
}

// The last step fails the job unless every check ran and passed; a skipped
// check is not a pass.
func TestImageChecksEnforceEveryCheck(t *testing.T) {
	t.Parallel()
	action := loadYAML(t, ".github/actions/image-checks/action.yml")
	if containsDirectRunInput(action) {
		t.Error("image-checks run blocks must not interpolate inputs directly")
	}
	steps := sliceValue(t, mapValue(t, action, "runs"), "steps")
	if stepIndex(steps, "Validate inputs") != 0 {
		t.Error("image-checks must validate its inputs first")
	}
	for _, name := range []string{"Vulnerability scan", "Malware scan", "Secret scan"} {
		step := steps[stepIndex(steps, name)].(map[string]any)
		if step["continue-on-error"] != true {
			t.Errorf("%s must not stop the other checks from running", name)
		}
	}
	script := compositeStep(t, ".github/actions/image-checks/action.yml", "Enforce the checks")
	all := []string{"UNPACK", "CVE", "MALWARE", "SECRET", "SUMMARY"}
	for _, failing := range append([]string{""}, all...) {
		for _, outcome := range []string{"failure", "skipped", "cancelled", ""} {
			env := make([]string, 0, 2+len(all))
			env = append(env, "IMAGE=ghcr.io/nvidia/aicr", "PLATFORM=linux/amd64")
			for _, name := range all {
				value := "success"
				if name == failing {
					value = outcome
				}
				env = append(env, name+"="+value)
			}
			_, log, err := runStep(t, script, t.TempDir(), env...)
			if failing == "" && err != nil {
				t.Fatalf("all checks passed but the enforce step failed: %v\n%s", err, log)
			}
			if failing != "" && err == nil {
				t.Errorf("enforce step passed with %s=%q", failing, outcome)
			}
		}
	}
}

func TestReleaseImageCheckInputValidation(t *testing.T) {
	t.Parallel()
	validate := func(t *testing.T, workflow string) string {
		job := mapValue(t, mapValue(t, loadYAML(t, workflow), "jobs"), "validate-inputs")
		assertPermissions(t, job, map[string]string{})
		return stringValue(t, sliceValue(t, job, "steps")[0].(map[string]any), "run")
	}
	digestMap := fmt.Sprintf(
		`{"aicr":"sha256:%s","aicrd":"sha256:%s","aiperf-bench":"sha256:%s","conformance":"sha256:%s","deployment":"sha256:%s","gate":"sha256:%s","performance":"sha256:%s"}`,
		strings.Repeat("0", 64), strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64),
		strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64))
	images := validate(t, ".github/workflows/check-images.yaml")
	for name, tc := range map[string]struct {
		value   string
		wantErr bool
	}{
		"canonical":      {value: digestMap},
		"non-canonical":  {value: strings.Replace(digestMap, `{"aicr"`, `{ "aicr"`, 1), wantErr: true},
		"missing image":  {value: strings.Replace(digestMap, `,"gate":"sha256:`+strings.Repeat("5", 64)+`"`, "", 1), wantErr: true},
		"uppercase hex":  {value: strings.Replace(digestMap, strings.Repeat("0", 64), strings.Repeat("A", 64), 1), wantErr: true},
		"extra image":    {value: strings.Replace(digestMap, `{"aicr"`, `{"aaa":"sha256:`+strings.Repeat("7", 64)+`","aicr"`, 1), wantErr: true},
		"not an object":  {value: `[]`, wantErr: true},
		"empty":          {value: ``, wantErr: true},
		"short digest":   {value: strings.Replace(digestMap, strings.Repeat("0", 64), strings.Repeat("0", 63), 1), wantErr: true},
		"trailing space": {value: digestMap + " ", wantErr: true},
	} {
		outputs, log, err := runStep(t, images, t.TempDir(), "INPUT_DIGEST_MAP="+tc.value)
		if (err != nil) != tc.wantErr {
			t.Errorf("check-images %s: err = %v, wantErr %v\n%s", name, err, tc.wantErr, log)
		}
		if !tc.wantErr && outputs["digest_map"] != digestMap {
			t.Errorf("check-images %s: digest_map = %q", name, outputs["digest_map"])
		}
	}

	operator := validate(t, ".github/workflows/check-operator.yaml")
	for name, tc := range map[string]struct {
		value   string
		wantErr bool
	}{
		"candidate artifact": {value: "aicr-operator-candidate-candidate-123-1-attempt-2"},
		"other artifact":     {value: "release-candidate-digests-candidate-123-1-attempt-2", wantErr: true},
		"path":               {value: "../aicr-operator-candidate-candidate-123-1-attempt-2", wantErr: true},
		"newline":            {value: "aicr-operator-candidate-candidate-123-1-attempt-2\nx=y", wantErr: true},
		"empty":              {value: "", wantErr: true},
	} {
		_, log, err := runStep(t, operator, t.TempDir(), "INPUT_ARTIFACT="+tc.value)
		if (err != nil) != tc.wantErr {
			t.Errorf("check-operator %s: err = %v, wantErr %v\n%s", name, err, tc.wantErr, log)
		}
	}
}

// The operator's two images are checked from the candidate's own layouts,
// before anything is pushed, and the operator publishes only after them.
func TestReleaseChecksOperatorLayoutsBeforePush(t *testing.T) {
	t.Parallel()
	jobs := mapValue(t, loadYAML(t, ".github/workflows/on-tag.yaml"), "jobs")
	checks := mapValue(t, jobs, "operator-checks")
	if fmt.Sprint(checks["uses"]) != "./.github/workflows/check-operator.yaml" ||
		fmt.Sprint(mapValue(t, checks, "with")["artifact"]) != "${{ needs.operator-candidate.outputs.artifact }}" {

		t.Error("operator-checks must check the operator candidate artifact")
	}
	if !slices.Contains(stringSlice(mapValue(t, jobs, "operator-publish")["needs"]), "operator-checks") {
		t.Error("operator-publish must wait for operator-checks")
	}

	check := mapValue(t, mapValue(t, loadYAML(t, ".github/workflows/check-operator.yaml"), "jobs"), "check")
	matrix := mapValue(t, mapValue(t, check, "strategy"), "matrix")
	want := map[string]string{
		"ghcr.io/nvidia/aicr-operator": "image/operator",
		"ghcr.io/nvidia/aicr-registry": "registry-image/registry",
	}
	got := map[string]string{}
	for _, raw := range sliceValue(t, matrix, "image") {
		entry := raw.(map[string]any)
		got[fmt.Sprint(entry["ref"])] = fmt.Sprint(entry["layout"]) + "/" + fmt.Sprint(entry["field"])
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("operator check matrix = %v, want %v", got, want)
	}
	if len(sliceValue(t, matrix, "platform")) != 2 {
		t.Error("operator checks must cover both platforms")
	}
	text := marshalYAML(t, check)
	for _, required := range []string{"candidate/candidate.json", "candidate/${{ matrix.image.layout }}", "index: ${{ steps.digest.outputs.index }}"} {
		if !strings.Contains(text, required) {
			t.Errorf("operator checks must read the layout and its digest from the candidate: missing %q", required)
		}
	}
}

// The release checks its own commit's licenses and CodeQL result, and blocks on
// open CodeQL alerts of High or Critical security severity only.
func TestReleaseRunsSourceChecksOnTheTag(t *testing.T) {
	t.Parallel()
	jobs := mapValue(t, loadYAML(t, ".github/workflows/on-tag.yaml"), "jobs")
	license := mapValue(t, jobs, "license-check")
	if !strings.Contains(marshalYAML(t, license), "run: make license-check") {
		t.Error("license-check must run make license-check")
	}
	codeql := mapValue(t, jobs, "codeql")
	codeqlText := marshalYAML(t, codeql)
	for _, required := range []string{"github/codeql-action/init@", "languages: go", "github/codeql-action/analyze@"} {
		if !strings.Contains(codeqlText, required) {
			t.Errorf("codeql job must analyze Go: missing %q", required)
		}
	}
	for _, name := range []string{"license-check", "codeql"} {
		needs := stringSlice(mapValue(t, jobs, name)["needs"])
		if !slices.Equal(needs, []string{"detect"}) {
			t.Errorf("%s must run beside the builds, needing only detect: %v", name, needs)
		}
	}

	steps := sliceValue(t, codeql, "steps")
	analyze := steps[stepIndex(steps, "Analyze")].(map[string]any)
	gate := steps[stepIndex(steps, "Block on open High or Critical alerts")].(map[string]any)
	if fmt.Sprint(analyze["id"]) != "analyze" || fmt.Sprint(mapValue(t, gate, "env")["SARIF_ID"]) != "${{ steps.analyze.outputs.sarif-id }}" {
		t.Error("the gate must wait on this run's own CodeQL upload")
	}
	script := stringValue(t, gate, "run")
	alert := func(number int, rule string) string {
		return fmt.Sprintf(`{"number":%d,"rule":%s,"most_recent_instance":{"location":{"path":"x.go","start_line":%d}}}`, number, rule, number)
	}
	blocking := alert(1, `{"id":"go/a","security_severity_level":"critical"}`) + "," +
		alert(2, `{"id":"go/b","security_severity_level":"high"}`)
	lower := alert(3, `{"id":"go/c","security_severity_level":"medium"}`) + "," +
		alert(4, `{"id":"go/d","severity":"warning"}`)
	for name, tc := range map[string]struct {
		alerts   string
		status   string
		statusRC int
		sarifID  string
		wantErr  bool
		blocked  []string
		queried  bool
	}{
		"high and critical block":  {alerts: "[" + blocking + "," + lower + "]", wantErr: true, blocked: []string{"#1 critical", "#2 high"}, queried: true},
		"medium and below pass":    {alerts: "[" + lower + "]", queried: true},
		"none":                     {alerts: `[]`, queried: true},
		"processing still pending": {alerts: `[]`, status: "pending", wantErr: true},
		"processing failed":        {alerts: `[]`, status: "failed", wantErr: true},
		"status request error":     {alerts: `[]`, statusRC: 1, wantErr: true},
		"no upload to wait for":    {alerts: `[]`, sarifID: "-", wantErr: true},
	} {
		dir := t.TempDir()
		fixture := filepath.Join(dir, "alerts.json")
		args := filepath.Join(dir, "args")
		if err := os.WriteFile(fixture, []byte(tc.alerts), 0o600); err != nil {
			t.Fatal(err)
		}
		status := tc.status
		if status == "" {
			status = "complete"
		}
		path := fakeBin(t, "gh", fmt.Sprintf(`if [[ "$2" == */code-scanning/sarifs/* ]]; then
  [[ %d -eq 0 ]] || exit %d
  echo %q
  exit 0
fi
printf '%%s\n' "$@" > %q
while [[ $# -gt 0 ]]; do if [[ "$1" == --jq ]]; then expr="$2"; fi; shift; done
jq -r "${expr}" %q
`, tc.statusRC, tc.statusRC, status, args, fixture))
		sarifID := "abc-123"
		if tc.sarifID == "-" {
			sarifID = ""
		}
		_, log, err := runStep(t, script, dir, "PATH="+path, "GH_TOKEN=x", "REF=refs/tags/v9.9.9", "REPO=NVIDIA/aicr",
			"SARIF_ID="+sarifID, "PROCESSING_TIMEOUT_SECONDS=0", "POLL_SECONDS=0")
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v\n%s", name, err, tc.wantErr, log)
		}
		for _, alert := range tc.blocked {
			if !strings.Contains(log, alert) {
				t.Errorf("%s: blocking alert %q not reported\n%s", name, alert, log)
			}
		}
		if strings.Contains(log, "#3 ") || strings.Contains(log, "#4 ") {
			t.Errorf("%s: a Medium or non-security alert was reported as blocking\n%s", name, log)
		}
		_, statErr := os.Stat(args)
		if queried := statErr == nil; queried != tc.queried {
			t.Errorf("%s: alerts queried = %v, want %v; the gate must read alerts only after processing completes", name, queried, tc.queried)
		}
		if !tc.queried {
			continue
		}
		sent := string(readFileAt(t, args))
		for _, want := range []string{"ref=refs/tags/v9.9.9", "state=open", "tool_name=CodeQL", "--paginate"} {
			if !strings.Contains(sent, want+"\n") {
				t.Errorf("%s: gate query missing %q: %s", name, want, sent)
			}
		}
	}
}

// The unpack step fails when the coverage report it hands to the malware
// scan lacks the counts; an empty expected_files would switch off the
// file-count check.
func TestImageChecksUnpackFailsOnIncompleteCoverage(t *testing.T) {
	t.Parallel()
	script := compositeStep(t, ".github/actions/image-checks/action.yml", "Unpack the image")
	const manifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	for name, tc := range map[string]struct {
		coverage string
		wantErr  bool
		expected string
	}{
		"complete":         {coverage: `{"manifest":"` + manifest + `","totals":{"files":10,"empty":2}}`, expected: "8"},
		"no empty count":   {coverage: `{"manifest":"` + manifest + `","totals":{"files":10}}`, wantErr: true},
		"no manifest":      {coverage: `{"totals":{"files":10,"empty":2}}`, wantErr: true},
		"no totals":        {coverage: `{"manifest":"` + manifest + `"}`, wantErr: true},
		"malformed digest": {coverage: `{"manifest":"sha256:x","totals":{"files":10,"empty":2}}`, wantErr: true},
	} {
		dir := t.TempDir()
		work := filepath.Join(dir, "work")
		if err := os.MkdirAll(filepath.Join(work, "reports"), 0o750); err != nil {
			t.Fatal(err)
		}
		path := fakeBin(t, "go", fmt.Sprintf(`printf '%%s' %q > %q
`, tc.coverage, filepath.Join(work, "reports", "coverage.json")))
		outputs, log, err := runStep(t, script, dir, "PATH="+path, "LAYOUT=layout", "INDEX=x", "PLATFORM=linux/amd64", "WORK="+work)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v\n%s", name, err, tc.wantErr, log)
		}
		if !tc.wantErr && (outputs["expected_files"] != tc.expected || outputs["manifest"] != manifest) {
			t.Errorf("%s: outputs = %v", name, outputs)
		}
	}
}

// The malware scan reads the per-layer tree with the shared database and the
// unpack's file count; the main images' Grype reads the registry so OpenVEX
// statements apply, the operator's the layout.
func TestImageChecksWiring(t *testing.T) {
	t.Parallel()
	steps := sliceValue(t, mapValue(t, loadYAML(t, ".github/actions/image-checks/action.yml"), "runs"), "steps")
	malware := mapValue(t, steps[stepIndex(steps, "Malware scan")].(map[string]any), "with")
	for key, want := range map[string]string{
		"scan_path":      "${{ steps.validate.outputs.work }}/tree/layers",
		"database_dir":   "${{ inputs.clamav_database }}",
		"expected_files": "${{ steps.unpack.outputs.expected_files }}",
	} {
		if fmt.Sprint(malware[key]) != want {
			t.Errorf("Malware scan %s = %v, want %s", key, malware[key], want)
		}
	}
	validate := compositeStep(t, ".github/actions/image-checks/action.yml", "Validate inputs")
	for source, wantErr := range map[string]bool{"registry": false, "layout": false, "docker": true, "": true} {
		_, log, err := runStep(t, validate, t.TempDir(), "RUNNER_TEMP="+t.TempDir(),
			"GITHUB_ACTION_PATH="+filepath.Join(repositoryRoot(t), ".github/actions/image-checks"),
			"INPUT_KEY=aicr", "INPUT_IMAGE=ghcr.io/nvidia/aicr",
			"INPUT_INDEX=sha256:"+strings.Repeat("0", 64), "INPUT_PLATFORM=linux/amd64", "INPUT_GRYPE_SOURCE="+source)
		if (err != nil) != wantErr {
			t.Errorf("grype_source %q: err = %v, wantErr %v\n%s", source, err, wantErr, log)
		}
	}
	for workflow, want := range map[string]string{
		".github/workflows/check-images.yaml":   "registry",
		".github/workflows/check-operator.yaml": "<nil>",
	} {
		check := mapValue(t, mapValue(t, loadYAML(t, workflow), "jobs"), "check")
		uses := jobUses(t, check, "./.github/actions/image-checks")
		if len(uses) != 1 || fmt.Sprint(mapValue(t, uses[0], "with")["grype_source"]) != want {
			t.Errorf("%s grype_source = %v, want %s", workflow, uses, want)
		}
	}
}

// Every artifact a check writes carries the attempt that wrote it, and a rerun
// of failed checks reads the signature database through the job output of
// the attempt that downloaded it, for the 30 days a run can be rerun.
func TestImageChecksArtifactsAreAttemptScoped(t *testing.T) {
	t.Parallel()
	for workflow, prefix := range map[string]string{
		".github/workflows/check-images.yaml":   "clamav-db-images-attempt-",
		".github/workflows/check-operator.yaml": "clamav-db-operator-attempt-",
	} {
		jobs := mapValue(t, loadYAML(t, workflow), "jobs")
		database := mapValue(t, jobs, "clamav-database")
		steps := sliceValue(t, database, "steps")
		naming := stringValue(t, steps[stepIndex(steps, "Name the database artifact")].(map[string]any), "run")
		outputs, log, err := runStep(t, naming, t.TempDir(), "GITHUB_RUN_ATTEMPT=2")
		if err != nil || outputs["artifact"] != prefix+"2" {
			t.Errorf("%s: database artifact = %q (%v)\n%s", workflow, outputs["artifact"], err, log)
		}
		upload := mapValue(t, steps[stepIndex(steps, "Upload the signature database")].(map[string]any), "with")
		if fmt.Sprint(upload["retention-days"]) != "30" {
			t.Errorf("%s: database retention = %v, want 30", workflow, upload["retention-days"])
		}
		check := mapValue(t, jobs, "check")
		checkSteps := sliceValue(t, check, "steps")
		download := mapValue(t, checkSteps[stepIndex(checkSteps, "Download the signature database")].(map[string]any), "with")
		if fmt.Sprint(download["name"]) != "${{ needs.clamav-database.outputs.artifact }}" {
			t.Errorf("%s: checks must read the database the clamav-database job named, not recompute it", workflow)
		}
	}
	steps := sliceValue(t, mapValue(t, loadYAML(t, ".github/actions/image-checks/action.yml"), "runs"), "steps")
	upload := mapValue(t, steps[stepIndex(steps, "Upload the checks summary and reports")].(map[string]any), "with")
	if !strings.HasSuffix(fmt.Sprint(upload["name"]), "-attempt-${{ github.run_attempt }}") {
		t.Errorf("checks report artifact %v must carry the attempt", upload["name"])
	}
}

// Gitleaks runs from the tree root, so the allowlist paths are relative to
// it: they must cover the strings of AICR executables and nothing a layer
// ships under a similar name.
func TestGitleaksAllowlistPathsAreAnchored(t *testing.T) {
	t.Parallel()
	config := string(readFile(t, ".gitleaks.toml"))
	quote := strings.Repeat("'", 3)
	matches := regexp.MustCompile(`(?m)^paths = \[`+quote+`(.+)`+quote+`\]$`).FindAllStringSubmatch(config, -1)
	if len(matches) == 0 {
		t.Fatal("no allowlist paths in .gitleaks.toml")
	}
	for _, m := range matches {
		pattern := regexp.MustCompile(m[1])
		if !pattern.MatchString("strings/000/ko-app/aicr.strings") {
			t.Errorf("%s does not cover an AICR strings file", m[1])
		}
		for _, other := range []string{
			"layers/000/strings/000/x.strings",
			"layers/003/app/strings/001/x.strings",
			"/tmp/image-checks/tree/strings/000/ko-app/aicr.strings",
			"./strings/000/ko-app/aicr.strings",
		} {
			if pattern.MatchString(other) {
				t.Errorf("%s also matches %s", m[1], other)
			}
		}
	}
}

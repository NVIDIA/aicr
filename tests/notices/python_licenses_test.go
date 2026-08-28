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

package notices

import (
	"archive/zip"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// update rewrites the golden file instead of comparing against it.
// Run: go test ./tests/notices/... -run TestPythonLicensesGolden -update
var update = flag.Bool("update", false, "rewrite golden files")

const (
	pythonLicensesTool = "tools/generate-python-licenses"
	scriptTimeout      = 60 * time.Second
)

// wheel describes a synthetic wheel. A wheel is a zip with a
// <name>-<version>.dist-info/METADATA member, so fixtures need no network and
// no build tooling.
type wheel struct {
	name     string
	version  string
	metadata string
	// files are extra dist-info members, keyed by path relative to dist-info.
	files map[string]string
}

func (w wheel) write(t *testing.T, dir string) {
	t.Helper()

	path := filepath.Join(dir, w.name+"-"+w.version+"-py3-none-any.whl")
	file, err := os.Create(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("create wheel %s: %v", path, err)
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			t.Fatalf("close wheel %s: %v", path, cerr)
		}
	}()

	archive := zip.NewWriter(file)
	distInfo := w.name + "-" + w.version + ".dist-info"

	add := func(member, content string) {
		writer, werr := archive.Create(distInfo + "/" + member)
		if werr != nil {
			t.Fatalf("add %s: %v", member, werr)
		}
		if _, werr = writer.Write([]byte(content)); werr != nil {
			t.Fatalf("write %s: %v", member, werr)
		}
	}

	add("METADATA", w.metadata)
	for member, content := range w.files {
		add(member, content)
	}

	if err := archive.Close(); err != nil {
		t.Fatalf("close archive %s: %v", path, err)
	}
}

// fixtures pins one wheel per behavior the generator has to get right. Each
// corresponds to a real package that exercised the rule.
func fixtures() []wheel {
	return []wheel{
		{
			// Declares License-File and a PEP 639 SPDX expression: the
			// straightforward case.
			name:    "declared",
			version: "1.0.0",
			metadata: strings.Join([]string{
				"Metadata-Version: 2.4",
				"Name: declared",
				"Version: 1.0.0",
				"License-Expression: Apache-2.0",
				"License-File: LICENSE",
				"Home-page: https://example.invalid/declared",
				"",
			}, "\n"),
			files: map[string]string{"licenses/LICENSE": "Apache text for declared.\n"},
		},
		{
			// Ships a license WITHOUT declaring License-File. Relying on the
			// declaration alone silently dropped text for 18 of 130 real
			// packages (blinker, matplotlib, pandas, rich, scipy...).
			name:    "undeclared",
			version: "2.0.0",
			metadata: strings.Join([]string{
				"Metadata-Version: 2.1",
				"Name: undeclared",
				"Version: 2.0.0",
				"License: MIT",
				"Home-page: https://example.invalid/undeclared",
				"",
			}, "\n"),
			files: map[string]string{"LICENSE": "MIT text, never declared.\n"},
		},
		{
			// British spelling, as tqdm ships. Filename globbing must not
			// assume "LICENSE".
			name:    "britishspelling",
			version: "3.0.0",
			metadata: strings.Join([]string{
				"Metadata-Version: 2.1",
				"Name: britishspelling",
				"Version: 3.0.0",
				"License: MPL-2.0 AND MIT",
				"",
			}, "\n"),
			files: map[string]string{"LICENCE": "Licence with a C.\n"}, //nolint:misspell // deliberate British spelling, as tqdm ships
		},
		{
			// License text containing a code fence, as pillow ships. A fixed
			// ```text block is closed early by this, inverting every
			// subsequent block in the document.
			name:    "fenced",
			version: "4.0.0",
			metadata: strings.Join([]string{
				"Metadata-Version: 2.1",
				"Name: fenced",
				"Version: 4.0.0",
				"License: HPND",
				"",
			}, "\n"),
			files: map[string]string{
				"LICENSE": "Example usage:\n\n```\nnot a real fence\n```\n\nEnd.\n",
			},
		},
		{
			// Dumps prose into License instead of an identifier, as
			// choreographer/kaleido/logistro/tiktoken do.
			name:    "prose",
			version: "5.0.0",
			metadata: strings.Join([]string{
				"Metadata-Version: 2.1",
				"Name: prose",
				"Version: 5.0.0",
				"License: MIT License" +
					"\n        \n        Copyright (c) 2026 Somebody" +
					"\n        \n        Permission is hereby granted, free of charge, to any person",
				"Project-URL: Source, https://example.invalid/prose",
				"",
			}, "\n"),
			files: map[string]string{"LICENSE": "Full prose license text.\n"},
		},
		{
			// No license file at all, as nvidia-ml-py/sentencepiece/tokenizers.
			// Must still appear in the index rather than vanish.
			name:    "notext",
			version: "6.0.0",
			metadata: strings.Join([]string{
				"Metadata-Version: 2.1",
				"Name: NoText.Pkg",
				"Version: 6.0.0",
				"License: BSD-3-Clause",
				"",
			}, "\n"),
		},
	}
}

func runGenerator(t *testing.T, wheelsDir, outPath string) (string, error) {
	t.Helper()

	requirements := filepath.Join(t.TempDir(), "requirements.txt")
	if err := os.WriteFile(requirements, []byte("example==1.0.0\n"), 0o600); err != nil {
		t.Fatalf("write requirements: %v", err)
	}

	args := []string{
		repositoryPath(t, pythonLicensesTool),
		"--requirements", requirements,
		"--output", outPath,
		"--wheels-dir", wheelsDir,
	}

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, "python3", args...)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("generator exceeded deadline: %v\n%s", ctx.Err(), output)
	}
	return string(output), err
}

// TestPythonLicensesGolden byte-compares the rendered fragment against a
// checked-in golden. Substring assertions would not have caught the fence and
// counting defects that motivated this suite.
func TestPythonLicensesGolden(t *testing.T) {
	wheelsDir := t.TempDir()
	for _, w := range fixtures() {
		w.write(t, wheelsDir)
	}

	outPath := filepath.Join(t.TempDir(), "python-notices.md")
	output, err := runGenerator(t, wheelsDir, outPath)
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, output)
	}

	got, err := os.ReadFile(outPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	// The fragment embeds the requirements sha256, which varies with the
	// temporary path's content only, but keep the golden stable regardless.
	normalized := regexp.MustCompile(`sha256 `+"`"+`[0-9a-f]{64}`+"`").
		ReplaceAll(got, []byte("sha256 `<redacted>`"))
	normalized = regexp.MustCompile(`from `+"`"+`[^`+"`"+`]*requirements\.txt`+"`").
		ReplaceAll(normalized, []byte("from `<requirements>`"))

	goldenPath := repositoryPath(t, "tests/notices/testdata/python-notices.golden.md")
	if *update {
		if writeErr := os.WriteFile(goldenPath, normalized, 0o600); writeErr != nil {
			t.Fatalf("update golden: %v", writeErr)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath) //nolint:gosec // fixed test path
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(normalized) != string(want) {
		t.Errorf("rendered fragment differs from golden; run with -update to accept.\n"+
			"got %d bytes, want %d bytes", len(normalized), len(want))
	}
}

// TestPythonLicensesFencesBalance guards the failure mode that corrupts the
// whole document: license text containing a code fence must not terminate the
// block that wraps it.
func TestPythonLicensesFencesBalance(t *testing.T) {
	wheelsDir := t.TempDir()
	for _, w := range fixtures() {
		w.write(t, wheelsDir)
	}

	outPath := filepath.Join(t.TempDir(), "python-notices.md")
	if output, err := runGenerator(t, wheelsDir, outPath); err != nil {
		t.Fatalf("generator failed: %v\n%s", err, output)
	}

	content, err := os.ReadFile(outPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	fence := regexp.MustCompile("^(`{3,})(\\w*)\\s*$")
	var open string
	blocks := 0
	for index, line := range strings.Split(string(content), "\n") {
		match := fence.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		ticks, info := match[1], match[2]
		if open == "" {
			if info == "" {
				t.Fatalf("line %d: closing fence with no opener: %q "+
					"(license text escaped its block)", index+1, line)
			}
			open = ticks
			continue
		}
		if info == "" && len(ticks) >= len(open) {
			open = ""
			blocks++
		}
	}
	if open != "" {
		t.Error("unterminated code fence")
	}
	if blocks == 0 {
		t.Error("no fenced license blocks rendered")
	}
}

// TestPythonLicensesIndexCoversEveryWheel pins the silent-omission failure:
// every wheel supplied must appear in the index, including one with no license
// file and one whose name needs PEP 503 normalization.
func TestPythonLicensesIndexCoversEveryWheel(t *testing.T) {
	wheelsDir := t.TempDir()
	all := fixtures()
	for _, w := range all {
		w.write(t, wheelsDir)
	}

	outPath := filepath.Join(t.TempDir(), "python-notices.md")
	if output, err := runGenerator(t, wheelsDir, outPath); err != nil {
		t.Fatalf("generator failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(outPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	text := string(content)

	rows := regexp.MustCompile("(?m)^\\| `").FindAllString(text, -1)
	if len(rows) != len(all) {
		t.Errorf("index rows = %d, want %d (a package was dropped)", len(rows), len(all))
	}

	// NoText.Pkg normalizes to notext-pkg per PEP 503.
	for _, want := range []string{
		"declared", "undeclared", "britishspelling", "fenced", "prose", "notext-pkg",
	} {
		if !strings.Contains(text, "### "+want+"\n") {
			t.Errorf("package %q missing from license texts", want)
		}
	}
}

// TestPythonLicensesRecoversUndeclaredText covers the regression that lost
// verbatim text for packages shipping a license without a License-File header.
func TestPythonLicensesRecoversUndeclaredText(t *testing.T) {
	wheelsDir := t.TempDir()
	for _, w := range fixtures() {
		w.write(t, wheelsDir)
	}

	outPath := filepath.Join(t.TempDir(), "python-notices.md")
	output, err := runGenerator(t, wheelsDir, outPath)
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(outPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	text := string(content)

	for _, want := range []string{
		"MIT text, never declared.", // undeclared License-File
		"Licence with a C.",         //nolint:misspell // LICENCE spelling is the fixture
		"Apache text for declared.", // declared License-File
	} {
		if !strings.Contains(text, want) {
			t.Errorf("license text %q not rendered", want)
		}
	}

	// Only the package shipping no license file at all may fall back.
	const fallback = "License text unavailable in the distributed package."
	if got := strings.Count(text, fallback); got != 1 {
		t.Errorf("fallback notice count = %d, want 1 (only notext-pkg lacks a license)", got)
	}
}

// TestPythonLicensesSanitizesProseLicense keeps multi-line prose out of the
// index table, where it would break the Markdown row.
func TestPythonLicensesSanitizesProseLicense(t *testing.T) {
	wheelsDir := t.TempDir()
	for _, w := range fixtures() {
		w.write(t, wheelsDir)
	}

	outPath := filepath.Join(t.TempDir(), "python-notices.md")
	if output, err := runGenerator(t, wheelsDir, outPath); err != nil {
		t.Fatalf("generator failed: %v\n%s", err, output)
	}
	content, err := os.ReadFile(outPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	for line := range strings.SplitSeq(string(content), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		if strings.Count(line, "|") != 5 {
			t.Errorf("index row has broken cell count: %q", line)
		}
		if len(line) > 300 {
			t.Errorf("index row suspiciously long, prose leaked into a cell: %q", line[:120])
		}
	}
}

func repositoryPath(t *testing.T, relative string) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", relative))
}

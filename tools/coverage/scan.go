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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// maxScanFileBytes bounds how much of a signal file we read; runner scripts and
// test manifests are small, so anything larger is almost certainly not a hand
// authored invocation we care about.
const maxScanFileBytes = 1 << 20 // 1 MiB

// scanTarget is one path the scanner walks, tagged with the harness it
// represents. A target may be a directory (walked recursively) or a single file
// (e.g. an extensionless UAT runner script).
type scanTarget struct {
	path    string
	harness Harness
}

// scanTargets is the execution surface the matrix sources from:
//   - tests/chainsaw (whole tree)     — per-PR CI
//   - the enrolled UAT runner scripts — real nightly H100 (wired only)
//   - tests/uat/lib (shared phase lib) — same, via the runners that source it
//   - demos (whole tree)              — documented, not executed
//
// Critically, the UAT targets are the *enrolled lanes* — not the whole tests/uat
// tree — so present-but-unwired assets (a `nightly-intents: []` cloud, the
// cuj2-inference dirs no scheduled run invokes) are not mistaken for executed
// coverage.
//
// Every UAT target must exist: the registry says the nightly batch runs it, so a
// missing runner or phase library is a broken lane, not an absent signal.
// walkSignalFiles skips what it cannot stat, which would quietly shed exactly the
// coverage this generator is meant to report (#1977), so they are checked here.
func scanTargets(repoRoot string, wired wiredUAT) ([]scanTarget, error) {
	runners := wired.runners()
	targets := make([]scanTarget, 0, len(runners)+3)
	targets = append(targets, scanTarget{path: filepath.Join(repoRoot, "tests", "chainsaw"), harness: HarnessChainsaw})
	for _, runner := range runners {
		targets = append(targets, scanTarget{path: filepath.Join(repoRoot, runner), harness: HarnessUAT})
	}
	// The per-cloud runners are thin shims that `source ../lib/phases.sh`; every
	// real `aicr` invocation lives in that shared library, so it carries the same
	// nightly signal as the runners sourcing it. Scanning only the shims made the
	// UAT lanes invisible after the phase bodies moved there (#1977).
	//
	// Gated on at least one enrolled runner: the library stays on disk even when
	// every reservation opts out of the batch, and an inert library must not read
	// as live coverage.
	if len(runners) > 0 {
		targets = append(targets, scanTarget{path: filepath.Join(repoRoot, "tests", "uat", "lib"), harness: HarnessUAT})
	}
	targets = append(targets, scanTarget{path: filepath.Join(repoRoot, "demos"), harness: HarnessDemo})

	for _, t := range targets {
		if t.harness != HarnessUAT {
			continue
		}
		if _, err := os.Stat(t.path); err != nil {
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				"nightly-enrolled UAT signal path is missing", err)
		}
	}
	return targets, nil
}

// binInvocation matches the start of an `aicr` invocation in the forms the test,
// runner, and demo trees use: bare `aicr`, `./aicr`, or a `${AICR_BIN}`/`$AICR`
// shell variable (with or without surrounding quotes).
const binInvocation = `(?:\$\{?AICR[A-Z_]*\}?|\./aicr|\baicr)["']?[ \t]+`

// arrayInvocation matches the shell argv-array form the UAT runner uses, e.g.
// `args=(evidence verify ./evidence/pointer.yaml)` — the binary is applied
// separately via "${args[@]}", so the verb words follow the array opener.
const arrayInvocation = `=\([ \t]*`

// scanTextExt is the set of explicitly-recognized text extensions. Extensionless
// files (e.g. the UAT `run` scripts) are also scanned when they look like text.
var scanTextExt = map[string]bool{
	".yaml": true, ".yml": true, ".md": true, ".sh": true, ".txt": true,
}

// verbRegex builds a matcher for a (possibly multi-word) verb path invoked after
// the binary or as a shell argv array, e.g. "evidence verify" matches both
// `${AICR_BIN} evidence verify` and `args=(evidence verify ...)`.
func verbRegex(verbPath string) *regexp.Regexp {
	words := strings.Fields(verbPath)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	joined := strings.Join(words, `[ \t]+`)
	return regexp.MustCompile(`(?:` + binInvocation + `|` + arrayInvocation + `)` + joined + `\b`)
}

// scanVerbs walks every scan target and reports which harnesses invoke each
// verb path.
func scanVerbs(repoRoot string, verbs []string, wired wiredUAT) (map[string]map[Harness]bool, error) {
	targets, err := scanTargets(repoRoot, wired)
	if err != nil {
		return nil, err
	}

	res := make(map[string]map[Harness]bool, len(verbs))
	matchers := make(map[string]*regexp.Regexp, len(verbs))
	for _, v := range verbs {
		res[v] = map[Harness]bool{}
		matchers[v] = verbRegex(v)
	}

	for _, t := range targets {
		walkSignalFiles(t.path, func(content string) {
			for _, v := range verbs {
				if matchers[v].MatchString(content) {
					res[v][t.harness] = true
				}
			}
		})
	}
	return res, nil
}

// walkSignalFiles invokes fn with the text content of path (a single file) or of
// each scannable file under path (a directory). Missing paths are skipped.
func walkSignalFiles(path string, fn func(content string)) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if !info.IsDir() {
		readScannable(path, fn)
		return
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort scan; an unreadable entry is not fatal
		}
		if scannableExt(p) {
			readScannable(p, fn)
		}
		return nil
	})
}

// scannableExt reports whether p should be scanned: a known text extension, or
// an extensionless file (the UAT runner scripts) that sniffs as text.
func scannableExt(p string) bool {
	ext := filepath.Ext(p)
	if scanTextExt[ext] {
		return true
	}
	if ext == "" {
		return looksLikeText(p)
	}
	return false
}

// looksLikeText sniffs the first bytes of p for NUL, the cheap binary tell, so
// extensionless executables that are real scripts are scanned and stray binaries
// are skipped.
func looksLikeText(p string) bool {
	f, err := os.Open(p) //nolint:gosec // path bounded by a signal-root dir under repo root
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return !bytes.Contains(buf[:n], []byte{0})
}

// readScannable reads up to maxScanFileBytes of p and hands the content to fn.
func readScannable(p string, fn func(content string)) {
	f, err := os.Open(p) //nolint:gosec // path bounded by a signal-root dir under repo root
	if err != nil {
		return
	}
	defer f.Close()
	data := make([]byte, maxScanFileBytes)
	n, _ := f.Read(data)
	fn(string(data[:n]))
}

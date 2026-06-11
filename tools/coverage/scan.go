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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// signalRoot is one in-repo tree the scanner walks, tagged with the harness it
// represents and whether that harness is currently inert (stubbed).
type signalRoot struct {
	rel     string // path relative to repo root
	harness Harness
	stubbed bool // assets present but no workflow runs them (Azure UAT)
}

// defaultSignalRoots is the canonical, ordered set of trees RQ3 sources from.
// Azure UAT is marked stubbed: the trees exist but no workflow references them
// (revive-or-retire owned by DC6, #1280).
func defaultSignalRoots() []signalRoot {
	return []signalRoot{
		{rel: "tests/chainsaw", harness: HarnessChainsaw},
		{rel: "tests/uat/aws", harness: HarnessUAT},
		{rel: "tests/uat/gcp", harness: HarnessUAT},
		{rel: "tests/uat/azure", harness: HarnessUAT, stubbed: true},
		{rel: "demos", harness: HarnessDemo},
	}
}

// binInvocation matches the start of an `aicr` invocation in any of the forms
// the test/demo trees use: bare `aicr`, `./aicr`, or a `${AICR_BIN}` / `$AICR`
// shell variable. The trailing space is required so the verb word follows.
const binInvocation = `(?:\$\{?AICR[A-Z_]*\}?|\./aicr|\baicr)[ \t]+`

// scanText is the subset of file extensions worth scanning for invocations.
var scanText = map[string]bool{
	".yaml": true, ".yml": true, ".md": true, ".sh": true, ".txt": true,
}

// verbRegex builds a matcher for a (possibly multi-word) verb path invoked
// after the binary, e.g. "evidence verify" -> <bin> evidence verify.
func verbRegex(verbPath string) *regexp.Regexp {
	words := strings.Fields(verbPath)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	// \b after the last word so "verify" does not match "verifyx"; intervening
	// whitespace between words is flexible.
	return regexp.MustCompile(binInvocation + strings.Join(words, `[ \t]+`) + `\b`)
}

// verbSignals records, per verb path, the harnesses that exercise it and whether
// it was seen only in a stubbed tree.
type verbSignals struct {
	harnesses   map[Harness]bool
	stubbedOnly bool // matched only under a stubbed root (e.g. Azure UAT)
}

// scanVerbs walks every signal root and reports which harnesses invoke each
// verb. A verb seen only under stubbed roots is flagged stubbedOnly so the
// caller can render it as stubbed rather than covered.
func scanVerbs(repoRoot string, verbs []string) map[string]*verbSignals {
	res := make(map[string]*verbSignals, len(verbs))
	matchers := make(map[string]*regexp.Regexp, len(verbs))
	for _, v := range verbs {
		res[v] = &verbSignals{harnesses: map[Harness]bool{}, stubbedOnly: true}
		matchers[v] = verbRegex(v)
	}

	for _, root := range defaultSignalRoots() {
		walkSignalFiles(filepath.Join(repoRoot, root.rel), func(content string) {
			for _, v := range verbs {
				if matchers[v].MatchString(content) {
					res[v].harnesses[root.harness] = true
					if !root.stubbed {
						res[v].stubbedOnly = false
					}
				}
			}
		})
	}

	// A verb with no matches at all is not stubbed-only; it is simply uncovered.
	for _, sig := range res {
		if len(sig.harnesses) == 0 {
			sig.stubbedOnly = false
		}
	}
	return res
}

// walkSignalFiles invokes fn with the text content of each scannable file under
// dir. Missing dirs are skipped silently (a tree may not exist in a fixture).
func walkSignalFiles(dir string, fn func(content string)) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil //nolint:nilerr // best-effort scan; an unreadable entry is not fatal
		}
		if d.IsDir() || !scanText[filepath.Ext(path)] {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // path bounded by dir under repo root
		if rerr != nil {
			return nil //nolint:nilerr // skip unreadable file
		}
		fn(string(data))
		return nil
	})
}

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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Documentation that tells a user to run a command the CLI does not accept is
// worse than missing documentation: it sends them down a path that cannot work
// and costs them the time to discover why.
//
// This is not hypothetical. Four published-doc locations instructed users to
// run `aicr recipe -r <overlay>` as the remediation for a deliberate breaking
// change (#2421). That command has never existed — `aicr recipe` takes
// `--snapshot,-s` — so the guidance aimed at exactly the users the change broke
// was itself broken. It took a human reviewer to notice, twice.
//
// It never needed a human. The CLI surface baseline pins every command and flag
// authoritatively, so a doc claiming a flag that is not in it is mechanically
// detectable. This turns that class of error into a merge-gate failure.

// docsClaimPattern matches an `aicr` invocation with at least one flag, taking
// up to two command words so `aicr evidence digest --recipe` resolves to the
// subcommand rather than to `aicr evidence`.
var docsClaimPattern = regexp.MustCompile(
	`\baicr((?: [a-z][a-z-]*){1,2})((?: --?[a-zA-Z][a-zA-Z0-9-]*)+)`)

// frameworkFlags are injected by urfave/cli during setup rather than declared
// in the command tree, so the golden does not contain them (see #2451). They
// are genuinely invokable, so documenting them is correct.
var frameworkFlags = map[string]bool{
	"--help": true, "-h": true, "--version": true, "-v": true,
}

// docsClaimRoots are the trees whose `aicr` invocations are instructions to a
// user, so a command named there must work.
//
// docs/design is deliberately excluded. ADRs describe proposed designs, and a
// proposal naming a flag that does not exist yet is correct by construction —
// ADR-018 specifies `aicr bundle --split`, which is unimplemented on purpose.
// Gating ADRs would force authors to either implement first or weaken the
// design record, and the resulting noise is how a gate gets disabled.
var docsClaimRoots = []string{
	filepath.Join("docs", "user"),
	filepath.Join("docs", "integrator"),
	filepath.Join("docs", "contributor"),
	".", // repo-root Markdown: README, RELEASING, CONTRIBUTING
}

// docsClaimSkip names files excluded by path. AGENTS.local.md is a gitignored
// personal overlay that may quote a broken command as an example of a past
// defect; it is absent in CI and must not fail the gate locally.
var docsClaimSkip = map[string]bool{
	"AGENTS.local.md": true,
}

// surfaceFromGolden reads the committed baseline into the command set and the
// per-command flag set.
//
// The golden is the source rather than a live RootCommand() walk so this test
// and TestCLISurface agree by construction: if the golden is stale, that test
// fails first and names the drift, instead of this one failing with a confusing
// message about documentation.
func surfaceFromGolden(t *testing.T) (commands map[string]bool, flags map[string]map[string]bool) {
	t.Helper()

	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}

	commands = make(map[string]bool)
	flags = make(map[string]map[string]bool)
	flagLine := regexp.MustCompile(`^(.*?)  (-{1,2}[^\s]+)  type=`)

	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "command "):
			path, _, ok := strings.Cut(strings.TrimPrefix(line, "command "), "  aliases=")
			if ok {
				commands[path] = true
			}
		case strings.HasPrefix(line, "flag    "):
			m := flagLine.FindStringSubmatch(strings.TrimPrefix(line, "flag    "))
			if m == nil {
				continue
			}
			if flags[m[1]] == nil {
				flags[m[1]] = make(map[string]bool)
			}
			for _, name := range strings.Split(m[2], ",") {
				flags[m[1]][name] = true
			}
		}
	}

	if len(commands) == 0 || len(flags) == 0 {
		t.Fatal("parsed no commands or flags from the golden; every assertion " +
			"below would pass vacuously")
	}
	return commands, flags
}

// markdownFiles lists the Markdown under root, non-recursively for the repo
// root and recursively otherwise.
func markdownFiles(t *testing.T, repoRoot, root string) []string {
	t.Helper()

	dir := filepath.Join(repoRoot, root)
	var out []string

	if root == "." {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !docsClaimSkip[e.Name()] {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
		return out
	}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") && !docsClaimSkip[d.Name()] {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestDocsNameOnlyRealCLIFlags is the gate.
func TestDocsNameOnlyRealCLIFlags(t *testing.T) {
	t.Parallel()

	repoRoot := docsRepoRoot(t)
	commands, flags := surfaceFromGolden(t)

	files := make([]string, 0, len(docsClaimRoots)*32)
	for _, root := range docsClaimRoots {
		files = append(files, markdownFiles(t, repoRoot, root)...)
	}
	if len(files) == 0 {
		t.Fatal("found no Markdown to scan; the roots are wrong")
	}

	var scanned int
	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // in-repo doc, path derived from the module root
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			rel = path
		}

		for lineNo, line := range strings.Split(string(data), "\n") {
			for _, m := range docsClaimPattern.FindAllStringSubmatch(line, -1) {
				words := strings.Fields(m[1])

				// Prefer the longest command path that exists, so
				// `aicr evidence digest --recipe` checks digest's flags
				// rather than evidence's.
				cmdPath := ""
				for depth := len(words); depth >= 1; depth-- {
					candidate := "aicr " + strings.Join(words[:depth], " ")
					if commands[candidate] {
						cmdPath = candidate
						break
					}
				}
				if cmdPath == "" {
					// Not a command this CLI has. Prose like "aicr recipes
					// --data" is not an invocation worth policing here;
					// TestCLISurface owns the command set itself.
					continue
				}

				scanned++
				for _, flagName := range strings.Fields(m[2]) {
					if frameworkFlags[flagName] || flags[cmdPath][flagName] {
						continue
					}
					t.Errorf("%s:%d: docs tell the user to run %q, but %q has no %s flag.\n"+
						"        Accepted flags for that command are pinned in %s.\n"+
						"        Either correct the documentation or add the flag.",
						rel, lineNo+1, cmdPath+" "+flagName, cmdPath, flagName, goldenPath)
				}
			}
		}
	}

	if scanned == 0 {
		t.Fatal("matched no aicr invocations in any scanned file; the pattern is " +
			"wrong and this gate is inert")
	}
	t.Logf("checked %d documented aicr invocations across %d files", scanned, len(files))
}

// docsRepoRoot walks up to the module root.
func docsRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod walking up from the test working directory")
		}
		dir = parent
	}
}

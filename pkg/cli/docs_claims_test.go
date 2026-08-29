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

// docsInvocation matches `aicr` followed by a run of space-separated tokens,
// each either a flag or a lowercase word. The tokens are walked positionally
// rather than split into "command part" and "flag part", because urfave/cli
// allows root flags and subcommands to interleave: in
// `aicr --debug recipe --service eks` the first flag belongs to the root and
// the second to `recipe`.
//
// An earlier two-pattern version could not express that. One pattern required a
// command word immediately after `aicr`, the other matched root flags only up
// to the first non-flag, so `--recipie` in `aicr --debug recipe --recipie` was
// attributed to nothing and never checked.
var docsInvocation = regexp.MustCompile(
	`\baicr((?: (?:--?[A-Za-z][A-Za-z0-9-]*|[a-z][a-z-]*))+)`)

// docsLinePrefix reports whether everything before the match on its line is
// blank or shell-prompt decoration, i.e. the invocation starts the line.
var docsLinePrefix = regexp.MustCompile("^[\\s>]*[`$]?\\s*$")

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
			for _, loc := range docsInvocation.FindAllStringSubmatchIndex(line, -1) {
				tokens := strings.Fields(line[loc[2]:loc[3]])
				if len(tokens) == 0 {
					continue
				}

				// Entry rule. A line-initial invocation may begin with a root
				// flag. Anywhere else, the first token must be a real
				// subcommand — that requirement is what keeps the bare word
				// `aicr` appearing as an argument to another tool
				// (`kubectl describe job aicr -n gpu-operator`) from being read
				// as one of ours, with kubectl's flags attributed to us.
				if !docsLinePrefix.MatchString(line[:loc[0]]) &&
					!commands["aicr "+tokens[0]] {

					continue
				}

				// Walk left to right, attributing each flag to the deepest
				// command resolved so far.
				cmdPath := "aicr"
				for _, tok := range tokens {
					if !strings.HasPrefix(tok, "-") {
						if next := cmdPath + " " + tok; commands[next] {
							cmdPath = next
						}
						// A non-command word is a flag value or prose; it
						// neither extends the path nor ends the invocation.
						continue
					}

					scanned++
					if frameworkFlags[tok] || flags[cmdPath][tok] {
						continue
					}
					t.Errorf("%s:%d: docs tell the user to run %q, but %q has no %s flag.\n"+
						"        Accepted flags for that command are pinned in %s.\n"+
						"        Either correct the documentation or add the flag.",
						rel, lineNo+1, cmdPath+" "+tok, cmdPath, tok, goldenPath)
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

// TestDocsClaimWalkAttributesFlagsToTheRightCommand pins the attribution rules
// directly, instead of relying on the corpus scan to exercise them.
//
// The corpus is all-valid by construction — it is the docs, and they pass — so
// a scan over it cannot show that a *bad* flag would be caught. Two earlier
// revisions were verified only that way and both were wrong: one never checked
// root flags at all, and the two-pattern version that replaced it silently
// skipped every flag after an interleaved root flag, so `aicr --debug recipe
// --totallyfake` passed. These cases assert the failing direction.
func TestDocsClaimWalkAttributesFlagsToTheRightCommand(t *testing.T) {
	t.Parallel()

	commands, flags := surfaceFromGolden(t)

	// check mirrors the walk in TestDocsNameOnlyRealCLIFlags and returns the
	// flags it would report.
	check := func(line string) []string {
		var bad []string
		for _, loc := range docsInvocation.FindAllStringSubmatchIndex(line, -1) {
			tokens := strings.Fields(line[loc[2]:loc[3]])
			if len(tokens) == 0 {
				continue
			}
			if !docsLinePrefix.MatchString(line[:loc[0]]) && !commands["aicr "+tokens[0]] {
				continue
			}
			cmdPath := "aicr"
			for _, tok := range tokens {
				if !strings.HasPrefix(tok, "-") {
					if next := cmdPath + " " + tok; commands[next] {
						cmdPath = next
					}
					continue
				}
				if !frameworkFlags[tok] && !flags[cmdPath][tok] {
					bad = append(bad, cmdPath+" "+tok)
				}
			}
		}
		return bad
	}

	tests := []struct {
		name    string
		line    string
		wantBad bool
	}{
		// The bug that started this: a flag on a command that has no such flag.
		{"unknown flag on a subcommand", "aicr recipe -r overlay.yaml", true},
		{"typo on a real subcommand flag", "aicr bundle --recipie r.yaml", true},
		{"unknown flag on a nested subcommand", "aicr evidence digest --nope", true},

		// Root flags, which the first revision could not see at all.
		{"unknown root flag", "aicr --recipie", true},
		{"real root flag", "aicr --debug", false},

		// Interleaving, which the second revision could not see.
		{"bad flag after an interleaved root flag", "aicr --debug recipe --totallyfake", true},
		{"good flag after an interleaved root flag", "aicr --debug recipe --service eks", false},

		// Real invocations must stay quiet.
		{"subcommand with real flags", "aicr recipe --snapshot s.yaml --output r.yaml", false},
		{"nested subcommand with a real flag", "aicr evidence digest --recipe r.yaml", false},
		{"short alias", "aicr bundle -r r.yaml --deployer argocd", false},
		{"framework flag", "aicr bundle --help", false},

		// `aicr` as another tool's argument: the flags belong to that tool.
		// Only line-initial invocations may lead with a flag, which is what
		// keeps these quiet.
		{"aicr as a kubectl job name", "kubectl describe job aicr -n gpu-operator", false},
		{"aicr as a kubectl namespace", "kubectl describe pod -n aicr -l app=aicrd", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := check(tt.line)
			if got := len(bad) > 0; got != tt.wantBad {
				t.Errorf("line %q reported %v; wantBad=%v", tt.line, bad, tt.wantBad)
			}
		})
	}
}

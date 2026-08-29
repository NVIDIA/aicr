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

// docsAicrToken locates `aicr` as a standalone word.
//
// \b treats a following hyphen as a boundary, so this also matches inside
// identifiers like `aicr-evidence` and `aicr-corroboration-meta/v1`. Those are
// filtered by docsIsHyphenatedIdent below rather than by the pattern, because
// RE2 has no lookahead.
var docsAicrToken = regexp.MustCompile(`\baicr\b`)

// docsFlagName extracts the flag name from a token, discarding the markdown
// that surrounds it in prose: "--attest`):" and
// "--relocate`](../user/cli-reference.md#aicr-evidence-sign))," both yield the
// flag alone. Trimming a fixed punctuation set from the right cannot do this,
// because the junk is not always trailing.
var docsFlagName = regexp.MustCompile(`^--?[A-Za-z][A-Za-z0-9-]*`)

// docsLinePrefix reports whether everything before the match is blank or shell
// prompt decoration, i.e. the invocation starts the (logical) line.
var docsLinePrefix = regexp.MustCompile("^[\\s>]*[`$]?\\s*$")

// docsShellSeparators end an invocation. Without them, `aicr recipe --service
// eks && kubectl apply -f x.yaml` would attribute kubectl's -f to aicr.
var docsShellSeparators = map[string]bool{
	"|": true, "||": true, "&&": true, ";": true, "&": true,
	">": true, ">>": true, "<": true,
}

// docsLogicalLines joins backslash-continued lines, returning each logical line
// with the 1-based number of the physical line it started on.
//
// Docs wrap long invocations, and a physical-line scan attributes nothing to
// the continuation because it contains no `aicr` token — so every flag on the
// wrapped portion went unchecked.
type docsLine struct {
	Text string
	Line int
}

func docsLogicalLines(content string) []docsLine {
	physical := strings.Split(content, "\n")
	out := make([]docsLine, 0, len(physical))
	for i := 0; i < len(physical); i++ {
		text, start := physical[i], i+1
		for strings.HasSuffix(strings.TrimRight(text, " \t"), "\\") && i+1 < len(physical) {
			trimmed := strings.TrimRight(text, " \t")
			text = trimmed[:len(trimmed)-1] + " " + strings.TrimSpace(physical[i+1])
			i++
		}
		out = append(out, docsLine{Text: text, Line: start})
	}
	return out
}

// docsClaimOffenders returns every flag in one logical line that the command it
// is attached to does not accept.
//
// This is the single implementation. The corpus scan and the table test both
// call it, so a case pinned in the table cannot diverge from what actually runs
// over the documentation — an earlier revision had the table re-implement the
// walk, and the two drifted.
// strict controls how far an invocation extends. In a fenced code block the
// whole line is the command, so the walk continues past positional values
// (`--snapshot s.yaml --output r.yaml`). In prose it must stop at the first
// token that is neither a flag nor a command word, because prose legitimately
// discusses flags that do not exist:
//
//	`aicr validate` has no `--set` flag
//	`aicr recipe` has no --namespace flag
//
// A greedy walk reads those as invocations and reports the documentation for
// being accurate. Adjacency is what tells an instruction from a discussion.
func docsClaimOffenders(
	line string,
	strict bool,
	commands map[string]bool,
	flags map[string]map[string]bool,
) (bad []string, attributed int) {

	for _, loc := range docsAicrToken.FindAllStringIndex(line, -1) {
		// Skip hyphenated identifiers that merely begin with "aicr":
		// aicr-evidence, aicr-attestation.sigstore.json, aicr-demo2-gpu-nic-0.
		// Their suffix looks like a flag and belongs to no command.
		if loc[1] < len(line) {
			switch line[loc[1]] {
			case '-', '/', '.', ':', '_':
				continue
			}
		}
		rest := strings.Fields(line[loc[1]:])

		// Consume up to the first shell separator; beyond it the flags belong
		// to another command.
		tokens := make([]string, 0, len(rest))
		for _, tok := range rest {
			if docsShellSeparators[tok] {
				break
			}
			tokens = append(tokens, strings.Trim(tok, "`\"'"))
		}
		if len(tokens) == 0 {
			continue
		}

		// Entry rule: a line-initial invocation may lead with a root flag.
		// Anywhere else the first token must be a real subcommand, which keeps
		// `kubectl describe job aicr -n gpu-operator` from being read as ours.
		if !docsLinePrefix.MatchString(line[:loc[0]]) && !commands["aicr "+tokens[0]] {
			continue
		}

		attributed++
		cmdPath := "aicr"
		for _, tok := range tokens {
			if !strings.HasPrefix(tok, "-") {
				if next := cmdPath + " " + tok; commands[next] {
					cmdPath = next
					continue
				}
				if strict {
					break // prose: the invocation ended at this word
				}
				continue // code block: a positional value
			}
			name := docsFlagName.FindString(tok) // strips trailing markdown
			if name == "" {
				continue
			}
			if frameworkFlags[name] || flags[cmdPath][name] {
				continue
			}
			bad = append(bad, cmdPath+"\x00"+name)
		}
	}
	return bad, attributed
}

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

		inFence := false
		for _, ll := range docsLogicalLines(string(data)) {
			if strings.HasPrefix(strings.TrimSpace(ll.Text), "```") {
				inFence = !inFence
				continue
			}
			// A shell comment inside a fence is prose, not a command: the
			// fence says "code", the leading # says "discussion". Without
			// this, `# ... \`aicr recipe\` has no --namespace flag` is read
			// as an invocation of a flag it exists to say does not exist.
			strict := !inFence || strings.HasPrefix(strings.TrimSpace(ll.Text), "#")
			offenders, n := docsClaimOffenders(ll.Text, strict, commands, flags)
			scanned += n
			for _, offender := range offenders {
				cmdPath, flagName, _ := strings.Cut(offender, "\x00")
				t.Errorf("%s:%d: docs tell the user to run %q, but %q has no %s flag.\n"+
					"        Accepted flags for that command are pinned in %s.\n"+
					"        Either correct the documentation or add the flag.",
					rel, ll.Line, cmdPath+" "+flagName, cmdPath, flagName, goldenPath)
			}
		}
	}

	// scanned counts invocations the walk actually attributed to a command, not
	// raw "aicr" substrings. A substring count stays high even when attribution
	// resolves nothing — after a regression in the entry rule or in
	// surfaceFromGolden — so it could not detect the inert gate it guards
	// against.
	if scanned == 0 {
		t.Fatal("resolved no aicr invocations to a command in any scanned file; " +
			"attribution is broken and this gate is inert")
	}
	t.Logf("attributed %d documented aicr invocations across %d files", scanned, len(files))
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

	check := func(line string, strict bool) []string {
		bad := make([]string, 0, 4)
		for _, ll := range docsLogicalLines(line) {
			offenders, _ := docsClaimOffenders(ll.Text, strict, commands, flags)
			bad = append(bad, offenders...)
		}
		return bad
	}

	tests := []struct {
		name    string
		line    string
		strict  bool // true = prose, false = fenced code block
		wantBad bool
	}{
		// --- Fenced code blocks: the whole line is the command. ---

		// The bug that started this.
		{name: "unknown flag on a subcommand", line: "aicr recipe -r overlay.yaml", wantBad: true},
		{name: "typo on a real subcommand flag", line: "aicr bundle --recipie r.yaml", wantBad: true},
		{name: "unknown flag on a nested subcommand", line: "aicr evidence digest --nope", wantBad: true},

		// Root flags, invisible to the first revision.
		{name: "unknown root flag", line: "aicr --recipie", wantBad: true},
		{name: "real root flag", line: "aicr --debug", wantBad: false},

		// Interleaving, invisible to the second revision.
		{name: "bad flag after an interleaved root flag", line: "aicr --debug recipe --totallyfake", wantBad: true},
		{name: "good flag after an interleaved root flag", line: "aicr --debug recipe --service eks", wantBad: false},

		// Flags after a positional value, invisible to the third.
		{name: "bad flag after a value", line: "aicr recipe --snapshot s.yaml --not-real", wantBad: true},
		{name: "good flag after a value", line: "aicr recipe --snapshot s.yaml --output r.yaml", wantBad: false},

		// Backslash continuation: docs wrap long invocations.
		{name: "bad flag on a continued line", line: "aicr bundle \\\n  --not-real-either r.yaml", wantBad: true},
		{name: "good flag on a continued line", line: "aicr bundle \\\n  --deployer argocd", wantBad: false},

		// A following command's flags are not ours.
		{name: "flags after a shell separator", line: "aicr recipe --service eks && kubectl apply -f x.yaml", wantBad: false},

		// Real invocations stay quiet.
		{name: "subcommand with real flags", line: "aicr recipe --snapshot s.yaml --output r.yaml", wantBad: false},
		{name: "nested subcommand with a real flag", line: "aicr evidence digest --recipe r.yaml", wantBad: false},
		{name: "short alias", line: "aicr bundle -r r.yaml --deployer argocd", wantBad: false},
		{name: "framework flag", line: "aicr bundle --help", wantBad: false},

		// `aicr` as another tool's argument.
		{name: "aicr as a kubectl job name", line: "kubectl describe job aicr -n gpu-operator", wantBad: false},
		{name: "aicr as a kubectl namespace", line: "kubectl describe pod -n aicr -l app=aicrd", wantBad: false},

		// --- Prose: adjacency separates an instruction from a discussion. ---

		// The #2421 bug lived in prose with inline code, not a fence, so prose
		// coverage is not optional.
		{name: "prose inline invocation with a bad flag", strict: true,
			line: "an overlay passed directly (`aicr recipe -r overlay.yaml`) must carry one", wantBad: true},
		{name: "prose inline invocation with a good flag", strict: true,
			line: "run `aicr bundle -r recipe.yaml` to build the bundle", wantBad: false},

		// Prose that documents a flag's *absence* is correct and must not be
		// reported. A greedy walk reads these as invocations.
		{name: "prose stating a flag does not exist", strict: true,
			line: "`aicr validate` has no `--set` flag and never persists it", wantBad: false},
		{name: "prose stating absence without backticks", strict: true,
			line: "(the cm:// URI carries the namespace; `aicr recipe` has no --namespace flag)", wantBad: false},
		// A shell comment inside a fence is prose. The corpus contains exactly
		// this line, and a fence-only rule reports it.
		{name: "shell comment inside a fence", strict: true,
			line: "# (the cm:// URI carries the namespace; `aicr recipe` has no --namespace flag)", wantBad: false},
		{name: "prose with an ellipsis placeholder", strict: true,
			line: "`aicr … --rekor-url …` in the same job auto-picks it up", wantBad: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bad := check(tt.line, tt.strict)
			if got := len(bad) > 0; got != tt.wantBad {
				t.Errorf("line %q reported %v; wantBad=%v", tt.line, bad, tt.wantBad)
			}
		})
	}
}

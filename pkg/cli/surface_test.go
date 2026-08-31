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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// The CLI is one of the four surfaces ROADMAP §1 freezes at v1. This file is
// its baseline and diff gate: `cli-surface.golden` is the committed inventory
// of every command, flag, alias, type, default, and env var, and the test below
// fails when the live tree stops matching it.
//
// Why a golden rather than assertions: the value is in the *diff*. A reviewer
// seeing `- flag aicr bundle --deployer` in a PR knows immediately that a flag
// was dropped, without knowing anything about how the gate works. Hand-written
// assertions would need one per flag and would drift out of date silently.
//
// The gate classifies its own failure. An added command or flag is additive and
// the fix is to regenerate; a removed or altered one is breaking and owes the
// deprecation window in RELEASING.md. Reporting those identically would train
// everyone to run -update reflexively, which is exactly the reflex that lets a
// rename reach main.

var updateGolden = flag.Bool("update", false,
	"rewrite pkg/cli/testdata/cli-surface.golden from the live command tree")

const goldenPath = "testdata/cli-surface.golden"

// surfaceHeader explains the file to whoever opens it in a diff, since that is
// where it will most often be read.
const surfaceHeader = `# aicr CLI surface baseline — do NOT hand-edit.
#
# Regenerate:  go test ./pkg/cli/ -run TestCLISurface -update
#
# Adding a command or flag is additive: regenerate and commit in the same PR.
# Removing or renaming a command, flag, or alias, or changing a default, is a
# breaking change to a frozen v1 surface and owes the notice period in
# RELEASING.md § Deprecation Policy.
`

// flagFacts is the slice of a flag this baseline pins. Usage text is
// deliberately excluded: it is prose, it changes for good reasons, and pinning
// it would make the gate cry wolf on every wording fix.
func flagFacts(path string, f cli.Flag) string {
	names := append([]string(nil), f.Names()...)
	// Order is preserved, not sorted: urfave returns the primary name first and
	// aliases after, and `--gpu` being promoted over `--accelerator` is a
	// contract change rather than a reordering.
	//
	// The dash prefix is chosen by name length alone, independent of position.
	// Keying it on position instead would render a single-character *primary*
	// name as `--x`, which is not how it is invoked. No flag in the tree has one
	// today, so this is future-proofing rather than a live fix.
	rendered := make([]string, 0, len(names))
	for _, n := range names {
		if len(n) > 1 {
			rendered = append(rendered, "--"+n)
			continue
		}
		rendered = append(rendered, "-"+n)
	}

	typeName, defaultValue, envVars := "unknown", "", ""
	if dg, ok := f.(cli.DocGenerationFlag); ok {
		typeName = dg.TypeName()
		// GetValue, never GetDefaultText. GetDefaultText returns the author's
		// DefaultText field — display prose for help output, which can be
		// reworded without changing behavior. Pinning it would make this gate
		// fail on a help-text edit, the same cry-wolf problem that keeps Usage
		// out of the baseline. GetValue returns the actual default, which is
		// the contract. It already quotes strings and leaves other types bare,
		// so it is interpolated verbatim below rather than re-quoted.
		defaultValue = dg.GetValue()
		if vars := dg.GetEnvVars(); len(vars) > 0 {
			sorted := append([]string(nil), vars...)
			sort.Strings(sorted)
			envVars = strings.Join(sorted, ",")
		}
	}

	required := false
	if rf, ok := f.(cli.RequiredFlag); ok {
		required = rf.IsRequired()
	}

	hidden := false
	if vf, ok := f.(cli.VisibleFlag); ok {
		hidden = !vf.IsVisible()
	}

	return fmt.Sprintf("flag    %s  %s  type=%s default=%s required=%t hidden=%t env=%s",
		path, strings.Join(rendered, ","), typeName, defaultValue, required, hidden, envVars)
}

// collectSurface walks the command tree depth-first and returns one sorted line
// per command and per flag.
func collectSurface(cmd *cli.Command, path string, out *[]string) {
	if path == "" {
		path = cmd.Name
	}

	// Command aliases are accepted invocation names in urfave/cli, exactly like
	// flag aliases, so dropping one breaks anybody scripting against it. They
	// are recorded sorted rather than in declaration order: unlike flag
	// aliases, no alias here is "primary" — the primary name is cmd.Name — so
	// reordering carries no meaning and sorting keeps the golden stable.
	aliases := append([]string(nil), cmd.Aliases...)
	sort.Strings(aliases)

	*out = append(*out, fmt.Sprintf("command %s  aliases=%s hidden=%t",
		path, strings.Join(aliases, ","), cmd.Hidden))

	flags := append([]cli.Flag(nil), cmd.Flags...)
	sort.Slice(flags, func(i, j int) bool {
		return firstName(flags[i]) < firstName(flags[j])
	})
	for _, f := range flags {
		*out = append(*out, flagFacts(path, f))
	}

	subs := append([]*cli.Command(nil), cmd.Commands...)
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	for _, sub := range subs {
		collectSurface(sub, path+" "+sub.Name, out)
	}
}

func firstName(f cli.Flag) string {
	if names := f.Names(); len(names) > 0 {
		return names[0]
	}
	return ""
}

func renderSurface() string {
	var lines []string
	collectSurface(RootCommand(), "", &lines)
	sort.Strings(lines)
	return surfaceHeader + strings.Join(lines, "\n") + "\n"
}

// TestCLISurface is the diff gate. It runs under `make test`, so it is already
// inside the merge gate without a separate workflow.
func TestCLISurface(t *testing.T) {
	got := renderSurface()

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}

	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v\n\nIf this is the first run, generate it with:\n"+
			"  go test ./pkg/cli/ -run TestCLISurface -update", goldenPath, err)
	}

	if got == string(wantBytes) {
		return
	}

	want := stripComments(string(wantBytes))
	added, removed := diffLines(want, stripComments(got))

	var b strings.Builder
	b.WriteString("the aicr CLI surface no longer matches its committed baseline.\n\n")

	if len(removed) > 0 {
		b.WriteString("BREAKING — these entries were removed or altered:\n")
		for _, line := range removed {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
		b.WriteString("\nRemoving or renaming a command, flag, or alias, or changing a\n" +
			"default, breaks a surface frozen at v1. It owes the notice period in\n" +
			"RELEASING.md § Deprecation Policy: ship the deprecation with a warning\n" +
			"first, and remove it only after the window. If this removal is\n" +
			"intentional and the window has passed, regenerate the golden.\n\n")
	}

	newlyRequired, compatible := classifyAdded(want, added)

	if len(newlyRequired) > 0 {
		b.WriteString("BREAKING — these flags are newly required on commands that already existed:\n")
		for _, line := range newlyRequired {
			fmt.Fprintf(&b, "  ! %s\n", line)
		}
		b.WriteString("\nAdding a required flag to an existing command makes previously valid\n" +
			"invocations fail, which RELEASING.md § Deprecation Policy classifies as\n" +
			"breaking. Give the flag a default that preserves current behavior, or\n" +
			"ship it through the deprecation window.\n\n")
	}

	if len(compatible) > 0 {
		b.WriteString("Additive — these entries are new:\n")
		for _, line := range compatible {
			fmt.Fprintf(&b, "  + %s\n", line)
		}
		b.WriteString("\nAdditions are compatible. Regenerate the golden and commit it in\n" +
			"this PR.\n\n")
	}

	b.WriteString("Regenerate with:\n  go test ./pkg/cli/ -run TestCLISurface -update")
	t.Error(b.String())
}

func stripComments(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// diffLines reports set differences. Both inputs are sorted line sets, so a
// rename shows up as one removal plus one addition — which is the honest
// rendering: the gate cannot know the two are related, and a reviewer reading
// "removed --accelerator, added --gpu" learns more than "renamed" would tell
// them anyway.
func diffLines(want, got []string) (added, removed []string) {
	inWant := make(map[string]bool, len(want))
	for _, line := range want {
		inWant[line] = true
	}
	inGot := make(map[string]bool, len(got))
	for _, line := range got {
		inGot[line] = true
	}

	for _, line := range got {
		if !inWant[line] {
			added = append(added, line)
		}
	}
	for _, line := range want {
		if !inGot[line] {
			removed = append(removed, line)
		}
	}
	return added, removed
}

// TestCLISurfaceDefaultsAreBuildIndependent guards the one flag default in the
// tree that is computed rather than literal.
//
// `aicr snapshot --image` defaults to defaultAgentImage(), which derives from
// the `version` package var that release builds overwrite via ldflags. Under
// `go test` nothing sets ldflags, so version stays at versionDefault and the
// default resolves to :latest deterministically — which is what the golden
// records.
//
// If that ever stops being true, the symptom would be an unexplained one-line
// golden diff appearing only in some build contexts, and the natural reaction
// would be to regenerate the golden and move on. This test converts that into a
// named failure instead. A freeze gate that flakes is a freeze gate someone
// eventually deletes.
//
// The other way this can break is in-process: TestDefaultAgentImage assigns
// `version` directly. It restores the original via t.Cleanup and neither test
// is parallel, so the two do not interact today — but marking that test
// t.Parallel() would let its window overlap this one, and this assertion is
// what would say so.
func TestCLISurfaceDefaultsAreBuildIndependent(t *testing.T) {
	if version != versionDefault {
		t.Fatalf("version = %q, want %q — this test binary was built with "+
			"version ldflags, which makes the `aicr snapshot --image` default "+
			"build-dependent and the CLI surface golden unstable. Either stop "+
			"injecting version into test builds, or normalize version-derived "+
			"defaults in flagFacts.", version, versionDefault)
	}
}

// TestCLISurfaceIsNotEmpty guards the gate itself. A refactor that made
// RootCommand return an empty tree would otherwise "pass" the moment someone
// regenerated the golden, freezing nothing at all.
func TestCLISurfaceIsNotEmpty(t *testing.T) {
	lines := stripComments(renderSurface())

	var commands, flags int
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "command "):
			commands++
		case strings.HasPrefix(line, "flag "):
			flags++
		}
	}

	// The tree had 11 top-level verbs and 9 subcommands when this gate landed.
	// The floor is deliberately well below that: it catches a collapsed tree,
	// not ordinary evolution.
	if commands < 15 {
		t.Errorf("collected %d commands, want at least 15 — the walker is not "+
			"reaching the command tree", commands)
	}
	if flags < 50 {
		t.Errorf("collected %d flags, want at least 50 — the walker is not "+
			"reaching command flags", flags)
	}
}

// TestCollectSurfaceRecordsCommandAliases proves the walker captures command
// aliases.
//
// The golden cannot demonstrate this on its own: no command in the tree
// currently declares an alias, so every committed line reads `aliases=` and a
// walker that ignored cmd.Aliases entirely would produce byte-identical output.
// That is precisely the shape of gap that lets a freeze gate look healthy while
// guarding nothing — the first command alias anyone adds would be silently
// unpinned. A synthetic tree exercises the path that the real one cannot.
func TestCollectSurfaceRecordsCommandAliases(t *testing.T) {
	t.Parallel()

	root := &cli.Command{
		Name: "aicr",
		Commands: []*cli.Command{{
			// Declared unsorted to pin the sort, so the golden cannot churn on
			// a reordering that changes nothing a user can observe.
			Name:    "recipe",
			Aliases: []string{"recipes", "r"},
		}},
	}

	var lines []string
	collectSurface(root, "", &lines)

	var got string
	for _, line := range lines {
		if strings.HasPrefix(line, "command aicr recipe ") {
			got = line
		}
	}
	if got == "" {
		t.Fatalf("no line for the subcommand; collected: %v", lines)
	}

	const want = "command aicr recipe  aliases=r,recipes hidden=false"
	if got != want {
		t.Errorf("command line = %q, want %q", got, want)
	}

	// Removing an alias must change the output, or the gate cannot detect it.
	root.Commands[0].Aliases = []string{"r"}
	var after []string
	collectSurface(root, "", &after)
	for _, line := range after {
		if line == want {
			t.Error("dropping an alias left the recorded line unchanged; " +
				"command aliases are not actually pinned")
		}
	}
}

// TestFlagFactsRendersDashPrefixByNameLength pins the dash-prefix rule against
// a synthetic flag whose PRIMARY name is a single character.
//
// The golden cannot cover this: every flag in the tree has a multi-character
// primary name, so an implementation that keyed the prefix on position rather
// than length produces a byte-identical baseline and looks correct forever. The
// first short primary name anyone adds would then be recorded as `--x`, which
// is not how it is invoked, and the surface gate would pin the wrong contract.
func TestFlagFactsRendersDashPrefixByNameLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flag cli.Flag
		want string
	}{
		{
			name: "single-char primary renders with one dash",
			flag: &cli.StringFlag{Name: "x", Aliases: []string{"extra"}},
			want: "-x,--extra",
		},
		{
			name: "long primary with short alias keeps both forms",
			flag: &cli.StringFlag{Name: "recipe", Aliases: []string{"r"}},
			want: "--recipe,-r",
		},
		{
			name: "long primary with long alias",
			flag: &cli.StringFlag{Name: "accelerator", Aliases: []string{"gpu"}},
			want: "--accelerator,--gpu",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := flagFacts("aicr test", tt.flag)
			// flagFacts renders "flag    <path>  <names>  type=..."; the names
			// field is what this test is about.
			_, rest, ok := strings.Cut(got, "aicr test  ")
			if !ok {
				t.Fatalf("unexpected flagFacts layout: %q", got)
			}
			names, _, ok := strings.Cut(rest, "  type=")
			if !ok {
				t.Fatalf("unexpected flagFacts layout: %q", got)
			}
			if names != tt.want {
				t.Errorf("rendered names = %q, want %q", names, tt.want)
			}
		})
	}
}

// classifyAdded splits new baseline lines into the ones that break the CLI
// contract and the ones that do not.
//
// Not every addition is compatible. A flag arriving already required on a
// command that already existed invalidates invocations that were valid before,
// which RELEASING.md classifies as breaking — only a new flag whose default
// preserves behavior is additive. On a brand-new command there is no prior
// invocation to break, so requiredness there is additive; the split is by
// whether the command was already in the baseline.
//
// This is a named function rather than a loop inside TestCLISurface because the
// reporting block around it runs only on the failure path, which a green CI run
// never reaches. Asserting on this directly is what keeps the classification
// itself covered.
func classifyAdded(want, added []string) (newlyRequired, compatible []string) {
	existing := commandPaths(want)
	for _, line := range added {
		if isRequiredFlagLine(line) && existing[flagCommandPath(line)] {
			newlyRequired = append(newlyRequired, line)
			continue
		}
		compatible = append(compatible, line)
	}
	return newlyRequired, compatible
}

// commandPaths returns the command paths present in a baseline line set.
func commandPaths(lines []string) map[string]bool {
	paths := make(map[string]bool)
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, "command ")
		if !ok {
			continue
		}
		if path, _, found := strings.Cut(rest, "  aliases="); found {
			paths[path] = true
		}
	}
	return paths
}

// isRequiredFlagLine reports whether a rendered flag line marks the flag
// required.
func isRequiredFlagLine(line string) bool {
	return strings.HasPrefix(line, "flag ") && strings.Contains(line, " required=true ")
}

// flagCommandPath extracts the command path from a rendered flag line, whose
// shape is "flag    <path>  <names>  type=...".
func flagCommandPath(line string) string {
	rest, ok := strings.CutPrefix(line, "flag    ")
	if !ok {
		return ""
	}
	path, _, found := strings.Cut(rest, "  ")
	if !found {
		return ""
	}
	return path
}

// TestClassifyAddedSplitsBreakingFromAdditive drives the real classification
// used by TestCLISurface's failure report.
//
// Two things make this worth asserting directly. The classify-and-report block
// runs only when the live tree diverges from the golden, so a normal green run
// never executes it — a swapped bucket would mislabel a newly required flag as
// "Additive — Regenerate the golden", which is precisely the reflexive -update
// this file exists to prevent. And the lines are produced by real flagFacts
// calls rather than hand-written literals, so if its render format ever drifts,
// flagCommandPath stops matching and this test fails instead of silently
// fail-opening a breaking flag into the compatible bucket.
func TestClassifyAddedSplitsBreakingFromAdditive(t *testing.T) {
	t.Parallel()

	baseline := []string{
		"command aicr  aliases= hidden=false",
		"command aicr bundle  aliases= hidden=false",
		flagFacts("aicr bundle", &cli.StringFlag{Name: "recipe", Aliases: []string{"r"}}),
	}

	requiredOnExisting := flagFacts("aicr bundle",
		&cli.StringFlag{Name: "must-set", Required: true})
	optionalOnExisting := flagFacts("aicr bundle",
		&cli.StringFlag{Name: "nice-to-have"})
	requiredOnNew := flagFacts("aicr brandnew",
		&cli.StringFlag{Name: "must-set", Required: true})
	newCommand := "command aicr brandnew  aliases= hidden=false"

	newlyRequired, compatible := classifyAdded(baseline, []string{
		requiredOnExisting, optionalOnExisting, requiredOnNew, newCommand,
	})

	if len(newlyRequired) != 1 || newlyRequired[0] != requiredOnExisting {
		t.Errorf("newlyRequired = %q, want exactly [%q]", newlyRequired, requiredOnExisting)
	}

	wantCompatible := []string{optionalOnExisting, requiredOnNew, newCommand}
	if !slices.Equal(compatible, wantCompatible) {
		t.Errorf("compatible = %q, want %q", compatible, wantCompatible)
	}
}

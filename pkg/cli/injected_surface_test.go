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
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// Part of the CLI surface is injected by urfave/cli during setup rather than
// declared in the command tree: the `completion` command with its four shell
// subcommands, `--help` throughout, and root `--version`.
//
// cli-surface.golden cannot cover them. renderSurface walks RootCommand(),
// which is the tree *before* setup runs, so none of that surface appears in the
// baseline and removing any of it passes TestCLISurface silently (#2451).
//
// The obvious fix — render from a post-setup tree — is not available. urfave's
// setupDefaults and setupCommandGraph are unexported, so reaching them means
// calling Run, and root.go:64 warns that urfave mutates parsed state on the
// Flag value, which is why the codebase builds a fresh command per invocation.
// Baking parsed state into a committed golden would trade a known gap for an
// unpredictable one.
//
// So this surface is asserted behaviorally instead, which is also the stronger
// claim: not that the golden contains a line, but that a user can actually run
// the command. Every case uses a fresh newRootCmd() for exactly the reason
// root.go gives.
//
// This is not incidental surface. root.go sets EnableShellCompletion and then
// uses ConfigureShellCompletionCommand to un-hide the completion command and
// give it a category — a deliberate decision to make it public. Users script
// against `aicr completion bash`.

// runFresh invokes args against a brand-new command tree and returns its
// output. A fresh instance per call is required: urfave performs setup and
// records parsed state on the instance, so reuse leaks state between cases.
func runFresh(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var buf bytes.Buffer
	cmd := newRootCmd()
	cmd.Writer = &buf
	cmd.ErrWriter = &buf
	err := cmd.Run(t.Context(), args)
	return buf.String(), err
}

// completionCommand returns the injected completion command from a throwaway
// post-setup tree, or nil.
//
// Callers must check this before invoking `aicr completion <shell>`. When the
// command is absent, urfave routes the invocation to its help handler, which
// calls os.Exit and takes the whole test binary with it — the run then reports
// only "No help topic for 'completion'" with no failing test named. Checking
// first turns that into an actionable message.
func completionCommand(t *testing.T) *cli.Command {
	t.Helper()

	var buf bytes.Buffer
	cmd := newRootCmd()
	cmd.Writer = &buf
	if err := cmd.Run(t.Context(), []string{"aicr", "--help"}); err != nil {
		t.Fatalf("run --help to trigger setup: %v", err)
	}
	for _, sub := range cmd.Commands {
		if sub.Name == "completion" {
			return sub
		}
	}
	return nil
}

// requireCompletion fails with a useful message rather than letting a missing
// command kill the process.
func requireCompletion(t *testing.T) *cli.Command {
	t.Helper()

	c := completionCommand(t)
	if c == nil {
		t.Fatal("no completion command after setup. urfave injects it when " +
			"EnableShellCompletion is set OR ConfigureShellCompletionCommand is " +
			"non-nil (command_setup.go:124, an OR); root.go sets both, so both " +
			"must be removed to lose it. `aicr completion bash` is public surface " +
			"users script against.")
	}
	return c
}

// TestShellCompletionIsInvokable asserts each shell script the CLI advertises
// can actually be produced.
func TestShellCompletionIsInvokable(t *testing.T) {
	requireCompletion(t)

	// urfave injects exactly these four. A fifth appearing, or one of these
	// disappearing, is a surface change worth noticing.
	for _, shell := range []string{"bash", "zsh", "fish", "pwsh"} {
		t.Run(shell, func(t *testing.T) {
			out, err := runFresh(t, "aicr", "completion", shell)
			if err != nil {
				t.Fatalf("aicr completion %s: %v\noutput: %s", shell, err, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("aicr completion %s produced no script", shell)
			}
		})
	}
}

// TestShellCompletionScriptsAreDistinct is what keeps the case above from
// being vacuous.
//
// Asserting only "no error, non-empty output" would pass even if every shell
// resolved to the same handler, or to the root help text. Four distinct scripts
// prove four distinct subcommands actually resolved.
//
// The negative form — invoking an unknown shell and expecting an error — is not
// usable here: urfave routes it to the help handler, which calls os.Exit and
// takes the test binary with it.
func TestShellCompletionScriptsAreDistinct(t *testing.T) {
	requireCompletion(t)

	shells := []string{"bash", "zsh", "fish", "pwsh"}
	scripts := make(map[string]string, len(shells))

	for _, shell := range shells {
		out, err := runFresh(t, "aicr", "completion", shell)
		if err != nil {
			t.Fatalf("aicr completion %s: %v", shell, err)
		}
		scripts[shell] = out
	}

	for i, a := range shells {
		for _, b := range shells[i+1:] {
			if scripts[a] == scripts[b] {
				t.Errorf("aicr completion %s and %s produced identical output; "+
					"the shell subcommands are not resolving separately, so the "+
					"invokability assertions prove nothing", a, b)
			}
		}
	}
}

// TestRootVersionAndHelpAreInvokable covers the other injected surface.
func TestRootVersionAndHelpAreInvokable(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"root version", []string{"aicr", "--version"}, "aicr version"},
		{"root help", []string{"aicr", "--help"}, "COMMANDS"},
		{"subcommand help", []string{"aicr", "bundle", "--help"}, "OPTIONS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runFresh(t, tt.args...)
			if err != nil {
				t.Fatalf("%v: %v\noutput: %s", tt.args, err, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("%v output does not contain %q\ngot: %s", tt.args, tt.want, out)
			}
		})
	}
}

// TestCompletionCommandIsPublic pins the two choices root.go makes about the
// injected completion command.
//
// ConfigureShellCompletionCommand un-hides it and gives it a category. Both are
// deliberate — the default is hidden — and both are invisible to the golden,
// so silently reverting either would change documented public surface with no
// test failing.
func TestCompletionCommandIsPublic(t *testing.T) {
	completion := requireCompletion(t)

	if completion.Hidden {
		t.Error("completion is hidden; root.go's ConfigureShellCompletionCommand " +
			"un-hides it deliberately, so it is public surface")
	}
	if completion.Category == "" {
		t.Error("completion has no category; root.go assigns one so it groups in help")
	}

	// The four shell subcommands are the surface users actually invoke. The
	// comparison runs both ways: a missing one breaks a documented command, and
	// an unexpected one is public surface nothing covers, since the golden
	// cannot see this tree either. A urfave upgrade adding a fifth shell should
	// be a conscious decision, not a silent one.
	want := map[string]bool{"bash": true, "zsh": true, "fish": true, "pwsh": true}
	got := make(map[string]bool, len(completion.Commands))
	for _, sub := range completion.Commands {
		got[sub.Name] = true
	}

	for shell := range want {
		if !got[shell] {
			t.Errorf("completion has no %q subcommand; `aicr completion %s` is "+
				"documented surface", shell, shell)
		}
	}
	for shell := range got {
		if !want[shell] {
			t.Errorf("completion has an unexpected %q subcommand. It is public "+
				"surface that neither this test nor cli-surface.golden covers. "+
				"Add it here and to docs/user/cli-reference.md, or remove it.", shell)
		}
	}
	if len(got) != len(want) {
		t.Errorf("completion has %d subcommands, want %d", len(got), len(want))
	}
}

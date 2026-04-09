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

package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	urfave "github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// runCompletion runs the root command with --generate-shell-completion appended
// and returns the Writer buffer content. It sets os.Args to match the simulated
// command line (completeWithAllFlags reads os.Args to determine the last
// user-typed argument) and sanitizes args the same way Execute() does.
func runCompletion(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := newRootCmd()

	var buf bytes.Buffer
	cmd.Writer = &buf

	fullArgs := append([]string{"aicr"}, args...)
	fullArgs = append(fullArgs, "--generate-shell-completion")

	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = fullArgs

	err := cmd.Run(context.Background(), sanitizeCompletionArgs(fullArgs))
	return buf.String(), err
}

// longFlagNames extracts all long flag names (--name) from a command's flags,
// skipping single-character aliases.
func longFlagNames(cmd *urfave.Command) []string {
	var names []string
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			if len(n) > 1 {
				names = append(names, "--"+n)
			}
		}
	}
	return names
}

func TestCompletion_RootSubcommands(t *testing.T) {
	output, err := runCompletion(t)
	if err != nil {
		t.Fatalf("root completion failed: %v", err)
	}

	root := newRootCmd()
	for _, sub := range root.Commands {
		if sub.Hidden {
			continue
		}
		if !strings.Contains(output, sub.Name) {
			t.Errorf("expected subcommand %q in completion output, got:\n%s", sub.Name, output)
		}
	}
}

func TestCompletion_SubcommandFlags(t *testing.T) {
	// Pass a bare "-" to trigger flag completion. This matches the bash/zsh
	// completion script behavior when the cursor is on a flag prefix
	// (e.g., "aicr bundle -<TAB>" sends "aicr bundle - --generate-shell-completion").
	subcommands := map[string]*urfave.Command{
		"snapshot": snapshotCmd(),
		"recipe":   recipeCmd(),
		"query":    queryCmd(),
		"bundle":   bundleCmd(),
		"verify":   bundleVerifyCmd(),
		"validate": validateCmd(),
	}

	for name, cmd := range subcommands {
		t.Run(name, func(t *testing.T) {
			output, err := runCompletion(t, name, "-")
			if err != nil {
				t.Fatalf("%s completion failed: %v", name, err)
			}

			for _, flag := range longFlagNames(cmd) {
				if !strings.Contains(output, flag) {
					t.Errorf("expected flag %q in %s completion output, got:\n%s", flag, name, output)
				}
			}
		})
	}
}

func TestCompletion_PartialFlagPrefix(t *testing.T) {
	// When the user types "aicr verify --form<TAB>", the shell sends
	// "aicr verify --form --generate-shell-completion". The partial flag
	// "--form" fails urfave/cli's flag parser, but completeWithAllFlags
	// reads os.Args directly and still provides matching completions.
	output, err := runCompletion(t, "verify", "--form")
	if err != nil {
		t.Fatalf("verify --form<TAB> completion failed: %v", err)
	}

	if !strings.Contains(output, "--format") {
		t.Errorf("expected --format in completion output, got:\n%s", output)
	}
}

func TestCompletion_DoubleDashFlags(t *testing.T) {
	// When the user types "aicr snapshot --<TAB>", the shell sends
	// "aicr snapshot -- --generate-shell-completion". sanitizeCompletionArgs
	// replaces "--" with "-" so completion mode stays active and flag
	// suggestions are returned instead of executing the command.
	subcommands := map[string]*urfave.Command{
		"snapshot": snapshotCmd(),
		"bundle":   bundleCmd(),
		"validate": validateCmd(),
	}

	for name, cmd := range subcommands {
		t.Run(name, func(t *testing.T) {
			output, err := runCompletion(t, name, "--")
			if err != nil {
				t.Fatalf("%s --<TAB> completion failed: %v", name, err)
			}

			for _, flag := range longFlagNames(cmd) {
				if !strings.Contains(output, flag) {
					t.Errorf("expected flag %q in %s completion output, got:\n%s", flag, name, output)
				}
			}
		})
	}
}

func TestCompletion_NestedSubcommands(t *testing.T) {
	// trust has a nested "update" subcommand
	output, err := runCompletion(t, "trust")
	if err != nil {
		t.Fatalf("trust completion failed: %v", err)
	}

	if !strings.Contains(output, "update") {
		t.Errorf("expected nested subcommand %q in trust completion output, got:\n%s", "update", output)
	}
}

func TestCompletion_RecipeFlagValues(t *testing.T) {
	tests := []struct {
		flag   string
		expect []string
	}{
		{"--intent", recipe.GetCriteriaIntentTypes()},
		{"--service", recipe.GetCriteriaServiceTypes()},
		{"--accelerator", recipe.GetCriteriaAcceleratorTypes()},
		{"--gpu", recipe.GetCriteriaAcceleratorTypes()},
		{"--os", recipe.GetCriteriaOSTypes()},
		{"--platform", recipe.GetCriteriaPlatformTypes()},
	}

	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			output, err := runCompletion(t, "recipe", tt.flag, "")
			if err != nil {
				t.Fatalf("recipe %s completion failed: %v", tt.flag, err)
			}
			for _, v := range tt.expect {
				if !strings.Contains(output, v) {
					t.Errorf("expected %q in output for %s, got:\n%s", v, tt.flag, output)
				}
			}
		})
	}
}

func TestCompletion_SharedFormatFlag(t *testing.T) {
	for _, cmd := range []string{"recipe", "snapshot"} {
		t.Run(cmd, func(t *testing.T) {
			output, err := runCompletion(t, cmd, "--format", "")
			if err != nil {
				t.Fatalf("%s --format completion failed: %v", cmd, err)
			}
			for _, v := range serializer.SupportedFormats() {
				if !strings.Contains(output, v) {
					t.Errorf("expected %q in %s --format output, got:\n%s", v, cmd, output)
				}
			}
		})
	}
}

func TestCompletion_BundleDeployerFlag(t *testing.T) {
	output, err := runCompletion(t, "bundle", "--deployer", "")
	if err != nil {
		t.Fatalf("bundle --deployer completion failed: %v", err)
	}
	for _, v := range config.GetDeployerTypes() {
		if !strings.Contains(output, v) {
			t.Errorf("expected %q in --deployer output, got:\n%s", v, output)
		}
	}
}

func TestCompletion_VerifyFlagValues(t *testing.T) {
	tests := []struct {
		flag   string
		expect []string
	}{
		{"--min-trust-level", verifier.GetTrustLevels()},
		{"--format", []string{"text", "json"}},
	}

	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			output, err := runCompletion(t, "verify", tt.flag, "")
			if err != nil {
				t.Fatalf("verify %s completion failed: %v", tt.flag, err)
			}
			for _, v := range tt.expect {
				if !strings.Contains(output, v) {
					t.Errorf("expected %q in output for %s, got:\n%s", v, tt.flag, output)
				}
			}
		})
	}
}

func TestCompletion_QueryInheritsRecipeCompletions(t *testing.T) {
	output, err := runCompletion(t, "query", "--intent", "")
	if err != nil {
		t.Fatalf("query --intent completion failed: %v", err)
	}
	for _, v := range recipe.GetCriteriaIntentTypes() {
		if !strings.Contains(output, v) {
			t.Errorf("expected %q in query --intent output, got:\n%s", v, output)
		}
	}
}

func TestFindCompletableFlag(t *testing.T) {
	cmd := &urfave.Command{
		Flags: []urfave.Flag{
			withCompletions(&urfave.StringFlag{
				Name:    "intent",
				Aliases: []string{"i"},
			}, func() []string { return []string{"inference", "training"} }),
			&urfave.StringFlag{
				Name: "output",
			},
		},
	}

	tests := []struct {
		name    string
		flagArg string
		wantOK  bool
		wantLen int
	}{
		{"primary name", "--intent", true, 2},
		{"alias", "-i", true, 2},
		{"non-completable flag", "--output", false, 0},
		{"unknown flag", "--bogus", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf, ok := findCompletableFlag(cmd, tt.flagArg)
			if ok != tt.wantOK {
				t.Fatalf("findCompletableFlag(%q) ok = %v, want %v", tt.flagArg, ok, tt.wantOK)
			}
			if ok && len(cf.Completions()) != tt.wantLen {
				t.Errorf("completions len = %d, want %d", len(cf.Completions()), tt.wantLen)
			}
		})
	}
}

func TestSanitizeCompletionArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "no completion flag",
			in:   []string{"aicr", "snapshot", "--namespace", "foo"},
			want: []string{"aicr", "snapshot", "--namespace", "foo"},
		},
		{
			name: "completion without double dash",
			in:   []string{"aicr", "snapshot", "-", "--generate-shell-completion"},
			want: []string{"aicr", "snapshot", "-", "--generate-shell-completion"},
		},
		{
			name: "double dash before completion flag",
			in:   []string{"aicr", "snapshot", "--", "--generate-shell-completion"},
			want: []string{"aicr", "snapshot", "-", "--generate-shell-completion"},
		},
		{
			name: "double dash not immediately before completion flag",
			in:   []string{"aicr", "snapshot", "--", "arg", "--generate-shell-completion"},
			want: []string{"aicr", "snapshot", "--", "arg", "--generate-shell-completion"},
		},
		{
			name: "too few args",
			in:   []string{"aicr", "--generate-shell-completion"},
			want: []string{"aicr", "--generate-shell-completion"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeCompletionArgs(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("arg[%d]: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

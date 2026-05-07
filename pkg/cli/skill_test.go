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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestExtractCLIMeta(t *testing.T) {
	root := newRootCmd()
	meta := extractCLIMeta(root)

	if meta.Name != name {
		t.Errorf("name = %q, want %q", meta.Name, name)
	}
	if meta.Version == "" {
		t.Error("version must not be empty")
	}

	// All top-level commands should be captured (snapshot, recipe, query,
	// bundle, verify, validate, trust). Hidden commands and the "skill"
	// command itself (if registered) must be excluded.
	wantCmds := []string{"snapshot", "recipe", "query", "bundle", "verify", "validate", "trust"}
	got := make(map[string]bool)
	for _, c := range meta.Commands {
		got[c.Name] = true
	}
	for _, w := range wantCmds {
		if !got[w] {
			t.Errorf("expected command %q in meta, got commands: %v", w, keys(got))
		}
	}

	// The "skill" command must NOT appear (self-reference avoidance).
	if got["skill"] {
		t.Error("skill command must be excluded from meta")
	}

	// Recipe command must have flags with completions (e.g. --service, --intent).
	var recipeMeta *cmdMeta
	for i := range meta.Commands {
		if meta.Commands[i].Name == "recipe" {
			recipeMeta = &meta.Commands[i]
			break
		}
	}
	if recipeMeta == nil {
		t.Fatal("recipe command not found in meta")
	}

	completionFound := false
	for _, f := range recipeMeta.Flags {
		if len(f.Completions) > 0 {
			completionFound = true
			break
		}
	}
	if !completionFound {
		t.Error("expected at least one flag with completions on recipe command")
	}

	// Verify help and version flags are excluded.
	for _, c := range meta.Commands {
		for _, f := range c.Flags {
			if f.Name == "help" || f.Name == "version" {
				t.Errorf("flag %q must be excluded from command %q", f.Name, c.Name)
			}
		}
	}

	// Trust command should have a subcommand "update".
	var trustMeta *cmdMeta
	for i := range meta.Commands {
		if meta.Commands[i].Name == "trust" {
			trustMeta = &meta.Commands[i]
			break
		}
	}
	if trustMeta == nil {
		t.Fatal("trust command not found in meta")
	}
	subFound := false
	for _, sub := range trustMeta.Subcommands {
		if sub.Name == "update" {
			subFound = true
			break
		}
	}
	if !subFound {
		t.Error("expected trust subcommand 'update' in meta")
	}

	// Root-level (global) flags must be captured on cliMeta.Flags so they
	// appear in the generated skill output.
	rootFlags := make(map[string]bool, len(meta.Flags))
	for _, f := range meta.Flags {
		rootFlags[f.Name] = true
	}
	for _, want := range []string{"debug", "log-json"} {
		if !rootFlags[want] {
			t.Errorf("expected root flag %q in meta.Flags, got: %v", want, keys(rootFlags))
		}
	}
	if rootFlags["help"] || rootFlags["version"] {
		t.Errorf("auto-generated flags must be excluded from meta.Flags, got: %v", keys(rootFlags))
	}
}

// TestExtractCLIMetaAfterRun exercises the post-Run() command tree, which is
// the state the skill command actually sees in production. urfave/cli/v3
// injects an unhidden "help" subcommand into every non-leaf command (via
// setupDefaults -> ensureHelp) and a "completion" command at the root (with
// Hidden=false because root.go's ConfigureShellCompletionCommand flips it).
// extractCLIMeta must filter both, otherwise the generated SKILL.md ends up
// with `aicr <cmd> help` and `aicr completion` noise that misleads agents.
func TestExtractCLIMetaAfterRun(t *testing.T) {
	root := newRootCmd()
	// Run with --version to trigger setupDefaults without doing real work;
	// urfave returns immediately after printing version, but the command tree
	// is fully set up by then.
	if err := root.Run(context.Background(), []string{name, "--version"}); err != nil {
		t.Fatalf("root.Run failed: %v", err)
	}

	meta := extractCLIMeta(root)

	for _, c := range meta.Commands {
		if isFrameworkCommand(c.Name) {
			t.Errorf("framework command %q must be excluded from meta.Commands", c.Name)
		}
		assertNoFrameworkSubcommand(t, c, c.Name)
	}
}

func assertNoFrameworkSubcommand(t *testing.T, c cmdMeta, path string) {
	t.Helper()
	for _, sub := range c.Subcommands {
		full := path + " " + sub.Name
		if isFrameworkCommand(sub.Name) {
			t.Errorf("framework subcommand %q must be excluded (under %q)", sub.Name, full)
		}
		assertNoFrameworkSubcommand(t, sub, full)
	}
}

// TestWriteFlagEntryNormalizesUsage verifies that flag Usage strings carrying
// embedded newlines or tabs (e.g. recipe's --snapshot/--config multi-line
// descriptions) are collapsed onto a single markdown bullet so the generated
// SKILL.md does not get its layout broken.
func TestWriteFlagEntryNormalizesUsage(t *testing.T) {
	root := newRootCmd()
	if err := root.Run(context.Background(), []string{name, "--version"}); err != nil {
		t.Fatalf("root.Run failed: %v", err)
	}
	meta := extractCLIMeta(root)

	out, err := (&claudeCodeGenerator{}).generate(meta)
	if err != nil {
		t.Fatalf("generate() unexpected error: %v", err)
	}
	body := string(out)

	// Recipe --snapshot has a multi-line Usage; assert the bullet line for
	// it contains no embedded newlines or tabs and stays on one line.
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "`--snapshot`") {
			continue
		}
		if strings.ContainsAny(line, "\t") {
			t.Errorf("--snapshot bullet contains tab; not normalized: %q", line)
		}
		// strings.Split already removed the trailing \n, but a multi-line
		// Usage would have produced multiple lines starting with " — ...";
		// look for the marker inside this single line.
		if !strings.Contains(line, "—") {
			t.Errorf("--snapshot bullet missing usage separator: %q", line)
		}
		return
	}
	t.Error("did not find --snapshot bullet in generated skill")
}

// TestExtractCmdMetaCarriesArgsUsage verifies that cmd.ArgsUsage is captured
// onto cmdMeta so positional argument syntax is preserved for downstream
// rendering. None of the current commands declare ArgsUsage, so this also
// guards against the field silently disappearing on a future refactor.
func TestExtractCmdMetaCarriesArgsUsage(t *testing.T) {
	cmd := &cli.Command{
		Name:      "demo",
		ArgsUsage: "<bundle-dir>",
		Usage:     "demo command",
	}
	m := extractCmdMeta(cmd)
	if m.ArgsUsage != "<bundle-dir>" {
		t.Errorf("ArgsUsage = %q, want %q", m.ArgsUsage, "<bundle-dir>")
	}
}

// TestWriteCommandEntryRendersArgsUsage verifies the command heading shows
// positional arg syntax when ArgsUsage is set, so e.g. `aicr verify` would
// render as `aicr verify <bundle-dir>` once that command opts in.
func TestWriteCommandEntryRendersArgsUsage(t *testing.T) {
	var buf bytes.Buffer
	cmd := cmdMeta{
		Name:      "verify",
		ArgsUsage: "<bundle-dir>",
		Usage:     "Verify a bundle.",
	}
	writeCommandEntry(&buf, "aicr", cmd, 0)
	out := buf.String()
	if !strings.Contains(out, "`aicr verify <bundle-dir>`") {
		t.Errorf("command heading missing ArgsUsage: %q", out)
	}
}

// TestWriteCriteriaValuesAllowlist verifies the criteria values section only
// includes the recipe selection criteria flags, not other completable flags
// like --format whose completions are output formats, not criteria values.
func TestWriteCriteriaValuesAllowlist(t *testing.T) {
	root := newRootCmd()
	if err := root.Run(context.Background(), []string{name, "--version"}); err != nil {
		t.Fatalf("root.Run failed: %v", err)
	}
	meta := extractCLIMeta(root)

	out, err := (&claudeCodeGenerator{}).generate(meta)
	if err != nil {
		t.Fatalf("generate() unexpected error: %v", err)
	}
	body := string(out)

	// Find the Criteria Values section bounds.
	start := strings.Index(body, "## Criteria Values")
	if start < 0 {
		t.Fatal("Criteria Values section missing from generated skill")
	}
	rest := body[start:]
	end := strings.Index(rest[len("## Criteria Values"):], "\n## ")
	if end < 0 {
		end = len(rest)
	} else {
		end += len("## Criteria Values")
	}
	section := rest[:end]

	for _, want := range []string{"service", "accelerator", "intent", "os", "platform"} {
		if !strings.Contains(section, "**"+want+"**") {
			t.Errorf("criteria values section missing real criterion %q", want)
		}
	}
	// --format is a completable recipe flag too, but it is not a criterion.
	if strings.Contains(section, "**format**") {
		t.Errorf("criteria values section must not include --format (output format, not a criterion)")
	}
}

func TestParseAgentType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    agentType
		wantErr bool
	}{
		{"claude-code", "claude-code", agentClaudeCode, false},
		{"codex", "codex", agentCodex, false},
		{"empty string", "", "", true},
		{"unknown agent", "gemini", "", true},
		{"case sensitive", "Claude-Code", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAgentType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseAgentType(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseAgentType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSupportedAgents(t *testing.T) {
	agents := supportedAgents()
	if len(agents) == 0 {
		t.Fatal("supportedAgents() must return at least one agent")
	}

	// Verify known agents are present.
	found := make(map[string]bool, len(agents))
	for _, a := range agents {
		found[a] = true
	}
	if !found[string(agentClaudeCode)] {
		t.Error("expected claude-code in supported agents")
	}
	if !found[string(agentCodex)] {
		t.Error("expected codex in supported agents")
	}
}

func TestWriteSkillFile(t *testing.T) {
	dir := t.TempDir()
	// Nested path to verify MkdirAll.
	path := filepath.Join(dir, "nested", "dir", "skill.md")

	content := []byte("# test skill content")
	if err := writeSkillFile(path, content); err != nil {
		t.Fatalf("writeSkillFile() unexpected error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("file content = %q, want %q", got, content)
	}
}

func TestWriteSkillFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim.md")
	if err := os.WriteFile(target, []byte("victim content"), 0o600); err != nil {
		t.Fatalf("failed to seed victim file: %v", err)
	}
	link := filepath.Join(dir, "skill.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	err := writeSkillFile(link, []byte("attacker content"))
	if err == nil {
		t.Fatal("writeSkillFile() expected error when target is a symlink")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite symlink") {
		t.Errorf("error = %q, want containing 'refusing to overwrite symlink'", err.Error())
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read victim: %v", err)
	}
	if string(got) != "victim content" {
		t.Errorf("victim was clobbered through symlink: got %q", got)
	}
}

func TestWriteSkillFileOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skill.md")

	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatalf("failed to create existing file: %v", err)
	}

	if err := writeSkillFile(path, []byte("new content")); err != nil {
		t.Fatalf("writeSkillFile() unexpected error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(got) != "new content" {
		t.Errorf("file content = %q, want %q", got, "new content")
	}
}

func TestConfirmOverwrite(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"yes lower", "y\n", true},
		{"yes word", "yes\n", true},
		{"yes uppercase", "Y\n", true},
		{"yes word mixed case", "Yes\n", true},
		{"no lower", "n\n", false},
		{"empty defaults to no", "\n", false},
		{"eof defaults to no", "", false},
		{"unrecognized defaults to no", "maybe\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := confirmOverwrite("/tmp/skill.md", strings.NewReader(tt.input), &out)
			if err != nil {
				t.Fatalf("confirmOverwrite() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("confirmOverwrite(%q) = %v, want %v", tt.input, got, tt.want)
			}
			if !strings.Contains(out.String(), "overwrite? [y/N]") {
				t.Errorf("prompt missing from output: %q", out.String())
			}
		})
	}
}

func TestConfirmOverwriteNonTTY(t *testing.T) {
	// Use /dev/null as a non-terminal *os.File.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open /dev/null: %v", err)
	}
	defer func() { _ = f.Close() }()

	_, err = confirmOverwrite("/tmp/skill.md", f, io.Discard)
	if err == nil {
		t.Fatal("confirmOverwrite() expected error for non-TTY input")
	}
	if !strings.Contains(err.Error(), "use --force to overwrite") {
		t.Errorf("error message = %q, want containing '--force' hint", err.Error())
	}
}

func TestUserHomeDir(t *testing.T) {
	dir, err := userHomeDir()
	if err != nil {
		t.Fatalf("userHomeDir() unexpected error: %v", err)
	}
	if dir == "" {
		t.Error("userHomeDir() returned empty string")
	}
}

func TestSkillInstallPath(t *testing.T) {
	path, err := skillInstallPath(".claude/skills/aicr/SKILL.md")
	if err != nil {
		t.Fatalf("skillInstallPath() unexpected error: %v", err)
	}
	if path == "" {
		t.Error("skillInstallPath() returned empty string")
	}
	// Must end with the expected suffix.
	want := filepath.Join(".claude", "skills", "aicr", "SKILL.md")
	if !strings.HasSuffix(path, want) {
		t.Errorf("skillInstallPath() = %q, want suffix %q", path, want)
	}
}

func TestExtractFlagMeta(t *testing.T) {
	meta := extractCLIMeta(newRootCmd())

	// Find snapshot command to check flag type extraction.
	var snapMeta *cmdMeta
	for i := range meta.Commands {
		if meta.Commands[i].Name == "snapshot" {
			snapMeta = &meta.Commands[i]
			break
		}
	}
	if snapMeta == nil {
		t.Fatal("snapshot command not found in meta")
	}

	// Check that boolean flags are typed correctly.
	flagTypes := make(map[string]string)
	for _, f := range snapMeta.Flags {
		flagTypes[f.Name] = f.Type
	}

	if flagTypes["no-cleanup"] != flagTypeBool {
		t.Errorf("no-cleanup type = %q, want %q", flagTypes["no-cleanup"], flagTypeBool)
	}
	if flagTypes["timeout"] != flagTypeDuration {
		t.Errorf("timeout type = %q, want %q", flagTypes["timeout"], flagTypeDuration)
	}
	if flagTypes["namespace"] != flagTypeString {
		t.Errorf("namespace type = %q, want %q", flagTypes["namespace"], flagTypeString)
	}
}

// TestExtractFlagMetaCompletableStringFlag verifies that completableStringFlag
// (the only known wrapper today) has all StringFlag fields captured: Usage,
// Default, Required, AND the wrapper's Completions().
func TestExtractFlagMetaCompletableStringFlag(t *testing.T) {
	wrapped := withCompletions(&cli.StringFlag{
		Name:     "agent",
		Usage:    "target coding agent",
		Value:    "claude-code",
		Required: true,
	}, func() []string { return []string{"claude-code", "codex"} })

	fm := extractFlagMeta(wrapped)
	if fm == nil {
		t.Fatal("extractFlagMeta() returned nil for completableStringFlag")
	}
	if fm.Name != "agent" {
		t.Errorf("Name = %q, want %q", fm.Name, "agent")
	}
	if fm.Type != flagTypeString {
		t.Errorf("Type = %q, want %q", fm.Type, flagTypeString)
	}
	if fm.Usage != "target coding agent" {
		t.Errorf("Usage = %q, want %q", fm.Usage, "target coding agent")
	}
	if fm.Default != "claude-code" {
		t.Errorf("Default = %q, want %q", fm.Default, "claude-code")
	}
	if !fm.Required {
		t.Error("Required = false, want true")
	}
	wantCompletions := []string{"claude-code", "codex"}
	if len(fm.Completions) != len(wantCompletions) {
		t.Fatalf("Completions = %v, want %v", fm.Completions, wantCompletions)
	}
	for i, c := range wantCompletions {
		if fm.Completions[i] != c {
			t.Errorf("Completions[%d] = %q, want %q", i, fm.Completions[i], c)
		}
	}
}

func TestSkillGenerateClaudeCode(t *testing.T) {
	root := newRootCmd()
	meta := extractCLIMeta(root)

	gen := &claudeCodeGenerator{}
	content, err := gen.generate(meta)
	if err != nil {
		t.Fatalf("generate() unexpected error: %v", err)
	}

	out := string(content)

	// Must start with YAML frontmatter.
	if !strings.HasPrefix(out, "---\n") {
		t.Error("output must start with YAML frontmatter delimiter '---'")
	}

	// Frontmatter fields.
	mustContain := []string{
		"name: aicr",
		"user_invocable: true",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("output missing frontmatter field %q", s)
		}
	}

	// All functional commands must appear.
	wantCmds := []string{"snapshot", "recipe", "query", "bundle", "validate"}
	for _, cmd := range wantCmds {
		if !strings.Contains(out, cmd) {
			t.Errorf("output missing command %q", cmd)
		}
	}

	// Workflow examples.
	wantExamples := []string{
		"aicr snapshot",
		"aicr recipe",
		"aicr bundle",
		"aicr validate",
	}
	for _, ex := range wantExamples {
		if !strings.Contains(out, ex) {
			t.Errorf("output missing workflow example %q", ex)
		}
	}

	// Output format guidance.
	if !strings.Contains(out, "--format json") {
		t.Error("output missing --format json guidance")
	}

	// Prerequisites.
	if !strings.Contains(out, "aicr --version") {
		t.Error("output missing prerequisite 'aicr --version'")
	}

	// Error handling section.
	if !strings.Contains(out, "Error Handling") {
		t.Error("output missing error handling section")
	}

	// Best practices section.
	if !strings.Contains(out, "Best Practices") {
		t.Error("output missing best practices section")
	}

	// Criteria values section should contain dynamic values from recipe flags.
	if !strings.Contains(out, "Criteria Values") {
		t.Error("output missing criteria values section")
	}

	// Global flags section must list root-level flags so agents discover them.
	if !strings.Contains(out, "Global Flags") {
		t.Error("output missing 'Global Flags' section")
	}
	for _, want := range []string{"--debug", "--log-json"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing global flag %q", want)
		}
	}
}

func TestSkillClaudeCodeInstallPath(t *testing.T) {
	gen := &claudeCodeGenerator{}
	path, err := gen.installPath()
	if err != nil {
		t.Fatalf("installPath() unexpected error: %v", err)
	}

	want := filepath.Join(".claude", "skills", "aicr", "SKILL.md")
	if !strings.HasSuffix(path, want) {
		t.Errorf("installPath() = %q, want suffix %q", path, want)
	}
}

func TestSkillGenerateCodex(t *testing.T) {
	root := newRootCmd()
	meta := extractCLIMeta(root)

	gen := &codexGenerator{}
	content, err := gen.generate(meta)
	if err != nil {
		t.Fatalf("generate() unexpected error: %v", err)
	}

	claudeContent, err := (&claudeCodeGenerator{}).generate(meta)
	if err != nil {
		t.Fatalf("claude generate() unexpected error: %v", err)
	}

	out := string(content)

	// Must start with YAML frontmatter for Codex skill discovery.
	if !strings.HasPrefix(out, "---\n") {
		t.Error("output must start with YAML frontmatter delimiter '---'")
	}

	if !strings.Contains(out, "name: aicr") {
		t.Error("output missing Codex skill frontmatter name")
	}

	if out != string(claudeContent) {
		t.Error("Codex skill content must exactly match Claude Code skill content")
	}

	// Must start with a markdown heading.
	if !strings.Contains(out, "\n# ") {
		t.Error("output must start with markdown heading '# '")
	}

	// All functional commands must appear.
	wantCmds := []string{"snapshot", "recipe", "query", "bundle", "validate"}
	for _, cmd := range wantCmds {
		if !strings.Contains(out, cmd) {
			t.Errorf("output missing command %q", cmd)
		}
	}

	// Output format guidance.
	if !strings.Contains(out, "--format json") {
		t.Error("output missing --format json guidance")
	}
}

func TestSkillCodexInstallPath(t *testing.T) {
	gen := &codexGenerator{}
	path, err := gen.installPath()
	if err != nil {
		t.Fatalf("installPath() unexpected error: %v", err)
	}

	want := filepath.Join(".codex", "skills", "aicr", "SKILL.md")
	if !strings.HasSuffix(path, want) {
		t.Errorf("installPath() = %q, want suffix %q", path, want)
	}
}

func TestSkillCmd_CommandStructure(t *testing.T) {
	cmd := skillCmd()

	if cmd.Name != "skill" {
		t.Errorf("Name = %q, want %q", cmd.Name, "skill")
	}
	if cmd.Category != "Utilities" {
		t.Errorf("Category = %q, want %q", cmd.Category, "Utilities")
	}
	if cmd.Action == nil {
		t.Error("Action must not be nil")
	}

	flagNames := make(map[string]bool)
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			flagNames[n] = true
		}
	}
	if !flagNames["agent"] {
		t.Error("expected --agent flag")
	}
	if !flagNames["stdout"] {
		t.Error("expected --stdout flag")
	}
	if !flagNames["force"] {
		t.Error("expected --force flag")
	}
}

func TestSkillCmd_Stdout(t *testing.T) {
	// Use the real root command so runSkillCmd reads the same cmd.Root() as
	// production. A stripped-down root would miss regressions in top-level
	// metadata extraction (e.g., framework-command leaks, missing global
	// flags, criteria filtering) and stdout cleanliness.
	root := newRootCmd()
	var buf bytes.Buffer
	root.Writer = &buf

	err := root.Run(context.Background(), []string{name, "skill", "--agent", "claude-code", "--stdout"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, "---\n") {
		t.Error("output must start with YAML frontmatter '---'")
	}
	// The real command tree must show through to stdout — assert a real
	// top-level command appears, not just the literal "aicr" substring.
	for _, want := range []string{"aicr snapshot", "aicr recipe", "aicr bundle"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing real command entry %q", want)
		}
	}
	// And the framework-command filter must hold here too.
	for _, leak := range []string{"aicr snapshot help", "aicr completion"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaked framework command %q", leak)
		}
	}
}

func TestSkillCmd_MissingAgent(t *testing.T) {
	var buf bytes.Buffer

	root := &cli.Command{
		Name:   "aicr",
		Writer: &buf,
		Commands: []*cli.Command{
			skillCmd(),
		},
	}

	err := root.Run(context.Background(), []string{"aicr", "skill"})
	if err == nil {
		t.Fatal("expected error when --agent is missing")
	}
}

// keys returns sorted map keys for diagnostic output.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

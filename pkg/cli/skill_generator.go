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
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// agentType identifies a supported coding agent.
type agentType string

const (
	agentClaudeCode agentType = "claude-code"
	agentCodex      agentType = "codex"
)

// supportedAgents returns the string names of all recognized agent types.
// The return type is []string so it can be passed directly to withCompletions.
func supportedAgents() []string {
	return []string{string(agentClaudeCode), string(agentCodex)}
}

// parseAgentType validates and returns the agentType for the given string.
func parseAgentType(s string) (agentType, error) {
	for _, a := range supportedAgents() {
		if s == a {
			return agentType(a), nil
		}
	}
	return "", errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("unsupported agent type %q: must be one of %v", s, supportedAgents()))
}

// skillGenerator produces an agent-specific skill file from CLI metadata.
// Implementations are registered per agentType (see claude_code_generator.go,
// codex_generator.go).
type skillGenerator interface {
	// generate renders the skill file content from the given CLI metadata.
	generate(meta *cliMeta) ([]byte, error)
	// installPath returns the absolute path where the skill file should be written.
	installPath() (string, error)
}

// cliMeta captures the full CLI command tree for skill generation.
type cliMeta struct {
	Name     string
	Version  string
	Flags    []flagMeta
	Commands []cmdMeta
}

// cmdMeta captures a single CLI command. ArgsUsage carries positional
// argument syntax (e.g. "<bundle-dir>" for `aicr verify <bundle-dir>`) so the
// generated skill can show full command shapes, not just the command name.
type cmdMeta struct {
	Name        string
	ArgsUsage   string
	Usage       string
	Flags       []flagMeta
	Subcommands []cmdMeta
}

const (
	flagTypeString      = "string"
	flagTypeBool        = "bool"
	flagTypeInt         = "int"
	flagTypeDuration    = "duration"
	flagTypeStringSlice = "stringSlice"
)

// flagMeta captures a single CLI flag.
type flagMeta struct {
	Name        string
	Aliases     []string
	Usage       string
	Type        string
	Default     string
	Required    bool
	Completions []string
}

// Auto-injected command names from urfave/cli/v3. The per-command help child
// is added by ensureHelp during setupDefaults; the shell-completion command
// is added when EnableShellCompletion is true. Both surface as regular
// commands in the tree but are framework plumbing, not AICR workflow.
const (
	cmdNameHelp       = "help"
	cmdNameCompletion = "completion"
)

// isFrameworkCommand reports whether a command name belongs to one of the
// urfave/cli auto-injected commands listed above and should not appear in
// the skill output. The skill teaches AICR workflows; framework plumbing
// is noise that misleads agent consumers.
func isFrameworkCommand(name string) bool {
	return name == cmdNameHelp || name == cmdNameCompletion
}

// extractCLIMeta walks the urfave/cli command tree rooted at root and returns
// a cliMeta snapshot. Hidden commands, the "skill" command itself, and
// auto-injected framework commands ("help", "completion") are excluded;
// root-level (global) flags such as --debug and --log-json are captured on
// cliMeta.Flags so they appear in the generated skill output.
func extractCLIMeta(root *cli.Command) *cliMeta {
	meta := &cliMeta{
		Name:    root.Name,
		Version: version,
	}

	for _, f := range root.Flags {
		fm := extractFlagMeta(f)
		if fm == nil {
			continue
		}
		meta.Flags = append(meta.Flags, *fm)
	}

	for _, cmd := range root.Commands {
		if cmd.Hidden || cmd.Name == "skill" || isFrameworkCommand(cmd.Name) {
			continue
		}
		meta.Commands = append(meta.Commands, extractCmdMeta(cmd))
	}

	return meta
}

// extractCmdMeta builds a cmdMeta from a single urfave/cli command,
// recursively extracting subcommands and flags.
func extractCmdMeta(cmd *cli.Command) cmdMeta {
	m := cmdMeta{
		Name:      cmd.Name,
		ArgsUsage: cmd.ArgsUsage,
		Usage:     cmd.Usage,
	}

	for _, f := range cmd.Flags {
		fm := extractFlagMeta(f)
		if fm == nil {
			continue
		}
		m.Flags = append(m.Flags, *fm)
	}

	for _, sub := range cmd.Commands {
		if sub.Hidden || isFrameworkCommand(sub.Name) {
			continue
		}
		m.Subcommands = append(m.Subcommands, extractCmdMeta(sub))
	}

	return m
}

// extractFlagMeta converts a urfave/cli Flag into a flagMeta.
// Returns nil for help and version flags which are auto-generated.
func extractFlagMeta(f cli.Flag) *flagMeta {
	names := f.Names()
	if len(names) == 0 {
		return nil
	}

	primary := names[0]
	if primary == "help" || primary == "version" {
		return nil
	}

	fm := &flagMeta{
		Name: primary,
	}

	if len(names) > 1 {
		fm.Aliases = names[1:]
	}

	// completableStringFlag wraps *cli.StringFlag, so the inner StringFlag is
	// the source of truth for Usage/Default/Required. Match it before the
	// generic *cli.StringFlag case so all string-flag fields are captured.
	if cs, ok := f.(*completableStringFlag); ok {
		fm.Type = flagTypeString
		fm.Usage = cs.Usage
		fm.Default = cs.Value
		fm.Required = cs.Required
		fm.Completions = cs.Completions()
		return fm
	}

	switch tf := f.(type) {
	case *cli.StringFlag:
		fm.Type = flagTypeString
		fm.Usage = tf.Usage
		fm.Default = tf.Value
		fm.Required = tf.Required
	case *cli.BoolFlag:
		fm.Type = flagTypeBool
		fm.Usage = tf.Usage
		if tf.Value {
			fm.Default = "true"
		}
	case *cli.IntFlag:
		fm.Type = flagTypeInt
		fm.Usage = tf.Usage
		if tf.Value != 0 {
			fm.Default = fmt.Sprintf("%d", tf.Value)
		}
	case *cli.DurationFlag:
		fm.Type = flagTypeDuration
		fm.Usage = tf.Usage
		if tf.Value != 0 {
			fm.Default = tf.Value.String()
		}
	case *cli.StringSliceFlag:
		fm.Type = flagTypeStringSlice
		fm.Usage = tf.Usage
	default:
		if u, ok := f.(interface{ GetUsage() string }); ok {
			fm.Usage = u.GetUsage()
		}
	}

	// Check if flag provides shell completions.
	if cf, ok := f.(CompletableFlag); ok {
		fm.Completions = cf.Completions()
	}

	return fm
}

// userHomeDir returns the current user's home directory.
func userHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to determine home directory", err)
	}
	return home, nil
}

// skillInstallPath builds an absolute path relative to the user's home directory.
// Each generator provides its own relative path (e.g. ".claude/skills/aicr/SKILL.md").
func skillInstallPath(relPath string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, filepath.FromSlash(relPath)), nil
}

// writeSkillFile writes content to the given path, creating parent
// directories as needed. The write is atomic and TOCTOU-safe:
//
//  1. Lstat is used to refuse overwriting a symlink at the target — without
//     this, an attacker (or a stale dotfile setup) could redirect the write
//     after confirmation by swapping in a symlink before the final replace.
//  2. Content is written to a temp file in the same directory and committed
//     via os.Rename, so callers either see the previous content or the new
//     content, never a partially written file.
//
// The caller is responsible for any pre-write existence/confirmation checks
// (the rename itself unconditionally replaces a regular file).
func writeSkillFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create directory %s", dir), err)
	}

	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("refusing to overwrite symlink at skill path %s", path))
	}

	tmp, err := os.CreateTemp(dir, ".skill-*.tmp")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create temp file in %s", dir), err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write temp skill file %s", tmpPath), err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to close temp skill file %s", tmpPath), err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil { //nolint:gosec // G703: tmpPath is the path returned by os.CreateTemp(dir, ...), constrained to dir
		cleanup()
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to chmod temp skill file %s", tmpPath), err)
	}
	if err := os.Rename(tmpPath, path); err != nil { //nolint:gosec // G703: tmpPath from os.CreateTemp; path validated upstream by skillInstallPath
		cleanup()
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to rename temp skill file to %s", path), err)
	}

	return nil
}

// confirmOverwrite prompts the user to confirm overwriting an existing file.
// Returns true only when the user answers y/yes (case-insensitive).
//
// If in is a non-terminal *os.File (pipe, redirected file, /dev/null), the
// function returns an error instead of silently defaulting — this protects
// non-interactive runs (CI, scripts) from accidental overwrites; the caller
// should use --force to opt in. Other reader types (e.g., a bytes.Buffer in
// tests) bypass the TTY check and are read directly.
func confirmOverwrite(path string, in io.Reader, out io.Writer) (bool, error) {
	if f, ok := in.(*os.File); ok && !term.IsTerminal(int(f.Fd())) { //nolint:gosec // G115: os file descriptors fit safely in int
		return false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("skill file already exists: %s (use --force to overwrite, or remove it first)", path))
	}

	fmt.Fprintf(out, "skill file already exists: %s\noverwrite? [y/N]: ", path)

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, errors.Wrap(errors.ErrCodeInternal, "failed to read user confirmation", err)
		}
		return false, nil
	}
	response := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return response == "y" || response == "yes", nil
}

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
	"fmt"
	"strings"
)

// cmdNameRecipe is the urfave/cli command name for the recipe command.
const cmdNameRecipe = "recipe"

// Compile-time interface check.
var _ skillGenerator = (*claudeCodeGenerator)(nil)

// claudeCodeGenerator produces a Claude Code SKILL.md from CLI metadata.
type claudeCodeGenerator struct{}

func (g *claudeCodeGenerator) generate(meta *cliMeta) ([]byte, error) {
	var buf bytes.Buffer

	writeSkillFrontmatter(&buf, true)

	// Heading.
	fmt.Fprintf(&buf, "# AICR CLI (%s)\n\n", meta.Version)

	writePrerequisites(&buf)
	writeCommandReference(&buf, meta)
	writeCriteriaValues(&buf, meta)
	writeOutputFormatGuidance(&buf)
	writeWorkflowExamples(&buf)
	writeErrorHandling(&buf)
	writeBestPractices(&buf)

	return buf.Bytes(), nil
}

func (g *claudeCodeGenerator) installPath() (string, error) {
	return skillInstallPath(".claude/skills/aicr/SKILL.md")
}

func writeSkillFrontmatter(buf *bytes.Buffer, userInvocable bool) {
	fmt.Fprintf(buf, "---\n")
	fmt.Fprintf(buf, "name: aicr\n")
	fmt.Fprintf(buf, "description: |\n")
	fmt.Fprintf(buf, "  NVIDIA AI Cluster Runtime (AICR) CLI skill.\n")
	fmt.Fprintf(buf, "  Use when the user asks to generate a GPU recipe,\n")
	fmt.Fprintf(buf, "  snapshot a cluster, bundle Helm values, validate a recipe,\n")
	fmt.Fprintf(buf, "  query AICR configuration, or verify bundle provenance.\n")
	if userInvocable {
		fmt.Fprintf(buf, "user_invocable: true\n")
	}
	fmt.Fprintf(buf, "---\n\n")
}

// ---------------------------------------------------------------------------
// Shared content helpers (reused by codex generator)
// ---------------------------------------------------------------------------

// writePrerequisites writes the prerequisites section.
func writePrerequisites(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "## Prerequisites\n\n")
	fmt.Fprintf(buf, "Verify the CLI is available before running commands:\n\n")
	fmt.Fprintf(buf, "```bash\n")
	fmt.Fprintf(buf, "aicr --version\n")
	fmt.Fprintf(buf, "```\n\n")
}

// writeCommandReference writes a dynamic command reference from CLI metadata.
func writeCommandReference(buf *bytes.Buffer, meta *cliMeta) {
	fmt.Fprintf(buf, "## Command Reference\n\n")

	if len(meta.Flags) > 0 {
		fmt.Fprintf(buf, "### Global Flags\n\n")
		fmt.Fprintf(buf, "Available on every `%s` invocation:\n\n", meta.Name)
		for _, f := range meta.Flags {
			writeFlagEntry(buf, f)
		}
		fmt.Fprintf(buf, "\n")
	}

	for _, cmd := range meta.Commands {
		writeCommandEntry(buf, meta.Name, cmd, 0)
	}
}

// writeCommandEntry writes a single command with its flags, recursing into subcommands.
func writeCommandEntry(buf *bytes.Buffer, parent string, cmd cmdMeta, depth int) {
	full := parent + " " + cmd.Name
	heading := strings.Repeat("#", depth+3)

	header := full
	if argsUsage := normalizeMarkdownText(cmd.ArgsUsage); argsUsage != "" {
		header = full + " " + argsUsage
	}
	fmt.Fprintf(buf, "%s `%s`\n\n", heading, header)

	if cmd.Usage != "" {
		fmt.Fprintf(buf, "%s\n\n", cmd.Usage)
	}

	if len(cmd.Flags) > 0 {
		fmt.Fprintf(buf, "**Flags:**\n\n")
		for _, f := range cmd.Flags {
			writeFlagEntry(buf, f)
		}
		fmt.Fprintf(buf, "\n")
	}

	for _, sub := range cmd.Subcommands {
		writeCommandEntry(buf, full, sub, depth+1)
	}
}

// writeFlagEntry writes a single flag as a markdown list item.
func writeFlagEntry(buf *bytes.Buffer, f flagMeta) {
	aliases := ""
	if len(f.Aliases) > 0 {
		parts := make([]string, len(f.Aliases))
		for i, a := range f.Aliases {
			if len(a) == 1 {
				parts[i] = "-" + a
			} else {
				parts[i] = "--" + a
			}
		}
		aliases = " (" + strings.Join(parts, ", ") + ")"
	}

	line := fmt.Sprintf("- `--%s`%s", f.Name, aliases)

	if f.Type != "" && f.Type != flagTypeString {
		line += fmt.Sprintf(" [%s]", f.Type)
	}

	// Flag Usage strings can carry embedded newlines/tabs (e.g. recipe's
	// --snapshot/--config multi-line descriptions); collapse whitespace
	// so the markdown bullet renders as a single line.
	if usage := normalizeMarkdownText(f.Usage); usage != "" {
		line += " — " + usage
	}

	if f.Default != "" {
		line += fmt.Sprintf(" (default: `%s`)", f.Default)
	}

	if f.Required {
		line += " **required**"
	}

	if len(f.Completions) > 0 {
		line += fmt.Sprintf("  \n  Values: `%s`", strings.Join(f.Completions, "`, `"))
	}

	fmt.Fprintf(buf, "%s\n", line)
}

// normalizeMarkdownText collapses any run of whitespace (including newlines
// and tabs) to single spaces so the result is safe to inline into a single
// markdown line. Leading/trailing whitespace is trimmed.
func normalizeMarkdownText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// criteriaFlagAllowlist is the set of recipe flags that represent recipe
// selection criteria. Other completable flags on the recipe command (e.g.
// --format, which advertises yaml/json/table) must be excluded here so the
// generated skill does not mislabel them as criteria for agent consumers.
var criteriaFlagAllowlist = map[string]struct{}{
	"service":     {},
	"accelerator": {},
	"intent":      {},
	"os":          {},
	"platform":    {},
}

// writeCriteriaValues writes the dynamic criteria values section extracted
// from the recipe command's criteria flag completions.
func writeCriteriaValues(buf *bytes.Buffer, meta *cliMeta) {
	var recipeMeta *cmdMeta
	for i := range meta.Commands {
		if meta.Commands[i].Name == cmdNameRecipe {
			recipeMeta = &meta.Commands[i]
			break
		}
	}
	if recipeMeta == nil {
		return
	}

	type criteriaField struct {
		name   string
		values []string
	}

	var fields []criteriaField
	for _, f := range recipeMeta.Flags {
		if len(f.Completions) == 0 {
			continue
		}
		if _, ok := criteriaFlagAllowlist[f.Name]; !ok {
			continue
		}
		fields = append(fields, criteriaField{name: f.Name, values: f.Completions})
	}

	if len(fields) == 0 {
		return
	}

	fmt.Fprintf(buf, "## Criteria Values\n\n")
	fmt.Fprintf(buf, "Valid values for recipe criteria flags:\n\n")

	for _, cf := range fields {
		fmt.Fprintf(buf, "- **%s**: `%s`\n", cf.name, strings.Join(cf.values, "`, `"))
	}

	fmt.Fprintf(buf, "\n")
}

// writeOutputFormatGuidance writes static output format guidance.
func writeOutputFormatGuidance(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "## Output Format\n\n")
	fmt.Fprintf(buf, "Use `--format json` for machine-readable output. Pipe through `jq` for field extraction:\n\n")
	fmt.Fprintf(buf, "```bash\n")
	fmt.Fprintf(buf, "aicr recipe --service eks --accelerator h100 --intent training --os ubuntu --format json | jq '.componentRefs[].name'\n")
	fmt.Fprintf(buf, "```\n\n")
	fmt.Fprintf(buf, "Default output format is YAML.\n\n")
}

// writeWorkflowExamples writes static workflow example content.
func writeWorkflowExamples(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "## Workflow Examples\n\n")

	fmt.Fprintf(buf, "**Full pipeline:**\n\n")
	fmt.Fprintf(buf, "```bash\n")
	fmt.Fprintf(buf, "# Capture cluster state\n")
	fmt.Fprintf(buf, "aicr snapshot --output snapshot.yaml\n\n")
	fmt.Fprintf(buf, "# Generate optimized recipe\n")
	fmt.Fprintf(buf, "aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml\n\n")
	fmt.Fprintf(buf, "# Validate recipe against cluster\n")
	fmt.Fprintf(buf, "aicr validate --recipe recipe.yaml --snapshot snapshot.yaml\n\n")
	fmt.Fprintf(buf, "# Create deployment bundle\n")
	fmt.Fprintf(buf, "aicr bundle --recipe recipe.yaml --output ./bundles\n")
	fmt.Fprintf(buf, "```\n\n")

	fmt.Fprintf(buf, "**Query specific values:**\n\n")
	fmt.Fprintf(buf, "```bash\n")
	fmt.Fprintf(buf, "aicr query --service eks --accelerator gb200 --intent training --os ubuntu \\\n")
	fmt.Fprintf(buf, "  --format json --selector components.gpu-operator.values.driver\n")
	fmt.Fprintf(buf, "```\n\n")

	fmt.Fprintf(buf, "**Bundle with value overrides:**\n\n")
	fmt.Fprintf(buf, "```bash\n")
	fmt.Fprintf(buf, "aicr bundle -r recipe.yaml --set gpuoperator:driver.version=580.105.08 -o ./bundles\n")
	fmt.Fprintf(buf, "```\n\n")

	fmt.Fprintf(buf, "**Verify bundle:**\n\n")
	fmt.Fprintf(buf, "```bash\n")
	fmt.Fprintf(buf, "aicr verify ./bundles\n")
	fmt.Fprintf(buf, "```\n\n")
}

// writeErrorHandling writes static error handling guidance.
func writeErrorHandling(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "## Error Handling\n\n")
	fmt.Fprintf(buf, "- Exit code 0 indicates success; non-zero indicates failure\n")
	fmt.Fprintf(buf, "- Use `--debug` for verbose diagnostic output\n")
	fmt.Fprintf(buf, "- Errors are structured with codes: check the `code` field in JSON error output\n")
	fmt.Fprintf(buf, "- Common failures:\n")
	fmt.Fprintf(buf, "  - Cluster unreachable — verify kubeconfig and connectivity\n")
	fmt.Fprintf(buf, "  - Invalid recipe criteria — check valid values in the Criteria Values section\n")
	fmt.Fprintf(buf, "  - Missing snapshot — generate one with `aicr snapshot` first\n\n")
}

// writeBestPractices writes static best practices guidance.
func writeBestPractices(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "## Best Practices\n\n")
	fmt.Fprintf(buf, "- Always use `--format json` when parsing output programmatically\n")
	fmt.Fprintf(buf, "- Pipe JSON output through `jq` for field extraction\n")
	fmt.Fprintf(buf, "- Use `--debug` when troubleshooting unexpected behavior\n")
	fmt.Fprintf(buf, "- Check `aicr --version` before running commands to ensure the expected version\n")
}

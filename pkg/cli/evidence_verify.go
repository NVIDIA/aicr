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
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

const (
	evidenceVerifyFormatText = "text"
	evidenceVerifyFormatJSON = "json"
)

// evidenceVerifyCmd implements `aicr evidence verify <input>`. Only
// directory input is supported today.
func evidenceVerifyCmd() *cli.Command {
	return &cli.Command{
		Name:     "verify",
		Category: functionalCategoryName,
		Usage:    "Verify a recipe evidence bundle (offline).",
		Description: `Verifies a recipe-evidence v1 bundle's manifest hash chain and
surfaces the signed predicate's fingerprint, phase counts, and BOM info.

Only directory input is supported today:

  aicr evidence verify ./out/summary-bundle

Pointer files (recipes/evidence/<recipe>.yaml), OCI references, and
cryptographic signature verification are not yet implemented.

Exit codes (see Exit Codes section in cli-reference.md):

  0   bundle valid; every check passed.
  2   bundle invalid, OR recorded validator results show failures.
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "output-markdown",
				Usage:    "Write the Markdown summary to this path in addition to stdout.",
				Category: catOutput,
			},
			withCompletions(&cli.StringFlag{
				Name:     "format",
				Aliases:  []string{"t"},
				Value:    evidenceVerifyFormatText,
				Usage:    "Output format: text (Markdown summary), json.",
				Category: catOutput,
			}, func() []string { return []string{evidenceVerifyFormatText, evidenceVerifyFormatJSON} }),
		},
		Action: runEvidenceVerifyCmd,
	}
}

func runEvidenceVerifyCmd(ctx context.Context, cmd *cli.Command) error {
	input := cmd.Args().First()
	if input == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"input is required: aicr evidence verify <directory>")
	}
	format := cmd.String("format")
	if format != evidenceVerifyFormatText && format != evidenceVerifyFormatJSON {
		return errors.New(errors.ErrCodeInvalidRequest, "invalid --format: must be text or json")
	}

	opts := verifier.VerifyOptions{
		Input:        input,
		MarkdownPath: cmd.String("output-markdown"),
	}

	result, err := verifier.Verify(ctx, opts)
	if err != nil {
		return err
	}
	if err := writeVerifyOutput(cmd.Root().Writer, format, result); err != nil {
		return err
	}
	if opts.MarkdownPath != "" {
		if err := verifier.WriteMarkdown(opts.MarkdownPath, result); err != nil {
			return err
		}
	}

	switch result.Exit {
	case verifier.ExitValidPassed:
		return nil
	case verifier.ExitValidPhaseFailures:
		return errors.New(errors.ErrCodeConflict,
			"bundle valid; recorded validator results show failures")
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			"bundle verification failed; see the verifier output for details")
	}
}

func writeVerifyOutput(w io.Writer, format string, r *verifier.VerifyResult) error {
	if format == evidenceVerifyFormatJSON {
		body, err := verifier.RenderJSON(r)
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write JSON output", err)
		}
		return nil
	}
	if _, err := fmt.Fprint(w, verifier.RenderMarkdown(r)); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write Markdown output", err)
	}
	return nil
}

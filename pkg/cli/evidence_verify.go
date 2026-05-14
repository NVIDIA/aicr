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
	"os"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

const (
	evidenceVerifyFormatText = "text"
	evidenceVerifyFormatJSON = "json"
)

// evidenceVerifyCmd implements `aicr evidence verify <input>`.
// Accepts three input forms (auto-detected): directory, pointer file,
// or OCI reference. When the bundle is signed (attestation.intoto.jsonl
// present), the signature is verified against the Sigstore trusted
// root and the predicate body is extracted from the verified payload.
func evidenceVerifyCmd() *cli.Command {
	return &cli.Command{
		Name:     "verify",
		Category: functionalCategoryName,
		Usage:    "Verify a recipe evidence bundle (offline).",
		Description: `Verifies a recipe-evidence v1 bundle's signature (when present)
and manifest hash chain, then surfaces the predicate's fingerprint,
phase counts, and BOM info.

Input is auto-detected as one of:

  pointer    recipes/evidence/<recipe>.yaml — pulls the OCI artifact named inside.
  oci        ghcr.io/owner/aicr-evidence@sha256:abc... or oci://...
  directory  ./out/summary-bundle/ (or a parent containing it).

The rendered output goes to stdout by default; -o writes it to a file
instead. -t selects the format (text = Markdown, json = structured).

Exit codes:

  0   bundle valid; every check passed.
  1   bundle valid; recorded validator results show failures (informational).
  2   bundle invalid (signature, schema, or integrity failure).
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     flagOutput,
				Aliases:  []string{"o"},
				Usage:    "Write output to this file (default: stdout).",
				Category: catOutput,
			},
			withCompletions(&cli.StringFlag{
				Name:     flagFormat,
				Aliases:  []string{"t"},
				Value:    evidenceVerifyFormatText,
				Usage:    "Output format: text (Markdown summary), json.",
				Category: catOutput,
			}, func() []string { return []string{evidenceVerifyFormatText, evidenceVerifyFormatJSON} }),
			&cli.StringFlag{
				Name:     "expected-issuer",
				Usage:    "Pin the OIDC issuer URL on the signing certificate (empty = any issuer).",
				Category: catEvidence,
			},
			&cli.StringFlag{
				Name:     "expected-identity-regexp",
				Usage:    "Pin the signer's SubjectAlternativeName via regex (empty = any identity).",
				Category: catEvidence,
			},
			&cli.StringFlag{
				Name:     "bundle",
				Usage:    "OCI reference override when the pointer carries no bundle.oci.",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     "registry-plain-http",
				Usage:    "Use HTTP for registry traffic (local-registry tests only).",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     "registry-insecure-tls",
				Usage:    "Skip TLS verification for the registry (self-signed certificates).",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     "allow-unpinned-tag",
				Usage:    "Accept tag-only OCI references (default: refuse). Tags are not content-addressable; opt in only for one-off debugging.",
				Category: catEvidence,
			},
		},
		Action: runEvidenceVerifyCmd,
	}
}

func runEvidenceVerifyCmd(ctx context.Context, cmd *cli.Command) error {
	input := cmd.Args().First()
	if input == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"input is required: aicr evidence verify <pointer|oci-ref|directory>")
	}
	format := cmd.String(flagFormat)
	if format != evidenceVerifyFormatText && format != evidenceVerifyFormatJSON {
		return errors.New(errors.ErrCodeInvalidRequest, "invalid --format: must be text or json")
	}

	result, err := verifier.Verify(ctx, verifier.VerifyOptions{
		Input:                  input,
		BundleRef:              cmd.String("bundle"),
		ExpectedIssuer:         cmd.String("expected-issuer"),
		ExpectedIdentityRegexp: cmd.String("expected-identity-regexp"),
		PlainHTTP:              cmd.Bool("registry-plain-http"),
		InsecureTLS:            cmd.Bool("registry-insecure-tls"),
		AllowUnpinnedTag:       cmd.Bool("allow-unpinned-tag"),
	})
	if err != nil {
		return err
	}

	w, closeFn, err := openVerifyOutput(cmd.String(flagOutput), cmd.Root().Writer)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := writeVerifyOutput(w, format, result); err != nil {
		return err
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

// openVerifyOutput resolves the --output flag. Empty path returns the
// CLI's default writer (stdout); a path opens the file for writing and
// returns a closer that the caller must invoke (typically via defer).
func openVerifyOutput(path string, stdout io.Writer) (io.Writer, func(), error) {
	if path == "" {
		return stdout, func() {}, nil
	}
	f, err := os.Create(path) //nolint:gosec // operator-supplied destination
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to open --output file for writing", err)
	}
	return f, func() { _ = f.Close() }, nil
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

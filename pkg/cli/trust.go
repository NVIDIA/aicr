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
	"os"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
	"github.com/urfave/cli/v3"
)

func trustCmd() *cli.Command {
	return &cli.Command{
		Name:     "trust",
		Category: functionalCategoryName,
		Usage:    "Manage Sigstore trusted root for attestation verification.",
		Commands: []*cli.Command{
			trustUpdateCmd(),
		},
	}
}

func trustUpdateCmd() *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "Fetch the latest Sigstore trusted root via TUF.",
		Description: `Fetches the latest Sigstore trusted root and Rekor v2 signing
config from the TUF CDN and updates the local cache. This is needed
when Sigstore rotates their signing keys (a few times per year).

The trusted root enables offline verification of bundle attestations
without contacting Sigstore infrastructure. The signing config drives
which Rekor/timestamp endpoints signing writes to (e.g. Rekor v2).

Use --emit-signing-config to also write the signing config to a file
for tools that take a signing-config path (e.g. cosign attest-blob).

Example:
  aicr trust update
  aicr trust update --emit-signing-config signing-config.json
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  flagEmitSigningConfig,
				Usage: "Write the fetched Rekor v2 signing config to this file path.",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			material, err := trust.Update(ctx)
			if err != nil {
				// trust.Update returns coded errors (Unauthorized for sig
				// failures, Timeout for ctx, Unavailable for transport);
				// preserve the inner code rather than re-wrapping.
				return err
			}

			w := cmd.Root().Writer
			fmt.Fprintf(w, "  ✓ Trusted root updated\n")
			fmt.Fprintf(w, "  CAs: %d certificate authorities\n", len(material.FulcioCertificateAuthorities()))
			fmt.Fprintf(w, "  Logs: %d transparency logs\n", len(material.RekorLogs()))

			// Report the Rekor v2 signing config warmed into the cache by Update.
			// Best-effort: the trusted root (verification) is the primary result,
			// so a missing/failed signing config is surfaced as a note, not an
			// error — signers that need it get a clear error at sign time.
			if sc, scErr := trust.GetSigningConfig(); scErr != nil {
				fmt.Fprintf(w, "  Signing config: unavailable (%v)\n", scErr)
			} else {
				fmt.Fprintf(w, "  ✓ Signing config updated\n")
				for _, s := range sc.RekorLogURLs() {
					fmt.Fprintf(w, "  Rekor: %s (v%d)\n", s.URL, s.MajorAPIVersion)
				}
			}

			// Optionally materialize the signing config to a file for tools that
			// take a signing-config path (cosign). Writes the raw TUF-verified
			// bytes so the file is byte-identical to what Sigstore distributes.
			if out := cmd.String(flagEmitSigningConfig); out != "" {
				scJSON, err := trust.SigningConfigJSON()
				if err != nil {
					return err
				}
				if err := os.WriteFile(out, scJSON, 0o600); err != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to write signing config file", err)
				}
				fmt.Fprintf(w, "  ✓ Signing config written to %s\n", out)
			}

			return nil
		},
	}
}

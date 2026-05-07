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
		Description: `Fetches the latest Sigstore trusted root from the TUF CDN and
updates the local cache. This is needed when Sigstore rotates
their signing keys (a few times per year).

The trusted root enables offline verification of bundle attestations
without contacting Sigstore infrastructure.

Example:
  aicr trust update
`,
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

			return nil
		},
	}
}

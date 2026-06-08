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

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	recipecat "github.com/NVIDIA/aicr/pkg/recipe/catalog"
)

func recipeVerifyCatalogCmd() *cli.Command {
	return &cli.Command{
		Name:      "verify-catalog",
		Usage:     "Verify the embedded recipe catalog against its Sigstore bundle.",
		ArgsUsage: "<bundle-path>",
		Description: `Recomputes the deterministic SHA-256 over the embedded registry.yaml and
validators/catalog.yaml, then verifies the digest against the provided
Sigstore bundle (.sigstore.json) using NVIDIA CI identity pinning.

The bundle is distributed as a release asset (recipe-catalog.sigstore.json)
alongside each tagged aicr binary. Download it from the GitHub Releases page
for the version you are running:

  curl -Lo recipe-catalog.sigstore.json \
    https://github.com/NVIDIA/aicr/releases/download/vX.Y.Z/recipe-catalog.sigstore.json

  aicr recipe verify-catalog recipe-catalog.sigstore.json

Exit code is 0 on success, non-zero on any verification failure.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "identity-pattern",
				Usage:   "Override the NVIDIA CI certificate identity regexp. Must contain 'NVIDIA/aicr'.",
				Sources: cli.EnvVars("AICR_CATALOG_IDENTITY_PATTERN"),
			},
		},
		Action: runRecipeVerifyCatalogCmd,
	}
}

func runRecipeVerifyCatalogCmd(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() != 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"usage: aicr recipe verify-catalog <bundle-path>")
	}
	bundlePath := cmd.Args().First()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")

	result, err := recipecat.Verify(ctx, bundlePath, provider, recipecat.VerifyOptions{
		CertificateIdentityRegexp: cmd.String("identity-pattern"),
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.Root().Writer, "catalog verified\n  digest:   sha256:%s\n  identity: %s\n",
		result.Digest, result.Identity)
	return nil
}

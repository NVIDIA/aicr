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
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// evidenceDigestCmd implements `aicr evidence digest -r <recipe-or-overlay>`.
// It produces the same hex digest that `aicr validate --emit-attestation`
// records in predicate.recipe.digest, so a CI gate can compare the digest
// pinned inside a signed bundle against the recipe on the PR branch
// without pulling the OCI artifact.
func evidenceDigestCmd() *cli.Command {
	return &cli.Command{
		Name:     "digest",
		Category: functionalCategoryName,
		Usage:    "Print the canonical digest of a resolved recipe.",
		Description: `Resolves the input to a RecipeResult (overlays are auto-hydrated via the
recipe builder, the same path ` + "`aicr validate -r`" + ` takes) and prints
the lowercase hex sha256 of the canonical YAML — byte-for-byte the same
digest stored in predicate.recipe.digest by ` + "`aicr validate --emit-attestation`" + `.

Useful for CI gates that need to detect drift between a checked-in
evidence pointer and the current recipe on the PR branch:

  signed=$(aicr evidence verify recipes/evidence/<slug>.yaml --format json |
           jq -r .predicate.recipe.digest)
  current=$(aicr evidence digest -r recipes/overlays/<file>.yaml)
  [[ "$signed" == "$current" ]] || echo "evidence is stale"

Exit codes:

  0   digest printed.
  non-zero   input could not be loaded, hydrated, or canonicalized.
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     cmdNameRecipe,
				Aliases:  []string{"r"},
				Usage:    "Path/URI to a recipe or overlay file (file, HTTP/HTTPS, or cm://namespace/name).",
				Category: catInput,
				Required: true,
			},
			kubeconfigFlag(),
		},
		Action: runEvidenceDigestCmd,
	}
}

func runEvidenceDigestCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, cmdNameRecipe, "kubeconfig"); err != nil {
		return err
	}

	path := cmd.String(cmdNameRecipe)
	if path == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--recipe is required: aicr evidence digest -r <recipe-or-overlay>")
	}

	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	digest, err := attestation.ComputeRecipeDigest(ctx, dp, path, cmd.String("kubeconfig"), version)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintln(cmd.Root().Writer, digest); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write digest", err)
	}
	return nil
}

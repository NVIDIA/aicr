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
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

func recipeSignCatalogCmd() *cli.Command {
	return &cli.Command{
		Name:   "sign-catalog",
		Hidden: true,
		Usage:  "Sign the embedded recipe catalog (registry + validator catalog). CI-only.",
		Description: `Compute a deterministic SHA-256 over registry.yaml and validators/catalog.yaml,
sign the digest via Sigstore keyless OIDC, and write a recipe-catalog.sigstore.json
bundle to --output. Called by the goreleaser after-hook on every tagged release.

Keyless OIDC signing uses the same precedence chain as 'aicr bundle --attest':
  --identity-token > COSIGN_IDENTITY_TOKEN env > GitHub Actions ambient OIDC >
  --oidc-device-flow > interactive browser flow.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "output",
				Aliases:  []string{"o"},
				Usage:    "Path to write the recipe-catalog.sigstore.json bundle.",
				Required: true,
			},
			&cli.StringFlag{
				Name:    flagIdentityToken,
				Usage:   "Pre-fetched OIDC identity token for keyless signing.",
				Sources: cli.EnvVars("COSIGN_IDENTITY_TOKEN"),
			},
			&cli.BoolFlag{
				Name:    flagOIDCDeviceFlow,
				Usage:   "Use OAuth 2.0 device authorization grant for OIDC.",
				Sources: cli.EnvVars("AICR_OIDC_DEVICE_FLOW"),
			},
			// No --fulcio-url counterpart to `bundle --attest`: the catalog
			// signature is verified by `recipe verify-catalog` against the
			// public-good Sigstore root only, so a private CA here could only
			// produce an artifact nothing can verify. Client.SignCatalog
			// rejects the setting for the same reason.
			&cli.StringFlag{
				Name:    flagRekorURL,
				Usage:   "Sign to Rekor v1 at this URL instead of the Rekor v2 default (e.g. a private v1 instance, or the public-good v1 URL).",
				Sources: cli.EnvVars("AICR_REKOR_URL"),
			},
			&cli.StringFlag{
				Name:    flagSigningConfig,
				Usage:   "Path to a Sigstore signing config JSON to sign with, instead of the default Rekor v2 config (advanced).",
				Sources: cli.EnvVars("AICR_SIGNING_CONFIG"),
			},
		},
		Action: runRecipeSignCatalogCmd,
	}
}

func runRecipeSignCatalogCmd(ctx context.Context, cmd *cli.Command) error {
	output := cmd.String("output")

	rekorURL := cmd.String(flagRekorURL)
	signingConfig := cmd.String(flagSigningConfig)
	// Shared with bundle --attest: derive the Rekor v2 default and enforce the
	// --rekor-url / --signing-config exclusivity in one place.
	useV2, err := signingTargetFromFlags(rekorURL, signingConfig)
	if err != nil {
		return err
	}

	client, err := embeddedClient()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// The facade sets Attest, resolves the attester lazily, stamps
	// ToolVersion from the Client's version, and rejects a nil bundle.
	result, err := client.SignCatalog(ctx, aicr.CatalogSignOptions{
		Output: output,
		OIDCResolve: aicr.OIDCResolveOptions{
			IdentityToken:       cmd.String(flagIdentityToken),
			AmbientURL:          os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
			AmbientToken:        os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
			DeviceFlow:          cmd.Bool(flagOIDCDeviceFlow),
			RekorURL:            rekorURL,
			SigningConfigPath:   signingConfig,
			UseTUFSigningConfig: useV2,
			PromptWriter:        os.Stderr,
		},
	})
	if err != nil {
		return err
	}

	slog.Info("catalog signed", "digest", result.Digest, "output", output)
	fmt.Fprintf(cmd.Root().Writer, "catalog signed: sha256:%s -> %s\n", result.Digest, output)
	return nil
}

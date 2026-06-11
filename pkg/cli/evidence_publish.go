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
	"os"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// evidencePublishCmd implements `aicr evidence publish <bundle-dir> --push <ref>`.
// It signs and pushes an already-emitted on-disk evidence bundle, then
// writes pointer.yaml — the off-network second leg of the workflow whose
// first leg is `aicr validate --emit-attestation` (no --push).
func evidencePublishCmd() *cli.Command {
	return &cli.Command{
		Name:      "publish",
		Category:  functionalCategoryName,
		Usage:     "Sign and push an already-emitted evidence bundle.",
		ArgsUsage: "<bundle-dir>",
		Description: `Sign, push, and write the pointer for a recipe-evidence v1 bundle
that was produced earlier by ` + "`aicr validate --emit-attestation`" + ` (without
--push), leaving an unsigned bundle on disk.

This decouples the cluster-bound validate step from the
Fulcio/Rekor-bound signing step so they can run on different networks:
validate where the cluster is reachable (often a corporate VPN), then
publish from a host with Sigstore egress (CI runner, jump box, hotspot).

The signed artifact is content-addressable, so the result is identical
to the one-shot ` + "`validate --emit-attestation --push`" + ` output regardless of
which host ran which leg — the predicate (including its baked-in
attestedAt timestamp) is signed verbatim from the bundle on disk.

<bundle-dir> is either the directory ` + "`--emit-attestation`" + ` wrote (holds
summary-bundle/ and receives pointer.yaml) or the summary-bundle/
directory itself.

Keyless OIDC signing uses the same precedence chain as ` + "`aicr validate --push`" + `:
--identity-token > COSIGN_IDENTITY_TOKEN env > GitHub Actions ambient OIDC >
--oidc-device-flow > interactive browser flow.

Example:

  # On VPN: produce an unsigned bundle from a passing validation.
  aicr validate -r recipe.yaml -s snapshot.yaml --emit-attestation ./out

  # Off VPN: sign, push, and write the pointer.
  aicr evidence publish ./out --push ghcr.io/myorg/aicr-evidence`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     flagPush,
				Usage:    "OCI registry reference (e.g. ghcr.io/myorg/aicr-evidence) to push the signed summary bundle to. Required.",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     flagPlainHTTP,
				Usage:    "Use HTTP instead of HTTPS when pushing the evidence OCI artifact (local registry tests).",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     flagInsecureTLS,
				Usage:    "Skip TLS verification when pushing the evidence OCI artifact (self-signed registries).",
				Category: catEvidence,
			},
			&cli.StringFlag{
				Name:     flagIdentityToken,
				Usage:    "Pre-fetched OIDC identity token for keyless signing. Skips ambient/browser/device-code flows. Prefer COSIGN_IDENTITY_TOKEN on shared hosts; flag values are visible in process listings (ps, /proc/<pid>/cmdline).",
				Sources:  cli.EnvVars("COSIGN_IDENTITY_TOKEN"),
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     flagOIDCDeviceFlow,
				Usage:    "Use the OAuth 2.0 device authorization grant for OIDC instead of opening a browser callback. Useful on headless hosts when --identity-token / COSIGN_IDENTITY_TOKEN and ambient GitHub Actions OIDC are both unavailable.",
				Sources:  cli.EnvVars("AICR_OIDC_DEVICE_FLOW"),
				Category: catEvidence,
			},
			assumeYesFlag(catEvidence),
		},
		Action: runEvidencePublishCmd,
	}
}

func runEvidencePublishCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, flagPush, flagIdentityToken); err != nil {
		return err
	}

	dir := cmd.Args().First()
	if dir == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"bundle directory is required: aicr evidence publish <bundle-dir> --push <ref>")
	}
	push := cmd.String(flagPush)
	if push == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--push <oci-ref> is required")
	}

	// `evidence publish` always signs (push is required), so gate the
	// interactive keyless login behind the identity-disclosure prompt before
	// it can open a browser/device-code flow. Non-interactive token sources
	// and non-TTY runs pass through inside the gate.
	oidcResolve := oidcResolveOptionsFromFlags(cmd)
	if err := confirmKeylessSigningDisclosure(oidcResolve, cmd.Bool(flagAssumeYes), os.Stdin, os.Stderr); err != nil {
		return err
	}

	err := attestation.Publish(ctx, attestation.PublishOptions{
		BundleDir:   dir,
		Push:        push,
		PlainHTTP:   cmd.Bool(flagPlainHTTP),
		InsecureTLS: cmd.Bool(flagInsecureTLS),
		AICRVersion: version,
		OIDCResolve: oidcResolve,
	})
	// Publish already returns coded pkg/errors; PropagateOrWrap preserves
	// those and only classifies any uncoded error that slips through.
	return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to publish evidence bundle")
}

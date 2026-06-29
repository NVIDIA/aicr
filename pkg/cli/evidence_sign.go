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
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

// evidenceSignCmd implements `aicr evidence sign <pointer>`. It completes the
// signing leg for a bundle that was already pushed unsigned (via
// `--no-sign`): it pulls the bundle the committed pointer references, signs
// it with ambient OIDC, attaches the Sigstore Bundle as an OCI referrer, and
// patches the pointer's signer block in place. This is the step the
// fork-based CI workflow runs after a contributor commits an unsigned pointer.
func evidenceSignCmd() *cli.Command {
	return &cli.Command{
		Name:      "sign",
		Category:  functionalCategoryName,
		Usage:     "Sign an already-pushed, unsigned evidence bundle and patch its pointer.",
		ArgsUsage: "<pointer>",
		Description: `Sign the bundle a committed pointer references, then fill in the
pointer's signer block in place.

Consumes the unsigned pointer left by ` + "`aicr evidence publish --no-sign`" + ` (or
` + "`validate --emit-attestation --push --no-sign`" + `): the pointer already carries
` + "`bundle.oci`" + ` + ` + "`bundle.digest`" + `, so no recipe-name or bundle-ref input is
needed. The bundle is pulled (not re-emitted), its predicate signed with
keyless OIDC (Fulcio + Rekor), and the resulting Sigstore Bundle attached
as an OCI referrer of the existing artifact. The pointer's
` + "`signer.{identity,issuer,rekorLogIndex}`" + ` are then written back to the same file.

This is the only leg that needs Fulcio egress, so it is designed to run in
CI (GitHub Actions ambient OIDC) where Sigstore is reachable, while the
push leg runs wherever the cluster lives.

Keyless OIDC signing uses the same precedence chain as ` + "`aicr evidence publish`" + `:
--identity-token > COSIGN_IDENTITY_TOKEN env > GitHub Actions ambient OIDC >
--oidc-device-flow > interactive browser flow.

With --relocate, the now-signed pointer is moved from its flat pending path
(` + "`recipes/evidence/<recipe>.yaml`" + `) to its canonical per-source path
(` + "`recipes/evidence/<recipe>/<source>/<digest>.yaml`" + `) — the layout the
per-source contract gate requires. A flat pointer is the only committable
state for an unsigned pointer, since the ` + "`<source>`" + ` segment derives from the
signer it does not yet have. --relocate is idempotent: an already-signed flat
pointer is moved without re-signing.

Example:

  # In CI (ambient OIDC), after a contributor committed an unsigned pointer:
  aicr evidence sign recipes/evidence/h100-eks-ubuntu-training.yaml --relocate`,
		Flags: []cli.Flag{
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
			&cli.BoolFlag{
				Name:     flagPlainHTTP,
				Usage:    "Use HTTP instead of HTTPS for the registry (pull + referrer attach; local registry tests).",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     flagInsecureTLS,
				Usage:    "Skip TLS verification for the registry (pull + referrer attach; self-signed registries).",
				Category: catEvidence,
			},
			&cli.BoolFlag{
				Name:     flagRelocate,
				Usage:    "After signing, move the pointer from its flat pending path (recipes/evidence/<recipe>.yaml) to its canonical per-source path recipes/evidence/<recipe>/<source>/<digest>.yaml. Used by the fork-based CI signing leg to complete the commit-flat -> sign -> relocate flow.",
				Category: catEvidence,
			},
			assumeYesFlag(catEvidence),
		},
		Action: runEvidenceSignCmd,
	}
}

func runEvidenceSignCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, flagIdentityToken); err != nil {
		return err
	}

	path := cmd.Args().First()
	if path == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer file is required: aicr evidence sign <pointer>")
	}
	// Reject extra positional args so a mistyped invocation fails loudly
	// rather than silently signing only the first file.
	if cmd.Args().Len() > 1 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"exactly one pointer file is allowed: aicr evidence sign <pointer>")
	}

	relocate := cmd.Bool(flagRelocate)

	pointer, err := verifier.LoadAndValidatePointer(path)
	if err != nil {
		return err
	}

	// Idempotent recovery: with --relocate, an already-signed flat pointer
	// (signing succeeded on a prior run but the relocation didn't land) is
	// moved to its canonical path without re-signing. Without --relocate this
	// falls through to ValidateSignablePointer, which fails closed on an
	// already-signed pointer rather than re-signing it. LoadAndValidatePointer
	// already guarantees exactly one attestation, so [0] is safe.
	//
	// This trusts the on-disk signer block without re-materializing or
	// cryptographically verifying the bundle — the same metadata-only trust the
	// contract gate applies, and not a new escalation (a contributor can write
	// the nested path directly). Closing that gap (verify the signature before
	// honoring a committed signer) is tracked repo-wide in #1535.
	if relocate && pointer.Attestations[0].Signer != nil {
		dest, rerr := attestation.RelocatePointerToCanonical(path, pointer)
		if rerr != nil {
			return rerr
		}
		if dest == path {
			// RelocatePointerToCanonical no-ops when the pointer is already at
			// its canonical path; don't claim a move that didn't happen.
			slog.Info("evidence pointer already signed and at its canonical path; nothing to do",
				"path", path, "recipe", pointer.Recipe)
		} else {
			slog.Info("evidence pointer already signed; relocated to canonical path",
				"from", path, "to", dest, "recipe", pointer.Recipe)
		}
		return nil
	}

	// Fail fast (before the registry pull) unless the pointer is in the exact
	// state we sign. The rule lives in pkg/evidence/attestation — SignExisting
	// enforces it too — so the CLI cannot drift from the domain contract.
	if err = attestation.ValidateSignablePointer(pointer); err != nil {
		return err
	}

	// Signing publishes the signer's identity on the interactive keyless
	// paths, so gate it behind the disclosure prompt; ambient/token sources
	// and non-TTY runs (the CI case this command targets) pass through.
	oidcResolve := oidcResolveOptionsFromFlags(cmd)
	if discErr := confirmKeylessSigningDisclosure(oidcResolve, cmd.Bool(flagAssumeYes), os.Stdin, os.Stderr); discErr != nil {
		return discErr
	}

	plainHTTP := cmd.Bool(flagPlainHTTP)
	insecureTLS := cmd.Bool(flagInsecureTLS)

	// Pull the already-pushed bundle so SignExisting can reconstruct the
	// predicate the signature binds and so we resolve the artifact's full
	// descriptor (mediaType + size) — a pointer alone carries only the digest.
	mat, err := verifier.MaterializeBundle(ctx, verifier.VerifyOptions{
		Input:       path,
		PlainHTTP:   plainHTTP,
		InsecureTLS: insecureTLS,
	}, verifier.InputFormPointer, pointer)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeUnavailable,
			"failed to pull the bundle referenced by the pointer (is the registry package accessible?)")
	}
	defer mat.Cleanup()

	// Sign, attach the Sigstore referrer, and patch the pointer's signer
	// block back to the file the caller passed.
	if err = attestation.SignExisting(ctx, attestation.SignExistingOptions{
		Pointer:     pointer,
		PointerPath: path,
		BundleDir:   mat.BundleDir,
		Artifact: attestation.MainArtifactDescriptor{
			Digest:    mat.Digest,
			MediaType: mat.MediaType,
			Size:      mat.Size,
		},
		PlainHTTP:   plainHTTP,
		InsecureTLS: insecureTLS,
		OIDCResolve: oidcResolve,
	}); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to sign existing evidence bundle")
	}

	// SignExisting patched the in-memory pointer's signer block, so its
	// canonical <source> path is now derivable. Relocate it home to satisfy
	// the per-source contract gate.
	if relocate {
		dest, rerr := attestation.RelocatePointerToCanonical(path, pointer)
		if rerr != nil {
			// The bundle is already signed in the registry; only the local move
			// failed. Surface that explicitly: PropagateOrWrap returns the inner
			// coded error verbatim (no double-wrap), so this operator-critical
			// context would otherwise be lost. Recovery is to relocate manually
			// (re-running `aicr evidence sign --relocate` no-ops the already-done
			// signing and just moves the file), or resolve a canonical-path
			// conflict — never re-sign.
			slog.Error("bundle signed but pointer relocation failed; relocate the pointer (no re-sign needed)",
				"path", path, "recipe", pointer.Recipe, "error", rerr)
			return errors.PropagateOrWrap(rerr, errors.ErrCodeInternal,
				"signed the bundle but failed to relocate the pointer to its canonical path")
		}
		slog.Info("evidence pointer signed and relocated", "from", path, "to", dest, "recipe", pointer.Recipe)
		return nil
	}

	slog.Info("evidence pointer signed", "path", path, "recipe", pointer.Recipe)
	return nil
}

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
	"os"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/urfave/cli/v3"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// evidenceSigningFlags returns the KMS and private-Sigstore signing flags of
// every command that signs recipe evidence (`validate --push`, `evidence
// publish`, `evidence sign`). They mirror the bundle --attest flags of the same
// names, but read no environment variables: those already steer bundle
// signing, and evidence signing has always ignored them, so honoring them now
// would silently move existing evidence runs to another log or CA.
func evidenceSigningFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name: flagSigningKey,
			Usage: "Sign the evidence bundle with a KMS-backed key (awskms:// | gcpkms:// | azurekms:// | hashivault://) " +
				"instead of keyless OIDC, for CI/CD without OIDC. The pointer records signer issuer \"key\" and the " +
				"key's sha256 public-key fingerprint as its identity. Mutually exclusive with --identity-token, " +
				"--oidc-device-flow and --fulcio-url. Verify with `aicr evidence verify --key <uri|public-key.pem>`. " +
				"Key-signed evidence is for private use: the upstream evidence gate accepts keyless signers only.",
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:  flagTLogUpload,
			Value: true,
			Usage: "Upload the --signing-key signature to the Rekor transparency log (default: true). " +
				"Set --tlog-upload=false for fully offline / air-gapped signing; this requires --signing-key, " +
				"because keyless OIDC signing needs Fulcio + Rekor network access. Verify the result with " +
				"`aicr evidence verify --key <public-key.pem> --insecure-ignore-tlog`.",
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: flagFulcioURL,
			Usage: "Override the Fulcio CA URL for keyless evidence signing (e.g. a private Sigstore instance). " +
				"Must be an absolute https:// URL with no embedded credentials. Defaults to the public-good Fulcio.",
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: flagRekorURL,
			Usage: "Sign the evidence bundle to Rekor v1 at this URL instead of the Rekor v2 default (e.g. a private " +
				"Sigstore instance). Must be an absolute https:// URL with no embedded credentials. " +
				"Mutually exclusive with --signing-config.",
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: flagSigningConfig,
			Usage: "Sign the evidence bundle with a custom Sigstore signing config JSON instead of the default " +
				"Rekor v2 config (advanced). Mutually exclusive with --rekor-url.",
			Category: catEvidence,
		},
	}
}

// evidenceSigningOptionsFromFlags builds the evidence signing options from the
// --identity-token / --oidc-device-flow pair, the GitHub Actions ambient-OIDC
// env vars and evidenceSigningFlags, rejecting the combinations bundle --attest
// rejects. Shared by every command that signs an evidence bundle so the
// source-precedence wiring and the mode rules stay in one place.
//
// Evidence signing targets Rekor v2 (UseTUFSigningConfig) unless --rekor-url or
// --signing-config opts out, matching bundle and catalog signing so all AICR
// signatures land in the same log by default. See #1650.
func evidenceSigningOptionsFromFlags(cmd *cli.Command) (aicr.OIDCResolveOptions, error) {
	identityToken := cmd.String(flagIdentityToken)
	deviceFlow := cmd.Bool(flagOIDCDeviceFlow)
	fulcioURL := cmd.String(flagFulcioURL)
	rekorURL := cmd.String(flagRekorURL)
	signingConfigPath := cmd.String(flagSigningConfig)
	// A command that does not register --tlog-upload uploads (the default);
	// an unregistered flag must never read as the opt-out.
	tlogUpload := !cmd.IsSet(flagTLogUpload) || cmd.Bool(flagTLogUpload)

	signingKey := strings.TrimSpace(cmd.String(flagSigningKey))
	if cmd.IsSet(flagSigningKey) && signingKey == "" {
		return aicr.OIDCResolveOptions{}, errors.New(errors.ErrCodeInvalidRequest,
			"--"+flagSigningKey+" must not be empty")
	}
	if signingKey != "" {
		if err := bundleattest.ValidateSigningKeyRef(signingKey); err != nil {
			return aicr.OIDCResolveOptions{}, err
		}
	}
	if err := config.ValidateHTTPSURL("fulcio URL", fulcioURL); err != nil {
		return aicr.OIDCResolveOptions{}, err
	}
	if err := config.ValidateHTTPSURL("rekor URL", rekorURL); err != nil {
		return aicr.OIDCResolveOptions{}, err
	}
	if rekorURL != "" && signingConfigPath != "" {
		return aicr.OIDCResolveOptions{}, errors.New(errors.ErrCodeInvalidRequest,
			"--"+flagRekorURL+" and --"+flagSigningConfig+" are mutually exclusive")
	}
	if signingKey != "" {
		if err := validateKeylessOnlyUnset(identityToken, deviceFlow, fulcioURL); err != nil {
			return aicr.OIDCResolveOptions{}, err
		}
	}
	if !tlogUpload {
		if signingKey == "" {
			return aicr.OIDCResolveOptions{}, errors.New(errors.ErrCodeInvalidRequest,
				"--"+flagTLogUpload+"=false (air-gapped signing) requires --"+flagSigningKey+
					"; keyless OIDC signing needs Fulcio/Rekor network access")
		}
		if err := validateTLogOptOutTarget(rekorURL, signingConfigPath); err != nil {
			return aicr.OIDCResolveOptions{}, err
		}
	}

	// Parsed now so a bad file fails before a validation run and a push, and
	// the config checked here is the one signed with.
	var signingConfig *root.SigningConfig
	if signingConfigPath != "" {
		sc, err := bundleattest.LoadSigningConfigForValidation(signingConfigPath)
		if err != nil {
			return aicr.OIDCResolveOptions{}, err
		}
		signingConfig = sc
	}

	return aicr.OIDCResolveOptions{
		IdentityToken:       identityToken,
		SigningKey:          signingKey,
		DisableTLogUpload:   !tlogUpload,
		AmbientURL:          os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
		AmbientToken:        os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
		DeviceFlow:          deviceFlow,
		FulcioURL:           fulcioURL,
		RekorURL:            rekorURL,
		SigningConfigPath:   signingConfigPath,
		SigningConfig:       signingConfig,
		UseTUFSigningConfig: rekorURL == "" && signingConfigPath == "",
		PromptWriter:        os.Stderr,
	}, nil
}

// validateNoSignWithoutSigningFlags rejects --no-sign alongside a flag that
// only shapes a signature, so a request to sign with a key is never silently
// downgraded to an unsigned push.
func validateNoSignWithoutSigningFlags(cmd *cli.Command) error {
	if !cmd.Bool(flagNoSign) {
		return nil
	}
	for _, name := range []string{flagSigningKey, flagTLogUpload, flagFulcioURL, flagRekorURL, flagSigningConfig} {
		if cmd.IsSet(name) {
			return errors.New(errors.ErrCodeInvalidRequest,
				"--"+flagNoSign+" cannot be combined with --"+name+": the bundle is pushed unsigned")
		}
	}
	return nil
}

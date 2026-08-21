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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/urfave/cli/v3"
)

// verifyFormat values for the --format flag on `aicr verify`.
const (
	verifyFormatText = "text"
	verifyFormatJSON = "json"
)

func bundleVerifyCmd() *cli.Command {
	return &cli.Command{
		Name:     "verify",
		Category: functionalCategoryName,
		Usage:    "Verify bundle integrity and attestation chain.",
		Description: `Verifies a bundle's checksums, attestation signatures, and provenance chain.

Trust levels:
  verified    Full chain verified: checksums, bundle attestation, binary attestation with NVIDIA CI identity
  attested    Chain verified but binary attestation missing or external data used
  unverified  Checksums valid, no attestation files (--attest was not used)
  unknown     Missing/invalid checksums, or an attestation present but failing verification

Examples:

Verify a bundle (auto-detects maximum achievable trust level):
  aicr verify ./my-bundle

Require a minimum trust level:
  aicr verify ./my-bundle --min-trust-level verified

Require a specific creator identity:
  aicr verify ./my-bundle --require-creator jdoe@company.com

Require a minimum CLI version (bare version defaults to >= semantics):
  aicr verify ./my-bundle --cli-version-constraint 0.8.0
  aicr verify ./my-bundle --cli-version-constraint ">= 0.8.0"
  aicr verify ./my-bundle --cli-version-constraint "== 0.8.0"

Verify a privately-signed bundle against an org trusted root:
  aicr verify ./my-bundle --trust-root ./trusted_root.json

Read the verification policy from a committed AICRConfig (spec.verify):
  aicr verify ./my-bundle --config aicr-config.yaml

Verify an offline/air-gapped bundle (no transparency-log network calls; use a
local PEM key for fully offline operation, since a KMS URI still resolves remotely):
  aicr verify ./bundle --key ./bundle-signer.pub --insecure-ignore-tlog

Output as JSON:
  aicr verify ./my-bundle --format json
`,
		Flags: []cli.Flag{
			withCompletions(&cli.StringFlag{
				Name:  "min-trust-level",
				Value: "max",
				Usage: `Minimum required trust level. "max" (default) auto-detects the highest
	achievable level for this bundle and verifies against it.
	Explicit levels: verified, attested, unverified, unknown`,
			}, aicr.TrustLevels),
			&cli.StringFlag{
				Name:  "require-creator",
				Usage: "Require a specific creator identity (matched against bundle attestation certificate)",
			},
			&cli.StringFlag{
				Name: "cli-version-constraint",
				Usage: `Version constraint for the aicr CLI version in the attestation predicate.
	Supports operators: >=, >, <=, <, ==, !=.
	A bare version (e.g. "0.8.0") is treated as ">= 0.8.0".`,
			},
			&cli.StringFlag{
				Name: "certificate-identity-regexp",
				Usage: `Override the certificate identity pattern for binary attestation verification.
	Must begin with "https://github.com/NVIDIA/aicr/" (a leading "^" is allowed) and
	must not use top-level alternation. Default pins to the release workflow on tag refs.`,
			},
			&cli.StringFlag{
				Name:  "key",
				Usage: "Verify a key-signed bundle attestation against a KMS key URI (awskms:// | gcpkms:// | azurekms:// | hashivault://) or a local PEM public-key file. The counterpart to `bundle --signing-key`. Coexists with --certificate-identity-regexp (which pins the separate binary attestation).",
			},
			&cli.StringFlag{
				Name:  "trust-root",
				Usage: "Verify the bundle attestation against a private Sigstore trusted root (a trusted_root.json from a self-hosted Fulcio/Rekor). Additive to AICR's built-in public-good root, so NVIDIA-signed and privately-signed bundles both verify. Composes with --key and --certificate-identity-regexp. The verify counterpart to `bundle --fulcio-url`/`--rekor-url`.",
			},
			&cli.BoolFlag{
				Name:  "insecure-ignore-tlog",
				Usage: "Skip transparency-log verification for an offline/air-gapped bundle signed with `bundle --signing-key ... --tlog-upload=false`. Verifies the signature against --key with no transparency-log network calls. Requires --key (the air-gapped path is key-based); a local PEM key is fully offline, while a KMS URI still makes a live GetPublicKey call to resolve the key. \"insecure\" because, with no transparency log, there is no trusted timestamp proving when the signature was made.",
			},
			withCompletions(&cli.StringFlag{
				Name:  flagFormat,
				Value: verifyFormatText,
				Usage: "Output format: text, json",
			}, func() []string { return []string{verifyFormatJSON, verifyFormatText} }),
			configFlag(),
		},
		Action: runBundleVerifyCmd,
	}
}

func runBundleVerifyCmd(ctx context.Context, cmd *cli.Command) error {
	// Bundle directory is the first positional argument
	bundleDir := cmd.Args().First()
	if bundleDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "bundle directory is required: aicr verify <bundle-dir>")
	}

	absDir, err := filepath.Abs(bundleDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to resolve bundle path", err)
	}

	format := cmd.String(flagFormat)
	if format != verifyFormatText && format != verifyFormatJSON {
		return errors.New(errors.ErrCodeInvalidRequest, "invalid --format: must be text or json")
	}

	// Load and resolve --config before any verification work so a malformed
	// spec.verify fails immediately, with spec-path attribution, rather than
	// after a full verification pass.
	cfg, err := loadFacadeConfig(ctx, cmd)
	if err != nil {
		return err
	}
	// Derive the config-supplied options through the facade, so an SDK caller
	// reading the same document gets the same values. The flag overlay below
	// stays here: only this layer knows whether a flag was explicitly set.
	resolved, err := cfg.BundleVerifyOptions()
	if err != nil {
		return err
	}

	slog.Info("verifying bundle", "dir", absDir)

	// A config-supplied floor can be *lower* than the "max" default, and
	// stringFlagOrConfig only logs when a CLI flag overrides config, so that
	// case would otherwise be silent. Surface it: the trust floor is the one
	// setting here where a committed value quietly weakening the gate is worth
	// an audit line.
	if !cmd.IsSet("min-trust-level") && resolved.MinTrustLevel != "" && resolved.MinTrustLevel != "max" {
		slog.Info("trust floor set by config", "minTrustLevel", resolved.MinTrustLevel,
			"source", "spec.verify.policy.minTrustLevel")
	}

	// Overlay the flags. Every field is CLI-flag-over-config, matching the
	// precedence the other --config-aware commands use.
	//
	// Note the derivation above validated none of this: cfg.BundleVerifyOptions
	// only projects spec.verify, and IgnoreTLog has no config field at all.
	// Client.VerifyBundle is what rejects a bad identity pattern and an
	// IgnoreTLog without a Key, before any verification work runs. The pairing
	// check is repeated below purely to word the message in flags rather than
	// struct fields.
	opts := aicr.BundleVerifyOptions{
		CertificateIdentityRegexp: stringFlagOrConfig(cmd, "certificate-identity-regexp", resolved.CertificateIdentityRegexp),
		Key:                       stringFlagOrConfig(cmd, "key", resolved.Key),
		TrustRoot:                 stringFlagOrConfig(cmd, "trust-root", resolved.TrustRoot),
		MinTrustLevel:             stringFlagOrConfig(cmd, "min-trust-level", resolved.MinTrustLevel),
		RequireCreator:            stringFlagOrConfig(cmd, "require-creator", resolved.RequireCreator),
		CLIVersionConstraint:      stringFlagOrConfig(cmd, "cli-version-constraint", resolved.CLIVersionConstraint),
		IgnoreTLog:                cmd.Bool("insecure-ignore-tlog"),
	}
	if opts.IgnoreTLog && opts.Key == "" {
		// Reworded for the CLI so the message names flags, not fields.
		return errors.New(errors.ErrCodeInvalidRequest,
			"--insecure-ignore-tlog requires --key: offline verification is key-based (verify a bundle signed with `bundle --signing-key ... --tlog-upload=false`)")
	}

	client, err := embeddedClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	verification, err := client.VerifyBundle(ctx, absDir, opts)
	if err != nil {
		// The facade already propagates coded errors from the verifier (e.g.
		// ErrCodeInvalidRequest for a bad --trust-root file, ErrCodeNotFound
		// for a missing bundle dir), so the user sees the real classification.
		return err
	}

	// Output results with final verdict. Route through cmd.Root().Writer
	// so tests can capture output via cmd.SetWriter (the convention used
	// everywhere else in pkg/cli).
	out := cmd.Root().Writer
	if format == verifyFormatJSON {
		if jsonErr := outputJSON(out, verification.Report); jsonErr != nil {
			return jsonErr
		}
	} else {
		outputText(out, verification.Report, verification.PolicyFailure)
	}

	if verification.PolicyFailure != "" {
		return errors.New(errors.ErrCodeInvalidRequest, verification.PolicyFailure)
	}

	if len(verification.Report.Errors) > 0 {
		return errors.New(errors.ErrCodeUnauthorized, "bundle verification failed: "+verification.Report.Errors[0])
	}

	return nil
}

func outputText(w io.Writer, r *aicr.BundleVerifyReport, policyFailure string) {
	if r.ChecksumsPassed {
		fmt.Fprintf(w, "  ✓ Checksums verified (%d files)\n", r.ChecksumFiles)
	} else {
		fmt.Fprintf(w, "  ✗ Checksum verification failed\n")
	}

	if r.BundleAttested {
		fmt.Fprintf(w, "  ✓ Bundle attested by: %s\n", r.BundleCreator)
	}

	if r.BinaryAttested {
		fmt.Fprintf(w, "  ✓ Binary built by: %s\n", r.BinaryBuilder)
	}

	if r.IdentityPinned {
		fmt.Fprintf(w, "  ✓ Identity pinned to NVIDIA CI\n")
	}

	fmt.Fprintf(w, "  Trust level: %s\n", r.TrustLevel)
	if r.TrustReason != "" {
		fmt.Fprintf(w, "    ↳ %s\n", r.TrustReason)
	}

	if len(r.Errors) > 0 {
		fmt.Fprintf(w, "\nDetails:\n")
		for _, e := range r.Errors {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}

	switch {
	case policyFailure != "":
		fmt.Fprintf(w, "\nBundle verification: FAILED\n")
		fmt.Fprintf(w, "  %s\n", policyFailure)
	case len(r.Errors) > 0:
		fmt.Fprintf(w, "\nBundle verification: FAILED\n")
	default:
		fmt.Fprintf(w, "\nBundle verification: PASSED\n")
	}
}

func outputJSON(w io.Writer, r *aicr.BundleVerifyReport) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal verification result", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

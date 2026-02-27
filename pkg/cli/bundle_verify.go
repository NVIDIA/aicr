// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"log/slog"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/urfave/cli/v3"
)

func bundleVerifyCmd() *cli.Command {
	return &cli.Command{
		Name:                  "verify",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Verify bundle integrity and attestation chain.",
		Description: `Verifies a bundle's checksums, attestation signatures, and provenance chain.

Trust levels:
  verified    Full chain verified: checksums, bundle attestation, binary attestation with NVIDIA CI identity
  attested    Chain verified but binary attestation missing or external data used
  unverified  Checksums valid, no attestation files (--attest was not used)
  unknown     Missing checksums or attestation files

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

Output as JSON:
  aicr verify ./my-bundle --format json
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "min-trust-level",
				Value: "max",
				Usage: `Minimum required trust level. "max" (default) auto-detects the highest
	achievable level for this bundle and verifies against it.
	Explicit levels: verified, attested, unverified, unknown`,
			},
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
	Must contain "NVIDIA/aicr". Default pins to the release workflow on tag refs.`,
			},
			&cli.StringFlag{
				Name:  "format",
				Value: "text",
				Usage: "Output format: text, json",
			},
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

	format := cmd.String("format")
	if format != "text" && format != "json" {
		return errors.New(errors.ErrCodeInvalidRequest, "invalid --format: must be text or json")
	}

	slog.Info("verifying bundle", "dir", absDir)

	// Build verify options
	verifyOpts := &verifier.VerifyOptions{}
	identityRegexp := cmd.String("certificate-identity-regexp")
	if identityRegexp != "" {
		if validErr := verifier.ValidateIdentityPattern(identityRegexp); validErr != nil {
			return validErr
		}
		verifyOpts.CertificateIdentityRegexp = identityRegexp
	}

	// Run verification
	result, err := verifier.Verify(ctx, absDir, verifyOpts)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "bundle verification failed", err)
	}

	// Check policy requirements
	policy := verifier.Policy{
		MinTrustLevel:     cmd.String("min-trust-level"),
		RequireCreator:    cmd.String("require-creator"),
		VersionConstraint: cmd.String("cli-version-constraint"),
	}
	policyFailure, policyErr := result.CheckPolicy(policy)
	if policyErr != nil {
		return policyErr
	}

	// Output results with final verdict
	if format == "json" {
		if jsonErr := outputJSON(result); jsonErr != nil {
			return jsonErr
		}
	} else {
		outputText(result, policyFailure)
	}

	if policyFailure != "" {
		return errors.New(errors.ErrCodeInvalidRequest, policyFailure)
	}

	return nil
}

func outputText(r *verifier.VerifyResult, policyFailure string) {
	if r.ChecksumsPassed {
		fmt.Printf("  ✓ Checksums verified (%d files)\n", r.ChecksumFiles)
	} else {
		fmt.Printf("  ✗ Checksum verification failed\n")
	}

	if r.BundleAttested {
		fmt.Printf("  ✓ Bundle attested by: %s\n", r.BundleCreator)
	}

	if r.BinaryAttested {
		fmt.Printf("  ✓ Binary built by: %s\n", r.BinaryBuilder)
	}

	if r.IdentityPinned {
		fmt.Printf("  ✓ Identity pinned to NVIDIA CI\n")
	}

	fmt.Printf("  Trust level: %s\n", r.TrustLevel)

	if len(r.Errors) > 0 {
		fmt.Printf("\nDetails:\n")
		for _, e := range r.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}

	if policyFailure != "" {
		fmt.Printf("\nBundle verification: FAILED\n")
		fmt.Printf("  %s\n", policyFailure)
	} else {
		fmt.Printf("\nBundle verification: PASSED\n")
	}
}

func outputJSON(r *verifier.VerifyResult) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal verification result", err)
	}
	fmt.Println(string(data))
	return nil
}

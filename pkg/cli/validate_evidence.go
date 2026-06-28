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

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// recipeEvidenceConfig groups the inputs to `aicr validate --emit-attestation`.
type recipeEvidenceConfig struct {
	OutDir      string
	BOMPath     string
	Push        string
	PlainHTTP   bool
	InsecureTLS bool

	// NoSign pushes an unsigned bundle and writes a pointer with an empty
	// signer block (requires Push). Defers Fulcio/Rekor signing; see
	// attestation.EmitOptions.NoSign. CLI-only — there is no config-file
	// equivalent, mirroring Full.
	NoSign bool

	// Full disables evidence minimization. Default (false) ships a redacted
	// snapshot and CTRF reports with stdout/message omitted; --full ships the
	// raw payloads. CLI-only — there is no config-file equivalent.
	Full bool

	// OIDC token resolution is deferred until adjacent to SignStatement
	// (see attestation.Emit): Fulcio binds the token to a fresh nonce at
	// issue, and a multi-minute validation run between resolve and sign
	// invalidates it.
	OIDCResolve bundleattest.ResolveOptions

	// AssumeYes bypasses the interactive keyless-signing identity-disclosure
	// prompt (--yes / AICR_ASSUME_YES). The banner is still emitted.
	AssumeYes bool
}

// buildRecipeEvidenceConfig parses the --emit-attestation flag family with
// CLI > config precedence. Returns nil when neither the flag nor
// spec.validate.evidence.attestation.out is set, signaling the validate
// run should not produce a recipe-evidence bundle.
func buildRecipeEvidenceConfig(cmd *cli.Command, resolved *config.ValidateResolved) *recipeEvidenceConfig {
	att := resolved.EvidenceAttestation
	if att == nil {
		att = &config.EvidenceAttestationResolved{}
	}
	out := stringFlagOrConfig(cmd, "emit-attestation", att.Out)
	if out == "" {
		return nil
	}
	return &recipeEvidenceConfig{
		OutDir:      out,
		BOMPath:     stringFlagOrConfig(cmd, "bom", att.BOM),
		Push:        stringFlagOrConfig(cmd, flagPush, att.Push),
		PlainHTTP:   boolFlagOrConfig(cmd, flagPlainHTTP, att.PlainHTTP),
		InsecureTLS: boolFlagOrConfig(cmd, flagInsecureTLS, att.InsecureTLS),
		NoSign:      cmd.Bool(flagNoSign),
		Full:        cmd.Bool(flagFull),
		OIDCResolve: oidcResolveOptionsFromFlags(cmd),
		AssumeYes:   cmd.Bool(flagAssumeYes),
	}
}

// oidcResolveOptionsFromFlags builds the keyless-signing OIDC resolution
// options from the shared --identity-token / --oidc-device-flow flag pair
// plus the GitHub Actions ambient-OIDC env vars. Shared by every command
// that signs an evidence bundle (`validate --push`, `evidence publish`) so
// the source-precedence wiring stays in one place.
func oidcResolveOptionsFromFlags(cmd *cli.Command) bundleattest.ResolveOptions {
	return bundleattest.ResolveOptions{
		IdentityToken: cmd.String(flagIdentityToken),
		AmbientURL:    os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
		AmbientToken:  os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
		DeviceFlow:    cmd.Bool(flagOIDCDeviceFlow),
		PromptWriter:  os.Stderr,
	}
}

// emitRecipeEvidence is the CLI shim over Client.EmitRecipeEvidence. The
// catalog load, facade→internal PhaseResult conversion, and attestation.Emit
// orchestration now live behind the Client facade so a non-CLI caller can emit
// recipe evidence with facade types alone. The CLI retains only the
// interactive keyless-signing disclosure (a UI concern) and the flag→options
// mapping, then delegates.
func emitRecipeEvidence(
	ctx context.Context,
	client *aicr.Client,
	rec *aicr.RecipeResult,
	snap *snapshotter.Snapshot,
	results []*aicr.PhaseResult,
	cfg *recipeEvidenceConfig,
) error {

	// Signing happens only when --push is set without --no-sign (see
	// attestation.Emit). Gate the interactive keyless login behind the
	// identity-disclosure prompt before the long push run can open a
	// browser/device-code flow. Non-interactive token sources and non-TTY runs
	// pass through inside the gate; --no-sign skips it entirely because no OIDC
	// flow runs.
	if cfg.Push != "" && !cfg.NoSign {
		if err := confirmKeylessSigningDisclosure(cfg.OIDCResolve, cfg.AssumeYes, os.Stdin, os.Stderr); err != nil {
			return err
		}
	}

	return client.EmitRecipeEvidence(ctx, rec, aicr.WrapSnapshot(snap), results, aicr.EvidenceOptions{
		OutDir:      cfg.OutDir,
		BOMPath:     cfg.BOMPath,
		Push:        cfg.Push,
		PlainHTTP:   cfg.PlainHTTP,
		InsecureTLS: cfg.InsecureTLS,
		NoSign:      cfg.NoSign,
		Full:        cfg.Full,
		Commit:      commit,
		OIDCResolve: cfg.OIDCResolve,
	})
}

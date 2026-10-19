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

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
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

	// AllowMutableValidatorTags emits despite a validator image resolving to a
	// moving tag (see attestation.EmitOptions.AllowMutableValidatorTags).
	// CLI-only, mirroring Full and NoSign: a config file is committed, and an
	// opt-out of a provenance guard should be a deliberate per-invocation act
	// rather than a setting a later run inherits silently.
	AllowMutableValidatorTags bool

	// OIDCResolve selects the signing mode (a KMS key, or keyless). OIDC
	// token resolution is deferred until adjacent to SignStatement (see
	// attestation.Emit): Fulcio binds the token to a fresh nonce at issue,
	// and a multi-minute validation run between resolve and sign
	// invalidates it.
	OIDCResolve aicr.OIDCResolveOptions

	// AssumeYes bypasses the interactive keyless-signing identity-disclosure
	// prompt (--yes / AICR_ASSUME_YES). The banner is still emitted.
	AssumeYes bool
}

// buildRecipeEvidenceConfig parses the --emit-attestation flag family with
// CLI > config precedence. att is the facade projection of
// spec.validate.evidence.attestation (Config.EvidenceAttestationOptions);
// its zero value is a valid "config configured nothing" input, matching what
// that method returns for an absent section. Returns nil when neither the
// flag nor att.OutDir is set, signaling the validate run should not produce a
// recipe-evidence bundle.
func buildRecipeEvidenceConfig(cmd *cli.Command, att aicr.EvidenceOptions) (*recipeEvidenceConfig, error) {
	out := stringFlagOrConfig(cmd, "emit-attestation", att.OutDir)
	if out == "" {
		return nil, nil //nolint:nilnil // nil means the run emits no recipe-evidence bundle
	}
	if err := validateNoSignWithoutSigningFlags(cmd); err != nil {
		return nil, err
	}
	signing, err := evidenceSigningOptionsFromFlags(cmd)
	if err != nil {
		return nil, err
	}
	return &recipeEvidenceConfig{
		OutDir:      out,
		BOMPath:     stringFlagOrConfig(cmd, "bom", att.BOMPath),
		Push:        stringFlagOrConfig(cmd, flagPush, att.Push),
		PlainHTTP:   boolFlagOrConfig(cmd, flagPlainHTTP, att.PlainHTTP),
		InsecureTLS: boolFlagOrConfig(cmd, flagInsecureTLS, att.InsecureTLS),
		NoSign:      cmd.Bool(flagNoSign),
		Full:        cmd.Bool(flagFull),

		AllowMutableValidatorTags: cmd.Bool(flagAllowMutableValidatorTags),

		OIDCResolve: signing,
		AssumeYes:   cmd.Bool(flagAssumeYes),
	}, nil
}

// evidenceConfigForRunMode suppresses CONFIG-DRIVEN recipe-evidence emission in
// --no-cluster mode. (An explicit --emit-attestation/--push alongside
// --no-cluster is rejected upstream with ErrCodeInvalidRequest — an explicit CLI
// flag must not be warn-and-ignored; this path handles only
// spec.validate.evidence.attestation resolved from config.) An offline dry-run
// reports every check as "skipped", so an emitted attestation would attest to
// nothing. Worse, when the config sets attestation.{out,push} (as the UAT test
// configs do), a dry-run would sign and push a bundle to the same OCI repo the
// authoritative live-cluster validate later pushes to — leaving two
// independently-signed bundles whose digests differ, which breaks `aicr evidence
// verify`: the signed subject no longer matches the pulled run-tagged artifact.
// A dry-run must never emit signed evidence, so drop the config and log once.
// Returns cfg unchanged for a live-cluster run (including cfg == nil).
func evidenceConfigForRunMode(noCluster bool, cfg *recipeEvidenceConfig) *recipeEvidenceConfig {
	if noCluster && cfg != nil {
		slog.Warn("skipping evidence emission: --no-cluster is an offline dry-run and must not sign or push an attestation",
			"emitAttestation", cfg.OutDir, "push", cfg.Push)
		return nil
	}
	return cfg
}

// emitRecipeEvidence is the CLI shim over Client.EmitRecipeEvidenceWithResult,
// returning what was pushed so runValidation can write --image-refs. The
// catalog load, facade→internal PhaseResult conversion, and attestation.Emit
// orchestration now live behind the Client facade so a non-CLI caller can emit
// recipe evidence with facade types alone. The CLI retains only the
// interactive keyless-signing disclosure (a UI concern) and the flag→options
// mapping, then delegates.
func emitRecipeEvidence(
	ctx context.Context,
	client *aicr.Client,
	rec *aicr.RecipeResult,
	snap *aicr.Snapshot,
	results []*aicr.PhaseResult,
	cfg *recipeEvidenceConfig,
) (*aicr.EvidenceEmission, error) {

	// Signing happens only when --push is set without --no-sign (see
	// attestation.Emit). Gate the interactive keyless login behind the
	// identity-disclosure prompt before the long push run can open a
	// browser/device-code flow. Non-interactive token sources and non-TTY runs
	// pass through inside the gate; --no-sign skips it entirely because no OIDC
	// flow runs.
	if cfg.Push != "" && !cfg.NoSign {
		if err := confirmKeylessSigningDisclosure(cfg.OIDCResolve, cfg.AssumeYes, os.Stdin, os.Stderr); err != nil {
			return nil, err
		}
	}

	return client.EmitRecipeEvidenceWithResult(ctx, rec, snap, results, aicr.EvidenceOptions{
		OutDir:      cfg.OutDir,
		BOMPath:     cfg.BOMPath,
		Push:        cfg.Push,
		PlainHTTP:   cfg.PlainHTTP,
		InsecureTLS: cfg.InsecureTLS,
		NoSign:      cfg.NoSign,
		Full:        cfg.Full,
		Commit:      commit,

		AllowMutableValidatorTags: cfg.AllowMutableValidatorTags,

		OIDCResolve: cfg.OIDCResolve,
	})
}

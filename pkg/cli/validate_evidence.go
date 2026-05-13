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
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

// recipeEvidenceConfig groups the inputs to `aicr validate --emit-attestation`.
type recipeEvidenceConfig struct {
	OutDir      string
	BOMPath     string
	Push        string
	PlainHTTP   bool
	InsecureTLS bool

	// OIDC token resolution is deferred until adjacent to SignStatement
	// (see attestation.Emit): Fulcio binds the token to a fresh nonce at
	// issue, and a multi-minute validation run between resolve and sign
	// invalidates it.
	OIDCResolve bundleattest.ResolveOptions
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
		Push:        stringFlagOrConfig(cmd, "push", att.Push),
		PlainHTTP:   boolFlagOrConfig(cmd, "plain-http", att.PlainHTTP),
		InsecureTLS: boolFlagOrConfig(cmd, "insecure-tls", att.InsecureTLS),
		OIDCResolve: bundleattest.ResolveOptions{
			IdentityToken: cmd.String("identity-token"),
			AmbientURL:    os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
			AmbientToken:  os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
			DeviceFlow:    cmd.Bool("oidc-device-flow"),
			PromptWriter:  os.Stderr,
		},
	}
}

// emitRecipeEvidence is the CLI shim over attestation.Emit. It loads the
// validator catalog (a CLI-side concern keyed by the binary's build-time
// version), builds an EmitOptions value from the parsed flag config, and
// delegates the orchestration to the evidence package so the same code
// path is reachable from the future API surface.
func emitRecipeEvidence(
	ctx context.Context,
	rec *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
	results []*validator.PhaseResult,
	cfg *recipeEvidenceConfig,
) error {

	cat, err := catalog.Load(version, commit)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog for evidence", err)
	}

	_, err = attestation.Emit(ctx, attestation.EmitOptions{
		OutDir:       cfg.OutDir,
		BOMPath:      cfg.BOMPath,
		Push:         cfg.Push,
		PlainHTTP:    cfg.PlainHTTP,
		InsecureTLS:  cfg.InsecureTLS,
		Recipe:       rec,
		Snapshot:     snap,
		PhaseResults: results,
		Catalog:      cat,
		AICRVersion:  version,
		OIDCResolve:  cfg.OIDCResolve,
	})
	return err
}

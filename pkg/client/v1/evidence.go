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

package aicr

import (
	"context"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	evattest "github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// OIDCResolveOptions configures keyless-signing OIDC token resolution for a
// pushed evidence bundle. Transparent alias of
// pkg/bundler/attestation.ResolveOptions, mirroring how BundleOptions exposes
// BundleAttester: the caller (CLI/server) builds the resolution inputs and the
// facade threads them through to attestation.Emit, which resolves the token
// adjacent to signing. The zero value is valid (no token sources → ambient or
// interactive flows handled by the caller before invoking the facade).
type OIDCResolveOptions = bundleattest.ResolveOptions

// EvidenceOptions configures Client.EmitRecipeEvidence. It is the facade-owned
// mirror of the inputs the CLI used to assemble inline, minus the interactive
// signing-disclosure prompt, which is a UI concern the caller owns.
type EvidenceOptions struct {
	// OutDir is the directory to write the recipe-evidence v1 bundle to
	// (summary-bundle/, optionally logs-bundle/, and pointer.yaml). Required.
	OutDir string

	// BOMPath optionally embeds a CycloneDX BOM; when empty a recipe-bound
	// BOM is synthesized from the recipe's component refs and the validator
	// catalog images that ran.
	BOMPath string

	// Push, when set, is the OCI reference to push the (optionally signed)
	// summary bundle to.
	Push string

	// PlainHTTP / InsecureTLS control the OCI transport for Push (local /
	// self-signed registries).
	PlainHTTP   bool
	InsecureTLS bool

	// NoSign pushes an unsigned bundle and writes a pointer with an empty
	// signer block (requires Push); defers Fulcio/Rekor signing.
	NoSign bool

	// Full disables evidence minimization (ships the raw snapshot and CTRF
	// payloads instead of the redacted defaults).
	Full bool

	// Commit is the build commit used to resolve the validator catalog for
	// the bundle's BOM. The Client's version is used for the catalog version
	// and stamped as AICRVersion; commit has no Client-level home, so it is
	// supplied per call.
	Commit string

	// OIDCResolve carries keyless-signing token-resolution inputs, consumed
	// only when Push is set and NoSign is false.
	OIDCResolve OIDCResolveOptions
}

// MergeReports merges the per-phase CTRF reports from a ValidateState run into
// a single combined report, stamped with the tool name "aicr" and this
// Client's version. Library and server callers use it to produce the same
// combined CTRF document the CLI writes, without reaching into
// pkg/validator/ctrf merge internals. Nil results and phases with a nil Report
// contribute nothing.
func (c *Client) MergeReports(results []*PhaseResult) *ctrf.Report {
	reports := make([]*ctrf.Report, 0, len(results))
	for _, pr := range results {
		if pr == nil {
			continue
		}
		reports = append(reports, pr.Report)
	}
	var version string
	if c != nil {
		version = c.version
	}
	return ctrf.MergeReports("aicr", version, reports)
}

// EmitRecipeEvidence builds (and optionally pushes) a recipe-evidence v1
// attestation bundle from a completed validation run. It is the facade
// counterpart to the logic the CLI previously assembled inline: it converts
// the facade PhaseResults back to the internal shape, loads the validator
// catalog against THIS Client's data source and version, and delegates to the
// evidence attestation package.
//
// Interactive keyless-signing disclosure is intentionally NOT performed here —
// that is a UI concern the caller handles (the CLI prompts before calling).
// This method does no prompting and can run unattended from a server/library.
func (c *Client) EmitRecipeEvidence(
	ctx context.Context,
	rec *RecipeResult,
	snap *Snapshot,
	results []*PhaseResult,
	opts EvidenceOptions,
) error {

	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if rec == nil || rec.internal == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"nil or unresolved RecipeResult — call Client.ResolveRecipe to obtain an evidence-emittable RecipeResult")
	}
	if err := c.assertOwns(rec); err != nil {
		return err
	}
	if snap == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "nil Snapshot")
	}
	if opts.OutDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "evidence OutDir is required")
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	// Snapshot the per-Client provider + version so the validator catalog
	// resolves against THIS Client's recipe source, matching the run.
	dp := c.dp
	clientVersion := c.version
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	cat, err := catalog.LoadWithDataProvider(ctx, dp, clientVersion, opts.Commit)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load validator catalog for evidence")
	}

	_, err = evattest.Emit(ctx, evattest.EmitOptions{
		OutDir:       opts.OutDir,
		BOMPath:      opts.BOMPath,
		Push:         opts.Push,
		PlainHTTP:    opts.PlainHTTP,
		InsecureTLS:  opts.InsecureTLS,
		NoSign:       opts.NoSign,
		Full:         opts.Full,
		Recipe:       rec.Resolved(),
		Snapshot:     toInternalSnapshot(snap),
		PhaseResults: toInternalPhaseResults(results),
		Catalog:      cat,
		AICRVersion:  clientVersion,
		OIDCResolve:  opts.OIDCResolve,
	})
	return err
}

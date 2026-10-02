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
	"fmt"
	"log/slog"
	"slices"
	"strings"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	evattest "github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// OIDCResolveOptions configures keyless-signing OIDC token resolution for a
// pushed evidence bundle. Deliberate transparent alias of
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
	// OutDir is the directory to write the recipe-evidence bundle to
	// (summary-bundle/ and pointer.yaml). Required.
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

	// AllowMutableValidatorTags emits even when a validator image resolves to
	// a moving tag (`:edge`, `:latest`, anything not a release, `:sha-<commit>`
	// or `:uat-<run_id>` ref). Emission otherwise fails closed, because the
	// predicate identifies validators by tag alone. See #2873.
	AllowMutableValidatorTags bool

	// Commit is the build commit. It resolves the validator catalog for the
	// bundle's BOM and is stamped into the predicate as AICRCommit. The
	// Client has no commit of its own, so it is supplied per call.
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

// EmitRecipeEvidence builds (and optionally pushes) a recipe-evidence
// attestation bundle from a completed validation run — predicateType v1 for
// unprofiled recipes, v2 when the recipe carries a configuration profile
// (metadata.selectedProfile). It is the facade
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
		OutDir:      opts.OutDir,
		BOMPath:     opts.BOMPath,
		Push:        opts.Push,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
		NoSign:      opts.NoSign,
		Full:        opts.Full,
		Recipe:      rec.Resolved(),

		AllowMutableValidatorTags: opts.AllowMutableValidatorTags,

		Snapshot:     toInternalSnapshot(snap),
		PhaseResults: toInternalPhaseResults(results),
		Catalog:      cat,
		AICRVersion:  clientVersion,
		AICRCommit:   opts.Commit,
		OIDCResolve:  opts.OIDCResolve,
	})
	return err
}

// CNCFCollectOptions configures Client.CollectCNCFEvidence.
type CNCFCollectOptions struct {
	// Dir is the directory the behavioral evidence is written to. Required.
	Dir string

	// Features restricts collection to the named features or their aliases
	// (see CNCFEvidenceFeatures). Empty, or "all", collects every feature.
	Features []string

	// Kubeconfig is the kubeconfig path the collector's kubectl calls use.
	// Empty uses kubectl's own resolution (KUBECONFIG, then ~/.kube/config).
	Kubeconfig string

	// NoCluster runs in test mode. Every section is reported as skipped and
	// nothing is executed against a cluster.
	NoCluster bool
}

// CNCFEvidenceFeatures returns the canonical feature names
// CNCFCollectOptions.Features accepts, in collection order. Short aliases
// and "all" are accepted too but not listed. The returned slice is a copy.
func CNCFEvidenceFeatures() []string {
	return slices.Clone(cncf.ValidFeatures)
}

// RenderCNCFEvidence writes CNCF AI Conformance evidence markdown (one file
// per submission requirement, plus index.md) to dir from a CTRF report,
// typically the one MergeReports returns. Skipped checks are omitted, and a
// report with no submission-required checks writes nothing and returns nil.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client or ctx is nil, or dir is empty.
//   - ErrCodeTimeout when rendering exceeds defaults.EvidenceRenderTimeout.
//   - ErrCodeInternal when a file cannot be written.
func (c *Client) RenderCNCFEvidence(ctx context.Context, report *ctrf.Report, dir string) error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if dir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "CNCF evidence directory is required")
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.EvidenceRenderTimeout)
	defer cancel()

	if err := cncf.New(cncf.WithOutputDir(dir)).Render(ctx, report); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "CNCF evidence rendering failed")
	}
	return nil
}

// CollectCNCFEvidence collects behavioral CNCF AI Conformance submission
// evidence by deploying GPU test workloads to the cluster and capturing
// their results under opts.Dir. It runs for up to
// defaults.CNCFSubmissionTimeout.
//
// rec is optional. When non-nil, it must come from this Client, and the GPU
// allocation policy it configures selects the mechanism the dra-support and
// secure-access sections exercise. A policy that cannot be resolved fails
// the call rather than collecting evidence for the wrong mechanism. When
// nil, the collector detects the mechanism from cluster capabilities.
//
// A real run needs bash and kubectl on PATH.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client or ctx is nil, the Client is
//     closed, opts.Dir is empty, a feature name is unknown, or rec is
//     unresolved or owned by another Client.
//   - ErrCodeUnavailable when bash or kubectl is not on PATH.
//   - ErrCodeTimeout when collection exceeds its deadline.
//   - The policy resolver's own code when rec's allocation policy is invalid.
//   - ErrCodeInternal when one or more sections fail.
func (c *Client) CollectCNCFEvidence(ctx context.Context, rec *RecipeResult, opts CNCFCollectOptions) error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if opts.Dir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "CNCF evidence directory is required")
	}
	for _, f := range opts.Features {
		if !cncf.IsValidFeature(f) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown feature %q; valid features: %s",
					f, strings.Join(cncf.ValidFeatures, ", ")))
		}
	}
	if rec != nil {
		if rec.internal == nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				"RecipeResult has no internal recipe state: call Client.ResolveRecipe or Client.LoadRecipe to obtain one")
		}
		if err := c.assertOwns(rec); err != nil {
			return err
		}
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := context.WithTimeout(ctx, defaults.CNCFSubmissionTimeout)
	defer cancel()

	var policy string
	if rec != nil {
		var err error
		if policy, err = validatorv1.ResolveGPUAllocationPolicy(ctx, rec.internal); err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to resolve the GPU allocation policy for CNCF evidence collection")
		}
	}

	slog.Info("starting CNCF submission evidence collection",
		"evidenceDir", opts.Dir, "features", opts.Features, "gpuAllocationPolicy", policy)

	collectorOpts := []cncf.CollectorOption{
		cncf.WithFeatures(opts.Features),
		cncf.WithKubeconfig(opts.Kubeconfig),
		cncf.WithNoCluster(opts.NoCluster),
	}
	if policy != "" {
		collectorOpts = append(collectorOpts, cncf.WithAllocationPolicy(policy))
	}
	if err := cncf.NewCollector(opts.Dir, collectorOpts...).Run(ctx); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "CNCF evidence collection failed")
	}
	return nil
}

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
	"path/filepath"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	evattest "github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// OIDCResolveOptions selects how a pushed evidence bundle is signed: with the
// KMS key SigningKey names (DisableTLogUpload skips the Rekor entry), or
// keylessly with deferred OIDC token resolution. Deliberate transparent alias of
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

	// OIDCResolve selects KMS signing (SigningKey) or keyless signing and
	// carries its inputs, consumed only when Push is set and NoSign is false.
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

// EvidenceEmission reports what Client.EmitRecipeEvidenceWithResult pushed.
// Both fields are empty when EvidenceOptions.Push was unset.
type EvidenceEmission struct {
	// PushedReference is the registry/repository:tag the summary bundle was
	// pushed to: pointer.yaml's bundle.oci.
	PushedReference string

	// PushedDigest is the pushed manifest's digest (sha256:...):
	// pointer.yaml's bundle.digest.
	PushedDigest string
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

	_, err := c.EmitRecipeEvidenceWithResult(ctx, rec, snap, results, opts)
	return err
}

// EmitRecipeEvidenceWithResult is EmitRecipeEvidence, also reporting the
// reference and digest of the bundle it pushed, so a caller can hand them on
// without reading pointer.yaml back from OutDir. The result is non-nil
// whenever the error is nil.
func (c *Client) EmitRecipeEvidenceWithResult(
	ctx context.Context,
	rec *RecipeResult,
	snap *Snapshot,
	results []*PhaseResult,
	opts EvidenceOptions,
) (*EvidenceEmission, error) {

	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if rec == nil || rec.internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"nil or unresolved RecipeResult — call Client.ResolveRecipe to obtain an evidence-emittable RecipeResult")
	}
	if err := c.assertOwns(rec); err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil Snapshot")
	}
	if opts.OutDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "evidence OutDir is required")
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
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
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load validator catalog for evidence")
	}

	res, err := evattest.Emit(ctx, evattest.EmitOptions{
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
	if err != nil {
		return nil, err
	}
	emission := &EvidenceEmission{}
	if res != nil && res.PushSummary != nil {
		emission.PushedReference = oci.TrimScheme(res.PushSummary.Reference)
		emission.PushedDigest = res.PushSummary.Digest
	}
	return emission, nil
}

// CNCFCollectOptions configures Client.CollectCNCFEvidence.
type CNCFCollectOptions struct {
	// Dir is the directory the submission evidence is written to. Required.
	// A relative path, like a relative Kubeconfig, resolves against the
	// caller's working directory.
	Dir string

	// Features must be empty; a non-empty value is rejected with
	// ErrCodeInvalidRequest.
	//
	// Deprecated: the --feature interface was removed with the evidence
	// collector script (#3602). The conformance checks the recipe declares
	// are the only selection, and every one of them runs.
	Features []string

	// Kubeconfig is the kubeconfig path the snapshot agent and the validator
	// Jobs use. Empty uses the default discovery (KUBECONFIG, then
	// ~/.kube/config).
	Kubeconfig string

	// Namespace is where the snapshot agent and the validator Jobs run, as
	// `aicr validate --namespace`. Empty means defaults.ValidationNamespace.
	Namespace string

	// NotApplicable declares conditional MUST requirements not applicable,
	// keyed by upstream id with the justification as value, exactly as
	// RenderCNCFEvidenceOptions.NotApplicable and the CLI's
	// spec.validate.evidence.cncf.notApplicable (today only
	// cluster_autoscaling). It is checked before the cluster is touched.
	NotApplicable map[string]string

	// NoCluster must be false; true is rejected with ErrCodeInvalidRequest.
	//
	// Deprecated: submission evidence comes from checks that ran on a live
	// cluster, so there is no offline mode. For an offline dry-run call
	// ValidateState with WithValidationNoCluster(true) directly.
	NoCluster bool
}

// CNCFEvidenceFeatures returns an empty, non-nil slice: no feature names are
// accepted since the --feature interface was removed (#3602). The returned
// slice is never shared.
//
// Deprecated: CNCFCollectOptions.Features is rejected when non-empty; there
// is nothing to list.
func CNCFEvidenceFeatures() []string {
	return []string{}
}

// RenderCNCFEvidenceOptions selects the upstream checklist version
// RenderCNCFEvidenceWithOptions renders against. The zero value renders
// against cncf.LatestVersion.
type RenderCNCFEvidenceOptions struct {
	// ConformanceVersion names the checklist explicitly ("v1.36" or "1.36").
	// A version this build does not carry (see cncf.SupportedVersions) is
	// refused with ErrCodeInvalidRequest. Takes precedence over
	// KubernetesVersion.
	ConformanceVersion string

	// KubernetesVersion is the validated cluster's server version
	// ("v1.36.3"), typically Snapshot.KubernetesVersion. Its minor selects
	// the checklist. A minor this build carries no checklist for is rendered
	// against the nearest carried one with a warning rather than failing the
	// render: below the oldest carried checklist (v1.35) that oldest one,
	// above the newest the latest. Upstream publishes checklists this build
	// does not carry (v1.33, v1.34), so the fallback is "not carried", not
	// "not published". An unparseable value also falls back to the latest.
	KubernetesVersion string

	// NotApplicable declares conditional MUST requirements not applicable,
	// keyed by upstream id with the justification as value: the config key
	// spec.validate.evidence.cncf.notApplicable (CNCFEvidenceOptions). It is
	// the only source of a not-applicable status; only a conditional MUST
	// (today cluster_autoscaling) is accepted, and the declaration takes
	// precedence over a check that ran, whose result is kept as an
	// observation. Any other id is refused with ErrCodeInvalidRequest.
	NotApplicable map[string]string
}

// CNCFRequirementAggregate is the requirement status aggregation
// RenderCNCFEvidenceWithResult returns. It is a transparent alias of
// cncf.Aggregate, so its methods (Passed, IncompleteMusts, Requirement) are
// the facade's own declared return type: the exit-code rule a submission run
// derives from it must not be re-derived by callers.
type CNCFRequirementAggregate = cncf.Aggregate

// ValidateCNCFEvidenceOptions checks RenderCNCFEvidenceOptions the way
// RenderCNCFEvidenceWithResult will, without rendering: the checklist version
// resolves (an unsupported ConformanceVersion is refused) and every
// NotApplicable id is a conditional MUST of that version with a non-blank
// justification. It exists so a caller can refuse a bad notApplicable key
// before the validation run, rather than after every check has run.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client is nil, opts.ConformanceVersion
//     is unsupported, or opts.NotApplicable names a requirement that may not
//     be declared not applicable or lacks a justification.
func (c *Client) ValidateCNCFEvidenceOptions(opts RenderCNCFEvidenceOptions) error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	version, err := resolveConformanceVersion(opts)
	if err != nil {
		return err
	}
	return cncf.ValidateNotApplicable(version, opts.NotApplicable)
}

// RenderCNCFEvidence writes CNCF AI Conformance evidence markdown (one file
// per submission requirement, plus index.md) to dir from a CTRF report,
// typically the one MergeReports returns, against the latest checklist
// version this build carries. It is RenderCNCFEvidenceWithOptions with the
// zero options; see there for the contract.
func (c *Client) RenderCNCFEvidence(ctx context.Context, report *ctrf.Report, dir string) error {
	return c.RenderCNCFEvidenceWithOptions(ctx, report, dir, RenderCNCFEvidenceOptions{})
}

// RenderCNCFEvidenceWithOptions is RenderCNCFEvidenceWithResult without the
// aggregate; see there for the contract.
func (c *Client) RenderCNCFEvidenceWithOptions(ctx context.Context, report *ctrf.Report, dir string, opts RenderCNCFEvidenceOptions) error {
	_, err := c.RenderCNCFEvidenceWithResult(ctx, report, dir, opts)
	return err
}

// RenderCNCFEvidenceWithResult writes CNCF AI Conformance evidence markdown
// (one file per requirement a non-skipped check evidenced, plus index.md
// listing every requirement of the selected checklist version) to dir from a
// CTRF report, typically the one MergeReports returns, and returns the
// requirement status aggregation the pages and index were rendered from.
//
// The verdict rules (proof paths, levels, the effect of opts.NotApplicable)
// are cncf.AggregateReport's. A report with no submission-required result
// writes nothing, and the aggregate then reports every requirement not
// tested except those opts.NotApplicable declares. A check whose requirement
// the selected version does not list (dra_support from v1.36 on) renders as
// supporting evidence. A report from a run that used
// WithValidationSkipChecks is refused: a run with withheld checks is narrower
// than the recipe by construction, so its evidence cannot stand for the
// recipe.
//
// The aggregate is non-nil whenever the error is nil.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client or ctx is nil, dir is empty,
//     opts.ConformanceVersion is unsupported, opts.NotApplicable names a
//     requirement that may not be declared not applicable, or the report
//     contains a check withheld by WithValidationSkipChecks.
//   - ErrCodeTimeout when rendering exceeds defaults.EvidenceRenderTimeout.
//   - ErrCodeInternal when a file cannot be written.
func (c *Client) RenderCNCFEvidenceWithResult(ctx context.Context, report *ctrf.Report, dir string, opts RenderCNCFEvidenceOptions) (*CNCFRequirementAggregate, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if dir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "CNCF evidence directory is required")
	}
	if report != nil {
		for _, t := range report.Results.Tests {
			if t.Status == ctrf.StatusSkipped && t.Extra["skipReason"] == validator.SkipCheckReasonCode {
				return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"check %q was withheld by WithValidationSkipChecks: a run with withheld checks is narrower than "+
						"the recipe by construction, so its CNCF evidence cannot stand for the recipe", t.Name))
			}
		}
	}

	version, err := resolveConformanceVersion(opts)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.EvidenceRenderTimeout)
	defer cancel()

	r := cncf.New(cncf.WithOutputDir(dir), cncf.WithConformanceVersion(version), cncf.WithNotApplicable(opts.NotApplicable))
	agg, err := r.RenderWithResult(ctx, report)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "CNCF evidence rendering failed")
	}
	return agg, nil
}

// resolveConformanceVersion applies RenderCNCFEvidenceOptions' precedence:
// an explicit version must be supported; a Kubernetes version selects the
// checklist for its minor, or the nearest carried one when this build has
// none for it; nothing selects the latest.
func resolveConformanceVersion(opts RenderCNCFEvidenceOptions) (cncf.Version, error) {
	if opts.ConformanceVersion != "" {
		v, err := cncf.ParseVersion(opts.ConformanceVersion)
		if err != nil {
			return "", err
		}
		return v, nil
	}
	if opts.KubernetesVersion == "" {
		return cncf.LatestVersion, nil
	}
	v, exact, err := cncf.NearestVersion(opts.KubernetesVersion)
	if err != nil {
		slog.Warn("cluster Kubernetes version is not parseable; rendering CNCF AI Conformance evidence against the latest carried checklist",
			"kubernetesVersion", opts.KubernetesVersion, "using", cncf.LatestVersion, "error", err)
		return cncf.LatestVersion, nil
	}
	if !exact {
		slog.Warn("this build carries no CNCF AI Conformance checklist for the cluster's Kubernetes minor; rendering against the nearest carried one",
			"kubernetesVersion", opts.KubernetesVersion, "using", v, "carried", cncf.SupportedVersions())
	}
	return v, nil
}

// CollectCNCFEvidence runs the conformance phase against a live cluster and
// renders the CNCF AI Conformance evidence into opts.Dir. It is the Go
// counterpart of `aicr validate --phase conformance --cncf-submission
// --evidence-dir`, composed from the public surface: CollectSnapshot deploys
// the snapshot agent, ValidateState runs the conformance checks rec declares,
// MergeReports combines them, and RenderCNCFEvidenceWithResult writes the
// evidence against the checklist of the cluster's Kubernetes minor. Flags
// never change what a check does: the checks and their verdicts are those
// ValidateState produces. Each stage bounds itself the way it does for every
// caller: the snapshot by defaults.SnapshotOperationTimeout plus its grace,
// the run by defaults.ValidationOperationTimeout; a tighter caller deadline
// always wins.
//
// rec is required and must come from this Client: the recipe selects the
// conformance checks, and the former capability-detection path of the bash
// collector is gone. A caller that needs the snapshot agent configured
// (image, scheduling, service account) or the validator options should call
// the four methods above itself; this wrapper uses their defaults with
// opts.Kubeconfig and opts.Namespace.
//
// The call fails after rendering under the rule the CLI applies
// (CNCFRequirementAggregate.Outcome): a check that failed or errored fails
// it, as in any run, and so does an applicable MUST requirement that is not
// established. The aggregation itself never fails it on a SHOULD. Unlike `aicr validate`, which stamps one run id on
// the agent Job and the validator Jobs (ADR-020), the agent Job here carries
// its own generated id, so the two are not correlated by label.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client or ctx is nil, the Client is
//     closed, opts.Dir is empty, opts.Features is non-empty, opts.NoCluster
//     is true, opts.NotApplicable names a requirement that may not be
//     declared not applicable, or rec is nil, unresolved or owned by another
//     Client.
//   - The snapshot, validator and render errors propagate with their own
//     codes (ErrCodeTimeout when a stage's deadline expires).
//   - ErrCodeInternal when a check failed or errored, or an applicable MUST
//     requirement is not established.
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
	if len(opts.Features) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("CNCFCollectOptions.Features %v: the --feature interface was removed with the evidence collector (#3602); "+
				"the recipe's conformance checks are the only selection", opts.Features))
	}
	if opts.NoCluster {
		return errors.New(errors.ErrCodeInvalidRequest,
			"CNCFCollectOptions.NoCluster: CNCF submission evidence needs a live cluster; "+
				"for an offline dry-run call ValidateState with WithValidationNoCluster(true)")
	}
	if rec == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"recipe is required: the conformance checks a submission runs are selected from it")
	}
	if rec.internal == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"RecipeResult has no internal recipe state: call Client.ResolveRecipe or Client.LoadRecipe to obtain one")
	}
	if err := c.assertOwns(rec); err != nil {
		return err
	}
	// Pre-flight against the latest carried checklist, as the CLI does, so a
	// bad key fails here rather than after every validator Job; the render
	// repeats the check against the selected version.
	if err := c.ValidateCNCFEvidenceOptions(RenderCNCFEvidenceOptions{NotApplicable: opts.NotApplicable}); err != nil {
		return err
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve CNCF evidence directory", err)
	}
	kubeconfig := opts.Kubeconfig
	if kubeconfig != "" {
		if kubeconfig, err = filepath.Abs(kubeconfig); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve kubeconfig path", err)
		}
	}

	namespace := opts.Namespace
	if namespace == "" {
		namespace = defaults.ValidationNamespace
	}
	slog.Info("starting CNCF submission run", "evidenceDir", dir, "recipe", rec.Name, "namespace", namespace)
	return runCNCFSubmission(ctx, c, rec, cncfSubmissionInputs{
		dir:           dir,
		kubeconfig:    kubeconfig,
		namespace:     namespace,
		notApplicable: opts.NotApplicable,
	})
}

// cncfSubmissionInputs are CollectCNCFEvidence's resolved inputs: absolute
// paths and the defaulted namespace.
type cncfSubmissionInputs struct {
	dir           string
	kubeconfig    string
	namespace     string
	notApplicable map[string]string
}

// cncfSubmissionRunner is the slice of Client that runCNCFSubmission
// composes. *Client satisfies it; tests substitute a fake so the composition
// and its error propagation are exercised without a cluster.
type cncfSubmissionRunner interface {
	CollectSnapshot(ctx context.Context, cfg *AgentConfig) (*Snapshot, error)
	ValidateState(ctx context.Context, recipe *RecipeResult, snap *Snapshot, opts ...ValidateOption) ([]*PhaseResult, error)
	MergeReports(results []*PhaseResult) *ctrf.Report
	RenderCNCFEvidenceWithResult(ctx context.Context, report *ctrf.Report, dir string, opts RenderCNCFEvidenceOptions) (*CNCFRequirementAggregate, error)
}

var _ cncfSubmissionRunner = (*Client)(nil)

// runCNCFSubmission is the submission run: snapshot, conformance phase,
// merge, render, verdict. Every stage's error propagates as that stage
// reports it.
func runCNCFSubmission(ctx context.Context, r cncfSubmissionRunner, rec *RecipeResult, in cncfSubmissionInputs) error {
	// Privileged as `aicr validate` deploys it: the GPU, SystemD and OS
	// collectors the conformance snapshot needs do not run restricted.
	snap, err := r.CollectSnapshot(ctx, &AgentConfig{
		Kubeconfig: in.kubeconfig,
		Namespace:  in.namespace,
		Cleanup:    true,
		Privileged: true,
	})
	if err != nil {
		return err
	}
	results, err := r.ValidateState(ctx, rec, snap,
		WithValidationKubeconfig(in.kubeconfig),
		WithValidationNamespace(in.namespace),
		WithValidationPhases(PhaseConformance))
	if err != nil {
		return err
	}
	report := r.MergeReports(results)
	agg, err := r.RenderCNCFEvidenceWithResult(ctx, report, in.dir, RenderCNCFEvidenceOptions{
		KubernetesVersion: snap.KubernetesVersion(),
		NotApplicable:     in.notApplicable,
	})
	if err != nil {
		return err
	}
	// The same rule as `aicr validate --cncf-submission` with --fail-on-error
	// (the default): a failed or errored check fails the run, and the
	// requirement verdict is added on top.
	return agg.Outcome(report, true)
}

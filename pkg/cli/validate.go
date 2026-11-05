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
	stderrors "errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// validateNameBase is the prefix applied to the live-capture snapshot
// agent's Job/ServiceAccount/RBAC names when --job-name / --service-account-name
// are left unset, distinguishing `aicr validate`'s agent resources from
// `aicr snapshot`'s (which use the package-level "aicr" base). RunID is
// always appended, so the deployed names are run-scoped regardless of
// whether this base is used.
//
// It doubles as --job-name's declared Value: default so the released v1
// help output is unchanged. That default is never read as a value — see
// explicitStringFlagOrConfig — which is why the constant, not the flag,
// is what actually supplies the prefix.
const validateNameBase = "aicr-validate"

// validateAgentConfig holds parsed agent configuration for validate command.
type validateAgentConfig struct {
	kubeconfig         string
	namespace          string
	image              string
	imagePullSecrets   []string
	jobName            string
	serviceAccountName string
	nodeSelector       map[string]string
	tolerations        []corev1.Toleration
	timeout            time.Duration
	cleanup            bool
	debug              bool
	requireGPU         bool
	aksGPUPoolsPath    string
	gkeGPUPoolsPath    string

	// runID correlates this run's live-capture snapshot agent with the
	// validator Jobs runValidation deploys for the same `aicr validate`
	// invocation — both are given the SAME id (generated once, up front,
	// by the caller of parseValidateAgentConfig) so ADR-020 run isolation
	// scopes every resource this command creates to a single run rather
	// than splitting it across two independently generated ids.
	runID         string
	okeAddonsPath string
}

// parseValidateAgentConfig builds the snapshot-capture agent's deployment
// config. Shared inputs (nodeSelector, tolerations, imagePullSecrets,
// namespace, cleanup) are resolved once by the caller and passed in; this
// keeps any CLI-overrides-config slog.Info from firing twice when both
// the agent and the downstream validator job want the same value.
//
// runID is the single id generated once for the whole `aicr validate`
// invocation; passing it in here (rather than leaving AgentConfig.RunID
// empty and letting the snapshot agent default its own) is what keeps the
// live-capture agent and the validator Jobs it feeds correlated under one
// RunID.
func parseValidateAgentConfig(
	cmd *cli.Command,
	opts aicr.ValidateSettings,
	shared validateSharedResolved,
	runID string,
) *validateAgentConfig {

	// jobName and serviceAccountName resolve through
	// explicitStringFlagOrConfig, not stringFlagOrConfig: the flags keep
	// their released defaults for --help, but only a name the operator
	// actually supplied (flag or --config) may travel downstream. An unset
	// flag stays "" so validateNameBase governs — see toAgentConfig.
	return &validateAgentConfig{
		kubeconfig:         cmd.String("kubeconfig"),
		namespace:          shared.namespace,
		image:              stringFlagOrConfig(cmd, "image", opts.Image),
		imagePullSecrets:   shared.imagePullSecrets,
		jobName:            explicitStringFlagOrConfig(cmd, "job-name", opts.JobName),
		serviceAccountName: explicitStringFlagOrConfig(cmd, "service-account-name", opts.ServiceAccountName),
		nodeSelector:       shared.nodeSelector,
		tolerations:        shared.tolerations,
		timeout:            durationFlagOrConfig(cmd, "timeout", opts.Timeout),
		cleanup:            !shared.noCleanup,
		debug:              cmd.Bool("debug"),
		requireGPU:         boolFlagOrConfig(cmd, "require-gpu", opts.RequireGPU),
		aksGPUPoolsPath:    cmd.String("aks-gpu-pools"),
		gkeGPUPoolsPath:    cmd.String("gke-gpu-pools"),
		runID:              runID,
		okeAddonsPath:      cmd.String("oke-addons"),
	}
}

// validateSharedResolved holds the validate-command fields that get
// consumed by both the snapshot-capture agent path AND the validator Job
// path. Resolving them once and threading through avoids duplicate
// CLI-overrides-config log lines that would otherwise fire from
// every helper call site.
type validateSharedResolved struct {
	namespace        string
	imagePullSecrets []string
	nodeSelector     map[string]string
	tolerations      []corev1.Toleration
	noCleanup        bool
}

// derefBoolOr returns *p when p is non-nil, otherwise fallback. Used to
// turn the *bool config-presence signal (nil = field unset) into the bool
// fallback that boolFlagOrConfig expects: when config did not set the
// field, the CLI flag's default value flows through.
func derefBoolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

// phaseStringsFallback casts opts.Phases ([]aicr.Phase, cast rather than
// re-parsed by ValidateSettings) back to []string for
// validator.ParsePhaseSelection / stringSliceFlagOrConfig, which both operate
// on the raw wire form. Returns nil when config set no phases, matching the
// zero-value fallback stringSliceFlagOrConfig expects.
func phaseStringsFallback(opts aicr.ValidateSettings) []string {
	if len(opts.Phases) == 0 {
		return nil
	}
	out := make([]string, len(opts.Phases))
	for i, p := range opts.Phases {
		out[i] = string(p)
	}
	return out
}

// validateCleanupFallback picks the cleanup-polarity default
// validateSharedResolved.noCleanup merges against --no-cleanup.
//
// opts.Cleanup is false both when spec.validate is silent about cleanup AND
// when spec.validate is absent entirely — ValidateSettings' own "zero value
// is not a safe default" godoc. present is what tells the two apart: only
// when the section is present does opts.Cleanup reflect a real
// (possibly-defaulted) config decision worth trusting as a fallback; absent
// — including the plain no-`--config` invocation, the common case — the
// CLI's own default (clean up) must apply instead, mirroring
// parseSnapshotCmdOptions' cleanupFallback for the sibling command.
//
// Extracted as its own function (rather than inlined in the Action) so a
// test can call it directly without needing a live cluster: this is the
// exact computation whose absence let a plain `aicr validate` leave the
// cluster-admin ClusterRoleBinding and validator Jobs active by default (see
// TestValidateCmd_NoConfigDefaultsToCleanup).
func validateCleanupFallback(opts aicr.ValidateSettings, present bool) bool {
	if !present {
		return true
	}
	return opts.Cleanup
}

// validateFlagCombinations rejects the validate command's mutually
// inconsistent flag pairings before any I/O runs. Split out of the Action
// closure so the guard logic can be read (and length-budgeted) independently
// of the rest of the command's orchestration.
func validateFlagCombinations(cncfSubmission bool, evidenceDir string, noCluster, explicitAttest bool, skipChecks []string) error {
	// --cncf-submission and --evidence-dir require each other (ADR-007
	// amendment, decision 2): CNCF conformance evidence is written only for a
	// submission run, and a submission run has to write it somewhere. The
	// --evidence-dir-alone render that preceded the single runner is the one
	// deprecation the collector removal left behind (#3602).
	if cncfSubmission && evidenceDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--cncf-submission requires --evidence-dir")
	}
	if evidenceDir != "" && !cncfSubmission {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--evidence-dir requires --cncf-submission: CNCF conformance evidence is written only for a submission run")
	}
	// A submission's evidence comes from checks that ran on a live cluster;
	// an offline dry-run skips every check, so there is nothing to submit.
	if cncfSubmission && noCluster {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--cncf-submission cannot be combined with --no-cluster: submission evidence comes from checks that ran on a live cluster")
	}
	// A withheld check must not reach a CNCF conformance submission. The
	// renderer (pkg/evidence/cncf) aggregates fail-closed: a skipped check
	// leaves its requirement NOT TESTED with the skip reason code recorded,
	// so the evidence would be accurate, but a run that withholds a check the
	// recipe declares is narrower than the recipe by construction and cannot
	// evidence the recipe (ADR-007 amendment, decision 4). --skip-check is for
	// lanes that cannot host a check at all (Kind, KWOK), which are not
	// submission lanes.
	if evidenceDir != "" && len(skipChecks) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--skip-check cannot be combined with --evidence-dir: a withheld check leaves its requirement "+
				"NOT TESTED, so the run is narrower than the recipe by construction and cannot produce "+
				"conformance evidence for it")
	}
	// An explicit --emit-attestation/--push is a request to sign and
	// (optionally) push an attestation; --no-cluster is an offline dry-run
	// whose checks are all skipped, so the two conflict. Reject the explicit
	// combination rather than warn-and-ignore an explicit CLI flag (repo rule).
	// A config-driven spec.validate.evidence.attestation is still silently
	// suppressed in --no-cluster mode by evidenceConfigForRunMode.
	if noCluster && explicitAttest {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--emit-attestation/--push cannot be combined with --no-cluster: an offline dry-run must not sign or push an attestation")
	}
	return nil
}

// validateSubmissionPhases refuses a submission run whose phase selection
// leaves the conformance phase out: the submission evidence is rendered from
// that phase's checks, so without it the run would write a directory in
// which every requirement reads NOT TESTED. A nil selection runs every phase.
func validateSubmissionPhases(cncfSubmission bool, phases []validator.Phase) error {
	if !cncfSubmission || phases == nil || slices.Contains(facadePhases(phases), aicr.PhaseConformance) {
		return nil
	}
	return errors.New(errors.ErrCodeInvalidRequest,
		"--cncf-submission requires the conformance phase: add --phase conformance (or --phase all)")
}

// resolveValidateNodeSelector resolves the validation node selector with
// CLI-overrides-config precedence. The CLI flag is a repeated string in
// key=value form; the config value is already a typed map. Either source
// can be empty; the result preserves the same nil-vs-empty semantics.
func resolveValidateNodeSelector(cmd *cli.Command, opts aicr.ValidateSettings) (map[string]string, error) {
	if cmd.IsSet("node-selector") {
		ns, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid node-selector", err)
		}
		if opts.NodeSelector != nil {
			slog.Info("CLI flag overriding config value", "flag", "node-selector",
				"config", opts.NodeSelector, "override", ns)
		}
		return ns, nil
	}
	return opts.NodeSelector, nil
}

// resolveValidateTolerations resolves the validation toleration list,
// preserving the "no --toleration flag" sentinel: snapshotter.ParseTolerations
// returns DefaultTolerations() (a single bare Exists entry that matches every
// taint) when its input is empty, which collapses the implicit default and an
// explicit `--toleration '*'` into the same in-memory value. Validators like
// inference-perf that want to mirror the target node's taints by default
// must distinguish "operator opted into tolerate-all" from "operator said
// nothing". Returning nil here when neither CLI nor config set the field
// keeps the env var unset, so the inner validator context sees nil. The live
// snapshot path consumes that same nil as its signal to apply the agent's
// tolerate-all default at the Job projection boundary.
func resolveValidateTolerations(cmd *cli.Command, opts aicr.ValidateSettings) ([]corev1.Toleration, error) {
	if cmd.IsSet("toleration") {
		tols, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", err)
		}
		if opts.Tolerations != nil {
			slog.Info("CLI flag overriding config value", "flag", "toleration",
				"config", opts.Tolerations, "override", tols)
		}
		return tols, nil
	}
	return opts.Tolerations, nil
}

// toAgentConfig projects the validate command's resolved flags onto the
// facade AgentConfig that Client.CollectSnapshot consumes. Privileged is
// unconditional here: the validation snapshot needs the GPU and SystemD
// collectors, which do not work in restricted mode. RunID forwards the
// single id parseValidateAgentConfig's caller generated for this whole
// `aicr validate` invocation, so the live-capture agent's Job/RBAC share it
// with the validator Jobs runValidation deploys — one RunID per command,
// per ADR-020, instead of the agent falling back to its own default.
func (c *validateAgentConfig) toAgentConfig() *aicr.AgentConfig {
	return &aicr.AgentConfig{
		Kubeconfig:         c.kubeconfig,
		Namespace:          c.namespace,
		Image:              c.image,
		ImagePullSecrets:   c.imagePullSecrets,
		JobName:            c.jobName,
		ServiceAccountName: c.serviceAccountName,
		NodeSelector:       c.nodeSelector,
		Tolerations:        c.tolerations,
		Timeout:            c.timeout,
		Cleanup:            c.cleanup,
		Debug:              c.debug,
		Privileged:         true,
		RequireGPU:         c.requireGPU,
		AKSGPUPoolsPath:    c.aksGPUPoolsPath,
		GKEGPUPoolsPath:    c.gkeGPUPoolsPath,
		RunID:              c.runID,
		NameBase:           validateNameBase,
		OKEAddonsPath:      c.okeAddonsPath,
	}
}

// deployAgentForValidation deploys an agent to capture a snapshot and returns the Snapshot.
// The agent deployer ensures the namespace itself (ensureNamespace, with the
// managed-by label, unless --service-account-name names an existing
// ServiceAccount) using the same explicit kubeconfig (#1787), so no
// pre-create happens here — deployAndWaitForResult's up-front pool-file
// projection must run before ANY cluster mutation so a malformed
// --aks-gpu-pools file fails without side effects.
//
// Collection runs through the same Client.CollectSnapshot that `aicr snapshot`
// uses, rather than a second hand-rolled snapshotter.AgentConfig, so the facade
// mirror is exercised here too. Output is left empty: validate consumes the
// snapshot in memory and never writes it out.
func deployAgentForValidation(ctx context.Context, client *aicr.Client, cfg *validateAgentConfig) (*aicr.Snapshot, error) {
	snap, err := client.CollectSnapshot(ctx, cfg.toAgentConfig())
	if err != nil {
		// PropagateOrWrap: a structured error (e.g. ErrCodeInvalidRequest
		// from a malformed --aks-gpu-pools file) keeps its code.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to capture snapshot")
	}

	// Returned in the facade shape rather than unwrapped: both snapshot
	// sources in this command now produce *aicr.Snapshot, so nothing
	// downstream has to convert. Unwrapping here and re-wrapping at the
	// ValidateState call also discarded Snapshot.Raw.
	return snap, nil
}

// facadePhases converts resolved validator phases into the facade's Phase
// type. Shared by the skip-list preflight and the validation run so the two
// cannot disagree about which phases they are talking about.
func facadePhases(phases []validator.Phase) []aicr.Phase {
	out := make([]aicr.Phase, len(phases))
	for i, p := range phases {
		out[i] = aicr.Phase(p)
	}
	return out
}

// logValidationModeBanner makes it explicit whether this run touches a live
// cluster (issue #1383). --no-cluster is an offline dry-run that reports checks
// as skipped; otherwise validation deploys validator Jobs against the active
// kube-context.
func logValidationModeBanner(noCluster bool) {
	if noCluster {
		slog.Info("validating in --no-cluster mode — offline dry-run; checks are reported as skipped, no cluster is contacted")
		return
	}
	slog.Info("validating against the live cluster — validator Jobs will be deployed to the active kube-context")
}

// runSkipCheckPreflight rejects an unusable --skip-check list against the
// recipe's own check catalog. Split out of the Action closure so the guard can
// be read (and length-budgeted) independently, as validateFlagCombinations is.
//
// Phases are passed only when a subset was requested, matching runValidation,
// because an empty selection means "all phases" on both sides: the preflight
// must judge the same phase set the run will execute, or it would reject a list
// the run would have accepted (or accept one it would not).
func runSkipCheckPreflight(
	ctx context.Context,
	client *aicr.Client,
	rec *aicr.RecipeResult,
	phases []validator.Phase,
	skipChecks []string,
) error {

	opts := []aicr.ValidateOption{aicr.WithValidationSkipChecks(skipChecks...)}
	if len(phases) > 0 {
		opts = append(opts, aicr.WithValidationPhases(facadePhases(phases)...))
	}
	return client.PreflightSkipChecks(ctx, rec, opts...)
}

// resolveValidatorServiceAccount resolves --validator-service-account-name
// (flag over config) and, when one is named, verifies it before the
// live-capture branch creates the snapshot agent's Job and RBAC, for the same
// reason runSkipCheckPreflight runs early. ValidateState repeats the check for
// callers that reach it directly.
func resolveValidatorServiceAccount(
	ctx context.Context,
	cmd *cli.Command,
	client *aicr.Client,
	opts aicr.ValidateSettings,
	kubeconfig, namespace string,
	noCluster bool,
) (string, error) {

	name := stringFlagOrConfig(cmd, flagValidatorSA, opts.ValidatorServiceAccountName)
	err := client.PreflightValidatorServiceAccount(ctx,
		aicr.WithValidationKubeconfig(kubeconfig),
		aicr.WithValidationNamespace(namespace),
		aicr.WithValidationNoCluster(noCluster),
		aicr.WithValidatorServiceAccountName(name),
	)
	return name, err
}

// warnRetainedValidatorBinding warns that --no-cleanup leaves the per-run
// cluster-admin ClusterRoleBinding in place. A run using an existing validator
// ServiceAccount created no binding, so it has none to warn about.
func warnRetainedValidatorBinding(shared validateSharedResolved, validatorSA string) {
	if !shared.noCleanup || validatorSA != "" {
		return
	}
	slog.Warn("--no-cleanup: cluster-admin ClusterRoleBinding will remain active after validation",
		"namespace", shared.namespace,
		"bindingSelector", "app.kubernetes.io/name=aicr-validator",
		"cleanupHint", "kubectl delete clusterrolebinding -l app.kubernetes.io/name=aicr-validator")
}

// validationConfig holds all parameters for a validation run.
type validationConfig struct {
	// Input
	phases []validator.Phase

	// skipChecks names checks to withhold from every phase that runs
	// (--skip-check / spec.validate.execution.skipChecks). Empty runs every
	// check the recipe declares.
	skipChecks []string

	// runID is generated once, up front, for the whole `aicr validate`
	// invocation (see validateCmd's Action) and reused here instead of a
	// second v1.GenerateRunID() call. Sharing one ID keeps names, labels,
	// logs, and cleanup hints correlated to a single command instead of
	// fragmenting across an agent-side ID and a validator-side one.
	runID string

	// Kubeconfig path; propagated to ConfigMap reads/writes so a single
	// validate invocation can target a non-default cluster end-to-end.
	kubeconfig string

	// Output
	output    string
	outFormat serializer.Format

	// Validator deployment
	validationNamespace string
	cleanup             bool
	imagePullSecrets    []string
	noCluster           bool

	// validatorServiceAccountName names an existing ServiceAccount the
	// validator Jobs run as; empty keeps per-run RBAC.
	validatorServiceAccountName string

	// Scheduling
	nodeSelector map[string]string
	tolerations  []corev1.Toleration

	// Image overrides
	imageRegistryOverride string
	imageTagOverride      string

	// Behavior
	failOnError bool
	failFast    bool

	// Evidence
	evidenceDir string
	// cncfSubmission marks a submission run: evidenceDir is rendered after
	// the phases ran and the exit status of the conformance phase follows the
	// requirement aggregation instead of the per-check rule.
	cncfSubmission bool
	// cncfNotApplicable is spec.validate.evidence.cncf.notApplicable, the
	// conditional MUSTs declared not applicable with their justifications.
	cncfNotApplicable map[string]string

	// Recipe-evidence bundle config; nil disables --emit-attestation work.
	evidence *recipeEvidenceConfig

	// imageRefs receives the pushed evidence bundle's digest; nil when
	// --image-refs is unset.
	imageRefs *imageRefsTarget
}

// runValidation runs validation using the container-per-validator engine.
//
// The validator run is driven through the aicr.Client facade (ValidateState),
// which owns the per-command DataProvider. Report merging and recipe-evidence
// emission also go through the facade (Client.MergeReports /
// Client.EmitRecipeEvidence), so the catalog resolves against the Client's
// source and no internal validator types are reconstructed CLI-side.
func runValidation(
	ctx context.Context,
	client *aicr.Client,
	rec *aicr.RecipeResult,
	snap *aicr.Snapshot,
	cfg validationConfig,
) error {

	slog.Info("running validation", "phases", cfg.phases)

	// Generated once CLI-side, before either snapshot source ran (see
	// validateCmd's Action), rather than letting the validator auto-generate
	// it internally: the no-cleanup debug log below needs to surface the
	// same ID the validator stamps on its Jobs/RBAC so an operator can
	// locate the kept resources. Passing it via WithValidationRunID keeps
	// that value in our hands.
	runID := cfg.runID

	// Translate the resolved CLI values into facade ValidateOptions. These
	// mirror the validator.With* options the direct invocation used to set;
	// ValidateState applies them plus the Client's own DataProvider. Cleanup,
	// namespace, etc. are always set (matching the old unconditional
	// validator.With* calls); the image/commit overrides are passed verbatim
	// (empty string is the validator's "unset" sentinel).
	opts := []aicr.ValidateOption{
		// Thread the CLI's resolved kubeconfig so every validator-engine
		// operation (namespace, RBAC, input/result ConfigMaps, validator
		// Jobs, watches, cleanup) targets the same cluster as artifact I/O
		// (#1787). Empty keeps default discovery — the facade skips the
		// option entirely on "".
		aicr.WithValidationKubeconfig(cfg.kubeconfig),
		aicr.WithValidationNamespace(cfg.validationNamespace),
		aicr.WithValidationRunID(runID),
		aicr.WithValidationCleanup(cfg.cleanup),
		aicr.WithValidationImagePullSecrets(cfg.imagePullSecrets),
		aicr.WithValidationNoCluster(cfg.noCluster),
		aicr.WithValidationTolerations(cfg.tolerations),
		aicr.WithValidationNodeSelector(cfg.nodeSelector),
		aicr.WithValidationCommit(commit),
		aicr.WithValidationImageRegistryOverride(cfg.imageRegistryOverride),
		aicr.WithValidationImageTagOverride(cfg.imageTagOverride),
		// Uncap the facade-level validation deadline (0 = no cap) so an
		// all-phase run isn't cut short by ValidationOperationTimeout — the
		// per-validator timeouts (incl. the 65m inference-perf check) govern,
		// matching the pre-facade CLI behavior. WithValidationTolerations
		// above is always passed (even when resolved tolerations are nil) so
		// the override always clears the validator's default tolerate-all.
		aicr.WithValidationTimeout(0),
		aicr.WithValidationFailFast(cfg.failFast),
		aicr.WithValidatorServiceAccountName(cfg.validatorServiceAccountName),
	}
	// Pass phases only when explicitly selected. cfg.phases is nil when the
	// user requested all phases (or used the "all" wildcard), and
	// WithValidationPhases(nil...) would be a no-op anyway — but keeping the
	// option off the slice preserves the exact "run all phases" default path.
	if len(cfg.phases) > 0 {
		opts = append(opts, aicr.WithValidationPhases(facadePhases(cfg.phases)...))
	}
	// Same shape as phases: pass the option only when there is something to
	// say, so the default path stays exactly the one that existed before the
	// knob did.
	if len(cfg.skipChecks) > 0 {
		opts = append(opts, aicr.WithValidationSkipChecks(cfg.skipChecks...))
	}

	results, err := client.ValidateState(ctx, rec, snap, opts...)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "validation failed")
	}

	// Merge the per-phase CTRF reports into a single combined report via the
	// facade, so a library/server caller of ValidateState produces the same
	// combined document without reimplementing the merge.
	combined := client.MergeReports(results)

	// Serialize combined report; thread kubeconfig so ConfigMap writes
	// target the same cluster used for snapshot/recipe reads.
	ser, serErr := serializer.NewFileWriterOrStdoutWithKubeconfig(cfg.outFormat, cfg.output, cfg.kubeconfig)
	if serErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create output writer", serErr)
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				slog.Warn("failed to close serializer", "error", closeErr)
			}
		}
	}()

	if writeErr := ser.Serialize(ctx, combined); writeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize CTRF report", writeErr)
	}

	// Log per-phase summary. The skipped count is printed alongside the status
	// because a phase can report passed while several of its checks never ran:
	// --skip-check withholds them deliberately, and an operator reading only
	// "status=passed" would have no way to see that from here.
	anyFailed := false
	for _, pr := range results {
		skipped := 0
		if pr.Report != nil {
			skipped = pr.Report.Results.Summary.Skipped
		}
		slog.Info("phase result",
			"phase", pr.Phase,
			"status", pr.Status,
			"skipped", skipped,
			"duration", pr.Duration)
		if ctrf.IsFailingStatus(pr.Status) {
			anyFailed = true
		}
	}

	// If cleanup is disabled, provide helpful debugging info
	if !cfg.cleanup {
		kept := "Jobs and RBAC"
		if cfg.validatorServiceAccountName != "" {
			kept = "Jobs and ConfigMaps"
		}
		slog.Info("cleanup disabled - validator "+kept+" kept for debugging",
			"namespace", cfg.validationNamespace,
			"runID", runID)
		slog.Info("to inspect Job logs: kubectl logs -l " + labels.RunID + "=" + runID + " -n " + cfg.validationNamespace)
		slog.Info("to list Jobs: kubectl get jobs -n " + cfg.validationNamespace)
		slog.Info("to cleanup manually: kubectl delete jobs -l app.kubernetes.io/name=aicr -n " + cfg.validationNamespace)
	}

	// Render the submission evidence when requested. The checklist version
	// follows the validated cluster's Kubernetes minor. A minor this build
	// carries no checklist for renders against the nearest carried one and the
	// facade warns: below v1.35 against v1.35, above the newest against the
	// latest (cncf.NearestVersion). An explicit version flag is a follow-up
	// to #3606.
	//
	// The exit rule of a submission run is CNCFRequirementAggregate.Outcome,
	// shared with the SDK: a failed or crashed check fails the run as it does
	// without the flag (conformance checks with no requirement mapping, such
	// as platform-health, are visible only to that rule), and an applicable
	// MUST that is not established fails it on top (ADR-007 amendment,
	// decision 2). A render error, like that outcome, is held until the
	// attestation below has been emitted: the two evidence kinds are
	// independent pipelines over the same run, and a failed CNCF render must
	// not cost the run its signed recipe-evidence bundle.
	var renderErr error
	// outcome is CNCFRequirementAggregate.Outcome once the evidence rendered;
	// decided marks that it was computed, since nil is a valid outcome.
	var outcome error
	var decided bool
	if cfg.evidenceDir != "" {
		renderOpts := aicr.RenderCNCFEvidenceOptions{
			KubernetesVersion: snap.KubernetesVersion(),
			NotApplicable:     cfg.cncfNotApplicable,
		}
		agg, err := client.RenderCNCFEvidenceWithResult(ctx, combined, cfg.evidenceDir, renderOpts)
		switch {
		case err != nil:
			renderErr = err
			slog.Error("conformance evidence rendering failed", "error", err, "dir", cfg.evidenceDir)
		case anyRenderedCheck(agg):
			slog.Info("conformance evidence written", "dir", cfg.evidenceDir, "kubernetesVersion", renderOpts.KubernetesVersion)
			logRequirementAggregate(agg)
			outcome, decided = agg.Outcome(combined, cfg.failOnError), true
		default:
			// The renderer writes a page only for a check that ran and was
			// not skipped; with none, it creates nothing, not even the
			// directory.
			slog.Warn("no conformance check ran to completion; no evidence was written", "dir", cfg.evidenceDir)
			logRequirementAggregate(agg)
			outcome, decided = agg.Outcome(combined, cfg.failOnError), true
		}
	}

	// Emit even on failure: failed runs document hardware-specific limits.
	// The facade (Client.EmitRecipeEvidence) owns the facade→internal
	// PhaseResult conversion, catalog load (against this Client's data
	// source), and attestation.Emit; the CLI shim only adds the interactive
	// signing-disclosure prompt.
	if cfg.evidence != nil {
		emission, err := emitRecipeEvidence(ctx, client, rec, snap, results, cfg.evidence)
		if err != nil {
			return err
		}
		if err := writeEvidenceImageRefs(ctx, cfg.imageRefs, emission); err != nil {
			return err
		}
	}

	return validationExit(decided, outcome, cfg.failOnError && anyFailed, renderErr)
}

// validationExit picks the run's exit error. A submission run whose evidence
// rendered is decided by CNCFRequirementAggregate.Outcome alone: it already
// applied the per-check rule to the same merged report, so wrapping it again
// with the phase-level failure would repeat the message. Every other run,
// including a submission run whose render failed, goes through runOutcome.
func validationExit(decided bool, outcome error, checksFailed bool, renderErr error) error {
	if decided {
		return outcome
	}
	return runOutcome(checksFailed, renderErr)
}

// runOutcome combines a per-check failure with a submission-stage error, the
// render failing before any outcome was computed. When both are present the
// submission error is wrapped with the per-check context under its own classified code
// (a render canceled by the operator stays CANCELED, a timed-out one stays
// TIMEOUT), so the exit code follows the cause rather than the context; a
// plain error falls back to INTERNAL.
func runOutcome(checksFailed bool, submissionErr error) error {
	switch {
	case submissionErr != nil && checksFailed:
		code := errors.ErrCodeInternal
		if se, ok := stderrors.AsType[*errors.StructuredError](submissionErr); ok && se.Code != "" {
			code = se.Code
		}
		return errors.Wrap(code,
			"validation failed: one or more phases did not pass, and the CNCF evidence render failed", submissionErr)
	case submissionErr != nil:
		return submissionErr
	case checksFailed:
		return errors.New(errors.ErrCodeInternal, "validation failed: one or more phases did not pass")
	}
	return nil
}

// anyRenderedCheck reports whether the renderer had anything to write: a
// check that is present and not skipped, on a requirement or as supporting
// evidence. The renderer drops skipped results before grouping, so a report
// of only skips renders no page and no index.
func anyRenderedCheck(agg *aicr.CNCFRequirementAggregate) bool {
	if agg == nil {
		return false
	}
	for _, r := range agg.Requirements {
		for _, c := range r.Checks {
			if c.Present && c.Status != ctrf.StatusSkipped {
				return true
			}
		}
	}
	for _, r := range agg.Supporting {
		for _, c := range r.Checks {
			if c.Present && c.Status != ctrf.StatusSkipped {
				return true
			}
		}
	}
	return false
}

func validateCmdFlags() []cli.Flag {
	flags := []cli.Flag{
		&cli.StringFlag{
			Name:    cmdNameRecipe,
			Aliases: []string{"r"},
			Usage: `Path/URI to recipe file containing constraints to validate.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).`,
			Category: catInput,
		},
		&cli.StringFlag{
			Name:    cmdNameSnapshot,
			Aliases: []string{"s"},
			Usage: `Path/URI to snapshot file containing actual system measurements.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	If not provided, an agent will be deployed to capture a fresh snapshot.`,
			Category: catInput,
		},
		&cli.StringSliceFlag{
			Name: "phase",
			Usage: `Validation phase(s) to run (can be repeated).
	Options: "deployment", "performance", "conformance", "all".
	Default: all phases.
	Example: --phase deployment --phase conformance`,
			Category: catValidationControl,
		},
		&cli.StringSliceFlag{
			Name: "skip-check",
			Usage: `Check(s) to withhold from every phase that runs (can be repeated).
	For a caller that cannot satisfy a check the recipe declares, e.g. a lane
	deploying a subset of the recipe. Each named check is REPORTED as skipped,
	not dropped, so the report still accounts for it.
	Rejected before any validation resource is created when a name matches no
	check, or when the list would leave a requested phase with nothing to run.
	A cm:// recipe is read from the cluster first, so that form contacts the
	API server before the list is judged.
	Example: --skip-check gpu-operator-health --skip-check dra-support`,
			Category: catValidationControl,
		},
		&cli.BoolFlag{
			Name:  "fail-on-error",
			Value: true,
			Usage: "Exit with non-zero status if any phase check reports failed or other " +
				"(crash/OOM/timeout). Does not affect the readiness pre-flight, which always " +
				"fails closed with exit 2 when a prerequisite constraint (e.g. K8s version) is not met, " +
				"nor the --cncf-submission verdict, which fails the run on an unestablished MUST regardless of this flag",
			Category: catValidationControl,
		},
		&cli.BoolFlag{
			Name:     "fail-fast",
			Value:    false,
			Usage:    "Stop validation after the first phase that fails. By default all phases run and produce results.",
			Category: catValidationControl,
		},
		&cli.BoolFlag{
			Name:     "no-cluster",
			Usage:    "Run validation without cluster access (dry-run mode). Reports all checks as skipped.",
			Category: catValidationControl,
		},
		// Agent deployment flags (used when --snapshot is not provided)
		&cli.StringFlag{
			Name:     "namespace",
			Aliases:  []string{"n"},
			Usage:    "Kubernetes namespace for snapshot agent and validation Jobs",
			Sources:  cli.EnvVars("AICR_NAMESPACE"),
			Value:    defaults.ValidationNamespace,
			Category: catDeployment,
		},
		&cli.StringFlag{
			Name:     "image",
			Usage:    "Container image for snapshot agent",
			Sources:  cli.EnvVars("AICR_VALIDATOR_IMAGE"),
			Value:    defaultAgentImage(),
			Category: catAgentDeployment,
		},
		&cli.StringSliceFlag{
			Name:     "image-pull-secret",
			Usage:    "Secret name for pulling images from private registries (can be repeated)",
			Category: catAgentDeployment,
		},
		// Value stays at the released v1 defaults ("aicr-validate" and
		// "aicr"): they are what `--help` prints and what
		// testdata/cli-surface.golden pins. parseValidateAgentConfig reads
		// both with explicitStringFlagOrConfig, so an unset flag reaches the
		// agent as "" and validateNameBase supplies the prefix instead.
		&cli.StringFlag{
			Name:     "job-name",
			Usage:    "Job name prefix; the run ID is always appended",
			Value:    validateNameBase,
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "service-account-name",
			Usage:    "ServiceAccount the live snapshot-capture agent runs as. Exact-if-exists: when a ServiceAccount of exactly this name already exists in --namespace it is used verbatim, and the agent leaves the namespace untouched and creates and deletes no RBAC for the run (generate its RBAC manifests with 'aicr snapshot --add-roles-to-service-account', then apply them yourself). The validator Jobs still apply the namespace unless --validator-service-account-name is set. Otherwise it is a name prefix and the run ID is appended. Leaving the flag unset is not the same as passing the default shown: an unset value is never probed for existence, and the agent's run-scoped names are derived from the \"aicr-validate\" base instead.",
			Value:    name,
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     flagValidatorSA,
			Usage:    "Existing ServiceAccount in --namespace that the validator Jobs run as, instead of a per-run ServiceAccount bound to cluster-admin. For the validator Jobs the run then creates and deletes no ServiceAccount or ClusterRoleBinding and does not apply the namespace, so the caller needs no permission to grant cluster-admin. An administrator must create the namespace and the ServiceAccount and bind it to the cluster-admin ClusterRole; the run verifies that binding and fails before creating anything when the ServiceAccount is missing, unbound, or cannot be checked. Without --snapshot, the snapshot-capture agent still creates or labels the namespace and creates its own RBAC unless --service-account-name names an existing ServiceAccount for it.",
			Category: catDeployment,
		},
		&cli.StringSliceFlag{
			Name:     "node-selector",
			Usage:    "Override GPU node selection for the live snapshot agent (when --snapshot is omitted) and inner validation workloads (format: key=value, can be repeated). Replaces platform-specific selectors on inner workloads (e.g., NCCL benchmark pods). Does not affect the validator orchestrator Job.",
			Category: catScheduling,
		},
		&cli.StringSliceFlag{
			Name:     "toleration",
			Usage:    "Override tolerations for the live snapshot agent (when --snapshot is omitted) and inner validation workloads (format: key=value:effect, can be repeated). When omitted, the snapshot agent tolerates all taints. Does not affect the validator orchestrator Job.",
			Category: catScheduling,
		},
		&cli.DurationFlag{
			Name:     "timeout",
			Usage:    "Timeout for waiting for Job completion",
			Value:    defaults.CLISnapshotTimeout,
			Category: catAgentDeployment,
		},
		&cli.BoolFlag{
			Name:     "no-cleanup",
			Usage:    "Skip removal of Job and RBAC resources on completion (leaves the per-run validator cluster-admin binding active unless --validator-service-account-name is set)",
			Category: catAgentDeployment,
		},
		&cli.BoolFlag{
			Name:     "require-gpu",
			Sources:  cli.EnvVars("AICR_REQUIRE_GPU"),
			Usage:    "Request nvidia.com/gpu resource for the agent pod.",
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "oke-addons",
			Usage:    "Path to an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump on the local filesystem. When validate captures a live snapshot, the NvidiaGpuPlugin add-on's control-plane state is projected into the K8s oke-addons subtype so profile constraints recorded in OKE recipes can evaluate. The NvidiaNetworkOperator add-on is projected the same way into K8s.oke-addons.nvidia-network-operator, which OKE recipes that deploy network-operator require to be absent. The --all flag on the oci command is required. Without it, removed and non-ACTIVE add-ons are omitted from the dump and would read as absent instead of failing closed. Capturing with --page is not supported. A dump with opc-next-page set is rejected, but a final page alone looks like a complete list. Ignored when --snapshot supplies a pre-captured snapshot.",
			Sources:  cli.EnvVars("AICR_OKE_ADDONS_PATH"),
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "aks-gpu-pools",
			Usage:    "Path to an `az aks nodepool list -o json` dump on the local filesystem. When validate captures a live snapshot, the GPU pools' gpuProfile.driver values are projected into the K8s aks-gpu-pools subtype (ADR-015 DD3) so profile constraints recorded in AKS recipes can evaluate. Ignored when --snapshot supplies a pre-captured snapshot.",
			Sources:  cli.EnvVars("AICR_AKS_GPU_POOLS_PATH"),
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "gke-gpu-pools",
			Usage:    "Path to a `gcloud container node-pools list --cluster <cluster> --format=json` dump on the local filesystem. When validate captures a live snapshot, each GPU pool's gpuDriverInstallationConfig.gpuDriverVersion is projected into the K8s gke-gpu-pools subtype so profile constraints recorded in GKE recipes can evaluate. Ignored when --snapshot supplies a pre-captured snapshot.",
			Sources:  cli.EnvVars("AICR_GKE_GPU_POOLS_PATH"),
			Category: catAgentDeployment,
		},
	}
	flags = append(flags, validateEvidenceFlags()...)
	flags = append(flags, evidenceSigningFlags()...)
	return append(flags,
		configFlag(),
		dataFlag(),
		outputFlag(),
		kubeconfigFlag(),
	)
}

// validateEvidenceFlags returns the --emit-attestation flag family: what to
// emit, where to push it, how much of it to ship, and the keyless-signing
// inputs. Split out of validateCmdFlags to keep that function under the
// funlen limit; the order here is the order the flags appear under the
// Evidence heading in `aicr validate --help`.
func validateEvidenceFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name: "evidence-dir",
			Usage: `Directory the CNCF AI Conformance submission evidence is written to.
	Requires --cncf-submission; the two flags require each other.`,
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name: "cncf-submission",
			Usage: `Also write the CNCF AI Conformance submission evidence for this run into --evidence-dir.
	The run is the conformance phase as the recipe declares it: the same checks with the same verdicts
	as a run without this flag, rendered afterwards into one evidence page per requirement a non-skipped check
	evidenced, plus index.md; a run in which no check ran to completion writes nothing.
	Requires --recipe, --evidence-dir and the conformance phase; refused with --no-cluster and --skip-check.
	The exit status adds the requirement aggregation to the per-check rule: an applicable MUST that is not
	established fails the run regardless of --fail-on-error; the aggregation never fails it on a SHOULD.`,
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: "emit-attestation",
			Usage: `Directory to write a recipe-evidence attestation bundle (predicate v3; signed when --push is set, unless --no-sign).
	Produces summary-bundle/ and pointer.yaml suitable for copying to recipes/evidence/<recipe>/<source>/<digest>.yaml (see the emit 'copyTo' hint).
	The bundle is minimized by default (sensitive snapshot fields and CTRF logs removed); use --full to ship raw payloads.
	See ADR-007 (docs/design/007-recipe-evidence.md).`,
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name: flagFull,
			Usage: `Emit the full (unredacted) evidence bundle. By default the bundle is minimized:
	the snapshot is reduced to an allowlisted set of fields and per-test CTRF stdout/message are omitted,
	so node names, provider instance IDs, the node label/taint set, OS tuning, and raw container logs are
	not published. --full restores the complete payloads. The cryptographic verification story
	(predicate digests, manifest binding, signature) holds either way.`,
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name: flagAllowMutableValidatorTags,
			// Deliberately no Sources: an env var set once for a disposable run
			// would silently disable the gate for every later run in that shell
			// or CI job — the same "applied from habit" failure AICR_VALIDATOR_IMAGE_TAG
			// caused in #2873. Opting out must be argued per invocation.
			Usage: `Emit the attestation even when a validator image resolves to a mutable tag.
	Emission otherwise fails closed: the predicate identifies the validators that ran by tag alone, so a moving
	tag (:edge, :latest) leaves the attestation naming a reference that can later resolve to different validator
	code. Immutable tags are :vX.Y.Z, :sha-<commit>, :uat-<run-id>, and digest-pinned refs.
	The usual cause is a stale AICR_VALIDATOR_IMAGE_TAG override — unset it before a conformance run rather than
	reaching for this flag. Reserve the flag for disposable evidence (a local demo, a side-loaded smoke lane).
	There is no environment-variable or config-file equivalent: the opt-out must be passed per invocation.`,
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: "bom",
			Usage: `Path to a CycloneDX BOM (bom.cdx.json) to embed in the evidence bundle.
	Optional with --emit-attestation: when omitted, aicr synthesizes a
	recipe-bound BOM from the recipe's component refs + the validator
	catalog images that ran. Pass an explicit path for an exhaustive
	BOM (e.g., produced by 'make bom').`,
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: flagPush,
			Usage: `OCI registry reference (e.g. ghcr.io/myorg/aicr-evidence) to push the signed summary bundle to.
	Sigstore keyless OIDC signing uses the same precedence chain as ` + "`aicr bundle --attest`" + `:
	--identity-token > COSIGN_IDENTITY_TOKEN env > GitHub Actions ambient OIDC >
	--oidc-device-flow > interactive browser flow. --signing-key signs with a KMS key instead.`,
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: flagImageRefs,
			Usage: `File to receive the digest of the evidence bundle --push published (sha256:...), the value pointer.yaml
	records as bundle.digest, in the format aicr bundle --image-refs writes. Requires --emit-attestation and --push.
	The parent must be an existing real directory; the target must be absent or a regular file, clear of
	--emit-attestation and --output. A mount point such as a Kubernetes termination-message file is written in place.
	The target is checked, including that it can be written, before validation starts.`,
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name: flagNoSign,
			Usage: `Push the evidence bundle unsigned (requires --emit-attestation and --push) and write a pointer with an empty signer block.
	Defers Fulcio/Rekor signing to a later step (the fork-based CI workflow), so the network-light push can run
	where the cluster lives even when Sigstore egress is blocked. No-op unless both --emit-attestation and --push are set.`,
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     flagPlainHTTP,
			Usage:    "Use HTTP instead of HTTPS when pushing the evidence OCI artifact (local registry tests).",
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     flagInsecureTLS,
			Usage:    "Skip TLS verification when pushing the evidence OCI artifact (self-signed registries).",
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name:     flagIdentityToken,
			Usage:    "Pre-fetched OIDC identity token for --push keyless signing. Skips ambient/browser/device-code flows. Prefer COSIGN_IDENTITY_TOKEN on shared hosts; flag values are visible in process listings (ps, /proc/<pid>/cmdline).",
			Sources:  cli.EnvVars("COSIGN_IDENTITY_TOKEN"),
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     flagOIDCDeviceFlow,
			Usage:    "Use the OAuth 2.0 device authorization grant for --push OIDC instead of opening a browser callback. Useful on headless hosts when --identity-token / COSIGN_IDENTITY_TOKEN and ambient GitHub Actions OIDC are both unavailable.",
			Sources:  cli.EnvVars("AICR_OIDC_DEVICE_FLOW"),
			Category: catEvidence,
		},
		assumeYesFlag(catEvidence),
	}
}

// warnIgnoredProjections notes each provider projection flag a pre-captured
// --snapshot makes inert.
func warnIgnoredProjections(cmd *cli.Command, snapshotFilePath string) {
	warnIgnoredAKSGPUPools(cmd, snapshotFilePath)
	warnIgnoredOKEAddons(cmd, snapshotFilePath)
	warnIgnoredGKEGPUPools(cmd, snapshotFilePath)
}

// warnIgnoredAKSGPUPools notes that the pool projection only applies to live
// capture: a pre-captured --snapshot must have been taken with the flag, or
// the user later sees "reading unavailable" on a run where they did pass it.
// When the value arrived via the ambient env var (e.g. a CI lane exporting
// AICR_AKS_GPU_POOLS_PATH job-wide whose snapshots WERE captured with the
// reading), log at debug — it is not an explicit per-invocation request.
func warnIgnoredAKSGPUPools(cmd *cli.Command, snapshotFilePath string) {
	warnIgnoredProjection(cmd, snapshotFilePath, "aks-gpu-pools", "AICR_AKS_GPU_POOLS_PATH")
}

// warnIgnoredOKEAddons is the OKE analog of warnIgnoredAKSGPUPools.
func warnIgnoredOKEAddons(cmd *cli.Command, snapshotFilePath string) {
	warnIgnoredProjection(cmd, snapshotFilePath, "oke-addons", "AICR_OKE_ADDONS_PATH")
}

// warnIgnoredGKEGPUPools warns when a --gke-gpu-pools flag or
// AICR_GKE_GPU_POOLS_PATH env var is ignored because the snapshot being
// validated was already captured without the projection.
func warnIgnoredGKEGPUPools(cmd *cli.Command, snapshotFilePath string) {
	warnIgnoredProjection(cmd, snapshotFilePath, "gke-gpu-pools", "AICR_GKE_GPU_POOLS_PATH")
}

func warnIgnoredProjection(cmd *cli.Command, snapshotFilePath, flagName, envVar string) {
	shouldLog, atWarn := classifyIgnoredProjection(
		os.Args[1:], os.Getenv(envVar),
		cmd.String(flagName), snapshotFilePath, flagName)
	if !shouldLog {
		return
	}
	msg := "--" + flagName + " does not apply to a pre-captured --snapshot; " +
		"the reading must already be in that snapshot (capture it with the same flag if missing)"
	if atWarn {
		slog.Warn(msg)
		return
	}
	slog.Debug(msg)
}

// classifyIgnoredAKSGPUPools decides whether the ignored-projection note is
// emitted and at which level. Provenance, not value equality: an explicit
// CLI flag always warns — the user asked for the projection on THIS
// invocation. Only a purely ambient env source (e.g. a CI lane exporting
// AICR_AKS_GPU_POOLS_PATH job-wide whose snapshots WERE captured with the
// reading) is demoted to debug. Pure function for testability.
func classifyIgnoredProjection(args []string, envValue, poolsPath, snapshotFilePath, flagName string) (shouldLog, atWarn bool) {
	if snapshotFilePath == "" || poolsPath == "" {
		return false, false
	}
	explicit := false
	for _, arg := range args {
		if arg == "--"+flagName || strings.HasPrefix(arg, "--"+flagName+"=") {
			explicit = true
			break
		}
	}
	if !explicit && envValue != "" {
		return true, false
	}
	return true, true
}

func validateCmd() *cli.Command {
	return &cli.Command{
		Name:     "validate",
		Category: functionalCategoryName,
		Usage:    "Validate cluster against recipe constraints using containerized validators.",
		Description: `Run validation checks against a cluster snapshot using the constraints and
checks defined in a recipe. Each validator runs as an isolated Kubernetes Job.

Results are output in CTRF (Common Test Report Format) JSON — an industry-standard
schema for test reporting (https://ctrf.io/). Output goes to stdout or the file
specified by --output.

You can either provide an existing snapshot file or let the command deploy an
agent to capture a fresh snapshot from the cluster.

# Examples

Validate using an existing snapshot:
  aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

Deploy agent to capture and validate in one step:
  aicr validate --recipe recipe.yaml

Run specific phases:
  aicr validate -r recipe.yaml -s snapshot.yaml \
    --phase deployment --phase conformance

Save CTRF report to file:
  aicr validate -r recipe.yaml -s snapshot.yaml --output report.json

Run validation without failing on phase check errors (informational mode).
Note: the readiness pre-flight still fails closed with exit 2 if a prerequisite
constraint (e.g. K8s version) is not met — --fail-on-error scopes to phase checks:
  aicr validate -r recipe.yaml -s snapshot.yaml --fail-on-error=false
`,
		Flags: validateCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output", "config", "namespace", "image", "job-name", "service-account-name", flagValidatorSA, "timeout", "data", "evidence-dir", "emit-attestation", "bom", "aks-gpu-pools", "oke-addons", "gke-gpu-pools", flagPush, flagImageRefs, flagIdentityToken, flagSigningKey, flagFulcioURL, flagRekorURL, flagSigningConfig); err != nil {
				return err
			}

			cfg, err := loadFacadeConfig(ctx, cmd)
			if err != nil {
				return err
			}
			opts, validatePresent, err := cfg.ValidateSettings()
			if err != nil {
				return err
			}
			input, err := cfg.ValidateInputOptions()
			if err != nil {
				return err
			}

			cncfOpts, err := cfg.CNCFEvidenceOptions()
			if err != nil {
				return err
			}
			evidenceDir := stringFlagOrConfig(cmd, "evidence-dir", cncfOpts.Dir)
			cncfSubmission := boolFlagOrConfig(cmd, "cncf-submission", cncfOpts.CNCFSubmission)
			// Resolved once here (flag > config) and reused by the guards below and
			// the mode banner further down.
			noCluster := boolFlagOrConfig(cmd, "no-cluster", opts.NoCluster)
			explicitAttest := cmd.IsSet("emit-attestation") || cmd.IsSet(flagPush)
			// Not checked against the catalog here: that needs the recipe,
			// which is not loaded yet. runSkipCheckPreflight does it further
			// down, once the recipe is in hand and still before the
			// snapshot/agent branch touches the cluster. Resolved this early
			// only because the flag-combination guard below needs it.
			skipChecks := stringSliceFlagOrConfig(cmd, "skip-check", opts.SkipChecks)

			if err = validateFlagCombinations(cncfSubmission, evidenceDir, noCluster, explicitAttest, skipChecks); err != nil {
				return err
			}

			evidenceCfg, imageRefs, err := prepareRecipeEvidence(ctx, cmd, cfg, noCluster)
			if err != nil {
				return err
			}
			defer closeEvidenceImageRefs(imageRefs)

			recipeFilePath := stringFlagOrConfig(cmd, "recipe", input.RecipePath)

			phases, err := validator.ParsePhaseSelection(stringSliceFlagOrConfig(cmd, "phase", phaseStringsFallback(opts)))
			if err != nil {
				return err
			}
			if err = validateSubmissionPhases(cncfSubmission, phases); err != nil {
				return err
			}

			snapshotFilePath := stringFlagOrConfig(cmd, "snapshot", input.SnapshotPath)
			warnIgnoredProjections(cmd, snapshotFilePath)
			kubeconfig := cmd.String("kubeconfig")

			if recipeFilePath == "" {
				return errors.New(errors.ErrCodeInvalidRequest,
					"--recipe is required (or set spec.validate.input.recipe in --config)")
			}

			failOnError := boolFlagOrConfig(cmd, "fail-on-error", derefBoolOr(input.FailOnError, true))
			failFast := boolFlagOrConfig(cmd, "fail-fast", derefBoolOr(opts.FailFast, false))

			logValidationModeBanner(noCluster)

			// Resolve shared fields once, before the snapshot/agent split, so
			// CLI-overrides-config log lines fire exactly once per field even
			// when both the agent-deploy path and the validator Job want the
			// same value.
			tolerations, err := resolveValidateTolerations(cmd, opts)
			if err != nil {
				return err
			}
			nodeSelector, err := resolveValidateNodeSelector(cmd, opts)
			if err != nil {
				return err
			}
			cleanupFallback := validateCleanupFallback(opts, validatePresent)
			shared := validateSharedResolved{
				namespace:        stringFlagOrConfig(cmd, "namespace", opts.Namespace),
				imagePullSecrets: stringSliceFlagOrConfig(cmd, "image-pull-secret", opts.ImagePullSecrets),
				nodeSelector:     nodeSelector,
				tolerations:      tolerations,
				// cleanupFallback is cleanup-polarity (adjusted for absence above);
				// invert it to noCleanup-polarity to merge with --no-cleanup, which is
				// the polarity validateSharedResolved.noCleanup stores.
				noCleanup: boolFlagOrConfig(cmd, "no-cleanup", !cleanupFallback),
			}

			// Build the Client from the resolved data source (--data flag,
			// else spec.recipe.data) via the shared helper. The Client owns
			// its DataProvider, replacing the old process-global data
			// provider; evidence emission below uses a separate provider
			// handle to the same directory (dataDir) so SLSA / conformance
			// evidence resolves files against the command's source rather
			// than the package global.
			client, err := validateClient(ctx, cmd, cfg, evidenceDir, cncfOpts.NotApplicable)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			slog.Info("loading recipe", "uri", recipeFilePath)

			rec, err := client.LoadRecipe(ctx, recipeFilePath, kubeconfig)
			if err != nil {
				return err
			}

			// The --skip-check help text promises rejection "before the
			// cluster is touched". The guard that decides it is catalog-backed
			// and lives inside ValidateState, which the agent-deploy branch
			// below reaches only AFTER creating a ServiceAccount, a Role and a
			// Job, so a typo'd name used to cost cluster resources before it
			// was rejected. Run the same guard here, against the recipe just
			// loaded and with the same phases and skip list runValidation will
			// pass, so both decisions are identical. The guard inside
			// ValidateState stays: pkg/server and other SDK callers reach it
			// directly and must remain covered. An empty skip list returns
			// immediately, without loading the catalog.
			if err = runSkipCheckPreflight(ctx, client, rec, phases, skipChecks); err != nil {
				return err
			}

			validatorSA, err := resolveValidatorServiceAccount(ctx, cmd, client, opts, kubeconfig, shared.namespace, noCluster)
			if err != nil {
				return err
			}

			// Generate the run ID once, up front, for this whole invocation —
			// before either snapshot source below runs. The validator Jobs
			// deployed by runValidation need it (passed through
			// validationConfig.runID); generating it here rather than inside
			// runValidation means a single `aicr validate` no longer risks
			// splitting its correlation ID across two independent
			// v1.GenerateRunID() calls. The same runID is also threaded into
			// parseValidateAgentConfig below, so the live-capture branch's
			// snapshot agent shares this run's id with the validator Jobs
			// rather than defaulting its own inside DeployAndCollect —
			// ADR-020 requires one RunID per `aicr validate` invocation.
			//
			// WARNING, not enforced by an automated test: this line must
			// stay the ONLY v1.GenerateRunID() call in this Action. Adding a
			// second call — e.g. generating a fresh id inline for the
			// parseValidateAgentConfig call below, or for the
			// validationConfig{runID: ...} literal further down — silently
			// splits a single `aicr validate` invocation back into two
			// uncorrelated runs. The two consumer sites are NOT mutually
			// exclusive: with neither --snapshot nor --no-cluster, the
			// live-capture branch below and runValidation both consume this
			// id in the same invocation — which is exactly the case the
			// invariant protects. What blocks a unit test is that reaching
			// both requires live-cluster I/O (deployAgentForValidation
			// deploys a Job; runValidation deploys validator Jobs) with no
			// injectable seam here to fake it. See
			// TestValidateAgentConfig_ToAgentConfig_ForwardsRunID and
			// TestParseValidateAgentConfig_ForwardsCallerRunID in
			// validate_test.go for what IS covered (passthrough at each
			// function boundary only) and what is not.
			runID := v1.GenerateRunID()

			var snap *aicr.Snapshot

			// --no-cluster means "do not touch the cluster". The agent-deploy
			// branch below contradicts that (it creates a Job and captures a
			// snapshot from the live API), so a snapshot file is the only valid
			// data source in that mode. Placed after recipe.LoadFromFile so
			// recipe kind-check and auto-hydration still run for CLI coverage.
			if snapshotFilePath == "" && noCluster {
				return errors.New(errors.ErrCodeInvalidRequest,
					"--no-cluster requires --snapshot (or set spec.validate.input.snapshot in --config); cannot deploy the snapshot-capture agent without cluster access")
			}

			if snapshotFilePath != "" {
				slog.Info("loading snapshot", "uri", snapshotFilePath)
				snap, err = client.LoadSnapshot(ctx, snapshotFilePath, kubeconfig)
				if err != nil {
					return err
				}
			} else {
				slog.Info("deploying agent to capture snapshot")

				agentCfg := parseValidateAgentConfig(cmd, opts, shared, runID)

				var deployErr error
				snap, deployErr = deployAgentForValidation(ctx, client, agentCfg)
				if deployErr != nil {
					return deployErr
				}
			}

			// Advisory: warn when the running binary, the recipe-producing
			// binary, and the snapshot-producing binary report different
			// release versions. Mixed-version artifacts can cause confusing
			// validation failures; this does not fail the command.
			// Unwrap for the snapshot's producer version: Metadata is not
			// projected onto the facade Snapshot, which carries only the
			// fields the resolve and validate paths consume.
			warnVersionSkew(version, rec.Resolved().Metadata.Version, snap.Unwrap().Metadata["version"])

			// Warn when a requested phase has no checks defined in the recipe.
			// The helper reads the full recipe's Validation section, which the
			// lossy facade RecipeResult does not expose — reach the underlying
			// pkg/recipe.RecipeResult via Resolved(). Advisory only.
			validator.WarnPhasesAgainstRecipe(phases, rec.Resolved())

			warnRetainedValidatorBinding(shared, validatorSA)

			return runValidation(ctx, client, rec, snap, validationConfig{
				phases:                      phases,
				skipChecks:                  skipChecks,
				runID:                       runID,
				kubeconfig:                  kubeconfig,
				output:                      cmd.String("output"),
				outFormat:                   serializer.FormatJSON,
				failOnError:                 failOnError,
				failFast:                    failFast,
				validationNamespace:         shared.namespace,
				validatorServiceAccountName: validatorSA,
				cleanup:                     !shared.noCleanup,
				imagePullSecrets:            shared.imagePullSecrets,
				noCluster:                   noCluster,
				nodeSelector:                shared.nodeSelector,
				tolerations:                 shared.tolerations,
				imageRegistryOverride:       os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"),
				imageTagOverride:            os.Getenv("AICR_VALIDATOR_IMAGE_TAG"),
				evidenceDir:                 evidenceDir,
				cncfSubmission:              cncfSubmission,
				cncfNotApplicable:           cncfOpts.NotApplicable,
				evidence:                    evidenceCfg,
				imageRefs:                   imageRefs,
			})
		},
	}
}

// validateClient builds the facade Client for a validation run and refuses a
// bad spec.validate.evidence.cncf.notApplicable key against it before any
// recipe, snapshot or cluster work, rather than leaving that to the render
// after every validator Job has run. The checklist version is not known yet
// (it follows the snapshot), so the pre-flight checks against the latest
// carried checklist; the conditional MUST set is the same in every carried
// version, and the renderer repeats the check against the selected version
// as the fail-closed backstop. A run without --evidence-dir renders nothing
// and is not checked. The Client is closed when the pre-flight refuses.
func validateClient(ctx context.Context, cmd *cli.Command, cfg *aicr.Config, evidenceDir string, notApplicable map[string]string) (*aicr.Client, error) {
	client, err := recipeClientFromCmd(ctx, cmd, cfg)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to initialize data provider")
	}
	if evidenceDir == "" {
		return client, nil
	}
	if err := client.ValidateCNCFEvidenceOptions(aicr.RenderCNCFEvidenceOptions{NotApplicable: notApplicable}); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// logRequirementAggregate reports the CNCF requirement aggregation of a
// submission run: the checklist, whether every applicable MUST passed, and
// each MUST that is not established with its reason. When no conformance
// check produced a result at all (a recipe that declares none), one warning
// says so instead of one per MUST, since every MUST would read not run for
// the same reason. The exit status is CNCFRequirementAggregate.Outcome.
func logRequirementAggregate(agg *aicr.CNCFRequirementAggregate) {
	if agg == nil {
		return
	}
	incomplete := agg.IncompleteMusts()
	slog.Info("CNCF requirement aggregation",
		"checklist", agg.Version,
		"applicableMustsPassed", agg.Passed(),
		"incompleteMusts", len(incomplete))
	switch {
	case len(incomplete) == 0:
	case !anyCheckPresent(agg):
		slog.Warn("no conformance check produced a result; every requirement reads not tested",
			"checklist", agg.Version)
	default:
		for _, r := range incomplete {
			slog.Warn("CNCF MUST requirement not established",
				"requirement", r.ID, "status", r.Status, "reason", r.Reason)
		}
	}
}

// anyCheckPresent reports whether the report the aggregate was computed from
// carried a result for any expected check of any requirement.
func anyCheckPresent(agg *aicr.CNCFRequirementAggregate) bool {
	for _, r := range agg.Requirements {
		for _, c := range r.Checks {
			if c.Present {
				return true
			}
		}
	}
	return false
}

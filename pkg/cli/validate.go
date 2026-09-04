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
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
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
func validateFlagCombinations(cncfSubmission bool, evidenceDir string, features []string, noCluster, explicitAttest bool) error {
	if cncfSubmission && evidenceDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--cncf-submission requires --evidence-dir")
	}
	if len(features) > 0 && !cncfSubmission {
		return errors.New(errors.ErrCodeInvalidRequest, "--feature requires --cncf-submission")
	}
	// --cncf-submission deploys GPU workloads and collects behavioral evidence
	// against the active kube-context, so it is incompatible with the offline
	// --no-cluster dry-run. The Action's short-circuit reaches the live-cluster
	// collector directly, bypassing the noCluster handling further down, so
	// reject the combination here rather than silently contacting the cluster.
	if cncfSubmission && noCluster {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--cncf-submission cannot be combined with --no-cluster: the behavioral evidence collector requires a live cluster")
	}
	// An explicit --emit-attestation/--push is a request to sign and
	// (optionally) push an attestation; --no-cluster is an offline dry-run
	// whose checks are all skipped, so the two conflict. Reject the explicit
	// combination rather than warn-and-ignore an explicit CLI flag (repo rule)
	// — consistent with the --cncf-submission guard above. A config-driven
	// spec.validate.evidence.attestation is still silently suppressed in
	// --no-cluster mode by evidenceConfigForRunMode.
	if noCluster && explicitAttest {
		return errors.New(errors.ErrCodeInvalidRequest,
			"--emit-attestation/--push cannot be combined with --no-cluster: an offline dry-run must not sign or push an attestation")
	}
	return nil
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
		RunID:              c.runID,
		NameBase:           validateNameBase,
		OKEAddonsPath:      c.okeAddonsPath,
	}
}

// deployAgentForValidation deploys an agent to capture a snapshot and returns the Snapshot.
// The agent deployer creates the namespace itself (ensureNamespace, with the
// managed-by label) using the same explicit kubeconfig (#1787), so no
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

// validationConfig holds all parameters for a validation run.
type validationConfig struct {
	// Input
	phases []validator.Phase

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

	// Recipe-evidence bundle config; nil disables --emit-attestation work.
	evidence *recipeEvidenceConfig
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
	}
	// Pass phases only when explicitly selected. cfg.phases is nil when the
	// user requested all phases (or used the "all" wildcard), and
	// WithValidationPhases(nil...) would be a no-op anyway — but keeping the
	// option off the slice preserves the exact "run all phases" default path.
	if len(cfg.phases) > 0 {
		facadePhases := make([]aicr.Phase, len(cfg.phases))
		for i, p := range cfg.phases {
			facadePhases[i] = aicr.Phase(p)
		}
		opts = append(opts, aicr.WithValidationPhases(facadePhases...))
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

	// Log per-phase summary
	anyFailed := false
	for _, pr := range results {
		slog.Info("phase result",
			"phase", pr.Phase,
			"status", pr.Status,
			"duration", pr.Duration)
		if ctrf.IsFailingStatus(pr.Status) {
			anyFailed = true
		}
	}

	// If cleanup is disabled, provide helpful debugging info
	if !cfg.cleanup {
		slog.Info("cleanup disabled - Jobs and RBAC kept for debugging",
			"namespace", cfg.validationNamespace,
			"runID", runID)
		slog.Info("to inspect Job logs: kubectl logs -l " + labels.RunID + "=" + runID + " -n " + cfg.validationNamespace)
		slog.Info("to list Jobs: kubectl get jobs -n " + cfg.validationNamespace)
		slog.Info("to cleanup manually: kubectl delete jobs -l app.kubernetes.io/name=aicr -n " + cfg.validationNamespace)
	}

	// Generate conformance evidence if requested.
	if cfg.evidenceDir != "" {
		evidenceCtx, evidenceCancel := context.WithTimeout(ctx, defaults.EvidenceRenderTimeout)
		defer evidenceCancel()

		renderer := cncf.New(cncf.WithOutputDir(cfg.evidenceDir))
		if renderErr := renderer.Render(evidenceCtx, combined); renderErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "evidence rendering failed", renderErr)
		}
		slog.Info("conformance evidence written", "dir", cfg.evidenceDir)
	}

	// Emit even on failure: failed runs document hardware-specific limits.
	// The facade (Client.EmitRecipeEvidence) owns the facade→internal
	// PhaseResult conversion, catalog load (against this Client's data
	// source), and attestation.Emit; the CLI shim only adds the interactive
	// signing-disclosure prompt.
	if cfg.evidence != nil {
		if err := emitRecipeEvidence(ctx, client, rec, snap, results, cfg.evidence); err != nil {
			return err
		}
	}

	if cfg.failOnError && anyFailed {
		return errors.New(errors.ErrCodeInternal, "validation failed: one or more phases did not pass")
	}

	return nil
}

func validateCmdFlags() []cli.Flag {
	return []cli.Flag{
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
		&cli.BoolFlag{
			Name:  "fail-on-error",
			Value: true,
			Usage: "Exit with non-zero status if any phase check reports failed or other " +
				"(crash/OOM/timeout). Does not affect the readiness pre-flight, which always " +
				"fails closed with exit 2 when a prerequisite constraint (e.g. K8s version) is not met",
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
			Value:    "aicr-validation",
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
			Usage:    "ServiceAccount the live snapshot-capture agent runs as. Exact-if-exists: when a ServiceAccount of exactly this name already exists in --namespace it is used verbatim and the agent creates and deletes no RBAC for the run (generate its RBAC manifests with 'aicr snapshot --add-roles-to-service-account', then apply them yourself). Otherwise it is a name prefix and the run ID is appended. Leaving the flag unset is not the same as passing the default shown: an unset value is never probed for existence, and the agent's run-scoped names are derived from the \"aicr-validate\" base instead.",
			Value:    name,
			Category: catAgentDeployment,
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
			Usage:    "Skip removal of Job and RBAC resources on completion (leaves cluster-admin binding active)",
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
			Usage:    "Path to an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump on the local filesystem. When validate captures a live snapshot, the NvidiaGpuPlugin add-on's control-plane state is projected into the K8s oke-addons subtype so profile constraints recorded in OKE recipes can evaluate. The --all flag on the oci command is required: without it removed and non-ACTIVE add-ons are omitted from the dump and would read as absent instead of failing closed. Ignored when --snapshot supplies a pre-captured snapshot.",
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
			Name:     "evidence-dir",
			Usage:    "Write CNCF conformance evidence markdown to this directory. Requires --phase conformance.",
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     "cncf-submission",
			Usage:    "Collect detailed behavioral evidence for CNCF AI Conformance submission. Deploys GPU workloads, captures nvidia-smi output, Prometheus queries, and HPA scaling tests. Requires --evidence-dir. Takes ~15 minutes.",
			Category: catEvidence,
		},
		&cli.StringSliceFlag{
			Name:    "feature",
			Aliases: []string{"f"},
			Usage: "Evidence feature to collect (repeatable, default: all). Only used with --cncf-submission.\n" +
				"Options: " + strings.Join(cncf.ValidFeatures, ", "),
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name: "emit-attestation",
			Usage: `Directory to write a recipe-evidence attestation bundle (v1, or v2 when the recipe carries a configuration profile; signed when --push is set, unless --no-sign).
	Produces summary-bundle/, optionally logs-bundle/, and pointer.yaml suitable for copying to recipes/evidence/<recipe>/<source>/<digest>.yaml (see the emit 'copyTo' hint).
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
	--oidc-device-flow > interactive browser flow.`,
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
		configFlag(),
		dataFlag(),
		outputFlag(),
		kubeconfigFlag(),
	}
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
			if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output", "config", "namespace", "image", "job-name", "service-account-name", "timeout", "data", "evidence-dir", "emit-attestation", "bom", "aks-gpu-pools", "oke-addons", flagPush, flagIdentityToken); err != nil {
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
			features := stringSliceFlagOrConfig(cmd, "feature", cncfOpts.Features)
			// Resolved once here (flag > config) and reused by the guards below and
			// the mode banner further down.
			noCluster := boolFlagOrConfig(cmd, "no-cluster", opts.NoCluster)
			explicitAttest := cmd.IsSet("emit-attestation") || cmd.IsSet(flagPush)

			if err = validateFlagCombinations(cncfSubmission, evidenceDir, features, noCluster, explicitAttest); err != nil {
				return err
			}

			// recipeFilePath is resolved once here (flag > config) so both the
			// --cncf-submission short-circuit below and the normal validation path
			// further down consume the same value, rather than each resolving it
			// independently (which would also double the CLI-overrides-config log
			// line when a flag is set).
			recipeFilePath := stringFlagOrConfig(cmd, "recipe", input.RecipePath)

			// Short-circuit: --cncf-submission bypasses normal validation and runs
			// the behavioral evidence collector directly. When recipe context is
			// available (--recipe or config input), the recipe's GPU allocation
			// policy is resolved and threaded into the collector so the
			// dra-support and secure-access sections are mode-aware (#1629);
			// standalone runs pass no policy and keep capability-driven
			// detection (#1327 contract).
			if cncfSubmission {
				policy, policyErr := resolveCNCFAllocationPolicy(ctx, cmd, cfg, recipeFilePath)
				if policyErr != nil {
					return policyErr
				}
				return runCNCFSubmission(ctx, evidenceDir, features, cmd.String("kubeconfig"), policy)
			}

			phases, err := validator.ParsePhaseSelection(stringSliceFlagOrConfig(cmd, "phase", phaseStringsFallback(opts)))
			if err != nil {
				return err
			}

			snapshotFilePath := stringFlagOrConfig(cmd, "snapshot", input.SnapshotPath)
			warnIgnoredAKSGPUPools(cmd, snapshotFilePath)
			warnIgnoredOKEAddons(cmd, snapshotFilePath)
			kubeconfig := cmd.String("kubeconfig")

			if recipeFilePath == "" {
				return errors.New(errors.ErrCodeInvalidRequest,
					"--recipe is required (or set spec.validate.input.recipe in --config)")
			}

			failOnError := boolFlagOrConfig(cmd, "fail-on-error", derefBoolOr(input.FailOnError, true))
			failFast := boolFlagOrConfig(cmd, "fail-fast", derefBoolOr(opts.FailFast, false))

			// Mode banner: make it explicit whether this run touches a live
			// cluster (issue #1383). --no-cluster is an offline dry-run that
			// reports checks as skipped; otherwise validation deploys
			// validator Jobs against the active kube-context.
			if noCluster {
				slog.Info("validating in --no-cluster mode — offline dry-run; checks are reported as skipped, no cluster is contacted")
			} else {
				slog.Info("validating against the live cluster — validator Jobs will be deployed to the active kube-context")
			}

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
			client, err := recipeClientFromCmd(ctx, cmd, cfg)
			if err != nil {
				return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to initialize data provider")
			}
			defer func() { _ = client.Close() }()

			slog.Info("loading recipe", "uri", recipeFilePath)

			rec, err := client.LoadRecipe(ctx, recipeFilePath, kubeconfig)
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

			if shared.noCleanup {
				slog.Warn("--no-cleanup: cluster-admin ClusterRoleBinding will remain active after validation",
					"namespace", shared.namespace,
					"bindingSelector", "app.kubernetes.io/name=aicr-validator",
					"cleanupHint", "kubectl delete clusterrolebinding -l app.kubernetes.io/name=aicr-validator")
			}

			attOpts, _, err := cfg.EvidenceAttestationOptions()
			if err != nil {
				return err
			}
			evidenceCfg := evidenceConfigForRunMode(noCluster, buildRecipeEvidenceConfig(cmd, attOpts))

			return runValidation(ctx, client, rec, snap, validationConfig{
				phases:                phases,
				runID:                 runID,
				kubeconfig:            kubeconfig,
				output:                cmd.String("output"),
				outFormat:             serializer.FormatJSON,
				failOnError:           failOnError,
				failFast:              failFast,
				validationNamespace:   shared.namespace,
				cleanup:               !shared.noCleanup,
				imagePullSecrets:      shared.imagePullSecrets,
				noCluster:             noCluster,
				nodeSelector:          shared.nodeSelector,
				tolerations:           shared.tolerations,
				imageRegistryOverride: os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"),
				imageTagOverride:      os.Getenv("AICR_VALIDATOR_IMAGE_TAG"),
				evidenceDir:           evidenceDir,
				evidence:              evidenceCfg,
			})
		},
	}
}

// resolveCNCFAllocationPolicy resolves the recipe-configured GPU allocation
// policy for --cncf-submission runs. recipeFilePath is the caller's already
// resolved (flag > config) --recipe value; without one (no --recipe flag and
// no config recipe input) it returns "" — a standalone run keeps the evidence
// script's capability-driven detection, mirroring the #1327 contract ("only
// recipe-less standalone runs select automatically"). With recipe context,
// load/resolution errors fail closed: an invalid allocation configuration
// must not silently collect evidence for the wrong mechanism.
func resolveCNCFAllocationPolicy(ctx context.Context, cmd *cli.Command, cfg *aicr.Config, recipeFilePath string) (string, error) {
	if recipeFilePath == "" {
		return "", nil
	}

	client, err := recipeClientFromCmd(ctx, cmd, cfg)
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to initialize data provider")
	}
	defer func() { _ = client.Close() }()

	slog.Info("loading recipe to resolve the GPU allocation policy for CNCF evidence collection", "uri", recipeFilePath)
	rec, err := client.LoadRecipe(ctx, recipeFilePath, cmd.String("kubeconfig"))
	if err != nil {
		return "", err
	}
	return v1.ResolveGPUAllocationPolicy(ctx, rec.Resolved())
}

// runCNCFSubmission handles --cncf-submission: validates feature names and
// runs the behavioral evidence collector against the live cluster. policy is
// the recipe-resolved GPU allocation policy ("" for standalone runs — see
// resolveCNCFAllocationPolicy).
func runCNCFSubmission(ctx context.Context, evidenceDir string, features []string, kubeconfig, policy string) error {
	// Validate feature names.
	for _, f := range features {
		if !cncf.IsValidFeature(f) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown feature %q; valid features: %s",
					f, strings.Join(cncf.ValidFeatures, ", ")))
		}
	}

	cncfTimeout := defaults.CNCFSubmissionTimeout
	ctx, cancel := context.WithTimeout(ctx, cncfTimeout)
	defer cancel()

	slog.Info("starting CNCF submission evidence collection",
		"evidenceDir", evidenceDir, "features", features, "gpuAllocationPolicy", policy)

	opts := []cncf.CollectorOption{
		cncf.WithFeatures(features),
		cncf.WithKubeconfig(kubeconfig),
	}
	if policy != "" {
		opts = append(opts, cncf.WithAllocationPolicy(policy))
	}
	collector := cncf.NewCollector(evidenceDir, opts...)
	return collector.Run(ctx)
}

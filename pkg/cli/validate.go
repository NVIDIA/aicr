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
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1 "github.com/NVIDIA/aicr/pkg/api/validator/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

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
}

// parseValidateAgentConfig builds the snapshot-capture agent's deployment
// config. Shared inputs (nodeSelector, tolerations, imagePullSecrets,
// namespace, cleanup) are resolved once by the caller and passed in; this
// keeps any CLI-overrides-config slog.Info from firing twice when both
// the agent and the downstream validator job want the same value.
func parseValidateAgentConfig(
	cmd *cli.Command,
	resolved *config.ValidateResolved,
	shared validateSharedResolved,
) *validateAgentConfig {

	return &validateAgentConfig{
		kubeconfig:         cmd.String("kubeconfig"),
		namespace:          shared.namespace,
		image:              stringFlagOrConfig(cmd, "image", resolved.Image),
		imagePullSecrets:   shared.imagePullSecrets,
		jobName:            stringFlagOrConfig(cmd, "job-name", resolved.JobName),
		serviceAccountName: stringFlagOrConfig(cmd, "service-account-name", resolved.ServiceAccountName),
		nodeSelector:       shared.nodeSelector,
		tolerations:        shared.tolerations,
		timeout:            durationFlagOrConfig(cmd, "timeout", resolved.Timeout),
		cleanup:            !shared.noCleanup,
		debug:              cmd.Bool("debug"),
		requireGPU:         boolFlagOrConfig(cmd, "require-gpu", resolved.RequireGPU),
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

// resolveValidateNodeSelector resolves the validation node selector with
// CLI-overrides-config precedence. The CLI flag is a repeated string in
// key=value form; the config value is already a typed map. Either source
// can be empty; the result preserves the same nil-vs-empty semantics.
func resolveValidateNodeSelector(cmd *cli.Command, resolved *config.ValidateResolved) (map[string]string, error) {
	if cmd.IsSet("node-selector") {
		ns, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid node-selector", err)
		}
		if resolved.NodeSelector != nil {
			slog.Info("CLI flag overriding config value", "flag", "node-selector",
				"config", resolved.NodeSelector, "override", ns)
		}
		return ns, nil
	}
	return resolved.NodeSelector, nil
}

// resolveValidateTolerations resolves the validation toleration list,
// preserving the "no --toleration flag" sentinel: snapshotter.ParseTolerations
// returns DefaultTolerations() (a single bare Exists entry that matches every
// taint) when its input is empty, which collapses the implicit default and an
// explicit `--toleration '*'` into the same in-memory value. Validators like
// inference-perf that want to mirror the target node's taints by default
// must distinguish "operator opted into tolerate-all" from "operator said
// nothing". Returning nil here when neither CLI nor config set the field
// keeps the env var unset, so the inner validator context sees nil.
func resolveValidateTolerations(cmd *cli.Command, resolved *config.ValidateResolved) ([]corev1.Toleration, error) {
	if cmd.IsSet("toleration") {
		tols, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", err)
		}
		if resolved.Tolerations != nil {
			slog.Info("CLI flag overriding config value", "flag", "toleration",
				"config", resolved.Tolerations, "override", tols)
		}
		return tols, nil
	}
	return resolved.Tolerations, nil
}

// parseValidationPhases parses phase strings into Phase values, accepting
// the canonical vocabulary in validator.PhaseNames. The validator.PhaseAll
// wildcard collapses the whole selection to nil (= run every phase),
// matching the documented "Default: all phases" behavior. PhaseAll is
// exclusive: combining it with any specific phase is a hard error rather
// than silently treating the selection as wildcard, so a typo like
// `--phase deployment --phase all` does not mask the user's mistake.
//
// Every entry is parsed before the wildcard collapse, so an invalid
// phase name surfaces an error even when "all" is also present.
func parseValidationPhases(phaseStrs []string) ([]validator.Phase, error) {
	if len(phaseStrs) == 0 {
		return nil, nil // nil = all phases
	}

	var (
		sawAll bool
		phases []validator.Phase
		seen   = make(map[validator.Phase]bool)
	)
	for _, s := range phaseStrs {
		if s == validator.PhaseAll {
			sawAll = true
			continue
		}
		p, ok := validator.ParsePhase(s)
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid phase %q: must be one of: %s",
					s, strings.Join(validator.PhaseNames, ", ")))
		}
		if !seen[p] {
			phases = append(phases, p)
			seen[p] = true
		}
	}

	if sawAll && len(phases) > 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("phase %q cannot be combined with other phases", validator.PhaseAll))
	}
	if sawAll {
		return nil, nil
	}
	return phases, nil
}

// validatePhasesAgainstRecipe warns when a requested phase has no checks
// defined in the recipe. The phase will still run but produce 0 tests
// in the CTRF report.
func validatePhasesAgainstRecipe(phases []validator.Phase, rec *recipe.RecipeResult) error {
	if rec.Validation == nil {
		if len(phases) > 0 {
			slog.Warn("recipe has no validation section; requested phases will have no checks",
				"phases", phases)
		}
		return nil
	}

	if len(phases) == 0 {
		return nil
	}

	defined := make(map[validator.Phase]bool)
	if rec.Validation.Deployment != nil && len(rec.Validation.Deployment.Checks) > 0 {
		defined[validator.PhaseDeployment] = true
	}
	if rec.Validation.Performance != nil && len(rec.Validation.Performance.Checks) > 0 {
		defined[validator.PhasePerformance] = true
	}
	if rec.Validation.Conformance != nil && len(rec.Validation.Conformance.Checks) > 0 {
		defined[validator.PhaseConformance] = true
	}

	for _, p := range phases {
		if !defined[p] {
			slog.Warn("phase requested but no checks defined in recipe; phase will be empty",
				"phase", p)
		}
	}

	return nil
}

// deployAgentForValidation deploys an agent to capture a snapshot and returns the Snapshot.
// Creates the namespace if it does not exist.
func deployAgentForValidation(ctx context.Context, cfg *validateAgentConfig) (*snapshotter.Snapshot, string, error) {
	// Ensure namespace exists before deploying the agent Job.
	clientset, _, err := k8sclient.GetKubeClient()
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.namespace}}
	if _, nsErr := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); nsErr != nil {
		if !apierrors.IsAlreadyExists(nsErr) {
			return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", nsErr)
		}
	}

	agentConfig := &snapshotter.AgentConfig{
		Kubeconfig:         cfg.kubeconfig,
		Namespace:          cfg.namespace,
		Image:              cfg.image,
		ImagePullSecrets:   cfg.imagePullSecrets,
		JobName:            cfg.jobName,
		ServiceAccountName: cfg.serviceAccountName,
		NodeSelector:       cfg.nodeSelector,
		Tolerations:        cfg.tolerations,
		Timeout:            cfg.timeout,
		Cleanup:            cfg.cleanup,
		Debug:              cfg.debug,
		Privileged:         true,
		RequireGPU:         cfg.requireGPU,
	}

	snap, err := snapshotter.DeployAndGetSnapshot(ctx, agentConfig)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to capture snapshot", err)
	}

	source := fmt.Sprintf("agent:%s/%s", cfg.namespace, cfg.jobName)
	return snap, source, nil
}

// validationConfig holds all parameters for a validation run.
type validationConfig struct {
	// Input
	phases []validator.Phase

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

	// Behavior
	failOnError bool

	// Evidence
	evidenceDir string

	// Recipe-evidence bundle config; nil disables --emit-attestation work.
	evidence *recipeEvidenceConfig
}

// runValidation runs validation using the container-per-validator engine.
func runValidation(
	ctx context.Context,
	rec *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
	cfg validationConfig,
) error {

	slog.Info("running validation", "phases", cfg.phases)

	v := validator.New(
		validator.WithVersion(version),
		validator.WithCommit(commit),
		validator.WithNamespace(cfg.validationNamespace),
		validator.WithCleanup(cfg.cleanup),
		validator.WithImagePullSecrets(cfg.imagePullSecrets),
		validator.WithNoCluster(cfg.noCluster),
		validator.WithTolerations(cfg.tolerations),
		validator.WithNodeSelector(cfg.nodeSelector),
	)

	validationInput := v1.ToValidationInput(rec)
	results, err := v.ValidatePhases(ctx, cfg.phases, validationInput, snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "validation failed", err)
	}

	// Extract CTRF reports from phase results and merge into a single report.
	reports := make([]*ctrf.Report, 0, len(results))
	for _, pr := range results {
		reports = append(reports, pr.Report)
	}
	combined := ctrf.MergeReports("aicr", version, reports)

	// Serialize combined report
	ser, serErr := serializer.NewFileWriterOrStdout(cfg.outFormat, cfg.output)
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
		if pr.Status == "failed" {
			anyFailed = true
		}
	}

	// If cleanup is disabled, provide helpful debugging info
	if !cfg.cleanup {
		slog.Info("cleanup disabled - Jobs and RBAC kept for debugging",
			"namespace", cfg.validationNamespace,
			"runID", v.RunID)
		slog.Info("to inspect Job logs: kubectl logs -l aicr.nvidia.com/job -n " + cfg.validationNamespace)
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
	if cfg.evidence != nil {
		if err := emitRecipeEvidence(ctx, rec, snap, results, cfg.evidence); err != nil {
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
			Name:     "fail-on-error",
			Value:    true,
			Usage:    "Exit with non-zero status if any check fails validation",
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
		&cli.StringFlag{
			Name:     "job-name",
			Usage:    "Override default Job name",
			Value:    "aicr-validate",
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "service-account-name",
			Usage:    "Override default ServiceAccount name",
			Value:    name,
			Category: catAgentDeployment,
		},
		&cli.StringSliceFlag{
			Name:     "node-selector",
			Usage:    "Override GPU node selection for validation workloads (format: key=value, can be repeated). Replaces platform-specific selectors on inner workloads (e.g., NCCL benchmark pods). Use when GPU nodes have non-standard labels. Does not affect the validator orchestrator Job.",
			Category: catScheduling,
		},
		&cli.StringSliceFlag{
			Name:     "toleration",
			Usage:    "Override tolerations for validation workloads (format: key=value:effect, can be repeated). Replaces the default tolerate-all policy on inner workloads. Does not affect the validator orchestrator Job.",
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
			Usage: `Directory to write a recipe-evidence v1 attestation bundle (signed when --push is set).
	Produces summary-bundle/, optionally logs-bundle/, and pointer.yaml suitable for copying to recipes/evidence/<recipe>.yaml.
	See ADR-007 (docs/design/007-recipe-evidence.md).`,
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
			Name: "push",
			Usage: `OCI registry reference (e.g. ghcr.io/myorg/aicr-evidence) to push the signed summary bundle to.
	Sigstore keyless OIDC signing uses the same precedence chain as ` + "`aicr bundle --attest`" + `:
	--identity-token > COSIGN_IDENTITY_TOKEN env > GitHub Actions ambient OIDC >
	--oidc-device-flow > interactive browser flow.`,
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     "plain-http",
			Usage:    "Use HTTP instead of HTTPS when pushing the evidence OCI artifact (local registry tests).",
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     "insecure-tls",
			Usage:    "Skip TLS verification when pushing the evidence OCI artifact (self-signed registries).",
			Category: catEvidence,
		},
		&cli.StringFlag{
			Name:     "identity-token",
			Usage:    "Pre-fetched OIDC identity token for --push keyless signing. Skips ambient/browser/device-code flows. Prefer COSIGN_IDENTITY_TOKEN on shared hosts; flag values are visible in process listings (ps, /proc/<pid>/cmdline).",
			Sources:  cli.EnvVars("COSIGN_IDENTITY_TOKEN"),
			Category: catEvidence,
		},
		&cli.BoolFlag{
			Name:     "oidc-device-flow",
			Usage:    "Use the OAuth 2.0 device authorization grant for --push OIDC instead of opening a browser callback. Useful on headless hosts when --identity-token / COSIGN_IDENTITY_TOKEN and ambient GitHub Actions OIDC are both unavailable.",
			Sources:  cli.EnvVars("AICR_OIDC_DEVICE_FLOW"),
			Category: catEvidence,
		},
		configFlag(),
		dataFlag(),
		outputFlag(),
		kubeconfigFlag(),
	}
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

Run validation without failing on check errors (informational mode):
  aicr validate -r recipe.yaml -s snapshot.yaml --fail-on-error=false
`,
		Flags: validateCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output", "config", "namespace", "image", "job-name", "service-account-name", "timeout", "data", "evidence-dir", "emit-attestation", "bom", "push", "identity-token"); err != nil {
				return err
			}

			cfg, err := loadCmdConfig(ctx, cmd)
			if err != nil {
				return err
			}
			resolved, err := cfg.Validation().Resolve()
			if err != nil {
				return err
			}

			if initErr := initDataProvider(cmd, cfg); initErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", initErr)
			}

			cncfCfg := resolved.EvidenceCNCF
			if cncfCfg == nil {
				cncfCfg = &config.EvidenceCNCFResolved{}
			}
			evidenceDir := stringFlagOrConfig(cmd, "evidence-dir", cncfCfg.Dir)
			cncfSubmission := boolFlagOrConfig(cmd, "cncf-submission", cncfCfg.CNCFSubmission)
			features := stringSliceFlagOrConfig(cmd, "feature", cncfCfg.Features)

			// Validate flag combinations.
			if cncfSubmission && evidenceDir == "" {
				return errors.New(errors.ErrCodeInvalidRequest, "--cncf-submission requires --evidence-dir")
			}
			if len(features) > 0 && !cncfSubmission {
				return errors.New(errors.ErrCodeInvalidRequest, "--feature requires --cncf-submission")
			}

			// Short-circuit: --cncf-submission bypasses normal validation and runs
			// the behavioral evidence collector directly.
			if cncfSubmission {
				return runCNCFSubmission(ctx, evidenceDir, features, cmd.String("kubeconfig"))
			}

			phases, err := parseValidationPhases(stringSliceFlagOrConfig(cmd, "phase", resolved.Phases))
			if err != nil {
				return err
			}

			recipeFilePath := stringFlagOrConfig(cmd, "recipe", resolved.RecipePath)
			snapshotFilePath := stringFlagOrConfig(cmd, "snapshot", resolved.SnapshotPath)
			kubeconfig := cmd.String("kubeconfig")

			if recipeFilePath == "" {
				return errors.New(errors.ErrCodeInvalidRequest,
					"--recipe is required (or set spec.validate.input.recipe in --config)")
			}

			failOnError := boolFlagOrConfig(cmd, "fail-on-error", derefBoolOr(resolved.FailOnError, true))
			noCluster := boolFlagOrConfig(cmd, "no-cluster", resolved.NoCluster)

			// Resolve shared fields once, before the snapshot/agent split, so
			// CLI-overrides-config log lines fire exactly once per field even
			// when both the agent-deploy path and the validator Job want the
			// same value.
			tolerations, err := resolveValidateTolerations(cmd, resolved)
			if err != nil {
				return err
			}
			nodeSelector, err := resolveValidateNodeSelector(cmd, resolved)
			if err != nil {
				return err
			}
			shared := validateSharedResolved{
				namespace:        stringFlagOrConfig(cmd, "namespace", resolved.Namespace),
				imagePullSecrets: stringSliceFlagOrConfig(cmd, "image-pull-secret", resolved.ImagePullSecrets),
				nodeSelector:     nodeSelector,
				tolerations:      tolerations,
				noCleanup:        boolFlagOrConfig(cmd, "no-cleanup", resolved.NoCleanup),
			}

			slog.Info("loading recipe", "uri", recipeFilePath)

			rec, err := recipe.LoadFromFile(ctx, recipeFilePath, kubeconfig, version)
			if err != nil {
				return err
			}

			var snap *snapshotter.Snapshot

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
				snap, err = serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapshotFilePath, kubeconfig)
				if err != nil {
					return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %q", snapshotFilePath), err)
				}
			} else {
				slog.Info("deploying agent to capture snapshot")

				agentCfg := parseValidateAgentConfig(cmd, resolved, shared)

				var deployErr error
				snap, _, deployErr = deployAgentForValidation(ctx, agentCfg)
				if deployErr != nil {
					return deployErr
				}
			}

			// Validate that requested phases are defined in the recipe.
			if err := validatePhasesAgainstRecipe(phases, rec); err != nil {
				return err
			}

			if shared.noCleanup {
				slog.Warn("--no-cleanup: cluster-admin ClusterRoleBinding will remain active after validation",
					"namespace", shared.namespace,
					"binding", "aicr-validator")
			}

			evidenceCfg := buildRecipeEvidenceConfig(cmd, resolved)

			return runValidation(ctx, rec, snap, validationConfig{
				phases:              phases,
				output:              cmd.String("output"),
				outFormat:           serializer.FormatJSON,
				failOnError:         failOnError,
				validationNamespace: shared.namespace,
				cleanup:             !shared.noCleanup,
				imagePullSecrets:    shared.imagePullSecrets,
				noCluster:           noCluster,
				nodeSelector:        shared.nodeSelector,
				tolerations:         shared.tolerations,
				evidenceDir:         evidenceDir,
				evidence:            evidenceCfg,
			})
		},
	}
}

// runCNCFSubmission handles --cncf-submission: validates feature names and
// runs the behavioral evidence collector against the live cluster.
func runCNCFSubmission(ctx context.Context, evidenceDir string, features []string, kubeconfig string) error {
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
		"evidenceDir", evidenceDir, "features", features)

	collector := cncf.NewCollector(evidenceDir,
		cncf.WithFeatures(features),
		cncf.WithKubeconfig(kubeconfig),
	)
	return collector.Run(ctx)
}

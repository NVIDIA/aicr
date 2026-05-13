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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/urfave/cli/v3"
)

// bundleCmdOptions holds parsed options for the bundle command.
type bundleCmdOptions struct {
	recipeFilePath             string
	outputDir                  string
	kubeconfig                 string
	deployer                   config.DeployerType
	repoURL                    string
	valueOverrides             []config.ComponentPath
	systemNodeSelector         map[string]string
	systemNodeTolerations      []corev1.Toleration
	acceleratedNodeSelector    map[string]string
	acceleratedNodeTolerations []corev1.Toleration
	workloadGateTaint          *corev1.Taint
	workloadSelector           map[string]string
	estimatedNodeCount         int
	storageClass               string
	targetRevision             string

	// dynamicValues declares value paths provided at install time.
	dynamicValues []config.ComponentPath

	// vendorCharts pulls upstream Helm chart bytes into the bundle so
	// the resulting artifact is self-contained and air-gap deployable.
	vendorCharts bool

	// attest enables bundle attestation and binary verification.
	attest bool

	// certificateIdentityRegexp overrides the identity pattern for binary attestation.
	certificateIdentityRegexp string

	// identityToken is a pre-fetched OIDC identity token used for keyless signing.
	// When non-empty it short-circuits all OIDC acquisition flows. Mirrors cosign's
	// --identity-token / COSIGN_IDENTITY_TOKEN.
	identityToken string

	// oidcDeviceFlow opts in to the OAuth 2.0 Device Authorization Grant flow
	// (RFC 8628) for headless hosts where a browser callback is unavailable.
	oidcDeviceFlow bool

	// OCI output reference (nil if outputting to local directory)
	ociRef        *oci.Reference
	plainHTTP     bool
	insecureTLS   bool
	imageRefsPath string // Path to write published image references (like ko --image-refs)
}

// parseBundleCmdOptions parses and validates command options. The wire
// config is converted to a typed *config.BundleResolved up-front via
// (*config.BundleSpec).Resolve so this function only layers CLI flag
// overrides onto already-typed values. All string→typed conversion of
// config-supplied fields happens at the conversion boundary in Resolve;
// errors from that boundary carry "spec.bundle.<path>" attribution.
//
//nolint:gocyclo // option resolution is inherently long but linear
func parseBundleCmdOptions(cmd *cli.Command, cfg *appcfg.AICRConfig) (*bundleCmdOptions, error) {
	resolved, err := cfg.Bundle().Resolve()
	if err != nil {
		return nil, err
	}

	opts := &bundleCmdOptions{
		recipeFilePath:            stringFlagOrConfig(cmd, "recipe", resolved.RecipeInput),
		kubeconfig:                cmd.String("kubeconfig"),
		repoURL:                   stringFlagOrConfig(cmd, "repo", resolved.Repo),
		attest:                    boolFlagOrConfig(cmd, "attest", resolved.Attest),
		vendorCharts:              boolFlagOrConfig(cmd, "vendor-charts", resolved.VendorCharts),
		certificateIdentityRegexp: stringFlagOrConfig(cmd, "certificate-identity-regexp", resolved.CertIDRegexp),
		identityToken:             cmd.String("identity-token"),
		oidcDeviceFlow:            boolFlagOrConfig(cmd, "oidc-device-flow", resolved.OIDCDeviceFlow),
		insecureTLS:               boolFlagOrConfig(cmd, "insecure-tls", resolved.InsecureTLS),
		plainHTTP:                 boolFlagOrConfig(cmd, "plain-http", resolved.PlainHTTP),
		imageRefsPath:             stringFlagOrConfig(cmd, "image-refs", resolved.ImageRefs),
	}

	if opts.recipeFilePath == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"--recipe is required (or set spec.bundle.input.recipe in --config)")
	}

	// Resolve recipe path to absolute and validate it exists early.
	// This prevents confusing "file not found" errors when the working
	// directory context differs from where the recipe was created.
	if opts.recipeFilePath != "" &&
		!strings.HasPrefix(opts.recipeFilePath, "http://") &&
		!strings.HasPrefix(opts.recipeFilePath, "https://") &&
		!strings.HasPrefix(opts.recipeFilePath, serializer.ConfigMapURIScheme) {

		absPath, absErr := filepath.Abs(opts.recipeFilePath)
		if absErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve recipe path", absErr)
		}
		if _, statErr := os.Stat(absPath); statErr != nil {
			if os.IsNotExist(statErr) {
				return nil, errors.New(errors.ErrCodeNotFound, "recipe file not found: "+absPath)
			}
			return nil, errors.Wrap(errors.ErrCodeInternal, "cannot access recipe file: "+absPath, statErr)
		}
		opts.recipeFilePath = absPath
	}

	if opts.deployer, err = resolveDeployer(cmd, resolved.Deployer); err != nil {
		return nil, err
	}

	ref, err := resolveOutputTarget(cmd, resolved)
	if err != nil {
		return nil, err
	}

	if ref.IsOCI {
		// Use CLI version as default tag when not specified in URI
		if ref.Tag == "" {
			opts.ociRef = ref.WithTag(version)
		} else {
			opts.ociRef = ref
		}
		// For OCI output, use current directory for bundle generation
		opts.outputDir = "./bundle"
	} else {
		// Resolve local output path to absolute to ensure consistent behavior
		// regardless of how the binary is invoked.
		absOut, absErr := filepath.Abs(ref.LocalPath)
		if absErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve output path: "+ref.LocalPath, absErr)
		}
		opts.outputDir = absOut
	}

	// When using Argo CD deployer with OCI output and no explicit --repo,
	// auto-populate repoURL from the OCI reference (issue #519).
	if opts.deployer == config.DeployerArgoCD && opts.ociRef != nil && opts.repoURL == "" {
		opts.repoURL = opts.ociRef.Registry + "/" + opts.ociRef.Repository
	}

	// Derive target revision: use OCI tag when available
	if opts.ociRef != nil && opts.ociRef.Tag != "" {
		opts.targetRevision = opts.ociRef.Tag
	}

	if opts.valueOverrides, err = resolveComponentPaths(cmd, "set", resolved.ValueOverrides, config.ParseValueOverrides); err != nil {
		return nil, err
	}
	if opts.dynamicValues, err = resolveComponentPaths(cmd, "dynamic", resolved.DynamicValues, config.ParseDynamicValues); err != nil {
		return nil, err
	}

	if opts.systemNodeSelector, err = resolveNodeSelector(cmd, "system-node-selector", resolved.SystemNodeSelector); err != nil {
		return nil, err
	}
	if opts.acceleratedNodeSelector, err = resolveNodeSelector(cmd, "accelerated-node-selector", resolved.AcceleratedNodeSelector); err != nil {
		return nil, err
	}

	if opts.systemNodeTolerations, err = resolveTolerations(cmd, "system-node-toleration", resolved.SystemNodeTolerations); err != nil {
		return nil, err
	}
	if opts.acceleratedNodeTolerations, err = resolveTolerations(cmd, "accelerated-node-toleration", resolved.AcceleratedNodeTolerations); err != nil {
		return nil, err
	}

	if opts.workloadGateTaint, err = resolveTaint(cmd, "workload-gate", resolved.WorkloadGate); err != nil {
		return nil, err
	}

	if opts.workloadSelector, err = resolveNodeSelector(cmd, "workload-selector", resolved.WorkloadSelector); err != nil {
		return nil, err
	}

	// Estimated node count for bundle; 0 = unset.
	n := intFlagOrConfig(cmd, "nodes", resolved.Nodes)
	if n < 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "--nodes must be >= 0")
	}
	opts.estimatedNodeCount = n

	if cmd.IsSet("storage-class") {
		sc := strings.TrimSpace(cmd.String("storage-class"))
		if sc == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "--storage-class cannot be blank when specified")
		}
		opts.storageClass = sc
	} else if resolved.StorageClass != "" {
		opts.storageClass = resolved.StorageClass
	}

	return opts, nil
}

// resolveOutputTarget returns the parsed *oci.Reference for --output,
// preferring CLI input over the typed fallback in resolved. When the
// CLI flag is set to an empty string OR neither source supplies a
// value, defaults to the current directory ("."). The empty-CLI case
// matches pre-refactor behavior where stringFlagOrConfig surfaced ""
// and the caller substituted "." before parsing.
func resolveOutputTarget(cmd *cli.Command, resolved *appcfg.BundleResolved) (*oci.Reference, error) {
	const flagName = "output"
	if cmd.IsSet(flagName) {
		target := cmd.String(flagName)
		if target == "" {
			target = "."
		}
		if resolved.OutputTargetRaw != "" && resolved.OutputTargetRaw != target {
			slog.Info("CLI flag overriding config value", "flag", flagName,
				"config", resolved.OutputTargetRaw, "override", target)
		}
		ref, err := oci.ParseOutputTarget(target)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --"+flagName+" value", err)
		}
		return ref, nil
	}
	if resolved.OutputTarget != nil {
		return resolved.OutputTarget, nil
	}
	ref, err := oci.ParseOutputTarget(".")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve default output target", err)
	}
	return ref, nil
}

//nolint:funlen // bundle command is inherently large (flags + description + action)
func bundleCmd() *cli.Command {
	return &cli.Command{
		Name:     "bundle",
		Category: functionalCategoryName,
		Usage:    "Generate deployment bundle from a given recipe.",
		Description: `Generates a deployment bundle from a given recipe.
Use --deployer argocd to generate Argo CD Applications.
Use --deployer flux to generate Flux HelmRelease and Kustomization manifests.

Helm:
  - README.md: Root deployment guide with ordered steps
  - deploy.sh: Automation script
  - recipe.yaml: Copy of the input recipe for reference
  - <component>/values.yaml: Helm values per component
  - <component>/README.md: Component install/upgrade/uninstall
  - checksums.txt: SHA256 checksums of generated files

Argo CD:
  - app-of-apps.yaml: Parent Argo CD Application
  - <component>/application.yaml: Argo CD Application per component
  - <component>/values.yaml: Values for each component
  - README.md: Deployment instructions
  - checksums.txt: SHA256 checksums of generated files

Flux:
  - kustomization.yaml: Root Kustomize orchestration
  - namespaces/: Namespace manifests for component namespaces
  - sources/: HelmRepository and GitRepository source CRs
  - <component>/helmrelease.yaml: Flux HelmRelease with inline values
  - README.md: Deployment instructions
  - checksums.txt: SHA256 checksums of generated files

Examples:

Generate Helm per-component bundle (default):
  aicr bundle --recipe recipe.yaml --output ./my-bundle

Generate Argo CD App of Apps:
  aicr bundle --recipe recipe.yaml --output ./my-bundle --deployer argocd

Generate Flux manifests:
  aicr bundle --recipe recipe.yaml --output ./my-bundle --deployer flux

Override values in generated bundle:
  aicr bundle --recipe recipe.yaml --set gpuoperator:driver.version=570.133.20

Set node selectors for GPU workloads:
  aicr bundle --recipe recipe.yaml \
    --accelerated-node-selector nodeGroup=gpu-nodes \
    --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule

Package and push bundle to OCI registry (uses CLI version as tag):
  aicr bundle --recipe recipe.yaml --output oci://ghcr.io/nvidia/aicr-bundle

Package with explicit tag (overrides CLI version):
  aicr bundle --recipe recipe.yaml --output oci://ghcr.io/nvidia/aicr-bundle:v1.0.0
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    cmdNameRecipe,
				Aliases: []string{"r"},
				Usage: `Path/URI to previously generated recipe from which to build the bundle.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	May also be supplied via spec.bundle.input.recipe in --config.`,
				Category: catInput,
			},
			configFlag(),
			&cli.StringFlag{
				Name:    flagOutput,
				Aliases: []string{"o"},
				Usage: `Output target: local directory path or OCI registry URI (default: current dir).
	For local output: ./my-bundle or /tmp/bundle
	For OCI registry: oci://ghcr.io/nvidia/bundle:v1.0.0
	If no tag specified, CLI version is used (e.g., oci://ghcr.io/nvidia/bundle)
	May also be supplied via spec.bundle.output.target in --config.`,
				Category: catOutput,
			},
			&cli.StringSliceFlag{
				Name: "set",
				Usage: `Override values in generated bundle files
	(format: component:path.to.field=value, e.g., --set gpuoperator:gds.enabled=true).
	Use the special 'enabled' key to include/exclude components at bundle time
	(e.g., --set awsebscsidriver:enabled=false to skip aws-ebs-csi-driver)`,
				Category: catDeployment,
			},
			&cli.StringSliceFlag{
				Name: "dynamic",
				Usage: `Declare value paths as install-time parameters
	(format: component:path.to.field, e.g., --dynamic alloy:clusterName).
	Dynamic paths are removed from values.yaml and placed in cluster-values.yaml
	for the user to fill in at install time.`,
				Category: catDeployment,
			},
			&cli.StringSliceFlag{
				Name:     "system-node-selector",
				Usage:    "Node selector for system components (format: key=value, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "system-node-toleration",
				Usage:    "Toleration for system components (format: key=value:effect, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "accelerated-node-selector",
				Usage:    "Node selector for accelerated/GPU nodes (format: key=value, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "accelerated-node-toleration",
				Usage:    "Toleration for accelerated/GPU nodes (format: key=value:effect, can be repeated)",
				Category: catScheduling,
			},
			&cli.StringFlag{
				Name:     "workload-gate",
				Usage:    "Taint for nodewright-operator runtime required (format: key=value:effect or key:effect). This is a day 2 option for cluster scaling operations.",
				Category: catScheduling,
			},
			&cli.StringSliceFlag{
				Name:     "workload-selector",
				Usage:    "Label selector for nodewright-customizations to prevent eviction of running training jobs (format: key=value, can be repeated). Required when nodewright-customizations is enabled with training intent.",
				Category: catScheduling,
			},
			&cli.IntFlag{
				Name:     "nodes",
				Value:    0,
				Usage:    "Estimated number of GPU nodes (written to nodeScheduling.nodeCountPaths in registry). 0 = unset.",
				Category: catScheduling,
			},
			&cli.StringFlag{
				Name:     "storage-class",
				Usage:    "Kubernetes StorageClass name to inject into components at bundle time (written to registry-declared storageClassPaths). Overrides any storageClassName set in recipe overlays.",
				Category: catScheduling,
			},
			withCompletions(&cli.StringFlag{
				Name:     "deployer",
				Aliases:  []string{"d"},
				Usage:    fmt.Sprintf("Deployment method (default: helm; e.g. %s)", strings.Join(config.GetDeployerTypes(), ", ")),
				Category: catDeployment,
			}, config.GetDeployerTypes),
			&cli.StringFlag{
				Name:  "repo",
				Value: "",
				Usage: "Git repository URL for GitOps deployers (used with --deployer argocd and --deployer flux). " +
					"Ignored by --deployer argocd-helm: that bundle is URL-portable and the publish " +
					"location is supplied at install time via `helm install --set repoURL=...`.",
				Category: catDeployment,
			},
			&cli.BoolFlag{
				Name: "vendor-charts",
				Usage: `Pull upstream Helm chart bytes into the bundle at bundle time so the
	resulting artifact eliminates Helm chart registry egress at deploy time.
	Container-image pulls and other non-chart resources may still require
	network access. Each vendored chart is recorded in provenance.yaml with
	name, version, source URL, and SHA256. Trades the upstream CVE-yank
	fail-loud signal for offline deployability — vendored bundles silently
	install a frozen chart version even after upstream yank. Requires the
	'helm' binary on PATH at bundle time.`,
				Category: "Deployment",
			},
			&cli.BoolFlag{
				Name:     "attest",
				Usage:    "Enable bundle attestation and binary provenance verification (requires binary installed via install script; uses OIDC authentication)",
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name: "certificate-identity-regexp",
				Usage: `Override the certificate identity pattern for binary attestation verification.
	Must contain "NVIDIA/aicr". Use for testing with binaries attested by non-release
	workflows (e.g., build-attested.yaml). Not intended for production use.`,
				Category: catDeployment,
			},
			&cli.StringFlag{
				Name:     "identity-token",
				Usage:    "Pre-fetched OIDC identity token for --attest keyless signing. Skips ambient/browser/device-code flows. Prefer COSIGN_IDENTITY_TOKEN on shared hosts; flag values are visible in process listings (ps, /proc/<pid>/cmdline).",
				Sources:  cli.EnvVars("COSIGN_IDENTITY_TOKEN"),
				Category: catDeployment,
			},
			&cli.BoolFlag{
				Name:     "oidc-device-flow",
				Usage:    "Use the OAuth 2.0 device authorization grant for --attest OIDC instead of opening a browser callback. Useful on headless hosts (bastions, remote build boxes) when --identity-token / COSIGN_IDENTITY_TOKEN and ambient GitHub Actions OIDC are both unavailable.",
				Sources:  cli.EnvVars("AICR_OIDC_DEVICE_FLOW"),
				Category: catDeployment,
			},
			kubeconfigFlag(),
			dataFlag(),
			// OCI registry connection flags (used when --output is oci://...)
			&cli.BoolFlag{
				Name:     "insecure-tls",
				Usage:    "Skip TLS certificate verification for OCI registry",
				Category: catOCIRegistry,
			},
			&cli.BoolFlag{
				Name:     "plain-http",
				Usage:    "Use HTTP instead of HTTPS for OCI registry (for local development)",
				Category: catOCIRegistry,
			},
			&cli.StringFlag{
				Name:     "image-refs",
				Usage:    "Path to file where the published image reference will be written (only used with OCI output)",
				Category: catOCIRegistry,
			},
		},
		Action: runBundleCmd,
	}
}

// runBundleCmd is the Action handler for the bundle command.
func runBundleCmd(ctx context.Context, cmd *cli.Command) error {
	// Validate single-value flags are not duplicated
	if err := validateSingleValueFlags(cmd, "recipe", "config", "output", "deployer", "repo", "storage-class"); err != nil {
		return err
	}

	cfg, err := loadCmdConfig(ctx, cmd)
	if err != nil {
		return err
	}

	// Initialize external data provider if --data flag is set
	if err = initDataProvider(cmd, cfg); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", err)
	}

	opts, err := parseBundleCmdOptions(cmd, cfg)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid bundle command options")
	}

	outputType := "Helm per-component bundle"
	switch opts.deployer {
	case config.DeployerHelm:
		// default
	case config.DeployerArgoCD:
		outputType = "Argo CD applications"
	case config.DeployerArgoCDHelm:
		outputType = "Argo CD Helm chart app-of-apps"
	case config.DeployerFlux:
		outputType = "Flux manifests"
	}
	slog.Info("generating bundle",
		slog.String("deployer", opts.deployer.String()),
		slog.String("type", outputType),
		slog.String("recipe", opts.recipeFilePath),
		slog.String("output", opts.outputDir),
		slog.Bool("oci", opts.ociRef != nil),
	)

	// Load recipe from file/URL/ConfigMap; auto-hydrates RecipeMetadata overlays.
	rec, err := recipe.LoadFromFile(ctx, opts.recipeFilePath, opts.kubeconfig, version)
	if err != nil {
		return err
	}

	// Validate custom identity pattern if provided
	if opts.certificateIdentityRegexp != "" {
		if validErr := verifier.ValidateIdentityPattern(opts.certificateIdentityRegexp); validErr != nil {
			return validErr
		}
	}

	// Create bundler with config
	bcfg := config.NewConfig(
		config.WithVersion(version),
		config.WithDeployer(opts.deployer),
		config.WithRepoURL(opts.repoURL),
		config.WithTargetRevision(opts.targetRevision),
		config.WithAttest(opts.attest),
		config.WithCertificateIdentityRegexp(opts.certificateIdentityRegexp),
		config.WithValueOverridePaths(opts.valueOverrides),
		config.WithDynamicValuePaths(opts.dynamicValues),
		config.WithSystemNodeSelector(opts.systemNodeSelector),
		config.WithSystemNodeTolerations(opts.systemNodeTolerations),
		config.WithAcceleratedNodeSelector(opts.acceleratedNodeSelector),
		config.WithAcceleratedNodeTolerations(opts.acceleratedNodeTolerations),
		config.WithWorkloadGateTaint(opts.workloadGateTaint),
		config.WithWorkloadSelector(opts.workloadSelector),
		config.WithEstimatedNodeCount(opts.estimatedNodeCount),
		config.WithStorageClass(opts.storageClass),
		config.WithVendorCharts(opts.vendorCharts),
	)

	// Note: binary attestation pre-flight check is handled by bundler.New().
	attester, err := selectAttester(ctx, opts)
	if err != nil {
		return err
	}

	b, err := bundler.New(
		bundler.WithConfig(bcfg),
		bundler.WithAttester(attester),
	)
	if err != nil {
		slog.Error("failed to create bundler", "error", err)
		return err
	}

	// Generate bundle
	out, err := b.Make(ctx, rec, opts.outputDir)
	if err != nil {
		slog.Error("bundle generation failed", "error", err)
		return err
	}

	slog.Info("bundle generated",
		"type", outputType,
		"files", out.TotalFiles,
		"size_bytes", out.TotalSize,
		"duration_sec", out.TotalDuration.Seconds(),
		"output_dir", out.OutputDir,
	)

	// Print deployment instructions (only for dir output)
	if opts.ociRef == nil && out.Deployment != nil {
		printDeploymentInstructions(cmd.Root().Writer, out)
	}

	// Package and push as OCI artifact when output is oci://
	if opts.ociRef != nil {
		if err := pushOCIBundle(ctx, opts, out); err != nil {
			return err
		}
	}

	return nil
}

// selectAttester wires CLI flags and runtime environment into the
// attestation package's source-precedence resolver. All token-acquisition
// and precedence logic lives in attestation.ResolveAttesterLazy; this
// function only translates pkg/cli surface (flags + env vars + stderr)
// into a ResolveOptions value.
//
// Uses ResolveAttesterLazy so the OIDC token is resolved at first
// Attest() call (which fires during bundler.Make), not at attester
// construction. Fulcio binds the certificate to a fresh nonce at
// token-issue time; resolving early-and-holding can exceed Fulcio's
// tolerance for long bundle runs and fail with "error processing the
// identity token". Matches the deferred-resolve pattern used by
// aicr validate --emit-attestation --push.
//
// On failure it surfaces the "remove --attest to skip" remediation hint via
// slog so headless users who hit a 5-minute device-flow timeout know the
// escape hatch — the underlying error is propagated unchanged so its
// pkg/errors classification (and the resulting CLI exit code) is preserved.
func selectAttester(ctx context.Context, opts *bundleCmdOptions) (attestation.Attester, error) {
	att, err := attestation.ResolveAttesterLazy(ctx, attestation.ResolveOptions{
		Attest:        opts.attest,
		IdentityToken: opts.identityToken,
		AmbientURL:    os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
		AmbientToken:  os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
		DeviceFlow:    opts.oidcDeviceFlow,
		// Prompts (verification URL + user code) go to stderr so they don't
		// pollute stdout when callers redirect bundle output.
		PromptWriter: os.Stderr,
	})
	if err != nil && opts.attest {
		slog.Error("bundle attestation requires authentication; remove --attest to bundle without signing", "error", err)
	}
	return att, err
}

// pushOCIBundle packages and pushes the bundle to an OCI registry.
func pushOCIBundle(ctx context.Context, opts *bundleCmdOptions, out *result.Output) error {
	pushResult, err := oci.PackageAndPush(ctx, oci.OutputConfig{
		SourceDir:   opts.outputDir,
		OutputDir:   opts.outputDir,
		Reference:   opts.ociRef,
		Version:     version,
		PlainHTTP:   opts.plainHTTP,
		InsecureTLS: opts.insecureTLS,
	})
	if err != nil {
		return err
	}

	// Update results with OCI metadata
	for i := range out.Results {
		if out.Results[i].Success {
			out.Results[i].SetOCIMetadata(pushResult.Digest, pushResult.Reference, true)
		}
	}

	// Write image reference to file if --image-refs specified
	if opts.imageRefsPath != "" {
		if err := os.WriteFile(opts.imageRefsPath, []byte(pushResult.Digest+"\n"), 0600); err != nil {
			slog.Error("failed to write image refs file", "error", err, "path", opts.imageRefsPath)
			return errors.Wrap(errors.ErrCodeInternal, "failed to write image refs", err)
		}
		slog.Info("wrote image reference", "path", opts.imageRefsPath, "ref", pushResult.Digest)
	}

	return nil
}

// printDeploymentInstructions writes user-friendly deployment instructions to w.
func printDeploymentInstructions(w io.Writer, out *result.Output) {
	fmt.Fprintf(w, "\n%s generated successfully!\n", out.Deployment.Type)
	fmt.Fprintf(w, "Output directory: %s\n", out.OutputDir)
	fmt.Fprintf(w, "Files generated: %d\n", out.TotalFiles)

	if len(out.Deployment.Notes) > 0 {
		fmt.Fprintln(w, "\nNote:")
		for _, note := range out.Deployment.Notes {
			fmt.Fprintf(w, "  ⚠ %s\n", note)
		}
	}

	if len(out.Deployment.Steps) > 0 {
		fmt.Fprintln(w, "\nTo deploy:")
		for i, step := range out.Deployment.Steps {
			fmt.Fprintf(w, "  %d. %s\n", i+1, step)
		}
	}
}

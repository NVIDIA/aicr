// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/urfave/cli/v3"
)

// bundleCmdOptions holds parsed options for the bundle command.
type bundleCmdOptions struct {
	recipeFilePath             string
	outputDir                  string
	kubeconfig                 string
	deployer                   config.DeployerType
	repoURL                    string
	valueOverrides             map[string]map[string]string
	systemNodeSelector         map[string]string
	systemNodeTolerations      []corev1.Toleration
	acceleratedNodeSelector    map[string]string
	acceleratedNodeTolerations []corev1.Toleration
	workloadGateTaint          *corev1.Taint
	workloadSelector           map[string]string
	estimatedNodeCount         int
	targetRevision             string

	// dynamicValues declares value paths provided at install time.
	dynamicValues map[string][]string

	// attest enables bundle attestation and binary verification.
	attest bool

	// certificateIdentityRegexp overrides the identity pattern for binary attestation.
	certificateIdentityRegexp string

	// OCI output reference (nil if outputting to local directory)
	ociRef        *oci.Reference
	plainHTTP     bool
	insecureTLS   bool
	imageRefsPath string // Path to write published image references (like ko --image-refs)
}

// parseBundleCmdOptions parses and validates command options.
func parseBundleCmdOptions(cmd *cli.Command) (*bundleCmdOptions, error) {
	opts := &bundleCmdOptions{
		recipeFilePath:            cmd.String("recipe"),
		kubeconfig:                cmd.String("kubeconfig"),
		repoURL:                   cmd.String("repo"),
		attest:                    cmd.Bool("attest"),
		certificateIdentityRegexp: cmd.String("certificate-identity-regexp"),
		insecureTLS:               cmd.Bool("insecure-tls"),
		plainHTTP:                 cmd.Bool("plain-http"),
		imageRefsPath:             cmd.String("image-refs"),
	}

	// Resolve recipe path to absolute and validate it exists early.
	// This prevents confusing "file not found" errors when the working
	// directory context differs from where the recipe was created.
	if opts.recipeFilePath != "" &&
		!strings.HasPrefix(opts.recipeFilePath, "http://") &&
		!strings.HasPrefix(opts.recipeFilePath, "https://") &&
		!strings.HasPrefix(opts.recipeFilePath, serializer.ConfigMapURIScheme) {

		absPath, err := filepath.Abs(opts.recipeFilePath)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve recipe path", err)
		}
		if _, err := os.Stat(absPath); err != nil {
			if os.IsNotExist(err) {
				return nil, errors.New(errors.ErrCodeNotFound, "recipe file not found: "+absPath)
			}
			return nil, errors.Wrap(errors.ErrCodeInternal, "cannot access recipe file: "+absPath, err)
		}
		opts.recipeFilePath = absPath
	}

	// Parse and validate deployer flag using strongly-typed parser
	deployerStr := cmd.String("deployer")
	if deployerStr == "" {
		opts.deployer = config.DeployerHelm
	} else {
		deployer, err := config.ParseDeployerType(deployerStr)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --deployer value", err)
		}
		opts.deployer = deployer
	}

	// Parse output target (detects oci:// URI or local directory)
	outputTarget := cmd.String("output")
	ref, err := oci.ParseOutputTarget(outputTarget)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --output value", err)
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

	// Parse value overrides from --set flags
	opts.valueOverrides, err = config.ParseValueOverrides(cmd.StringSlice("set"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --set flag", err)
	}

	// Parse dynamic value declarations from --dynamic flags
	opts.dynamicValues, err = config.ParseDynamicValues(cmd.StringSlice("dynamic"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --dynamic flag", err)
	}

	// Parse node selectors
	opts.systemNodeSelector, err = snapshotter.ParseNodeSelectors(cmd.StringSlice("system-node-selector"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --system-node-selector", err)
	}
	opts.acceleratedNodeSelector, err = snapshotter.ParseNodeSelectors(cmd.StringSlice("accelerated-node-selector"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --accelerated-node-selector", err)
	}

	// Parse tolerations
	opts.systemNodeTolerations, err = snapshotter.ParseTolerations(cmd.StringSlice("system-node-toleration"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --system-node-toleration", err)
	}
	opts.acceleratedNodeTolerations, err = snapshotter.ParseTolerations(cmd.StringSlice("accelerated-node-toleration"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --accelerated-node-toleration", err)
	}

	// Parse workload-gate taint
	workloadGateStr := cmd.String("workload-gate")
	if workloadGateStr != "" {
		opts.workloadGateTaint, err = snapshotter.ParseTaint(workloadGateStr)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --workload-gate", err)
		}
	}

	// Parse workload-selector
	opts.workloadSelector, err = snapshotter.ParseNodeSelectors(cmd.StringSlice("workload-selector"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --workload-selector", err)
	}

	// Parse --nodes (estimated node count for bundle; 0 = unset)
	n := cmd.Int("nodes")
	if n < 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "--nodes must be >= 0")
	}
	opts.estimatedNodeCount = n

	return opts, nil
}

//nolint:funlen // bundle command is inherently large (flags + description + action)
func bundleCmd() *cli.Command {
	return &cli.Command{
		Name:     "bundle",
		Category: functionalCategoryName,
		Usage:    "Generate deployment bundle from a given recipe.",
		Description: `Generates a deployment bundle from a given recipe. 
Use --deployer argocd to generate Argo CD Applications.

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

Examples:

Generate Helm per-component bundle (default):
  aicr bundle --recipe recipe.yaml --output ./my-bundle

Generate Argo CD App of Apps:
  aicr bundle --recipe recipe.yaml --output ./my-bundle --deployer argocd

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
				Name:     "recipe",
				Aliases:  []string{"r"},
				Required: true,
				Usage: `Path/URI to previously generated recipe from which to build the bundle.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).`,
				Category: "Input",
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Value:   ".",
				Usage: `Output target: local directory path or OCI registry URI.
	For local output: ./my-bundle or /tmp/bundle
	For OCI registry: oci://ghcr.io/nvidia/bundle:v1.0.0
	If no tag specified, CLI version is used (e.g., oci://ghcr.io/nvidia/bundle)`,
				Category: "Output",
			},
			&cli.StringSliceFlag{
				Name: "set",
				Usage: `Override values in generated bundle files
	(format: component:path.to.field=value, e.g., --set gpuoperator:gds.enabled=true).
	Use the special 'enabled' key to include/exclude components at bundle time
	(e.g., --set awsebscsidriver:enabled=false to skip aws-ebs-csi-driver)`,
				Category: "Deployment",
			},
			&cli.StringSliceFlag{
				Name: "dynamic",
				Usage: `Declare value paths as install-time parameters
	(format: component:path.to.field, e.g., --dynamic alloy:clusterName).
	Dynamic paths are removed from values.yaml and placed in cluster-values.yaml
	for the user to fill in at install time.`,
				Category: "Deployment",
			},
			&cli.StringSliceFlag{
				Name:     "system-node-selector",
				Usage:    "Node selector for system components (format: key=value, can be repeated)",
				Category: "Scheduling",
			},
			&cli.StringSliceFlag{
				Name:     "system-node-toleration",
				Usage:    "Toleration for system components (format: key=value:effect, can be repeated)",
				Category: "Scheduling",
			},
			&cli.StringSliceFlag{
				Name:     "accelerated-node-selector",
				Usage:    "Node selector for accelerated/GPU nodes (format: key=value, can be repeated)",
				Category: "Scheduling",
			},
			&cli.StringSliceFlag{
				Name:     "accelerated-node-toleration",
				Usage:    "Toleration for accelerated/GPU nodes (format: key=value:effect, can be repeated)",
				Category: "Scheduling",
			},
			&cli.StringFlag{
				Name:     "workload-gate",
				Usage:    "Taint for skyhook-operator runtime required (format: key=value:effect or key:effect). This is a day 2 option for cluster scaling operations.",
				Category: "Scheduling",
			},
			&cli.StringSliceFlag{
				Name:     "workload-selector",
				Usage:    "Label selector for skyhook-customizations to prevent eviction of running training jobs (format: key=value, can be repeated). Required when skyhook-customizations is enabled with training intent.",
				Category: "Scheduling",
			},
			&cli.IntFlag{
				Name:     "nodes",
				Value:    0,
				Usage:    "Estimated number of GPU nodes (written to nodeScheduling.nodeCountPaths in registry). 0 = unset.",
				Category: "Scheduling",
			},
			withCompletions(&cli.StringFlag{
				Name:     "deployer",
				Aliases:  []string{"d"},
				Value:    string(config.DeployerHelm),
				Usage:    fmt.Sprintf("Deployment method (e.g. %s)", strings.Join(config.GetDeployerTypes(), ", ")),
				Category: "Deployment",
			}, config.GetDeployerTypes),
			&cli.StringFlag{
				Name:     "repo",
				Value:    "",
				Usage:    "Git repository URL for Argo CD applications (only used with --deployer argocd)",
				Category: "Deployment",
			},
			&cli.BoolFlag{
				Name:     "attest",
				Usage:    "Enable bundle attestation and binary provenance verification (requires binary installed via install script; uses OIDC authentication)",
				Category: "Deployment",
			},
			&cli.StringFlag{
				Name: "certificate-identity-regexp",
				Usage: `Override the certificate identity pattern for binary attestation verification.
	Must contain "NVIDIA/aicr". Use for testing with binaries attested by non-release
	workflows (e.g., build-attested.yaml). Not intended for production use.`,
				Category: "Deployment",
			},
			kubeconfigFlag,
			dataFlag,
			// OCI registry connection flags (used when --output is oci://...)
			&cli.BoolFlag{
				Name:     "insecure-tls",
				Usage:    "Skip TLS certificate verification for OCI registry",
				Category: "OCI Registry",
			},
			&cli.BoolFlag{
				Name:     "plain-http",
				Usage:    "Use HTTP instead of HTTPS for OCI registry (for local development)",
				Category: "OCI Registry",
			},
			&cli.StringFlag{
				Name:     "image-refs",
				Usage:    "Path to file where the published image reference will be written (only used with OCI output)",
				Category: "OCI Registry",
			},
		},
		Action: runBundleCmd,
	}
}

// runBundleCmd is the Action handler for the bundle command.
func runBundleCmd(ctx context.Context, cmd *cli.Command) error {
	// Validate single-value flags are not duplicated
	if err := validateSingleValueFlags(cmd, "recipe", "output", "deployer", "repo"); err != nil {
		return err
	}

	// Initialize external data provider if --data flag is set
	if err := initDataProvider(cmd); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", err)
	}

	opts, err := parseBundleCmdOptions(cmd)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid bundle command options", err)
	}

	outputType := "Helm per-component bundle"
	switch opts.deployer {
	case config.DeployerHelm:
		// default
	case config.DeployerArgoCD:
		outputType = "Argo CD applications"
	case config.DeployerArgoCDHelm:
		outputType = "Argo CD Helm chart app-of-apps"
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
	cfg := config.NewConfig(
		config.WithVersion(version),
		config.WithDeployer(opts.deployer),
		config.WithRepoURL(opts.repoURL),
		config.WithTargetRevision(opts.targetRevision),
		config.WithAttest(opts.attest),
		config.WithCertificateIdentityRegexp(opts.certificateIdentityRegexp),
		config.WithValueOverrides(opts.valueOverrides),
		config.WithDynamicValues(opts.dynamicValues),
		config.WithSystemNodeSelector(opts.systemNodeSelector),
		config.WithSystemNodeTolerations(opts.systemNodeTolerations),
		config.WithAcceleratedNodeSelector(opts.acceleratedNodeSelector),
		config.WithAcceleratedNodeTolerations(opts.acceleratedNodeTolerations),
		config.WithWorkloadGateTaint(opts.workloadGateTaint),
		config.WithWorkloadSelector(opts.workloadSelector),
		config.WithEstimatedNodeCount(opts.estimatedNodeCount),
	)

	// Pre-flight: verify binary attestation file exists before OIDC auth.
	// selectAttester may open a browser; fail fast if attestation is impossible.
	if opts.attest {
		binaryPath, execErr := os.Executable()
		if execErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				"could not resolve executable path; remove --attest to skip", execErr)
		}
		if _, findErr := attestation.FindBinaryAttestation(binaryPath); findErr != nil {
			return errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("binary attestation not found at %s\n\n"+
					"The --attest flag requires a binary installed using the install script, which\n"+
					"includes a cryptographic attestation from NVIDIA CI. Binaries installed via\n"+
					"\"go install\" or manual download do not include this file.\n\n"+
					"To fix:\n"+
					"  - Reinstall using the install script\n"+
					"  - Or remove --attest to generate bundles without attestation",
					binaryPath+attestation.AttestationFileSuffix))
		}
	}

	attester, err := selectAttester(ctx, opts.attest)
	if err != nil {
		return err
	}

	b, err := bundler.New(
		bundler.WithConfig(cfg),
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
		printDeploymentInstructions(out)
	}

	// Package and push as OCI artifact when output is oci://
	if opts.ociRef != nil {
		if err := pushOCIBundle(ctx, opts, out); err != nil {
			return err
		}
	}

	return nil
}

// selectAttester returns the appropriate attester based on flags and environment.
func selectAttester(ctx context.Context, attest bool) (attestation.Attester, error) {
	if !attest {
		return attestation.NewNoOpAttester(), nil
	}

	// Try ambient OIDC (GitHub Actions)
	requestURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	requestToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if requestURL != "" && requestToken != "" {
		oidcToken, err := attestation.FetchAmbientOIDCToken(ctx, requestURL, requestToken)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeUnavailable,
				"failed to fetch OIDC token for attestation; remove --attest to skip", err)
		}
		return attestation.NewKeylessAttester(oidcToken), nil
	}

	// No ambient OIDC — try interactive browser flow
	slog.Info("no ambient OIDC token, attempting interactive authentication")
	oidcToken, err := attestation.FetchInteractiveOIDCToken(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			"bundle attestation requires authentication; remove --attest to skip", err)
	}
	return attestation.NewKeylessAttester(oidcToken), nil
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

// printDeploymentInstructions prints user-friendly deployment instructions from the deployer.
func printDeploymentInstructions(out *result.Output) {
	fmt.Printf("\n%s generated successfully!\n", out.Deployment.Type)
	fmt.Printf("Output directory: %s\n", out.OutputDir)
	fmt.Printf("Files generated: %d\n", out.TotalFiles)

	if len(out.Deployment.Notes) > 0 {
		fmt.Println("\nNote:")
		for _, note := range out.Deployment.Notes {
			fmt.Printf("  ⚠ %s\n", note)
		}
	}

	if len(out.Deployment.Steps) > 0 {
		fmt.Println("\nTo deploy:")
		for i, step := range out.Deployment.Steps {
			fmt.Printf("  %d. %s\n", i+1, step)
		}
	}
}

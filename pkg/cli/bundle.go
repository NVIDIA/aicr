// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
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

	// OCI output reference (nil if outputting to local directory)
	ociRef        *oci.Reference
	plainHTTP     bool
	insecureTLS   bool
	imageRefsPath string // Path to write published image references (like ko --image-refs)
}

// parseBundleCmdOptions parses and validates command options.
func parseBundleCmdOptions(cmd *cli.Command) (*bundleCmdOptions, error) {
	opts := &bundleCmdOptions{
		recipeFilePath: cmd.String("recipe"),
		kubeconfig:     cmd.String("kubeconfig"),
		repoURL:        cmd.String("repo"),
		insecureTLS:    cmd.Bool("insecure-tls"),
		plainHTTP:      cmd.Bool("plain-http"),
		imageRefsPath:  cmd.String("image-refs"),
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

	// Parse value overrides from --set flags
	opts.valueOverrides, err = config.ParseValueOverrides(cmd.StringSlice("set"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --set flag", err)
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
		Name:                  "bundle",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Generate deployment bundle from a given recipe.",
		Description: `Generates a deployment bundle from a given recipe. 
Use --deployer argocd to generate ArgoCD Applications.

Helm:
  - README.md: Root deployment guide with ordered steps
  - deploy.sh: Automation script
  - recipe.yaml: Copy of the input recipe for reference
  - <component>/values.yaml: Helm values per component
  - <component>/README.md: Component install/upgrade/uninstall
  - checksums.txt: SHA256 checksums of generated files

ArgoCD:
  - app-of-apps.yaml: Parent ArgoCD Application
  - <component>/application.yaml: ArgoCD Application per component
  - <component>/values.yaml: Values for each component
  - README.md: Deployment instructions
  - checksums.txt: SHA256 checksums of generated files

Examples:

Generate Helm per-component bundle (default):
  aicr bundle --recipe recipe.yaml --output ./my-bundle

Generate ArgoCD App of Apps:
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
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Value:   ".",
				Usage: `Output target: local directory path or OCI registry URI.
	For local output: ./my-bundle or /tmp/bundle
	For OCI registry: oci://ghcr.io/nvidia/bundle:v1.0.0
	If no tag specified, CLI version is used (e.g., oci://ghcr.io/nvidia/bundle)`,
			},
			&cli.StringSliceFlag{
				Name: "set",
				Usage: `Override values in generated bundle files 
	(format: bundler:path.to.field=value, e.g., --set gpuoperator:gds.enabled=true)`,
			},
			&cli.StringSliceFlag{
				Name:  "system-node-selector",
				Usage: "Node selector for system components (format: key=value, can be repeated)",
			},
			&cli.StringSliceFlag{
				Name:  "system-node-toleration",
				Usage: "Toleration for system components (format: key=value:effect, can be repeated)",
			},
			&cli.StringSliceFlag{
				Name:  "accelerated-node-selector",
				Usage: "Node selector for accelerated/GPU nodes (format: key=value, can be repeated)",
			},
			&cli.StringSliceFlag{
				Name:  "accelerated-node-toleration",
				Usage: "Toleration for accelerated/GPU nodes (format: key=value:effect, can be repeated)",
			},
			&cli.StringFlag{
				Name:  "workload-gate",
				Usage: "Taint for skyhook-operator runtime required (format: key=value:effect or key:effect). This is a day 2 option for cluster scaling operations.",
			},
			&cli.StringSliceFlag{
				Name:  "workload-selector",
				Usage: "Label selector for skyhook-customizations to prevent eviction of running training jobs (format: key=value, can be repeated). Required when skyhook-customizations is enabled with training intent.",
			},
			&cli.IntFlag{
				Name:  "nodes",
				Value: 0,
				Usage: "Estimated number of GPU nodes (written to nodeScheduling.nodeCountPaths in registry). 0 = unset.",
			},
			&cli.StringFlag{
				Name:    "deployer",
				Aliases: []string{"d"},
				Value:   string(config.DeployerHelm),
				Usage:   fmt.Sprintf("Deployment method (e.g. %s)", strings.Join(config.GetDeployerTypes(), ", ")),
			},
			&cli.StringFlag{
				Name:  "repo",
				Value: "",
				Usage: "Git repository URL for ArgoCD applications (only used with --deployer argocd)",
			},
			kubeconfigFlag,
			dataFlag,
			// OCI registry connection flags (used when --output is oci://...)
			&cli.BoolFlag{
				Name:  "insecure-tls",
				Usage: "Skip TLS certificate verification for OCI registry",
			},
			&cli.BoolFlag{
				Name:  "plain-http",
				Usage: "Use HTTP instead of HTTPS for OCI registry (for local development)",
			},
			&cli.StringFlag{
				Name:  "image-refs",
				Usage: "Path to file where the published image reference will be written (only used with OCI output)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
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
			if opts.deployer == config.DeployerArgoCD {
				outputType = "ArgoCD applications"
			}
			slog.Info("generating bundle",
				slog.String("deployer", opts.deployer.String()),
				slog.String("type", outputType),
				slog.String("recipe", opts.recipeFilePath),
				slog.String("output", opts.outputDir),
				slog.Bool("oci", opts.ociRef != nil),
			)

			// Load recipe from file/URL/ConfigMap
			rec, err := serializer.FromFileWithKubeconfig[recipe.RecipeResult](opts.recipeFilePath, opts.kubeconfig)
			if err != nil {
				slog.Error("failed to load recipe file", "error", err, "path", opts.recipeFilePath)
				return errors.Wrap(errors.ErrCodeInternal, "failed to load recipe file", err)
			}

			// Create bundler with config
			cfg := config.NewConfig(
				config.WithVersion(version),
				config.WithDeployer(opts.deployer),
				config.WithRepoURL(opts.repoURL),
				config.WithValueOverrides(opts.valueOverrides),
				config.WithSystemNodeSelector(opts.systemNodeSelector),
				config.WithSystemNodeTolerations(opts.systemNodeTolerations),
				config.WithAcceleratedNodeSelector(opts.acceleratedNodeSelector),
				config.WithAcceleratedNodeTolerations(opts.acceleratedNodeTolerations),
				config.WithWorkloadGateTaint(opts.workloadGateTaint),
				config.WithWorkloadSelector(opts.workloadSelector),
				config.WithEstimatedNodeCount(opts.estimatedNodeCount),
			)

			b, err := bundler.NewWithConfig(cfg)
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
		},
	}
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

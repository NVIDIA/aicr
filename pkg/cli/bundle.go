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
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/eidos/pkg/bundler"
	"github.com/NVIDIA/eidos/pkg/bundler/config"
	"github.com/NVIDIA/eidos/pkg/bundler/result"
	"github.com/NVIDIA/eidos/pkg/oci"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
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

	// Parse and validate deployer flag using strongly-typed parser
	deployerStr := cmd.String("deployer")
	if deployerStr == "" {
		opts.deployer = config.DeployerHelm
	} else {
		deployer, err := config.ParseDeployerType(deployerStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --deployer value: %w", err)
		}
		opts.deployer = deployer
	}

	// Parse output target (detects oci:// URI or local directory)
	outputTarget := cmd.String("output")
	ref, err := oci.ParseOutputTarget(outputTarget)
	if err != nil {
		return nil, fmt.Errorf("invalid --output value: %w", err)
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
		opts.outputDir = ref.LocalPath
	}

	// Parse value overrides from --set flags
	opts.valueOverrides, err = config.ParseValueOverrides(cmd.StringSlice("set"))
	if err != nil {
		return nil, fmt.Errorf("invalid --set flag: %w", err)
	}

	// Parse node selectors
	opts.systemNodeSelector, err = snapshotter.ParseNodeSelectors(cmd.StringSlice("system-node-selector"))
	if err != nil {
		return nil, fmt.Errorf("invalid --system-node-selector: %w", err)
	}
	opts.acceleratedNodeSelector, err = snapshotter.ParseNodeSelectors(cmd.StringSlice("accelerated-node-selector"))
	if err != nil {
		return nil, fmt.Errorf("invalid --accelerated-node-selector: %w", err)
	}

	// Parse tolerations
	opts.systemNodeTolerations, err = snapshotter.ParseTolerations(cmd.StringSlice("system-node-toleration"))
	if err != nil {
		return nil, fmt.Errorf("invalid --system-node-toleration: %w", err)
	}
	opts.acceleratedNodeTolerations, err = snapshotter.ParseTolerations(cmd.StringSlice("accelerated-node-toleration"))
	if err != nil {
		return nil, fmt.Errorf("invalid --accelerated-node-toleration: %w", err)
	}

	return opts, nil
}

func bundleCmd() *cli.Command {
	return &cli.Command{
		Name:                  "bundle",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Generate deployment bundle from a given recipe.",
		Description: `Generates a deployment bundle from a given recipe. 
Use --deployer argocd to generate ArgoCD Applications.

Helm:
  - Chart.yaml: Helm chart metadata with component dependencies
  - values.yaml: Combined values for all components
  - README.md: Deployment instructions
  - recipe.yaml: Copy of the input recipe for reference
  - checksums.txt: SHA256 checksums of generated files

ArgoCD:
  - app-of-apps.yaml: Parent ArgoCD Application
  - <component>/application.yaml: ArgoCD Application per component
  - <component>/values.yaml: Values for each component
  - README.md: Deployment instructions
  - checksums.txt: SHA256 checksums of generated files

Examples:

Generate Helm umbrella chart (default):
  eidos bundle --recipe recipe.yaml --output ./my-bundle

Generate ArgoCD App of Apps:
  eidos bundle --recipe recipe.yaml --output ./my-bundle --deployer argocd

Override values in generated bundle:
  eidos bundle --recipe recipe.yaml --set gpuoperator:driver.version=570.133.20

Set node selectors for GPU workloads:
  eidos bundle --recipe recipe.yaml \
    --accelerated-node-selector nodeGroup=gpu-nodes \
    --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule

Package and push bundle to OCI registry (uses CLI version as tag):
  eidos bundle --recipe recipe.yaml --output oci://ghcr.io/nvidia/eidos-bundle

Package with explicit tag (overrides CLI version):
  eidos bundle --recipe recipe.yaml --output oci://ghcr.io/nvidia/eidos-bundle:v1.0.0
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
				return fmt.Errorf("failed to initialize data provider: %w", err)
			}

			opts, err := parseBundleCmdOptions(cmd)
			if err != nil {
				return err
			}

			outputType := "Helm umbrella chart"
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
				return err
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
			return fmt.Errorf("failed to write image refs: %w", err)
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

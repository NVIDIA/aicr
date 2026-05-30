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

/*
Package bundler provides orchestration for generating deployment bundles from recipes.

The bundler package generates deployment-ready artifacts (Helm per-component bundles,
Argo CD applications, Flux HelmRelease manifests, helmfile release graphs, or rendered
local manifests) from recipe configurations. Component configuration is loaded from
the declarative component registry (recipes/registry.yaml).

# Architecture

  - DefaultBundler: Orchestrates bundle generation; the concrete output is produced by a deployer
  - Component Registry: Declarative configuration in recipes/registry.yaml
  - Deployers: helm (default), argocd, argocd-helm, flux, helmfile, localformat
  - result.Output: Aggregated generation results

# Quick Start

	b, err := bundler.New()
	output, err := b.Make(ctx, recipeResult, "./bundle")
	fmt.Printf("Generated: %d files\n", output.TotalFiles)

With options:

	cfg := config.NewConfig(
	    config.WithDeployer(config.DeployerHelm),
	    config.WithIncludeChecksums(true),
	)
	b, err := bundler.New(bundler.WithConfig(cfg))

# Supported Components

Components are defined declaratively in recipes/registry.yaml. The set evolves
without Go changes; representative entries include gpu-operator, network-operator,
nvidia-dra-driver-gpu, cert-manager, nfd, nvsentinel, nodewright-operator,
kube-prometheus-stack, dynamo-platform, kueue, kubeflow-trainer, and the slinky
slurm components. See recipes/registry.yaml for the current authoritative list.

# Output Formats

Helm (default):
  - README.md: Root deployment guide with ordered steps
  - deploy.sh: Automation script (0755)
  - recipe.yaml: Copy of the input recipe
  - NNN-<component>/install.sh: Per-folder install script
  - NNN-<component>/values.yaml: Static Helm values
  - NNN-<component>/cluster-values.yaml: Per-cluster dynamic values
  - NNN-<component>/upstream.env: CHART/REPO/VERSION (upstream-helm folders)
  - NNN-<component>/Chart.yaml + templates/: Local chart (local-helm folders)

Argo CD:
  - app-of-apps.yaml: Parent Argo CD Application
  - <component>/application.yaml: Argo CD Application per component
  - <component>/values.yaml: Values for each component

# Configuration

	cfg := config.NewConfig(
	    config.WithDeployer(config.DeployerHelm),
	    config.WithIncludeReadme(true),
	    config.WithSystemNodeSelector(map[string]string{"node-role": "system"}),
	)
	b, err := bundler.New(bundler.WithConfig(cfg))

# Adding New Components

To add a new component, add an entry to recipes/registry.yaml.
No Go code is required.

Helm Component Example:

  - name: my-component
    displayName: My Component
    valueOverrideKeys: [mycomponent]
    helm:
    defaultRepository: https://charts.example.com
    defaultChart: example/my-component
    nodeScheduling:
    system:
    nodeSelectorPaths: [operator.nodeSelector]

Kustomize Component Example:

  - name: my-kustomize-app
    displayName: My Kustomize App
    valueOverrideKeys: [mykustomize]
    kustomize:
    defaultSource: https://github.com/example/my-app
    defaultPath: deploy/production
    defaultTag: v1.0.0

Note: A component must have either 'helm' OR 'kustomize' configuration, not both.

See https://github.com/NVIDIA/aicr for more information.
*/
package bundler

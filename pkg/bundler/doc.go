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

/*
Package bundler provides orchestration for generating deployment bundles from recipes.

The bundler package generates deployment-ready artifacts (Helm per-component bundles or
Argo CD applications) from recipe configurations. Component configuration is loaded
from the declarative component registry (recipes/registry.yaml).

# Architecture

  - DefaultBundler: Generates Helm per-component bundles or Argo CD applications
  - Component Registry: Declarative configuration in recipes/registry.yaml
  - Deployers: Helm (default) and Argo CD output formats
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

Components are defined in recipes/registry.yaml:

  - gpu-operator: NVIDIA GPU Operator
  - network-operator: NVIDIA Network Operator
  - nvidia-dra-driver-gpu: NVIDIA DRA Driver
  - cert-manager: Certificate Manager
  - nvsentinel: NVSentinel
  - skyhook-operator: Skyhook node optimization

# Output Formats

Helm (default):
  - README.md: Root deployment guide with ordered steps
  - deploy.sh: Automation script (0755)
  - recipe.yaml: Copy of the input recipe
  - <component>/values.yaml: Helm values per component
  - <component>/README.md: Component install/upgrade/uninstall
  - <component>/manifests/: Optional manifest files

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

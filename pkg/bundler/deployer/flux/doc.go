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
Package flux provides Flux manifest generation for AICR recipes.

The flux package generates Flux custom resources from RecipeResult objects,
enabling GitOps-based deployment of GPU-accelerated infrastructure components
using the Flux toolkit.

# Overview

The package generates:
  - HelmRelease CRs for all components (helm.toolkit.fluxcd.io/v2)
  - HelmRepository source CRs for upstream chart repositories (source.toolkit.fluxcd.io/v1)
  - GitRepository source CRs for local Helm chart sources (source.toolkit.fluxcd.io/v1)
  - Local Helm charts (Chart.yaml + templates/) for manifest-only components
  - A root kustomization.yaml (plain Kustomize) that orchestrates all resources
  - README with deployment instructions

# Deployment Ordering

Components are deployed in order using Flux dependsOn references. The deployment
order is determined by the recipe's DeploymentOrder field. Each component depends
on the component immediately preceding it in the order.

# Source Deduplication

Multiple components sharing the same upstream repository (e.g., two charts from
the same Helm repo) produce a single HelmRepository source CR.

# OCI Support

OCI-based Helm repositories (prefixed with oci://) generate HelmRepository CRs
with spec.type set to "oci". HTTPS repositories use the default type.

# Component Type Support

Only Helm components (type "helm") are currently supported. Kustomize
components produce an ErrCodeInvalidRequest error at generation time.

# Usage

	generator := &flux.Generator{
		RecipeResult:     recipeResult,
		ComponentValues:  componentValues,
		Version:          "v0.9.0",
		RepoURL:          "https://github.com/my-org/my-gitops-repo.git",
		IncludeChecksums: true,
	}

	output, err := generator.Generate(ctx, "/path/to/output")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Generated %d files (%d bytes)\n", len(output.Files), output.TotalSize)

# Generated Structure

	output/
	├── kustomization.yaml              # Root Kustomize orchestration
	├── README.md                       # Deployment instructions
	├── checksums.txt                   # SHA256 checksums (optional)
	├── sources/
	│   ├── gitrepo-<name>.yaml         # GitRepository (for local Helm charts)
	│   ├── helmrepo-charts-jetstack-io.yaml
	│   └── helmrepo-helm-ngc-nvidia-com-nvidia.yaml
	├── cert-manager/
	│   └── helmrelease.yaml            # HelmRelease (HelmRepository source)
	├── gpu-operator/
	│   └── helmrelease.yaml            # HelmRelease (HelmRepository source)
	├── gpu-operator-post/
	│   ├── Chart.yaml                  # Local Helm chart for manifest templates
	│   ├── helmrelease.yaml            # HelmRelease (GitRepository source)
	│   └── templates/
	│       └── dcgm-exporter.yaml      # Manifest template rendered by Helm controller
	└── nodewright-customizations/
	    ├── Chart.yaml                  # Local Helm chart for manifest templates
	    ├── helmrelease.yaml            # HelmRelease (GitRepository source)
	    └── templates/
	        └── tuning.yaml             # Manifest template rendered by Helm controller
*/
package flux

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
Package argocd provides Argo CD Application generation for Cloud Native Stack recipes.

The argocd package generates Argo CD Application manifests from RecipeResult objects,
enabling GitOps-based deployment of GPU-accelerated infrastructure components.

# Overview

The package supports the App of Apps pattern, generating:
  - Individual Application manifests for each component
  - An app-of-apps.yaml manifest that manages all applications
  - Values files for Helm chart configuration
  - README with deployment instructions

# Deployment Ordering

Components are deployed in order using Argo CD sync-waves. The deployment order
is determined by the recipe's DeploymentOrder field. Components are assigned
sync-wave annotations starting from 0.

# Usage

	generator := &argocd.Generator{
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
	├── app-of-apps.yaml           # Parent application
	├── README.md                  # Deployment instructions
	├── checksums.txt              # SHA256 checksums (optional)
	├── cert-manager/
	│   ├── application.yaml       # Argo CD Application (sync-wave: 0)
	│   └── values.yaml
	├── gpu-operator/
	│   ├── application.yaml       # Argo CD Application (sync-wave: 1)
	│   └── values.yaml
	└── network-operator/
	    ├── application.yaml       # Argo CD Application (sync-wave: 2)
	    └── values.yaml

# Configuration

The RepoURL field in Generator sets the Git repository URL in the
app-of-apps.yaml manifest. If not provided, a placeholder URL is used
that must be updated manually before deployment.
*/
package argocd

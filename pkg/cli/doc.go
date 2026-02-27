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

// Package cli implements the command-line interface for the AICR aicr tool.
//
// # Overview
//
// The aicr CLI provides commands for the four-stage workflow: capturing system snapshots,
// generating configuration recipes, validating constraints, and creating deployment bundles.
// It is designed for cluster administrators and SREs managing NVIDIA GPU infrastructure.
//
// # Commands
//
// snapshot - Capture system configuration (Step 1):
//
//	aicr snapshot [--output FILE] [--format yaml|json|table]
//	aicr snapshot --output cm://namespace/configmap-name  # ConfigMap output
//	aicr snapshot --namespace my-namespace                  # Custom namespace
//
// Captures a comprehensive snapshot of the current system including CPU/GPU settings,
// kernel parameters, systemd services, Kubernetes configuration, Helm releases, and
// ArgoCD applications. Supports file, stdout, and Kubernetes ConfigMap output.
//
// recipe - Generate configuration recipes (Step 2):
//
//	aicr recipe --os ubuntu --osv 24.04 --service eks --gpu h100 --intent training
//	aicr recipe --snapshot system.yaml --intent inference --output recipe.yaml
//	aicr recipe -s cm://namespace/snapshot -o cm://namespace/recipe  # ConfigMap I/O
//	aicr recipe --criteria criteria.yaml --output recipe.yaml  # Criteria file mode
//
// Generates optimized configuration recipes based on either:
//   - Specified environment parameters (OS, service, GPU, intent)
//   - Existing system snapshot (analyzes snapshot to extract parameters)
//   - Criteria file (Kubernetes-style YAML/JSON with kind: RecipeCriteria)
//
// # Criteria File Mode
//
// The --criteria/-c flag allows defining criteria in a Kubernetes-style
// resource file instead of individual CLI flags:
//
//	aicr recipe --criteria /path/to/criteria.yaml
//
// Criteria file format (YAML or JSON):
//
//	kind: RecipeCriteria
//	apiVersion: aicr.nvidia.com/v1alpha1
//	metadata:
//	  name: my-deployment-criteria
//	spec:
//	  service: eks
//	  accelerator: gb200
//	  os: ubuntu
//	  intent: training
//	  nodes: 8
//
// Individual CLI flags override criteria file values:
//
//	aicr recipe --criteria criteria.yaml --service gke  # service=gke overrides file
//
// validate - Validate recipe constraints (Step 3):
//
//	aicr validate --recipe recipe.yaml --snapshot snapshot.yaml
//	aicr validate -r recipe.yaml -s cm://default/aicr-snapshot
//	aicr validate -r recipe.yaml -s cm://ns/snapshot --fail-on-error
//
// Validates recipe constraints against actual measurements from a snapshot.
// Supports version comparisons (>=, <=, >, <), equality (==, !=), and exact match.
// Use --fail-on-error for CI/CD pipelines (non-zero exit on failures).
//
// bundle - Create deployment bundles (Step 4):
//
//	aicr bundle --recipe recipe.yaml --output ./bundles
//	aicr bundle -r recipe.yaml --deployer argocd -o ./bundles
//	aicr bundle -r recipe.yaml --set gpuoperator:driver.version=580.86.16
//
// Generates deployment artifacts from recipes. By default creates a Helm
// per-component bundle with individual values.yaml per component. Use
// --deployer argocd for ArgoCD Application manifests.
//
// # Global Flags
//
//	--output, -o   Output file path (default: stdout)
//	--format, -t   Output format: yaml, json, table (default: yaml)
//	--debug        Enable debug logging
//	--log-json     Output logs in JSON format
//	--help, -h     Show command help
//	--version, -v  Show version information
//
// # Output Formats
//
// YAML (default):
//   - Human-readable, preserves structure
//   - Suitable for version control
//
// JSON:
//   - Machine-parseable, compact
//   - Suitable for programmatic consumption
//
// Table:
//   - Hierarchical text representation
//   - Suitable for terminal viewing
//
// # Usage Examples
//
// Complete workflow:
//
//	aicr snapshot --output snapshot.yaml
//	aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml
//	aicr validate --recipe recipe.yaml --snapshot snapshot.yaml
//	aicr bundle --recipe recipe.yaml --output ./bundles
//
// ConfigMap-based workflow:
//
//	aicr snapshot -o cm://default/aicr-snapshot
//	aicr recipe -s cm://default/aicr-snapshot -o cm://default/aicr-recipe
//	aicr validate -r cm://default/aicr-recipe -s cm://default/aicr-snapshot
//	aicr bundle -r cm://default/aicr-recipe -o ./bundles
//
// Generate recipe for Ubuntu 24.04 on EKS with H100 GPUs:
//
//	aicr recipe --os ubuntu --osv 24.04 --service eks --gpu h100 --intent training
//
// Override bundle values at generation time:
//
//	aicr bundle -r recipe.yaml --set gpuoperator:gds.enabled=true -o ./bundles
//
// # Environment Variables
//
//	LOG_LEVEL              Set logging verbosity (debug, info, warn, error)
//	NODE_NAME              Override node name for Kubernetes collection
//	KUBERNETES_NODE_NAME   Fallback node name if NODE_NAME not set
//	HOSTNAME               Final fallback for node name
//	KUBECONFIG             Path to kubeconfig file
//
// # Exit Codes
//
//	0  Success
//	1  General error (invalid arguments, execution failure)
//	2  Context canceled or timeout
//
// # Architecture
//
// The CLI uses the urfave/cli/v3 framework and delegates to specialized packages:
//   - pkg/snapshotter - System snapshot collection
//   - pkg/recipe - Recipe generation from queries or snapshots
//   - pkg/bundler - Bundle orchestration and generation
//   - pkg/component - Individual bundler implementations
//   - pkg/serializer - Output formatting (including ConfigMap)
//   - pkg/logging - Structured logging
//
// Version information is embedded at build time using ldflags:
//
//	go build -ldflags="-X 'github.com/NVIDIA/aicr/pkg/cli.version=1.0.0'"
package cli

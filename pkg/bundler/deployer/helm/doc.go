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

// Package helm generates per-component Helm bundles from recipe results.
//
// Per-component folder layout (NNN-prefixed, written by pkg/bundler/deployer/localformat):
//
//   - NNN-<component>/install.sh: Per-folder install script
//   - NNN-<component>/values.yaml: Static Helm values
//   - NNN-<component>/cluster-values.yaml: Per-cluster dynamic values
//   - NNN-<component>/upstream.env: CHART/REPO/VERSION (upstream-helm folders)
//   - NNN-<component>/Chart.yaml + templates/: Local chart (local-helm folders)
//
// Top-level files (owned by this deployer):
//
//   - README.md: Root deployment guide with ordered steps
//   - deploy.sh: Automation script (0755)
//   - undeploy.sh: Reverse-order uninstall script (0755)
//   - checksums.txt: SHA256 digests for verification (optional)
//
// Usage:
//
//	generator := &helm.Generator{
//	    RecipeResult:     recipeResult,
//	    ComponentValues:  componentValues,
//	    Version:          "1.0.0",
//	    IncludeChecksums: true,
//	}
//	output, err := generator.Generate(ctx, "/path/to/output")
package helm

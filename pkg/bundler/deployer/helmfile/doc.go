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

// Package helmfile generates a helmfile.yaml release graph from a configured
// recipe. The bundler emits the release graph as data — operators drive
// rollouts with the upstream `helmfile` CLI (apply / diff / destroy) instead
// of an AICR-emitted bash wrapper.
//
// # Output shape
//
//	output/
//	  helmfile.yaml             # release graph + repositories + helmDefaults
//	  README.md                 # user-facing apply/diff/destroy walkthrough
//	  001-cert-manager/         # per-component dirs from localformat.Write()
//	    values.yaml
//	    cluster-values.yaml     # only when component has DynamicValues
//	    upstream.env            # KindUpstreamHelm; chart pulled at apply time
//	  002-gpu-operator/         # KindLocalHelm (kustomize/manifest-only/vendored)
//	    Chart.yaml
//	    templates/...
//	    values.yaml
//
// # Mapping from AICR concepts to helmfile primitives
//
//   - DeploymentOrder        → release.needs (immediate predecessor)
//   - Helm-typed component   → release pointing at upstream <alias>/<chart> +
//     a repositories: entry for the chart's source URL
//   - KindLocalHelm folder   → release with chart: ./<NNN-name>
//     (wrapping covers manifest-only, kustomize, vendored, and mixed-post
//     components; the helmfile deployer treats them uniformly)
//   - DynamicValues[name]    → extra values: entry referencing
//     cluster-values.yaml so operators can edit it pre-apply
//   - ASYNC_COMPONENTS list  → per-release wait: false
//     (mirrored from pkg/bundler/deployer/helm/templates/deploy.sh.tmpl;
//     promoting these to the recipe schema is tracked as a follow-up to
//     #632)
//   - Component-specific helm timeout (e.g., kai-scheduler 20m) →
//     per-release timeout: <seconds>
//
// # Non-goals (per issue #632)
//
//   - No deploy.sh / undeploy.sh emitted alongside helmfile.yaml.
//   - No prepare:/presync:/postsync:/postuninstall: hooks emitted.
//   - No replication of helm-deployer pre-flight or undeploy cleanup —
//     `helmfile destroy` is the simple graceful-uninstall primitive.
//
// Cluster pre-flight, finalizer scrubbing, and scorched-earth cleanup are
// operator-owned concerns; see #632 for the rationale.
package helmfile

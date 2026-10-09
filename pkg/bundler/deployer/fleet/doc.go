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
Package fleet generates a Rancher Fleet bundle from AICR recipes, as a GitRepo
(ModeGitRepo, the default) or as HelmOps (ModeHelmOp).

Per-component folders are written by pkg/bundler/deployer/localformat (the
same NNN-<component>/ layout the helm and helmfile deployers use). In GitRepo
mode this package adds the Fleet-specific files on top:

  - NNN-<component>/fleet.yaml: one Fleet bundle per folder. Upstream-chart
    folders set helm.repo/chart/version; local-chart folders (Chart.yaml
    present) set helm.chart to "." and are deployed as the folder itself.
    values.yaml and, when present, cluster-values.yaml are passed through
    helm.valuesFiles, which Fleet reads only when helm.chart or helm.repo
    is set.
  - NNN-<component>/.fleetignore: keeps install.sh, upstream.env and
    apply-crds.sh out of the Fleet Bundle resource.
  - gitrepo.yaml: a fleet.cattle.io/v1alpha1 GitRepo listing every folder
    path, applied to the Fleet workspace (default fleet-default).
  - README.md: deployment instructions.

In HelmOp mode it writes helmops.yaml instead of fleet.yaml, .fleetignore and
gitrepo.yaml: one fleet.cattle.io/v1alpha1 HelmOp per folder with the values
inlined, so Fleet pulls each chart at deploy time instead of embedding it in a
Bundle. A HelmOp needs a chart repository, so local-chart folders are
rejected. The folders' values files are kept for reference only; Fleet and
bundleinfo both read the inlined values.

# Deployment Ordering

Each fleet.yaml sets an explicit bundle name (<app>-<release>) so dependsOn
references are deterministic regardless of the GitRepo name or the path the
bundle is pushed under; a HelmOp carries the same name. Every folder depends
on the folder immediately
preceding it, so Fleet reconciles the bundle in the same order deploy.sh
installs a helm bundle, including injected -pre and -post folders.

# Targeting

The GitRepo or HelmOps target clusters by label (aicr.nvidia.com/bundle=<app>)
so nothing is rolled out until a cluster is explicitly opted in. In the
fleet-local workspace they target the Rancher local cluster instead.

# Limitations

Fleet cannot run the apply-crds.sh pre-upgrade step, so components that own
their CRDs follow Helm's crds/ semantics (installed once, not upgraded), the
same as the helmfile deployer. Readiness hooks are not supported.
*/
package fleet

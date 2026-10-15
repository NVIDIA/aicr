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
Package terraform generates a Terraform module from an AICR recipe: a root
module by default, or a child module to call from a configuration that owns
the helm provider.

Per-component folder content (values.yaml, cluster-values.yaml, Chart.yaml +
templates/) is delegated to pkg/bundler/deployer/localformat, the same writer
the helm and helmfile deployers use. The two files that exist for helm's shell
path — install.sh and the upstream.env it sources — are removed afterwards:
the module call carries the chart coordinates itself, and a script that
installs the release names Terraform manages has no place beside the state
that manages them (see stripHelmOnlyFiles). This package owns only the
Terraform layer on top: a module with one module call per component, plus the
generic component module they all share.

# One module call per component

localformat emits up to four folders for a component — pre, primary, post,
readiness. They become slots on a single module call and chain inside it, so a
dependent's depends_on names the component and gets the whole chain. The flat
alternative, one call per folder, makes the module boundary lie: with
--readiness-hooks, depends_on = [module.gpu_operator] would not mean
gpu-operator is ready, and a dependent would have to name
module.gpu_operator_readiness to say so.

Assigning a folder to a slot means classifying it, which this package does with
the same suffix test argocd's waveForFolder uses (see phaseOf). localformat
reserves "-readiness" outright and rejects a component whose name would collide
with an injected "-pre"/"-post" sibling, which is what makes the comparison
safe.

# Deployment Ordering

Terraform's depends_on is a native DAG, so the recipe's declared dependencyRefs
project onto it exactly — the same fidelity the flux deployer gets from
dependsOn, and strictly more than the tier-based argocd (integer sync-waves)
and helmfile (nested level files) deployers, which can only approximate the
graph as depth tiers and therefore make every component wait on every sibling
at the prior depth.

Edges land in two places:

  - across components, on the module call: each call depends on the calls of
    its declared dependencyRefs. A ref naming a component absent from the
    enabled set (disabled via overrides, or provided externally) is dropped —
    depends_on = [module.absent] is a parse error, not a plan error.
  - within a component, on the resources inside the module: pre -> primary ->
    post -> readiness, in that fixed order.

Under Serial (--serial) the cross-component edges collapse to a single chain
through deployment order, so components apply strictly one at a time.

# Why depends_on on the module call

A depends_on on a call is a fact about everything the module contains, which is
exactly the claim a dependent wants to make. It also lets a reader of main.tf
see the recipe graph without opening the module.

# Readiness gates

With ComponentReadiness set, a component's gate is the last slot in its module:
a Job asserting the signal that actually means ready, which helm's own wait
cannot see. The slot hardcodes wait rather than reading var.wait, so a
component carrying an async override (see deployer.ComponentOverrideFor)
cannot disarm its own gate.

The gate Job carries the same helm.sh/hook annotations the helm deployer's gate
does, and the hook is what holds the release open: helm waits for a hook to
complete as part of the release. The slot therefore sets wait but not
wait_for_jobs, which covers Jobs in the release's own resource set and so would
do nothing for a hook — the same reason deploy.sh passes --wait without
--wait-for-jobs. The apply does not return, and dependents do not start, until
the Job succeeds, and a failing gate fails the apply.

hook-delete-policy: before-hook-creation is what lets the gate re-assert. A
Job's spec.template is immutable, so an unchanged manifest is a no-op patch;
without the delete-and-recreate the gate would assert once, when it is created.

That covers every upgrade Terraform actually performs on this release. It does
not cover an upgrade elsewhere in the component: helm_release tracks chart,
version and values, not rendered manifests, so a gate release whose own inputs
are unchanged plans clean and is never upgraded at all. Re-asserting on a
dependency's upgrade needs the gate release to carry something that moves with
it — a content digest — which is a follow-up, not a property of this code.

# Cluster connection

The generated module does NOT create a cluster. In root form it configures the
helm provider from variables, so the bundle applies against whatever cluster the
operator points it at (a child module declares no provider and no connection
variables, and uses the caller's):

  - var.kubeconfig_path / var.kube_context for an existing kubeconfig (default);
  - var.cluster_host / var.cluster_ca_certificate / var.cluster_token when the
    cluster is created in the same configuration and its attributes are only
    known after apply.

The second form composes with a cluster resource in the same configuration.
helm_release does not contact the API server at plan time, so a provider
configuration that is only known after apply does not block the plan: one
terraform apply creates the cluster and installs the whole bundle behind it.
See README.md in the generated bundle.

# Cluster rollover

A helm_release's state describes objects inside one cluster. Replace the
cluster and those objects are gone, but the release rows remain — state that
confidently describes nothing.

With ClusterRollover set, each component module carries one terraform_data
keyed on var.cluster_keepers, and every slot in it a replace_triggered_by
pointing at that resource. A change in the keepers replaces the shim, which
replaces every release: destroy-then-create, Terraform's default replacement,
one release at a time in dependency order.

The keepers are the operator's statement of what identifies the cluster — the
"keepers" of the random provider, by name and by contract. The bundle cannot
choose them: an API endpoint survives a rebuild behind a DNS name or a VIP, a
cluster name is reused by design, and a kubeconfig context is a local alias.
What reliably changes is the CA certificate, a provider-assigned id or
creation timestamp, or an endpoint on a platform that mints one per cluster;
a map combines several. Both forms require the variable: a default of null
would contain nothing, and the first real value would then replace every
release. Hence off by default.

Its effect depends on whether the plan can still see the releases. On a cluster
the plan can reach, a changed keeper replaces every release through
replace_triggered_by — measured on Terraform 1.16 as 17 uninstalls followed by
17 installs. When the cluster itself is being replaced in the same
configuration, the provider's configuration is unknown while planning and
hashicorp/helm's Read drops a release it cannot reach from state on any failed
existence check; the plan then creates every release in the new cluster and
uninstalls none in the old, and the shim replaces only itself. The end state is
the same; what differs is whether the old releases were uninstalled, which
matters only when the old cluster outlives the configuration.

# Component Type Support

Only Helm components are supported. Kustomize components produce an
ErrCodeInvalidRequest at generation time, matching the flux deployer.

# Generated Structure

	output/
	├── main.tf                     # one module call per component, carrying the DAG
	├── versions.tf                 # required_providers + the helm provider config (root form)
	├── variables.tf
	├── outputs.tf
	├── terraform.tfvars.example    # root form only
	├── modules/
	│   └── component/
	│       ├── main.tf             # the generic component module: pre/chart/post/gate
	│       ├── variables.tf
	│       └── outputs.tf
	├── 001-nfd/                    # localformat folders, minus helm's shell files
	│   ├── values.yaml
	│   └── cluster-values.yaml
	├── 002-cert-manager/
	│   └── ...
	├── README.md
	└── checksums.txt               # optional
*/
package terraform

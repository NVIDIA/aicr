# Generating Bundles

`aicr bundle` materializes a recipe into deployment-ready artifacts — one
folder per component, each with Helm values, checksums, and a README. This
guide covers the common bundling tasks: choosing a deployer, overriding values,
enabling or disabling components, pinning node scheduling, producing offline
bundles, and gating on component readiness.

This is a task-oriented how-to. For the complete flag list and exit codes, see
the `aicr bundle` section of the [CLI Reference](cli-reference.md). For the
recipe → bundle → deploy → validate flow end to end, see the
[End-to-End Tutorial](tutorial.md).

## Choose a deployer

The `--deployer` (`-d`) flag selects the output format. The bundle content is
the same validated configuration; only the serialization differs, so you can
re-render the same recipe for whatever pipeline you run:

| Deployer | Output |
|----------|--------|
| `helm` (default) | Per-component Helm values + a `deploy.sh` that installs in dependency order. |
| `helmfile` | A `helmfile.yaml` release graph. |
| `argocd` | Argo CD `Application` manifests (app-of-apps), published from a Git repo (`--repo`). |
| `argocd-helm` | A Helm chart app-of-apps; `repoURL` defaults to the push-target registry — plain `helm install` works with no `--set repoURL` needed. Override with `--set repoURL=oci://mirror` when mirroring. Bringing your own root Application? Set `deployer.includeRootApp=false` to render children-only — see [Argo CD Deployer Options](cli-reference.md#argo-cd-deployer-options). |
| `flux` | Flux `HelmRelease` and `Kustomization` manifests. |

```bash
# GitOps with Argo CD, sourced from your config repo
aicr bundle --recipe recipe.yaml --deployer argocd \
  --repo https://github.com/my-org/my-gitops-repo.git \
  --output ./bundles
```

## Bundle layout

This is the canonical description of what a bundle contains. The trees below
are frozen at v1 and gated by `TestBundleLayoutMatchesManifest`, so a path
shown here will not disappear or be renamed without a deliberate, reviewed
change. Automation may read these paths.

Every deployer writes `checksums.txt` and `README.md` at the bundle root. Four
of the five group components into ordered `NNN-<component>` directories; Flux
is the exception and uses a plain `<component>` directory with shared
`sources/`.

```text
helm/                          argocd/                     flux/
  001-cert-manager/              001-cert-manager/            cert-manager/
    values.yaml                    application.yaml             helmrelease.yaml
    cluster-values.yaml            values.yaml                nfd/
    install.sh                   002-nfd/                     helmrelease.yaml
    upstream.env                   application.yaml           sources/
  002-nfd/                         values.yaml                  helmrepo-<host>.yaml
    ...                          app-of-apps.yaml             gitrepo-<host>.yaml
  deploy.sh                      checksums.txt              kustomization.yaml
  recipe.yaml                    README.md                  checksums.txt
  checksums.txt                                             README.md
  README.md
```

`helmfile` shares Helm's per-component files but not its root: it writes
`helmfile.yaml` instead of `deploy.sh`, and does not emit `recipe.yaml`. A
recipe with dependencies also produces one `level-N.yaml` per dependency depth,
which is derived from the recipe rather than fixed by the layout.

```text
helmfile/
  001-cert-manager/            same four files as helm
  002-nfd/
  helmfile.yaml
  level-N.yaml                 one per dependency depth; absent when flat
  checksums.txt
  README.md
```

`argocd-helm` renders a Helm chart at the root — `Chart.yaml`, `values.yaml`,
`values.schema.json` — with one template per component.

Two kinds of name appear in these trees, and only one is a promise:

- **Fixed names are contract.** `deploy.sh`, `app-of-apps.yaml`,
  `kustomization.yaml`, `checksums.txt`, `values.yaml`, `helmrelease.yaml`,
  and the `NNN-<component>` convention itself.
- **Derived names are not.** Flux writes one `helmrepo-<host>.yaml` per chart
  repository and helmfile one `level-N.yaml` per dependency depth, so both sets
  change with the recipe. Discover them by listing the directory rather than
  hardcoding a name.

### Generated chart versions

Not every chart in a bundle comes from upstream. AICR generates one for each
Kustomize-derived component, each manifest-only component, each injected
`-pre` / `-post` / `-readiness` release, and — under `--vendor-charts` — as a
wrapper around every vendored upstream chart. A generated `Chart.yaml` has to
answer two different questions, so it answers them in separate fields:

| Field | Carries | Why |
|-------|---------|-----|
| `version` | the AICR version that produced the bundle | The chart's content is AICR's own, so AICR's version identifies the artifact. This is the `aicr` binary that ran `bundle`, which is not necessarily the one that generated the recipe. |
| `appVersion` | the payload version | The upstream chart pin for a Helm component, the git ref for a Kustomize one. Free-form, so a ref like `release-1.4` need not look like SemVer. |
| `aicr.run/component-version` annotation | the payload version | The same value as `appVersion`, under a stable key to read from a live release. |
| `aicr.run/generated-by` annotation | the AICR version | The same value as `version`. |

To read the payload version back out of a cluster, the rule is: **use
`aicr.run/component-version` when it is present, otherwise use the release's
own chart version.** A component installed straight from its upstream chart
carries neither annotation, and its release version already *is* the payload
version. The annotation's presence is exactly the signal that the chart
version describes the wrapper instead of what it wraps.

So for the `gpu-operator-post` release generated alongside gpu-operator, a
`helm list` reports chart `gpu-operator-post-1.4.0` — the AICR version — while
its app version and `aicr.run/component-version` both read `v25.3.0`, the
gpu-operator pin those manifests accompany.

A component with no upstream pin — a manifest-only component, and the injected
releases belonging to one — ships only AICR-authored content, so `appVersion`
and the annotation report the AICR version too.

A build that is not release-stamped (`aicr --version` reports `dev`) reports
`0.0.0-dev`. Helm validates `version:` as SemVer 2 and refuses to load a chart
whose version is not, so `dev` cannot be written through verbatim.

The root chart `argocd-helm` renders is not one of these generated wrappers and
carries neither annotation. Its `version:` tracks the recipe's
`metadata.version` — the AICR build that generated the *recipe*, not the one
that ran `bundle` — so on a two-step workflow where the two binaries differ, it
will not match the wrappers alongside it. Only the `0.0.0-dev` substitution
above is shared.

Verify a bundle you received with `aicr verify` — see
[Artifact verification](artifact-verification.md).

## Override values

Use `--set` for scalar overrides, scoped per component as
`component:path.to.field=value`:

```bash
aicr bundle --recipe recipe.yaml \
  --set gpuoperator:driver.version=570.86.16 \
  --set gpuoperator:gds.enabled=true \
  --output ./bundles
```

On recipes that carry an ADR-015 configuration profile
(`metadata.selectedProfile`, e.g. the AKS family's `gpuStack`), the
profile's owned paths are locked. The lock is enforced per surface:

- **`aicr bundle` static overrides** (`--set`, `--set-json`,
  `--set-file`, or a config-file override, from any of those sources):
  a value identical to the selected one is accepted; a divergent value
  is rejected. The typed sources (`--set-json`, `--set-file`) are
  always rejected for the special `enabled` presence key — even when
  the value matches — because they would write a stray literal
  `enabled:` chart value instead of toggling the component.
- **`aicr mirror list --set` overrides**: mirror exposes only the
  repeatable scalar `--set` (no `--set-json`/`--set-file`), and the
  same identical-accepted / divergent-rejected rule applies to it.
  Note that `mirror list` does not apply a config file's
  `spec.bundle.deployment.set` overrides — pass image-affecting
  overrides to mirror via `--set` explicitly.
- **`--dynamic` exports**: rejected whenever they merely intersect an
  owned path, regardless of the current value — install-time mutability
  of a locked path is itself the violation.
- **argocd-helm install-time values**: any install-time key whose path
  equals, contains, or is contained by an owned path is rejected at Helm
  render time, even when the value is identical. Key presence alone
  trips the guard.
- **Component presence (the synthetic `enabled` owned path)**: not
  changeable by selecting a different profile value — profile fragments
  cannot assign `enabled`, so no `--profile` choice adds or removes a
  component. The lock rejects removing (or bundle-subsetting away) an
  owned component; changing which components are present is a
  catalog/composition change.

Change owned **value** paths by regenerating with
`aicr recipe --profile name=value` instead; component presence is not
affected by reselection.

`--set` is scalar-only. For list or object values use `--set-json` (inline JSON)
or `--set-file` (value read from a file); both deep-merge objects and replace
lists/scalars, and take precedence over `--set` on the same path:

```bash
aicr bundle --recipe recipe.yaml \
  --set-json agentgateway:allowedSourceRanges='["216.228.127.128/30"]' \
  --output ./bundles
```

The `agentgateway` inference-gateway is **private by default**: with no
`allowedSourceRanges` override, the bundler scopes the LoadBalancer to private
RFC1918 ranges so it is not exposed to the public internet. Use the override
above to admit specific clients (e.g. a corporate VPN). See
[Inference Gateway Network Exposure](component-catalog.md#inference-gateway-network-exposure).

## Enable or disable components

The special `enabled` key includes or excludes a component at bundle time
without editing the recipe:

```bash
# Skip the AWS EBS CSI driver for this bundle
aicr bundle --recipe recipe.yaml \
  --set awsebscsidriver:enabled=false \
  --output ./bundles
```

A recipe or overlay can also disable a component by default via
`overrides.enabled: false` (for example, a platform that ships its own
cert-manager). Such components are already excluded from the recipe's
`deploymentOrder`.

`--set <component>:enabled=false` disables a component the recipe leaves on.
A component the recipe **disabled** cannot be re-enabled at bundle time —
`--set <component>:enabled=true` on such a component is rejected with an error.
The recipe author disables a component because the target platform already
provides it, so re-enabling would install a conflicting second copy. To deploy
a component the recipe disables, edit the recipe/overlay instead.

### Overrides that cannot take effect are rejected

An override whose component will not appear in the generated bundle is
rejected with an error rather than silently discarded. This covers a
component that is absent because the recipe disabled it, because
`--set <component>:enabled=false` removed it, because the `bundlers=`
filter excluded it, or because the component name is neither one the recipe declares nor a
registered `valueOverrideKeys` alias of one (usually a typo — registered
aliases such as `gpuoperator` for `gpu-operator` remain valid):

```bash
# Rejected: the second --set can never take effect
aicr bundle --recipe recipe.yaml \
  --set nv-sentinel:enabled=false \
  --set nv-sentinel:labeler.assumeDriverInstalled=true \
  --output ./bundles
```

The two flags ask for contradictory things — remove the component, and
configure it — so the command fails instead of shipping a bundle with
one request quietly dropped. The rejection here is about the contradiction,
not about who owns the component: a profile-owned presence lock is a
separate rejection, described at the end of this section. Only a scalar `--set <component>:enabled=false`
is exempt on a declared component: it is the supported way to remove
one, and it is also accepted on a component the recipe already disables.
`enabled=true` on a component the `bundlers=` filter excludes is
rejected like any other ineffective override, and the `enabled` key is
never honored from `--set-json`/`--set-file` (present or absent — the
typed path would write a literal `enabled:` chart value instead of
toggling the component).

A component whose presence a configuration profile owns cannot be removed
at all — `enabled=false` on it is rejected regardless of the rule above.
NVSentinel is in that position on the AKS and GKE-COS families, whose
`gpuStack` profiles name it; see
[NVSentinel on provider-installed-driver platforms](component-catalog.md#nvsentinel-on-provider-installed-driver-platforms).

The same rule applies to `--set-json`, `--set-file`, `--dynamic`, and
the REST API's equivalent parameters. For `--dynamic` no path is exempt,
`enabled` included: a dynamic path on an absent component exports
nothing (there is no `cluster-values.yaml` to defer it to), and a
dynamic path is never a removal idiom. Unknown component names were
already rejected by the `bundlers=` filter and by `--dynamic`
registry validation; this extends the same fail-closed rule to every
override source.

## Pin node scheduling

Steer system components and GPU workloads onto the right nodes with selector and
toleration flags (repeatable):

```bash
aicr bundle --recipe recipe.yaml \
  --system-node-selector nodeGroup=system \
  --system-node-toleration dedicated=system:NoSchedule \
  --accelerated-node-selector nvidia.com/gpu.present=true \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --output ./bundles
```

## Prepare DRA nodes when opting in to eviction coordination

DRA eviction coordination is **opt-in**. By default a bundle containing both
`gpu-operator` and `nvidia-dra-driver-gpu` adds no eviction node label, and the
DRA kubelet plugin runs on every accelerated node with no extra labeling. The
trade-off is that the plugin is not descheduled ahead of a GPU driver container
restart; `aicr bundle` warns about this where GPU Operator manages the driver,
and [DRA Driver Upgrade Eviction](cli-reference.md#dra-driver-upgrade-eviction)
describes what can go wrong.

Generate with `--dra-eviction-node-label key=value` to opt in. The rest of this
section applies only then. The same applies to the corresponding `-ocp`
components.

### Choosing whether to opt in

The label is how GPU Operator's Driver Manager finds the plugin: it deschedules
the plugin by rewriting the label's value and restores it afterwards, so the
plugin's `nodeSelector` has to match the label for the mechanism to work at all.
That is what makes it a placement requirement, and why an unlabeled node ends up
with no plugin rather than with an uncoordinated one.

| | Not opted in (default) | Opted in |
|---|---|---|
| Node labeling | none needed | every GPU node, in the node pool definition |
| Plugin placement | every accelerated node | only nodes carrying the label |
| Driver restart | plugin is not descheduled first | plugin is descheduled and restored |
| If a node is missed | n/a | that node silently runs without DRA |

Opt in when GPU Operator manages the driver (`driver.enabled=true`) and you can
guarantee the label is set at provisioning time for every GPU node, including
ones added later by autoscaling or node replacement. Otherwise the default is
the safer choice: a plugin that always runs, with a documented risk at driver
restarts, beats a plugin that silently does not run on some nodes.

There is nothing to opt in to where the driver is provider-installed
(`driver.enabled=false` — AKS `azure-managed`, GKE COS, OKE). GPU Operator
deploys no driver pod and therefore no Driver Manager, so no restart can occur
under the plugin and no warning is emitted.

Also note the mechanism is best-effort under `k8s-driver-manager` v0.12: the
configured label is paused in the same batch as other GPU operands and no wait
covers the standalone DRA kubelet plugin, so ordering against DRA claim holders
and completion of plugin teardown are not guaranteed. See
[NVIDIA/k8s-driver-manager#250](https://github.com/NVIDIA/k8s-driver-manager/issues/250).

> **Opt-in requirement:** label every GPU node that must run the DRA kubelet
> plugin *before* applying the bundle. Applying it first can reduce the
> DaemonSet to zero eligible nodes, interrupting ComputeDomain/IMEX and any
> whole-GPU resources advertised through DRA.

### Set the label at node-pool provisioning time

Put the label in the **node pool definition** — an EKS managed nodegroup
`labels` entry, a Karpenter `NodePool` `spec.template.metadata.labels` entry, or the equivalent for
your provisioner — alongside the `nodeGroup=gpu-worker` label you already set
there.

A one-off `kubectl label node` is a repair, not a configuration. It does not
survive node replacement or recycling, cluster autoscaling adding GPU nodes, or
a nodegroup scaled from zero. Any GPU node added afterwards arrives unlabeled
and silently runs without the DRA kubelet plugin, leaving the cluster
**partially DRA-enabled**. This is harder to detect than uniform failure,
because it is intermittent and node-dependent.

Use `kubectl label` only to repair nodes that already exist, and fix the node
pool definition in the same change so replacements inherit it. Substitute the
same `key=value` pair you passed to `--dra-eviction-node-label` — the examples
below use the documented default:

```bash
kubectl label node <node-name> nvidia.com/dra-kubelet-plugin=true
kubectl get nodes -l nvidia.com/dra-kubelet-plugin=true
```

### The failure mode is silent

An unlabeled GPU node produces no error anywhere. `helm install`/`helm
upgrade` reports success and the bundle's `deploy.sh` exits 0. What you get
instead is:

- no DRA kubelet plugin on any unlabeled node, and no `ResourceSlices` from
  it — so no ComputeDomain/IMEX capability there
- the `nvidia-dra-driver-gpu-kubelet-plugin` DaemonSet at `DESIRED=0` **if no
  GPU node carries the label at all**

Partial coverage is the shape node replacement and autoscaling produce: labeled nodes work normally while the rest silently lack
DRA. A split cluster is harder to notice than uniform failure, because the
DaemonSet looks healthy and only some workloads misbehave.

Because the absence is not self-announcing, confirm the selector matches the
nodes you expect **before** applying the bundle, and check the DaemonSet
afterwards.

### This applies to existing clusters, not just fresh installs

The requirement is easy to read as a fresh-install prerequisite, but the
upgrade path is especially easy to miss. A cluster whose bundle was generated
before this selector existed has a working kubelet-plugin DaemonSet selecting
on `nodeGroup=gpu-worker` alone. Regenerating the bundle and running `helm
upgrade` adds the second selector, and working functionality **disappears** —
still with no error. Revisit the node labels whenever you regenerate a bundle
for an existing deployment, not only when building a new cluster.

`aicr bundle` emits a non-blocking warning describing this requirement whenever
both components are enabled and an eviction label is configured. The
complementary opt-out warning fires only where GPU Operator manages the driver
(`driver.enabled=true`). See
[Storage Class](cli-reference.md#storage-class), where that warning is
described alongside the other cluster-state dependency reported the same way.

### Upgrading from a build that applied the label implicitly

For a window on unreleased `main`, the eviction label was applied
automatically rather than requested. Bundles generated in that window — via the
CLI, the REST API, config, or a Go caller — received the
`nvidia.com/dra-kubelet-plugin=true` selector and the matching Driver Manager
environment entry without asking for them. No tagged release contains that
behavior, so only clusters built from `main` in that interval are affected.

At this head the same inputs produce the opposite result: omitting
`--dra-eviction-node-label` removes both the selector and the Driver Manager
entry. Regenerating and upgrading such a cluster therefore *drops* eviction
coordination silently — the reverse of the direction described above, and the
case the preceding subsection does not cover.

Decide deliberately, and only two answers are defensible:

- **Keep coordination.** Confirm the nodes still carry the label
  (`kubectl get nodes -l nvidia.com/dra-kubelet-plugin=true`), confirm the count
  matches the GPU nodes you expect, then pass
  `--dra-eviction-node-label nvidia.com/dra-kubelet-plugin=true` explicitly on
  every subsequent generation. Verify node labels *before* upgrading: passing
  the flag against unlabeled nodes takes the DaemonSet to `DESIRED=0`.
- **Accept the opt-out.** Regenerate without the flag and accept the documented
  risks in [DRA Driver Upgrade
  Eviction](cli-reference.md#dra-driver-upgrade-eviction). The node labels
  become inert and can be removed at leisure.

The opt-out warning `aicr bundle` prints bounds the blast radius at generation
time, but it is not upgrade guidance: it fires on every unlabeled generation and
cannot know the cluster previously had coordination.

### Custom label conventions and post-install checks

The flag both opts in and selects the convention: generate the bundle with
`--dra-eviction-node-label key=value` and apply that exact pair to the nodes.
AICR gives the full pair to the DRA node selector, but GPU Operator's Driver
Manager receives only the label key because its eviction contract matches and
temporarily removes the label by key.

After installation and after every GPU driver upgrade, monitor the kubelet
plugin DaemonSet until all desired pods are ready. This also catches a Driver
Manager rollout that did not restore the eviction label:

```bash
kubectl -n nvidia-dra-driver get daemonset \
  nvidia-dra-driver-gpu-kubelet-plugin
```

The integration is not rendered when either component is absent. A dynamic
declaration intersecting `kubeletPlugin.nodeSelector` or `driver.manager.env`
is rejected because moving either path to install-time configuration would let
the two halves drift independently. See
[DRA Driver Upgrade Eviction](cli-reference.md#dra-driver-upgrade-eviction) for
configuration details and NVIDIA's
[GPU Operator DRA installation guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.3/dra-intro-install.html)
for the upstream contract.

## Produce an offline (vendored) bundle

`--vendor-charts` pulls upstream Helm chart bytes into the bundle at bundle
time, so the artifact needs no Helm chart registry egress at deploy time. Each
vendored chart is recorded in `provenance.yaml` with name, version, source URL,
and SHA256. Requires the `helm` binary on `PATH`.

```bash
aicr bundle --recipe recipe.yaml --vendor-charts --output ./bundles
```

> Trade-off: vendoring freezes the chart version. A vendored bundle will keep
> installing a frozen chart even if upstream later yanks it for a CVE — you lose
> the fail-loud signal you get when pulling charts live. Container-image pulls
> may still require network access. For full air-gapped operation, also mirror
> images; see [Air-Gap Mirror](air-gap-mirror.md).

Recipe-side manifests of mixed components (AICR-authored manifests shipped
alongside a vendored upstream chart — for example the network-operator
`NicClusterPolicy` or the AKS `nvidia-peermem-reloader` DaemonSet) get the same
lifecycle under `--vendor-charts` as they do without it. The vendored primary
folder wraps only the upstream chart, and the manifests are emitted as a
separate `<component>-post` Helm release installed immediately after it:

```text
002-network-operator/          # wrapper chart + charts/<chart>-<ver>.tgz
003-network-operator-post/     # recipe-side manifests, tracked release
```

Because they are ordinary members of that release rather than Helm hook
resources, they are patched in place by `helm upgrade` (three-way merge),
removed by `helm uninstall`, and applied normally by Argo CD under
`syncPolicy.automated`. Bundle-layer `NNN-` folder ordering sequences the two
releases, so any `helm.sh/hook` annotation a recipe manifest declares is
stripped when the `-post` chart is written — the same treatment the
non-vendored path has always applied.

> Earlier releases injected these manifests into the vendored wrapper chart as
> `helm.sh/hook: post-install` resources, which Helm never re-applied on
> upgrade, left behind on uninstall, and Argo CD silently skipped as a PostSync
> hook. Those live objects are **not** members of the new `<component>-post`
> release, so a redeploy of a rebundled layout fails with Helm ownership
> conflicts (`exists and cannot be imported into the current release`) until
> each resource is adopted or removed.

Adoption is the default path — it is non-destructive and required before
`helm upgrade --install` of the `-post` chart. For each previously
hook-injected resource (name and kind from the new
`<NNN>-<component>-post/templates/` files, or from the old primary
`templates/` if you still have that bundle):

```bash
# Include -n <ns> for namespaced kinds (same <ns> as release-namespace below).
# Omit -n for cluster-scoped kinds (CRD, ClusterRole, NicClusterPolicy, …).
kubectl label -n <ns> <kind>/<name> app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate -n <ns> <kind>/<name> \
  meta.helm.sh/release-name=<component>-post \
  meta.helm.sh/release-namespace=<ns> --overwrite
```

Prefer `kubectl delete` only for kinds where cascade is acceptable (for example
a ConfigMap or ClusterRole with no dependents). **Do not** `kubectl delete -f`
CRD or Namespace manifests from a migration cleanup — deleting a CRD
garbage-collects every CR of that type cluster-wide (Gateway API / Inference
Extension CRDs under `agentgateway-crds` are the concrete risk), and deleting a
Namespace removes everything inside it.

## Gate on component readiness

`--readiness-hooks` emits a standalone readiness-gate chart for each component
that ships a readiness test, run as a post-component Job so the deployer blocks
on component-specific signals (e.g. GPU Operator `ClusterPolicy` state) that
Helm and Argo CD cannot assess natively. Supported with `--deployer helm`,
`argocd`, and `argocd-helm`; off by default.

```bash
aicr bundle --recipe recipe.yaml --readiness-hooks --output ./bundles
```

## Deploy the bundle

For the default `helm` deployer, verify the bundle before installing it:

```bash
cd bundles && aicr verify . && chmod +x deploy.sh && ./deploy.sh
```

For GitOps deployers, commit/publish the manifests per your Argo CD or Flux
workflow. Verify the bundle directory before it enters the repository or
registry; a pull-based controller reconciles on its own schedule, so a pipeline
step gates what gets published rather than what the cluster applies. For the
per-deployer gates and which of them the cluster can enforce, see
[Gating Deployment on Verification](../integrator/supply-chain-verification.md#gating-deployment-on-verification).

After deploying, confirm the cluster matches the recipe with
[`aicr validate`](validation.md).

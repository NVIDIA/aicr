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
| `argocd-helm` | A Helm chart app-of-apps; `repoURL` defaults to the push-target registry — plain `helm install` works with no `--set repoURL` needed. Override with `--set repoURL=oci://mirror` when mirroring. |
| `flux` | Flux `HelmRelease` and `Kustomization` manifests. |

```bash
# GitOps with Argo CD, sourced from your config repo
aicr bundle --recipe recipe.yaml --deployer argocd \
  --repo https://github.com/my-org/my-gitops-repo.git \
  --output ./bundles
```

## Override values

Use `--set` for scalar overrides, scoped per component as
`component:path.to.field=value`:

```bash
aicr bundle --recipe recipe.yaml \
  --set gpuoperator:driver.version=570.86.16 \
  --set gpuoperator:gds.enabled=true \
  --output ./bundles
```

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

For the default `helm` deployer:

```bash
cd bundles && chmod +x deploy.sh && ./deploy.sh
```

For GitOps deployers, commit/publish the manifests per your Argo CD or Flux
workflow. After deploying, confirm the cluster matches the recipe with
[`aicr validate`](validation.md).

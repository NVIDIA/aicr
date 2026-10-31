# CoreWeave H100 Setup

The `h100-cks-ubuntu-training` recipe targets CoreWeave Kubernetes
Service (CKS) with H100 GPUs and Ubuntu. This is a Preview recipe;
live deployment, training performance, lifecycle qualification, and signed
validation evidence remain pending under [#2342](https://github.com/NVIDIA/aicr/issues/2342).
A reference snapshot establishes observed configuration, not qualification.

## GPU stack ownership

The default and only `gpuStack` value is `coreweave`. CoreWeave supplies the
driver, container integration, device plugin, NFD, and GPU DRA driver. AICR
records an external `nvidia.com/gpu` advertiser and disables its GPU Operator,
NFD, GPU DRA driver, and DRA node labeler. The recipe adds no Network Operator
or NodeWright host customizations.

Snapshot-based recipe generation requires both `GPU.hardware.driver-loaded`
and `K8s.policy.devicePlugin.enabled` to be `true`; absent or false readings
reject the profile. These are prerequisites, not comprehensive health checks
of CoreWeave's controller operands. Criteria-only generation records the
requirements without observing a cluster.

## Generate and inspect

Pass `--service cks` explicitly, including when supplying a snapshot.
`coreweave` is accepted as a CLI service alias and resolves to `cks`. Automatic
CKS fingerprint detection is not implemented by this recipe addition.

```bash
aicr recipe --service cks --accelerator h100 \
  --os ubuntu --intent training --output recipe.yaml

aicr bundle --recipe recipe.yaml --deployer argocd --output ./bundle
```

To check compatibility with an existing snapshot before bundling:

```bash
aicr recipe --snapshot snapshot.yaml --service cks \
  --accelerator h100 --os ubuntu --intent training \
  --output recipe.yaml
```

The recipe retains upstream cert-manager, NodeWright Operator, Prometheus
Operator CRDs, kube-prometheus-stack, ephemeral-storage metrics, and KAI
Scheduler. Before deployment, check ownership of any existing installations
and use bundle-time component disables for components managed elsewhere.
Installing NodeWright Operator alone does not install host customization
packages. This recipe requires no private package images or image-pull
credentials.

CoreWeave does not preinstall cert-manager on bare CKS H100 clusters. This
recipe includes it by default, as does the CKS RTX PRO 6000 recipe. Finding
cert-manager in a snapshot can reflect a manual installation rather than a
provider-supplied component.

If cert-manager is already installed manually or managed by another cluster
operator, decide who will manage it before deploying. To keep that existing
installation separately managed, omit AICR's copy when generating the bundle:

```bash
aicr bundle --recipe recipe.yaml --deployer argocd --output ./bundle \
  --set cert-manager:enabled=false
```

Use a fresh output directory for each bundle. Setting only
`crds.enabled=false` does not avoid collisions with the existing controllers,
webhooks, or cluster-scoped RBAC. The pinned NodeWright v0.20.0 operator
manages self-signed webhook certificates; its chart does not require
cert-manager. Check the requirements of other workloads before omitting it.
Apply the same ownership review to existing Prometheus Operator CRDs,
monitoring components, and schedulers before installing the bundle; an
offline recipe check does not establish resource ownership.

CoreWeave already runs node-exporter on host port 9100, so this recipe disables
kube-prometheus-stack's node-exporter. NVSentinel is disabled pending
qualification of its integration with the provider GPU runtime. Prometheus
Adapter is disabled pending a qualified scrape of CoreWeave's DCGM exporter;
this recipe does not promise GPU metrics or GPU-based HPA.

## Qualification scope

This page covers `cks / h100 / ubuntu / training`. RTX PRO 6000 has a
[separate setup guide](cks-rtx-pro-6000-setup.md). Inference, platform
variants, provider-specific health checks, fabric tuning,
and service detection remain separate work. No driver version is pinned:
CoreWeave owns that lifecycle.

Deployment validation runs `expected-resources` for the enabled AICR
components and `check-nvidia-smi` on schedulable GPU nodes that are not busy.
The CKS recipe removes the inherited GPU Operator health and version checks,
including the operator-version constraint, because CoreWeave owns that
installation. The conformance phase runs `platform-health` for the enabled
AICR components; it does not verify disabled components' provider equivalents.

KWOK profiles model CPU system nodes and H100 GPU nodes for the
repository's simulated scheduling tests. These exercise bundle rendering and
pod placement without running the GPU software. They do not qualify the
provider stack or GPU workloads. This recipe adds no training benchmark or
performance threshold. Passing catalog, render, scheduling, or offline
snapshot checks does not establish live workload health.

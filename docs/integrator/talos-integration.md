# Talos integration

Talos Linux enforces stricter pod-security defaults than most managed
Kubernetes distributions, and several AICR components need privileged
access (host filesystem, host network, root user) to do their jobs.
AICR ships an opt-in `os-talos` mixin that handles the namespace and
Pod Security Admission (PSA) bookkeeping for the affected components
so a recipe author doesn't have to.

This page documents what the mixin does and why. It assumes you
already have a Talos cluster reachable via `kubectl` and that you've
read [recipe-development.md](recipe-development.md) for the general
recipe / overlay / mixin model.

## What happens when you select os=talos

A leaf overlay that targets Talos opts into the mixin:

```yaml
spec:
  base: <some-base-overlay>
  mixins:
    - os-talos
  criteria:
    os: talos
```

When `aicr bundle` resolves that recipe, the `os-talos` mixin:

1. Overrides the install namespace of five components to
   `privileged-<component>`.
2. Attaches a per-component `manifests/talos-namespace.yaml` manifest (a
   Namespace resource with PSA-privileged labels) via the
   `preManifestFiles` field — so the bundler emits a `-pre` folder
   at sync-wave N-1 ahead of the corresponding chart at wave N.
3. Adds a `OS.release.ID == talos` constraint to the recipe so the
   bundle won't silently install on a non-Talos cluster.

No operator pre-cluster setup is required. `kubectl apply -f` or an
Argo CD GitOps sync handles namespace creation and chart install in
the right order.

## Namespaces the mixin creates

| Component | Namespace | Manifest source |
|-----------|-----------|-----------------|
| gpu-operator | `privileged-gpu-operator` | `recipes/components/gpu-operator/manifests/talos-namespace.yaml` |
| network-operator | `privileged-network-operator` | `recipes/components/network-operator/manifests/talos-namespace.yaml` |
| nvsentinel | `privileged-nvsentinel` | `recipes/components/nvsentinel/manifests/talos-namespace.yaml` |
| nvidia-dra-driver-gpu | `privileged-nvidia-dra-driver-gpu` | `recipes/components/nvidia-dra-driver-gpu/manifests/talos-namespace.yaml` |
| nodewright-operator | `privileged-nodewright-operator` | `recipes/components/nodewright-operator/manifests/talos-namespace.yaml` |

## Why these components run privileged

- **gpu-operator** — installs NVIDIA drivers via DaemonSets that
  need `privileged: true`, host paths into `/sys`, `/dev`, and
  hostPath for kernel-module loading.
- **network-operator** — installs RDMA/NIC drivers; the driver
  DaemonSet needs hostNetwork plus kernel-module privileges.
- **nvsentinel** — health and observability daemon that reads the
  kernel ring buffer, host log paths, and hardware sysfs entries.
- **nvidia-dra-driver-gpu** — Dynamic Resource Allocation plugin
  reads/writes CDI device manifests under `/var/run/cdi`, requiring
  hostPath and root.
- **nodewright-operator** — controller managing kernel-tuning
  Customization CRDs. The operator itself is the gate for
  privileged actions on managed nodes.

## Pod Security Standards label set

Each generated Namespace carries:

```yaml
pod-security.kubernetes.io/enforce: privileged
pod-security.kubernetes.io/enforce-version: latest
pod-security.kubernetes.io/audit: privileged
pod-security.kubernetes.io/audit-version: latest
pod-security.kubernetes.io/warn: privileged
pod-security.kubernetes.io/warn-version: latest
app.kubernetes.io/managed-by: aicr
app.kubernetes.io/component: <component-name>
aicr.nvidia.com/os: talos
```

Setting all three of `enforce`, `audit`, and `warn` to `privileged`
keeps audit logs and API-server warnings consistent with what's
actually being enforced. The AICR-managed labels make these
namespaces selectable for fleet-wide audits ("which privileged
namespaces in this cluster are AICR-managed?").

Background:
- [Kubernetes Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
- [Talos pod-security guidance](https://www.talos.dev/v1.9/kubernetes-guides/configuration/pod-security/)

## Apply ordering

For each affected component the bundle contains:

```text
NNN-<component>-pre/   # Namespace + PSA labels (sync-wave N-1 in Argo CD)
(NNN+1)-<component>/    # the chart (sync-wave N in Argo CD)
```

The bundler emits the `-pre` folder ahead of the primary folder in
the local directory layout, and Argo CD's sync-wave is the folder
index, so:

- **Helm deployer:** the generated `install.sh` iterates folders in
  order, so `helm install` for the namespace happens before the
  chart install. No operator action needed.
- **Argo CD deployer:** each folder becomes an Application with
  `argocd.argoproj.io/sync-wave: "<index>"`. The pre-folder has the
  lowest wave, so Argo applies it first. No operator action needed.

## What the mixin does NOT cover

The following privileged components are intentionally not in the
mixin. If you hit PSA rejection on one of them when deploying on
Talos, please open an issue against [#565](https://github.com/NVIDIA/aicr/issues/565):

- **`aws-ebs-csi-driver`, `aws-efa`** — cloud-specific drivers,
  belong in a per-cloud mixin (future work).
- **`gke-nccl-tcpxo`** — GKE-specific NIC tuning, same reasoning.
- **`nodewright-customizations`** — the Customization CRDs the
  nodewright-operator manages; out of scope until their per-node
  privileged story is settled.
- **`kube-prometheus-stack`** — only the `node-exporter` daemon
  needs privileged. The right fix is a chart-level override
  (`nodeExporter.enabled: false` or a custom Helm value pointing
  the daemon at a separate namespace), not a whole-chart namespace
  move.

## Snapshot agent on Talos

The `aicr snapshot` command's agent pod has its own Talos handling
that is separate from this mixin. The agent's `OS=talos` pod-shape
branch in `pkg/k8s/agent/job.go` skips the `/run/systemd` and
`/etc/os-release` hostPath mounts because Talos has no systemd D-Bus.
See PR #714 for the agent-side history and `tools/talos-test/` for
the local-cluster test harness.

## References

- [Kubernetes Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
- [Talos pod-security guidance](https://www.talos.dev/v1.9/kubernetes-guides/configuration/pod-security/)
- [AICR mixin authoring](recipe-development.md)
- Source: [`recipes/mixins/os-talos.yaml`](../../recipes/mixins/os-talos.yaml)

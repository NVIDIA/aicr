# RKE2 VR200 Setup

Bare-metal setup guide for the four Preview coordinates AICR publishes on VR200
(Vera Rubin) NVL72 hardware running RKE2:

- [`rke2/vr200-ubuntu/training`](https://validation.aicr.run/#/rke2/vr200-ubuntu/training)
- [`rke2/vr200-ubuntu/training-kubeflow`](https://validation.aicr.run/#/rke2/vr200-ubuntu/training-kubeflow) — adds Kubeflow Trainer to
  the training leaf
- [`rke2/vr200-ubuntu/inference`](https://validation.aicr.run/#/rke2/vr200-ubuntu/inference) — platform-neutral inference base (resolves when `--platform` is omitted)
- [`rke2/vr200-ubuntu/inference-dynamo`](https://validation.aicr.run/#/rke2/vr200-ubuntu/inference-dynamo)

> **`service=rke2` and `accelerator=vr200` are Preview.** They publish an early-adopter recipe path without the full production support and lifecycle qualification required for Supported status. Published validation evidence exists for all four coordinates at [validation.aicr.run](https://validation.aicr.run/); freshness against the current recipe is captured in the **Evidence status** note below.

**Evidence status.** The recipes for the three original coordinates have
changed since evidence publication (`aicr evidence digest` reports a mismatch
against each pointer's `predicate.recipe.digest`); treat that linked evidence
as historical precedent for the recipe content at publication time, not as
validating the current recipe. The `training-kubeflow` evidence is current — it
was published from a three-phase run against the recipe as it ships today.

**Which prerequisites apply where.** The node-level requirements — Ubuntu 26.04
with the 64k-page kernel, the Skyhook-driven kernel-cmdline reboots, and the
host `nvidia-imex` masking — apply to **all four** coordinates. The Kubeflow
leaf inherits them from `vr200-rke2-ubuntu-training`.

The two families do **not** otherwise share a chain: training resolves through
`vr200-rke2-ubuntu-training -> rke2-training -> rke2`, inference through
`rke2-inference -> rke2`. Requirements that follow from the inference chain —
the `< 1.36.0` Kubernetes cap and the Gateway API / LoadBalancer prerequisites
— are inference-only and are marked as such where they appear below.

## Cluster Prerequisites

- **Bare-metal RKE2.** These recipes target physical VR200 NVL72 racks; there
  is no cloud-managed RKE2 lane in scope for v1. Node lifecycle (reimage,
  reboot recovery, BMC intervention) is the operator's responsibility — plan
  BMC access before rollout because the Skyhook-driven kernel-cmdline changes
  described below reboot each GPU node.
- **Kubernetes version window.** The training leaves (plain and Kubeflow) pin
  `K8s.server.version >= 1.34.1` because ComputeDomain / IMEX for the NVL72
  MNNVL fabric uses the GA DRA API (`resource.k8s.io/v1`), and the RKE2 root's
  CDI/NRI containerd 2.1 lands at `v1.34.1+rke2r1`. **Both** inference
  leaves (`vr200-rke2-ubuntu-inference` and `vr200-rke2-ubuntu-inference-dynamo`)
  additionally **cap** the window at `< 1.36.0` — the cap is authored on the
  `rke2-inference` base, restated on the VR200 inference base, and inherited
  by the Dynamo child. RKE2 v1.36 changes the default packaged ingress to
  Traefik, whose bundled `rke2-traefik-crd` release would collide with the
  Gateway API CRDs `rke2-inference` installs itself.
- **Ubuntu 26.04 with the Vera-optimized arm64 64k-page kernel.** The
  `os-ubuntu` mixin is deliberately not used — it pins Ubuntu 24.04, while the
  VR200 NVL72 reference image ships Ubuntu 26.04 with
  `7.0.0-*-nvidia-bos-64k`.
- **StorageClass.** RKE2 ships no default StorageClass (unlike k3s, which
  bundles `local-path-provisioner`). The training recipe does not need one,
  but `inference-dynamo` does — its bundled NATS JetStream StatefulSet
  requests a PVC that will otherwise hang Pending. Install any dynamic
  provisioner and mark it default before deploying the inference bundle.
- **LoadBalancer (inference chain only).** RKE2 provisions no LoadBalancer
  controller, and the `inference-gateway` Service the `rke2-inference` base
  installs is `type: LoadBalancer`. Install a bare-metal LB implementation
  (e.g. MetalLB or kube-vip) before deploying the `inference` or
  `inference-dynamo` bundle. Without one the Service sits at
  `EXTERNAL-IP <pending>` and the chain's conformance check gates on
  `Gateway.status.conditions[?type=='Programmed'].status == 'True'` — it
  will not go green. (Until an LB exists the gateway is still reachable via
  its NodePort / ClusterIP for smoke tests.)
- **Mask the host `nvidia-imex` service on every GPU node.** The Vera Rubin
  reference image installs the host-managed 615 driver stack, which enables
  an `nvidia-imex` systemd service by default. All four shipped coordinates
  install the DRA `ComputeDomain` daemon unconditionally, which starts its
  own `nvidia-imex` and cannot allocate the session while the host service
  owns it (`NV_ERR_IN_USE`). The consequence is silent: the ComputeDomain
  daemon never goes `Ready`, `ComputeDomain` workloads hang on
  `NodePrepareResources`, and no validator surfaces this. Disable *and* mask
  on every GPU node **before** deploying the bundle (a disable-only lets
  systemd reactivate the service on any dependency pull-in or package
  upgrade, reproducing `NV_ERR_IN_USE`):

  ```shell
  systemctl disable --now nvidia-imex.service
  systemctl mask nvidia-imex.service
  ```

  To fully reverse (unmask alone leaves the service inactive because it was
  also disabled and stopped above):

  ```shell
  systemctl unmask nvidia-imex.service
  systemctl enable --now nvidia-imex.service
  ```

## Rollout Behavior

**All four VR200 coordinates reboot every GPU node they touch.** One
Skyhook CR (`tuning`, from `nodewright-customizations`) ships in each leaf and
carries two rebooting packages:

- `nvidia-tuned` — writes the native `vr200/rke2` `nvidia-tuned` profile to
  `/etc/default/grub.d`; the specific profile is intent-dependent, so
  `tuned-adm active` reports different profiles on different leaves:
  - Training (`vr200-rke2-ubuntu-training`) applies `multiNodeTraining`,
    whose `[bootloader]` stanza adds IOMMU passthrough,
    `init_on_alloc=0`, NUMA balancing off, hugepages, and earlycon.
  - Inference and inference-dynamo (`vr200-rke2-ubuntu-inference*`) apply
    VR200's `inference` profile, whose `[bootloader]` stanza differs.

  In both cases the kernel cmdline is written at config time and nothing is
  live until the reboot. `post-interrupt-bootloader-check` then asserts
  every argument in whichever profile is active reached `/proc/cmdline`
  before the node is labelled tuned.
- `rdma-netns-exclusive` — flips `ib_core netns_mode=0` on every selected
  GPU node. This is a **host-wide, module-level** kernel parameter
  change: it affects every RDMA user on the node, not just this recipe's
  pods. The downstream benefit AICR relies on is that `dranet` can then
  present each pod with only its allocated HCA
  (`/sys/class/infiniband`), but any pre-existing RDMA workload on the
  node — MPI jobs, RDMA-backed storage, other operators — sees the same
  netns-mode change and must tolerate it. **Coordinate with existing RDMA
  users before rollout**, especially on shared reference clusters. Like
  `nvidia-tuned`, this is a kernel-module parameter change that requires
  a reboot to take effect. Enabled by `nodewright-customizations`
  `rdmaNetnsExclusive: true`, independent of `tuningEnabled`.

Because both packages ride one CR, its `interruptionBudget.count: 1`
bounds every reboot the recipe causes: one GPU node at a time. Neither
package depends on the other, so nodewright runs them in the same pass and
coalesces their reboots — **each GPU node reboots once**, and the rack
converges in roughly **N × (reboot time)**. Separate CRs could not give
either guarantee, because an `interruptionBudget` does not compose across
CRs ([#2572](https://github.com/NVIDIA/aicr/issues/2572)).

**Practical implications:**

- `count: 1` sizing suits the small NVL72 clusters VR200 Preview targets
  and gradual rollout onto a cluster already running work; it does not
  scale to hundreds of nodes.
- The CR is `runtimeRequired`, so each node keeps the runtime-required
  taint until **both** packages finish on it — workloads never land on a
  node still in shared RDMA mode.
- On bare metal with no auto-reimage, BMC access must be ready before
  rollout — a reboot that hangs at BIOS is on you to recover.
- **This is not a drive-by deploy.** On any cluster with running workloads,
  apply the bundle deliberately rather than letting it roll unattended.

**Upgrading from a bundle with a separate `rdma-netns-exclusive`
component.** Earlier bundles shipped the RDMA mode as its own component and
CR, whose `interruptionBudget` is independent of the merged `tuning` CR's. If
both are rolling at once, each can reboot a different GPU node:

1. Before upgrading, wait until the old CR reports `complete`
   (`kubectl get nodewright rdma-netns-exclusive -o jsonpath='{.status.status}'`);
   a complete CR issues no further reboots. If it cannot complete, stop it
   with `kubectl annotate nodewright rdma-netns-exclusive
   nodewright.nvidia.com/disable=true` and wait until every GPU node is
   `Ready`.
2. Apply the new bundle.
3. Remove the old release. Argo CD's app-of-apps prunes the
   `rdma-netns-exclusive` `Application` once the new bundle replaces the old
   one in Git, and Flux prunes its `HelmRelease` when the Kustomization sets
   `prune: true`. The Helm and helmfile deployers leave the release
   installed; remove it with `helm uninstall rdma-netns-exclusive -n nodewright`.
4. The CR was a Helm hook, so it can outlive its release. Delete it if it
   remains: `kubectl delete nodewright rdma-netns-exclusive` (the resource
   is cluster-scoped).

The package has no uninstall step, so removing the CR leaves the host setting
in place. The first rollout of the merged CR re-applies the RDMA package on
already-tuned nodes, which costs one more reboot per GPU node.

## Coordinating on a Shared Reference Cluster

Coordination on a shared VR200 reference cluster is a **results-validity**
requirement, not a stability one. `nvidia-tuned` allows multiple Skyhook CRs
to coexist in `complete` state; adding this recipe's `tuning` CR alongside
an out-of-band tuning CR means the profile actually in effect is decided by
priority ordering. A measurement taken as-is would be ambiguous about which
configuration it describes. Deconflict before running validation or capturing
evidence.

## Preview Boundary and Known Gaps

This coordinate is Preview, not Supported. Explicit gaps beyond the [Preview
recipes definition](recipe-development.md#preview-recipes):

- **SKU auto-detection cannot identify `vr200` yet.** The pre-release driver
  reports the generic `NVIDIA Graphics Device` placeholder, so
  snapshot-backed recipe generation on VR200 hardware resolves
  `accelerator: any`; the coordinate must be selected explicitly with
  `--accelerator vr200` (or the equivalent query parameter).
- **`Deployment.gpu-operator.version >= v26.7.0` does not gate
  `nvidia-dra-driver-gpu`'s deployed version.** No per-component deployment
  check exists for the DRA driver. The current registry default is
  `nvidia-dra-driver-gpu` 0.5.0 (bumped together with `gpu-operator`
  v26.7.0 in [#2439](https://github.com/NVIDIA/aicr/issues/2439)), which
  matches the constraint; but an independently upgraded / downgraded /
  older-bundle cluster can pass the `gpu-operator` gate while still running
  the previous 0.4.1 default, which crash-loops on VR200's NVML arch value.
- **Upstream `nodewright-packages` defects** affect settings the recipe
  configures but cannot enforce (containerd `LimitSTACK` drop-in targets an
  inactive unit on stock RKE2; the `vr200` `[bootloader]` stanza requests
  `hugepagesz=1G hugepages=2` against a 64k-page kernel that has no 1 GiB
  pool; `configure_bootloader.sh` duplicates TuneD's native
  `$tuned_params` hook). Tracked upstream at NVIDIA/nodewright-packages#134
  / #135 / #136.

## Related Issues

- [#2326](https://github.com/NVIDIA/aicr/issues/2326) — VR200 Preview epic
  (v1 milestone, Preview boundary and post-v1 qualification list).
- [#2564](https://github.com/NVIDIA/aicr/issues/2564) — added the
  `training-kubeflow` leaf for `rke2/vr200` and fixed the underlying
  `pkg/health` leaf-scoring gap that had deferred it.
- [#2569](https://github.com/NVIDIA/aicr/issues/2569) —
  `nccl-benchmark-runtime-ref` cannot satisfy namespaced DRA dependencies
  after per-run namespace isolation.
- [#2572](https://github.com/NVIDIA/aicr/issues/2572) — why the RDMA mode
  and node tuning share one Skyhook CR instead of two.

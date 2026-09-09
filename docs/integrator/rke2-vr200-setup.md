# RKE2 VR200 Setup

Bare-metal setup guide for the three Preview coordinates AICR publishes on VR200
(Vera Rubin) NVL72 hardware running RKE2:

- [`rke2/vr200-ubuntu/training`](https://validation.aicr.run/#/rke2/vr200-ubuntu/training)
- [`rke2/vr200-ubuntu/inference`](https://validation.aicr.run/#/rke2/vr200-ubuntu/inference) — platform-neutral inference base (resolves when `--platform` is omitted)
- [`rke2/vr200-ubuntu/inference-dynamo`](https://validation.aicr.run/#/rke2/vr200-ubuntu/inference-dynamo)

> **`service=rke2` and `accelerator=vr200` are Preview.** They publish an early-adopter recipe path without the full production support and lifecycle qualification required for Supported status. See the published validation evidence for these Preview coordinates at [validation.aicr.run](https://validation.aicr.run/); freshness against the current recipe is captured in the **Evidence status** note below.

**Evidence status.** The recipes for all three coordinates above have changed
since evidence publication (`aicr evidence digest` reports a mismatch
against each pointer's `predicate.recipe.digest`); treat the linked evidence
as historical precedent for the recipe content at publication time, not as
validating the current recipe. Fresh hardware validation is pending VR
cluster access.

## Cluster Prerequisites

- **Bare-metal RKE2.** These recipes target physical VR200 NVL72 racks; there
  is no cloud-managed RKE2 lane in scope for v1. Node lifecycle (reimage,
  reboot recovery, BMC intervention) is the operator's responsibility — plan
  BMC access before rollout because the Skyhook-driven kernel-cmdline changes
  described below reboot each GPU node.
- **Kubernetes version window.** The training leaf pins
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
  an `nvidia-imex` systemd service by default. All three shipped coordinates
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

**Both VR200 leaves reboot every GPU node they touch.** Two rebooting Skyhook
CRs ship in each leaf and are applied when the bundle deploys:

- `tuning-rke2` — writes the native `vr200/rke2` `nvidia-tuned` profile to
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
  `tuning-rke2`, this is a kernel-module parameter change that requires
  a reboot to take effect.

Each CR limits its own rollout to one node at a time via
`interruptionBudget.count: 1`, but that budget is **per-CR and does not
compose across CRs**. Under the default `sequencing: node`,
`IsNodeReadyForSkyhook` checks per-node completion of a predecessor rather
than global completion, so one node can start tuning while another is still
on the RDMA CR — **concurrent reboots across the two CRs are possible on a
multi-node cluster today**, and nothing guarantees the RDMA CR exists when
tuning becomes runnable. Full analysis and the candidate fixes (merge both
packages into a single Skyhook CR, or `sequencing: all` plus a dependency
edge) are tracked in [#2572](https://github.com/NVIDIA/aicr/issues/2572).

**Practical implications:**

- Convergence cost depends on how the two CRs interleave — neither of the
  bounds below is a Skyhook or nodewright default. The recipe ships each CR
  with `interruptionBudget.count: 1` (this is per-CR, not a controller
  default, and does not compose across CRs), and the recipe as shipped does
  **not** enforce serialization between the two CRs — that is an operator
  choice tracked in [#2572](https://github.com/NVIDIA/aicr/issues/2572).
  - **Manually serialized (recommended)** — the operator holds off applying
    the second CR (or gates it via a sequencing runbook) until the first
    has drained. Each GPU node then takes two independent reboots, so the
    rack performs roughly **2N × (reboot time)**.
  - **Fully overlapped (unattended)** — apply both CRs at once and let the
    Skyhook controller schedule them freely on the same node set (both
    select `nvidia.com/gpu.present`). Wall-clock trends toward
    **~N × (reboot time)** in the best case, but this is exactly the
    "not a drive-by deploy" behavior below and the concurrent-reboot risk
    the section above describes.

  Neither shape scales to hundreds of nodes: `count: 1` sizing suits the
  small NVL72 clusters VR200 Preview targets and gradual rollout onto a
  cluster already running work.
- On bare metal with no auto-reimage, BMC access must be ready before
  rollout — a reboot that hangs at BIOS is on you to recover.
- **This is not a drive-by deploy.** On any cluster with running workloads,
  apply these CRs deliberately rather than letting both roll unattended.

## Coordinating on a Shared Reference Cluster

Coordination on a shared VR200 reference cluster is a **results-validity**
requirement, not a stability one. `nvidia-tuned` allows multiple Skyhook CRs
to coexist in `complete` state; adding this recipe's `tuning-rke2` alongside
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
- [#2564](https://github.com/NVIDIA/aicr/issues/2564) — the
  `training-kubeflow` leaf for `rke2/vr200` and the underlying `pkg/health`
  leaf-scoring gap that deferred it.
- [#2569](https://github.com/NVIDIA/aicr/issues/2569) —
  `nccl-benchmark-runtime-ref` cannot satisfy namespaced DRA dependencies
  after per-run namespace isolation.
- [#2572](https://github.com/NVIDIA/aicr/issues/2572) — the two rebooting
  Skyhook CRs described above and their concurrency shape.

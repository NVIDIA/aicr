# k0s H200 Setup

AICR publishes one Preview coordinate for [k0s](https://k0sproject.io/):
[`k0s/h200-ubuntu/training`](https://validation.aicr.run/#/k0s/h200-ubuntu/training).

> **`service=k0s` is Preview.** It publishes an early-adopter recipe path
> without the full production support and lifecycle qualification required
> for Supported status. Published validation evidence is at
> [validation.aicr.run](https://validation.aicr.run/).

The evidence comes from a k0s v1.36.3+k0s.0 cluster on Ubuntu 24.04 with
2x H200 NVL attached by PCIe passthrough to a KubeVirt guest, and the
NVIDIA driver 610.57.04 installed on that node. Nothing in the recipe is
specific to the fixture — passthrough hands the node the full devices, so
a physical host carrying the same driver looks identical to it.

## Cluster Prerequisites

- **Kubernetes 1.34 or newer.** A single node running both controller and
  worker roles (`k0s install controller --enable-worker`) is enough.
- **Ubuntu 24.04.** The leaf constrains `OS.release.ID: ubuntu` and
  `OS.release.VERSION_ID: "24.04"`. Other OS or version combinations need
  their own leaf.
- **Install the NVIDIA driver on the node before deploying.** The recipe
  assumes a host-provided driver: it turns the GPU Operator's driver
  install off, points the DRA driver at the host root
  (`nvidiaDriverRoot: /`), sets NVSentinel's labeler to
  `assumeDriverInstalled`, and disables its metadata collector. These four
  settings ship together in the leaf, and AICR's driver-ownership
  coherence checks fail closed if they diverge — don't override one
  without the others. If you want the GPU Operator to install the driver
  instead, this leaf is not your coordinate — author one with the
  opposite posture (the default in most other service roots).
- **Leave containerd alone.** k0s runs its own bundled containerd (socket
  `/run/k0s/containerd.sock`), not the distro's. The `k0s` service root
  already handles this: the container toolkit writes
  `/etc/k0s/containerd.d/nvidia.toml` into k0s's drop-in directory, and
  k0s picks it up without a config rewrite or restart. Configuring the
  toolkit by hand tends to fail silently — it configures the wrong
  containerd, reports success, and GPU pods later fail with
  `no runtime for "nvidia" is configured`.
- **StorageClass is optional.** k0s ships none, and the training recipe
  does not need one — the bundled `kube-prometheus-stack` defaults to
  emptyDir storage. For persistent monitoring, install a dynamic
  provisioner (e.g. `local-path-provisioner`) and override
  `prometheusSpec.storageSpec`.
- **MIG is off by default.** The leaf disables `mig-manager` because MIG
  reconfiguration disrupts running workloads and can require a node
  drain. Enable it explicitly if you partition your H200s.

## Rollout Behavior

Deploying the bundle reboots nothing. Unlike the VR200/RKE2 coordinates,
this leaf ships no Skyhook node tuning — `nodewright-operator` installs,
but no CRs come with it, so no node is rebooted or drained. Expect the
usual GPU Operator operand rollout on first install (container toolkit,
device plugin, DCGM, GPU feature discovery) and a brief runtime-class
registration as the toolkit's drop-in lands.

## Preview Boundary and Known Gaps

This coordinate is Preview, not Supported. Beyond the
[Preview recipes definition](recipe-development.md#preview-recipes):

- **No performance phase.** The reference cluster is a single node with
  no inter-node fabric, so the leaf declares no NCCL bandwidth floor.
  Adding the bare `nccl-all-reduce-bw` check will not create one: the
  default variant's applicability matrix has no k0s entry, so the check
  reports *skipped* and the performance phase still derives *passed* —
  a silent pass with no benchmark behind it. Multi-node operators
  should instead use `nccl-benchmark-profile` when an embedded profile
  matches their fabric, or `nccl-benchmark-runtime-ref` to supply a
  runtime from a `--data` tree. Both are documented in
  [Validation](../user/validation.md).

## References

- [#2656](https://github.com/NVIDIA/aicr/pull/2656) — the k0s service
  root, training layer, and `h200-k0s-ubuntu-training` leaf, with the
  published validation evidence (8/8 deployment and conformance checks).

# NVKE VR200 Setup

The `vr200-nvke-ubuntu-training` recipe targets NVKE (NVIDIA Kubernetes Engine,
formerly NKE): bare-metal Vera Rubin (VR200) NVL72 trays provisioned by
Forge/NICo, running Ubuntu 26.04 on an arm64 `-nvidia-bos-64k` kernel. It is a
Preview coordinate.

It pairs the NVKE platform handling of `gb200-nvke-ubuntu-training` with the
VR200 node facts of `vr200-rke2-ubuntu-training`, and differs from both on the
driver: the NVKE node image pre-bakes none, so GPU Operator installs it.

## Cluster prerequisites

- Kubernetes 1.34 or newer (DRA `resource.k8s.io/v1` for ComputeDomains).
  Observed in practice: 1.36.3.
- **Ubuntu 26.04.** The recipe constrains `OS.release.VERSION_ID` to `26.04`.
- **The `nvidia-bos-64k` kernel line.** This is the hard prerequisite and the
  recipe cannot express it. See below.
- Nodes carry `providerID: nico://<uuid>`, so `aicr snapshot` resolves
  `service: nvke` without `--service`.

## The nvidia-bos-64k kernel is mandatory and unconstrainable

On the generic `nvidia-64k` kernel **no driver brings the GPUs up at all**: PRI
reads return `0xbadf5720`, FSP and GSP never hand off, and every GPU fails probe
with `-EIO`. Verified against driver 610.57.04, 615.71.09 and 615.78.08. On
`nvidia-bos-64k` the stock driver container works.

The recipe cannot gate on this. The constraint grammar has only `>=`, `<=`, `>`,
`<`, `==` and `!=` with exact match — no substring or regex — and a version range
cannot separate the two series, because `pkg/version.Compare` drops every build
suffix except `-gke.N`. So `7.0.0-1018-nvidia-64k` and
`7.0.0-2022-nvidia-bos-64k` both compare as `7.0.0`. Check the kernel yourself
before deploying:

```shell
kubectl get nodes -o wide   # KERNEL-VERSION must contain -nvidia-bos-64k
```

A kernel-flavour measurement, so the recipe can require this, is a follow-up.

## What has been measured, and what has not

**Measured.** NCCL `all_reduce_perf` over MNNVL across two NVL72 trays, 8 GPUs,
1 GiB..16 GiB: 996 GB/s average, 1040 GB/s peak, zero mismatches, with NCCL
reporting one NVLS communicator spanning both nodes (`nvlsRanks 8`).

**This is a single NVLink domain.** The number says nothing about scaling beyond
one domain, and it is two trays of an NVL72, not a full-rack figure.

**The NIC RDMA east-west path is unqualified.** The NICs are ConnectX-9, but on the trays this was
developed against every `mlx5` port reports `link_layer` Ethernet with RoCE v2
GIDs and none reports InfiniBand, so the SR-IOV InfiniBand stack that
`gb200-nvke-ubuntu-training` ships does not apply. No RDMA transport number has
been measured on the Ethernet path. The recipe therefore declares no
`SriovNetworkNodePolicy` and no `SriovNetwork`, and turns the SR-IOV operator
off rather than running it with nothing to act on.

**Host NIC stack.** The node image installs DOCA OVS and the DOCA-repo `ibverbs`
userspace, but the kernel driver is the distribution's inbox `mlx5_core`, not
DOCA-OFED. Feature parity and qualified RDMA performance with DOCA-OFED are not
claimed. Measured on both workers of the reference cluster, identical on each:

- Kernel driver: inbox `mlx5_core`, vermagic `7.0.0-2022-nvidia-bos-64k`.
  `modinfo` reports no `version:` field, which is how an in-tree module
  presents, and `ethtool -i` echoes the kernel version rather than an OFED
  release.
- Kernel: `linux-image-nvidia-bos-64k` 7.0.0-2022.23, Ubuntu 26.04.1 LTS.
- RDMA userspace: `libibverbs1` and `ibverbs-providers` both 2607.0.8-1.
  Neither `rdma-core` nor `librdmacm` is installed.
- NIC firmware: 82.48.4222 (PSID MT_0000001658) on the eight ConnectX-9 ports,
  82.48.4238 (MT_0000001875) on the BlueField ports.
- `ofed_info` is absent, confirming no DOCA-OFED install.

That is the fixed baseline the unqualified label above is measured against.

**A NicClusterPolicy is deployed, carrying only `nicConfigurationOperator`.**
It stands up the `nic-configuration-daemon` that `aicr snapshot
--discover-network` needs. No `ofedDriver` and no rails: NVKE runs the host's
inbox `mlx5_core`, and this platform's rail layout is unmeasured. The SR-IOV
operator stays off, since nothing here declares a `SriovNetwork`.

**It pins network-operator 26.7.1, not the registry default of 26.7.0.** The
daemon in 26.7.0 has no firmware-discovery fallback, so a `flint` failure is
fatal — and `flint` cannot read ConnectX-9 here. Measured on a worker: 26.7.0
exits with `failed to discover NIC devices`, while 26.7.1 logs `flint failed,
falling back to sysfs` and discovers every port. The fix is Mellanox
[c27f7ae](https://github.com/Mellanox/nic-configuration-operator/commit/c27f7ae71bb65caa93a7142e06e0343156648684)
(#499/#533), which is absent from the `ghcr.io/mellanox` `v1.3.1` images the
other in-tree policies pin, so this leaf pins the `nvcr.io/nvidia/mellanox`
`network-operator-v26.7.1` images instead. ConnectX-7 and ConnectX-8 platforms
never hit this, because `flint` works there.

Verified on the reference cluster: the daemon discovered all eight ConnectX-9
ports and wrote a `NicDevice` per port, each carrying firmware 82.48.4222,
PSID `mt_0000001658`, and its `pci`/`netdev`/`rdma` mapping.

**Not verified end to end.** The compiled-validator path for this coordinate
is exercised by CI on merge, not on hardware here.

## Known gaps

- ConnectX-9 qualification and an SR-IOV/RDMA east-west fabric for
  multi-domain training.
- A kernel-flavour measurement so `nvidia-bos-64k` can be a constraint.
- A stable VR200 product matcher; until then, mixed GPU pools need
  `--node-selector`.


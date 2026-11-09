# NVKE VR200 Setup

The `vr200-nvke-ubuntu-*` recipes target NVKE (NVIDIA Kubernetes Engine,
formerly NKE): bare-metal Vera Rubin (VR200) NVL72 trays provisioned by
Forge/NICo, running Ubuntu 26.04 on an arm64 `-nvidia-bos-64k` kernel. Four
Preview coordinates:

- `vr200-nvke-ubuntu-training`
- `vr200-nvke-ubuntu-training-kubeflow` — adds Kubeflow Trainer to the training
  leaf
- `vr200-nvke-ubuntu-inference` — platform-neutral inference base, which
  resolves when `--platform` is omitted
- `vr200-nvke-ubuntu-inference-dynamo` — adds `dynamo-platform` and `grove` to
  the inference leaf

All four declare `gang-scheduling`, a v1.36 MUST: the KAI Scheduler the check
exercises comes from the NVKE base, and a check the recipe does not name
renders as "not tested", which never establishes the requirement.

Everything below applies to all four unless a section says otherwise. See
**The inference coordinate** for what the inference leaf adds and leaves out,
and **The workload leaves** for what the other two add.

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

## The inference coordinate

`vr200-nvke-ubuntu-inference` is the same node facts at `intent: inference`. It
adds `agentgateway-crds` and `agentgateway` — the Gateway API surface — and
nothing that serves a model; actual serving is
`vr200-nvke-ubuntu-inference-dynamo`, described under **The workload leaves**.

It declares the gateway rather than expecting one on the cluster because AICR
gates `ai_inference` on the recipe: a leaf that ships no gateway records
`SKIP (prerequisite absent)` even when a healthy Gateway is Programmed, since
"a Gateway left over from another install is not evidence for this recipe".

Three things it deliberately leaves out, each matching
`vr200-rke2-ubuntu-inference`:

- **No performance block.** An all-reduce floor is a training claim; the MNNVL
  measurement stays with the training coordinate.
- **No AI operator.** `robust_controller_check.go` takes its subject from the
  recipe — `kubeflow-trainer`, else `dynamo-platform`, else skip — so the check
  skips for this leaf, which is why the conformance list does not schedule it.
  `vr200-nvke-ubuntu-inference-dynamo` is the leaf that establishes
  `robust_controller` for this coordinate.
- **No east-west NIC stack.** Serving through the Gateway API needs no RoCE
  rails, so neither `network-operator` nor `sriov-ib-networks` is declared.

### External address for the gateway

NVKE provisions no LoadBalancer controller, and the Service the controller
generates for the Gateway is `type: LoadBalancer`. A bare-metal LB
implementation (MetalLB, kube-vip) is what gives it an external address.
Without one the Service stays at `EXTERNAL-IP <pending>` and the gateway is
reachable only in-cluster or through its NodePort.

That is a reachability limit, not a conformance one. agentgateway v1.5.0
reports `Programmed=True` when no negative condition is present, regardless of
address allocation, and `inference_gateway_check.go` gates readiness on ready
endpoints rather than an external address — its LoadBalancer handling inspects
`spec.loadBalancerSourceRanges` for internet exposure and records
"No LoadBalancer Service" as an observation.

**The Gateway is a separate bundle step.** `agentgateway` renders as two
releases: the Helm chart, which installs the controller and the GatewayClass,
and `agentgateway-post`, which applies
`recipes/components/agentgateway/manifests/inference-gateway.yaml` and is what
creates `agentgateway-system/inference-gateway`. The chart alone creates no
Gateway, so an install or upgrade that runs only the `helm upgrade` leaves the
`inference-gateway` check with nothing to read. `aicr validate --phase
deployment` catches the result: the agentgateway health check asserts that
`agentgateway-system/inference-gateway` exists with `Accepted=True` and
`Programmed=True`, so a cluster with no Gateway, or an unprogrammed one, fails
the deployment phase ([#3714](https://github.com/NVIDIA/aicr/issues/3714)).
The check detects a missing or unprogrammed Gateway, not which deployment
created it; a healthy Gateway left by an earlier deployment satisfies it.

On an LB-less tenant, keep the step rather than skipping it, and generate the
bundle with `--set agentgateway:serviceType=ClusterIP`. The controller then
programs the Gateway onto a ClusterIP Service, the shape the NVKE GB200
cluster runs, and the conformance check records "No LoadBalancer Service".
The knob is not what makes the health check pass: as noted above, agentgateway
v1.5.0 reports `Programmed=True` without an address. It avoids a Service that
sits at `EXTERNAL-IP <pending>` forever and a LoadBalancer-only field the
cluster cannot honor; with `ClusterIP` the rendered Service carries no
`loadBalancerSourceRanges`. Never edit the generated bundle to get there;
`aicr verify` checksums every file in it.

### Evidence

Collected on a two-worker VR200 tenant cluster with no LoadBalancer present,
with `ai_inference` PASS and `Programmed=True`. The set is pending in
[#3695](https://github.com/NVIDIA/aicr/pull/3695) and is being recollected
against the current collector, so no result count is quoted here.
`cluster_autoscaling` skips on bare metal. #3695 also records two narrower
results — `robust_controller` was credited to an operator this recipe does not
declare, which the collector it ran under allowed and the current one does not,
and `dra_support` passed without its MNNVL behavioral subtest because no pod
held the ComputeDomain channel at collection time.

## The workload leaves

`vr200-nvke-ubuntu-training-kubeflow` and `vr200-nvke-ubuntu-inference-dynamo`
exist so this coordinate can establish `robust_controller`, a v1.36 MUST. The
check takes its subject from the recipe — `kubeflow-trainer`, else
`dynamo-platform`, else skip — and the two plain leaves declare no AI operator,
so a submission from either stops short of the full MUST set. Each workload
leaf adds the operator and declares the check, following
`gb200-nvke-ubuntu-training-kubeflow` and
`vr200-rke2-ubuntu-inference-dynamo`.

- **`-training-kubeflow`** takes the `platform-kubeflow` mixin, which brings
  Kubeflow Trainer and the torch-distributed `ClusterTrainingRuntime` the check
  reconciles a `TrainJob` against. It adds no `componentRefs` of its own.
- **`-inference-dynamo`** adds `dynamo-platform` and `grove` to the inference
  leaf — the serving platform the base deliberately leaves out.

Both establish the requirement. Measured on 2026-11-07 against a VR200 NVKE
tenant cluster deployed from the `-inference-dynamo` bundle, the Dynamo path
reports `robust_controller: Implemented` with no documented entry configured,
on operator pods, an Established CRD and a webhook that rejects an invalid
custom resource. Only the CR-reconciliation extra is observation-only there,
because a `DynamoGraphDeployment` cannot reach a terminal condition without a
model and GPU workers; the Kubeflow path adds a `TrainJob` reconciled to
`Complete` on top of the same base.

Neither carries a performance block. Nothing has been measured on VR200 under
Dynamo, and a throughput or TTFT floor copied from another accelerator would be
a claim about hardware these leaves have never run on.

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
been measured on the Ethernet path. What the recipe declares instead is RoCE:
a `SriovNetworkNodePolicy` and a `SriovNetwork` per rail with the SR-IOV
operator on, deployed and attachable but carrying no traffic — see below.

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
  82.48.4238 (MT_0000001875) on the BlueField ports. **This baseline predates
  the current stable line.** NVIDIA's published ConnectX-9 firmware runs
  82.48.1000 (February 2026 GA), 82.49.1014 (June 2026) and 82.50.1002, so
  82.50 is current and 82.48 is two branches behind it. The build number here
  (4222) sits far above the 82.48 GA build (1000), which is the shape of a
  pre-GA build for these trays rather than a stale GA, so confirm
  applicability before treating 82.50 as a drop-in upgrade. This recipe sets
  no firmware: there is no firmware template in the NicClusterPolicy, so the
  version is whatever the node image and tray ship.
- `ofed_info` is absent, confirming no DOCA-OFED install.

That is the fixed baseline the unqualified label above is measured against.

**A NicClusterPolicy is deployed, carrying `nicConfigurationOperator`, `nvIpam` and `secondaryNetwork`.**
It stands up the `nic-configuration-daemon` that `aicr snapshot
--discover-network` needs, plus the addressing and multus the RoCE rails
attach through. Still no `ofedDriver`: NVKE runs the host's inbox `mlx5_core`.

**Eight ConnectX-9 RoCE rails: deployed and attachable, but carrying no
traffic.** The leaf ships an nv-ipam `IPPool`, a `SriovNetworkNodePolicy` and a
`SriovNetwork` per rail, and a `nccl-all-reduce-bw-net` check to exercise them.
On the reference cluster the operator created one VF per port on both workers
and advertised all eight as allocatable, nv-ipam assigned an address per rail,
multus attached `net1`..`net8`, and each VF came up with its own RDMA device
and a RoCE v2 GID at index 1. No packet has crossed between nodes: the spare
ConnectX-9 ports have no path between workers, because the BlueField DPU
bridges only the port carrying the node address, and bridging the rest is
DPU-side work outside the tenant cluster. So the `>= 50` floor is a
placeholder, and the only collective path proven here is MNNVL inside one
NVLink domain.

**These `SriovNetwork`s set no VF attributes, deliberately.** GB200's set
`linkState: enable`; on this platform the PF driver refuses it, and `trust` and
`spoofChk` too, failing the attachment with `operation not permitted`. The PF
is owned by the BlueField DPU, so the host can create VFs but cannot
administer them. With all three omitted the attachment succeeds.

**`NCCL_IB_GID_INDEX` must be set.** The RoCE v2 GID sits at index 1 on these
ports; NCCL defaults to 0, which holds the v1 GID.

`numVfs` is 1, which is the ConnectX-9 firmware's `sriov_totalvfs` on these
trays rather than a tuning choice — eight ports give eight VFs, so one worker
pod takes them all.

**If the rails are ever removed, clear the multus CNI config too.** Deleting a
NicClusterPolicy carrying `secondaryNetwork` removes multus's DaemonSet and
RBAC but leaves `/etc/cni/net.d/00-multus.conflist` on every node, which sorts
ahead of OVN-Kubernetes and fails every subsequent pod creation cluster-wide
with `Unauthorized`. This was observed on this cluster.

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

## Withholding the network check

`nccl-all-reduce-bw-net` runs on this leaf: `vr200/nvke` is in the compiled
`supportedNCCLCombinations[variantNET]` matrix and the recipe declares the
check. On a tenant where the DPU bridges only the port carrying the node
address, the rails have no path between workers, so the check fails the
performance phase. Withhold it on those tenants:

```bash
aicr validate --recipe ./bundles/recipe.yaml --phase performance \
  --skip-check nccl-all-reduce-bw-net
```

Keep it withheld until the DPU bridges the spare ConnectX-9 ports.

Use the flag rather than dropping the constraint from the recipe. The flag
records a limitation of one run; editing the recipe changes the check for
everyone who consumes this leaf.

One consequence: `--skip-check` cannot be combined with `--evidence-dir`, and
so not with `--cncf-submission`, because a run that withholds a check the
recipe declares is narrower than the recipe and cannot evidence it. A run that
withholds this check therefore produces no conformance evidence for this
recipe. The ordinary CTRF report still records the check as skipped, and so
does a signed recipe attestation (`--emit-attestation`), which carries the
`named-in-skip-checks` reason code through bundle redaction.

## Known gaps

- ConnectX-9 qualification and an SR-IOV/RDMA east-west fabric for
  multi-domain training.
- A kernel-flavour measurement so `nvidia-bos-64k` can be a constraint.
- A stable VR200 product matcher; until then, mixed GPU pools need
  `--node-selector`.


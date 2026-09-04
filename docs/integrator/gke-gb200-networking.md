# GKE GB200 (A4X) Networking Prerequisites

For the **GB200 GKE COS** recipes (`gb200-gke-cos-training`,
`gb200-gke-cos-training-kubeflow`, `gb200-gke-cos-training-slurm`,
`gb200-gke-cos-inference`, and `gb200-gke-cos-inference-dynamo`, all on
`a4x-highgpu-4g` nodes),
GPUDirect-RDMA over RoCE enables high-speed inter-node GPU communication on
GKE. The recipe's NCCL workloads set `NCCL_NET=gIB` explicitly (see
`recipes/components/gke-gb200-rdma/manifests/nccl-gib-installer-arm64.yaml`)
rather than letting NCCL auto-select a plugin, so a missing or
misconfigured RDMA fabric doesn't silently fall back to a slower network
path: it fails outright.

GPUDirect RDMA on `a4x-highgpu-4g` is also incompatible with NCCL Fast
Socket and the GPUDirect TCPX/TCPXO plugin (see
[GKE TCPXO Networking](gke-tcpxo-networking.md) for that alternative,
non-RDMA path); don't enable either on a cluster that uses RDMA.

## Infrastructure Prerequisites

GKE clusters must have multi-networking configured before deploying AICR bundles:

- Multi-networking enabled (1 gVNIC + 4 RDMA NICs per `a4x-highgpu-4g` node)
- `Network` + `GKENetworkParamSet` CRs for the gVNIC and 4 RDMA NICs (cluster-specific
  VPC/subnet values, but fixed object names; see below, not managed by AICR)
- `nccl-rdma-installer` DaemonSet on GPU nodes (included in the AICR bundle)
- Each GPUDirect-RDMA workload Pod must request all 4 GPUs and use all 4 RDMA NICs
  on a single node; RDMA can't be shared between Pods on the same node (a GKE
  `a4x-highgpu-4g` constraint, not an AICR-specific one). AICR's own recipes
  already request whole nodes this way; a custom workload built against this
  component must too.

The `nccl-rdma-installer` DaemonSet ships in the AICR bundle. The `Network`/
`GKENetworkParamSet` CRs and the multi-networking/VPC fabric underneath them
are **cluster provisioning**: AICR's `gke-gb200-rdma` health check detects
them but does not create them.

### Provisioning multi-networking

These steps are ordered, following Google's
[A4X custom setup guide](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom-a4x):

1. **Create the VPCs and subnets**: two VPCs in the cluster's region, one for
   the gVNIC (with one subnet) and one RDMA VPC (with four subnets, one per
   RDMA NIC); five subnets total across the two VPCs, not five separate VPCs.
2. **Create the cluster** with multi-networking enabled (HIPPO's `GKECluster` CR
   does this via `spec.networks.managed.gb200NetworkStrategy`).
3. **Create the GPU node pool** on an `a4x-highgpu-4g` machine type, attaching
   the five network/subnet pairs as `additionalNodeNetworkConfigs` (the RDMA
   VPC repeated across its four subnets, plus the gVNIC VPC/subnet).
4. **Apply the `Network` and `GKENetworkParamSet` CRs**: one pair per NIC,
   binding each additional node network into the cluster so pods can reference
   it. Unlike TCPXO (see [GKE TCPXO Networking](gke-tcpxo-networking.md)), the
   **object names are fixed, not cluster-specific**: `gvnic-1` for the gVNIC and
   `rdma-0` through `rdma-3` for the RDMA NICs. Only the `vpc`/`vpcSubnet` fields
   inside each `GKENetworkParamSet` vary per cluster (they name the VPC/subnet
   your cluster actually has):

```yaml
apiVersion: networking.gke.io/v1
kind: GKENetworkParamSet
metadata:
  name: gvnic-1
spec:
  vpc: "PREFIX-gvnic"
  vpcSubnet: "PREFIX-gvnic"
  deviceMode: NetDevice
---
apiVersion: networking.gke.io/v1
kind: Network
metadata:
  name: gvnic-1
spec:
  type: "Device"
  parametersRef:
    group: networking.gke.io
    kind: GKENetworkParamSet
    name: gvnic-1
```

   Repeat for `rdma-0` through `rdma-3`, pointing `vpc` at the single RDMA VPC
   from step 1 (the same value for all four) and `vpcSubnet` at that VPC's
   four subnets (`PREFIX-rdma-sub-0` through `PREFIX-rdma-sub-3`, or whatever
   names your subnets were given in step 1, with `PREFIX` replaced by your
   own), and set **`deviceMode: RDMA`** on all four, not `NetDevice` (that
   value is only correct for `gvnic-1` above).

> **The fixed naming is a requirement, not a convention.** AICR's
> `checks/gke-gb200-rdma/health-check.yaml` asserts these five objects by exact
> name (`gvnic-1`, `rdma-0`..`rdma-3`), including `spec.deviceMode` and
> `spec.parametersRef` linkage. A cluster provisioned with different `Network`
> names passes Google's own setup guide but fails this check; rename to match
> before running `aicr validate`.

AICR installs the `nccl-rdma-installer` DaemonSet and detects the CRs; it does
not provision the networking itself. These steps are a summary of the
prerequisite AICR depends on, not a complete provisioning runbook; follow
Google's guide above for the full procedure, including firewall rules and
supported GKE version floors.

Separately from GKE's own networking version floor, all AICR GB200 GKE
recipes (including `gb200-gke-cos-training-slurm`, which inherits it from
`gb200-gke-cos-training`) enforce `K8s.server.version >= 1.34`: NVLS
provisions the IMEX channel through a DRA `ComputeDomain`, which requires
the GA `resource.k8s.io/v1` API. `aicr validate` fails readiness on an
older control plane with this constraint by name.

### Verifying

```shell
kubectl get network.networking.gke.io \
  -o custom-columns='NAME:.metadata.name,PARAMETERS-REF:.spec.parametersRef.name'
kubectl get gkenetworkparamset.networking.gke.io \
  -o custom-columns='NAME:.metadata.name,DEVICE-MODE:.spec.deviceMode'
```

Expect `gvnic-1` and `rdma-0` through `rdma-3` (the five prerequisite
`Network`s from step 4), each bound to its `GKENetworkParamSet` via
`spec.parametersRef` (shown in the `PARAMETERS-REF` column above). Fewer
than five, or a `GKENetworkParamSet` with the wrong `DEVICE-MODE`, means
the prerequisite is incomplete or misconfigured; `aicr validate` (via the
`gke-gb200-rdma` health check) reports the shortfall by name.

You'll also see a `default` network/`GKENetworkParamSet` pair in the same
output; that one is GKE-managed (created automatically once
multi-networking is enabled), not part of this prerequisite, and isn't
checked by name.

## Driver Installer

`a4x-highgpu-4g` recipes generated with `--profile gpuStack=bundle-installer`
(see [GKE GPU Setup](gke-gpu-setup.md#alternative-let-the-bundle-own-the-gpu-stack))
get the driver from the bundle's `gcp-driver-installer` component — Google's
cos-gpu-installer DaemonSet, deployed and versioned by AICR alongside the
rest of the bundle, no manual DaemonSet apply required. This presumes the
node-pool prerequisite (pools created with `gpu-driver-version=disabled`
plus the `gke-no-default-nvidia-gpu-device-plugin=true` label) is already in
place; the component's `nodeAffinity` requires that label itself, so it
never schedules onto a pool that hasn't opted out of GKE's managed install.

**GB200-specific wrinkle:** the component's default `partitionGpuImage` (its
`partition-gpus` init container, Google's `nvidia-partition-gpu` MIG tool —
a no-op here since this recipe allocates whole GPUs per node rather than
configuring MIG) is pinned to an amd64-only digest and fails with
`exec format error` on GB200's arm64 nodes. AICR's `gb200-gke-cos-training`
and `gb200-gke-cos-inference` overlays (and everything based on them) set
`gcp-driver-installer.partitionGpuImage` to a multi-arch digest
automatically — found via live GB200 GKE validation, no action needed:

```yaml
partitionGpuImage: "gcr.io/gke-release/nvidia-partition-gpu@sha256:de12f85ebfb4fb6c1893cd30c23aab662a72fa0448f97ef74fccb82d7522ef17"
```

The driver version itself (`gcp-driver-installer.driverVersion`) needs no
GB200-specific override: the component's default is
[COS-qualified](https://cloud.google.com/kubernetes-engine/docs/how-to/gpus#cos)
for GB200 (see the component's `values.yaml` for the qualified COS builds).

### Validate the RDMA prerequisite before deploying

The `gcp-driver-installer` component deploys and orders itself ahead of the
GPU Operator automatically within `deploy.sh` (see [Driver
Installer](#driver-installer)) — there's nothing to hand-apply or pre-check
for the driver anymore. The RDMA `Network`/`GKENetworkParamSet` CRs are
still a true cluster-provisioning prerequisite applied before the node pool
exists (see [Provisioning multi-networking](#provisioning-multi-networking)
above); confirm they're in place before running the bundle's full
`deploy.sh`:

```shell
aicr validate --recipe recipe.yaml --phase deployment --fail-fast
```

The `gke-gb200-rdma` health check only needs the RDMA CRs to exist, not the
rest of the bundle deployed, so this catches un-applied CRs in seconds
instead of surfacing them deep into a 20-component deploy. `--fail-fast`
stops there instead of continuing on to conformance and performance (see
[Validation](../user/validation.md)). Note `check-nvidia-smi` in the same
`deployment` phase only passes once the driver installer has actually run
(inside `deploy.sh`, not before it), so don't run this check before the
node pool itself exists — expect `check-nvidia-smi` to fail until
`deploy.sh` has deployed `gcp-driver-installer`.

## Storage Prerequisites

`a4x-highgpu-4g` nodes can't attach Persistent Disk at all (regional or
zonal, any type, including `pd-balanced`); only Hyperdisk. On a stock GKE
Standard cluster the default StorageClass is `standard-rwo`
(`pd.csi.storage.gke.io`, `pd-balanced`), but "default" isn't inherent to
GKE Standard itself: a cluster admin can repoint the
`storageclass.kubernetes.io/is-default-class` annotation to any
StorageClass. Run `kubectl get storageclass` first and check which one is
annotated `(default)`, its `PROVISIONER`, and (via `kubectl get
storageclass -o yaml`) its `parameters.type`; don't assume it's
`standard-rwo`/`pd-balanced`. Any PVC scheduled onto a GB200 node with no
`storageClassName` set (which binds it to the cluster default) fails
this way unless that default's `parameters.type` is already
Hyperdisk-backed: `pd-balanced disk type cannot be used by
a4x-highgpu-4g machine type` (or the equivalent for whatever `pd-*` type
the default actually provisions).

This includes the `inference-perf` validator's model-weights cache PVC
when `AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS` (see
[Validation](../user/validation.md)) is left unset, it then falls back
to the cluster default too. Set that variable to name a Hyperdisk-backed
StorageClass explicitly (for example `hyperdisk-balanced`, applied below)
and the cache PVC uses it directly via `storageClassName`, independent of
whatever the cluster default resolves to.

If the cluster default isn't already Hyperdisk-backed, apply one. Like
the RDMA CRs above, this is a cluster prerequisite AICR does not
provision:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hyperdisk-balanced
provisioner: pd.csi.storage.gke.io
parameters:
  type: hyperdisk-balanced
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
```

Apply it once per cluster, then point the validator's model cache at it via
an `AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS=hyperdisk-balanced` entry
on the `inference-perf` catalog entry's `env` (or a catalog overlay in the
`aicr validate --data <dir>` directory).

## Running the NCCL Benchmark

The GB200 GKE training recipe (`gb200-gke-cos-training`) selects the
NVLS-variant performance check (`nccl-all-reduce-bw-nvls`): MNNVL across the
A4X nodes' IMEX domain is the fabric that carries all-reduce traffic; gIB is the
transport driver underneath, not the NCCL algorithm itself. Run it via:

```shell
aicr validate --recipe recipes/overlays/gb200-gke-cos-training.yaml \
  --phase performance
```

## References

- [GKE A4X custom setup guide](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom-a4x)
- [Component Catalog](../user/component-catalog.md)
- [Validation readiness gate](../user/validation.md)
- [GKE TCPXO Networking](gke-tcpxo-networking.md)

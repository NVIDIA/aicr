# GKE A4X Max (GB300) Networking Prerequisites

For the **GB300 GKE COS** recipes (`gb300-gke-cos-training`,
`gb300-gke-cos-training-kubeflow`, `gb300-gke-cos-inference`, and
`gb300-gke-cos-inference-dynamo`, all on `a4x-maxgpu-4g-metal` bare-metal
nodes), inter-node GPU traffic runs over GPUDirect RDMA on RoCE, and
intra-rack GPU traffic runs over Multi-Node NVLink (MNNVL) through an IMEX
domain provisioned as a DRA `ComputeDomain`.

**A4X Max is not A4X.** Both are Grace-Blackwell NVL72 racks, but the GKE
networking path is different, and the two are not interchangeable:

| | A4X (GB200, `a4x-highgpu-4g`) | A4X Max (GB300, `a4x-maxgpu-4g-metal`) |
|---|---|---|
| Node form | VM | Bare metal |
| RDMA attachment | Multi-networking: `Network` + `GKENetworkParamSet` pairs named `gvnic-1`, `rdma-0`..`rdma-3` | DRANET: `ResourceClaimTemplate` against the `mrdma.google.com` DeviceClass |
| NIC setup | No in-cluster component | `asapd-lite` DaemonSet |
| Pod wiring | Pod network annotations | DRA resource claims |

The A4X column shows the multi-networking path used by Google's A4X guide and
AICR's GB200 recipes; A4X also supports GKE managed DRANET. A4X Max has no
such choice: multi-networking **isn't supported** for the
`a4x-maxgpu-4g-metal` machine type, so none of the
`Network`/`GKENetworkParamSet` setup described in
[GKE GB200 Networking](gke-gb200-networking.md) applies here. Follow this
page instead.

These steps summarize the prerequisites AICR depends on. They are not a
complete provisioning runbook — follow Google's
[Create an AI-optimized GKE cluster that uses A4X Max](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-create-a4x-max)
guide for the full procedure, including cluster creation, the workload
policy, and reservation affinity.

## What AICR installs, and what it doesn't

AICR's GB300 GKE bundle installs the in-cluster GPU stack. Everything in the
second column is a **workload prerequisite** that you provision: RDMA and
MNNVL workloads cannot run without it, but `aicr validate` does not check
most of it.

| Installed by the AICR bundle | Your responsibility |
|---|---|
| `nvidia-dra-driver-gpu` (ComputeDomain CRD + DRA driver, `nvidiaDriverRoot: /home/kubernetes/bin/nvidia`) | Standard cluster created with GKE Dataplane V2, and a node pool created with the A4X Max flags below |
| `gpu-operator` (host-managed COS driver posture: `driver.enabled: false`, `cdi.enabled: true`) | `asapd-lite` DaemonSet |
| `nodewright-operator` + `nodewright-customizations` (Grace-Blackwell host kernel tuning) | GKE managed DRANET enabled on the node pool |
| `nfd`, `cert-manager`, `kube-prometheus-stack` | Per-workload `ComputeDomain` and `ResourceClaimTemplate` objects |

> Because AICR already deploys `nvidia-dra-driver-gpu`, do **not** also run
> the ResourceQuota and `helm install nvidia-dra-driver-gpu` steps from
> Google's guide on a cluster you intend to manage with AICR. Two installs of
> the same driver contend for the same CRDs, and the quota is scoped to the
> `system-node-critical` priority class, which AICR's install doesn't use.

Of the prerequisites in the second column, `aicr validate` needs only the GPU
nodes and a control plane that meets the [GKE version floor](#gke-version-floor).
It can pass without the node pool's networking flags, `asapd-lite`, managed
DRANET, or any workload `ComputeDomain` and `ResourceClaimTemplate`. The
`dra-support` check verifies the NVIDIA DRA driver and counts only
`*.nvidia.com` ResourceSlices, so it never sees the RDMA devices behind the
`mrdma.google.com` DeviceClass. On nodes labeled `nvidia.com/gpu.clique` it
also tests IMEX channel allocation, using a temporary `ComputeDomain` of its
own. The GB300 GKE recipes run no performance check, so nothing exercises
inter-node RDMA. Confirm the networking prerequisites with the commands in
[Verifying](#verifying).

## GKE version floor

All four GB300 GKE recipes carry this constraint:

```yaml
constraints:
  - name: K8s.server.version
    value: ">= 1.34.3-gke.1318000 < 1.35 || >= 1.35.0-gke.2745000"
```

The two alternatives are the per-branch minimums published in the
[A4X Max requirements](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-create-a4x-max#requirements):
on the 1.34 branch use 1.34.3-gke.1318000 or later, and on 1.35 or later use
1.35.0-gke.2745000 or later. A single `>= 1.34.3-gke.1318000` would wrongly
admit early 1.35 patches, so the floor is expressed as two alternatives
rather than one range.

Google's requirements say these versions help ensure that A4X Max uses GPU
driver R580.95.05 (the A4X Max minimum) and Coherent Driver-based Memory
Management (CDMM), both enabled by default, as well as GPUDirect RDMA and
MNNVL. `aicr validate` fails readiness on an older control plane, naming this
constraint.

> CDMM is on by default on these versions and is incompatible with
> Multi-Instance GPU. Don't plan MIG partitioning on A4X Max.

## Infrastructure prerequisites

- **GKE Dataplane V2, enabled at cluster creation.** Managed DRANET and
  accelerator network profiles both require it, and it can't be enabled on an
  existing cluster. Google's cluster-create command also disables shielded
  nodes (`--no-enable-shielded-nodes`).
- **Standard cluster, fixed-size node pool.** Bare-metal nodes such as A4X
  Max are incompatible with Autopilot clusters and with autoscaling.
- **Container-Optimized OS only.** Ubuntu and Windows node images are not
  supported on `a4x-maxgpu-4g-metal`.
- **Reservation-bound provisioning.** Other provisioning models are not
  supported. Google recommends a specific, sub-block-targeted reservation, as
  in the command below.
- **A `HIGH_THROUGHPUT` workload policy** with `accelerator-topology` `1x72`,
  so the node pool lands in one NVLink domain.
- **Whole-node RDMA.** A Pod must request all four GPUs and all of the node's
  secondary NICs. RDMA cannot be shared between Pods on one node. AICR doesn't
  add these requests for you: each workload must specify its own four-GPU limit
  and RDMA resource claim, including TrainJobs that use the bundled
  `torch-distributed` runtime.
- **Hugepages pre-allocated** on the node pool.

### Creating the node pool

The networking-relevant flags are `--accelerator-network-profile=auto` and
the two node labels:

```shell
gcloud container node-pools create NODE_POOL_NAME \
    --cluster=CLUSTER_NAME \
    --location=COMPUTE_REGION \
    --node-locations=COMPUTE_ZONE \
    --num-nodes=NODE_COUNT \
    --placement-policy=WORKLOAD_POLICY_NAME \
    --machine-type=a4x-maxgpu-4g-metal \
    --accelerator=type=nvidia-gb300,count=4,gpu-driver-version=latest \
    --system-config-from-file=node_custom.yaml \
    --accelerator-network-profile=auto \
    --node-labels=cloud.google.com/gke-networking-dra-driver=true,cloud.google.com/gke-dpv2-unified-cni=cni-migration \
    --reservation-affinity=specific \
    --reservation=RESERVATION_NAME/reservationBlocks/BLOCK_NAME/reservationSubBlocks/SUB_BLOCK_NAME
```

`NODE_COUNT` must be 18 or fewer; 18 gives the full `1x72` GPU topology in a
single sub-block. `node_custom.yaml` pre-allocates hugepages:

```yaml
linuxConfig:
  hugepageConfig:
    hugepage_size2m: 4096
```

`--accelerator-network-profile=auto` builds the accelerator network for you
and labels the nodes `gke.networks.io/accelerator-network-profile: auto`. It
requires the GKE service account in your project to have the
`compute.networkAdmin` role. Google's guide requires workloads to include the
profile label in their `nodeSelector`; the Pod example below does.

`cloud.google.com/gke-networking-dra-driver=true` is what turns on GKE
managed DRANET for the pool. Without it, no `mrdma.google.com` devices are
published and every RDMA resource claim stays pending.

### Installing `asapd-lite`

The `asapd-lite` DaemonSet configures the MRDMA NICs. Without it, child
interfaces aren't created and DRANET filters out the RDMA NICs, so Pods get
no RDMA.

```shell
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/refs/heads/master/asapd-lite-installer/asapd-lite-installer-a4x-max-bm-cos.yaml
kubectl get daemonset -n kube-system asapd-lite
```

`READY` should equal `DESIRED`, which counts every A4X Max node in the
cluster, not only this pool. For one 18-node pool:

```
NAME         DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR   AGE
asapd-lite   18        18        18      18           18          <none>          5m
```

This DaemonSet is **not** part of the AICR bundle.

### Verifying

```shell
kubectl get deviceclasses.resource.k8s.io mrdma.google.com
kubectl get resourceslices.resource.k8s.io \
    -o custom-columns=NODE:.spec.nodeName,DRIVER:.spec.driver
```

Expect the `mrdma.google.com` DeviceClass (GKE installs the networking
DeviceClasses automatically on supported versions) and `ResourceSlice`
objects publishing RDMA devices for each A4X Max node. On an AICR cluster the
list also includes `*.nvidia.com` slices from AICR's NVIDIA DRA driver, so a
non-empty list doesn't mean RDMA is available: use the `DRIVER` column to
find the DRANET slices for each node. If an A4X Max node has none, or RDMA
resource claims stay pending, recheck the prerequisites on this page:

- `asapd-lite` is Ready on the node.
- The node has the `cloud.google.com/gke-networking-dra-driver=true` label.
- The node pool was created with `--accelerator-network-profile=auto`.
- The cluster was created with GKE Dataplane V2.

AICR doesn't test RDMA on these recipes. For an end-to-end check, run
Google's NCCL/gIB test:
[Run NCCL on GKE clusters that use A4X Max](https://docs.cloud.google.com/ai-hypercomputer/docs/nccl/test-gke-custom-a4x-max).

## Workload wiring

AICR installs the NVIDIA DRA driver, but the `ComputeDomain` and the RDMA
`ResourceClaimTemplate` are per-workload objects that the workload author
creates. The `ComputeDomain` is required for MNNVL (IMEX channels), and the
RDMA claim for GPUDirect RDMA.

A `ComputeDomain` provisions the IMEX channel for MNNVL:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: a4x-max-compute-domain
spec:
  numNodes: NUM_NODES
  channel:
    resourceClaimTemplate:
      name: a4x-max-compute-domain-channel
```

A `ResourceClaimTemplate` requests the node's RDMA NICs through DRANET:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: all-mrdma
spec:
  spec:
    devices:
      requests:
        - name: req-mrdma
          exactly:
            deviceClassName: mrdma.google.com
            allocationMode: ExactCount
            count: 8
```

The Pod then claims both, requests all four GPUs, pins itself to `arm64`
nodes in the accelerator network profile pool, and mounts the host driver
directory:

```yaml
spec:
  nodeSelector:
    gke.networks.io/accelerator-network-profile: auto
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kubernetes.io/arch
                operator: In
                values:
                  - arm64
  volumes:
    - name: library-dir-host
      hostPath:
        path: /home/kubernetes/bin/nvidia
  containers:
    - name: my-container
      volumeMounts:
        - name: library-dir-host
          mountPath: /usr/local/nvidia
      env:
        - name: LD_LIBRARY_PATH
          value: /usr/local/nvidia/lib64
      resources:
        limits:
          nvidia.com/gpu: 4
        claims:
          - name: compute-domain-channel
          - name: rdma
  resourceClaims:
    - name: compute-domain-channel
      resourceClaimTemplateName: a4x-max-compute-domain-channel
    - name: rdma
      resourceClaimTemplateName: all-mrdma
```

The `arm64` affinity and the `/home/kubernetes/bin/nvidia` host mount are not
optional: A4X Max nodes are Grace (ARM64), and on COS the NVIDIA GPU driver
and its userspace libraries are host-managed rather than shipped in the
container.

The RDMA userspace is the exception. Unlike A4X, Google's A4X Max guide
installs it in the container image rather than on the host: build your image
with the `doca-ofed-userspace` package (arm64-sbsa build) and the latest NCCL
(`libnccl2`, `libnccl-dev`). See
[Configure your workload manifest for RDMA and IMEX domain](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-create-a4x-max#configure-manifest-rdma-imex)
for the installation steps.

## References

- [Create an AI-optimized GKE cluster that uses A4X Max](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-create-a4x-max)
- [Allocate network resources by using GKE managed DRANET](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/allocate-network-resources-dra)
- [Configure automated networking for accelerator VMs](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/config-auto-net-for-accelerators)
- [NVIDIA DRA Driver for GPUs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-cds.html)

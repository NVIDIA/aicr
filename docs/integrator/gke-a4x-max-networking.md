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
| NIC init | Host hypervisor | `asapd-lite` DaemonSet in-cluster |
| Pod wiring | Pod network annotations | DRA resource claims |

Multi-networking **isn't supported** for the `a4x-maxgpu-4g-metal` machine
type, so none of the `Network`/`GKENetworkParamSet` setup described in
[GKE GB200 Networking](gke-gb200-networking.md) applies here. Follow this
page instead.

These steps summarize the prerequisites AICR depends on. They are not a
complete provisioning runbook — follow Google's
[Create an AI-optimized GKE cluster that uses A4X Max](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom-a4x-max)
guide for the full procedure, including reservations, firewall rules, and
storage.

## What AICR installs, and what it doesn't

AICR's GB300 GKE bundle installs the in-cluster GPU stack. Everything in the
second column is **cluster provisioning** that must already be in place
before `aicr validate` can pass.

| Installed by the AICR bundle | Your responsibility |
|---|---|
| `nvidia-dra-driver-gpu` (ComputeDomain CRD + DRA driver, `nvidiaDriverRoot: /home/kubernetes/bin/nvidia`) | Node pool created with the A4X Max flags below |
| `gpu-operator` (host-managed COS driver posture: `driver.enabled: false`, `cdi.enabled: true`) | `asapd-lite` DaemonSet |
| `nodewright-operator` + `nodewright-customizations` (Grace-Blackwell host kernel tuning) | GKE managed DRANET enabled on the node pool |
| `nfd`, `cert-manager`, `kube-prometheus-stack` | Per-workload `ComputeDomain` and `ResourceClaimTemplate` objects |

> Because AICR already deploys `nvidia-dra-driver-gpu`, do **not** also run
> the `helm install nvidia-dra-driver-gpu` step from Google's guide on a
> cluster you intend to manage with AICR. Two installs of the same driver
> contend for the same CRDs.

AICR's `dra-support` conformance check detects that the DRA driver and
ComputeDomain support are live; it does not create the infrastructure.

## GKE version floor

All four GB300 GKE recipes carry this constraint:

```yaml
constraints:
  - name: K8s.server.version
    value: ">= 1.34.3-gke.1318000 < 1.35 || >= 1.35.0-gke.2745000"
```

The two alternatives are the per-branch minimums published in the
[A4X Max requirements](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom-a4x-max#requirements):
on the 1.34 branch use 1.34.3-gke.1318000 or later, and on 1.35 or later use
1.35.0-gke.2745000 or later. A single `>= 1.34.3-gke.1318000` would wrongly
admit early 1.35 patches, so the floor is expressed as two alternatives
rather than one range.

Below those versions a cluster does not get the A4X Max defaults the recipe
assumes: GPU driver R580.95.05 or later, Coherent Driver-based Memory
Management (CDMM), and GPUDirect RDMA plus MNNVL enablement. `aicr validate`
fails readiness on an older control plane, naming this constraint.

> CDMM is on by default on these versions and is incompatible with
> Multi-Instance GPU. Don't plan MIG partitioning on A4X Max.

## Infrastructure prerequisites

- **Container-Optimized OS only.** Ubuntu and Windows node images are not
  supported on `a4x-maxgpu-4g-metal`.
- **Reservation-bound provisioning.** Other provisioning models are not
  supported; the node pool must target a reservation sub-block.
- **A `HIGH_THROUGHPUT` workload policy** with `accelerator-topology` `1x72`,
  so the node pool lands in one NVLink domain.
- **Whole-node RDMA.** A Pod must request all four GPUs and all of the node's
  secondary NICs. RDMA cannot be shared between Pods on one node. AICR's own
  recipes already request whole nodes; a custom workload built against these
  recipes must too.
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
and labels the nodes `gke.networks.io/accelerator-network-profile: auto`.
**Workloads must carry that label in their `nodeSelector`**, or they will not
schedule onto the pool.

`cloud.google.com/gke-networking-dra-driver=true` is what turns on GKE
managed DRANET for the pool. Without it, no `mrdma.google.com` devices are
published and every RDMA resource claim stays pending.

### Installing `asapd-lite`

The `asapd-lite` DaemonSet initializes the MRDMA NICs. Until it is healthy
there is no RDMA connectivity, and DRANET filters the uninitialized NICs out
of its device inventory.

```shell
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/refs/heads/master/asapd-lite-installer/asapd-lite-installer-a4x-max-bm-cos.yaml
kubectl get daemonset -n kube-system asapd-lite
```

`READY` must equal the number of healthy nodes in the pool:

```
NAME         DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR   AGE
asapd-lite   18        18        18      18           18          <none>          5m
```

This DaemonSet is **not** part of the AICR bundle.

### Verifying

```shell
kubectl get deviceclasses.resource.k8s.io
kubectl get resourceslices.resource.k8s.io
```

Expect an `mrdma.google.com` DeviceClass (GKE installs the networking
DeviceClasses automatically on supported versions) and `ResourceSlice`
objects publishing RDMA devices for each A4X Max node. An empty slice list
with a healthy node pool almost always means `asapd-lite` is missing or the
`gke-networking-dra-driver` node label was not applied.

## Workload wiring

AICR installs the DRA driver, but the `ComputeDomain` and the RDMA
`ResourceClaimTemplate` are per-workload objects that the workload author
creates. Both are required for NVLS and GPUDirect RDMA to initialize.

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

The RDMA userspace is the exception. DOCA OFED is not host-managed and is
not mounted into the Pod, so the container image must install the
`doca-ofed-userspace` package (arm64-sbsa build) itself; without it the
claimed `mrdma.google.com` devices are present but unusable. See
[Configure your workload manifest for RDMA and IMEX domain](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom-a4x-max#configure-manifest-rdma-imex)
for the installation step.

## References

- [Create an AI-optimized GKE cluster that uses A4X Max](https://docs.cloud.google.com/ai-hypercomputer/docs/create/gke-ai-hypercompute-custom-a4x-max)
- [Allocate network resources by using GKE managed DRANET](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/allocate-network-resources-dra)
- [Configure automated networking for accelerator VMs](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/config-auto-net-for-accelerators)
- [NVIDIA DRA Driver for GPUs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-cds.html)

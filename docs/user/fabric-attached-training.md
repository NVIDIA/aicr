# Attaching a Training Workload to the Cluster Fabric

AICR recipes deliver the cluster-side prerequisites for high-speed inter-node
networking — the NCCL plugin, device injection, node labels and taints. What
they do not do is attach your *workload* to that fabric. This page describes
what a training workload must declare, per platform.

Without this wiring a multi-node job still runs: NCCL falls back to TCP over
the primary interface. Intra-node NVLink is unaffected, so nothing errors and
the job completes — just far slower than the hardware allows. Check for it
rather than assuming it.

## Which layer owns what

Kubeflow Trainer splits a workload across two objects, and the split determines
where fabric wiring can live.

A **TrainJob** can supply:

- pod annotations and labels, via `PodTemplatePatch.Metadata`
- volumes, via `PodSpecPatch.Volumes`
- per-container `env`, `volumeMounts` and `securityContext`, via `ContainerPatch`
- `image`, `command`, `args` and `resourcesPerNode` for the trainer container

A **TrainJob cannot add a container.** `ContainerPatch` carries only `name`,
`env`, `volumeMounts` and `securityContext` — no `image`, no `command` — and
the runtime rejects a patch naming a container it does not already define.

That single limitation decides everything below. Fabrics needing only resource
requests and environment are fully TrainJob-expressible. Fabrics needing a
sidecar require a **Runtime** — either one AICR ships, or one you author.

| Fabric | Platform | Needs a sidecar? | Where the wiring goes |
|---|---|---|---|
| GPUDirect TCPXO | GKE, A3 Mega | yes (`tcpxo-daemon`) | a `TrainingRuntime` you author |
| EFA | EKS | no | your `TrainJob` |
| InfiniBand / RDMA | AKS | no | your `TrainJob` |

## GKE — GPUDirect TCPXO

TCPXO needs a `tcpxo-daemon` sidecar, so the wiring lives in a Runtime.
`TrainingRuntime` is an ordinary namespaced resource: create one in your own
namespace and reference it from your TrainJob.

### Before you start

Confirm the cluster prerequisites are in place — see
[GKE TCPXO Networking Prerequisites](../integrator/gke-tcpxo-networking.md).
You need the eight GPU NIC `Network` objects bound to the GPU node pool:

```shell
kubectl get networks.networking.gke.io -o name | grep gpu-nic
```

**Use the names your cluster actually has.** AICR requires only that each name
contain `gpu-nic`; the rest is chosen by whoever provisioned the cluster, so
prefixed forms such as `aicr-demo2-gpu-nic-0` are common. Substitute them into
the `networking.gke.io/interfaces` annotation below — the examples use
`gpu-nic0`–`gpu-nic7`.

Match the sidecar image to the plugin your cluster runs — see
[NCCL Plugin Version Matching](../integrator/gke-tcpxo-networking.md#nccl-plugin-version-matching).

### The TrainingRuntime

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: torch-tcpxo
  namespace: my-team          # your namespace
  labels:
    trainer.kubeflow.org/framework: torch
spec:
  mlPolicy:
    numNodes: 2               # override per job with spec.trainer.numNodes
    torch: {}
  template:
    spec:
      replicatedJobs:
      - name: node
        template:
          spec:
            template:
              metadata:
                annotations:
                  # Expose GPU and DMA devices to the sidecar via NRI, so it
                  # does not need privileged mode. One entry per GPU, plus the
                  # three control devices.
                  devices.gke.io/container.tcpxo-daemon: |
                    - path: /dev/nvidia0
                    - path: /dev/nvidia1
                    - path: /dev/nvidia2
                    - path: /dev/nvidia3
                    - path: /dev/nvidia4
                    - path: /dev/nvidia5
                    - path: /dev/nvidia6
                    - path: /dev/nvidia7
                    - path: /dev/nvidiactl
                    - path: /dev/nvidia-uvm
                    - path: /dev/dmabuf_import_helper
                  networking.gke.io/default-interface: eth0
                  # Substitute your cluster's Network names.
                  networking.gke.io/interfaces: |
                    [
                      {"interfaceName":"eth0","network":"default"},
                      {"interfaceName":"eth1","network":"gpu-nic0"},
                      {"interfaceName":"eth2","network":"gpu-nic1"},
                      {"interfaceName":"eth3","network":"gpu-nic2"},
                      {"interfaceName":"eth4","network":"gpu-nic3"},
                      {"interfaceName":"eth5","network":"gpu-nic4"},
                      {"interfaceName":"eth6","network":"gpu-nic5"},
                      {"interfaceName":"eth7","network":"gpu-nic6"},
                      {"interfaceName":"eth8","network":"gpu-nic7"}
                    ]
              spec:
                initContainers:
                # Native sidecar: an initContainer with restartPolicy: Always
                # runs alongside the worker for the pod's lifetime.
                - name: tcpxo-daemon
                  image: us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.21
                  restartPolicy: Always
                  command: ["/bin/sh", "-c"]
                  args:
                  - |
                    set -ex
                    chmod 755 /fts/entrypoint_rxdm_container.sh
                    /fts/entrypoint_rxdm_container.sh --num_hops=2
                  securityContext:
                    capabilities:
                      add: ["NET_ADMIN", "NET_BIND_SERVICE"]
                  volumeMounts:
                  - {name: tcpxo-libraries, mountPath: /usr/local/nvidia, readOnly: true}
                  - {name: tcpxo-sys, mountPath: /hostsysfs}
                  - {name: tcpxo-proc-sys, mountPath: /hostprocsysfs}
                  env:
                  - name: LD_LIBRARY_PATH
                    value: /usr/local/nvidia/lib64
                containers:
                - name: node
                  # Your training image. It must be ABI-compatible with the
                  # host-installed plugin libraries mounted below; it does not
                  # need the plugin baked in.
                  image: nvcr.io/nvidia/pytorch:25.06-py3
                  resources:
                    limits:
                      nvidia.com/gpu: 8      # TCPXO requires all 8 GPUs
                    requests:
                      nvidia.com/gpu: 8
                  securityContext:
                    capabilities:
                      add: ["IPC_LOCK"]
                  volumeMounts:
                  - {name: dshm, mountPath: /dev/shm}
                  - {name: tcpxo-libraries, mountPath: /usr/local/nvidia, readOnly: true}
                  - {name: tcpxo-aperture-devices, mountPath: /dev/aperture_devices}
                volumes:
                - name: dshm
                  emptyDir: {medium: Memory}
                - name: tcpxo-libraries
                  hostPath: {path: /home/kubernetes/bin/nvidia}
                - name: tcpxo-sys
                  hostPath: {path: /sys}
                - name: tcpxo-proc-sys
                  hostPath: {path: /proc/sys}
                - name: tcpxo-aperture-devices
                  hostPath: {path: /dev/aperture_devices}
```

### Sourcing the NCCL configuration

The annotations and sidecar attach the NICs; they do not configure NCCL. The
plugin installer stages a profile script on each node, and your entrypoint must
source it before launching training:

```shell
NCCL_LIB_DIR=/usr/local/nvidia/lib64 . /usr/local/nvidia/lib64/nccl-env-profile.sh
```

That sets the complete versioned variable set for your plugin release —
`NCCL_FASTRAK_*` and also `NCCL_SOCKET_IFNAME`, `NCCL_CROSS_NIC`, protocol and
tuner paths, and `LD_LIBRARY_PATH`. Setting only the `NCCL_FASTRAK_*` variables
by hand is not equivalent, and the set changes between plugin releases.

### The TrainJob

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: my-training-job
  namespace: my-team
spec:
  runtimeRef:
    name: torch-tcpxo
    apiGroup: trainer.kubeflow.org
    kind: TrainingRuntime
  trainer:
    numNodes: 2
    image: my-registry/my-trainer:latest
    command: ["sh", "-c"]
    args:
    - |
      NCCL_LIB_DIR=/usr/local/nvidia/lib64 . /usr/local/nvidia/lib64/nccl-env-profile.sh
      torchrun --nnodes=$PET_NNODES --nproc-per-node=8 train.py
```

**Do not set `resourcesPerNode` here.** On the pinned Kubeflow Trainer version
a `resourcesPerNode` carrying limits or requests **replaces** the worker's
entire resource requirements — so a job requesting only CPU or memory silently
loses the runtime's `nvidia.com/gpu: 8`, and TCPXO stops working. Let the
runtime own the resource shape. If you must set it, repeat *every* resource,
including the GPU request.

## EKS — EFA

EFA needs no sidecar, so a TrainJob can attach to it directly against a generic
runtime such as the `torch-distributed` runtime AICR ships.

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: my-training-job
spec:
  runtimeRef:
    name: torch-distributed
    apiGroup: trainer.kubeflow.org
    kind: ClusterTrainingRuntime
  trainer:
    numNodes: 2
    image: my-registry/my-trainer:latest
    resourcesPerNode:
      limits:
        nvidia.com/gpu: 8
        vpc.amazonaws.com/efa: 32     # see below — varies by instance type
      requests:
        nvidia.com/gpu: 8
        vpc.amazonaws.com/efa: 32
    env:
    - name: FI_PROVIDER
      value: efa
    - name: FI_EFA_USE_DEVICE_RDMA
      value: "1"
    - name: LD_LIBRARY_PATH
      value: /opt/amazon/ofi-nccl/lib:/opt/amazon/efa/lib:/usr/local/nvidia/lib64
```

**Repeat the GPU request.** `resourcesPerNode` replaces the whole resource
block, so listing EFA without `nvidia.com/gpu` yields a pod with no GPUs.

**Determine the EFA count from the node, not from a table.** It varies by
instance type — 4 on p4d, 32 on p5, and on g7e by size, with some sizes having
none:

```shell
kubectl get nodes -l nvidia.com/gpu.present=true \
  -o jsonpath='{.items[0].status.allocatable.vpc\.amazonaws\.com/efa}'
```

## AKS — InfiniBand / RDMA

Also TrainJob-expressible. AICR fixes the resource name, and the value is
always `1`:

```yaml
  trainer:
    numNodes: 2
    image: my-registry/my-trainer:latest
    resourcesPerNode:
      limits:
        nvidia.com/gpu: 8
        rdma/hca_shared_devices_a: 1
      requests:
        nvidia.com/gpu: 8
        rdma/hca_shared_devices_a: 1
    env:
    - name: NCCL_IB_DISABLE
      value: "0"
    - name: NCCL_NET_PLUGIN
      value: none
```

The same GPU-request caveat applies.

## Verifying the fabric is in use

Run a short job with NCCL debug enabled and check which transport it selected:

```shell
kubectl logs <worker-pod> | grep -i 'NCCL INFO Using network'
```

Expect the plugin name — `FasTrak` for TCPXO, `AWS Libfabric` for EFA, `IB` for
InfiniBand. **`Socket` means the fabric is not in use** and the job is running
over TCP.

Enabling `NCCL_DEBUG=INFO` is verbose. Prefer a short dedicated run for the
check rather than leaving it on for a full training job.

## Related

- [GKE TCPXO Networking Prerequisites](../integrator/gke-tcpxo-networking.md) — cluster-side setup
- [AKS GPU Setup](../integrator/aks-gpu-setup.md) — RDMA prerequisites

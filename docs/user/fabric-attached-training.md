# Attaching a Training Workload to the Cluster Fabric

AICR recipes deliver the cluster side of high-speed inter-node networking — the
NCCL plugin, device injection, node labels and taints. They do **not** attach
your workload to it. That part is yours, and this page describes what it
involves.

Without it a multi-node job still runs: NCCL falls back to TCP over the primary
interface. Intra-node NVLink is unaffected, so nothing errors and the job
completes — just far slower than the hardware allows. Check for it rather than
assuming it.

## Which layer owns what

Kubeflow Trainer splits a workload across two objects, and the split decides
where fabric wiring can live.

A **TrainJob** can supply pod annotations and labels (`PodTemplatePatch.Metadata`),
volumes (`PodSpecPatch.Volumes`), and per-container `env`, `volumeMounts` and
`securityContext` (`ContainerPatch`).

A **TrainJob cannot add a container.** `ContainerPatch` carries only `name`,
`env`, `volumeMounts` and `securityContext` — no `image`, no `command` — and the
runtime rejects a patch naming a container it does not already define.

That single limitation decides the rest:

| Fabric | Platform | Needs a sidecar? | Where the wiring goes |
|---|---|---|---|
| GPUDirect TCPXO | GKE, A3 Mega | yes (`tcpxo-daemon`) | a `TrainingRuntime` you author |
| EFA | EKS | no | your `TrainJob` |
| InfiniBand / RDMA | AKS | no | your `TrainJob` |

## GKE — GPUDirect TCPXO

TCPXO needs the `tcpxo-daemon` sidecar, so the wiring cannot live in a TrainJob.
`TrainingRuntime` is an ordinary namespaced resource: author one in your
namespace and reference it from `runtimeRef`.

**What that runtime must carry**, all of it documented in
[Workload Pod Configuration](../integrator/gke-tcpxo-networking.md#workload-pod-configuration-nri-profile):

- the `networking.gke.io/interfaces` and `devices.gke.io/container.tcpxo-daemon`
  annotations, on the pod template metadata
- the `tcpxo-daemon` native sidecar, at the version paired with the plugin your
  cluster runs
- four hostPath volumes, and `IPC_LOCK` on the worker
- the NCCL configuration for your plugin release

**The shape, abridged** — elisions marked `...`; this is to show where things
go, not to copy verbatim:

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime          # namespaced — yours to create
metadata:
  name: torch-tcpxo
  labels:
    trainer.kubeflow.org/framework: torch
spec:
  mlPolicy: {numNodes: 2, torch: {}}
  template:
    spec:
      replicatedJobs:
      - name: node
        template:
          metadata:
            labels:
              trainer.kubeflow.org/trainjob-ancestor-step: trainer   # required
          spec:
            template:
              metadata:
                annotations:
                  devices.gke.io/container.tcpxo-daemon: ...         # NRI device list
                  networking.gke.io/default-interface: eth0
                  networking.gke.io/interfaces: ...                  # YOUR 8 network names
              spec:
                nodeSelector: ...        # carry over from your bundle
                tolerations: ...
                initContainers:
                - name: tcpxo-daemon     # native sidecar: restartPolicy: Always
                  image: .../tcpgpudmarxd-dev:<paired-with-your-plugin>
                  ...
                containers:
                - name: node
                  image: <your training image>
                  resources:
                    limits: {nvidia.com/gpu: 8}     # TCPXO needs all 8
                  ...
                volumes: ...             # 4 hostPaths + dshm
```

Then reference it from the TrainJob, which carries no fabric configuration at all:

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
spec:
  runtimeRef:
    name: torch-tcpxo
    kind: TrainingRuntime
    apiGroup: trainer.kubeflow.org
  trainer:
    numNodes: 2
    image: my-registry/my-trainer:latest
```

**For a complete, working reference**, see the runtime AICR's own performance
validator applies:
[`validators/performance/testdata/h100/gke/runtime.yaml`](https://github.com/NVIDIA/aicr/blob/main/validators/performance/testdata/h100/gke/runtime.yaml).
It is an MPI benchmark rather than a training job, so the launcher differs — but
the fabric wiring is exactly what a workload needs, and it is kept current
against the recipe.

Two details are easy to miss because they are not in the pod spec:

- Your runtime's `node` replicated job needs the label
  `trainer.kubeflow.org/trainjob-ancestor-step: trainer`. Without it Trainer
  applies none of the TrainJob's `trainer` block — image, command, `numNodes`,
  or the `PET_*` rendezvous variables.
- AICR's bundler injects `nodeSelector` and `tolerations` into the runtime it
  ships, from `--accelerated-node-selector` and `--accelerated-node-toleration`.
  **A runtime you author inherits nothing**, so carry your bundle's resolved
  values across or the job may stay Pending — or land on another 8-GPU pool
  without TCPXO.

### The network names are yours to supply

This is the part no example can fill in for you. The
`networking.gke.io/interfaces` annotation must name the eight GPU NIC `Network`
objects **as they exist on your cluster**. AICR requires only that each name
contain `gpu-nic`; the rest is chosen by whoever provisioned it, so prefixed
forms such as `aicr-demo2-gpu-nic-0` are common.

```shell
kubectl get networks.networking.gke.io \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep gpu-nic
```

That prints bare names, which is what the annotation takes. `-o name` would
prefix them with `network.networking.gke.io/` — not the form to paste.

`Network` is cluster-scoped. If your role is namespace-only you will not be able
to list them; ask whoever provisions the cluster for the eight names, or for the
`GKENetworkParamSet` mapping.

### Do not set `resourcesPerNode`

Let the runtime own the resource shape. `resourcesPerNode` on a TrainJob is not
merged into the runtime's — a value carrying `limits` or `requests` **replaces**
the worker's resource requirements outright, so a job that sets it to ask for
memory silently loses the runtime's `nvidia.com/gpu: 8` and TCPXO stops working.

Setting it at all — even to `{}` — also feeds Torch's process-count inference,
which prefers the TrainJob's value and can yield `PET_NPROC_PER_NODE=1`.

If you must set it, repeat *every* resource in it, including the GPU request.

## EKS — EFA

EFA needs no sidecar, so a TrainJob can attach to it against a generic runtime
such as the `torch-distributed` runtime AICR ships. It needs three things:

**The device request**, alongside every other resource — `resourcesPerNode`
replaces rather than merges, so omitting `nvidia.com/gpu` here yields a pod with
no GPUs:

```yaml
    resourcesPerNode:
      limits:
        nvidia.com/gpu: 8
        vpc.amazonaws.com/efa: 32     # per node — see below
      requests:
        nvidia.com/gpu: 8
        vpc.amazonaws.com/efa: 32
```

**An image carrying the EFA stack.** AICR installs the device plugin, which
exposes the devices — it does not put `libfabric` or `aws-ofi-nccl` into your
training image. An ordinary PyTorch image will fall back to sockets no matter
what `LD_LIBRARY_PATH` says. Build from a base that includes them, and point
`LD_LIBRARY_PATH` at wherever *that image* installs the plugin; AICR's tested
image uses `/opt/amazon/ofi-nccl/lib/x86_64-linux-gnu`.

**The count, read from the nodes you will actually run on.** It varies by
instance type — 4 on p4d, 32 on p5, and on g7e by size, with some sizes having
none. Check every eligible node and fail if they disagree, rather than trusting
the first one:

```shell
kubectl get nodes -l <your-gpu-pool-selector> \
  -o custom-columns='NODE:.metadata.name,EFA:.status.allocatable.vpc\.amazonaws\.com/efa'
```

## AKS — InfiniBand / RDMA

Also TrainJob-expressible. AICR fixes the resource name and the value is always
`1`:

```yaml
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

The same repeat-every-resource caveat applies. See
[AKS GPU Setup](../integrator/aks-gpu-setup.md) for the cluster-side
prerequisites.

## Verifying the fabric is in use

Run a short job with `NCCL_DEBUG=INFO` and check which transport NCCL selected:

```shell
kubectl logs <worker-pod> | grep -i 'NCCL INFO.*Using network'
```

Expect the plugin name — `FasTrak` for TCPXO, `AWS Libfabric` for EFA, `IB` for
InfiniBand. **`Socket` means the fabric is not in use** and the job is running
over TCP.

`NCCL_DEBUG=INFO` is verbose. Prefer a short dedicated run for the check rather
than leaving it on for a full training job.

## Related

- [GKE TCPXO Networking Prerequisites](../integrator/gke-tcpxo-networking.md) — cluster-side setup and the full pod-level wiring
- [AKS GPU Setup](../integrator/aks-gpu-setup.md) — RDMA prerequisites

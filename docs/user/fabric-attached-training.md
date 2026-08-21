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

A **TrainJob** can supply pod annotations and labels (`PodTemplatePatch.Metadata`)
and volumes (`PodSpecPatch.Volumes`) through `spec.runtimePatches`, plus
`image`, `command`, `args`, `env` and `resourcesPerNode` for the trainer
through `spec.trainer`.

Note where worker **environment** goes: `ContainerPatch.env` is rejected for the
`node`, `dataset-initializer` and `model-initializer` containers, so worker
variables belong in `spec.trainer.env`, not in a runtime patch. `ContainerPatch`
can still set `volumeMounts` and `securityContext` on `node`.

A **TrainJob cannot add a container.** `ContainerPatch` carries only `name`,
`env`, `volumeMounts` and `securityContext` — no `image`, no `command` — and the
runtime rejects a patch naming a container it does not already define.

That single limitation decides the rest:

| Fabric | Platform | Needs a sidecar? | Where the wiring goes |
|---|---|---|---|
| GPUDirect TCPXO | GKE, A3 Mega | yes (`tcpxo-daemon`) | a `TrainingRuntime` you author |
| EFA | EKS | no | your `TrainJob` |
| InfiniBand / RDMA | AKS | no | your `TrainJob` |

## GKE GPUDirect TCPXO

TCPXO needs the `tcpxo-daemon` sidecar, so the wiring cannot live in a TrainJob.
`TrainingRuntime` is an ordinary namespaced resource: author one in your
namespace and reference it from `runtimeRef`.

**What that runtime must carry.** The annotations and sidecar are specified in
[Workload Pod Configuration](../integrator/gke-tcpxo-networking.md#workload-pod-configuration-nri-profile);
the `dshm` volume, worker `IPC_LOCK`, daemon `args` and NCCL settings are not in
that section — take them from AICR's validator runtime, cited below.

- the `networking.gke.io/interfaces` and `devices.gke.io/container.tcpxo-daemon`
  annotations, on the pod template metadata
- the `tcpxo-daemon` native sidecar, at the version paired with the plugin your
  cluster runs
- four hostPath volumes plus a memory-backed `dshm` at `/dev/shm`, and `IPC_LOCK`
  on the worker
- the NCCL configuration for your plugin release. The ~40 `NCCL_FASTRAK_*`
  tuning variables ship as `/usr/local/nvidia/lib64/nccl-env-profile.sh`, laid
  down by the plugin installer and version-matched to it. Source that file in
  your container's startup rather than transcribing the variables — a job that
  skips it still attaches to the fabric, but runs well under its bandwidth

**A placement sketch** — `"<...>"` marks a value you must fill in. It shows
where each piece goes; it is not a manifest, abridged or otherwise, and the
authoritative field list is the integrator page linked below:

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
                  devices.gke.io/container.tcpxo-daemon: "<NRI device list — see reference>"
                  networking.gke.io/default-interface: eth0
                  networking.gke.io/interfaces: "<JSON array, 9 entries — see reference>"
              spec:
                nodeSelector: "<carry over from your bundle>"
                tolerations: "<carry over from your bundle>"
                initContainers:
                - name: tcpxo-daemon
                  restartPolicy: Always    # native sidecar — without this the
                                           # pod stalls in initialization
                  image: "<registry>/tcpgpudmarxd-dev:<paired-with-your-plugin>"
                  args: "<see reference>"                 # + capabilities, volumeMounts
                containers:
                - name: node
                  image: <your training image>
                  resources:
                    limits: {nvidia.com/gpu: 8}     # TCPXO needs all 8
                  env: "<incl. NCCL_FASTRAK_LLCM_DEVICE_DIRECTORY>"
                  securityContext: "<IPC_LOCK>"          # + volumeMounts
                volumes: "<4 hostPaths + dshm>"
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

**The authoritative wiring** is
[Workload Pod Configuration](../integrator/gke-tcpxo-networking.md#workload-pod-configuration-nri-profile)
together with the version table in
[NCCL Plugin Version Matching](../integrator/gke-tcpxo-networking.md#nccl-plugin-version-matching).
Take the annotation, sidecar, volume and capability requirements from there.

AICR's performance validator applies a runtime with the same wiring
([`validators/performance/testdata/h100/gke/runtime.yaml`](https://github.com/NVIDIA/aicr/blob/main/validators/performance/testdata/h100/gke/runtime.yaml)),
which is useful to read for shape. It is **not** a template to copy: it is an
MPI benchmark rather than a training job, it carries validator-only
placeholders substituted at apply time, and its own image pins are not
guaranteed to match the pair the recipe currently ships.

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

Let the runtime own the resource shape. On the pinned Kubeflow Trainer
**v2.2.0**, `resourcesPerNode` on a TrainJob is not merged into the runtime's —
a value carrying `limits` or `requests` **replaces**
the worker's resource requirements outright, so a job that sets it to ask for
memory silently loses the runtime's `nvidia.com/gpu: 8` and TCPXO stops working.

Setting it at all — even to `{}` — also feeds Torch's process-count inference,
which prefers the TrainJob's value and can yield `PET_NPROC_PER_NODE=1`.

If you must set it, repeat *every* resource in it, including the GPU request.

## EKS EFA

EFA needs no sidecar, so a TrainJob can attach to it against a generic runtime
such as the `torch-distributed` runtime AICR ships. It needs three things:

**The device request**, alongside every other resource. `torch-distributed`
carries **no `resources` block at all**, so `resourcesPerNode` is the only
source of GPUs here — omitting `nvidia.com/gpu` yields a pod with none:

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
spec:
  runtimeRef:
    name: torch-distributed
    kind: ClusterTrainingRuntime     # cluster-scoped, unlike the GKE example
    apiGroup: trainer.kubeflow.org
  trainer:
    numNodes: 2                      # torch-distributed defaults to 1
    resourcesPerNode:
      limits:
        nvidia.com/gpu: 8
        vpc.amazonaws.com/efa: "<EFA_COUNT>"    # per node — read it, see below
      requests:
        nvidia.com/gpu: 8
        vpc.amazonaws.com/efa: "<EFA_COUNT>"
```

**`IPC_LOCK` and `FI_EFA_FORK_SAFE=1`.** `torch-distributed` grants neither —
AICR's tested EFA runtime sets both. Add `FI_EFA_FORK_SAFE=1` alongside the
other `FI_*` variables in `spec.trainer.env`, and `IPC_LOCK` via a
`runtimePatches` entry setting `securityContext` on the `node` container
(`securityContext` is patchable there even though `env` is not). Without
`IPC_LOCK`, NCCL may fail to register pinned buffers.

**`NCCL_SOCKET_IFNAME=eth0`.** AICR's tested EFA and InfiniBand runtimes both pin
this so NCCL's bootstrap uses the control interface. On a multi-NIC node — p5
carries several EFA ENIs — NCCL may otherwise pick a secondary, non-routable NIC
for rendezvous and hang during initialization, before any transport is chosen.

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

## AKS InfiniBand and RDMA

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
    - name: NCCL_SOCKET_IFNAME
      value: eth0
```

`NCCL_SOCKET_IFNAME` pins NCCL's bootstrap and out-of-band traffic to the
primary interface. AICR's tested AKS runtime sets it, as does the EKS one — on
a multi-NIC node, leaving NCCL to guess can send rendezvous traffic down an
interface that cannot carry it. `eth0` assumes the pod's primary interface is
named that; under `hostNetwork` or a non-standard CNI it may not be. Confirm
on a running pod with
`kubectl exec -n <namespace> <pod> -- ip route get 1.1.1.1` and pin whatever
that reports.

The same repeat-every-resource caveat applies.

**`IPC_LOCK` is required here too, and allocating the RDMA device does not grant
it.** NCCL's IB transport registers pinned (memlocked) buffers via ibverbs, and
`IPC_LOCK` is what lifts `RLIMIT_MEMLOCK` — without it the job can fail *after*
the device is allocated, which reads as an NCCL bug rather than a missing
capability. Add it the same way as for EFA: a `runtimePatches` entry setting
`securityContext` on the `node` container.

**An image carrying the IB verbs stack.** As with EFA, allocating the device is
not enough: NCCL's IB transport dlopens `libibverbs` and the rest of rdma-core at
runtime. An image without them logs `NET/IB : No device found` and falls back to
sockets — the job still completes, just over TCP. Build from a base that carries
rdma-core, or install it in the image.

**A memory-backed `/dev/shm` is also expected.** AICR's tested runtime mounts a
`dshm` volume (`emptyDir: {medium: Memory}`) there; the default 64 MiB `/dev/shm`
is small for multi-process NCCL.

See [AKS GPU Setup](../integrator/aks-gpu-setup.md) for the cluster-side
prerequisites.

## Verifying the fabric is in use

Run a short job with `NCCL_DEBUG=INFO` and check which transport NCCL selected.
Every runtime AICR ships sets `NCCL_DEBUG=WARN`, at which this line is
suppressed — so grepping an ordinary run finds nothing, which is not evidence of
socket fallback:

```shell
# Select workers by label rather than guessing the generated pod name.
# --tail=-1 is required: with a selector, kubectl defaults to the last 10 lines
# and would omit the transport banner, which NCCL prints during init.
kubectl logs -n <namespace> -c node --tail=-1 --prefix \
  -l jobset.sigs.k8s.io/jobset-name=<trainjob-name>,jobset.sigs.k8s.io/replicatedjob-name=node \
  | grep -i 'NCCL INFO.*Using network'
```

This selects the `node` replicated job, which is where a node-only runtime such
as `torch-distributed` prints the banner. If your runtime uses an MPI launcher —
as AICR's tested AKS runtime does — the rank processes still execute on the
`node` pods, but they are started over sshd and `mpirun` aggregates their
stdout in the `launcher` pod, so select
`jobset.sigs.k8s.io/replicatedjob-name=launcher` instead. A grep against `node`
on such a runtime finds nothing, which is not evidence of socket fallback.

Expect the plugin name — `FasTrak` for TCPXO, `AWS Libfabric` for EFA, `IB` for
InfiniBand. **`Socket` means the fabric is not in use** and the job is running
over TCP.

`NCCL_DEBUG=INFO` is verbose. Prefer a short dedicated run for the check rather
than leaving it on for a full training job.

## Related

- [GKE TCPXO Networking Prerequisites](../integrator/gke-tcpxo-networking.md) — cluster-side setup and the full pod-level wiring
- [AKS GPU Setup](../integrator/aks-gpu-setup.md) — RDMA prerequisites

# AICR - Critical User Journey (CUJ) 1 — GKE

## Assumptions

* Assuming user is already authenticated to a GKE cluster with 2+ H100 (a3-megagpu-8g) nodes.
* GKE cluster runs Container-Optimized OS (COS) with GPU drivers pre-installed.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration` flags are only for example purposes. Assuming user will update these to match their cluster.
* System nodes have no custom taints (GKE managed pods don't tolerate them).

## Snapshot

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=gpu-workload:NoSchedule \
    --toleration nvidia.com/gpu=present:NoSchedule \
    --output snapshot.yaml
```

## Gen Recipe

```shell
aicr recipe \
  --service gke \
  --accelerator h100 \
  --intent training \
  --os cos \
  --platform kubeflow \
  --output recipe.yaml
```

## Validate Recipe Constraints

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --no-cluster \
    --phase deployment \
    --output dry-run.json
```

## Generate Bundle

```shell
aicr bundle \
  --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=gpu-workload:NoSchedule \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --system-node-selector nodeGroup=system-worker \
  --output bundle
```

> Note: GKE system nodes should not have custom taints (breaks konnectivity-agent and other GKE managed pods). Only `--system-node-selector` is needed, no `--system-node-toleration`.

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

> Note: If skyhook-operator is already installed on the cluster, comment out or skip the skyhook-operator and skyhook-customizations sections in deploy.sh to avoid upgrade conflicts.

## Validate Cluster

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=gpu-workload:NoSchedule \
    --toleration nvidia.com/gpu=present:NoSchedule \
    --phase conformance \
    --output report.json
```

## Run Job

Run a simple distributed PyTorch training job using the [Kubeflow TrainJob API](https://blog.kubeflow.org/trainer/intro/):

```shell
# Create the TrainJob
kubectl apply -f - <<EOF
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: pytorch-mnist
  namespace: kubeflow
spec:
  trainer:
    numNodes: 1
    image: kubeflow/pytorch-dist-mnist:v1-9e12c68
    command:
      - "python3"
      - "/opt/mnist/src/mnist.py"
      - "--epochs=1"
    resourcesPerNode:
      requests:
        nvidia.com/gpu: 1
      limits:
        nvidia.com/gpu: 1
  podTemplateOverrides:
    - targetJobs:
        - name: node
      spec:
        tolerations:
          - operator: Exists
  runtimeRef:
    name: torch-distributed
    apiGroup: trainer.kubeflow.org
    kind: ClusterTrainingRuntime
EOF

# Monitor the TrainJob
kubectl get trainjobs -n kubeflow
kubectl get pods -n kubeflow -l trainer.kubeflow.org/job-name=pytorch-mnist
kubectl logs -f -n kubeflow -l trainer.kubeflow.org/job-name=pytorch-mnist
```

## Performance Validation

> **Note:** `aicr validate --phase performance` is not yet automated for GKE.
> The GKE NCCL test uses raw Pods with a TCPXO daemon sidecar (required for GPUDirect),
> which differs from the EKS TrainJob-based approach. Run the test manually as shown below.
> Automated support is tracked as a follow-up.

### Option 1: Using testdata manifests (matches validator framework)

```shell
export NAMESPACE=nccl-perf
export GPU_COUNT_PER_NODE=8
export GPU_COUNT=16
export WORKER_COUNT=2
export TEST_TYPE=all_reduce_perf
export MIN_MESSAGE_SIZE=1M
export MAX_MESSAGE_SIZE=8G

kubectl create ns $NAMESPACE
envsubst < validators/performance/testdata/h100/gke/runtime.yaml | kubectl apply -f -

# Wait for pods to be 2/2 Running
kubectl get pods -n $NAMESPACE -o wide -w

# Trigger the AllReduce benchmark from host-1
kubectl exec nccl-test-host-1 -n $NAMESPACE -c nccl-test -- \
  /scripts/allreduce.sh nccl-host-1 nccl-host-2

# Expected: ~335 GB/s busBW at 8 GB (AllReduce), ~87 GB/s avg
# Clean up
kubectl delete ns $NAMESPACE
```

### Option 2: Using standalone demo manifest

```shell
kubectl create ns nccl-test
kubectl apply -f demos/workloads/training/gke-nccl-test-tcpxo.yaml -n nccl-test

# Wait for pods to be 2/2 Running
kubectl get pods -n nccl-test -o wide -w

# Trigger the AllReduce benchmark from host-1
kubectl exec nccl-test-host-1 -n nccl-test -c nccl-test -- bash -c '
  /scripts/init_ssh.sh nccl-host-1 nccl-host-2 &&
  pushd /scripts && /scripts/gen_hostfiles.sh nccl-host-1 nccl-host-2 && popd &&
  BENCHMARK=all_reduce_perf NHOSTS=2 NCCL_LIB_DIR="/usr/local/nvidia/lib64" \
    LD_LIBRARY_PATH="/usr/local/nvidia/lib64" /scripts/demo-run-nccl-test-tcpxo-via-mpi.sh'

# Expected: ~335 GB/s busBW at 8 GB (AllReduce), ~87 GB/s avg
# Clean up
kubectl delete ns nccl-test
```

### Prerequisites

- GKE cluster with multi-NIC networking (8 GPU NICs per a3-megagpu-8g node)
- `Network` + `GKENetworkParamSet` CRs configured for GPU NICs (infrastructure, cluster-specific)
- `nccl-tcpxo-installer` DaemonSet deployed on GPU nodes (included in AICR bundle)
- `nri-device-injector` DaemonSet deployed on GPU nodes (included in AICR bundle)
- Without multi-NIC, NCCL falls back to TCP (~4 GB/s vs ~335 GB/s with TCPXO)

### TCPXO Runtime Requirements

Each workload pod that needs GPUDirect TCPXO must include a `tcpxo-daemon` sidecar container.

**Recommended profile** (validated on GKE 1.35 / a3-megagpu-8g):
- `hostNetwork: true` — required for PCI sysfs visibility
- `privileged: false` — not needed with NRI device injection
- NRI annotations on the pod: `devices.gke.io/container.tcpxo-daemon` (GPU devices) and `networking.gke.io/interfaces` (multi-NIC mapping with cluster-specific network names)
- `securityContext.capabilities: [NET_ADMIN, NET_BIND_SERVICE]` on the tcpxo-daemon container
- Requires NRI device injector DaemonSet deployed on GPU nodes

**Fallback profile** (if NRI injector is not available):
- `hostNetwork: true` + `privileged: true`
- No annotations needed

> **Known issue:** Without `hostNetwork: true`, the TCPXO daemon cannot enumerate all GPUs via PCI sysfs — the container runtime restricts sysfs visibility, causing the daemon to detect fewer GPUs in the PCI tree than CUDA reports, and exit. NRI annotations provide `/dev/nvidia*` device access but do not restore full PCI sysfs visibility. This is a GKE container runtime limitation.

### Understanding the results

Each pod runs two containers: a `tcpxo-daemon` sidecar (manages GPUDirect TCPX data path) and the `nccl-test` container. The TCPXO sidecar is required for any workload that needs high-speed inter-node GPU communication on GKE.

| Metric | Without TCPXO | With TCPXO |
|--------|--------------|------------|
| AllReduce busBW (8 GB) | ~4 GB/s | ~335 GB/s |
| AllReduce avg busBW | ~4 GB/s | ~87 GB/s |

## Success

Job success + Fabric bandwidth within range

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

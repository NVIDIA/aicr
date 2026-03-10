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

## Run NCCL Performance Test (GPUDirect TCPXO)

For multi-node NCCL bandwidth testing with GPUDirect TCPXO (requires multi-NIC networking):

```shell
kubectl create ns nccl-test
kubectl apply -f demos/workloads/training/gke-nccl-allreduce-tcpxo.yaml -n nccl-test

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

## Success

Job success + Fabric bandwidth within range

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

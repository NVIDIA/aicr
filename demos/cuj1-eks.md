# AICR - Critical User Journey (CUJ) 1

## Assumptions

* Assuming user is already authenticated to an EKS cluster with 2+ H100 node.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration`, in `--system-node-toleration` flags are only for example purposes. Assuming user will update these to match their cluster. 

## Snapshot

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --output snapshot.yaml
```

## Gen Recipe

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
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
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --system-node-selector dedicated=system-workload \
  --system-node-toleration dedicated=system-workload:NoSchedule \
  --system-node-toleration dedicated=system-workload:NoExecute \
  --output bundle
```

> Both options allow for comma-separated values to supply multiple values. See the [bundle](../docs/user/cli-reference.md#aicr-bundle) section for more information.

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

## Validate Cluster 

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --phase all \
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

## Success

Job success + Fabric bandwidth within range

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

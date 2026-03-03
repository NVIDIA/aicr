# AICR - Critical User Journey (CUJ) 1

> Assuming user is already authenticated to an EKS cluster with 2+ H100 node

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

> Setting additional `--namespace` or `--node-selector` flag to land the agent on on the right node is OK

```shell
aicr validate \
  --phase readiness \
  --recipe recipe.yaml
```

## Generate Bundle

> Setting additional `--accelerated-node-selector`, `--accelerated-node-toleration`, or `--system-node-toleration` flags to land the agent on on the right node is OK

```shell
aicr bundle \
  --recipe recipe.yaml \
  --output bundle \
  --accelerated-node-selector [key]=[value] \
  --accelerated-node-toleration [key]=[value]:[operation] 
```

Replace the values for `--accelerated-node-selector` and `--accelerated-node-toleration` with the appropriate ones to match your gpu pool(s). You do not want optimizations and training workloads to run across all nodes. Both options allow for comma delimination to supply multiple values. See the [aicr bundle](../docs/user/cli-reference.md#aicr-bundle) section for more information.

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

## Validate Cluster 

```shell
aicr validate \
  --recipe recipe.yaml \
  --output report.yaml \
  --phase readiness \
  --phase deployment \
  --phase conformance
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

# Eidos - CUJ1

> Assuming user is already authenticated to an EKS cluster with 2+ H100 node

## Gen Recipe

```shell
eidos recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
  --platform kubeflow \
  --output recipe.yaml
```

## Validate Recipe Constraints

```shell
eidos validate \
  --phase readiness \
  --namespace gpu-operator \
  --node-selector nodeGroup=customer-gpu
  --output recipe.yaml
```

> Assuming cluster meets recipe constraints

## Generate Bundle

> Assuming user updates selectors and tolerations as needed

```shell
eidos bundle \
  --recipe recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule
```

## Install Bundle into the Cluster

```shell
cd ./bundle
helm dependency update
helm install eidos-stack . -f values.yaml
```

## Validate Cluster 

```shell
eidos validate \
  --phase readiness \ 
  --phase deployment \
  --phase conformance \
  --output recipe.yaml
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
    numNodes: 2
    image: docker.io/kubeflowkatib/pytorch-mnist:v1beta1-45c5727
    command:
      - "python3"
      - "/opt/pytorch-mnist/mnist.py"
      - "--epochs=1"
    resourcesPerNode:
      requests:
        nvidia.com/gpu: 1
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

> Synthetic workload, perf checks in CUJ2

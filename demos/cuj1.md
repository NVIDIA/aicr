# Eidos - Critical User Journey (CUJ) 1

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
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --output bundle
```

## Install Bundle into the Cluster

```shell
chmod +x deploy.sh
./deploy.sh
```

## Validate Cluster 

```shell
eidos validate \
  --recipe recipe.yaml \
  --phase readiness \
  --phase deployment \
  --phase conformance \
  --output report.yaml
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

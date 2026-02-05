# Eidos - CUJ1

> Assuming user is already authenticated to an EKS cluster with 2+ H100 node

## Gen Recipe

```shell
eidos recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
  --platform pytorch \
  --output recipe.yaml
```

## Validate Recipe Constraints

```shell
eidos validate \
  --phase readiness \
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

> TODO: Migrate to [TrainJob-v2](https://www.kubeflow.org/docs/components/trainer/operator-guides/migration/#new-trainjob-v2)

```shell
kubectl apply -f https://raw.githubusercontent.com/kubeflow/training-operator/master/examples/pytorch/simple.yaml
kubectl get pytorchjobs
kubectl get pods -l training.kubeflow.org/job-name=pytorch-simple
kubectl logs -f pytorch-simple-master-0
```

## Success

Job Success + Fabric Bandwidth Within Range

> Synthetic workload, perf checks in CUJ2

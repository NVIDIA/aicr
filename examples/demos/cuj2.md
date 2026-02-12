# Eidos - Critical User Journey (CUJ) 2

> Assuming user is already authenticated to Kubernetes cluster

## Gen Recipe

```shell
eidos recipe \
  --service eks \
  --accelerator gb200 \
  --os ubuntu \
  --intent inference \
  --platform dynamo \
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
  --phase readiness \ 
  --phase deployment \
  --phase conformance \
  --output recipe.yaml
```

## Run Job

TODO: Add simple Dynamo workload

## Success

1) Job success + Fabric bandwidth within range
2) Validation report correctly reflects the level of CNCF Conformance

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

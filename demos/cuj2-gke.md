# AICR - Critical User Journey (CUJ) 2 — GKE Inference

## Assumptions

* Assuming user is already authenticated to a GKE cluster with 1+ H100 (a3-megagpu-8g) nodes.
* GKE cluster runs Container-Optimized OS (COS) with GPU drivers pre-installed.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration` flags are only for example purposes. Assuming user will update these to match their cluster.
* System nodes have no custom taints (GKE managed pods don't tolerate them).

## Snapshot

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=gpu-workload:NoSchedule \
    --output snapshot.yaml
```

## Gen Recipe

```shell
aicr recipe \
  --service gke \
  --accelerator h100 \
  --intent inference \
  --os cos \
  --platform dynamo \
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
  --system-node-selector nodeGroup=system-worker \
  --storage-class <storage-class> \
  --output bundle
```

> Note: GKE system nodes should not have custom taints (breaks konnectivity-agent and other GKE managed pods). Only `--system-node-selector` is needed, no `--system-node-toleration`.
>
> Set `--storage-class` to the name of a StorageClass that exists on the target cluster (check with `kubectl get storageclass`). The GKE overlay configures `kube-prometheus-stack` with a `volumeClaimTemplate` but no `storageClassName`, so without this flag the PVC falls to the cluster's default StorageClass — and if no default is configured, the deploy hangs on a Pending PVC.

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

## Validate Cluster

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=gpu-workload:NoSchedule \
    --phase conformance \
    --output report.json
```

## Deploy Inference Workload

Deploy an inference serving graph using the Dynamo platform:

```shell
# Deploy the vLLM aggregation workload (includes KAI queue + DynamoGraphDeployment)
# Note: update tolerations in vllm-agg.yaml to match your cluster taints
kubectl apply -f demos/workloads/inference/vllm-agg.yaml

# Monitor the deployment
kubectl get dynamographdeployments -n dynamo-workload
kubectl get pods -n dynamo-workload -o wide -w

# Verify the inference gateway routes to the workload
kubectl get gateway inference-gateway -n agentgateway-system
kubectl get inferencepool -n dynamo-workload
```

## Chat with the Model

Once the workload is running, start a local chat server:

```shell
# Start the chat server (port-forwards to the inference gateway)
bash demos/workloads/inference/chat-server.sh

# Open the chat UI in your browser
open demos/workloads/inference/chat.html
```

## Success

* Bundle deployed with 14 components (inference recipe)
* CNCF conformance requirements pass
  * DRA Support, Gang Scheduling, Secure GPU Access, Accelerator Metrics,
    AI Service Metrics, Inference Gateway, Robust Controller (Dynamo),
    Pod Autoscaling (HPA), Cluster Autoscaling
* Dynamo inference workload serving requests via inference gateway

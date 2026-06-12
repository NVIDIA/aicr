# AICR - Critical User Journey (CUJ) 2 — Inference

End-to-end inference journey with the Dynamo platform: snapshot → recipe →
validate → bundle → deploy → validate → serve an OpenAI-compatible inference
endpoint. Shared steps are shown once; the **EKS** and **GKE** subsections call
out only where commands or flags genuinely differ.

## Assumptions

**EKS**

* User is already authenticated to an EKS cluster with 2+ H100 (p5.48xlarge) nodes.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration`,
  `--system-node-toleration` flags are examples only. Update them to match your cluster.

**GKE**

* User is already authenticated to a GKE cluster with 1+ H100 (a3-megagpu-8g) nodes.
* GKE cluster runs Container-Optimized OS (COS) with GPU drivers pre-installed.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration`
  flags are examples only. Update them to match your cluster.
* System nodes have no custom taints (GKE managed pods don't tolerate them).

## Snapshot

**EKS**

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --output snapshot.yaml
```

**GKE**

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=gpu-workload:NoSchedule \
    --output snapshot.yaml
```

## Gen Recipe

**EKS**

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --intent inference \
  --os ubuntu \
  --platform dynamo \
  --output recipe.yaml
```

**GKE**

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

Same for both clouds:

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --no-cluster \
    --phase deployment \
    --output dry-run.json
```

## Generate Bundle

Replace the values for `--accelerated-node-selector` and
`--accelerated-node-toleration` with the appropriate ones to match your GPU
pool(s) — you do not want optimizations and inference workloads to run across all
nodes.

> Selector / toleration flags accept comma-separated values. See the [bundle](../docs/user/cli-reference.md#aicr-bundle) section for the full flag set.
>
> Set `--storage-class` to the name of a StorageClass that exists on the target cluster (check with `kubectl get storageclass`). The cloud overlay configures `kube-prometheus-stack` with a `volumeClaimTemplate` but no `storageClassName`, so without this flag the PVC falls to the cluster's default StorageClass — and if no default is configured, the deploy hangs on a Pending PVC.

**EKS**

```shell
aicr bundle \
  --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --system-node-selector nodeGroup=system-worker \
  --system-node-toleration dedicated=system-workload:NoSchedule \
  --system-node-toleration dedicated=system-workload:NoExecute \
  --storage-class <storage-class> \
  --output bundle
```

**GKE**

```shell
aicr bundle \
  --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=gpu-workload:NoSchedule \
  --system-node-selector nodeGroup=system-worker \
  --storage-class <storage-class> \
  --output bundle
```

> **GKE only:** system nodes should not have custom taints (breaks konnectivity-agent and other GKE managed pods). Only `--system-node-selector` is needed, no `--system-node-toleration`.

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

## Validate Cluster

**EKS**

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --phase all \
    --output report.json
```

**GKE**

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=gpu-workload:NoSchedule \
    --phase conformance \
    --output report.json
```

## Deploy Inference Workload

Deploy an inference serving graph using the Dynamo platform (includes KAI queue +
DynamoGraphDeployment):

```shell
# GKE: first update tolerations in vllm-agg.yaml to match your cluster taints
kubectl apply -f demos/workloads/inference/vllm-agg.yaml
```

Monitor the deployment until all pods are `Running` and ready:

```shell
kubectl get dynamographdeployments -n dynamo-workload
kubectl get pods -n dynamo-workload -o wide -w
kubectl wait --for=condition=ready pod --all -n dynamo-workload --timeout=300s

# Verify the Dynamo frontend service is available
kubectl get svc vllm-agg-frontend -n dynamo-workload
```

## Test the Endpoint

### Option 1: Chat UI (browser)

Launch the chat server (port-forward + local UI on port 9090):

```shell
./demos/workloads/inference/chat-server.sh
```

Then open <http://127.0.0.1:9090/chat.html> in your browser. Press `Ctrl+C` to stop.

### Option 2: curl (command line)

```shell
kubectl port-forward -n dynamo-workload svc/vllm-agg-frontend 8000:8000 &

curl -s http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-0.6B",
    "messages": [{"role": "user", "content": "What is Kubernetes?"}],
    "max_tokens": 64
  }'
```

Sample response:

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "Qwen/Qwen3-0.6B",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Kubernetes is an open-source container orchestration platform..."
      },
      "finish_reason": "length"
    }
  ]
}
```

## Success

1. DynamoGraphDeployment pods are running and healthy.
2. The OpenAI-compatible chat completions API returns successful responses.
3. The validation report correctly reflects the level of CNCF Conformance —
   DRA Support, Gang Scheduling, Secure GPU Access, Accelerator Metrics, AI
   Service Metrics, Inference Gateway, Robust Controller (Dynamo), Pod
   Autoscaling (HPA), and Cluster Autoscaling. (Bundle component count varies
   by cloud and recipe — see the [Component Catalog](../docs/user/component-catalog.md).)

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

# AICR - Critical User Journey (CUJ) 2 — EKS Inference

## Assumptions

* Assuming user is already authenticated to an EKS cluster with 2+ H100 (p5.48xlarge) nodes.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration`, `--system-node-toleration` flags are only for example purposes. Assuming user will update these to match their cluster.

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
  --intent inference \
  --os ubuntu \
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
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --system-node-selector nodeGroup=system-worker \
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

## Deploy Inference Workload

Deploy an inference serving graph using the Dynamo platform:

```shell
# Deploy the vLLM aggregation workload (includes KAI queue + DynamoGraphDeployment)
kubectl apply -f demos/workloads/inference/vllm-agg.yaml

# Monitor the deployment
kubectl get dynamographdeployments -n dynamo-workload
kubectl get pods -n dynamo-workload -o wide -w

# Verify the inference gateway routes to the workload
kubectl get gateway inference-gateway -n kgateway-system
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

* Bundle deployed with 16 components (inference recipe)
* CNCF conformance: 9/9 requirements pass
  * DRA Support, Gang Scheduling, Secure GPU Access, Accelerator Metrics,
    AI Service Metrics, Inference Gateway, Robust Controller (Dynamo),
    Pod Autoscaling (HPA), Cluster Autoscaling
* Dynamo inference workload serving requests via inference gateway

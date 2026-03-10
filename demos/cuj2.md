# AICR - Critical User Journey (CUJ) 2

> Assuming user is already authenticated to Kubernetes cluster

## Gen Recipe

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent inference \
  --platform dynamo \
  --output recipe.yaml
```

## Validate Recipe Constraints

> Setting additional `--namespace` or `--node-selector` flag to land the agent on on the right node is OK

```shell
aicr validate \
  --phase performance \
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

Replace the values for `--accelerated-node-selector` and `--accelerated-node-toleration` with the appropriate ones to match your gpu pool(s). You do not want optimizations and inference workloads to run across all nodes. Both options allow for comma delimination to supply multiple values. See the [aicr bundle](../docs/user/cli-reference.md#aicr-bundle) section for more information.
```

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

## Validate Cluster 

```shell
aicr validate \
  --recipe recipe.yaml \
  --output report.yaml \
  --phase performance \
  --phase deployment \
  --phase conformance
```

## Run Inference Workload

### Create namespace and HuggingFace secret

> Set HF_TOKEN env var first

```shell
kubectl create ns dynamo-workload

sed "s/<your-hf-token>/$HF_TOKEN/" \
  demos/workloads/inference/hf-token-secret.yaml | kubectl apply -f -
```

### Deploy the DynamoGraphDeployment

```shell
kubectl apply -f demos/workloads/inference/vllm-agg.yaml
```

Monitor deployment, until all pods are `Running` and ready:

```shell
kubectl get dynamographdeployments -n dynamo-workload
kubectl get pods -n dynamo-workload -w
```

### Test the endpoint

#### Option 1: Chat UI (browser)

Launch the chat server (port-forward + local UI on port 9090)

```shell
./demos/workloads/inference/chat-server.sh
```

Then open http://127.0.0.1:9090/chat.html in your browser.

Press `Ctrl+C` to stop.

#### Option 2: curl (command line)

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

1) DynamoGraphDeployment pods are running and healthy
2) OpenAI-compatible chat completions API returns successful responses
3) Validation report correctly reflects the level of CNCF Conformance

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.


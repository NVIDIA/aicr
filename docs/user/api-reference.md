# API Reference

Complete reference for using the AICR API Server.

## Overview

The AICR API Server provides HTTP REST access to recipe generation and bundle creation for GPU-accelerated infrastructure. Use the API for programmatic access to configuration recommendations and deployment artifacts.

```
┌──────────────┐      ┌──────────────┐
│ GET /recipe  │─────▶│   Recipe     │
└──────────────┘      └──────────────┘
        │
        ▼
┌──────────────┐      ┌──────────────┐
│ POST /bundle │─────▶│  bundles.zip │
└──────────────┘      └──────────────┘
```

### API vs CLI

- Use the **API** for remote recipe generation and bundle creation
- Use the **CLI** for local operations, snapshot capture, and ConfigMap integration

| Feature | API | CLI |
|---------|-----|-----|
| Recipe generation | ✅ GET /v1/recipe | ✅ `aicr recipe` |
| Value query | ✅ GET /v1/query | ✅ `aicr query` |
| Bundle creation | ✅ POST /v1/bundle | ✅ `aicr bundle` |
| Snapshot capture | ❌ Use CLI | ✅ `aicr snapshot` |
| ConfigMap I/O | ❌ Use CLI | ✅ `cm://` URIs |
| Agent deployment | ❌ Use CLI | ✅ `aicr snapshot` |

## Base URL

Local development (example):
```
http://localhost:8080
```

Start the local server:
```shell
docker pull ghcr.io/nvidia/aicrd:latest
docker run -p 8080:8080 ghcr.io/nvidia/aicrd:latest
```

## Quick Start

### Get a Recipe

Generate an optimized configuration recipe for your environment:

```shell
# GET: Basic recipe for H100 on EKS (query parameters)
curl "http://localhost:8080/v1/recipe?accelerator=h100&service=eks"

# GET: Training workload on Ubuntu
curl "http://localhost:8080/v1/recipe?accelerator=h100&service=eks&intent=training&os=ubuntu"

# POST: Recipe from criteria file (YAML body)
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d 'kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: my-config
spec:
  service: eks
  accelerator: h100
  intent: training'

# Save recipe to file
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" -o recipe.json
```

### Generate Bundles

Create deployment bundles from a recipe:

```shell
# Pipe recipe directly to bundle endpoint
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# Extract the bundles
unzip bundles.zip -d ./bundles
```

## Endpoints

### GET /

Service information and available routes.

```shell
curl "http://localhost:8080/"
```

**Response:**
```json
{
  "service": "aicrd",
  "version": "v0.7.6",
  "routes": ["/v1/recipe", "/v1/query", "/v1/bundle"]
}
```

---

### GET /v1/recipe

Generate an optimized configuration recipe based on environment parameters.

**Query Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `service` | string | any | K8s service: `eks`, `gke`, `aks`, `oke`, `any` |
| `accelerator` | string | any | GPU type: `h100`, `gb200`, `a100`, `l40`, `any` |
| `gpu` | string | any | Alias for `accelerator` |
| `intent` | string | any | Workload: `training`, `inference`, `any` |
| `os` | string | any | Node OS: `ubuntu`, `rhel`, `cos`, `amazonlinux`, `any` |
| `platform` | string | any | Platform/framework: `kubeflow`, `any` |
| `nodes` | integer | 0 | GPU node count (0 = any) |

**Examples:**

```shell
# Minimal request
curl "http://localhost:8080/v1/recipe"

# Specify accelerator
curl "http://localhost:8080/v1/recipe?accelerator=h100"

# Full specification
curl "http://localhost:8080/v1/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&nodes=8"

# Using gpu alias
curl "http://localhost:8080/v1/recipe?gpu=gb200&service=gke"

# Pretty print with jq
curl -s "http://localhost:8080/v1/recipe?accelerator=h100" | jq '.'
```

---

### POST /v1/recipe

Generate an optimized configuration recipe from a criteria file body. This endpoint provides an alternative to query parameters, accepting a Kubernetes-style `RecipeCriteria` resource in the request body.

**Content Types:**
- `application/json` - JSON format
- `application/x-yaml` - YAML format

**Request Body:**

The request body must be a `RecipeCriteria` resource:

```yaml
kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: my-criteria
spec:
  service: eks
  accelerator: gb200
  os: ubuntu
  intent: training
  platform: kubeflow
  nodes: 8
```

**Examples:**

```shell
# POST with YAML body
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d 'kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: training-config
spec:
  service: eks
  accelerator: h100
  intent: training'

# POST with JSON body
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/json" \
  -d '{
    "kind": "RecipeCriteria",
    "apiVersion": "aicr.nvidia.com/v1alpha1",
    "metadata": {"name": "training-config"},
    "spec": {
      "service": "eks",
      "accelerator": "h100",
      "intent": "training"
    }
  }'

# POST with criteria file
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/yaml" \
  -d @criteria.yaml

# Pretty print response
curl -s -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/json" \
  -d '{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","spec":{"service":"eks","accelerator":"h100"}}' \
  | jq '.'
```

**Response:**

Same as GET /v1/recipe - returns a recipe JSON response.

**Error Responses:**
- `400 Bad Request` - Invalid criteria format, missing required fields, or invalid enum values
- `405 Method Not Allowed` - Only GET and POST are supported

**Response:**

```json
{
  "apiVersion": "aicr.nvidia.com/v1alpha1",
  "kind": "Recipe",
  "metadata": {
    "version": "v1.0.0",
    "created": "2026-01-11T10:30:00Z",
    "appliedOverlays": [
      "base",
      "eks",
      "eks-training",
      "gb200-eks-training"
    ],
    "excludedOverlays": [
      {
        "name": "h100-eks-ubuntu-training",
        "reason": "mixin-constraint-failed"
      }
    ],
    "constraintWarnings": [
      {
        "overlay": "h100-eks-ubuntu-training",
        "constraint": "OS.sysctl./proc/sys/kernel/osrelease",
        "expected": ">= 6.8",
        "actual": "5.15.0",
        "reason": "mixin-constraint-failed: expected >= 6.8, got 5.15.0"
      }
    ]
  },
  "criteria": {
    "service": "eks",
    "accelerator": "gb200",
    "intent": "training",
    "os": "any",
    "platform": "any"
  },
  "componentRefs": [
    {
      "name": "gpu-operator",
      "version": "v25.3.3",
      "order": 1,
      "repository": "https://helm.ngc.nvidia.com/nvidia"
    },
    {
      "name": "network-operator",
      "version": "v25.4.0",
      "order": 2,
      "repository": "https://helm.ngc.nvidia.com/nvidia"
    }
  ],
  "constraints": {
    "driver": {
      "version": "580.82.07",
      "cudaVersion": "13.1"
    }
  }
}
```

`metadata.excludedOverlays` is optional. When present, each entry includes the overlay `name` and a machine-readable `reason` such as `constraint-failed` or `mixin-constraint-failed`.

---

### GET /v1/query

Query a specific value from a fully hydrated recipe. Resolves a recipe from criteria (same parameters as GET /v1/recipe), merges all base, overlay, and inline overrides, then returns the value at the given selector path.

**Query Parameters:**

All GET /v1/recipe parameters are supported, plus:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `selector` | string | Yes | Dot-delimited path to the value to extract (e.g. `components.gpu-operator.values.driver.version`). Empty string returns the entire hydrated recipe. |

**Response:**

- **Scalar values** (string, number, bool) are returned as plain JSON values
- **Complex values** (maps, lists) are returned as JSON objects/arrays

**Examples:**

```shell
# Get a specific Helm value
curl -s "http://localhost:8080/v1/query?service=eks&accelerator=h100&intent=training&selector=components.gpu-operator.values.driver.version"

# Get deployment order
curl -s "http://localhost:8080/v1/query?service=eks&accelerator=h100&intent=training&selector=deploymentOrder" | jq '.'

# Get a component subtree
curl -s "http://localhost:8080/v1/query?service=eks&accelerator=h100&selector=components.gpu-operator.values.driver" | jq '.'
```

---

### POST /v1/bundle

Generate deployment bundles from a recipe.

**Query Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `bundlers` | string | (all) | Comma-delimited list of bundler types to execute |
| `set` | string[] | | Value overrides (format: `bundler:path.to.field=value`). Repeat for multiple. |
| `system-node-selector` | string[] | | Node selectors for system components (format: `key=value`). Repeat for multiple. |
| `system-node-toleration` | string[] | | Tolerations for system components (format: `key=value:effect`). Repeat for multiple. |
| `accelerated-node-selector` | string[] | | Node selectors for GPU nodes (format: `key=value`). Repeat for multiple. |
| `accelerated-node-toleration` | string[] | | Tolerations for GPU nodes (format: `key=value:effect`). Repeat for multiple. |
| `nodes` | int | 0 | Estimated number of GPU nodes (0 = unset). Written to Helm value paths declared in the registry under `nodeScheduling.nodeCountPaths`. |
| `deployer` | string | helm | Deployment method: `helm` or `argocd` |

**Request Body:**

The request body is the recipe (RecipeResult) directly. No wrapper object needed.

#### Components

Bundler names correspond to component names in [`recipes/registry.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml). Any component registered there can be passed as a bundler. Current components:

| Component | Description |
|-----------|-------------|
| `gpu-operator` | NVIDIA GPU Operator — driver and runtime lifecycle |
| `network-operator` | NVIDIA Network Operator — RDMA, SR-IOV, host networking |
| `gke-nccl-tcpxo` | NCCL TCPxO network plugin for optimized collective communication (GKE) |
| `aws-efa` | AWS Elastic Fabric Adapter device plugin (EKS) |
| `cert-manager` | TLS certificate management |
| `skyhook-operator` | OS-level node tuning and kernel configuration |
| `skyhook-customizations` | Environment-specific node tuning profiles |
| `nvsentinel` | GPU health monitoring and automated remediation |
| `nvidia-dra-driver-gpu` | Dynamic Resource Allocation driver for GPUs |
| `kube-prometheus-stack` | Prometheus, Grafana, Alertmanager monitoring stack |
| `prometheus-adapter` | Custom metrics for HPA scaling |
| `aws-ebs-csi-driver` | Amazon EBS CSI driver (EKS) |
| `k8s-ephemeral-storage-metrics` | Ephemeral storage usage metrics |
| `kai-scheduler` | DRA-aware gang scheduler with topology-aware placement |
| `dynamo-crds` | NVIDIA Dynamo inference serving CRDs |
| `dynamo-platform` | NVIDIA Dynamo inference serving platform |
| `kgateway-crds` | Kubernetes Gateway API CRDs |
| `kgateway` | Kubernetes Gateway API implementation |
| `kubeflow-trainer` | Kubeflow Training Operator for distributed training |

**Examples:**

```shell
# Basic: pipe recipe to bundle (GPU Operator only)
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# Advanced: with value overrides and Argo CD deployer
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator&deployer=argocd&repo=https://github.com/my-org/my-gitops-repo.git&set=gpuoperator:gds.enabled=true" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# With node scheduling for system and GPU nodes
curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator&system-node-selector=nodeGroup=system&system-node-toleration=dedicated=system:NoSchedule&accelerated-node-selector=nvidia.com/gpu.present=true&accelerated-node-toleration=nvidia.com/gpu=present:NoSchedule" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Generate GPU Operator bundle from saved recipe
curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Generate all available bundles (no bundlers param)
curl -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "aicr.nvidia.com/v1alpha1",
    "kind": "Recipe",
    "componentRefs": [
      {"name": "gpu-operator", "version": "v25.3.3", "type": "helm"},
      {"name": "network-operator", "version": "v25.4.0", "type": "helm"}
    ]
  }' \
  -o bundles.zip

# Generate multiple specific bundles
curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator,network-operator" \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "aicr.nvidia.com/v1alpha1",
    "kind": "Recipe",
    "componentRefs": [
      {"name": "gpu-operator", "version": "v25.3.3", "type": "helm"},
      {"name": "network-operator", "version": "v25.4.0", "type": "helm"}
    ]
  }' \
  -o bundles.zip
```

**Response Headers:**

| Header | Description | Example |
|--------|-------------|---------|
| `Content-Type` | Always `application/zip` | `application/zip` |
| `Content-Disposition` | Download filename | `attachment; filename="bundles.zip"` |
| `X-Bundle-Files` | Total files in archive | `10` |
| `X-Bundle-Size` | Uncompressed size (bytes) | `45678` |
| `X-Bundle-Duration` | Generation time | `1.234s` |

#### Bundle Structure

```
bundles.zip
├── gpu-operator/
│   ├── values.yaml              # Helm chart values
│   ├── manifests/
│   │   ├── clusterpolicy.yaml   # ClusterPolicy CR
│   │   └── dcgm-exporter.yaml   # DCGM Exporter config
│   ├── scripts/
│   │   ├── install.sh           # Installation script
│   │   └── uninstall.sh         # Cleanup script
│   ├── README.md                # Deployment instructions
│   └── checksums.txt            # SHA256 checksums
└── network-operator/
    └── ...
```

---

### GET /health

Service health check (liveness probe).

```shell
curl "http://localhost:8080/health"
```

**Response:**
```json
{
  "status": "healthy",
  "timestamp": "2026-01-11T10:30:00Z"
}
```

---

### GET /ready

Service readiness check (readiness probe).

```shell
curl "http://localhost:8080/ready"
```

**Response:**
```json
{
  "status": "ready",
  "timestamp": "2026-01-11T10:30:00Z"
}
```

---

### GET /metrics

Prometheus metrics endpoint.

```shell
curl "http://localhost:8080/metrics"
```

**Key Metrics:**

| Metric | Type | Description |
|--------|------|-------------|
| `aicr_http_requests_total` | counter | Total HTTP requests by method, path, status |
| `aicr_http_request_duration_seconds` | histogram | Request latency distribution |
| `aicr_http_requests_in_flight` | gauge | Current concurrent requests |
| `aicr_rate_limit_rejects_total` | counter | Rate limit rejections |

## Complete Workflow Example

Fetch a recipe and generate bundles in one workflow:

```shell
#!/bin/bash

# Step 1: Get recipe for H100 on EKS for training
echo "Fetching recipe..."
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks&intent=training" \
  -o recipe.json

# Display recipe summary
echo "Recipe components:"
jq -r '.componentRefs[] | "  - \(.name): \(.version)"' recipe.json

# Step 2: Generate bundles from recipe (pipe directly)
echo "Generating bundles..."
curl -s -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Alternative: one-liner without intermediate file
# curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
#   curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator" \
#     -H "Content-Type: application/json" -d @- -o bundles.zip

# Step 3: Extract and verify
echo "Extracting bundles..."
unzip -q bundles.zip -d ./deployment

# Verify checksums
echo "Verifying checksums..."
cd deployment/gpu-operator
sha256sum -c checksums.txt

# Step 4: Deploy (example)
echo "Bundle ready for deployment:"
ls -la
```

## Error Handling

### Error Response Format

```json
{
  "code": "ERROR_CODE",
  "message": "Human-readable error description",
  "details": { ... },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-01-11T10:30:00Z",
  "retryable": true
}
```

### Error Codes

| Code | HTTP Status | Description | Retryable |
|------|-------------|-------------|-----------|
| `INVALID_REQUEST` | 400 | Invalid query parameters, request body, or disallowed criteria value | No |
| `METHOD_NOT_ALLOWED` | 405 | Wrong HTTP method | No |
| `NO_MATCHING_RULE` | 404 | No configuration found | No |
| `RATE_LIMIT_EXCEEDED` | 429 | Too many requests | Yes |
| `INTERNAL_ERROR` | 500 | Server error | Yes |

### Handling Rate Limits

```shell
# Check rate limit headers
curl -I "http://localhost:8080/v1/recipe?accelerator=h100"

# Response headers:
# X-RateLimit-Limit: 100
# X-RateLimit-Remaining: 95
# X-RateLimit-Reset: 1736589000
```

When rate limited (HTTP 429), use the `Retry-After` header:

```shell
# Retry with backoff
response=$(curl -s -w "%{http_code}" "http://localhost:8080/v1/recipe?accelerator=h100")
if [ "${response: -3}" = "429" ]; then
  retry_after=$(curl -sI "http://localhost:8080/v1/recipe" | grep -i "Retry-After" | awk '{print $2}')
  echo "Rate limited. Retrying after ${retry_after}s..."
  sleep "$retry_after"
fi
```

## Rate Limiting

- **Limit**: 100 requests per second per IP
- **Burst**: 200 requests
- **Headers**: `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`
- **429 Response**: Includes `Retry-After` header

## Criteria Allowlists

The API server can be configured to restrict which criteria values are allowed. This enables operators to limit the API to specific accelerators, services, intents, or OS types.

### Configuration

Allowlists are configured via environment variables when starting the server:

| Environment Variable | Description | Example |
|---------------------|-------------|---------|
| `AICR_ALLOWED_ACCELERATORS` | Comma-separated list of allowed GPU types | `h100,l40` |
| `AICR_ALLOWED_SERVICES` | Comma-separated list of allowed K8s services | `eks,gke` |
| `AICR_ALLOWED_INTENTS` | Comma-separated list of allowed workload intents | `training` |
| `AICR_ALLOWED_OS` | Comma-separated list of allowed OS types | `ubuntu,rhel` |

**Behavior:**
- If an environment variable is **not set**, all values for that criteria are allowed
- If an environment variable is **set**, only the specified values are permitted
- The `any` value is always allowed regardless of allowlist configuration
- Allowlists apply to both `/v1/recipe` and `/v1/bundle` endpoints

### Example Configuration

```shell
# Start server allowing only H100 and L40 GPUs on EKS
docker run -p 8080:8080 \
  -e AICR_ALLOWED_ACCELERATORS=h100,l40 \
  -e AICR_ALLOWED_SERVICES=eks \
  ghcr.io/nvidia/aicrd:latest
```

### Error Response

When a disallowed criteria value is requested:

```shell
curl "http://localhost:8080/v1/recipe?accelerator=gb200&service=eks"
```

**Response** (HTTP 400):
```json
{
  "code": "INVALID_REQUEST",
  "message": "accelerator type not allowed",
  "details": {
    "requested": "gb200",
    "allowed": ["h100", "l40"]
  },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-01-27T10:30:00Z",
  "retryable": false
}
```

### CLI Behavior

The CLI (`aicr`) is **not affected** by allowlists. Allowlists only apply to the API server, allowing operators to restrict API access while maintaining full CLI functionality for administrative tasks.

## Programming Language Examples

### Python

```python
import requests
import zipfile
import io

BASE_URL = "http://localhost:8080"

# Get recipe
params = {
    "accelerator": "h100",
    "service": "eks",
    "intent": "training",
    "os": "ubuntu"
}

resp = requests.get(f"{BASE_URL}/v1/recipe", params=params)
resp.raise_for_status()
recipe = resp.json()

print(f"Recipe has {len(recipe['componentRefs'])} components")

# Generate bundles — recipe is the request body, bundlers are query params
resp = requests.post(
    f"{BASE_URL}/v1/bundle",
    params={"bundlers": "gpu-operator"},
    json=recipe,
)
resp.raise_for_status()

# Extract zip
with zipfile.ZipFile(io.BytesIO(resp.content)) as zf:
    zf.extractall("./deployment")
    print(f"Extracted {len(zf.namelist())} files")
```

### Go

```go
package main

import (
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
)

func main() {
    baseURL := "http://localhost:8080"

    // Get recipe
    params := url.Values{}
    params.Add("accelerator", "h100")
    params.Add("service", "eks")
    
    resp, err := http.Get(baseURL + "/v1/recipe?" + params.Encode())
    if err != nil {
        panic(err)
    }
    defer resp.Body.Close()

    var recipe map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&recipe)
    
    fmt.Printf("Got recipe with %d components\n", 
        len(recipe["componentRefs"].([]interface{})))
}
```

### JavaScript/Node.js

```javascript
const BASE_URL = "http://localhost:8080";

async function main() {
    // Get recipe
    const params = new URLSearchParams({
        accelerator: "h100",
        service: "eks",
        intent: "training"
    });
    
    const recipeResp = await fetch(`${BASE_URL}/v1/recipe?${params}`);
    const recipe = await recipeResp.json();
    
    console.log(`Recipe has ${recipe.componentRefs.length} components`);
    
    // Generate bundles — recipe is the request body, bundlers are query params
    const bundleResp = await fetch(`${BASE_URL}/v1/bundle?bundlers=gpu-operator`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(recipe),
    });
    
    // Save zip
    const buffer = await bundleResp.arrayBuffer();
    require("fs").writeFileSync("bundles.zip", Buffer.from(buffer));
    console.log("Bundles saved to bundles.zip");
}

main();
```

### Shell Script (Batch Processing)

```bash
#!/bin/bash
# Generate recipes for multiple environments

environments=(
  "os=ubuntu&accelerator=h100&service=eks"
  "os=ubuntu&accelerator=gb200&service=gke"
  "os=rhel&accelerator=a100&service=aks"
)

for env in "${environments[@]}"; do
  echo "Fetching recipe for: $env"

  curl -s "http://localhost:8080/v1/recipe?${env}" \
    | jq -r '.componentRefs[] | "\(.name): \(.version)"'

  echo ""
done
```

## OpenAPI Specification

The full OpenAPI 3.1 specification is available at:
[api/aicr/v1/server.yaml](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml)

Generate client SDKs:

```shell
# Download spec
curl https://raw.githubusercontent.com/NVIDIA/aicr/main/api/aicr/v1/server.yaml \
  -o openapi.yaml

# Generate Python client
openapi-generator-cli generate -i openapi.yaml -g python -o ./python-client

# Generate Go client
openapi-generator-cli generate -i openapi.yaml -g go -o ./go-client

# Generate TypeScript client
openapi-generator-cli generate -i openapi.yaml -g typescript-fetch -o ./ts-client
```

## Troubleshooting

### Common Issues

**"Invalid accelerator type" error:**
```shell
# Use valid values: h100, gb200, a100, l40, any
curl "http://localhost:8080/v1/recipe?accelerator=h100"
```

**"Recipe is required" error:**
```shell
# Ensure recipe is in request body
curl -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" \
  -d '{"recipe": {...}}'  # recipe must not be null
```

**Empty zip file:**
```shell
# Check recipe has componentRefs
curl -s "http://localhost:8080/v1/recipe?accelerator=h100" | jq '.componentRefs'
```

**Connection refused (local):**
```shell
# Start local server first
make server
```

## See Also

- [CLI Reference](cli-reference.md) - Command-line interface
- [Agent Deployment](agent-deployment.md) - Kubernetes agent for snapshot capture
- [Installation Guide](installation.md) - Setup instructions
- [Data Flow](../integrator/data-flow.md) - Understanding recipe data architecture
- [Automation Guide](../integrator/automation.md) - CI/CD integration patterns
- [Kubernetes Deployment](../integrator/kubernetes-deployment.md) - Self-hosted API server deployment

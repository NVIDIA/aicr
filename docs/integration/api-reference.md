# API Reference

Complete reference for the Eidos API Server REST API.

## Overview

The API server provides HTTP REST access to recipe generation and bundle creation for GPU-accelerated infrastructure. The API accepts query parameters or recipe payloads and returns configuration recommendations or deployment bundles.

**Base URL:** `http://localhost:8080`

**Capabilities:**

- Recipe generation from query parameters (service type, accelerator, OS, workload intent, node count)
- Bundle generation from recipes (returns zip archive with Helm values, manifests, scripts)
- Rate limiting (100 requests/second)
- Health and readiness probes
- Prometheus metrics endpoint
- SLSA Build Level 3 attestations (signed artifacts, SBOM)

**Limitations:**

- Does not capture snapshots (use CLI `eidos snapshot` or Kubernetes agent Job)
- Does not analyze snapshots (query mode only; snapshot mode requires CLI)
- Does not interact with Kubernetes ConfigMaps (use CLI for `cm://` URIs)

**For complete workflow** (snapshot → recipe → bundle), use the CLI:

```bash
# Capture snapshot to ConfigMap
eidos snapshot -o cm://namespace/name

# Generate recipe from ConfigMap
eidos recipe -s cm://namespace/name -o recipe.yaml

# Create deployment bundle
eidos bundle -r recipe.yaml -o ./bundles
```

See [CLI Reference](../user-guide/cli-reference.md) and [Agent Deployment](../user-guide/agent-deployment.md).

## Authentication

**Current implementation:** No authentication required. API is publicly accessible.

**Note:** Authentication may be added in future releases. Check release notes before upgrading production integrations.

## Base URL

For local development:
```
http://localhost:8080
```

## Endpoints

### GET /

Service information and available routes.

**Response:**
```json
{
  "service": "eidosd",
  "version": "v0.7.6",
  "routes": ["/v1/recipe", "/v1/bundle"]
}
```

---

### GET /v1/recipe

Generate optimized configuration recipe based on environment parameters.

**Query Parameters:**

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `service` | string | No | any | K8s service type: eks, gke, aks, oke, any |
| `accelerator` | string | No | any | GPU/accelerator type: h100, gb200, a100, l40, any |
| `gpu` | string | No | any | Alias for `accelerator` (backwards compatibility) |
| `intent` | string | No | any | Workload intent: training, inference, any |
| `os` | string | No | any | GPU node OS: ubuntu, rhel, cos, amazonlinux, any |
| `platform` | string | No | any | Platform/framework type: pytorch, runai, any |
| `nodes` | integer | No | 0 | Number of GPU nodes (0 = any/unspecified) |

**Request Headers:**

| Header | Required | Description |
|--------|----------|-------------|
| `X-Request-Id` | No | Client-provided request ID for tracing |
| `Accept` | No | Content negotiation (future versioning) |

**Response Headers:**

| Header | Description |
|--------|-------------|
| `X-Request-Id` | Server-assigned or echoed request ID |
| `Cache-Control` | Cache directives (public, max-age=300) |
| `X-RateLimit-Limit` | Request quota (100/second) |
| `X-RateLimit-Remaining` | Remaining requests in window |
| `X-RateLimit-Reset` | Unix timestamp when quota resets |

**Success Response (200 OK):**

```json
{
  "apiVersion": "eidos.nvidia.com/v1alpha1",
  "kind": "Recipe",
  "metadata": {
    "version": "v1.0.0",
    "created": "2025-12-31T10:30:00Z",
    "appliedOverlays": [
      "base",
      "eks",
      "eks-training",
      "gb200-eks-training"
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

**Error Responses:**

**400 Bad Request** - Invalid parameters:
```json
{
  "code": "INVALID_REQUEST",
  "message": "invalid accelerator type: must be one of h100, gb200, a100, l40, any",
  "details": {
    "error": "invalid accelerator type: invalid-gpu"
  },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": false
}
```

**404 Not Found** - No matching configuration:
```json
{
  "code": "NO_MATCHING_RULE",
  "message": "no configuration recipe found for the specified parameters",
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": false
}
```

**429 Too Many Requests** - Rate limit exceeded:
```json
{
  "code": "RATE_LIMIT_EXCEEDED",
  "message": "rate limit exceeded, please retry after indicated time",
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": true
}
```

Response includes `Retry-After` header.

**500 Internal Server Error** - Server error:
```json
{
  "code": "INTERNAL_ERROR",
  "message": "an internal error occurred processing your request",
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": true
}
```

---

### POST /v1/bundle

Generate deployment bundles from a recipe.

**Description:**

Generates deployment bundles (Helm values, Kubernetes manifests, installation scripts) from a recipe and returns them as a compressed zip archive. The request body contains the recipe (RecipeResult) directly. Bundler types can be specified via the "bundlers" query parameter (comma-delimited). If no bundlers are specified, all registered bundlers are executed.

This design enables a simple workflow: pipe the output from GET /v1/recipe directly to POST /v1/bundle.

**Query Parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `bundlers` | string | No | Comma-delimited list of bundler types to execute. If empty, all bundlers run. |
| `set` | string[] | No | Value overrides (format: `bundler:path.to.field=value`). Can be repeated for multiple overrides. |
| `system-node-selector` | string[] | No | Node selectors for system components (format: `key=value`). Can be repeated. |
| `system-node-toleration` | string[] | No | Tolerations for system components (format: `key=value:effect` or `key:effect`). Can be repeated. |
| `accelerated-node-selector` | string[] | No | Node selectors for GPU nodes (format: `key=value`). Can be repeated. |
| `accelerated-node-toleration` | string[] | No | Tolerations for GPU nodes (format: `key=value:effect` or `key:effect`). Can be repeated. |
| `deployer` | string | No | Deployment method: `helm` (default), `argocd`. |
| `repo` | string | No | Git repository URL for GitOps deployments (used with `deployer=argocd`). Sets the repository URL in the generated `app-of-apps.yaml`. |

**Request Body:**

The recipe (RecipeResult) directly in the body:

```json
{
  "apiVersion": "eidos.nvidia.com/v1alpha1",
  "kind": "Recipe",
  "componentRefs": [
    {
      "name": "gpu-operator",
      "version": "v25.3.3",
      "type": "helm",
      "repository": "https://helm.ngc.nvidia.com/nvidia",
      "valuesFile": "components/gpu-operator/values.yaml"
    }
  ]
}
```

**Supported Bundler Types:**

- `gpu-operator` - NVIDIA GPU Operator
- `network-operator` - NVIDIA Network Operator
- `skyhook` - Skyhook node optimization
- `nvsentinel` - NVSentinel monitoring
- `cert-manager` - Certificate Manager

**Response Headers:**

| Header | Description |
|--------|-------------|
| `Content-Type` | `application/zip` |
| `Content-Disposition` | `attachment; filename="bundles.zip"` |
| `X-Bundle-Files` | Total number of files in the bundle |
| `X-Bundle-Size` | Total size in bytes (uncompressed) |
| `X-Bundle-Duration` | Time taken to generate bundles |

**Success Response (200 OK):**

Returns a binary zip archive containing a Helm umbrella chart. The archive structure:

```
bundles.zip
├── Chart.yaml                   # Helm chart metadata with component dependencies
├── values.yaml                  # Combined values for all components
├── README.md                    # Deployment instructions
├── recipe.yaml                  # Copy of the input recipe for reference
└── checksums.txt                # SHA256 checksums of generated files
```

**Error Responses:**

**400 Bad Request** - Empty recipe or missing components:
```json
{
  "code": "INVALID_REQUEST",
  "message": "Recipe must contain at least one component reference",
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": false
}
```

**400 Bad Request** - Invalid bundler type:
```json
{
  "code": "INVALID_REQUEST",
  "message": "Invalid bundler type",
  "details": {
    "bundler": "invalid-bundler",
    "error": "unsupported bundle type: invalid-bundler",
    "valid": ["gpu-operator", "network-operator", "skyhook-operator", "nvsentinel", "cert-manager", "nvidia-dra-driver-gpu"]
  },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": false
}
```

**500 Internal Server Error** - Some bundlers failed:
```json
{
  "code": "INTERNAL_ERROR",
  "message": "Some bundlers failed",
  "details": {
    "errors": [
      {"bundler": "gpu-operator", "error": "template rendering failed"}
    ],
    "success_count": 2
  },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-12-31T10:30:00Z",
  "retryable": true
}
```

---

### GET /health

Liveness probe endpoint.

**Response (200 OK):**
```json
{
  "status": "healthy",
  "timestamp": "2025-12-31T10:30:00Z"
}
```

---

### GET /ready

Readiness probe endpoint.

**Response (200 OK):**
```json
{
  "status": "ready",
  "timestamp": "2025-12-31T10:30:00Z"
}
```

**Response (503 Service Unavailable):**
```json
{
  "status": "not_ready",
  "timestamp": "2025-12-31T10:30:00Z",
  "reason": "service is initializing"
}
```

---

### GET /metrics

Prometheus metrics endpoint.

**Response (200 OK):**
```
# HELP eidos_http_requests_total Total HTTP requests
# TYPE eidos_http_requests_total counter
eidos_http_requests_total{method="GET",path="/v1/recipe",status="200"} 42

# HELP eidos_http_request_duration_seconds HTTP request duration
# TYPE eidos_http_request_duration_seconds histogram
eidos_http_request_duration_seconds_bucket{method="GET",path="/v1/recipe",le="0.1"} 40
eidos_http_request_duration_seconds_bucket{method="GET",path="/v1/recipe",le="0.5"} 42

# HELP eidos_http_requests_in_flight Current HTTP requests in flight
# TYPE eidos_http_requests_in_flight gauge
eidos_http_requests_in_flight 3

# HELP eidos_rate_limit_rejects_total Total rate limit rejections
# TYPE eidos_rate_limit_rejects_total counter
eidos_rate_limit_rejects_total 5
```

## Usage Examples

### cURL

**Basic query:**
```shell
curl "http://localhost:8080/v1/recipe?os=ubuntu&gpu=h100"
```

**Full specification:**
```shell
curl "http://localhost:8080/v1/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&nodes=8"
```

**With request ID:**
```shell
curl -H "X-Request-Id: $(uuidgen)" \
  "http://localhost:8080/v1/recipe?os=ubuntu&gpu=gb200"
```

**Save to file:**
```shell
curl "http://localhost:8080/v1/recipe?os=ubuntu&gpu=h100" -o recipe.json
```

**Generate bundles (pipe recipe directly):**
```shell
# One-liner: get recipe and generate bundle
curl -s "http://localhost:8080/v1/recipe?os=ubuntu&gpu=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# Or from saved recipe file
curl -X POST "http://localhost:8080/v1/bundle?bundlers=gpu-operator,network-operator" \
  -H "Content-Type: application/json" -d @recipe.json -o bundles.zip
```

### Python (requests)

```python
import requests

# Basic request
params = {
    'service': 'eks',
    'accelerator': 'h100',
    'intent': 'training',
    'os': 'ubuntu'
}

response = requests.get('http://localhost:8080/v1/recipe', params=params)

if response.status_code == 200:
    recipe = response.json()
    print(f"Applied {len(recipe['metadata']['appliedOverlays'])} overlays")
    for comp in recipe['componentRefs']:
        print(f"{comp['name']}: {comp['version']}")
else:
    error = response.json()
    print(f"Error: {error['message']}")
```

**With rate limiting:**
```python
import requests
import time

def get_recipe_with_retry(params, max_retries=3):
    for attempt in range(max_retries):
        response = requests.get('http://localhost:8080/v1/recipe', params=params)
        
        if response.status_code == 200:
            return response.json()
        elif response.status_code == 429:
            retry_after = int(response.headers.get('Retry-After', 60))
            print(f"Rate limited. Retrying after {retry_after} seconds...")
            time.sleep(retry_after)
        else:
            raise Exception(f"API error: {response.json()['message']}")
    
    raise Exception("Max retries exceeded")

recipe = get_recipe_with_retry({'service': 'eks', 'accelerator': 'h100'})
```

### Go (net/http)

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "net/url"
)

type Recipe struct {
    APIVersion    string                 `json:"apiVersion"`
    Kind          string                 `json:"kind"`
    Metadata      map[string]interface{} `json:"metadata"`
    Criteria      map[string]interface{} `json:"criteria"`
    ComponentRefs []ComponentRef         `json:"componentRefs"`
}

type ComponentRef struct {
    Name       string `json:"name"`
    Version    string `json:"version"`
    Order      int    `json:"order"`
    Repository string `json:"repository"`
}

func main() {
    baseURL := "http://localhost:8080/v1/recipe"
    
    // Build query
    params := url.Values{}
    params.Add("os", "ubuntu")
    params.Add("accelerator", "h100")
    params.Add("service", "eks")
    
    // Make request
    resp, err := http.Get(baseURL + "?" + params.Encode())
    if err != nil {
        panic(err)
    }
    defer resp.Body.Close()
    
    // Parse response
    var recipe Recipe
    if err := json.NewDecoder(resp.Body).Decode(&recipe); err != nil {
        panic(err)
    }
    
    fmt.Printf("Got %d component references\n", len(recipe.ComponentRefs))
}
```

### JavaScript (fetch)

```javascript
// Basic request
async function getRecipe(params) {
  const query = new URLSearchParams(params);
  const response = await fetch(`http://localhost:8080/v1/recipe?${query}`);
  
  if (!response.ok) {
    const error = await response.json();
    throw new Error(error.message);
  }
  
  return response.json();
}

// Usage
const recipe = await getRecipe({
  os: 'ubuntu',
  accelerator: 'h100',
  service: 'eks',
  intent: 'training'
});

console.log(`Got ${recipe.componentRefs.length} component references`);
```

**With rate limit handling:**
```javascript
async function getRecipeWithRetry(params, maxRetries = 3) {
  for (let attempt = 0; attempt < maxRetries; attempt++) {
    const query = new URLSearchParams(params);
    const response = await fetch(`http://localhost:8080/v1/recipe?${query}`);
    
    if (response.ok) {
      return response.json();
    }
    
    if (response.status === 429) {
      const retryAfter = parseInt(response.headers.get('Retry-After') || '60');
      console.log(`Rate limited. Retrying after ${retryAfter}s...`);
      await new Promise(resolve => setTimeout(resolve, retryAfter * 1000));
      continue;
    }
    
    const error = await response.json();
    throw new Error(error.message);
  }
  
  throw new Error('Max retries exceeded');
}
```

### Shell Script

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

## Rate Limiting

**Limits:**
- **Rate**: 100 requests per second per IP
- **Burst**: 200 requests

**Headers:**
- `X-RateLimit-Limit`: Maximum requests per window
- `X-RateLimit-Remaining`: Remaining requests
- `X-RateLimit-Reset`: Unix timestamp when window resets

**Best practices:**
1. Respect `Retry-After` header when rate limited
2. Implement exponential backoff
3. Cache responses when possible (Cache-Control header)
4. Use request IDs for debugging

## Error Handling

**Error response structure:**
```json
{
  "code": "ERROR_CODE",
  "message": "Human-readable message",
  "details": { /* Optional context */ },
  "requestId": "uuid",
  "timestamp": "ISO-8601",
  "retryable": true/false
}
```

**Error codes:**

| Code | HTTP Status | Description | Retryable |
|------|-------------|-------------|-----------|
| `INVALID_REQUEST` | 400 | Invalid query parameters | No |
| `METHOD_NOT_ALLOWED` | 405 | Wrong HTTP method | No |
| `NO_MATCHING_RULE` | 404 | No configuration found | No |
| `RATE_LIMIT_EXCEEDED` | 429 | Too many requests | Yes |
| `INTERNAL_ERROR` | 500 | Server error | Yes |
| `SERVICE_UNAVAILABLE` | 503 | Service temporarily down | Yes |

## OpenAPI Specification

Full OpenAPI 3.1 specification: [api/eidos/v1/server.yaml](../../../api/eidos/v1/server.yaml)

**Generate client SDKs:**
```shell
# Download spec
curl https://raw.githubusercontent.com/NVIDIA/eidos/main/api/eidos/v1/server.yaml -o spec.yaml

# Generate Python client
openapi-generator-cli generate -i spec.yaml -g python -o ./python-client

# Generate Go client
openapi-generator-cli generate -i spec.yaml -g go -o ./go-client
```

## Deployment

See [Kubernetes Deployment](kubernetes-deployment.md) for deploying your own API server instance.

## Monitoring

**Health checks:**
```shell
# Liveness
curl http://localhost:8080/health

# Readiness
curl http://localhost:8080/ready
```

**Metrics (Prometheus):**
```shell
curl http://localhost:8080/metrics
```

**Key metrics:**
- `eidos_http_requests_total` - Total requests by method, path, status
- `eidos_http_request_duration_seconds` - Request latency histogram
- `eidos_http_requests_in_flight` - Current concurrent requests
- `eidos_rate_limit_rejects_total` - Rate limit rejections

## See Also

- [User Guide: API Reference](../user-guide/api-reference.md) - Quick start and usage examples
- [Data Flow](data-flow.md) - Understanding recipe data architecture
- [Automation](automation.md) - CI/CD integration patterns
- [Kubernetes Deployment](kubernetes-deployment.md) - Self-hosted deployment
- [CLI Reference](../user-guide/cli-reference.md) - CLI alternative

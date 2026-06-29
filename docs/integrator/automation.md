# Automation and CI/CD Integration

Integration patterns for using AICR in automated pipelines.

## Overview

Typical integration workflows:

1. **Snapshot capture**: Deploy agent Job to capture cluster configuration
2. **Recipe generation**: Generate configuration recommendations from snapshot or query parameters
3. **Bundle creation**: Create deployment artifacts (Helm values, manifests, scripts)
4. **Deployment**: Apply generated configuration to cluster
5. **Validation**: Verify deployment using test workloads

**Supported CI/CD platforms**: GitHub Actions, GitLab CI, Jenkins, Argo Workflows, Tekton

## Integration Patterns

### Pattern 1: Configuration Snapshot + Drift Detection

Periodically capture snapshots and compare against baseline.

**Use case:** Detect unauthorized configuration changes

```yaml
# GitHub Actions
name: Configuration Drift Detection
on:
  schedule:
    - cron: '0 */6 * * *'  # Every 6 hours

jobs:
  snapshot:
    runs-on: ubuntu-latest
    steps:
      - name: Configure kubectl
        uses: azure/k8s-set-context@v4
        with:
          kubeconfig: ${{ secrets.KUBECONFIG }}
      
      - name: Deploy AICR Agent
        run: |
          # aicr snapshot deploys the in-cluster Job, waits synchronously for
          # it to complete, writes the result, then cleans the Job up — no
          # separate `kubectl wait` step is needed.
          aicr snapshot --namespace gpu-operator \
            --output cm://gpu-operator/aicr-snapshot --timeout 300s
      
      - name: Capture snapshot from ConfigMap
        run: |
          kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
      
      - name: Compare with baseline
        run: |
          # Download baseline
          curl -O https://your-artifacts/baseline.yaml

          # Compare with the semantic differ. A raw `diff` would flag every
          # new snapshot timestamp as drift; `aicr diff --fail-on-drift`
          # exits non-zero only on a meaningful configuration change.
          aicr diff --baseline baseline.yaml --target snapshot.yaml \
            --fail-on-drift
      
      - name: Upload artifact
        uses: actions/upload-artifact@v4
        with:
          name: cluster-snapshots
          path: snapshot-*.yaml
```

### Pattern 2: Canonical Snapshot to Bundle Pipeline

Generate optimized configuration and deploy operators. The pipeline below is
the canonical reference: every stage uses the same `aicr` CLI invocations, so
it translates directly to any CI system (see [Translating to other CI
systems](#translating-to-other-ci-systems) below).

**Use case:** Deploy GPU Operator with environment-specific settings

```yaml
# GitHub Actions
name: GPU Stack Deploy
on:
  workflow_dispatch:

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Configure kubectl
        uses: azure/k8s-set-context@v4
        with:
          kubeconfig: ${{ secrets.KUBECONFIG }}

      # 1. Snapshot: agent Job writes cluster state to a ConfigMap.
      #    aicr snapshot waits synchronously for the Job and cleans it up,
      #    so no separate `kubectl wait` step is needed.
      - name: Capture snapshot
        run: |
          aicr snapshot --namespace gpu-operator \
            --output cm://gpu-operator/aicr-snapshot --timeout 300s

      # 2. Recipe: read the snapshot ConfigMap, emit an optimized recipe.
      #    Use --service/--accelerator/--intent flags for query mode instead.
      - name: Generate recipe
        run: |
          aicr recipe \
            --snapshot cm://gpu-operator/aicr-snapshot \
            --intent training \
            --platform kubeflow \
            --output recipe.yaml

      # 3. Bundle: render deployment artifacts. Add --set to override values,
      #    or --deployer argocd for GitOps output (see Pattern 3).
      - name: Create bundle
        run: aicr bundle --recipe recipe.yaml --output ./bundles

      # 4. Deploy: verify checksums, then run the generated script.
      - name: Deploy
        run: |
          cd bundles
          sha256sum -c checksums.txt
          chmod +x deploy.sh
          ./deploy.sh
```

#### Translating to other CI systems

The four stages above map one-to-one onto other platforms. Only the job/stage
syntax and artifact passing differ — the `aicr` commands are identical.

| Stage | GitLab CI | CircleCI | Terraform |
|-------|-----------|----------|-----------|
| Snapshot | `script:` step running `aicr snapshot`, declare `artifacts: paths: [snapshot.yaml]` (or write to a ConfigMap to skip artifacts) | `run:` step; `persist_to_workspace` to pass output downstream | `null_resource` + `local-exec` provisioner running `aicr snapshot` |
| Recipe | `script:` step running `aicr recipe`, `dependencies:` on the snapshot job | `run:` step after `attach_workspace` | `null_resource` + `local-exec`, `depends_on` the snapshot resource |
| Bundle | `script:` step running `aicr bundle`, publish `bundles/` as artifacts | `run:` step, `persist_to_workspace` | `null_resource` + `local-exec` running `aicr bundle` |
| Deploy | `script:` step with `when: manual` for approval gating | `run:` step inside a held workflow | `local-exec` running `deploy.sh`, gated by `terraform apply` approval |

Use a container image with the CLI preinstalled (`ghcr.io/nvidia/aicr:latest`)
for the recipe/bundle stages, and a `kubectl`-capable image for snapshot/deploy.

### Pattern 3: GitOps Deployment with Argo CD

Use Argo CD for declarative, GitOps-based deployments with automatic sync-wave ordering.

**Use case:** Automated deployment pipeline with Argo CD

```yaml
# GitHub Actions
name: GitOps Deploy with Argo CD
on:
  push:
    branches: [main]

jobs:
  generate-and-deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4
      
      - name: Setup aicr
        run: |
          # GoReleaser archives are versioned (aicr_<version>_<os>_<arch>.tar.gz),
          # so resolve the latest tag first rather than a fixed filename.
          VERSION=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
          curl -sLO "https://github.com/NVIDIA/aicr/releases/download/${VERSION}/aicr_${VERSION#v}_linux_amd64.tar.gz"
          tar -xzf "aicr_${VERSION#v}_linux_amd64.tar.gz"
          sudo mv aicr /usr/local/bin/
      
      - name: Generate recipe
        run: |
          aicr recipe \
            --service eks \
            --accelerator h100 \
            --intent training \
            --os ubuntu \
            --output recipe.yaml
      
      - name: Generate Argo CD bundles
        run: |
          aicr bundle \
            --recipe recipe.yaml \
            --deployer argocd \
            --repo https://github.com/${{ github.repository }}.git \
            --output ./bundles
      
      - name: Commit to GitOps repo
        run: |
          # Copy entire bundle to GitOps repository
          # Argo CD apps are in NNN-<component>/application.yaml files
          # app-of-apps.yaml is at bundle root
          cp -r bundles/* gitops-repo/
          
          cd gitops-repo
          git add .
          git commit -m "Update GPU stack components"
          git push
```

**Generated Argo CD Application with multi-source:**
```yaml
# bundles/NNN-gpu-operator/application.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: gpu-operator
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"  # Deployed after cert-manager (wave 0)
spec:
  project: default
  sources:
    # Helm chart from upstream
    - repoURL: https://helm.ngc.nvidia.com/nvidia
      chart: gpu-operator
      targetRevision: v26.3.2
      helm:
        valueFiles:
          # Values live under the numbered bundle dir (NNN-<component>/)
          - $values/002-gpu-operator/values.yaml
    # Values from GitOps repo
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      ref: values
  # Post-install manifests (ClusterPolicy, etc.) are NOT a source on this
  # Application — the bundler emits them as a separate numbered Application
  # (e.g. NNN-gpu-operator-post/application.yaml) ordered by sync-wave.
  destination:
    server: https://kubernetes.default.svc
    namespace: gpu-operator
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

### Pattern 4: Multi-Environment GitOps

Deploy to multiple environments with environment-specific deployers.

```bash
#!/bin/bash
# multi-env-gitops.sh

ENVIRONMENTS=(
  "staging:helm"       # Staging uses Helm per-component bundle
  "production:argocd"  # Production uses Argo CD
)

for env_config in "${ENVIRONMENTS[@]}"; do
  IFS=":" read -r ENV DEPLOYER <<< "$env_config"
  
  echo "Generating bundles for $ENV with $DEPLOYER deployer..."
  
  aicr bundle \
    --recipe "recipes/${ENV}.yaml" \
    --deployer "$DEPLOYER" \
    --output "./bundles/${ENV}"
  
  echo "Generated $DEPLOYER bundles in ./bundles/${ENV}/"
done
```

## Monitoring and Alerting

### Prometheus Metrics

**Scrape AICR API Server:**
```yaml
# prometheus-config.yaml
scrape_configs:
  - job_name: 'aicrd'
    static_configs:
      - targets: ['aicrd.default.svc.cluster.local:8080']
    metrics_path: /metrics
```

**Key metrics:**
```promql
# Request rate
rate(aicr_http_requests_total[5m])

# Error rate
rate(aicr_http_requests_total{status=~"5.."}[5m])

# Latency (p95)
histogram_quantile(0.95, 
  rate(aicr_http_request_duration_seconds_bucket[5m])
)

# Rate limit rejections
rate(aicr_rate_limit_rejects_total[5m])
```

### Alerting Rules

```yaml
# prometheus-rules.yaml
groups:
  - name: aicr_alerts
    interval: 30s
    rules:
      - alert: AICRHighErrorRate
        expr: |
          rate(aicr_http_requests_total{status=~"5.."}[5m]) > 0.05
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "AICR API high error rate"
          description: "Error rate is {{ $value | humanizePercentage }}"
      
      - alert: AICRHighLatency
        expr: |
          histogram_quantile(0.95,
            rate(aicr_http_request_duration_seconds_bucket[5m])
          ) > 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "AICR API high latency"
          description: "P95 latency is {{ $value }}s"
      
      - alert: AICRRateLimitHit
        expr: |
          rate(aicr_rate_limit_rejects_total[5m]) > 1
        for: 5m
        labels:
          severity: info
        annotations:
          summary: "AICR API rate limit reached"
          description: "Rate limit rejections: {{ $value }}/s"
```

## Best Practices

### 1. Caching Recipes

API responses are cacheable (recipe and query responses carry
`Cache-Control: public, max-age=600` — a 10-minute TTL). Note this is the
HTTP response cache only; the server's internal per-`Client` provider caches
are not time-bounded and persist until `Client.Close()` is called.

```python
import requests
from cachetools import TTLCache

# Cache recipes for 10 minutes, matching the server's max-age=600
recipe_cache = TTLCache(maxsize=100, ttl=600)

def get_recipe_cached(params):
    cache_key = frozenset(params.items())
    
    if cache_key not in recipe_cache:
        response = requests.get('http://localhost:8080/v1/recipe', params=params)
        recipe_cache[cache_key] = response.json()
    
    return recipe_cache[cache_key]
```

### 2. Error Handling and Retries

```python
import requests
from tenacity import retry, stop_after_attempt, wait_exponential

@retry(
    stop=stop_after_attempt(3),
    wait=wait_exponential(multiplier=1, min=4, max=10)
)
def get_recipe_with_retry(params):
    response = requests.get('http://localhost:8080/v1/recipe', params=params)
    response.raise_for_status()
    return response.json()
```

### 3. Parallel Recipe Generation

```python
from concurrent.futures import ThreadPoolExecutor
import requests

def get_recipe(params):
    response = requests.get('http://localhost:8080/v1/recipe', params=params)
    return response.json()

# Generate recipes for multiple environments in parallel
environments = [
    {'os': 'ubuntu', 'accelerator': 'h100', 'service': 'eks'},
    {'os': 'ubuntu', 'accelerator': 'gb200', 'service': 'gke'},
    {'os': 'rhel', 'accelerator': 'a100', 'service': 'aks'},
]

with ThreadPoolExecutor(max_workers=3) as executor:
    recipes = list(executor.map(get_recipe, environments))
```

### 4. Structured Logging

```python
import logging
import json

# Configure structured logging
logging.basicConfig(
    level=logging.INFO,
    format='%(message)s'
)

def log_recipe_request(params, recipe, duration):
    logging.info(json.dumps({
        'event': 'recipe_generated',
        'params': params,
        'component_refs': len(recipe.get('componentRefs', [])),
        'applied_overlays': len(recipe.get('metadata', {}).get('appliedOverlays', [])),
        'duration_ms': duration * 1000
    }))
```

### 5. Snapshot Versioning

```bash
#!/bin/bash
# Save snapshots with metadata

CLUSTER="prod-us-east-1"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
OUTPUT="snapshot-${CLUSTER}-${TIMESTAMP}.yaml"

# Capture snapshot from ConfigMap
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > "$OUTPUT"

# Add metadata
cat << EOF > "${OUTPUT}.meta"
cluster: $CLUSTER
timestamp: $TIMESTAMP
git_commit: $(git rev-parse HEAD)
k8s_version: $(kubectl version -o json | jq -r '.serverVersion.gitVersion')
EOF

# Upload to artifact storage
aws s3 cp "$OUTPUT" "s3://my-bucket/snapshots/"
aws s3 cp "${OUTPUT}.meta" "s3://my-bucket/snapshots/"
```

## Security Considerations

> **Note:** The API server does not yet provide built-in authentication
> (API keys or Bearer tokens). Front it with an ingress, service mesh, or
> API gateway that enforces authn/authz, and restrict reachability with the
> network policy below.

### Network Policies

Restrict AICR agent network access:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: aicr-agent
  namespace: gpu-operator
spec:
  podSelector:
    matchLabels:
      job-name: aicr
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector: {}
      ports:
        - protocol: TCP
          port: 443  # Kubernetes API
```

## Troubleshooting

### Debug API Calls

```bash
# Verbose curl
curl -v "http://localhost:8080/v1/recipe?os=ubuntu&accelerator=h100"

# With timing
curl -w "\nTime: %{time_total}s\n" \
  "http://localhost:8080/v1/recipe?os=ubuntu&accelerator=h100"

# Check headers
curl -I "http://localhost:8080/v1/recipe?os=ubuntu&accelerator=h100"
```

### Validate Snapshots

```bash
# Check YAML syntax
yamllint snapshot.yaml

# Validate structure
yq eval '.measurements | length' snapshot.yaml

# Check for required measurements
yq eval '.measurements[] | .type' snapshot.yaml | sort -u
```

### Test Recipe Generation

```bash
# Generate and validate
aicr recipe --os ubuntu --accelerator h100 --output recipe.yaml
yamllint recipe.yaml

# Check applied overlays
yq eval '.metadata.appliedOverlays' recipe.yaml

# Extract GPU Operator version from componentRefs
yq eval '.componentRefs[] | select(.name=="gpu-operator") | .version' recipe.yaml
```

## See Also

- [API Reference](../user/api-reference.md) - API endpoint documentation
- [Data Flow](data-flow.md) - Understanding data architecture
- [Kubernetes Deployment](kubernetes-deployment.md) - Self-hosted API server
- [CLI Reference](../user/cli-reference.md) - CLI commands
- [Agent Deployment](../user/agent-deployment.md) - Kubernetes agent

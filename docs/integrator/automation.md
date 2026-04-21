# Automation and CI/CD Integration

This guide describes integration patterns for using AICR in automated pipelines.

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
        uses: azure/k8s-set-context@v1
        with:
          kubeconfig: ${{ secrets.KUBECONFIG }}
      
      - name: Deploy AICR Agent
        run: |
          aicr snapshot --output cm://gpu-operator/aicr-snapshot --timeout 300s
      
      - name: Wait for completion
        run: |
          kubectl wait --for=condition=complete --timeout=300s job/aicr -n gpu-operator
      
      - name: Capture snapshot from ConfigMap
        run: |
          kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}' > snapshot-$(date +%Y%m%d-%H%M%S).yaml
      
      - name: Compare with baseline
        run: |
          # Download baseline
          curl -O https://your-artifacts/baseline.yaml
          
          # Compare
          if ! diff -q baseline.yaml snapshot-*.yaml; then
            echo "::error::Configuration drift detected"
            diff baseline.yaml snapshot-*.yaml
            exit 1
          fi
      
      - name: Upload artifact
        uses: actions/upload-artifact@v3
        with:
          name: cluster-snapshots
          path: snapshot-*.yaml
```

### Pattern 2: Recipe-Based Deployment

Generate optimized configuration and deploy operators.

**Use case:** Deploy GPU Operator with environment-specific settings

```yaml
# GitLab CI
stages:
  - snapshot
  - recipe
  - bundle
  - deploy

capture_snapshot:
  stage: snapshot
  image: bitnami/kubectl:latest
  script:
    - aicr snapshot --output snapshot.yaml --timeout 300s
  artifacts:
    paths:
      - snapshot.yaml

generate_recipe:
  stage: recipe
  image: ghcr.io/nvidia/aicr:latest
  script:
    # Option 1: Use ConfigMap directly (no artifact needed)
    - aicr recipe -s cm://gpu-operator/aicr-snapshot --intent training --platform kubeflow -o recipe.yaml
    # Option 2: Use snapshot file from previous stage
    # - aicr recipe --snapshot snapshot.yaml --intent training --platform kubeflow --output recipe.yaml
  artifacts:
    paths:
      - recipe.yaml
  dependencies:
    - capture_snapshot

create_bundle:
  stage: bundle
  image: ghcr.io/nvidia/aicr:latest
  script:
    - aicr bundle --recipe recipe.yaml --output ./bundles
    # Override values at bundle generation time
    # - aicr bundle -r recipe.yaml --set gpuoperator:gds.enabled=true -o ./bundles
  artifacts:
    paths:
      - bundles/
  dependencies:
    - generate_recipe

deploy_operators:
  stage: deploy
  image: bitnami/kubectl:latest
  script:
    - cd bundles
    - sha256sum -c checksums.txt
    - chmod +x deploy.sh
    - ./deploy.sh
  dependencies:
    - create_bundle
  when: manual
```

### Pattern 3: API-Driven Recipe Generation

Use API for recipe generation without installing CLI.

**Use case:** Lightweight recipe generation in containers

```yaml
# CircleCI
version: 2.1

jobs:
  generate_recipe:
    docker:
      - image: cimg/base:2025.01
    steps:
      - run:
          name: Generate recipe via API
          command: |
            # Detect environment
            OS="ubuntu"
            GPU="h100"
            SERVICE="eks"
            
            # Generate recipe
            curl -s "http://localhost:8080/v1/recipe?os=${OS}&gpu=${GPU}&service=${SERVICE}&intent=training" \
              -o recipe.json
            
            # Validate
            jq -e '.measurements | length > 0' recipe.json
      
      - persist_to_workspace:
          root: .
          paths:
            - recipe.json
  
  extract_versions:
    docker:
      - image: cimg/base:2025.01
    steps:
      - attach_workspace:
          at: .
      
      - run:
          name: Extract component versions
          command: |
            # GPU Operator version from componentRefs
            GPU_OP_VERSION=$(jq -r '.componentRefs[] | 
              select(.name=="gpu-operator") | .version' recipe.json)
            
            echo "GPU Operator: $GPU_OP_VERSION"
            
            # Save for deployment
            echo "export GPU_OP_VERSION=$GPU_OP_VERSION" >> $BASH_ENV

workflows:
  deploy:
    jobs:
      - generate_recipe
      - extract_versions:
          requires:
            - generate_recipe
```

### Pattern 4: Multi-Cluster Management

Deploy consistent configurations across multiple clusters.

**Use case:** Multi-region GPU clusters with unified configuration

```bash
#!/bin/bash
# multi-cluster-deploy.sh

# Define clusters
CLUSTERS=(
  "prod-us-east-1:eks:h100"
  "prod-eu-west-1:eks:h100"
  "staging-us-west-2:eks:gb200"
)

# Iterate clusters
for cluster_config in "${CLUSTERS[@]}"; do
  IFS=":" read -r CLUSTER SERVICE GPU <<< "$cluster_config"
  
  echo "Processing cluster: $CLUSTER"
  
  # Switch context
  kubectl config use-context "$CLUSTER"
  
  # Capture snapshot
  aicr snapshot --output "snapshot-${CLUSTER}.yaml" --timeout 300s
  
  # Generate recipe (can use ConfigMap directly or file)
  # Option 1: Use ConfigMap
  aicr recipe -s "cm://gpu-operator/aicr-snapshot" --intent training --platform kubeflow -o "recipe-${CLUSTER}.yaml"
  # Option 2: Use saved file
  # aicr recipe --snapshot "snapshot-${CLUSTER}.yaml" --intent training --platform kubeflow --output "recipe-${CLUSTER}.yaml"
  
  # Create bundle
  aicr bundle \
    --recipe "recipe-${CLUSTER}.yaml" \
    --output "./bundles/${CLUSTER}"

  # Or with value overrides for environment-specific customization
  # aicr bundle \
  #   --recipe "recipe-${CLUSTER}.yaml" \
  #   --set gpuoperator:gds.enabled=true \
  #   --set gpuoperator:mig.strategy=mixed \
  #   --output "./bundles/${CLUSTER}"
  
  # Deploy (with approval)
  echo "Deploy to $CLUSTER? [y/N]"
  read -r response
  if [[ "$response" =~ ^[Yy]$ ]]; then
    cd "bundles/${CLUSTER}"
    chmod +x deploy.sh && ./deploy.sh
    cd -
  fi
  
  # Clean up
  kubectl delete job aicr -n gpu-operator
done
```

### Pattern 5: GitOps Deployment with Argo CD

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
          curl -sLO https://github.com/nvidia/aicr/releases/latest/download/aicr_linux_amd64.tar.gz
          tar -xzf aicr_linux_amd64.tar.gz
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
          # Argo CD apps are in <component>/argocd/ directories
          # app-of-apps.yaml is at bundle root
          cp -r bundles/* gitops-repo/
          
          cd gitops-repo
          git add .
          git commit -m "Update GPU stack components"
          git push
```

**Generated Argo CD Application with multi-source:**
```yaml
# bundles/gpu-operator/argocd/application.yaml
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
      targetRevision: v25.3.3
      helm:
        valueFiles:
          - $values/gpu-operator/values.yaml
    # Values from GitOps repo
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      ref: values
    # Additional manifests (ClusterPolicy, etc.)
    - repoURL: <YOUR_GIT_REPO>
      targetRevision: main
      path: gpu-operator/manifests
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

### Pattern 6: Multi-Environment GitOps

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

## Terraform Integration

### Module: AICR Agent Deployment

```hcl
# modules/aicr-agent/main.tf

# Deploy agent and capture snapshot using CLI
resource "null_resource" "capture_snapshot" {
  provisioner "local-exec" {
    command = <<-EOT
      aicr snapshot \
        --output ${var.snapshot_output} \
        --timeout 300s
    EOT
  }
}

# Generate recipe (can use ConfigMap directly)
resource "null_resource" "generate_recipe" {
  provisioner "local-exec" {
    command = <<-EOT
      aicr recipe \
        -s cm://gpu-operator/aicr-snapshot \
        --intent ${var.workload_intent} \
        -o ${var.recipe_output}
    EOT
  }
  
  depends_on = [null_resource.wait_for_snapshot]
}

# variables.tf
variable "node_selector" {
  description = "Node selector for agent pod"
  type        = map(string)
  default     = { "nvidia.com/gpu.present" = "true" }
}

variable "tolerations" {
  description = "Tolerations for agent pod"
  type        = list(object({
    key    = string
    value  = string
    effect = string
  }))
  default = []
}

variable "image_version" {
  description = "AICR image version"
  type        = string
  default     = "latest"
}

variable "snapshot_output" {
  description = "Path to save snapshot"
  type        = string
  default     = "snapshot.yaml"
}

variable "recipe_output" {
  description = "Path to save recipe"
  type        = string
  default     = "recipe.yaml"
}

variable "workload_intent" {
  description = "Workload intent: training or inference"
  type        = string
  default     = "training"
}

# outputs.tf
output "snapshot_file" {
  value = var.snapshot_output
}

output "recipe_file" {
  value = var.recipe_output
}
```

**Usage:**
```hcl
# main.tf
module "aicr_agent" {
  source = "./modules/aicr-agent"
  
  node_selector = {
    "nodeGroup" = "gpu-nodes"
  }
  
  tolerations = [{
    key    = "nvidia.com/gpu"
    value  = ""
    effect = "NoSchedule"
  }]
  
  workload_intent = "training"
  snapshot_output = "cluster-${var.environment}-snapshot.yaml"
  recipe_output   = "cluster-${var.environment}-recipe.yaml"
}
```

## Kubernetes Operators

### Custom Operator: Configuration Drift Watcher

```go
// Watch for configuration changes and reconcile
package main

import (
    "context"
    "fmt"
    "time"
    
    "k8s.io/client-go/kubernetes"
    ctrl "sigs.k8s.io/controller-runtime"
)

type ConfigReconciler struct {
    Client    kubernetes.Interface
    Namespace string
}

func (r *ConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. Deploy AICR agent
    if err := r.deployAgent(ctx); err != nil {
        return ctrl.Result{}, err
    }
    
    // 2. Wait for completion
    if err := r.waitForJob(ctx); err != nil {
        return ctrl.Result{}, err
    }
    
    // 3. Retrieve snapshot
    snapshot, err := r.getSnapshot(ctx)
    if err != nil {
        return ctrl.Result{}, err
    }
    
    // 4. Compare with baseline
    if r.hasConfigDrift(snapshot) {
        // Alert or auto-remediate
        fmt.Println("Configuration drift detected!")
    }
    
    // 5. Clean up
    if err := r.cleanupAgent(ctx); err != nil {
        return ctrl.Result{}, err
    }
    
    // Requeue after 6 hours
    return ctrl.Result{RequeueAfter: 6 * time.Hour}, nil
}

func (r *ConfigReconciler) deployAgent(ctx context.Context) error {
    // Apply RBAC and Job manifests
    return nil
}

func (r *ConfigReconciler) waitForJob(ctx context.Context) error {
    // Wait for job completion with timeout
    return nil
}

func (r *ConfigReconciler) getSnapshot(ctx context.Context) (string, error) {
    // Retrieve snapshot from ConfigMap
    return "", nil
}

func (r *ConfigReconciler) hasConfigDrift(snapshot string) bool {
    // Compare with baseline
    return false
}

func (r *ConfigReconciler) cleanupAgent(ctx context.Context) error {
    // Delete job
    return nil
}
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

API responses are cacheable (Cache-Control: max-age=300):

```python
import requests
from cachetools import TTLCache

# Cache recipes for 5 minutes
recipe_cache = TTLCache(maxsize=100, ttl=300)

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
    {'os': 'ubuntu', 'gpu': 'h100', 'service': 'eks'},
    {'os': 'ubuntu', 'gpu': 'gb200', 'service': 'gke'},
    {'os': 'rhel', 'gpu': 'a100', 'service': 'aks'},
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

### API Key Management (Future)

```python
import os
import requests

API_KEY = os.environ.get('AICR_API_KEY')

headers = {
    'Authorization': f'Bearer {API_KEY}',
    'X-Request-Id': generate_uuid()
}

response = requests.get(
    'http://localhost:8080/v1/recipe',
    params={'os': 'ubuntu', 'gpu': 'h100'},
    headers=headers
)
```

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

### Secrets Management

```yaml
# kubernetes-secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: aicr-credentials
  namespace: gpu-operator
type: Opaque
stringData:
  api-key: your-api-key-here
```

```yaml
# Reference in pod
env:
  - name: AICR_API_KEY
    valueFrom:
      secretKeyRef:
        name: aicr-credentials
        key: api-key
```

## Troubleshooting

### Debug API Calls

```bash
# Verbose curl
curl -v "http://localhost:8080/v1/recipe?os=ubuntu&gpu=h100"

# With timing
curl -w "\nTime: %{time_total}s\n" \
  "http://localhost:8080/v1/recipe?os=ubuntu&gpu=h100"

# Check headers
curl -I "http://localhost:8080/v1/recipe?os=ubuntu&gpu=h100"
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

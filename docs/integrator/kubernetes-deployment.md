# Kubernetes Deployment

Deploy the Eidos API Server in your Kubernetes cluster for self-hosted recipe generation.

## Overview

**API Server deployment** enables self-hosted recipe generation:

- Isolated deployment: Recipe data stays within your infrastructure
- Custom recipes: Modify embedded recipe data (see `pkg/recipe/data/`)
- High availability: Deploy multiple replicas with load balancing
- Observability: Prometheus `/metrics` endpoint and structured logging

**API Server scope:**

- Recipe generation from query parameters (query mode)
- Does not capture snapshots (use agent Job or CLI)
- Does not generate bundles (use CLI)
- Does not analyze snapshots (query mode only)

**Agent deployment** (separate component):

- Kubernetes Job captures cluster configuration
- Writes snapshot to ConfigMap via Kubernetes API
- Requires RBAC: ServiceAccount with ConfigMap create/update permissions
- See [Agent Deployment](../user/agent-deployment.md)

**Typical workflow:**

1. Deploy agent Job → Captures snapshot → Writes to ConfigMap
2. CLI reads ConfigMap → Generates recipe → Writes to file or ConfigMap
3. CLI reads recipe → Generates bundle → Writes to filesystem
4. Apply bundle to cluster (Helm install, kubectl apply)

## Quick Start

### Deploy with Kustomize

```shell
# Create namespace
kubectl create namespace eidos

# Deploy API server
kubectl apply -k https://github.com/NVIDIA/eidos/deployments/eidosd

# Check deployment
kubectl get pods -n eidos
kubectl get svc -n eidos
```

### Deploy with Helm

**Status**: Helm chart not yet available. Use Kustomize or manual deployment.

<!-- Uncomment when Helm chart is published
```shell
helm repo add eidos https://nvidia.github.io/eidos
helm install eidosd eidos/eidosd -n eidos --create-namespace
```
-->

## Manual Deployment

### 1. Create Namespace

```yaml
# namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: eidos
  labels:
    app: eidosd
```

```shell
kubectl apply -f namespace.yaml
```

### 2. Create Deployment

```yaml
# deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: eidosd
  namespace: eidos
  labels:
    app: eidosd
spec:
  replicas: 3
  selector:
    matchLabels:
      app: eidosd
  template:
    metadata:
      labels:
        app: eidosd
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
        prometheus.io/path: "/metrics"
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
      
      containers:
        - name: api-server
          image: ghcr.io/nvidia/eidosd:latest
          imagePullPolicy: IfNotPresent
          
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          
          env:
            - name: PORT
              value: "8080"
            - name: LOG_LEVEL
              value: "info"
          
          livenessProbe:
            httpGet:
              path: /health
              port: http
            initialDelaySeconds: 10
            periodSeconds: 30
            timeoutSeconds: 5
            failureThreshold: 3
          
          readinessProbe:
            httpGet:
              path: /ready
              port: http
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 3
          
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 512Mi
          
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
```

```shell
kubectl apply -f deployment.yaml
```

### 3. Create Service

```yaml
# service.yaml
apiVersion: v1
kind: Service
metadata:
  name: eidosd
  namespace: eidos
  labels:
    app: eidosd
spec:
  type: ClusterIP
  selector:
    app: eidosd
  ports:
    - name: http
      port: 80
      targetPort: http
      protocol: TCP
```

```shell
kubectl apply -f service.yaml
```

### 4. Create Ingress (Optional)

```yaml
# ingress.yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: eidosd
  namespace: eidos
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/rate-limit: "100"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - eidos.yourdomain.com
      secretName: eidos-tls
  rules:
    - host: eidos.yourdomain.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: eidosd
                port:
                  number: 80
```

```shell
kubectl apply -f ingress.yaml
```

## Agent Deployment

Deploy the Eidos Agent as a Kubernetes Job to automatically capture cluster configuration.

### 1. Create RBAC Resources

```yaml
# agent-rbac.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: eidos
  namespace: gpu-operator
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: eidos
  namespace: gpu-operator
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: eidos
  namespace: gpu-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: eidos
subjects:
- kind: ServiceAccount
  name: eidos
  namespace: gpu-operator  # Must match ServiceAccount namespace
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: eidos
rules:
- apiGroups: [""]
  resources: ["nodes", "pods"]
  verbs: ["get", "list"]
- apiGroups: ["nvidia.com"]
  resources: ["clusterpolicies"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: eidos
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: eidos
subjects:
- kind: ServiceAccount
  name: eidos
  namespace: gpu-operator
```

```shell
kubectl apply -f agent-rbac.yaml
```

### 2. Create Agent Job

```yaml
# agent-job.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: eidos
  namespace: gpu-operator
  labels:
    app: eidos-agent
spec:
  template:
    metadata:
      labels:
        app: eidos-agent
    spec:
      serviceAccountName: eidos
      restartPolicy: Never
      
      containers:
      - name: eidos
        image: ghcr.io/nvidia/eidos:latest
        imagePullPolicy: IfNotPresent
        
        command:
        - eidos
        - snapshot
        - --output
        - cm://gpu-operator/eidos-snapshot
        
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          runAsNonRoot: true
          runAsUser: 65532
          capabilities:
            drop: ["ALL"]
```

```shell
kubectl apply -f agent-job.yaml

# Wait for completion
kubectl wait --for=condition=complete job/eidos -n gpu-operator --timeout=5m

# Verify ConfigMap was created
kubectl get configmap eidos-snapshot -n gpu-operator

# View snapshot data
kubectl get configmap eidos-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

### 3. Generate Recipe from ConfigMap

```bash
# Using CLI (local or in another Job)
eidos recipe --snapshot cm://gpu-operator/eidos-snapshot \
             --intent training \
             --platform kubeflow \
             --output recipe.yaml

# Or write recipe back to ConfigMap
eidos recipe --snapshot cm://gpu-operator/eidos-snapshot \
             --intent training \
             --platform kubeflow \
             --output cm://gpu-operator/eidos-recipe
```

### 4. Generate Bundle

```bash
# From file
eidos bundle --recipe recipe.yaml --output ./bundles

# From ConfigMap
eidos bundle --recipe cm://gpu-operator/eidos-recipe --output ./bundles
```

### E2E Testing

Validate the complete workflow:

```bash
# Run all CLI integration tests (no cluster needed)
make e2e

# Run cluster-based E2E tests (requires Kind cluster)
make e2e-tilt
```

CLI tests use [Kyverno Chainsaw](https://github.com/kyverno/chainsaw) for declarative YAML assertions. See `tests/chainsaw/README.md` for details.

## Configuration Options

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | 8080 | HTTP server port |
| `LOG_LEVEL` | info | Logging level: debug, info, warn, error |
| `RATE_LIMIT` | 100 | Requests per second |
| `RATE_BURST` | 200 | Burst capacity |
| `READ_TIMEOUT` | 30s | HTTP read timeout |
| `WRITE_TIMEOUT` | 30s | HTTP write timeout |
| `IDLE_TIMEOUT` | 60s | HTTP idle timeout |

**Note:** The API server uses structured JSON logging to stderr. The CLI supports three logging modes (CLI/Text/JSON), but the API server always uses JSON for consistent log aggregation.

### ConfigMap for Custom Recipe Data (Advanced)

> **Note:** This example shows the concept of mounting custom recipe data. The actual recipe format uses a base-plus-overlay architecture. See `pkg/recipe/data/` for the current schema (`overlays/*.yaml` including `base.yaml`).

```yaml
# configmap.yaml - Example showing custom recipe data mounting
apiVersion: v1
kind: ConfigMap
metadata:
  name: eidos-recipe-data
  namespace: eidos
data:
  overlays/base.yaml: |
    # Your custom base recipe
    apiVersion: eidos.nvidia.com/v1alpha1
    kind: RecipeMetadata
    # ... (see pkg/recipe/data/overlays/base.yaml for schema)
```

Mount in deployment:
```yaml
spec:
  template:
    spec:
      volumes:
        - name: recipe-data
          configMap:
            name: eidos-recipe-data
      containers:
        - name: api-server
          volumeMounts:
            - name: recipe-data
              mountPath: /data
          env:
            - name: RECIPE_DATA_PATH
              value: /data
```

## High Availability

### Horizontal Pod Autoscaler

```yaml
# hpa.yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: eidosd
  namespace: eidos
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: eidosd
  minReplicas: 3
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: 80
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
        - type: Percent
          value: 50
          periodSeconds: 60
    scaleUp:
      stabilizationWindowSeconds: 0
      policies:
        - type: Percent
          value: 100
          periodSeconds: 15
```

```shell
kubectl apply -f hpa.yaml
```

### Pod Disruption Budget

```yaml
# pdb.yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: eidosd
  namespace: eidos
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: eidosd
```

```shell
kubectl apply -f pdb.yaml
```

## Monitoring

### Prometheus ServiceMonitor

```yaml
# servicemonitor.yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: eidosd
  namespace: eidos
  labels:
    app: eidosd
spec:
  selector:
    matchLabels:
      app: eidosd
  endpoints:
    - port: http
      path: /metrics
      interval: 30s
      scrapeTimeout: 10s
```

```shell
kubectl apply -f servicemonitor.yaml
```

### Grafana Dashboard

Import dashboard JSON from `docs/monitoring/grafana-dashboard.json`:

**Key panels:**
- Request rate (by status code)
- Request duration (p50, p95, p99)
- Error rate
- Rate limit rejections
- Active connections

## Security

### Network Policies

```yaml
# networkpolicy.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: eidosd
  namespace: eidos
spec:
  podSelector:
    matchLabels:
      app: eidosd
  policyTypes:
    - Ingress
    - Egress
  ingress:
    - from:
        - namespaceSelector: {}
      ports:
        - protocol: TCP
          port: 8080
  egress:
    - to:
        - namespaceSelector: {}
      ports:
        - protocol: TCP
          port: 53  # DNS
    - to:
        - namespaceSelector:
            matchLabels:
              name: kube-system
      ports:
        - protocol: TCP
          port: 443  # Kubernetes API
```

### Pod Security Standards

```yaml
# Add to namespace
apiVersion: v1
kind: Namespace
metadata:
  name: eidos
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

### RBAC (If API server needs K8s access)

```yaml
# serviceaccount.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: eidosd
  namespace: eidos

---
# role.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: eidosd
rules:
  - apiGroups: [""]
    resources: ["nodes", "pods"]
    verbs: ["get", "list"]

---
# rolebinding.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: eidosd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: eidosd
subjects:
  - kind: ServiceAccount
    name: eidosd
    namespace: eidos
```

## Troubleshooting

### Check Pod Status

```shell
# Pod status
kubectl get pods -n eidos

# Describe pod
kubectl describe pod -n eidos -l app=eidosd

# View logs
kubectl logs -n eidos -l app=eidosd

# Follow logs
kubectl logs -n eidos -l app=eidosd -f
```

### Check Service

```shell
# Service status
kubectl get svc -n eidos

# Endpoints
kubectl get endpoints -n eidos

# Test from within cluster
kubectl run -it --rm debug --image=curlimages/curl --restart=Never -- \
  curl http://eidosd.eidos.svc.cluster.local/health
```

### Check Ingress

```shell
# Ingress status
kubectl get ingress -n eidos

# Describe ingress
kubectl describe ingress eidosd -n eidos

# Check cert-manager certificate
kubectl get certificate -n eidos
```

### Performance Issues

```shell
# Check resource usage
kubectl top pods -n eidos

# Check HPA status
kubectl get hpa -n eidos

# Check metrics
kubectl exec -n eidos -it deploy/eidosd -- \
  wget -qO- http://localhost:8080/metrics
```

### Connection Refused

1. Check service exists: `kubectl get svc -n eidos`
2. Check endpoints: `kubectl get endpoints -n eidos`
3. Check pod is ready: `kubectl get pods -n eidos`
4. Check readiness probe: `kubectl describe pod -n eidos <pod-name>`

### Rate Limiting

Check rate limit settings:
```shell
kubectl exec -n eidos deploy/eidosd -- env | grep RATE
```

Adjust via deployment:
```yaml
env:
  - name: RATE_LIMIT
    value: "200"  # Increase limit
  - name: RATE_BURST
    value: "400"
```

## Upgrading

### Rolling Update

```shell
# Update image
kubectl set image deployment/eidosd \
  api-server=ghcr.io/nvidia/eidosd:v0.8.0 \
  -n eidos

# Watch rollout
kubectl rollout status deployment/eidosd -n eidos

# Rollback if needed
kubectl rollout undo deployment/eidosd -n eidos
```

### Blue-Green Deployment

```shell
# Deploy new version
kubectl apply -f deployment-v2.yaml

# Switch service
kubectl patch service eidosd -n eidos \
  -p '{"spec":{"selector":{"version":"v2"}}}'

# Delete old deployment
kubectl delete deployment eidosd-v1 -n eidos
```

## Backup and Disaster Recovery

### Export Configuration

```shell
# Export all resources
kubectl get all -n eidos -o yaml > eidos-backup.yaml

# Export specific resources
kubectl get deployment,service,ingress -n eidos -o yaml > eidos-config.yaml
```

### Restore from Backup

```shell
# Restore namespace and resources
kubectl apply -f eidos-backup.yaml
```

## Cost Optimization

### Resource Limits

Start with minimal resources:
```yaml
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi
```

Monitor and adjust based on usage.

### Vertical Pod Autoscaler (Optional)

```yaml
# vpa.yaml
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: eidosd
  namespace: eidos
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: eidosd
  updatePolicy:
    updateMode: "Auto"
```

## See Also

- [API Reference](../user/api-reference.md) - API endpoint documentation
- [Automation](automation.md) - CI/CD integration
- [Data Flow](data-flow.md) - Understanding data architecture
- [API Server Architecture](../contributor/api-server.md) - Internal architecture

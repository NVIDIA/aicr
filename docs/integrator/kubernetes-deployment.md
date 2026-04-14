# Kubernetes Deployment

Deploy the AICR API Server in your Kubernetes cluster for self-hosted recipe generation.

## Overview

**API Server deployment** enables self-hosted recipe generation:

- Isolated deployment: Recipe data stays within your infrastructure
- Custom recipes: Modify embedded recipe data (see `recipes/`)
- High availability: Deploy multiple replicas with load balancing
- Observability: Prometheus `/metrics` endpoint and structured logging

**API Server scope:**

- Recipe generation from query parameters (query mode)
- Does not capture snapshots (use agent Job or CLI)
- Generates bundles via `POST /v1/bundle`
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
kubectl create namespace aicr

# Deploy API server (save the manifest from the Deployment section below as aicrd-deployment.yaml)
kubectl apply -f aicrd-deployment.yaml

# Check deployment
kubectl get pods -n aicr
kubectl get svc -n aicr
```

### Deploy with Helm

**Status**: Helm chart not yet available. Use Kustomize or manual deployment.


## Manual Deployment

### 1. Create Namespace

```yaml
# namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: aicr
  labels:
    app: aicrd
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
  name: aicrd
  namespace: aicr
  labels:
    app: aicrd
spec:
  replicas: 3
  selector:
    matchLabels:
      app: aicrd
  template:
    metadata:
      labels:
        app: aicrd
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
          image: ghcr.io/nvidia/aicrd:latest
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
  name: aicrd
  namespace: aicr
  labels:
    app: aicrd
spec:
  type: ClusterIP
  selector:
    app: aicrd
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
  name: aicrd
  namespace: aicr
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/rate-limit: "100"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - aicr.yourdomain.com
      secretName: aicr-tls
  rules:
    - host: aicr.yourdomain.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: aicrd
                port:
                  number: 80
```

```shell
kubectl apply -f ingress.yaml
```

## Agent Deployment

Deploy the AICR Agent as a Kubernetes Job to automatically capture cluster configuration.

### 1. Create RBAC Resources

```yaml
# agent-rbac.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: aicr
  namespace: gpu-operator
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: aicr
  namespace: gpu-operator
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: aicr
  namespace: gpu-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aicr
subjects:
- kind: ServiceAccount
  name: aicr
  namespace: gpu-operator  # Must match ServiceAccount namespace
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aicr
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
  name: aicr
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aicr
subjects:
- kind: ServiceAccount
  name: aicr
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
  name: aicr
  namespace: gpu-operator
  labels:
    app: aicr-agent
spec:
  template:
    metadata:
      labels:
        app: aicr-agent
    spec:
      serviceAccountName: aicr
      restartPolicy: Never
      
      containers:
      - name: aicr
        image: ghcr.io/nvidia/aicr:latest
        imagePullPolicy: IfNotPresent
        
        command:
        - aicr
        - snapshot
        - --output
        - cm://gpu-operator/aicr-snapshot
        
        securityContext:
          privileged: true
          runAsUser: 0
          runAsGroup: 0
      hostPID: true
      hostNetwork: true
      hostIPC: true
      volumes:
      - name: systemd
        hostPath:
          path: /run/systemd
          type: Directory
```

> **Note:** The agent defaults to privileged mode, which is required for GPU, SystemD, and OS collectors. For PSS-restricted namespaces where only the Kubernetes collector is needed, use `--privileged=false` when deploying via the CLI. See [Agent Deployment](../user/agent-deployment.md) for details.

```shell
kubectl apply -f agent-job.yaml

# Wait for completion
kubectl wait --for=condition=complete job/aicr -n gpu-operator --timeout=5m

# Verify ConfigMap was created
kubectl get configmap aicr-snapshot -n gpu-operator

# View snapshot data
kubectl get configmap aicr-snapshot -n gpu-operator -o jsonpath='{.data.snapshot\.yaml}'
```

### 3. Generate Recipe from ConfigMap

```bash
# Using CLI (local or in another Job)
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot \
             --intent training \
             --platform kubeflow \
             --output recipe.yaml

# Or write recipe back to ConfigMap
aicr recipe --snapshot cm://gpu-operator/aicr-snapshot \
             --intent training \
             --platform kubeflow \
             --output cm://gpu-operator/aicr-recipe
```

### 4. Generate Bundle

```bash
# From file
aicr bundle --recipe recipe.yaml --output ./bundles

# From ConfigMap
aicr bundle --recipe cm://gpu-operator/aicr-recipe --output ./bundles
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

> **Note:** This example shows the concept of mounting custom recipe data. The actual recipe format uses a base-plus-overlay architecture. See `recipes/` for the current schema (`overlays/*.yaml` including `base.yaml`).

```yaml
# configmap.yaml - Example showing custom recipe data mounting
apiVersion: v1
kind: ConfigMap
metadata:
  name: aicr-recipe-data
  namespace: aicr
data:
  overlays/base.yaml: |
    # Your custom base recipe
    apiVersion: aicr.nvidia.com/v1alpha1
    kind: RecipeMetadata
    # ... (see recipes/overlays/base.yaml for schema)
```

Mount in deployment:
```yaml
spec:
  template:
    spec:
      volumes:
        - name: recipe-data
          configMap:
            name: aicr-recipe-data
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
  name: aicrd
  namespace: aicr
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: aicrd
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
  name: aicrd
  namespace: aicr
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: aicrd
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
  name: aicrd
  namespace: aicr
  labels:
    app: aicrd
spec:
  selector:
    matchLabels:
      app: aicrd
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
  name: aicrd
  namespace: aicr
spec:
  podSelector:
    matchLabels:
      app: aicrd
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
  name: aicr
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
  name: aicrd
  namespace: aicr

---
# role.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aicrd
rules:
  - apiGroups: [""]
    resources: ["nodes", "pods"]
    verbs: ["get", "list"]

---
# rolebinding.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aicrd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aicrd
subjects:
  - kind: ServiceAccount
    name: aicrd
    namespace: aicr
```

## Troubleshooting

### Check Pod Status

```shell
# Pod status
kubectl get pods -n aicr

# Describe pod
kubectl describe pod -n aicr -l app=aicrd

# View logs
kubectl logs -n aicr -l app=aicrd

# Follow logs
kubectl logs -n aicr -l app=aicrd -f
```

### Check Service

```shell
# Service status
kubectl get svc -n aicr

# Endpoints
kubectl get endpoints -n aicr

# Test from within cluster
kubectl run -it --rm debug --image=curlimages/curl --restart=Never -- \
  curl http://aicrd.aicr.svc.cluster.local/health
```

### Check Ingress

```shell
# Ingress status
kubectl get ingress -n aicr

# Describe ingress
kubectl describe ingress aicrd -n aicr

# Check cert-manager certificate
kubectl get certificate -n aicr
```

### Performance Issues

```shell
# Check resource usage
kubectl top pods -n aicr

# Check HPA status
kubectl get hpa -n aicr

# Check metrics
kubectl exec -n aicr -it deploy/aicrd -- \
  wget -qO- http://localhost:8080/metrics
```

### Connection Refused

1. Check service exists: `kubectl get svc -n aicr`
2. Check endpoints: `kubectl get endpoints -n aicr`
3. Check pod is ready: `kubectl get pods -n aicr`
4. Check readiness probe: `kubectl describe pod -n aicr <pod-name>`

### Rate Limiting

Check rate limit settings:
```shell
kubectl exec -n aicr deploy/aicrd -- env | grep RATE
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
kubectl set image deployment/aicrd \
  api-server=ghcr.io/nvidia/aicrd:v0.8.0 \
  -n aicr

# Watch rollout
kubectl rollout status deployment/aicrd -n aicr

# Rollback if needed
kubectl rollout undo deployment/aicrd -n aicr
```

### Blue-Green Deployment

```shell
# Deploy new version
kubectl apply -f deployment-v2.yaml

# Switch service
kubectl patch service aicrd -n aicr \
  -p '{"spec":{"selector":{"version":"v2"}}}'

# Delete old deployment
kubectl delete deployment aicrd-v1 -n aicr
```

## Backup and Disaster Recovery

### Export Configuration

```shell
# Export all resources
kubectl get all -n aicr -o yaml > aicr-backup.yaml

# Export specific resources
kubectl get deployment,service,ingress -n aicr -o yaml > aicr-config.yaml
```

### Restore from Backup

```shell
# Restore namespace and resources
kubectl apply -f aicr-backup.yaml
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
  name: aicrd
  namespace: aicr
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: aicrd
  updatePolicy:
    updateMode: "Auto"
```

## See Also

- [API Reference](../user/api-reference.md) - API endpoint documentation
- [Automation](automation.md) - CI/CD integration
- [Data Flow](data-flow.md) - Understanding data architecture
- [API Server Architecture](../contributor/api-server.md) - Internal architecture

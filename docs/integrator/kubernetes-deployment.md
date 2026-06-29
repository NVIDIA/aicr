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

```shell
# Create namespace
kubectl create namespace aicr

# Deploy API server (save the manifest from the Deployment section below as aicrd-deployment.yaml)
kubectl apply -f aicrd-deployment.yaml

# Check deployment
kubectl get pods -n aicr
kubectl get svc -n aicr
```

> **Helm chart**: Not yet available. Use the manual manifests below.

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
            - name: AICR_LOG_LEVEL
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
          # POST /v1/bundle writes to a per-request os.MkdirTemp under /tmp.
          # With readOnlyRootFilesystem: true, mount a writable emptyDir at
          # /tmp or every bundle request fails with HTTP 500.
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
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

## Capturing Snapshots (Agent)

The API server only generates recipes and bundles — it does not capture
cluster state. Snapshot capture is a separate concern handled by the AICR
agent Job, including its RBAC (ServiceAccount, Role, ClusterRole), the
privileged-mode requirement, ConfigMap storage (`cm://<ns>/<name>`), and the
full snapshot → recipe → bundle CLI flow. That material is documented
canonically in [Agent Deployment](../user/agent-deployment.md) and is not
duplicated here.

## Configuration Options

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | 8080 | HTTP server port |
| `SHUTDOWN_TIMEOUT_SECONDS` | 30 | Graceful-shutdown drain timeout (seconds) |
| `AICR_LOG_LEVEL` | info | Logging level: debug, info, warn, error |
| `AICR_ALLOWED_ACCELERATORS` | (unset = all) | Comma-separated allowlist of accelerator types (e.g. `h100,l40`) |
| `AICR_ALLOWED_SERVICES` | (unset = all) | Comma-separated allowlist of service types (e.g. `eks,gke`) |
| `AICR_ALLOWED_INTENTS` | (unset = all) | Comma-separated allowlist of intent types (e.g. `training,inference`) |
| `AICR_ALLOWED_OS` | (unset = all) | Comma-separated allowlist of OS types (e.g. `ubuntu,rhel`) |

**Note:** These are the only environment variables the API server reads. The four `AICR_ALLOWED_*` allowlists are parsed once at startup to restrict which criteria values the server will accept. Rate-limit, request-timeout, and body-size settings are compiled-in constants from `pkg/defaults`, not environment-tunable. The server uses structured JSON logging to stderr. The CLI supports three logging modes (CLI/Text/JSON), but the API server always uses JSON for consistent log aggregation.

### ConfigMap for Custom Recipe Data (Advanced)

> **Note:** The `aicrd` HTTP server resolves recipes from the binary's
> **embedded** catalog only — it is not wired to consume an external recipe-data
> overlay at runtime, so there is no ConfigMap mount or environment variable that
> injects custom recipe data into the server.
>
> Custom recipe data (`overlays/*.yaml`, `validators/catalog.yaml`, …) is layered
> on top of the embedded catalog via the `aicr` CLI's `--data <dir>` flag (see
> [Data Extension](data-extension.md)). To serve customized data over HTTP today,
> bake the additional overlays/catalog entries into a custom `aicrd` image rather
> than mounting them at runtime. (Note that the data layout is a directory tree,
> and ConfigMap data keys must match `[-._a-zA-Z0-9]+` — they cannot contain `/`,
> so a flat ConfigMap cannot represent the `overlays/…` layout in any case.)

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

Rate-limit settings are **compiled-in** constants from `pkg/defaults`; the
server does not read `RATE_LIMIT`/`RATE_BURST` (or any rate-limit) environment
variables. To change the effective limits, front the server with an
ingress/gateway that enforces its own rate limit (see the Ingress example
above, which sets `nginx.ingress.kubernetes.io/rate-limit`), or build a custom
`aicrd` image with adjusted `pkg/defaults` values.

Rate-limit rejections surface in the `aicr_rate_limit_rejects_total` metric and
as HTTP 429 responses with the `X-RateLimit-*` headers.

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

The aicrd server is stateless — it holds no persistent data, so there is
nothing to back up beyond the manifests in this guide (keep them in version
control). Standard Kubernetes patterns apply unchanged for blue-green/canary
rollouts, backup/restore of resource definitions, and right-sizing requests
and limits (start small — see the requests/limits in the
[Deployment](#2-create-deployment) above — and adjust from `kubectl top`
output or a Vertical Pod Autoscaler). Refer to the upstream
[Kubernetes documentation](https://kubernetes.io/docs/concepts/workloads/)
for these; none require AICR-specific handling.

## See Also

- [API Reference](../user/api-reference.md) - API endpoint documentation
- [Automation](automation.md) - CI/CD integration
- [Data Flow](data-flow.md) - Understanding data architecture
- [API Server Architecture](../contributor/api-server.md) - Internal architecture

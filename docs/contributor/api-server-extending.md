# API Server: Extension and Operating Patterns

This page collects forward-looking guidance and operating patterns for the
`aicrd` API server that go beyond what is implemented in
[`pkg/api`](https://github.com/NVIDIA/aicr/tree/main/pkg/api) /
[`pkg/server`](https://github.com/NVIDIA/aicr/tree/main/pkg/server) today.
The code samples are illustrative — they describe how a feature *would* be
implemented or operated, not behavior that ships in the current binary.

For what the API server actually does today (architecture, request flow,
implemented endpoints, current observability, security model), see
[API Server Architecture](api-server.md).

> **Status:** Patterns and code in this document are aspirational unless
> explicitly noted otherwise. Treat them as a roadmap and reference, not
> as wired-up behavior. Do not link them from runbooks without verifying
> the underlying capability exists in the current build.

## Future Enhancements

### Near-Term Ideas

1. **Authentication & Authorization**  
   **Rationale**: Protect API from unauthorized access, enable usage tracking  
   **Implementation**: API key middleware with HMAC-SHA256 verification  
   **Example**:

   ```go
   func APIKeyMiddleware(validKeys map[string]string) func(http.Handler) http.Handler {
       return func(next http.Handler) http.Handler {
           return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
               key := r.Header.Get("X-API-Key")
               if _, ok := validKeys[key]; !ok {
                   http.Error(w, "Invalid API key", http.StatusUnauthorized)
                   return
               }
               next.ServeHTTP(w, r)
           })
       }
   }
   ```

   **Reference**: [HTTP Authentication](https://developer.mozilla.org/en-US/docs/Web/HTTP/Authentication)

2. **CORS Support**  
   **Use Case**: Enable browser-based clients (web dashboards)  
   **Implementation**: `rs/cors` middleware with configurable origins  
   **Configuration**:

   ```go
   c := cors.New(cors.Options{
       AllowedOrigins:   []string{"https://dashboard.example.com"},
       AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
       AllowedHeaders:   []string{"Content-Type", "X-API-Key"},
       AllowCredentials: true,
       MaxAge:           86400, // 24 hours
   })
   handler := c.Handler(mux)
   ```

   **Reference**: [CORS Specification](https://developer.mozilla.org/en-US/docs/Web/HTTP/CORS)

3. **Response Compression**  
   **Benefit**: Reduce bandwidth by 70-80% for JSON responses  
   **Implementation**: `gziphandler` middleware with quality threshold  

   ```go
   import "github.com/NYTimes/gziphandler"
   
   handler := gziphandler.GzipHandler(mux)
   // Only compresses responses > 1KB
   ```

   **Trade-off**: CPU usage (+5-10%) vs bandwidth savings  
   **Reference**: [gziphandler](https://github.com/NYTimes/gziphandler)

4. **Native TLS Support**  
   **Rationale**: Eliminate need for reverse proxy in simple deployments  
   **Implementation**: `http.ListenAndServeTLS` with Let's Encrypt integration  

   ```go
   import "golang.org/x/crypto/acme/autocert"
   
   m := &autocert.Manager{
       Prompt:      autocert.AcceptTOS,
       Cache:       autocert.DirCache("/var/cache/aicr"),
       HostPolicy:  autocert.HostWhitelist("api.example.com"),
   }
   
   srv := &http.Server{
       Addr:      ":https",
       TLSConfig: m.TLSConfig(),
       Handler:   handler,
   }
   srv.ListenAndServeTLS("", "")
   ```

   **Reference**: [autocert Package](https://pkg.go.dev/golang.org/x/crypto/acme/autocert)

5. **API Versioning**  
   **Use Case**: Support /v2 API with breaking changes while maintaining /v1  
   **Pattern**: URL-based versioning with version-specific handlers  

   ```go
   v1 := http.NewServeMux()
   v1.HandleFunc("/recipe", handleRecipeV1)
   
   v2 := http.NewServeMux()
   v2.HandleFunc("/recipe", handleRecipeV2)
   
   mux := http.NewServeMux()
   mux.Handle("/v1/", http.StripPrefix("/v1", v1))
   mux.Handle("/v2/", http.StripPrefix("/v2", v2))
   ```

   **Reference**: [API Versioning Best Practices](https://cloud.google.com/apis/design/versioning)

### Mid-Term Ideas

1. **OpenTelemetry Integration**  
   **Use Case**: Distributed tracing across services  
   **Implementation**: OTLP exporter with automatic instrumentation  

   ```go
   import (
       "go.opentelemetry.io/otel"
       "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
       "go.opentelemetry.io/otel/sdk/trace"
   )
   
   func initTracer() (*trace.TracerProvider, error) {
       exporter, err := otlptracehttp.New(context.Background(),
           otlptracehttp.WithEndpoint("otel-collector:4318"),
           otlptracehttp.WithInsecure(),
       )
       if err != nil {
           return nil, err
       }
       
       tp := trace.NewTracerProvider(
           trace.WithBatcher(exporter),
           trace.WithResource(/* service name */),
       )
       otel.SetTracerProvider(tp)
       return tp, nil
   }
   ```

   **Reference**: [OpenTelemetry Go](https://opentelemetry.io/docs/languages/go/)

2. **Recipe Caching**  
   **Benefit**: 95%+ cache hit rate for repeated queries  
   **Implementation**: Redis with TTL, fallback to recipe builder  

   ```go
   import "github.com/redis/go-redis/v9"
   
   func getRecipe(ctx context.Context, key string) (*recipe.Recipe, error) {
       // Try cache first
       cached, err := rdb.Get(ctx, key).Result()
       if err == nil {
           var r recipe.Recipe
           if err := json.Unmarshal([]byte(cached), &r); err != nil {
               return nil, errors.Wrap(errors.ErrCodeInternal, "unmarshal cached recipe", err)
           }
           return &r, nil
       }

       // Cache miss - build recipe
       r, err := builder.BuildRecipe(ctx, params)
       if err != nil {
           return nil, err
       }

       // Cache with 1 hour TTL
       data, err := json.Marshal(r)
       if err != nil {
           return nil, errors.Wrap(errors.ErrCodeInternal, "marshal recipe", err)
       }
       rdb.Set(ctx, key, data, time.Hour)

       return r, nil
   }
   ```

   **Reference**: [go-redis](https://redis.uptrace.dev/)

3. **GraphQL API**  
   **Rationale**: Enable clients to request only needed fields  
   **Implementation**: `graphql-go` with recipe schema  

   ```graphql
   type Query {
     recipe(
       os: String!
       osVersion: String
       gpu: String!
       service: String
     ): Recipe
   }
   
   type Recipe {
     request: RequestInfo!
     measurements: [Measurement!]!
     context: RecipeContext
   }
   ```

   **Trade-off**: Added complexity vs flexible querying  
   **Reference**: [GraphQL Go](https://graphql.org/code/#go)

### Longer-Term Ideas

1. **gRPC Support**  
   **Benefit**: 5-10x better performance, smaller payloads  
   **Implementation**: Protobuf definition with streaming support  

   ```protobuf
   service RecipeService {
     rpc GetRecipe(RecipeRequest) returns (Recipe);
     rpc StreamRecipes(stream RecipeRequest) returns (stream Recipe);
     rpc GetSnapshot(SnapshotRequest) returns (Snapshot);
   }
   
   message RecipeRequest {
     string os = 1;
     string os_version = 2;
     string gpu = 3;
     string service = 4;
   }
   ```

   **Deployment**: Run HTTP/2 and gRPC on same port with `cmux`  
   **Reference**: [gRPC Go](https://grpc.io/docs/languages/go/quickstart/)

2. **Multi-Tenancy**  
    **Use Case**: SaaS deployment with per-customer isolation  
    **Implementation**: Tenant ID from API key, separate rate limits  

    ```go
    type TenantRateLimiter struct {
        limiters map[string]*rate.Limiter
        mu       sync.RWMutex
    }
    
    func (t *TenantRateLimiter) Allow(tenantID string) bool {
        t.mu.RLock()
        limiter, exists := t.limiters[tenantID]
        t.mu.RUnlock()
        
        if !exists {
            t.mu.Lock()
            limiter = rate.NewLimiter(rate.Limit(100), 200) // Per-tenant
            t.limiters[tenantID] = limiter
            t.mu.Unlock()
        }
        
        return limiter.Allow()
    }
    ```

    **Database**: Separate recipe stores per tenant

3. **Admin API**  
    **Use Case**: Runtime configuration updates without restart  
    **Endpoints**:
    - `POST /admin/config/rate-limit` - Update rate limits
    - `POST /admin/config/log-level` - Change log verbosity
    - `GET /admin/debug/pprof` - CPU/memory profiling
    - `POST /admin/cache/flush` - Clear recipe cache
    **Security**: Separate admin API key with IP allowlist

4. **Feature Flags**  
    **Rationale**: A/B testing, gradual rollouts, instant rollback  
    **Implementation**: LaunchDarkly or custom flag service  

    ```go
    import "github.com/launchdarkly/go-server-sdk/v7"
    
    func handleRecipe(w http.ResponseWriter, r *http.Request) {
        user := ldclient.NewUser(getUserID(r))
        
        // Check feature flag
        if client.BoolVariation("use-optimized-builder", user, false) {
            // Use new optimized recipe builder
            recipe = optimizedBuilder.Build(params)
        } else {
            // Fall back to stable builder
            recipe = stableBuilder.Build(params)
        }
    }
    ```

    **Reference**: [LaunchDarkly Go SDK](https://docs.launchdarkly.com/sdk/server-side/go)

## Production Deployment Patterns

### Pattern 1: Kubernetes with Horizontal Pod Autoscaler

**Use Case**: Auto-scale API servers based on request rate

**Deployment Manifest**:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: aicrd
  namespace: aicr
spec:
  replicas: 3  # Initial replicas
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
      serviceAccountName: aicrd
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        fsGroup: 1000
      containers:
      - name: api-server
        image: ghcr.io/nvidia/aicrd:latest  # Or use specific tag like v0.8.12
        ports:
        - name: http
          containerPort: 8080
          protocol: TCP
        - name: metrics
          containerPort: 9090
          protocol: TCP
        env:
        - name: PORT
          value: "8080"
        - name: AICR_LOG_LEVEL
          value: "info"
        # Criteria allowlists (optional - omit to allow all values)
        - name: AICR_ALLOWED_ACCELERATORS
          value: "h100,l40,a100"
        - name: AICR_ALLOWED_SERVICES
          value: "eks,gke,aks"
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
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
          timeoutSeconds: 3
          failureThreshold: 2
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
            - ALL
        volumeMounts:
        - name: recipes
          mountPath: /etc/aicr/recipes
          readOnly: true
        - name: tmp
          mountPath: /tmp
      volumes:
      - name: recipes
        configMap:
          name: aicr-recipes
      - name: tmp
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: aicrd
  namespace: aicr
spec:
  type: ClusterIP
  ports:
  - name: http
    port: 80
    targetPort: http
    protocol: TCP
  selector:
    app: aicrd
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: aicrd-hpa
  namespace: aicr
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: aicrd
  minReplicas: 3
  maxReplicas: 20
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70
  - type: Pods
    pods:
      metric:
        name: http_requests_per_second
      target:
        type: AverageValue
        averageValue: "100"
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 60
      policies:
      - type: Percent
        value: 100  # Double pods
        periodSeconds: 60
      - type: Pods
        value: 4  # Add 4 pods
        periodSeconds: 60
      selectPolicy: Max
    scaleDown:
      stabilizationWindowSeconds: 300  # 5 min cooldown
      policies:
      - type: Percent
        value: 50  # Remove 50% of pods
        periodSeconds: 60
```

**Ingress with TLS**:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: aicrd
  namespace: aicr
  annotations:
    cert-manager.io/cluster-issuer: "letsencrypt-prod"
    nginx.ingress.kubernetes.io/rate-limit: "100"
    nginx.ingress.kubernetes.io/limit-rps: "20"
    nginx.ingress.kubernetes.io/ssl-redirect: "true"
spec:
  ingressClassName: nginx
  tls:
  - hosts:
    - api.aicr.example.com
    secretName: aicr-api-tls
  rules:
  - host: api.aicr.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: aicrd
            port:
              name: http
```

### Pattern 2: Service Mesh with mTLS

**Use Case**: Zero-trust security with automatic mTLS encryption

**Istio VirtualService**:

```yaml
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: aicrd
  namespace: aicr
spec:
  hosts:
  - aicrd.aicr.svc.cluster.local
  - api.aicr.example.com
  gateways:
  - aicr-gateway
  http:
  - match:
    - uri:
        prefix: /v1/recipe
    route:
    - destination:
        host: aicrd
        port:
          number: 80
    timeout: 10s
    retries:
      attempts: 3
      perTryTimeout: 3s
      retryOn: 5xx,reset,connect-failure
    headers:
      response:
        add:
          X-Content-Type-Options: nosniff
          X-Frame-Options: DENY
          Strict-Transport-Security: max-age=31536000
---
apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: aicrd
  namespace: aicr
spec:
  host: aicrd
  trafficPolicy:
    tls:
      mode: ISTIO_MUTUAL  # mTLS between services
    connectionPool:
      tcp:
        maxConnections: 100
      http:
        http1MaxPendingRequests: 50
        http2MaxRequests: 100
        maxRequestsPerConnection: 2
    outlierDetection:
      consecutiveErrors: 5
      interval: 30s
      baseEjectionTime: 30s
      maxEjectionPercent: 50
---
apiVersion: security.istio.io/v1beta1
kind: PeerAuthentication
metadata:
  name: aicrd
  namespace: aicr
spec:
  selector:
    matchLabels:
      app: aicrd
  mtls:
    mode: STRICT  # Require mTLS
---
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: aicrd
  namespace: aicr
spec:
  selector:
    matchLabels:
      app: aicrd
  action: ALLOW
  rules:
  - from:
    - source:
        namespaces: ["aicr", "monitoring"]
    to:
    - operation:
        methods: ["GET", "POST"]
        paths: ["/v1/*", "/health", "/metrics"]
```

### Pattern 3: Load Balancer with Health Checks

**Use Case**: Bare-metal deployment with HAProxy

**HAProxy Configuration**:

```cfg
global
    log /dev/log local0
    maxconn 4096
    user haproxy
    group haproxy
    daemon

defaults
    log     global
    mode    http
    option  httplog
    option  dontlognull
    timeout connect 5s
    timeout client  30s
    timeout server  30s
    retries 3
    option  redispatch

frontend aicr_api_frontend
    bind *:443 ssl crt /etc/ssl/certs/aicr-api.pem
    bind *:80
    redirect scheme https if !{ ssl_fc }
    
    # Rate limiting
    stick-table type ip size 100k expire 30s store http_req_rate(10s)
    http-request track-sc0 src
    http-request deny deny_status 429 if { sc_http_req_rate(0) gt 100 }
    
    # Security headers
    http-response set-header Strict-Transport-Security "max-age=31536000"
    http-response set-header X-Content-Type-Options "nosniff"
    
    default_backend aicr_api_backend

backend aicr_api_backend
    balance roundrobin
    option httpchk GET /health
    http-check expect status 200
    
    server api1 10.0.1.10:8080 check inter 10s fall 3 rise 2 maxconn 100
    server api2 10.0.1.11:8080 check inter 10s fall 3 rise 2 maxconn 100
    server api3 10.0.1.12:8080 check inter 10s fall 3 rise 2 maxconn 100
```

### Pattern 4: Blue-Green Deployment

**Use Case**: Zero-downtime updates with instant rollback

**Kubernetes Service Switching**:

```bash
#!/bin/bash
# Blue-green deployment script

set -euo pipefail

NAMESPACE=aicr
APP=aicrd
NEW_VERSION=$1

# Deploy green version
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${APP}-green
  namespace: ${NAMESPACE}
spec:
  replicas: 3
  selector:
    matchLabels:
      app: ${APP}
      version: green
  template:
    metadata:
      labels:
        app: ${APP}
        version: green
    spec:
      containers:
      - name: api-server
        image: ghcr.io/nvidia/${APP}:${NEW_VERSION}
        # ... same spec as blue ...
EOF

# Wait for green to be ready
kubectl rollout status deployment/${APP}-green -n ${NAMESPACE}

# Run smoke tests
GREEN_IP=$(kubectl get svc ${APP}-green -n ${NAMESPACE} -o jsonpath='{.spec.clusterIP}')
curl -f http://${GREEN_IP}/health || (echo "Health check failed" && exit 1)
curl -f "http://${GREEN_IP}/v1/recipe?os=ubuntu&gpu=h100" || (echo "Recipe test failed" && exit 1)

# Switch service to green
kubectl patch service ${APP} -n ${NAMESPACE} -p '{"spec":{"selector":{"version":"green"}}}'

echo "Switched to green (${NEW_VERSION})"
echo "Monitor for 10 minutes, then delete blue deployment"
echo "Rollback: kubectl patch service ${APP} -n ${NAMESPACE} -p '{\"spec\":{\"selector\":{\"version\":\"blue\"}}}'"

# Optional: Auto-delete blue after monitoring period
# sleep 600
# kubectl delete deployment ${APP}-blue -n ${NAMESPACE}
```

## Reliability Patterns

### Circuit Breaker

**Use Case**: Prevent cascading failures when recipe store is slow

**Implementation**:

```go
import "github.com/sony/gobreaker"

var (
    recipeStoreBreaker *gobreaker.CircuitBreaker
)

func init() {
    settings := gobreaker.Settings{
        Name:        "RecipeStore",
        MaxRequests: 3,  // Half-open state allows 3 requests
        Interval:    60 * time.Second,  // Reset counts every 60s
        Timeout:     30 * time.Second,  // Stay open for 30s
        ReadyToTrip: func(counts gobreaker.Counts) bool {
            failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
            return counts.Requests >= 10 && failureRatio >= 0.6
        },
        OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
            log.Info("Circuit breaker state changed",
                "name", name,
                "from", from,
                "to", to,
            )
        },
    }
    
    recipeStoreBreaker = gobreaker.NewCircuitBreaker(settings)
}

func handleRecipe(w http.ResponseWriter, r *http.Request) {
    result, err := recipeStoreBreaker.Execute(func() (interface{}, error) {
        return buildRecipe(r.Context(), params)
    })
    
    if err != nil {
        if errors.Is(err, gobreaker.ErrOpenState) {
            http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
            return
        }
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    recipe := result.(*recipe.Recipe)
    json.NewEncoder(w).Encode(recipe)
}
```

**Reference**: [gobreaker](https://github.com/sony/gobreaker)

### Bulkhead Pattern

**Use Case**: Isolate resources for different endpoints

**Implementation**:

```go
import "golang.org/x/sync/semaphore"

var (
    // Separate semaphores for different endpoints
    recipeSem   = semaphore.NewWeighted(100)  // 100 concurrent recipe requests
    snapshotSem = semaphore.NewWeighted(10)   // 10 concurrent snapshot requests
)

func handleRecipeWithBulkhead(w http.ResponseWriter, r *http.Request) {
    // Acquire from recipe bulkhead
    if !recipeSem.TryAcquire(1) {
        http.Error(w, "Too many requests", http.StatusTooManyRequests)
        return
    }
    defer recipeSem.Release(1)
    
    // Process request
    handleRecipe(w, r)
}

func handleSnapshotWithBulkhead(w http.ResponseWriter, r *http.Request) {
    // Acquire from snapshot bulkhead (more expensive operation)
    if !snapshotSem.TryAcquire(1) {
        http.Error(w, "Too many requests", http.StatusTooManyRequests)
        return
    }
    defer snapshotSem.Release(1)
    
    handleSnapshot(w, r)
}
```

**Benefit**: Recipe slowness doesn't affect snapshot endpoint

### Retry with Exponential Backoff

**Use Case**: Resilient calls to external APIs (recipe store, etc.)

**Implementation**:

```go
import "github.com/cenkalti/backoff/v4"

func fetchRecipeWithRetry(ctx context.Context, key string) (*recipe.Recipe, error) {
    var r *recipe.Recipe
    
    operation := func() error {
        var err error
        r, err = recipeStore.Get(ctx, key)
        
        // Don't retry on 404
        if errors.Is(err, ErrNotFound) {
            return backoff.Permanent(err)
        }
        
        return err
    }
    
    // Exponential backoff: 100ms, 200ms, 400ms, 800ms, 1.6s, 3.2s
    bo := backoff.NewExponentialBackOff()
    bo.InitialInterval = 100 * time.Millisecond
    bo.MaxInterval = 5 * time.Second
    bo.MaxElapsedTime = 30 * time.Second
    
    err := backoff.Retry(operation, backoff.WithContext(bo, ctx))
    return r, err
}
```

**Reference**: [backoff](https://github.com/cenkalti/backoff)

### Graceful Degradation

**Use Case**: Serve stale/cached data when primary source fails

**Implementation**:

```go
var (
    recipeCacheTTL = 1 * time.Hour
    recipeCache    = sync.Map{}
)

type cachedRecipe struct {
    recipe    *recipe.Recipe
    timestamp time.Time
}

func handleRecipeWithFallback(w http.ResponseWriter, r *http.Request) {
    key := buildCacheKey(r)
    
    // Try primary source
    recipe, err := buildRecipe(r.Context(), params)
    if err == nil {
        // Cache successful response
        recipeCache.Store(key, cachedRecipe{
            recipe:    recipe,
            timestamp: time.Now(),
        })
        
        json.NewEncoder(w).Encode(recipe)
        return
    }
    
    // Primary failed - try cache
    if cached, ok := recipeCache.Load(key); ok {
        cr := cached.(cachedRecipe)
        age := time.Since(cr.timestamp)
        
        log.Warn("Serving stale recipe",
            "key", key,
            "age", age,
            "error", err,
        )
        
        w.Header().Set("X-Cache", "stale")
        w.Header().Set("X-Cache-Age", age.String())
        json.NewEncoder(w).Encode(cr.recipe)
        return
    }
    
    // No cache available
    http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
}
```

## Performance Optimization

### Connection Pooling

**HTTP Client with Keep-Alive**:

```go
var httpClient = &http.Client{
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 10,
        IdleConnTimeout:     90 * time.Second,
        DisableCompression:  false,
        ForceAttemptHTTP2:   true,
    },
    Timeout: 10 * time.Second,
}

// Reuse client for all outbound requests
resp, err := httpClient.Get("https://recipe-store.example.com/recipes")
```

### Response Caching

**In-Memory Cache with TTL**:

```go
import "github.com/patrickmn/go-cache"

var (
    responseCache = cache.New(5*time.Minute, 10*time.Minute)
)

func handleRecipeWithCache(w http.ResponseWriter, r *http.Request) {
    key := buildCacheKey(r)
    
    // Check cache
    if cached, found := responseCache.Get(key); found {
        w.Header().Set("X-Cache", "hit")
        w.Header().Set("Content-Type", "application/json")
        w.Write(cached.([]byte))
        return
    }
    
    // Cache miss - build recipe
    recipe, err := buildRecipe(r.Context(), params)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    // Serialize and cache
    data, err := json.Marshal(recipe)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    responseCache.Set(key, data, cache.DefaultExpiration)
    
    w.Header().Set("X-Cache", "miss")
    w.Header().Set("Content-Type", "application/json")
    w.Write(data)
}
```

### Request Coalescing

**Deduplicate Concurrent Identical Requests**:

```go
import "golang.org/x/sync/singleflight"

var requestGroup singleflight.Group

func handleRecipeWithCoalescing(w http.ResponseWriter, r *http.Request) {
    key := buildCacheKey(r)
    
    // Deduplicate requests with same key
    result, err, shared := requestGroup.Do(key, func() (interface{}, error) {
        return buildRecipe(r.Context(), params)
    })
    
    if shared {
        w.Header().Set("X-Request-Coalesced", "true")
    }
    
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    json.NewEncoder(w).Encode(result)
}
```

**Benefit**: 10 concurrent identical requests = 1 recipe build

### Memory Profiling

```bash
# Enable pprof endpoint
import _ "net/http/pprof"

go func() {
    log.Println(http.ListenAndServe("localhost:6060", nil))
}()

# Capture heap profile
curl http://localhost:6060/debug/pprof/heap > heap.prof

# Analyze
go tool pprof heap.prof
(pprof) top10
(pprof) list buildRecipe

# Check for memory leaks
# Compare two profiles taken 5 minutes apart
go tool pprof -base heap1.prof heap2.prof
(pprof) top10  # Shows allocations between profiles
```

## Security Hardening

### Rate Limiting Per IP

```go
import "golang.org/x/time/rate"

type ipRateLimiter struct {
    limiters map[string]*rate.Limiter
    mu       sync.RWMutex
    rate     rate.Limit
    burst    int
}

func newIPRateLimiter(r rate.Limit, b int) *ipRateLimiter {
    return &ipRateLimiter{
        limiters: make(map[string]*rate.Limiter),
        rate:     r,
        burst:    b,
    }
}

func (i *ipRateLimiter) getLimiter(ip string) *rate.Limiter {
    i.mu.RLock()
    limiter, exists := i.limiters[ip]
    i.mu.RUnlock()
    
    if !exists {
        i.mu.Lock()
        limiter = rate.NewLimiter(i.rate, i.burst)
        i.limiters[ip] = limiter
        
        // Illustrative cleanup: clears the entire map past a threshold.
        // Production implementations should use LRU or TTL eviction
        // (e.g., github.com/hashicorp/golang-lru) so in-flight limiters
        // are preserved and a request-burst cliff is avoided.
        if len(i.limiters) > 10000 {
            i.limiters = make(map[string]*rate.Limiter)
        }
        i.mu.Unlock()
    }
    
    return limiter
}

func (i *ipRateLimiter) middleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ip := getClientIP(r)
        limiter := i.getLimiter(ip)
        
        if !limiter.Allow() {
            http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
            return
        }
        
        next.ServeHTTP(w, r)
    })
}

func getClientIP(r *http.Request) string {
    // Check X-Forwarded-For header (behind proxy)
    xff := r.Header.Get("X-Forwarded-For")
    if xff != "" {
        ips := strings.Split(xff, ",")
        return strings.TrimSpace(ips[0])
    }
    
    // Fall back to RemoteAddr
    ip, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil {
        // RemoteAddr might not include a port (e.g., some test setups);
        // returning RemoteAddr keeps a unique key per peer and avoids
        // collapsing every unparseable address into one rate-limit bucket.
        return r.RemoteAddr
    }
    return ip
}
```

### Input Validation

```go
import "github.com/go-playground/validator/v10"

var validate = validator.New()

type RecipeRequest struct {
    OS       string `validate:"required,oneof=ubuntu rhel cos"`
    OSVersion string `validate:"omitempty,semver"`
    GPU      string `validate:"required,oneof=h100 gb200 b200 a100 l40 rtx-pro-6000"`
    Service  string `validate:"omitempty,oneof=eks gke aks oke kind lke"`
}

func handleRecipe(w http.ResponseWriter, r *http.Request) {
    req := RecipeRequest{
        OS:       r.URL.Query().Get("os"),
        OSVersion: r.URL.Query().Get("osv"),
        GPU:      r.URL.Query().Get("gpu"),
        Service:  r.URL.Query().Get("service"),
    }
    
    if err := validate.Struct(req); err != nil {
        validationErrors := err.(validator.ValidationErrors)
        http.Error(w, validationErrors.Error(), http.StatusBadRequest)
        return
    }
    
    // Proceed with validated input
}
```

### Security Headers Middleware

```go
func securityHeadersMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // HSTS
        w.Header().Set("Strict-Transport-Security",
            "max-age=31536000; includeSubDomains; preload")
        
        // Prevent MIME sniffing
        w.Header().Set("X-Content-Type-Options", "nosniff")
        
        // Prevent clickjacking
        w.Header().Set("X-Frame-Options", "DENY")
        
        // XSS protection
        w.Header().Set("X-XSS-Protection", "1; mode=block")
        
        // CSP
        w.Header().Set("Content-Security-Policy",
            "default-src 'none'; script-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self';")
        
        // Referrer policy
        w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
        
        next.ServeHTTP(w, r)
    })
}
```

## Observability

### Custom Metrics

```go
import "github.com/prometheus/client_golang/prometheus"

var (
    recipeBuildDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "aicr_recipe_build_duration_seconds",
            Help:    "Time to build recipe",
            Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms to 4s
        },
        []string{"os", "gpu", "service"},
    )
    
    recipeCacheHits = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "aicr_recipe_cache_hits_total",
            Help: "Number of recipe cache hits",
        },
        []string{"cache_type"},
    )
    
    activeConnections = prometheus.NewGauge(
        prometheus.GaugeOpts{
            Name: "aicr_active_connections",
            Help: "Number of active HTTP connections",
        },
    )
)

func init() {
    prometheus.MustRegister(
        recipeBuildDuration,
        recipeCacheHits,
        activeConnections,
    )
}

func handleRecipe(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    defer func() {
        duration := time.Since(start).Seconds()
        recipeBuildDuration.WithLabelValues(
            params.OS,
            params.GPU,
            params.Service,
        ).Observe(duration)
    }()
    
    // Check cache
    if cached, found := cache.Get(key); found {
        recipeCacheHits.WithLabelValues("memory").Inc()
        // ...
    }
    
    // Build recipe
    // ...
}
```

### Structured Logging with Context

```go
import "log/slog"

func handleRecipe(w http.ResponseWriter, r *http.Request) {
    // Create logger with request context
    logger := slog.With(
        "request_id", r.Header.Get("X-Request-ID"),
        "remote_addr", r.RemoteAddr,
        "user_agent", r.UserAgent(),
    )
    
    logger.Info("Handling recipe request",
        "os", params.OS,
        "gpu", params.GPU,
    )
    
    recipe, err := buildRecipe(r.Context(), params)
    if err != nil {
        logger.Error("Failed to build recipe",
            "error", err,
            "params", params,
        )
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    logger.Info("Recipe built successfully",
        "measurement_count", len(recipe.Measurements),
        "duration_ms", time.Since(start).Milliseconds(),
    )
    
    json.NewEncoder(w).Encode(recipe)
}
```

### Distributed Tracing

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "go.opentelemetry.io/otel/trace"
)

func handleRecipe(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    tracer := otel.Tracer("aicrd")
    
    ctx, span := tracer.Start(ctx, "handleRecipe",
        trace.WithAttributes(
            attribute.String("os", params.OS),
            attribute.String("gpu", params.GPU),
        ),
    )
    defer span.End()
    
    // Propagate context to child operations
    recipe, err := buildRecipeWithTrace(ctx, params)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    span.SetAttributes(
        attribute.Int("measurement_count", len(recipe.Measurements)),
    )
    
    json.NewEncoder(w).Encode(recipe)
}

func buildRecipeWithTrace(ctx context.Context, params Params) (*recipe.Recipe, error) {
    tracer := otel.Tracer("aicrd")
    ctx, span := tracer.Start(ctx, "buildRecipe")
    defer span.End()
    
    // Build recipe with traced context
    return builder.Build(ctx, params)
}
```

# Copilot Instructions for NVIDIA Eidos

This file contains extended technical documentation for GitHub Copilot. For core rules and patterns, see `.claude/CLAUDE.md`.

## Critical Rules (Always Apply)

**Code Quality (Non-Negotiable):**
- Tests must pass with race detector (`make test`)
- Never disable tests to make CI green (including "temporary" skips)
- Use structured errors from `pkg/errors` with error codes (never `fmt.Errorf`)
- Context with timeouts for all I/O operations (collectors: 10s, handlers: 30s)
- Check `ctx.Done()` in loops and long-running operations
- Stop after 3 failed attempts at same fix → reassess approach

**Git Configuration:**
- Commit to `main` branch (not `master`)
- Do NOT add `Co-Authored-By` lines (organization policy)
- Use DCO sign-off: `git commit -s -m "message"`

**Decision Framework:**
Choose solutions based on: testability, readability, consistency, simplicity, reversibility

---

## Project Context

NVIDIA Eidos provides validated GPU-accelerated Kubernetes configurations through a four-stage workflow:

1. **Snapshot** → Capture system state (OS, kernel, K8s, GPU)
2. **Recipe** → Generate optimized config from captured data or query parameters
3. **Validate** → Check recipe constraints against actual cluster measurements
4. **Bundle** → Create deployment artifacts (Helm values, manifests, scripts)

**Core Components:**
- **CLI (`eidos`)**: All four stages (snapshot/recipe/validate/bundle)
- **API Server (`eidosd`)**: Recipe generation and bundle creation via REST API
- **Agent**: Kubernetes Job for automated cluster snapshots → ConfigMaps
- **Bundlers**: Plugin-based artifact generators (GPU Operator, Network Operator, Cert-Manager, NVSentinel, Skyhook, DRA Driver)
- **Deployers**: GitOps integration providers (helm, argocd) with deployment ordering

**Tech Stack:** Go 1.25, Kubernetes 1.33+, golangci-lint v2.6, Container images via Ko

**Package Architecture (Critical Principle):**
- **User Interaction Packages** (`pkg/cli`, `pkg/api`): Focus solely on capturing user intent, validating input, and formatting output. No business logic.
- **Functional Packages** (`pkg/oci`, `pkg/bundler`, `pkg/recipe`, `pkg/collector`): Self-contained, reusable business logic. Should be usable independently without CLI/API.
- **Example**: OCI packaging logic lives in `pkg/oci` (not `pkg/cli`), so both CLI and API can use it. Deployers return structured `DeploymentInfo` so the CLI just formats output.

---

## Common Tasks (Start Here)

### I Need To: Add GPU Support for New Hardware

1. **Add collector** in `pkg/collector/gpu/`:
   - Implement `Collector` interface
   - Add factory method in `factory.go`
   - Write table-driven tests with mocks

2. **Update recipe data** in `pkg/recipe/data/`:
   - Add base measurements in `overlays/base.yaml`
   - Create overlay files in `overlays/` with criteria matching

3. **Test workflow**:
   ```bash
   eidos snapshot --output snapshot.yaml
   eidos recipe --snapshot snapshot.yaml --intent training --platform kubeflow
   ```

→ See Extended Reference: Adding a New Collector for full example

### I Need To: Add Support for New Component/Operator

**For Helm Components:**

1. **Add to component registry** (`pkg/recipe/data/registry.yaml`):
   ```yaml
   - name: my-operator
     displayName: My Operator
     valueOverrideKeys: [myoperator]
     helm:
       defaultRepository: https://charts.example.com
       defaultChart: example/my-operator
   ```

2. **Create values file** (`pkg/recipe/data/components/my-operator/values.yaml`):
   - Define Helm chart values
   - Keep configuration minimal and reusable

3. **Reference in recipe** (`pkg/recipe/data/overlays/*.yaml`):
   ```yaml
   componentRefs:
     - name: my-operator
       type: Helm
       version: v1.0.0
       valuesFile: components/my-operator/values.yaml
   ```

**For Kustomize Components:**

1. **Add to component registry** (`pkg/recipe/data/registry.yaml`):
   ```yaml
   - name: my-kustomize-app
     displayName: My Kustomize App
     valueOverrideKeys: [mykustomize]
     kustomize:
       defaultSource: https://github.com/example/my-app
       defaultPath: deploy/production
       defaultTag: v1.0.0
   ```

2. **Reference in recipe** (`pkg/recipe/data/overlays/*.yaml`):
   ```yaml
   componentRefs:
     - name: my-kustomize-app
       type: Kustomize
       tag: v1.0.0
   ```

**Note:** A component must have either `helm` OR `kustomize` configuration, not both.

4. **Run tests**:
   ```bash
   go test -v ./pkg/recipe/... -run TestComponentRegistry
   make test
   ```

→ See [Bundler Development Guide](docs/contributor/component.md) for full details

### I Need To: Add New API Endpoint

1. Create handler in `pkg/api/`
2. Register route in `pkg/api/server.go`
3. Add middleware (metrics → version → requestID → panic → rateLimit → logging)
4. Update API spec in `api/eidos/v1/server.yaml`
5. Write integration tests

→ See Extended Reference: Adding a New API Endpoint for detailed steps

### I Need To: Fix Failing Tests

1. **Check error messages** → Use proper assertions:
   ```go
   if err != nil {
       t.Fatalf("unexpected error: %v", err)  // Stop test immediately
   }
   ```

2. **Race conditions** → Run `go test -race ./...`
3. **Linting issues** → `make lint` then fix reported problems
4. **Context** → Ensure collectors/handlers respect `ctx.Done()`

---

## Go Architecture Patterns

**1. Functional Options (Configuration)**
```go
builder := recipe.NewBuilder(
    recipe.WithVersion(version),
)
server := server.New(
    server.WithName("eidosd"),
    server.WithVersion(version),
)
```

**2. Factory Pattern (Collectors)**
```go
factory := collector.NewDefaultFactory(
    collector.WithSystemDServices([]string{"containerd.service"}),
)
gpuCollector := factory.CreateGPUCollector()
```

**3. Builder Pattern (Measurements)**
```go
measurement.NewMeasurement(measurement.TypeK8s).
    WithSubtype(subtype).
    Build()
```

**4. Singleton Pattern (K8s Client)**
```go
import "github.com/NVIDIA/eidos/pkg/k8s/client"

clientset, config, err := client.GetKubeClient()  // Uses sync.Once
```

---

## Testing & Quality

### Essential Commands

```bash
make qualify       # Full check: test + lint + e2e + scan (run before PR)
make test          # Unit tests with race detector
make lint          # golangci-lint + yamllint
make scan          # Grype vulnerability scan

# Run single test
go test -v ./pkg/recipe/... -run TestSpecificFunction

# Run with race detector for specific package
go test -race -v ./pkg/collector/...
```

### Testing Requirements

- **Coverage**: Aim for >70% meaningful coverage
- **Race Detector**: Always enabled (`make test` runs with `-race`)
- **Table-Driven Tests**: Required for multiple test cases
- **Mocks**: Use fake clients for external dependencies (K8s client-go fakes)
- **Error Cases**: Test error conditions and edge cases explicitly

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| K8s Connection | Check `~/.kube/config` or `KUBECONFIG` env |
| GPU Detection | Requires `nvidia-smi` in PATH |
| Linter Errors | Use `errors.Is()` for error comparison, add `return` after `t.Fatal()` |
| Race Conditions | Run tests with `-race` flag to detect |
| Build Failures | Run `make tidy` to fix Go module issues |

### Debugging Commands

```bash
# Enable debug logging
eidos --debug snapshot

# Run server with debug logs
LOG_LEVEL=debug make server

# Check race conditions
go test -race -run TestSpecificTest ./pkg/...

# Profile performance
go test -cpuprofile cpu.prof -memprofile mem.prof
go tool pprof cpu.prof
```

---

## Documentation Development

When writing documentation, act as a senior open-source documentation editor with CNCF/Linux Foundation experience.

**Standards:**

1. **Accuracy & Scope**
   - Don't invent features, guarantees, timelines, or roadmap commitments
   - Clearly distinguish current behavior from future intent
   - Remove speculative or marketing language

2. **Tone & Style**
   - Use neutral, factual, engineering-oriented language
   - Avoid hype ("best", "powerful", "game-changing")
   - Prefer short, declarative sentences

3. **Structure & Readability**
   - Organize with clear sections and logical flow
   - Use headings that answer user questions
   - Convert dense paragraphs into lists or tables

4. **Audience Awareness**
   - Assume engineers but not necessarily project experts
   - Define acronyms on first use
   - Clearly state prerequisites and assumptions

5. **Operational Clarity**
   - Document configuration boundaries and failure modes
   - Prefer "what happens" over "what should happen"

---

## Quick Reference

### CLI Workflow Examples

```bash
# Capture system state
eidos snapshot --output snapshot.yaml

# Generate recipe from snapshot
eidos recipe --snapshot snapshot.yaml --intent training --platform kubeflow

# Generate recipe from query parameters
eidos recipe --service eks --accelerator h100 --intent training --os ubuntu

# Create deployment bundle
eidos bundle --recipe recipe.yaml --output ./bundles

# Override bundle values at generation time
eidos bundle -r recipe.yaml \
  --set gpuoperator:gds.enabled=true \
  --set gpuoperator:driver.version=570.86.16 \
  -o ./bundles

# Node scheduling with selectors and tolerations
eidos bundle -r recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --system-node-toleration dedicated=system:NoSchedule \
  --accelerated-node-selector nvidia.com/gpu.present=true \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  -o ./bundles

# GitOps deployment with ArgoCD (sync-wave ordering)
eidos bundle -r recipe.yaml \
  --deployer argocd \
  --repo https://github.com/my-org/my-gitops-repo.git \
  -o ./bundles
```

### Integration Points

- **Kubernetes**: Singleton client via `pkg/k8s/client.GetKubeClient()`
- **NVIDIA Operators**: GPU Operator, Network Operator, NIM Operator, Nsight Operator
- **Container Images**: ghcr.io/nvidia/eidos, ghcr.io/nvidia/eidosd
- **Observability**: Prometheus metrics at `/metrics`, structured JSON logs to stderr

### Key Links

- **[CLAUDE.md](../.claude/CLAUDE.md)** – Core rules, patterns, and commands
- **[Contributing Guide](../CONTRIBUTING.md)** – Design principles, development setup, PR process
- **[Development Guide](../DEVELOPMENT.md)** – Local development, Make targets, Tilt/Kind setup
- **[Release Process](../RELEASING.md)** – Maintainer guide for releases, verification, hotfixes
- **[Architecture Overview](../docs/contributor/README.md)** – System design
- **[Bundler Development](../docs/contributor/component.md)** – Create new bundlers
- **[API Reference](../docs/user/api-reference.md)** – REST API endpoints
- **[GitHub Actions README](actions/README.md)** – CI/CD architecture
- **[API Specification](../api/eidos/v1/server.yaml)** – OpenAPI spec
- **[.versions.yaml](../.versions.yaml)** – Tool versions (single source of truth)

---

## Extended Reference

### Adding a New Collector

If adding a new system collector (like the OS release collector):

**1. Create the collector in `pkg/collector/os/`:**
```go
// pkg/collector/os/release.go
package os

import (
    "bufio"
    "context"
    "os"
    "strings"

    "github.com/NVIDIA/eidos/pkg/errors"
)

// collectRelease reads and parses /etc/os-release
func (c *Collector) collectRelease(ctx context.Context) (*measurement.Subtype, error) {
    data := make(map[string]measurement.Reading)

    file, err := os.Open("/etc/os-release")
    if err != nil {
        return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to open /etc/os-release", err)
    }
    defer file.Close()

    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        // Check context cancellation
        select {
        case <-ctx.Done():
            return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", ctx.Err())
        default:
        }

        line := scanner.Text()
        if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
            continue
        }

        parts := strings.SplitN(line, "=", 2)
        if len(parts) != 2 {
            continue
        }

        key := parts[0]
        value := strings.Trim(parts[1], `"`)
        data[key] = measurement.Reading{Value: value}
    }

    if err := scanner.Err(); err != nil {
        return nil, errors.Wrap(errors.ErrCodeInternal, "error reading /etc/os-release", err)
    }

    return &measurement.Subtype{
        Name: "release",
        Data: data,
    }, nil
}
```

**2. Update the main collector:**
```go
// pkg/collector/os/os.go
func (c *Collector) Collect(ctx context.Context) ([]*measurement.Measurement, error) {
    // Collect all OS subtypes in parallel
    grubSubtype, _ := c.collectGrub(ctx)
    sysctlSubtype, _ := c.collectSysctl(ctx)
    kmodSubtype, _ := c.collectKmod(ctx)
    releaseSubtype, _ := c.collectRelease(ctx) // New subtype

    return []*measurement.Measurement{{
        Type: measurement.TypeOS,
        Subtypes: []*measurement.Subtype{
            grubSubtype,
            sysctlSubtype,
            kmodSubtype,
            releaseSubtype, // Add to list
        },
    }}, nil
}
```

**3. Add tests:**
```go
// pkg/collector/os/release_test.go
func TestCollectRelease(t *testing.T) {
    tests := []struct {
        name    string
        wantErr bool
    }{
        {"valid os-release", false},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            c := NewCollector()
            ctx := context.Background()

            subtype, err := c.collectRelease(ctx)
            if (err != nil) != tt.wantErr {
                t.Fatalf("collectRelease() error = %v, wantErr %v", err, tt.wantErr)
            }

            if !tt.wantErr {
                // Verify expected fields exist
                expectedFields := []string{"ID", "VERSION_ID", "PRETTY_NAME"}
                for _, field := range expectedFields {
                    if _, exists := subtype.Data[field]; !exists {
                        t.Errorf("expected field %q not found", field)
                    }
                }

                if subtype.Name != "release" {
                    t.Errorf("expected subtype name 'release', got %q", subtype.Name)
                }
            }
        })
    }
}
```

### Adding a New Bundler

The bundler framework uses **BaseBundler** - a helper that reduces boilerplate by ~75%. Embed `BaseBundler` and override only what you need.

**1. Create bundler package in `pkg/bundler/<bundler-name>/`:**
```go
package networkoperator

import (
    "context"
    "embed"
    "path/filepath"

    "github.com/NVIDIA/eidos/pkg/bundler"
    "github.com/NVIDIA/eidos/pkg/bundler/result"
    "github.com/NVIDIA/eidos/pkg/errors"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

const (
    bundlerType = bundler.BundleType("network-operator")
    Name        = "network-operator"  // Use constant for component name
)

func init() {
    // Self-register using MustRegister (panics on duplicates)
    bundler.MustRegister(bundlerType, NewBundler())
}

// Bundler generates Network Operator deployment bundles.
type Bundler struct {
    *bundler.BaseBundler  // Embed helper for common functionality
}

// NewBundler creates a new Network Operator bundler instance.
func NewBundler() *Bundler {
    return &Bundler{
        BaseBundler: bundler.NewBaseBundler(bundlerType, templatesFS),
    }
}

// Make generates the bundle from RecipeResult.
func (b *Bundler) Make(ctx context.Context, input *result.RecipeResult,
    outputDir string) (*bundler.Result, error) {

    // 1. Get component reference from RecipeResult
    component := input.GetComponentRef(Name)
    if component == nil {
        return nil, errors.New(errors.ErrCodeNotFound, Name + " component not found in recipe result")
    }

    // 2. Get values map (with overrides already applied)
    values := input.GetValuesForComponent(Name)

    // 3. Create bundle directory structure
    if err := b.CreateBundleDir(outputDir); err != nil {
        return nil, err
    }

    // 4. Generate bundle metadata
    metadata := generateBundleMetadata(component, values)

    // 5. Generate files from templates
    filePath := filepath.Join(outputDir, "values.yaml")
    if err := b.GenerateFileFromTemplate(ctx, GetTemplate, "values.yaml",
        filePath, values, 0644); err != nil {
        return nil, err
    }

    var generatedFiles []string
    // ... collect file paths

    // 6. Generate checksums and return result
    return b.GenerateResult(outputDir, generatedFiles)
}
```

**2. Templates directory (only for custom Kubernetes manifests):**

Most bundlers don't need a templates directory - they only generate values.yaml and checksums.txt. Templates are only needed for custom manifests:

```
pkg/component/gpuoperator/templates/
├── dcgm-exporter.yaml.tmpl        # Custom ConfigMap manifest
└── kernel-module-params.yaml.tmpl # Custom ConfigMap manifest
```

**3. Write tests with TestHarness:**
```go
func TestBundler_Make(t *testing.T) {
    harness := internal.NewTestHarness(t, NewBundler())

    tests := []struct {
        name    string
        input   *result.RecipeResult
        wantErr bool
        verify  func(t *testing.T, outputDir string)
    }{
        {
            name:    "valid component reference",
            input:   createTestRecipeResult(),
            wantErr: false,
            verify: func(t *testing.T, outputDir string) {
                harness.AssertFileContains(outputDir, "values.yaml",
                    "version:", "namespace:")
            },
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := harness.RunTest(tt.input, tt.wantErr)
            if !tt.wantErr && tt.verify != nil {
                tt.verify(t, result.OutputDir)
            }
        })
    }
}
```

**Key Components:**
- **BaseBundler** provides: `CreateBundleDir`, `WriteFile`, `GenerateFileFromTemplate`, `GenerateResult`, `Validate`
- **RecipeResult methods**: `GetComponentRef(name)`, `GetValuesForComponent(name)`
- **TestHarness**: `NewTestHarness`, `RunTest`, `AssertFileContains`

**Best Practices:**
- Embed `BaseBundler` instead of implementing from scratch
- Use `Name` constant for component name (not hardcoded strings)
- Get values via `input.GetValuesForComponent(Name)` (already has overrides applied)
- Self-register with `MustRegister()` for fail-fast behavior
- Keep bundlers stateless for thread-safe operation

### Adding a New API Endpoint

**1. Create handler in `pkg/api/`:**
```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
    defer cancel()

    // Parse query parameters
    query, err := recipe.NewQuery(
        recipe.WithOS(r.URL.Query().Get("os")),
        recipe.WithGPU(r.URL.Query().Get("gpu")),
    )
    if err != nil {
        serializer.WriteError(w, r, err, http.StatusBadRequest)
        return
    }

    // Generate recipe
    recipe, err := h.builder.Build(ctx, query)
    if err != nil {
        serializer.WriteError(w, r, err, http.StatusInternalServerError)
        return
    }

    // Serialize response
    serializer.WriteJSON(w, recipe, http.StatusOK)
}
```

**2. Register route in `pkg/api/server.go`:**
```go
mux.Handle("/v1/recipe", handler)
```

**3. Add middleware (order matters):**
```go
// Order: metrics → version → requestID → panic → rateLimit → logging → handler
handler = metricsMiddleware(handler)
handler = versionMiddleware(handler, version)
handler = requestIDMiddleware(handler)
handler = panicMiddleware(handler)
handler = rateLimitMiddleware(handler, limiter)
handler = loggingMiddleware(handler)
```

**4. Update API spec in `api/eidos/v1/server.yaml`:**
```yaml
paths:
  /v1/recipe:
    get:
      summary: Get optimized system configuration recipe
      parameters:
        - name: os
          in: query
          required: false
          schema:
            type: string
            enum: [ubuntu, cos, any]
```

**5. Write integration tests:**
```go
func TestRecipeHandler(t *testing.T) {
    server := httptest.NewServer(handler)
    defer server.Close()

    resp, err := http.Get(server.URL + "/v1/recipe?os=ubuntu&accelerator=h100&platform=kubeflow")
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }
}
```

---

## GitHub Actions & CI/CD Architecture

Eidos uses a **three-layer composite actions architecture** for reusability:

**Layer 1: Primitives** (Single-Purpose Building Blocks)
- `ghcr-login` – GHCR authentication
- `setup-build-tools` – Modular tool installer (ko, syft, crane, goreleaser)
- `security-scan` – Grype vulnerability scanning

**Layer 2: Composed Actions** (Combine Primitives)
- `go-ci` – Complete Go CI pipeline (setup → test → lint)
- `go-build-release` – Full build/release pipeline
- `attest-image-from-tag` – Resolve digest + generate attestations
- `cloud-run-deploy` – Demo API server deployment (example deployment to GCP)

**Layer 3: Workflows** (Orchestrate Actions)
- `on-push.yaml` – CI validation for PRs and main branch
- `on-tag.yaml` – Release, attestation, and deployment

**Key Workflows:**

**on-push.yaml** (CI validation - runs unit, integration, e2e in parallel):
```yaml
jobs:
  unit:
    steps:
      - uses: actions/checkout@v4
      - uses: ./.github/actions/go-ci
        with:
          go_version: '1.25'
          golangci_lint_version: 'v2.6.2'
          coverage_report: 'true'
      - uses: ./.github/actions/security-scan
  integration:
    steps:
      - uses: ./.github/actions/integration
  e2e:
    steps:
      - uses: ./.github/actions/e2e
```

**on-tag.yaml** (Release pipeline - tests → build → attest → deploy):
```yaml
jobs:
  unit:        # parallel
  integration: # parallel
  e2e:         # parallel
  build:
    needs: [unit, integration, e2e]
    steps:
      - uses: ./.github/actions/go-build-release
  attest:
    needs: [build]
    steps:
      - uses: ./.github/actions/attest-image-from-tag
        with:
          image_name: 'ghcr.io/nvidia/eidos'
          tag: ${{ github.ref_name }}
  deploy:  # Demo deployment (example, not production)
    needs: [attest]
    steps:
      - uses: ./.github/actions/cloud-run-deploy
        with:
          source_image: 'ghcr.io/nvidia/eidosd:${{ github.ref_name }}'
          target_registry: 'us-docker.pkg.dev/eidosx/demo'
```

**Supply Chain Security:**
- **SLSA Build Level 3**: GitHub OIDC attestations
- **SBOMs**: SPDX format via Syft (containers) and GoReleaser (binaries)
- **Signing**: Cosign keyless signing (Fulcio + Rekor)
- **Verification**: `gh attestation verify oci://ghcr.io/nvidia/eidos:${TAG}`

For detailed GitHub Actions architecture, see [actions/README.md](actions/README.md)

---

## Workflow Patterns

### Complete End-to-End: Snapshot → Recipe → Bundle

```bash
# 1. Capture system configuration
eidos snapshot --output snapshot.yaml

# 2. Generate optimized recipe for training workloads
eidos recipe \
  --snapshot snapshot.yaml \
  --intent training \
  --platform kubeflow \
  --format yaml \
  --output recipe.yaml

# 3. Create deployment bundle
eidos bundle \
  --recipe recipe.yaml \
  --output ./bundles

# 4. Deploy to cluster
cd bundles
chmod +x deploy.sh && ./deploy.sh
```

### ConfigMap-based Workflow (for Kubernetes Jobs)

```bash
# 1. Capture snapshot directly to ConfigMap
eidos snapshot -o cm://gpu-operator/eidos-snapshot

# 2. Generate recipe from ConfigMap snapshot
eidos recipe -s cm://gpu-operator/eidos-snapshot \
  --intent training \
  --platform kubeflow \
  -o cm://gpu-operator/eidos-recipe

# 3. Create bundle from ConfigMap recipe
eidos bundle -r cm://gpu-operator/eidos-recipe \
  -o ./bundles

# 4. Verify ConfigMap data
kubectl get configmap eidos-snapshot -n gpu-operator -o yaml
kubectl get configmap eidos-recipe -n gpu-operator -o yaml
```

### E2E Testing

```bash
# Run CLI integration tests (no cluster needed)
make e2e

# Run a single chainsaw test
EIDOS_BIN=$(pwd)/dist/eidos \
  chainsaw test --no-cluster --test-dir tests/chainsaw/cli/recipe-generation

# Run cluster-based E2E tests (requires Kind cluster)
make e2e-tilt
```

### Agent Deployment Pattern

```bash
# Deploy agent for automated snapshots
kubectl apply -f deployments/eidos-agent/1-deps.yaml
kubectl apply -f deployments/eidos-agent/2-job.yaml

# Check logs
kubectl logs -n gpu-operator job/eidos

# Get snapshot from ConfigMap
kubectl get configmap eidos-snapshot -n gpu-operator \
  -o jsonpath='{.data.snapshot\.yaml}' > snapshot.yaml
```

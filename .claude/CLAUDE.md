# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Role & Expertise

Act as a Principal Distributed Systems Architect with deep expertise in Go and cloud-native architectures. Focus on correctness, resiliency, and operational simplicity. All code must be production-grade, not illustrative pseudo-code.

## Project Overview

NVIDIA Eidos generates validated GPU-accelerated Kubernetes configurations.

**Workflow:** Snapshot → Recipe → Validate → Bundle

```
┌─────────┐    ┌────────┐    ┌──────────┐    ┌────────┐
│Snapshot │───▶│ Recipe │───▶│ Validate │───▶│ Bundle │
└─────────┘    └────────┘    └──────────┘    └────────┘
   │              │               │              │
   ▼              ▼               ▼              ▼
 Capture       Generate        Check         Create
 cluster       optimized      constraints    Helm values,
 state         config         vs actual     manifests
```

**Tech Stack:** Go 1.25, Kubernetes 1.33+, golangci-lint v2.6, Ko for images

## Commands

```bash
# IMPORTANT: goreleaser (used by make build, make qualify, e2e) fails if
# GITLAB_TOKEN is set alongside GITHUB_TOKEN. Always unset it first:
unset GITLAB_TOKEN

# Development workflow
make qualify      # Full check: test + lint + e2e + scan (run before PR)
make test         # Unit tests with -race
make lint         # golangci-lint + yamllint
make scan         # Grype vulnerability scan
make build        # Build binaries
make tidy         # Format + update deps

# Run single test
go test -v ./pkg/recipe/... -run TestSpecificFunction

# Run tests with race detector for specific package
go test -race -v ./pkg/collector/...

# Local development
make server                 # Start API server locally (debug mode)
make dev-env                # Create Kind cluster + start Tilt
make dev-env-clean          # Stop Tilt + delete cluster

# KWOK simulated cluster tests (no GPU hardware required)
make kwok-test-all                    # All recipes
make kwok-e2e RECIPE=eks-training     # Single recipe

# E2E tests (unset GITLAB_TOKEN to avoid goreleaser conflicts)
unset GITLAB_TOKEN && ./tools/e2e

# Tools management
make tools-setup  # Install all required tools
make tools-check  # Verify versions match .versions.yaml
```

## Non-Negotiable Rules

1. **Read before writing** — Never modify code you haven't read
2. **Tests must pass** — `make test` with race detector; never skip tests
3. **Run `make qualify` often** — Run at every stopping point (after completing a phase, before commits, before moving on). Fix ALL lint/test failures before proceeding. Do not treat pre-existing failures as acceptable.
4. **Use project patterns** — Learn existing code before inventing new approaches
5. **3-strike rule** — After 3 failed fix attempts, stop and reassess
6. **Structured errors** — Use `pkg/errors` with error codes (never `fmt.Errorf`)
7. **Context timeouts** — All I/O operations need context with timeout
8. **Check context in loops** — Always check `ctx.Done()` in long-running operations

## Git Configuration

- Commit to `main` branch (not `master`)
- Do use `-S` to cryptographically sign the commit
- Do NOT add `Co-Authored-By` lines (organization policy)
- Do not sign-off commits (no `-s` flag) unless the commit can't be cryptographically signed

## Key Packages

| Package | Purpose | Business Logic? |
|---------|---------|-----------------|
| `pkg/cli` | User interaction, input validation, output formatting | No |
| `pkg/api` | REST API handlers | No |
| `pkg/recipe` | Recipe resolution, overlay system, component registry | Yes |
| `pkg/bundler` | Per-component Helm bundle generation from recipes | Yes |
| `pkg/component` | Bundler utilities and test helpers | Yes |
| `pkg/collector` | System state collection | Yes |
| `pkg/validator` | Constraint evaluation | Yes |
| `pkg/errors` | Structured error handling with codes | Yes |
| `pkg/k8s/client` | Singleton Kubernetes client | Yes |

**Critical Architecture Principle:**
- `pkg/cli` and `pkg/api` = user interaction only, no business logic
- Business logic lives in functional packages so CLI and API can both use it

## Required Patterns

**Errors (always use pkg/errors):**
```go
import "github.com/NVIDIA/eidos/pkg/errors"

// Simple error
return errors.New(errors.ErrCodeNotFound, "GPU not found")

// Wrap existing error
return errors.Wrap(errors.ErrCodeInternal, "collection failed", err)

// With context
return errors.WrapWithContext(errors.ErrCodeTimeout, "operation timed out", ctx.Err(),
    map[string]interface{}{"component": "gpu-collector", "timeout": "10s"})
```

**Error Codes:** `ErrCodeNotFound`, `ErrCodeUnauthorized`, `ErrCodeTimeout`, `ErrCodeInternal`, `ErrCodeInvalidRequest`, `ErrCodeUnavailable`

**Context with timeout (always):**
```go
// Collectors: 10s timeout
func (c *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    // ...
}

// HTTP handlers: 30s timeout
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
    defer cancel()
    // ...
}
```

**Table-driven tests (required for multiple cases):**
```go
func TestFunction(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        expected string
        wantErr  bool
    }{
        {"valid input", "test", "test", false},
        {"empty input", "", "", true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := Function(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
            }
            if result != tt.expected {
                t.Errorf("got %v, want %v", result, tt.expected)
            }
        })
    }
}
```

**Functional options (configuration):**
```go
builder := recipe.NewBuilder(
    recipe.WithVersion(version),
)
server := server.New(
    server.WithName("eidosd"),
    server.WithVersion(version),
)
```

**Concurrency (errgroup):**
```go
g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return collector1.Collect(ctx) })
g.Go(func() error { return collector2.Collect(ctx) })
if err := g.Wait(); err != nil {
    return fmt.Errorf("collection failed: %w", err)
}
```

**Structured logging (slog):**
```go
slog.Debug("request started", "requestID", requestID, "method", r.Method)
slog.Error("operation failed", "error", err, "component", "gpu-collector")
```

## Common Tasks

| Task | Location | Key Points |
|------|----------|------------|
| New Helm component | `pkg/recipe/data/registry.yaml` | Add entry with name, displayName, helm settings, nodeScheduling |
| New Kustomize component | `pkg/recipe/data/registry.yaml` | Add entry with name, displayName, kustomize settings |
| Component values | `pkg/recipe/data/components/<name>/` | Create values.yaml with Helm chart configuration |
| New collector | `pkg/collector/<type>/` | Implement `Collector` interface, add to factory |
| New API endpoint | `pkg/api/` | Handler + middleware chain + OpenAPI spec update |
| Fix test failures | Run `make test` | Check race conditions (`-race`), verify context handling |

**Adding a Helm component (declarative - no Go code needed):**
```yaml
# pkg/recipe/data/registry.yaml
- name: my-operator
  displayName: My Operator
  valueOverrideKeys: [myoperator]
  helm:
    defaultRepository: https://charts.example.com
    defaultChart: example/my-operator
  nodeScheduling:
    system:
      nodeSelectorPaths: [operator.nodeSelector]
```

**Adding a Kustomize component (declarative - no Go code needed):**
```yaml
# pkg/recipe/data/registry.yaml
- name: my-kustomize-app
  displayName: My Kustomize App
  valueOverrideKeys: [mykustomize]
  kustomize:
    defaultSource: https://github.com/example/my-app
    defaultPath: deploy/production
    defaultTag: v1.0.0
```

**Note:** A component must have either `helm` OR `kustomize` configuration, not both.

## Anti-Patterns (Do Not Do)

| Anti-Pattern | Correct Approach |
|--------------|------------------|
| Modify code without reading it first | Always `Read` files before `Edit` |
| Skip or disable tests to make CI pass | Fix the actual issue |
| Invent new patterns | Study existing code in same package first |
| Use `fmt.Errorf` for errors | Use `pkg/errors` with error codes |
| Ignore context cancellation | Always check `ctx.Done()` in loops/operations |
| Add features not requested | Implement exactly what was asked |
| Create new files when editing suffices | Prefer `Edit` over `Write` |
| Guess at missing parameters | Ask for clarification |
| Continue after 3 failed fix attempts | Stop, reassess approach, explain blockers |

## Key Files

| File | Purpose |
|------|---------|
| `CONTRIBUTING.md` | Contribution guidelines, PR process, DCO |
| `DEVELOPMENT.md` | Development setup, architecture, Make targets |
| `RELEASING.md` | Release process for maintainers |
| `.versions.yaml` | Tool versions (single source of truth) |
| `pkg/recipe/data/registry.yaml` | Declarative component configuration |
| `pkg/recipe/data/overlays/*.yaml` | Recipe overlay definitions |
| `pkg/recipe/data/components/*/values.yaml` | Component Helm values |
| `api/eidos/v1/server.yaml` | OpenAPI spec |
| `.goreleaser.yaml` | Release configuration |

## Troubleshooting

| Issue | Check |
|-------|-------|
| K8s connection fails | `~/.kube/config` or `KUBECONFIG` env |
| GPU not detected | `nvidia-smi` in PATH |
| Linter errors | Use `errors.Is()` not `==`; add `return` after `t.Fatal()` |
| Race conditions | Run with `-race` flag |
| Build failures | Run `make tidy` |

## Design Principles

**Operational:**
- Partial failure is the steady state — design for partitions, timeouts, bounded retries
- Boring first — default to proven, simple technologies
- Observability is mandatory — structured logging, metrics, tracing

**Foundational:**
- Local development equals CI — `.versions.yaml` is single source of truth
- Correctness must be reproducible — same inputs → same outputs, always
- Metadata is separate from consumption — recipes define *what*, bundlers determine *how*
- Recipe specialization requires explicit intent — never silently upgrade to specialized configs
- Trust requires verifiable provenance — SLSA, SBOM, Sigstore

## Decision Framework

When choosing between approaches, prioritize in this order:
1. **Testability** — Can it be unit tested without external dependencies?
2. **Readability** — Can another engineer understand it quickly?
3. **Consistency** — Does it match existing patterns in the codebase?
4. **Simplicity** — Is it the simplest solution that works?
5. **Reversibility** — Can it be easily changed later?

## CLI Workflow Examples

```bash
# Capture system state
eidos snapshot --output snapshot.yaml

# Generate recipe from snapshot
eidos recipe --snapshot snapshot.yaml --intent training --output recipe.yaml

# Generate recipe from query parameters
eidos recipe --service eks --accelerator h100 --intent training --os ubuntu --platform kubeflow

# Create deployment bundle
eidos bundle --recipe recipe.yaml --output ./bundles

# Validate recipe against snapshot
eidos validate --recipe recipe.yaml --snapshot snapshot.yaml

# Bundle with value overrides
eidos bundle -r recipe.yaml \
  --set gpuoperator:driver.version=570.86.16 \
  --deployer argocd \
  -o ./bundles
```

## Full Reference

See `CONTRIBUTING.md`, `DEVELOPMENT.md`, `RELEASING.md`, and `.github/copilot-instructions.md` for extended documentation including:
- Detailed code examples for collectors, bundlers, API endpoints
- GitHub Actions architecture (three-layer composite actions)
- CI/CD workflows, supply chain security (SLSA, SBOM, Cosign)
- E2E testing patterns and KWOK simulated cluster testing

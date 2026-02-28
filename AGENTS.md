# AGENTS.md

This file provides guidance to Codex when working with code in this repository.

## Scope

Applies to the entire repository rooted at this directory.

## Role & Expertise

Act as a Principal Distributed Systems Architect with deep expertise in Go and cloud-native architectures. Focus on correctness, resiliency, and operational simplicity. All code must be production-grade, not illustrative pseudo-code.

## Project Overview

NVIDIA AI Cluster Runtime (AICR) generates validated GPU-accelerated Kubernetes configurations.

Workflow: Snapshot -> Recipe -> Validate -> Bundle

```text
┌─────────┐    ┌────────┐    ┌──────────┐    ┌────────┐
│Snapshot │───▶│ Recipe │───▶│ Validate │───▶│ Bundle │
└─────────┘    └────────┘    └──────────┘    └────────┘
   │              │               │              │
   ▼              ▼               ▼              ▼
 Capture       Generate        Check         Create
 cluster       optimized      constraints    Helm values,
 state         config         vs actual     manifests
```

Tech stack: Go 1.26, Kubernetes 1.33+, golangci-lint v2.10.1, Ko for images.

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
make tools-check  # Verify versions match .settings.yaml
```

## Non-Negotiable Rules

1. Read before writing. Never modify code you have not read.
2. Tests must pass. Run `make test` with race detector; never skip tests.
3. Run `make qualify` often: at every stopping point (after completing a phase, before commits, before moving on). Fix all lint/test failures before proceeding. Do not treat pre-existing failures as acceptable.
4. Use project patterns. Learn existing code before inventing new approaches.
5. 3-strike rule. After 3 failed fix attempts, stop and reassess.
6. Structured errors. Use `pkg/errors` with error codes (never ad-hoc errors).
7. Context timeouts. All I/O operations need context with timeout.
8. Check context in loops. Always check `ctx.Done()` in long-running operations.

## Git Configuration

- Commit to `main` branch (not `master`).
- Use `-S` to cryptographically sign commits.
- Do not add `Co-Authored-By` lines (organization policy).
- Do not sign-off commits (no `-s` flag); cryptographic signing (`-S`) satisfies DCO for AI-authored commits.

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
| `pkg/manifest` | Shared Helm-compatible manifest rendering | Yes |
| `pkg/evidence` | Conformance evidence capture and formatting | Yes |
| `pkg/snapshotter` | System state snapshot orchestration | Yes |
| `pkg/k8s/client` | Singleton Kubernetes client | Yes |
| `pkg/k8s/pod` | Shared K8s Job/Pod utilities (wait, logs, ConfigMap URIs) | Yes |
| `pkg/validator/helper` | Shared validator helpers (PodLifecycle, test context) | Yes |
| `pkg/defaults` | Centralized timeout and configuration constants | Yes |

Critical architecture principle:

- `pkg/cli` and `pkg/api` are user interaction only, no business logic.
- Business logic lives in functional packages so CLI and API can both use it.

## Required Patterns

Errors (always use `pkg/errors`):

```go
import "github.com/NVIDIA/aicr/pkg/errors"

// Simple error
return errors.New(errors.ErrCodeNotFound, "GPU not found")

// Wrap existing error
return errors.Wrap(errors.ErrCodeInternal, "collection failed", err)

// With context
return errors.WrapWithContext(errors.ErrCodeTimeout, "operation timed out", ctx.Err(),
    map[string]interface{}{"component": "gpu-collector", "timeout": "10s"})
```

Error codes: `ErrCodeNotFound`, `ErrCodeUnauthorized`, `ErrCodeTimeout`, `ErrCodeInternal`, `ErrCodeInvalidRequest`, `ErrCodeUnavailable`.

Context with timeout (always):

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

Table-driven tests (required for multiple cases):

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

Functional options (configuration):

```go
builder := recipe.NewBuilder(
    recipe.WithVersion(version),
)
server := server.New(
    server.WithName("aicrd"),
    server.WithVersion(version),
)
```

Concurrency (`errgroup`):

```go
g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return collector1.Collect(ctx) })
g.Go(func() error { return collector2.Collect(ctx) })
if err := g.Wait(); err != nil {
    return fmt.Errorf("collection failed: %w", err)
}
```

Structured logging (`slog`):

```go
slog.Debug("request started", "requestID", requestID, "method", r.Method)
slog.Error("operation failed", "error", err, "component", "gpu-collector")
```

## Common Tasks

| Task | Location | Key Points |
|------|----------|------------|
| New Helm component | `recipes/registry.yaml` | Add entry with name, displayName, helm settings, nodeScheduling |
| New Kustomize component | `recipes/registry.yaml` | Add entry with name, displayName, kustomize settings |
| Component values | `recipes/components/<name>/` | Create `values.yaml` with Helm chart configuration |
| New collector | `pkg/collector/<type>/` | Implement `Collector` interface, add to factory |
| New API endpoint | `pkg/api/` | Handler + middleware chain + OpenAPI spec update |
| Fix test failures | Run `make test` | Check race conditions (`-race`), verify context handling |

Adding a Helm component (declarative, no Go code needed):

```yaml
# recipes/registry.yaml
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

Adding a Kustomize component (declarative, no Go code needed):

```yaml
# recipes/registry.yaml
- name: my-kustomize-app
  displayName: My Kustomize App
  valueOverrideKeys: [mykustomize]
  kustomize:
    defaultSource: https://github.com/example/my-app
    defaultPath: deploy/production
    defaultTag: v1.0.0
```

Note: A component must have either `helm` or `kustomize` configuration, not both.

## Error Wrapping Rules

Never return bare errors. Every `return err` must wrap with context:

```go
// BAD - bare return loses context
if err := doSomething(); err != nil {
    return err
}

// GOOD - wrapped with context
if err := doSomething(); err != nil {
    return errors.Wrap(errors.ErrCodeInternal, "failed to do something", err)
}
```

Do not double-wrap errors that already have proper codes. If a called function already returns a `pkg/errors` structured error with the right code, do not re-wrap and change its code:

```go
// BAD - overwrites inner ErrCodeNotFound with ErrCodeInternal
content, err := readTemplateContent(ctx, path) // returns ErrCodeNotFound
return errors.Wrap(errors.ErrCodeInternal, "read failed", err)

// GOOD - propagate as-is when inner error already has correct code
content, err := readTemplateContent(ctx, path)
return err
```

## Review Expectations

When asked to review:

1. Focus first on bugs, regressions, behavioral risks, and missing tests.
2. Provide precise file/line references.
3. List assumptions/open questions separately.
4. Keep overviews brief and secondary to findings.

## Local Environment Preferences (Mirrored from `.claude/settings.local.json`)

These are preference-level notes for local execution contexts:

- Sandbox enabled.
- Bash commands broadly allowed in sandboxed mode.
- Prefer web access via GitHub and raw GitHub domains when needed.
- Additional allowed host pattern: `*.eks.amazonaws.com`.

Note: actual tool/runtime permissions are enforced by the active Codex environment and may differ.

# CLAUDE.md

This file is the canonical source for coding-agent rules. `AGENTS.md` is an auto-synced mirror (CI enforced).
This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Local Overlay

If present, also read `AGENTS.local.md` at the repo root. The file is gitignored repo-wide so personal overlays stay local — agents must check the exact path directly (e.g., `Read` or `cat`), not rely on ignore-respecting discovery tools such as `rg`, `fd`, or `git ls-files`. Treat it as a local overlay for this working copy: follow it when it does not conflict with higher-priority instructions or this shared `AGENTS.md`.

## Role & Expertise

Act as a Principal Distributed Systems Architect with deep expertise in Go and cloud-native architectures. Focus on correctness, resiliency, and operational simplicity. All code must be production-grade, not illustrative pseudo-code.

## Project Overview

NVIDIA AI Cluster Runtime (AICR) generates validated GPU-accelerated Kubernetes configurations.

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

**Tech Stack:** Go 1.26, Kubernetes 1.33+, golangci-lint v2.10.1, Ko for images

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

# Local health check validation
make check-health COMPONENT=nvsentinel  # Direct chainsaw against Kind
make check-health-all                   # All components
make validate-local RECIPE=recipe.yaml  # Full pipeline in Kind
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

## Review Output Links

When providing review findings, use global GitHub file links by default
(`https://github.com/<org>/<repo>/blob/<sha>/<path>#L<line>`) instead of local
workspace paths. Use local file paths only when explicitly requested.

## Git Configuration

- Commit to `main` branch (not `master`)
- Do use `-S` to cryptographically sign the commit
- Do NOT add `Co-Authored-By` lines (organization policy)
- Do not sign-off commits (no `-s` flag); cryptographic signing (`-S`) satisfies DCO for AI-authored commits

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
| `pkg/collector/topology` | Cluster-wide node taint/label topology collection | Yes |
| `pkg/snapshotter` | System state snapshot orchestration | Yes |
| `pkg/k8s/client` | Singleton Kubernetes client | Yes |
| `pkg/k8s/pod` | Shared K8s Job/Pod utilities (wait, logs, ConfigMap URIs) | Yes |
| `pkg/validator/helper` | Shared validator helpers (PodLifecycle, test context) | Yes |
| `pkg/defaults` | Centralized timeout and configuration constants | Yes |

**Critical Architecture Principle:**
- `pkg/cli` and `pkg/api` = user interaction only, no business logic
- Business logic lives in functional packages so CLI and API can both use it

## Required Patterns

**Errors (always use pkg/errors):**
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
    server.WithName("aicrd"),
    server.WithVersion(version),
)
```

**Concurrency (errgroup):**
```go
g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return collector1.Collect(ctx) })
g.Go(func() error { return collector2.Collect(ctx) })
if err := g.Wait(); err != nil {
    return errors.Wrap(errors.ErrCodeInternal, "collection failed", err)
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
| New Helm component | `recipes/registry.yaml` | Add entry with name, displayName, helm settings, nodeScheduling |
| New Kustomize component | `recipes/registry.yaml` | Add entry with name, displayName, kustomize settings |
| Component values | `recipes/components/<name>/` | Create values.yaml with Helm chart configuration |
| New collector | `pkg/collector/<type>/` | Implement `Collector` interface, add to factory |
| New API endpoint | `pkg/api/` | Handler + middleware chain + OpenAPI spec update |
| Fix test failures | Run `make test` | Check race conditions (`-race`), verify context handling |
| New health check | `recipes/checks/<name>/` | Create `health-check.yaml`, register in `registry.yaml`, test with `make check-health` |

**Adding a Helm component (declarative - no Go code needed):**
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

**Adding a Kustomize component (declarative - no Go code needed):**
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

**Note:** A component must have either `helm` OR `kustomize` configuration, not both.

**Using mixins for shared OS/platform content:**
```yaml
# Leaf overlay referencing mixins instead of duplicating content
spec:
  base: h100-eks-ubuntu-training
  mixins:
    - os-ubuntu          # Ubuntu constraints (defined once in recipes/mixins/)
    - platform-kubeflow  # kubeflow-trainer component (defined once in recipes/mixins/)
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: kubeflow
  constraints:
    - name: K8s.server.version
      value: ">= 1.32.4"
```

Mixins carry only `constraints` and `componentRefs` — no `criteria`, `base`, `mixins`, or `validation`. They live in `recipes/mixins/` with `kind: RecipeMixin`.

## Error Wrapping Rules

**Never return bare errors.** Every `return err` must wrap with context:
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

**Don't double-wrap errors that already have proper codes.** If a called function already returns a `pkg/errors` StructuredError with the right code, don't re-wrap and change its code:
```go
// BAD - overwrites inner ErrCodeNotFound with ErrCodeInternal
content, err := readTemplateContent(ctx, path) // returns ErrCodeNotFound
return errors.Wrap(errors.ErrCodeInternal, "read failed", err)

// GOOD - propagate as-is when inner error already has correct code
content, err := readTemplateContent(ctx, path)
return err
```

**Exception:** Wrapping is unnecessary for read-only `Close()` returns and K8s helpers like `k8s.IgnoreNotFound(err)`.

**Always use `errors.Is()` for sentinel error checks.** `golangci-lint` enforces the `errorlint` rule — comparing errors with `==` fails on wrapped errors and will be rejected by CI:

```go
// BAD - fails errorlint, breaks on wrapped errors
if err == io.EOF {

// GOOD - works with wrapped errors, passes linter
if errors.Is(err, io.EOF) {
```

Note: in files that import `pkg/errors`, the standard library `errors` package is aliased as `stderrors`, so use `stderrors.Is(...)` there.

**Writable file handles must check `Close()` errors.** If a file handle is writable (e.g., from `os.Create` or `os.OpenFile`), closing it may flush buffered data; always capture and check the error:
```go
// BAD - writable Close() error ignored
defer f.Close()

// GOOD - writable Close() error checked
closeErr := f.Close()
if err == nil {
    err = closeErr
}
```

## Context Propagation Rules

**Never use `context.Background()` in I/O methods.** Use a timeout-bounded context:
```go
// BAD - unbounded context
func (r *Reader) Read(url string) ([]byte, error) {
    return r.ReadWithContext(context.Background(), url)
}

// GOOD - timeout-bounded
func (r *Reader) Read(url string) ([]byte, error) {
    ctx, cancel := context.WithTimeout(context.Background(), r.TotalTimeout)
    defer cancel()
    return r.ReadWithContext(ctx, url)
}
```

**`context.Background()` is acceptable ONLY for:** cleanup in deferred functions (when parent context is canceled), graceful shutdown, and test setup.

## HTTP Client Rules

**Never use `http.DefaultClient`.** It has zero timeout. Always use a custom client with an explicit timeout:
```go
// BAD - no timeout, can hang indefinitely
resp, err := http.DefaultClient.Do(req)

// GOOD - bounded timeout from pkg/defaults
client := &http.Client{Timeout: defaults.HTTPClientTimeout}
resp, err := client.Do(req)
```

## Logging Rules

**Always use `slog` for output in production code.** Never use `fmt.Println`, `fmt.Printf`, or `fmt.Fprintln` for logging or streaming output:
```go
// BAD
fmt.Println(scanner.Text())

// GOOD
slog.Info(scanner.Text())
```

**Exception:** `fmt.Fprintln(logWriter(), ...)` for agent log output to stderr is acceptable when structured logging would add noise to raw log streaming.

## Constants Rules

**Use named constants from `pkg/defaults` instead of magic literals.** If a timeout, limit, or configuration value is used anywhere, it should be a named constant:
```go
// BAD - magic literal
ExpectContinueTimeout: 1 * time.Second,

// GOOD - named constant
ExpectContinueTimeout: defaults.HTTPExpectContinueTimeout,
```

## Kubernetes Patterns

**Use watch API instead of polling** for efficiency and reduced API server load:
```go
// BAD - polling with sleep
ticker := time.NewTicker(500 * time.Millisecond)
for {
    select {
    case <-ticker.C:
        pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
        if pod.Status.Phase == v1.PodSucceeded {
            return nil
        }
    }
}

// GOOD - watch API
watcher, err := client.CoreV1().Pods(ns).Watch(ctx, metav1.ListOptions{
    FieldSelector: "metadata.name=" + name,
})
defer watcher.Stop()
for event := range watcher.ResultChan() {
    pod := event.Object.(*v1.Pod)
    if pod.Status.Phase == v1.PodSucceeded {
        return nil
    }
}
```

**Use create-or-update semantics for mutable K8s resources** instead of `IgnoreAlreadyExists`:
```go
// BAD - stale resource silently kept from prior run
_, err = clientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{})
if apierrors.IsAlreadyExists(err) {
    return nil // stale rules persist!
}

// GOOD - create, then update if exists
_, err = clientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{})
if apierrors.IsAlreadyExists(err) {
    _, err = clientset.RbacV1().Roles(ns).Update(ctx, role, metav1.UpdateOptions{})
    if err != nil {
        return errors.Wrap(errors.ErrCodeInternal, "failed to update Role", err)
    }
    return nil
}
```

**`IgnoreAlreadyExists` is acceptable ONLY for:** immutable resources (ServiceAccounts, Namespaces) where updates are not needed.

**Use shared utilities from `pkg/k8s/pod`** instead of reimplementing:
```go
// Use for Job completion
err := pod.WaitForJobCompletion(ctx, client, namespace, jobName, timeout)

// Use for pod logs
logs, err := pod.GetPodLogs(ctx, client, namespace, podName)

// Use for streaming logs
err := pod.StreamLogs(ctx, client, namespace, podName, os.Stdout)

// Use for ConfigMap URI parsing
namespace, name, err := pod.ParseConfigMapURI("cm://gpu-operator/aicr-snapshot")
```

## Test Isolation

**Always use `--no-cluster` flag in tests** to prevent production cluster access:
```go
// Unit tests: Use WithNoCluster(true)
v := validator.New(
    validator.WithNoCluster(true),
    validator.WithVersion(version),
)

// E2E tests: Use --no-cluster flag
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml --no-cluster

// Chainsaw tests: Always include --no-cluster
${AICR_BIN} validate -r recipe.yaml -s snapshot.yaml --no-cluster
```

**Test mode behavior:** When `NoCluster` is true:
- Validator skips RBAC creation (ServiceAccount, Role, ClusterRole)
- Validator skips Job deployment for checks
- All checks report status as "skipped - no-cluster mode (test mode)"
- Constraints are still evaluated inline (no cluster access needed)

## Anti-Patterns (Do Not Do)

| Anti-Pattern | Correct Approach |
|--------------|------------------|
| Modify code without reading it first | Always `Read` files before `Edit` |
| Skip or disable tests to make CI pass | Fix the actual issue |
| Invent new patterns | Study existing code in same package first |
| Use `fmt.Errorf` for errors | Use `pkg/errors` with error codes |
| Return bare `err` without wrapping | Always `errors.Wrap()` with context message |
| Use `context.Background()` in I/O methods | Use `context.WithTimeout()` with bounded deadline |
| Use `fmt.Println` for logging | Use `slog.Info/Debug/Warn/Error` |
| Hardcode timeout/limit values | Define in `pkg/defaults` and reference by name |
| Re-wrap errors that already have correct codes | Return as-is to preserve error code |
| Ignore context cancellation | Always check `ctx.Done()` in loops/operations |
| Add features not requested | Implement exactly what was asked |
| Create new files when editing suffices | Prefer `Edit` over `Write` |
| Guess at missing parameters | Ask for clarification |
| Continue after 3 failed fix attempts | Stop, reassess approach, explain blockers |
| Use polling loops for K8s operations | Use watch API for efficiency |
| Compare errors with `==` (e.g., `err == io.EOF`) | Use `errors.Is(err, io.EOF)` (`stderrors.Is` in files that alias stdlib errors) — `errorlint` enforced by CI |
| Duplicate K8s utilities across packages | Use shared utilities from `pkg/k8s/pod` |
| Run tests that connect to live clusters | Always use `--no-cluster` flag in tests |
| Use boolean flags to track options | Use pointer pattern (nil = not set, &value = set) |
| Use `http.DefaultClient` | Use custom `&http.Client{Timeout: defaults.HTTPClientTimeout}` |
| Use `IgnoreAlreadyExists` for mutable K8s resources | Use create-or-update semantics (Create, then Update if exists) |
| Ignore `Close()` error on writable file handles | Capture and check `closeErr := f.Close()` |
| Hardcode resource names from templates | Extract to named constants to keep code and templates in sync |

## Pull Request Requirements

**Pre-push checklist:** Always run `make qualify` before pushing. This is the CI-equivalent gate that covers tests, linting (golangci-lint + yamllint), e2e, vulnerability scan, and repo-specific checks (docs sidebar, agents sync). Do not substitute a subset of commands — if `make qualify` passes locally, CI will pass.

**Mandatory lint gate for Go changes:** If your PR changes any `.go` files, you MUST run `golangci-lint run -c .golangci.yaml` on each affected package path (e.g., `./pkg/recipe/...`, `./cmd/aicr/...`, `./tests/chainsaw/...`) and confirm zero issues before creating or pushing the PR. For a full module scan, use `./...`. Do not rely on CI to catch lint failures — fix them locally first. This applies even to PRs labeled as "documentation only" if they include Go code changes.

**Branch hygiene:**
- Always rebase onto the target branch before pushing: `git fetch origin main && git rebase origin/main`
- Squash commits into a single commit before push
- Cryptographically sign commits (`git commit -S`)

**PR description:** Use the template from `.github/PULL_REQUEST_TEMPLATE.md` exactly as defined there. Do not inline a modified copy — read and fill in the canonical template. The template covers: Summary, Motivation/Context (with Fixes/Related), Type of Change, Components Affected, Implementation Notes, Testing, Risk Assessment, and Checklist.

**Test coverage gate (Go packages only):**
Before pushing a PR that changes Go source files, check test coverage on affected packages. Set `pkg` to the narrowest directory root you want to measure — `$pkg/...` intentionally includes descendant packages. Prefer the narrowest changed root (e.g., if only `pkg/collector/topology` changed, use `pkg=pkg/collector/topology`, not `pkg=pkg/collector`). Use a broader root only when you intentionally want one combined delta across related subpackages.
1. Run `GOFLAGS="-mod=vendor" go test -coverprofile=cover.out ./$pkg/...` on each changed package
2. Get the baseline using a clean worktree (changes must be committed first): `(git worktree add $TMPDIR/baseline origin/main && (cd $TMPDIR/baseline && GOFLAGS="-mod=vendor" go test -coverprofile=$TMPDIR/base.out ./$pkg/...); rc=$?; git worktree remove --force $TMPDIR/baseline; return $rc 2>/dev/null || (exit $rc))`. This preserves the test exit status through cleanup. Write the profile to `$TMPDIR/base.out` (outside the worktree) so it survives cleanup. Compare with `go tool cover -func` on both profiles. Skip this step for entirely new packages.
3. **Block** if `make test-coverage` fails — this enforces the project-wide 70% floor (from `.settings.yaml`). Do not use per-package profiles for this check.
4. **Flag** any package with per-package coverage decrease > 0.5% (comparing step 1 vs step 2)
5. **Block** if any new exported function or method (identified via `git diff origin/main -- $pkg/` — look for added `func` lines with uppercase names) has 0% coverage — add tests before pushing
6. Report the delta in the PR description's Testing section (e.g., `pkg/recipe: 90.4% → 90.3% (-0.1%)`)
This rule does not apply to non-Go changes (YAML, docs, CI workflows). Note: CI also posts per-package coverage deltas post-push via `go-coverage-report` in `on-push-comment.yaml`; this gate catches regressions before push.

**PR policy:**
- Do NOT add `Co-Authored-By` lines (organization policy)
- Do NOT add "Generated with Claude Code", "Created by Codex", or similar attribution
- Add appropriate type labels: `enhancement`, `bug`, `documentation`
- Area labels are auto-assigned by `.github/labeler.yml` based on changed file paths (e.g., `area/recipes`, `area/ci`, `area/api`, `area/cli`, `area/bundler`, `area/collector`, `area/validator`, `area/docs`, `area/infra`, `area/tests`). You may also add them manually when the auto-labeler wouldn't match (e.g., issue-only PRs or cross-cutting changes).
- Do NOT add `size/*` labels (auto-assigned by bot)
- Keep the PR title under 70 characters; use the description for details

## Key Files

| File | Purpose |
|------|---------|
| `CONTRIBUTING.md` | Contribution guidelines, PR process, DCO |
| `DEVELOPMENT.md` | Development setup, architecture, Make targets |
| `RELEASING.md` | Release process for maintainers |
| `.settings.yaml` | Project settings: tool versions, quality thresholds, build/test config (single source of truth) |
| `recipes/registry.yaml` | Declarative component configuration |
| `recipes/overlays/*.yaml` | Recipe overlay definitions |
| `recipes/mixins/*.yaml` | Composable mixin fragments (OS constraints, platform components) |
| `recipes/components/*/values.yaml` | Component Helm values |
| `api/aicr/v1/server.yaml` | OpenAPI spec |
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
- Local development equals CI — `.settings.yaml` is single source of truth
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
aicr snapshot --output snapshot.yaml

# Generate recipe from snapshot
aicr recipe --snapshot snapshot.yaml --intent training --output recipe.yaml

# Generate recipe from query parameters
aicr recipe --service eks --accelerator h100 --intent training --os ubuntu --platform kubeflow

# Create deployment bundle
aicr bundle --recipe recipe.yaml --output ./bundles

# Query a specific hydrated value from a recipe
aicr query --service eks --accelerator h100 --intent training \
  --selector components.gpu-operator.values.driver.version

# Validate recipe against snapshot
aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

# Bundle with value overrides
aicr bundle -r recipe.yaml \
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

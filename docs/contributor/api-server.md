# API Server (`aicrd`)

`aicrd` is a stateless HTTP service that exposes recipe and bundle
generation over REST. It is a thin transport over the
[`pkg/client/v1`](https://github.com/NVIDIA/aicr/blob/main/pkg/client/v1)
facade — the same `aicr.Client` the CLI uses. The server owns parsing,
allowlist enforcement, response shape, and middleware; the facade and
downstream packages own everything else.

The boundary is hard. Handlers are **adapters**, not business logic.
Any code under `pkg/server/*_handler.go` that does more than parse →
allowlist-check → call facade → format response is a review-blocker.
See [contributor index](index.md) for the package separation rule and
[CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md)
for the underlying HTTP and error patterns.

For endpoint payload schemas, query parameters, and examples consult:

- [docs/user/api-reference.md](../user/api-reference.md) — user-facing reference
- [`api/aicr/v1/server.yaml`](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml) — canonical OpenAPI spec

This page covers the contributor view: package layout, middleware
ordering, the handler pattern, and the walkthrough for adding an
endpoint.

## Package Layout

All server code lives in [`pkg/server`](https://github.com/NVIDIA/aicr/tree/main/pkg/server).

| File | Responsibility |
|------|----------------|
| `serve.go` | Entry point. Parses env allowlists, constructs `aicr.Client`, wires `/v1/recipe`, `/v1/query`, `/v1/bundle`, runs `Server.Run` |
| `server.go` | `Server` struct, options, route mux, lifecycle (`Start`, `Shutdown`, `Run`) |
| `config.go` | `config` struct and env-var overrides (`PORT`, `SHUTDOWN_TIMEOUT_SECONDS`) |
| `middleware.go` | 8-layer middleware chain; ordering rationale lives in source comments |
| `recipe_handler.go` | `GET\|POST /v1/recipe` and `/v1/query` adapter over `Client.ResolveRecipeFromCriteria` |
| `bundle_handler.go` | `POST /v1/bundle` adapter over `Client.AdoptRecipe` + `Client.MakeBundle` |
| `health.go` | `GET /health` and `GET /ready` |
| `metrics.go` | Prometheus collectors (requests, duration, in-flight, rate-limit rejects, panic recoveries) |
| `version.go` | `X-API-Version` header negotiation from `Accept: application/vnd.nvidia.aicr.v1+json` |
| `errors.go` | `WriteError` / `WriteErrorFromErr` — central status mapping and cause-leak rule |
| `allowlist.go` | Handler-level allowlist pre-check (`validateAgainstAllowLists`) |
| `response_writer.go` | Status-capture wrapper so middleware can observe the handler's status code |
| `context.go` | Typed context keys and `RequestIDFromContext` helper |
| `openapi_sync_test.go` | CI gate: enum drift between OpenAPI spec and Go criteria types fails the build |

`cmd/aicrd/main.go` is a one-liner that calls `server.Serve()`.

## Middleware Chain

Composition lives in `withMiddleware` in
[`middleware.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/middleware.go#L45-L61).
Order is **outermost first**:

| # | Layer | Purpose |
|---|-------|---------|
| 1 | `metricsMiddleware` | Start timer, increment in-flight gauge, record duration and status histogram. Outermost so total latency is captured. |
| 2 | `versionMiddleware` | Parse `Accept` for `application/vnd.nvidia.aicr.v<N>+json`; stash version in context; set `X-API-Version` response header |
| 3 | `requestIDMiddleware` | Honor `X-Request-Id` if a valid UUID, else mint one; stash in context; echo to response header |
| 4 | `timeoutMiddleware` | `context.WithTimeout(r.Context(), defaults.ServerHandlerTimeout)` (90s). Bounds every inner layer, including body reads inside the handler. |
| 5 | `loggingMiddleware` | Captures status via `responseWriter`; logs request start (Debug) and completion (Debug/Warn/Error keyed on status class) |
| 6 | `panicRecoveryMiddleware` | `defer recover()` → 500 + `panicRecoveries` counter. Inside logging so the completion line still fires. |
| 7 | `rateLimitMiddleware` | `golang.org/x/time/rate` limiter (default 100 req/s, burst 200). Always emits `X-RateLimit-*` headers, including on the 429 branch. |
| 8 | `bodyLimitMiddleware` | `http.MaxBytesReader(r.Body, defaults.ServerMaxBodyBytes)` (8 MiB). Innermost so a handler installing a tighter cap composes cleanly. |

Ordering invariants (also documented in source):

- **Timeout outside logging.** Logged latency reflects the real deadline.
- **Panic recovery inside logging.** A panic-converted 500 still produces the completion log line.
- **Rate limit outside body limit.** A 429 short-circuits before any body-cap setup.
- **Body limit innermost.** Per-endpoint `http.MaxBytesReader` calls in handlers (recipe = 1 MiB, bundle = 8 MiB) reapply cleanly inside the default cap.

System endpoints — `/`, `/health`, `/ready`, `/metrics` — bypass the
chain entirely. Only application routes registered via `WithHandler`
go through it.

## Handler Pattern

Every handler is an adapter. The shape, in order:

1. **Method gate.** Reject with 405 and set `Allow:` header. Use `WriteError` with `ErrCodeMethodNotAllowed`.
2. **Per-handler context timeout.** `context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)` (30s) or `BundleHandlerTimeout` (60s). All must be ≤ `ServerHandlerTimeout` (90s) or the outer middleware clamps them.
3. **Parse input.** Query parameters via `recipe.ParseCriteriaFromRequest`; bodies via `json.NewDecoder` wrapped in `http.MaxBytesReader` for the per-endpoint cap.
4. **Allowlist pre-check.** `validateAgainstAllowLists(h.allowLists, criteria)` runs the same projection the facade uses (`aicr.ToInternalAllowLists`) so the handler error message and facade backstop never drift.
5. **Call the facade.** `Client.ResolveRecipeFromCriteria`, `Client.AdoptRecipe`, `Client.MakeBundle`. No business logic in the handler itself.
6. **Format the response.** `serializer.RespondJSON` for JSON; stream zip bytes directly for bundle. Set `Cache-Control: public, max-age=<RecipeCacheTTL>` on cacheable GETs.
7. **Errors via `WriteErrorFromErr`.**

### Body bounding

Bodies are bounded twice: defense-in-depth.

```go
// per-endpoint cap applied inside the handler
bounded := http.MaxBytesReader(w, r.Body, defaults.MaxBundlePOSTBytes)
if err := json.NewDecoder(bounded).Decode(&recipeResult); err != nil {
    var maxBytesErr *http.MaxBytesError
    if stderrors.As(err, &maxBytesErr) {
        WriteError(w, r, http.StatusRequestEntityTooLarge,
            aicrerrors.ErrCodeInvalidRequest, "...", false, ...)
        return
    }
    ...
}
```

| Cap | Value | Where |
|-----|-------|-------|
| `defaults.ServerMaxBodyBytes` | 8 MiB | Default for all routes via `bodyLimitMiddleware` |
| `defaults.MaxRecipePOSTBytes` | 1 MiB | Recipe and query POST bodies |
| `defaults.MaxBundlePOSTBytes` | 8 MiB | Bundle POST bodies |

### Error responses and the 5xx cause-leak rule

All errors flow through
[`WriteErrorFromErr`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/errors.go#L127).
It maps a `*errors.StructuredError` to an HTTP status via `httpStatusFromCode`
and serializes the `ErrorResponse` shape (`code`, `message`, `details`,
`requestId`, `timestamp`, `retryable`).

Critical rule, enforced at this single chokepoint:

> Embed `Cause.Error()` in `details["error"]` **only when status < 500**.
> 4xx errors typically carry validator feedback the client needs;
> 5xx errors carry internal paths, kubeconfig contents, or upstream
> service hostnames that must not leak.

Handlers must always go through `WriteErrorFromErr` — never construct
an `errorResponse` directly. Bare `fmt.Errorf` or string concatenation
of internal causes into a 500 response body is a review-blocker; the
underlying violation is the
[error-wrapping rule in CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md).

### Allowlists

`aicr.AllowLists` is parsed from environment at startup
(`aicr.ParseAllowListsFromEnv`) and passed to both:

- The `aicr.Client` via `aicr.WithAllowLists(...)`. The facade enforces on `ResolveRecipeFromCriteria` and `MakeBundle`. This is the **backstop**.
- Each handler via `newRecipeHandler(client, allowLists)` / `newBundleHandler(client, allowLists)`. The handler runs an explicit pre-check (`validateAgainstAllowLists`) so the user-facing rejection message stays exact.

Both call sites go through `aicr.ToInternalAllowLists` so a new
field is wired in one place.

## Endpoints

| Route | Methods | Purpose |
|-------|---------|---------|
| `/` | GET | Lists registered routes (unmatched paths route here via `ServeMux`) |
| `/health` | GET | Liveness — always 200 if the process is running |
| `/ready` | GET | Readiness — 503 with `reason` until `setReady(true)`, 200 after |
| `/metrics` | GET | Prometheus exposition (`promhttp.Handler()`) |
| `/v1/recipe` | GET, POST | Resolve recipe from criteria → `RecipeResult` JSON |
| `/v1/query` | GET, POST | Resolve recipe, hydrate values, return value at `?selector=path` |
| `/v1/bundle` | POST | Adopt `RecipeResult` body, generate bundle, stream zip |

Schemas, query parameters, and example payloads live in
[docs/user/api-reference.md](../user/api-reference.md) and
[`api/aicr/v1/server.yaml`](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml).

## Configuration

Environment variables read at startup:

| Variable | Default | Source |
|----------|---------|--------|
| `PORT` | 8080 | `defaults.EnvServerPort` (in `config.go`) |
| `SHUTDOWN_TIMEOUT_SECONDS` | 30 | `defaults.EnvServerShutdownTimeoutSeconds` |
| `AICR_ALLOWED_ACCELERATORS` | unset → unrestricted | `aicr.ParseAllowListsFromEnv` |
| `AICR_ALLOWED_SERVICES` | unset → unrestricted | same |
| `AICR_ALLOWED_INTENTS` | unset → unrestricted | same |
| `AICR_ALLOWED_OS` | unset → unrestricted | same |
| `AICR_LOG_LEVEL` | `info` | `pkg/logging` |

Compiled-time constants live in
[`pkg/defaults`](https://github.com/NVIDIA/aicr/tree/main/pkg/defaults):

| Constant | Value |
|----------|-------|
| `ServerHandlerTimeout` | 90s (outer middleware) |
| `RecipeHandlerTimeout` | 30s (per-handler ctx) |
| `BundleHandlerTimeout` | 60s (per-handler ctx) |
| `ServerReadTimeout` / `WriteTimeout` / `IdleTimeout` | 10s / 90s / 120s |
| `ServerReadHeaderTimeout` | 5s |
| `ServerMaxHeaderBytes` | 64 KiB |
| `ServerDefaultRateLimit` / `Burst` | 100 rps / 200 |
| `RecipeCacheTTL` | 10m |

Constraint: every per-handler `WithTimeout` must be ≤ `ServerHandlerTimeout`,
and `ServerWriteTimeout` must be ≥ `ServerHandlerTimeout`, else the outer
middleware silently clamps a slow request.

## OpenAPI Parity Test

[`pkg/server/openapi_sync_test.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/openapi_sync_test.go)
asserts that every criteria-field enum in `api/aicr/v1/server.yaml`
matches the corresponding `pkg/recipe.GetCriteria*Types()` function.
It scans both query-parameter enums and `components.schemas.Criteria`
properties.

Drift is a contract bug: clients conforming to the spec will reject
inputs the server actually accepts, or generate types that reject
server outputs. Adding a value to a Go criteria type without updating
the spec — or the reverse — fails CI here.

The wildcard `"any"` is allowed in the spec but not the Go list; the
test strips it before comparison.

## Adding an Endpoint

1. **Edit `api/aicr/v1/server.yaml`.** Add the operation under `paths:`, request and response schemas under `components.schemas`. If the operation accepts criteria, reference `#/components/schemas/Criteria` so the parity test covers it.
2. **Add a facade method.** If new business logic is required, add it to `pkg/client/v1/aicr.go` (or a sibling file in `pkg/client/v1`). The CLI and any external Go caller will use the same method. Handlers must never call into `pkg/recipe`, `pkg/bundler`, etc. directly.
3. **Add the handler.** Create `pkg/server/<name>_handler.go`. Mirror the existing handler shape: method gate, per-handler timeout, parse, allowlist pre-check (if it accepts user input dimensions), bounded body read, facade call, `serializer.RespondJSON` or zip stream, `WriteErrorFromErr` on every error path.
4. **Register the route.** Add an entry to the `map[string]http.HandlerFunc` in `serve.go` (the `WithHandler` argument). The route picks up the full middleware chain automatically.
5. **Wire allowlists if needed.** Pass `allowLists` into the handler constructor and call `validateAgainstAllowLists` before the facade call. Do not invent a parallel allowlist path; reuse `aicr.ToInternalAllowLists`.
6. **Tighten the body cap.** If the endpoint accepts POST bodies and 8 MiB is wrong, define a `defaults.Max<Name>POSTBytes` constant and wrap `r.Body` with `http.MaxBytesReader` inside the handler. Handle `*http.MaxBytesError` explicitly → 413.
7. **Run the parity test.** `go test -run TestOpenAPIEnumsMatchGoTypes ./pkg/server/...`. Add cases to `openapi_sync_test.go` if you introduced a new enum-bearing field.
8. **Update [docs/user/api-reference.md](../user/api-reference.md)** in the same PR. CLAUDE.md's docs-updates-with-behavior-changes rule applies.

The endpoint cannot return business types raw — it must serialize
through `serializer.RespondJSON` (which uses deterministic encoding)
or stream binary content directly. Returning `map[string]any` from
`yaml.Marshal` is a reproducibility hazard called out in CLAUDE.md.

## Operational Surfaces

**Graceful shutdown.** `Serve` installs a `signal.NotifyContext` for
`SIGINT`/`SIGTERM` at the entry point so cancellation propagates through
both pre-`Run` setup and request handling. `Server.Shutdown` flips
`/ready` to 503 immediately, then calls `httpServer.Shutdown(ctx)` with
`defaults.ServerShutdownTimeout` (30s, overridable via
`SHUTDOWN_TIMEOUT_SECONDS`). A fresh `context.Background()` is used
intentionally — the parent is already canceled.

**Rate limiting.** Token bucket from `golang.org/x/time/rate`. Defaults
to 100 rps with burst 200. Limiter is re-created on every `New()` call.
Limiter headers (`X-RateLimit-Limit`, `-Remaining`, `-Reset`) ship on
every response, not just 429s, so clients can back off proactively.

**Panic recovery.** Wraps `rateLimit` + `bodyLimit` + handler. A panic
becomes a 500 via `WriteError(..., ErrCodeInternal, ...)`, increments
the `aicr_server_panic_recoveries_total` counter, and logs the full
panic value at Error. The `loggingMiddleware` is outside this layer so
the completion log still fires.

**Version negotiation.** `versionMiddleware` parses `Accept` headers
of the form `application/vnd.nvidia.aicr.v<N>+json`, validates against
the allow-list in `isValidAPIVersion` (currently `v1` only), and sets
`X-API-Version` on the response. Unknown or absent version → `v1`.
Add `v2` by extending the map in `version.go`.

**Metrics.** Prometheus collectors registered via `promauto` in
`metrics.go`: `aicr_server_requests_total{method,path,status}`,
`aicr_server_request_duration_seconds`, `aicr_server_requests_in_flight`,
`aicr_server_rate_limit_rejects_total`,
`aicr_server_panic_recoveries_total`.

## Testing

Use `httptest.NewRecorder` with the handler directly. Inject a fake
or real `aicr.Client` constructed against an embedded data source.
Do **not** start a full `Server` — exercising the middleware chain
belongs in `middleware_test.go`.

```go
client, _ := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
h := newRecipeHandler(client, nil)

req := httptest.NewRequest(http.MethodGet, "/v1/recipe?service=eks&accelerator=h100", nil)
w := httptest.NewRecorder()
h.HandleRecipes(w, req)

if w.Code != http.StatusOK { t.Fatalf("status = %d", w.Code) }
```

Pattern reminders from CLAUDE.md:

- Table-driven test cases when there are multiple inputs.
- Always check `ctx.Done()` if the handler under test spawns goroutines.
- Never use a live cluster; the facade with `EmbeddedSource()` is fully in-process.

For end-to-end coverage, the chainsaw suite under
[`tests/chainsaw/server/`](https://github.com/NVIDIA/aicr/tree/main/tests/chainsaw)
exercises the server binary against the embedded data set.

## References

- [`net/http`](https://pkg.go.dev/net/http) — server, `MaxBytesReader`, `MaxBytesError`
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging used by all middleware
- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `Server.Run` concurrency
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — rate limiter
- [CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md) — HTTP Server Rules, Error Wrapping Rules, Context Propagation Rules

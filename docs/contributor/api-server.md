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
| `serve.go` | Entry point. Parses env allowlists, constructs `aicr.Client`, wires the recipe, query, and bundle routes, runs `Server.Run` |
| `server.go` | `Server` struct, options, route mux, lifecycle (`Start`, `Shutdown`, `Run`) |
| `config.go` | `config` struct and env-var overrides (`PORT`, `SHUTDOWN_TIMEOUT_SECONDS`) |
| `middleware.go` | 8-layer middleware chain; ordering rationale lives in source comments |
| `recipe_handler.go` | `GET\|POST /v1/recipe` and `/v1/query` adapter over the profile-aware `Client` resolution methods |
| `bundle_handler.go` | `POST /v1/bundle` adapter over `Client.AdoptRecipe` + `Client.MakeBundle` |
| `health.go` | `GET /health` and `GET /ready` |
| `metrics.go` | Prometheus collectors (requests, duration, in-flight, rate-limit rejects, panic recoveries) |
| `version.go` | `X-API-Version` header negotiation from `Accept: application/vnd.nvidia.aicr.v1+json` |
| `errors.go` | `WriteError` / `WriteErrorFromErr` — central status mapping and cause-leak rule |
| `allowlist.go` | Handler-level allowlist pre-check (`validateAgainstAllowLists`) |
| `response_writer.go` | Status-capture wrapper so middleware can observe the handler's status code |
| `context.go` | Typed context keys and `RequestIDFromContext` helper |
| `openapi_sync_test.go` | CI gate: criteria-enum and `/v1/bundle` contract drift fails the build |

`cmd/aicrd/main.go` is a one-liner that calls `server.Serve()`.

## Middleware Chain

Composition lives in `withMiddleware` in
[`pkg/server/middleware.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/middleware.go).
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

System endpoints — `/health`, `/ready`, `/metrics` — bypass the chain
entirely. The `/` root route is added to the handlers map by
`configureRootHandler` (not via `WithHandler`, which installs only
caller-provided handlers) and is then wrapped by the same middleware loop, so
it runs the full chain like application routes.

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
[`WriteErrorFromErr`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/errors.go).
It maps a `*errors.StructuredError` to an HTTP status via `httpStatusFromCode`
and serializes the `ErrorResponse` shape (`code`, `message`, `details`,
`requestId`, `timestamp`, `retryable`).

Critical rule, enforced at this single chokepoint:

> Embed `Cause.Error()` in `details["error"]` **only when `status < 500`**.
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
| `/metrics` | GET, HEAD | Prometheus exposition (`promhttp.Handler()` behind `readOnly`, which rejects other methods with a structured 405) |
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
| `AICR_SIGNING_KEY` | unset → signing off | `parseSigningConfig` (Mode A KMS URI) |
| `AICR_FULCIO_URL` | unset → signing off | `parseSigningConfig` (Mode B private Fulcio) |
| `AICR_IDENTITY_TOKEN_FILE` | unset | `parseSigningConfig` (Mode B token source) |
| `AICR_REKOR_URL` | unset | `parseSigningConfig` (both modes) |
| `AICR_SIGNING_CONFIG_PATH` | unset | `parseSigningConfig` (both modes; Rekor v2) |
| `AICR_TLOG_UPLOAD` | `true` | `parseSigningConfig` (Mode A only) |
| `AICR_BINARY_ATTESTATION_FILE` | unset → `<executable>-attestation.sigstore.json` next to the binary | `resolveBinaryAttestationPath` (override for ko `KO_DATA_PATH` layouts) |
| `AICR_BINARY_ATTESTATION_IDENTITY_REGEXP` | unset → `verifier.TrustedRepositoryPattern` (release `on-tag.yaml`) | `resolveBinaryAttestationIdentityPattern` (must be confined to `NVIDIA/aicr` — begins with the repository prefix, no top-level alternation, and no match against foreign-identity canaries — validated by `verifier.ValidateIdentityPattern`; retargets the attesting NVIDIA workflow, e.g. an e2e build) |

See [Server-Side Bundle Signing](#server-side-bundle-signing) for the identity
model and validation rules behind these variables.

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

## Server-Side Bundle Signing

`POST /v1/bundle?attest=true` returns a signed bundle. The signing identity is
operator configuration, parsed once at startup by `parseSigningConfig`
([`pkg/server/signing.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/signing.go))
into a `signingConfig`. No field on that struct ever comes from a request: the
server always signs as itself, so the request only chooses whether to sign, not
with what. The handler enforces the trust boundary in `resolveAttestRequest`,
rejecting `attest=true` with HTTP 400 when no identity is configured and
rejecting an unparseable `attest` value with HTTP 400.

**Access control is the operator's responsibility.** The signature attests that
this aicrd deployment produced the bundle (build and tool provenance); it does not
endorse the caller-supplied recipe's contents. `/v1/bundle` accepts arbitrary
recipes and is not itself authenticated, so when signing is enabled the endpoint
must be access-controlled by the deployment (network policy, gateway, or mTLS) to
prevent an untrusted caller from obtaining server-signed bundles. Signing is
opt-in and off by default.

**Two mutually exclusive modes.** Mode A is KMS-backed (`AICR_SIGNING_KEY`, a
cosign KMS URI validated against a scheme allowlist). Mode B is keyless against a
private Sigstore (`AICR_FULCIO_URL` plus a token source: `AICR_IDENTITY_TOKEN_FILE`
or GitHub Actions ambient OIDC). `parseSigningConfig` validates mode exclusivity
and completeness and **fails fast**: an ambiguous config (both modes set), a
malformed KMS URI, a partial keyless config, or a non-boolean `AICR_TLOG_UPLOAD`
(parsed only in KMS mode, where the toggle applies) returns an error from `Serve`
so the server does not start. When no signing variables are set, signing is simply
off and the server starts normally.

**Per-request options are rebuilt, not cached.** `signingConfig.resolveOptions`
constructs the per-request `attestation.ResolveOptions`. For Mode B it reads the
identity-token file **fresh** on every call, because ServiceAccount tokens rotate
and Fulcio binds each certificate to a fresh token.

**Binary attestation cached at startup.** Server signing also requires the aicrd
binary's own attestation (tool provenance): a Sigstore bundle shipped next to the
executable inside the container image, issued under the NVIDIA-CI identity and
bound to the binary's digest. `loadBinaryAttestation` verifies it **once** at
startup (`Serve` calls it after `parseSigningConfig`) and caches the raw bytes on
the `signingConfig`; each signed bundle embeds those bytes as
`attestation/aicr-attestation.sigstore.json`. This is fail-fast too: a signing
server that cannot prove its own provenance must not start. Producing that
attestation in CI/release is a separate dependency, tracked outside this feature.

`resolveBinaryAttestationPath` chooses which file to verify: by default the
conventional path next to the executable (`FindBinaryAttestation`), or the
explicit `AICR_BINARY_ATTESTATION_FILE` override when set. The override exists for
ko-built images, whose assets live under `KO_DATA_PATH`
(`/var/run/ko/aicrd-attestation.sigstore.json`) rather than next to the binary.
Only the attestation file path changes; the digest verified against it is always
the running `os.Executable()` binary's digest.

`resolveBinaryAttestationIdentityPattern` chooses the certificate-identity
pattern the attestation is verified against: `verifier.TrustedRepositoryPattern`
(the release `on-tag.yaml` workflow) by default, or the
`AICR_BINARY_ATTESTATION_IDENTITY_REGEXP` override when set. The override is
validated by `verifier.ValidateIdentityPattern`, which requires it to *begin
with* `https://github.com/NVIDIA/aicr/` (a leading `^` is allowed) and to avoid
top-level alternation, so it can only retarget which NVIDIA workflow attested
the binary (e.g. the server-kms e2e build), never widen the org. Merely
containing `NVIDIA/aicr` is not enough: a pattern that reaches the repository
down one branch and something else down another is rejected. A bad override fails
startup fast. This mirrors the CLI's `--certificate-identity-regexp`, and a
custom pattern is logged because bundles the server then signs will not pass a
verifier using the default identity.

**Injectable seams for tests.** The startup verifier
(`binaryAttestationVerifier`) and the per-request attester builder
(`attesterBuilder`) are function-typed fields so tests can inject fixtures: the
real verifier pins the NVIDIA-CI identity and the running binary's digest, which
a `go test` executable cannot satisfy.

## OpenAPI Parity Test

[`pkg/server/openapi_sync_test.go`](https://github.com/NVIDIA/aicr/blob/main/pkg/server/openapi_sync_test.go)
contains two contract gates:

- `TestOpenAPIEnumsMatchGoTypes` asserts that every criteria-field enum in
  `api/aicr/v1/server.yaml` matches the corresponding
  `pkg/recipe.GetCriteria*Types()` function. It scans both query-parameter enums
  and `components.schemas.Criteria` properties.
- `TestOpenAPIBundleContract` asserts that `POST /v1/bundle` points at
  `BundleRecipeRequest`, that the union's branches stay wired to the legacy and
  configured schemas, and that the header enums stay synchronized with the Go
  constants. It matches the `allOf` branches by content rather than position,
  since `allOf` is semantically unordered.

  Runtime acceptance of the legacy header shapes is pinned separately by
  `TestBundleHandler_LegacyRecipeHeaders`, which posts absent, empty, and
  target-`apiVersion` bodies to the handler, asserts 200, and round-trips the
  emitted `recipe.yaml` back through the file loader to prove the ingest
  normalization holds. The spec gate alone would only be checking the spec
  against itself. The one legacy shape that is *not* accepted, `kind: Recipe`
  (published through v0.18.0 and removed by the v1 collapse), is pinned by
  `TestBundleHandler_RejectsLegacyRecipeKind` so the rejection stays a decision
  rather than becoming an accident of a later refactor.

Drift is a contract bug: clients conforming to the spec will reject
inputs the server actually accepts, or generate types that reject
server outputs. Adding a value to a Go criteria type without updating
the spec — or the reverse — fails CI here.

The wildcard `"any"` is allowed in the spec but not the Go list; the
test strips it before comparison.

## REST Contract Gate

REST is one of the four surfaces [ROADMAP](https://github.com/NVIDIA/aicr/blob/main/ROADMAP.md)
section 1 freezes at v1. Two gates guard it, and they fail for different
reasons.

**`make openapi-diff`** ([`tools/openapi-diff`](https://github.com/NVIDIA/aicr/blob/main/tools/openapi-diff))
compares `api/aicr/v1/server.yaml` against the committed snapshot
`api/aicr/v1/server.baseline.yaml` using the pinned `oasdiff`, and fails on
breaking changes: a removed endpoint, a removed or narrowed field, a new
required request field, a removed enum value. Additive change passes. It runs
in `make qualify`, next to `api-diff`, which guards the Go SDK the same way.

Three ways to resolve a failure, in order of preference:

1. Make the change additive instead.
2. Record it in `api/aicr/v1/openapi-diff-exceptions.yaml` with the oasdiff
   rule id, the operation path, and a reason. **Scheduled ADR-022 apiVersion
   removals belong here** — that is what the issue means by representing a
   transition explicitly rather than disabling the gate.
3. Accept it into the contract with `make openapi-baseline`, which rewrites the
   baseline. That diff is the change under review; read it.

An acknowledgement that stops matching a real breaking change **fails the
gate**. A stale entry silently pre-approves the break returning later, which is
the failure the file exists to prevent — the same contract as
`pkg/client/v1/api-diff-exceptions.yaml`. `tools/openapi-diff_test.sh` pins each
branch of that verdict to an exact exit code.

**`pkg/server/openapi_validity_test.go`** answers a different question: not
"did this change break the contract" but "is the contract coherent". A dangling
`$ref`, an orphaned component, or a duplicate `operationId` is present in both
baseline and spec, so the diff sees no change and passes while every generated
client is wrong. It also guards the baseline against silent truncation, which
would make the diff gate report no breaking changes for anything the truncated
file omits.

The baseline is a committed snapshot rather than the previous release, unlike
`api-diff`. The v1 collapse (#2464) is unreleased, so comparing against v0.20.0
reports 28 breaking changes that are all one already-merged decision — the gate
would ship pre-loaded with noise that clears itself at v0.21 and teaches
everyone to skim it in the meantime.

## Artifact Schema Gate

Artifact schemas are the second of the four surfaces ROADMAP section 1 freezes
at v1 (issue #2113). `api/aicr/v1/schemas/*.schema.json` are JSON Schema
documents for `Snapshot`, `RecipeResult`, `RecipeMetadata`, `RecipeMixin` and
`RecipeCriteria`, generated from the Go types by
[`tools/schemagen`](https://github.com/NVIDIA/aicr/tree/main/tools/schemagen)
and regenerated with `make schemas`.

They are derived rather than authored, so three tests keep them honest and each
fails for a different reason:

- **`TestCommittedSchemasAreFresh`** — the committed files match the current Go
  types. Without it the schemas would drift into a snapshot of whatever the
  types looked like when someone last remembered to run the generator.
- **`TestSchemasDescribeRealArtifacts`** — the 110 committed overlays and 4
  mixins validate against their schema. Freshness proves the schema matches the
  *type*; this proves the type matches what is actually on disk.
- **`TestArtifactSchemasAreCompatible`** — the generated schemas are compared to
  the frozen snapshot in `api/aicr/v1/schemas/baseline/`, failing on a removed
  field, a newly required field, a changed type, a removed enum value, or a
  previously free-form field becoming an enum. Additive change passes.

Intentional breaks go in `api/aicr/v1/schemas/schema-diff-exceptions.yaml` with
a rule, kind, path and reason. **An acknowledgement that matches no reported
break fails the gate** — a stale entry silently pre-approves the break
returning. Same contract as `openapi-diff-exceptions.yaml` and
`api-diff-exceptions.yaml`. Scheduled ADR-022 apiVersion removals are the
expected occupants: retiring an alpha value is an `enum-value-removed` break,
planned and dated.

`make schema-baseline` accepts the current schemas as the frozen contract. That
diff is the change under review.

**`required` means different things for authored and emitted artifacts.**
`RecipeResult` and `Snapshot` are emitted, so a field without `omitempty` is
always written and a consumer may rely on it. `RecipeMetadata`, `RecipeMixin`
and `RecipeCriteria` are authored by hand, where the encoder's behavior says
nothing about what a human must supply — `ComponentRef.Source` is written on
every emit but set by only 17 of the 110 committed overlays. Authored artifacts
therefore declare nothing required; marking them published a schema that
rejected the project's own catalog.

The criteria enums add the `any` wildcard that `GetCriteria*Types()` omits, for
the same reason the OpenAPI parity test strips it before comparing.

**The generator uses reflection rather than a schema library on purpose.** It
must compile against the types, so it cannot be a pinned binary the way
`oasdiff` is; importing a schema library would put it in the module graph, and
therefore in the SBOM and vulnerability surface of the shipped binaries, for
something that only runs at build time. The cost is that `pkg/schema` is
hand-written, so it covers only the shapes the artifacts use and **fails on
anything else** rather than emitting a plausible guess.

## Adding an Endpoint

1. **Edit `api/aicr/v1/server.yaml`.** Add the operation under `paths:`, request and response schemas under `components.schemas`. If the operation accepts criteria, reference `#/components/schemas/Criteria` so the parity test covers it.
2. **Add a facade method.** If new business logic is required, add it to `pkg/client/v1/aicr.go` (or a sibling file in `pkg/client/v1`). The CLI and any external Go caller will use the same method. Handlers must never call into `pkg/recipe`, `pkg/bundler`, etc. directly.
3. **Add the handler.** Create `pkg/server/<name>_handler.go`. Mirror the existing handler shape: method gate, per-handler timeout, parse, allowlist pre-check (if it accepts user input dimensions), bounded body read, facade call, `serializer.RespondJSON` or zip stream, `WriteErrorFromErr` on every error path.
4. **Register the route.** Add an entry to the `map[string]http.HandlerFunc` in `serve.go` (the `WithHandler` argument). The route picks up the full middleware chain automatically.
5. **Wire allowlists if needed.** Pass `allowLists` into the handler constructor and call `validateAgainstAllowLists` before the facade call. Do not invent a parallel allowlist path; reuse `aicr.ToInternalAllowLists`.
6. **Tighten the body cap.** If the endpoint accepts POST bodies and 8 MiB is wrong, define a `defaults.Max<Name>POSTBytes` constant and wrap `r.Body` with `http.MaxBytesReader` inside the handler. Handle `*http.MaxBytesError` explicitly → 413.
7. **Run the contract tests.**
   `go test -run '^(TestOpenAPIEnumsMatchGoTypes|TestOpenAPIV1BundleRecipeContract)$' ./pkg/server/...`.
   Add cases to `openapi_sync_test.go` if you introduced a new enum-bearing
   field or changed the `/v1/bundle` recipe schema.
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

The handlers, middleware chain, and `Server.Run` lifecycle are covered
by in-process Go tests under
[`pkg/server`](https://github.com/NVIDIA/aicr/tree/main/pkg/server)
(`recipe_handler_test.go`, `middleware_test.go`, `serve_test.go`, and
peers), which drive the facade against the embedded data set without a
live cluster.

## References

- [`net/http`](https://pkg.go.dev/net/http) — server, `MaxBytesReader`, `MaxBytesError`
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging used by all middleware
- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `Server.Run` concurrency
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — rate limiter
- [CLAUDE.md](https://github.com/NVIDIA/aicr/blob/main/.claude/CLAUDE.md) — HTTP Server Rules, Error Wrapping Rules, Context Propagation Rules

# API Reference

Complete reference for using the AICR API Server.

## Overview

The AICR API Server provides HTTP REST access to recipe generation and bundle creation for GPU-accelerated infrastructure. Use the API for programmatic access to configuration recommendations and deployment artifacts.

> Version numbers in the sample requests and responses below (server version, chart versions, driver versions) are illustrative. The authoritative, current versions are in the [Component Catalog](component-catalog.md) and the [Container Images BOM](container-images.md).

```
┌──────────────┐      ┌──────────────┐
│ GET /recipe  │─────▶│   Recipe     │
└──────────────┘      └──────────────┘
        │
        ▼
┌──────────────┐      ┌──────────────┐
│ POST /bundle │─────▶│  bundles.zip │
└──────────────┘      └──────────────┘
```

### API vs CLI

- Use the **API** for remote recipe generation and bundle creation
- Use the **CLI** for local operations, snapshot capture, and ConfigMap integration

| Feature | API | CLI |
|---------|-----|-----|
| Recipe generation | ✅ profile- and Slurm-accounting-aware GET/POST `/v1/recipe` | ✅ `aicr recipe` |
| Value query | ✅ profile- and Slurm-accounting-aware GET/POST `/v1/query` | ✅ `aicr query` |
| Bundle creation | ✅ profile- and Slurm-accounting-aware POST `/v1/bundle` | ✅ `aicr bundle` |
| Bundle attestation | ✅ POST `/v1/bundle?attest=true` (server signs as itself) | ✅ `aicr bundle --attest` (interactive or ambient OIDC) |
| Snapshot capture | ❌ Use CLI | ✅ `aicr snapshot` |
| ConfigMap I/O | ❌ Use CLI | ✅ `cm://` URIs |
| Agent deployment | ❌ Use CLI | ✅ `aicr snapshot` |

## Base URL

Local development (example):
```
http://localhost:8080
```

Start the local server:
```shell
docker pull ghcr.io/nvidia/aicrd:latest
docker run -p 8080:8080 ghcr.io/nvidia/aicrd:latest
```

## Quick Start

### Get a Recipe

Generate an optimized configuration recipe for your environment:

```shell
# GET: Basic recipe for H100 on EKS (query parameters)
curl "http://localhost:8080/v1/recipe?accelerator=h100&service=eks"

# GET: Training workload on Ubuntu
curl "http://localhost:8080/v1/recipe?accelerator=h100&service=eks&intent=training&os=ubuntu"

# POST: Recipe from a criteria envelope (YAML body)
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d 'criteria:
  service: eks
  accelerator: h100
  intent: training'

# Save recipe to file
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" -o recipe.json
```

### Generate Bundles

Create deployment bundles from a recipe:

```shell
# Pipe recipe directly to bundle endpoint.
# The POST body must be a fully-hydrated RecipeResult; piping GET /v1/recipe
# output (as below) supplies one. Do not hand-author a partial body.
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# Extract the bundles
unzip bundles.zip -d ./bundles

# Verify the complete extracted inventory before deployment
(cd ./bundles && aicr verify .)
```

## Endpoints

### GET /

Service information and available routes.

```shell
curl "http://localhost:8080/"
```

**Response:**
```json
{
  "service": "aicrd",
  "version": "v0.14.0",
  "routes": [
    "/v1/recipe", "/v1/query", "/v1/bundle"
  ]
}
```

---

### GET /v1/recipe

Generate an optimized configuration recipe based on environment parameters.

**Query Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `service` | string | any | K8s service: `eks`, `gke`, `aks`, `oke`, `ocp`, `kind`, `lke`, `bcm`, `metal3`, `any` |
| `accelerator` | string | any | GPU type: `h100`, `h200`, `gb200`, `gb300`, `b200`, `a100`, `l40`, `l40s`, `rtx-pro-6000`, `any` |
| `gpu` | string | any | Alias for `accelerator` |
| `intent` | string | any | Workload: `training`, `inference`, `any` |
| `os` | string | any | Node OS: `ubuntu`, `rhel`, `cos`, `amazonlinux`, `ol`, `talos`, `any` |
| `platform` | string | any | Platform/framework: `dynamo`, `kubeflow`, `nim`, `runai`, `slurm`, `any` |
| `nodes` | integer | 0 | GPU node count hint (0 = unspecified). Advisory metadata — does not select or filter overlays. |

**Examples:**

```shell
# Minimal request — at least one criteria dimension is required
curl "http://localhost:8080/v1/recipe?accelerator=h100"

# Specify accelerator with service
curl "http://localhost:8080/v1/recipe?service=eks&accelerator=h100"

# Full specification
curl "http://localhost:8080/v1/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&nodes=8"

# Using gpu alias
curl "http://localhost:8080/v1/recipe?gpu=gb200&service=eks&os=ubuntu&intent=training"

# Pretty print with jq
curl -s "http://localhost:8080/v1/recipe?accelerator=h100" | jq '.'
```

---

### POST /v1/recipe

Generate an optimized configuration recipe from a criteria body. This endpoint provides an alternative to query parameters.

**Content Types:**
- `application/json` - JSON format
- `application/x-yaml` - YAML format

An explicit, supported `Content-Type` is required; a missing, aliased, or unsupported media type is rejected.

`GET` and `POST` return **identical documents** for equivalent criteria, including the `criteria` object echoed in the response: unspecified dimensions are reported as `any` on both. `HEAD` is accepted wherever `GET` is and returns the same headers without a body — it resolves the recipe to produce them, so it costs what `GET` costs rather than serving as a cheap probe. Use `/health` or `/ready` for liveness.

**Request Body:**

The request body is a strict envelope. Unknown fields are rejected, so a typo fails loudly instead of being silently ignored:

```yaml
criteria:
  service: eks
  accelerator: gb200
  os: ubuntu
  intent: training
  platform: kubeflow
  nodes: 8
profile: gpuStack=operator-managed  # optional
```

`profile` is optional; omit it to take the composition's declared default. If you also pass `profile=` as a query parameter, the two must agree.

The Kubernetes-style `RecipeCriteria` resource body that this endpoint accepted before v0.21 is no longer supported and returns `400`.

**Examples:**

```shell
# POST with YAML body
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d 'criteria:
  service: eks
  accelerator: h100
  intent: training'

# POST with JSON body
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/json" \
  -d '{
    "criteria": {
      "service": "eks",
      "accelerator": "h100",
      "intent": "training"
    }
  }'

# POST with criteria file
curl -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d @criteria.yaml

# Pretty print response
curl -s -X POST "http://localhost:8080/v1/recipe" \
  -H "Content-Type: application/json" \
  -d '{"criteria":{"service":"eks","accelerator":"h100"}}' \
  | jq '.'
```

**Error Responses:**
- `400 Bad Request` - No criteria provided: at least one of `service`, `accelerator`, `intent`, `os`, `platform`, or `nodes` must be non-zero. An empty request returns `"no criteria provided: specify at least one of service, accelerator, intent, os, platform, nodes"`. This guard applies to `GET /v1/recipe`, `POST /v1/recipe`, `GET /v1/query`, and `POST /v1/query`.
- `400 Bad Request` - Invalid criteria format, missing required fields, or invalid enum values
- `400 Bad Request` - A stated criteria dimension is not honored by any applicable recipe overlay (uncovered dimension). This applies to `GET /v1/recipe`, `POST /v1/recipe`, `GET /v1/query`, and `POST /v1/query`: every dimension you state (`service`, `accelerator`, `intent`, `os`, `platform`) must be matched by at least one applied overlay, or the request fails instead of silently returning a recipe that ignores it. `nodes` is exempt — it is advisory and never required to be covered. The response's `details.uncovered` array names the offending dimension(s), the requested value, and any `validCompletions` (additional criteria that would make the request coverable). Snapshot-driven resolution (CLI `--snapshot` / Go SDK) may additionally attach `excludedOverlays` and `constraintWarnings` to the error; the HTTP API resolves from criteria only and never emits those two fields.
- `405 Method Not Allowed` - Only GET and POST are supported

**Uncovered-Dimension Error Example:**

```json
{
  "code": "INVALID_REQUEST",
  "message": "platform 'kubeflow' for criteria(service=eks, accelerator=h100, intent=training, platform=kubeflow) requires os (valid: ubuntu)",
  "details": {
    "uncovered": [
      {
        "dimension": "platform",
        "requestedValue": "kubeflow",
        "validCompletions": [{"os": "ubuntu"}]
      }
    ]
  },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2025-01-15T10:30:00Z",
  "retryable": false
}
```

**Response:**

```json
{
  "apiVersion": "aicr.run/v1alpha2",
  "kind": "RecipeResult",
  "metadata": {
    "version": "v0.14.0",
    "appliedOverlays": [
      "base",
      "eks",
      "eks-training",
      "gb200-eks-training"
    ],
    "excludedOverlays": [
      {
        "name": "h100-eks-ubuntu-training",
        "reason": "mixin-constraint-failed"
      }
    ],
    "constraintWarnings": [
      {
        "overlay": "h100-eks-ubuntu-training",
        "constraint": "OS.sysctl./proc/sys/kernel/osrelease",
        "expected": ">= 6.8",
        "actual": "5.15.0",
        "reason": "mixin-constraint-failed: expected >= 6.8, got 5.15.0"
      }
    ]
  },
  "criteria": {
    "service": "eks",
    "accelerator": "gb200",
    "intent": "training",
    "os": "any",
    "platform": "any"
  },
  "constraints": [
    {
      "name": "GPU.driver.version",
      "value": "580.82.07"
    },
    {
      "name": "GPU.driver.cudaVersion",
      "value": "13.1"
    }
  ],
  "componentRefs": [
    {
      "name": "gpu-operator",
      "type": "Helm",
      "chart": "gpu-operator",
      "source": "https://helm.ngc.nvidia.com/nvidia",
      "version": "v25.3.3"
    },
    {
      "name": "network-operator",
      "type": "Helm",
      "chart": "network-operator",
      "source": "https://helm.ngc.nvidia.com/nvidia",
      "version": "v25.4.0"
    }
  ],
  "deploymentOrder": [
    "gpu-operator",
    "network-operator"
  ]
}
```

`metadata.excludedOverlays` is optional. When present, each entry includes the overlay `name` and a machine-readable `reason` such as `constraint-failed` or `mixin-constraint-failed`.

`metadata.gpuDriverState` is optional and appears only for snapshot-driven recipes. It records the NVIDIA kernel driver state observed on the sampled GPU node — `preinstalled` or `absent` — and is omitted when no snapshot was provided or the snapshot carried no usable driver-loaded reading. The bundle-time `CheckDriverOwnershipCoherence` validation consumes it: a recipe whose snapshot observed no driver (`absent`) is blocked from bundling with the preinstalled-driver assumption, since that would leave GPU nodes driverless.

`metadata.mariaDBOperatorState` is optional and appears when a snapshot supplies MariaDB Operator conflict evidence during resolution of AICR-provided Slurm accounting. It records `absent`, `api-detected`, `crs-detected`, or `unknown`; query-generated recipes and older snapshots without the collector subtype omit the field. Recipe generation remains observational: `api-detected`, `crs-detected`, and `unknown` emit warnings but still produce a recipe. At bundle time, `crs-detected` and `unknown` block AICR-provided installation, while `api-detected` or omitted evidence warns but proceeds; `absent` proceeds silently.

---

### GET /v1/query

Query a specific value from a fully hydrated recipe. Resolves a recipe from criteria (same parameters as GET /v1/recipe), merges all base, overlay, and inline overrides, then returns the value at the given selector path.

**Query Parameters:**

All GET /v1/recipe parameters are supported, plus:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `selector` | string | Yes | Dot-delimited path to the value to extract (e.g. `components.gpu-operator.values.driver.version`). Empty string returns the entire hydrated recipe. |

**Response:**

- **Scalar values** (string, number, bool) are returned as plain JSON values
- **Complex values** (maps, lists) are returned as JSON objects/arrays

**Error Responses:**

- `400 Bad Request` - No criteria provided: at least one criteria dimension (`service`, `accelerator`, `intent`, `os`, `platform`, or `nodes`) must be non-zero. Returns `"no criteria provided: specify at least one of service, accelerator, intent, os, platform, nodes"`. See the [POST /v1/recipe error responses](#post-v1recipe) entry for full details.
- `400 Bad Request` - Omitting `selector` returns `INVALID_REQUEST`. An explicitly empty `selector=` remains valid and returns the entire hydrated recipe.
- `400 Bad Request` - A stated criteria dimension not honored by any applicable overlay (uncovered dimension) — same `details.uncovered` shape as described in the [POST /v1/recipe error responses](#post-v1recipe) above.

**Examples:**

```shell
# Get a specific Helm value
curl -s "http://localhost:8080/v1/query?service=eks&accelerator=h100&intent=training&selector=components.gpu-operator.values.driver.version"

# Get deployment order
curl -s "http://localhost:8080/v1/query?service=eks&accelerator=h100&intent=training&selector=deploymentOrder" | jq '.'

# Get a component subtree
curl -s "http://localhost:8080/v1/query?service=eks&accelerator=h100&selector=components.gpu-operator.values.driver" | jq '.'
```

---

### POST /v1/query

Alternative to `GET /v1/query` that accepts the criteria and selector in the request body. The body is a strict envelope with a `criteria` object (the same dimensions accepted as query parameters on `GET /v1/query`), a required `selector` string, and an optional `profile`. Unknown fields are rejected.

**Content Types:**
- `application/json` - JSON format
- `application/x-yaml` - YAML format

**Request Body:**

```yaml
criteria:
  service: eks
  accelerator: h100
  intent: training
selector: "components.gpu-operator.values.driver.version"
```

**Examples:**

```shell
curl -X POST "http://localhost:8080/v1/query" \
  -H "Content-Type: application/json" \
  -d '{
    "criteria": {"service": "eks", "accelerator": "h100", "intent": "training"},
    "selector": "components.gpu-operator.values.driver.version"
  }'
```

The response format matches `GET /v1/query`: scalar values are returned as plain JSON values; maps and lists are returned as JSON objects/arrays.

**Error Responses:**

Same as `GET /v1/query` — see the [GET /v1/query error responses](#get-v1query) section above. The no-criteria and uncovered-dimension 400 cases apply. `selector` must be present on both GET and POST; omitting it is a 400. Passing it explicitly empty (`selector=` on GET, `"selector": ""` in a POST envelope) is the documented way to ask for the entire hydrated recipe.

---

### Profile and Slurm-accounting endpoints

The `v1` in the route and the `apiVersion` in a recipe document are
independent version axes. The route segment versions the HTTP contract;
`aicr.run/v1alpha2` and `aicr.run/v1alpha3` are the recipe schemas emitted by
this reader-first release. `/v1/bundle` also accepts their ADR-022 targets:
`aicr.run/v1` for a default recipe and `aicr.run/v1beta2` for a
profile/configuration recipe, plus versionless legacy artifacts. Selecting a
profile or resolving a Slurm accounting mode determines which schema track
applies.

There is a single route family. `/v1/recipe`, `/v1/query`, and `/v1/bundle`
serve every composition — profiled and unprofiled alike — with one contract.
The AKS and GKE families are the embedded profile adopters (`gpuStack`) and
need no special routing.

**GET `/v1/recipe`.** Accepts the `/v1/recipe` criteria parameters plus
optional `profile=name=value` and `slurmAccountingMode`. Profile omission
applies the resolved declaration's required default. Slurm accounting accepts
`disabled`, `customer-managed`, or `aicr-provided`; omission defaults a Slurm
recipe to `disabled`. The setting is recorded at
`configuration.slurm.accounting.mode` in an `aicr.run/v1alpha3` RecipeResult.
The route rejects unknown query parameters and conflicting repeated values.

```shell
# AKS, non-default value (omit profile= for the azure-managed default):
curl "http://localhost:8080/v1/recipe?service=aks&accelerator=h100&os=ubuntu&intent=training&profile=gpuStack=operator-managed"

# Slurm with AICR-provided accounting
curl "http://localhost:8080/v1/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm&slurmAccountingMode=aicr-provided"
```

**POST `/v1/recipe`.** Accepts a strict JSON or YAML envelope. `criteria` is
the plain criteria object, not a `RecipeCriteria` resource. Profile selection
may be supplied in the envelope, as the `profile` query parameter, or in both
places when the values agree. `slurmAccountingMode` is supplied as the same
query parameter used by GET. Conflicting selections are rejected:

```yaml
criteria:
  service: aks
  accelerator: h100
  intent: training
profile: gpuStack=azure-managed
```

Unknown fields, duplicate or trailing documents, malformed selections, and
selections against an unprofiled composition fail with `400
INVALID_REQUEST`. POST envelopes require `Content-Type: application/json` or
`Content-Type: application/x-yaml`; missing, aliased, or unsupported media
types are rejected.

**GET and POST `/v1/query`.** GET accepts the recipe parameters, including
`slurmAccountingMode`, plus
`selector`. POST accepts the same strict envelope with a required selector.
POST profile selection follows the same query/envelope agreement rule as
`/v1/recipe`:

```yaml
criteria:
  service: aks
  accelerator: h100
profile: gpuStack=azure-managed
selector: metadata.selectedProfile
```

**POST `/v1/bundle`.** Uses the same query parameters and ZIP response as
`POST /v1/bundle`. It carries no profile-selection field because its body is
an already-selected `RecipeResult`. It accepts legacy
`aicr.run/v1alpha2` and `aicr.run/v1` default recipes, including older
artifacts that omit `apiVersion`, and strictly decodes profiled or
accounting-configured `aicr.run/v1alpha3` and `aicr.run/v1beta2` recipes. The
request requires `Content-Type: application/json` or `Content-Type:
application/x-yaml`; missing, aliased, or unsupported media types are
rejected.

```shell
# The AKS and GKE families are the embedded adopters (gpuStack profiles).
# -f stops on an HTTP error so a 4xx/5xx recipe body is never staged and
# an error response is never written to bundles.zip. POSIX sh suffices:
# the commands are sequential (no pipeline), so pipefail is not needed.
set -eu
curl -fsS -o recipe.json \
  "http://localhost:8080/v1/recipe?service=aks&accelerator=h100&os=ubuntu&intent=training&profile=gpuStack=operator-managed"
curl -fsS -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" -d @recipe.json -o bundles.zip
```

Profile-bearing responses record `metadata.selectedProfile`; accounting-aware
responses record `configuration.slurm.accounting`. Both use recipe apiVersion
`aicr.run/v1alpha3`. Their owned paths are immutable across AICR's supported
override surfaces: divergent static values, intersecting dynamic paths,
owned-component removal, and argocd-helm install-time values fail closed before
output.

A recipe resolved without an explicit profile or `slurmAccountingMode` uses
the default-track response shape. That track is `aicr.run/v1alpha2` today, and
the schema also admits its ADR-022 target `aicr.run/v1` so a client generated
from this spec tolerates the value a release before AICR emits it. Profile and
Slurm-accounting selection are available on every endpoint; no composition
needs special routing.

The AKS and GKE families are the embedded profile adopters (`gpuStack`). Their
compositions resolve like any other: omit `profile=` to take the declared
default, or select one explicitly.

```shell
# GKE: omit profile= for the gke-default default, or select
# gpuStack=bundle-installer explicitly, then POST to /v1/bundle.
# -f stops on an HTTP error so a 4xx/5xx recipe body is never staged and
# an error response is never written to bundles.zip.
set -euo pipefail
curl -fsS -o recipe.json \
  "http://localhost:8080/v1/recipe?service=gke&accelerator=h100&os=cos&intent=training&profile=gpuStack=bundle-installer"
curl -fsS -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" -d @recipe.json -o bundles.zip
```

---

### POST /v1/bundle

Generate deployment bundles from a recipe.

**Query Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `bundlers` | string | (all) | Comma-delimited list of recipe component names to bundle (e.g. `gpu-operator,network-operator`). Whitespace around names is trimmed. Components not listed are skipped as if disabled (their dependency edges are treated as satisfied externally). A name the recipe does not declare, or one that is disabled (by the recipe or a `set` `enabled=false` override), is rejected with HTTP 400. |
| `set` | string[] | | Value overrides (format: `bundler:path.to.field=value`). Repeat for multiple. The reserved prefix `deployer:` carries Argo CD Application options for `deployer=argocd` and `deployer=argocd-helm` (`namePrefix`, `destinationServer`, `project`, `cascadeDelete`), e.g. `set=deployer:namePrefix=tenant-a-`. Unknown `deployer:` keys — or the prefix with any other deployer — are rejected with HTTP 400. An override whose component is absent from the generated bundle is rejected with HTTP 400 (`INVALID_REQUEST`, "cannot take effect") rather than silently discarded; the scalar `enabled=false` spelling is exempt on a declared component. See [Overrides that cannot take effect are rejected](bundling.md#overrides-that-cannot-take-effect-are-rejected) and the CLI reference's [Argo CD Deployer Options](cli-reference.md#argo-cd-deployer-options) for full semantics. |
| `dynamic` | string[] | | Declare value paths as install-time parameters (format: `component:path.to.field`). Repeat for multiple. Supported with `deployer=helm`, `deployer=argocd-helm`, `deployer=flux`, and `deployer=helmfile`. A declaration whose component is absent from the generated bundle is rejected with HTTP 400 (`INVALID_REQUEST`, "cannot take effect"); no path is exempt. Certain gate- or contract-owned paths on **present** components are also rejected — driver-ownership paths, GPU allocation-policy keys, the DRA eviction paths `kubeletPlugin.nodeSelector` and `driver.manager.env` when both contract components are enabled **and** `dra-eviction-node-label` is supplied, and, where the corresponding NVSentinel gate applies on the recipe's platform and configuration, the NVSentinel remedy/consumer/runtime-class paths; see [NVSentinel on provider-installed-driver platforms](component-catalog.md#nvsentinel-on-provider-installed-driver-platforms). See [Overrides that cannot take effect are rejected](bundling.md#overrides-that-cannot-take-effect-are-rejected). |
| `system-node-selector` | string[] | | Node selectors for system components (format: `key=value`). Repeat for multiple. |
| `system-node-toleration` | string[] | | Tolerations for system components (format: `key=value:effect`). Repeat for multiple. |
| `accelerated-node-selector` | string[] | | Node selectors for GPU nodes (format: `key=value`). Repeat for multiple. |
| `accelerated-node-toleration` | string[] | | Tolerations for GPU nodes (format: `key=value:effect`). Repeat for multiple. |
| `dra-eviction-node-label` | string | _(none)_ | Opt in to DRA kubelet-plugin eviction coordination with GPU Operator driver upgrades (format: `key=value`). Unset means AICR injects nothing and the plugin needs no extra node label. When set, and both components are enabled, nodes used for DRA GPU allocation must carry the same label. |
| `nodes` | int | 0 | Estimated number of GPU nodes (0 = unset). Written to Helm value paths declared in the registry under `nodeScheduling.nodeCountPaths`. |
| `vendor-charts` | bool | false | Pull upstream Helm chart bytes into the bundle at bundle time so the artifact is fully self-contained and air-gap deployable. Each vendored chart is recorded in `provenance.yaml` with name, version, source URL, and SHA256. Trades the upstream CVE-yank fail-loud signal for offline deployability — see the CLI reference's "Vendoring Charts for Air-Gap" section for the full tradeoff. Requires the `helm` binary on the API server's `$PATH`. **The server-side vendor path is opt-in and off by default** — the operator must set `AICR_ALLOW_VENDOR_CHARTS=true`, otherwise `vendor-charts=true` returns `400 vendor-charts is not enabled on this server`. Even when enabled, repository hosts that resolve to loopback, link-local, private, or cloud-metadata IPs are rejected with `400 INVALID_REQUEST`, and vendored artifacts are capped at 64 MiB. **Private HTTP(S) repository credentials:** the aicrd pre-check sends `HELM_REPOSITORY_USERNAME`/`HELM_REPOSITORY_PASSWORD` (as HTTP Basic auth) ONLY when `AICR_HELM_REPOSITORY_HOST` is set to that repository's exact host, the request scheme is `https`, and the request host matches (case-insensitive). All three conditions must hold — an operator setting only the username/password env vars will get no credentials attached, preventing a caller-supplied `Repository` URL from harvesting the operator's helm credentials. (Note: the upstream `helm pull --repo` subprocess does not itself read these env vars — private HTTP repos require a prior `helm repo add --username --password` in the aicrd image or an SDK-based puller.) OCI credentials flow through the standard docker config (`~/.docker/config.json` or `$DOCKER_CONFIG`), exactly like `helm pull oci://...`. If prerequisites are missing the request fails with a structured error code (`SERVICE_UNAVAILABLE` / HTTP 503 for missing helm). The index pre-check surfaces upstream HTTP status by class: `404` → `NOT_FOUND` / HTTP 404, `401`/`403` → `UNAUTHORIZED` / HTTP 401, `408`/`429` → `SERVICE_UNAVAILABLE` / HTTP 503 (retryable), other `4xx` → `INVALID_REQUEST` / HTTP 400, `5xx` → `SERVICE_UNAVAILABLE` / HTTP 503. |
| `serial` | bool | false | Sequence components strictly one at a time in deployment order, disabling the parallel rollout of independent components. Affects `deployer=argocd`, `argocd-helm`, `flux`, and `helmfile` (`helm` is already serial): argocd falls back to a linear sync-wave per folder, flux chains each `HelmRelease` `dependsOn` to the previous component, and helmfile chains every release via `needs:` into one linear apply order. An escape hatch for reproducing the pre-parallelism ordering or bisecting a rollout. |
| `deployer` | string | helm | Deployment method: `helm`, `argocd`, `argocd-helm`, `flux`, or `helmfile` |
| `repo` | string | | Git repository URL for GitOps deployments (used with `deployer=argocd` and `deployer=flux`; ignored by `deployer=argocd-helm`) |
| `app-name` | string | | Parent Argo Application name (default: `aicr-stack` for `deployer=argocd-helm`, `nvidia-stack` for `deployer=argocd`). Must be a DNS-1123 subdomain. Required when deploying multiple non-overlapping AICR bundles to the same Argo CD namespace so the parent Applications do not collide. For `deployer=argocd-helm`, the value is the chart default and can still be overridden at install time via `helm install --set appName=...`. Rejected with HTTP 400 on other deployers. |
| `attest` | bool | false | Return a cryptographically signed bundle. When `true`, the server signs the bundle as **itself** using its operator-configured signing identity; no signing key, token, or identity is ever taken from the request. Parsed with Go `strconv.ParseBool` semantics; a present-but-unparseable value is rejected with HTTP 400. Absent or `false` returns an unsigned bundle. If the server has no signing identity configured, `attest=true` is rejected with HTTP 400 (`Server is not configured for attestation`). See [Server-Side Signing](#server-side-signing) for setup. |

**Request Body:**

The request body is the recipe (`RecipeResult`) directly. No wrapper object is
needed. This release emits `apiVersion: aicr.run/v1alpha2` or
`aicr.run/v1alpha3` and `kind: RecipeResult`; its bundle readers additionally
accept `aicr.run/v1` and `aicr.run/v1beta2`, respectively. The profile track identifies
recipes carrying `metadata.selectedProfile`, typed
`configuration.slurm.accounting`, or both; profile-bearing artifacts must use
`/v1/bundle`. New clients should preserve the version emitted by recipe
resolution.

For backward compatibility, the endpoint also accepts:

- Legacy artifacts that omit `apiVersion` or `kind`, or carry them as empty
  strings after a decode/remarshal round trip.
- The `kind: Recipe` value this contract published through v0.18.0.

All three shapes reach the bundler identically: the endpoint normalizes `kind`
on ingest, stamping `kind: RecipeResult` when the request carries an absent,
empty, or legacy `Recipe` kind. The generated bundle's `recipe.yaml`
therefore always carries the canonical `kind` and reloads through
`aicr bundle -r`, `aicr validate -r`, and the tooling that reads a bundle's
`recipe.yaml` (TestGrid publication, evidence synthesis). Only `kind` is
rewritten — a request that omits `apiVersion` still produces an artifact with
an empty `apiVersion`, which every reader accepts as the legacy shape.

Any other `kind` is rejected with a 400, so the endpoint never emits an artifact
it would refuse to read back. This matches the `/v1/bundle` decode path, and the
CLI file loader for the same values — `aicr bundle -r` accepts a
`RecipeMetadata` file as an *overlay* to hydrate, but as a hydrated
`RecipeResult` artifact it too accepts only `RecipeResult` or an absent kind.
`apiVersion` is validated separately, as described next.

The shared artifact gate rejects any `apiVersion` outside
`aicr.run/v1alpha2`, `aicr.run/v1`, `aicr.run/v1alpha3`, and
`aicr.run/v1beta2` with a 400, on this endpoint as well as on the CLI file-load
path. An absent or empty `apiVersion` is still admitted as the legacy shape on
`RecipeResult` inputs through v0.22, and v0.23 stops admitting it along with the
alpha values. The tolerance is scoped to `RecipeResult`, which predates the
field: a `RecipeMetadata` overlay is a catalog document however it arrives, so
`aicr bundle -r` and `aicr validate -r` reject a headerless one exactly as a
`--data` catalog scan does. The reader and emitter clocks are separate: v0.21
and v0.22 both read the alpha values, the target values, and the empty header,
while generated recipes keep their alpha headers until v0.22 switches the
emitters. See
[Catalog and binary compatibility](../integrator/data-extension.md#catalog-and-binary-compatibility)
for the release-by-release table.

#### Components

These are the recipe **components** in [`recipes/registry.yaml`](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml) — the names the `bundlers` query parameter accepts (a request may only name components the recipe declares). The registry is the authoritative source — see the [component catalog](component-catalog.md) for detailed descriptions, pinned versions, and per-component caveats.

| Component | Description |
|-----------|-------------|
| `agentgateway` | Kubernetes Gateway API implementation for AI/ML inference (InferencePool routing) |
| `agentgateway-crds` | Kubernetes Gateway API CRDs for AI/ML inference (Gateway API + Inference Extension) |
| `aws-ebs-csi-driver` | Amazon EBS CSI driver (EKS) |
| `aws-efa` | AWS Elastic Fabric Adapter device plugin (EKS) |
| `cert-manager` | TLS certificate management |
| `cert-manager-ocp` | cert-manager variant for OpenShift (OCP) |
| `cert-manager-ocp-olm` | cert-manager for OpenShift via Operator Lifecycle Manager (OLM) |
| `dynamo-platform` | NVIDIA Dynamo inference serving platform |
| `gatekeeper` | OPA Gatekeeper policy controller |
| `gke-nccl-tcpxo` | NCCL TCPXO network plugin for optimized collective communication (GKE) |
| `gpu-operator` | NVIDIA GPU Operator — driver and runtime lifecycle |
| `gpu-operator-ocp` | GPU Operator variant for OpenShift (OCP) |
| `gpu-operator-ocp-olm` | GPU Operator for OpenShift via Operator Lifecycle Manager (OLM) |
| `grove` | Dynamo pod lifecycle management |
| `k8s-aibom` | Optional runtime AI workload inventory — namespace-scoped CycloneDX ML-BOM resources |
| `k8s-ephemeral-storage-metrics` | Ephemeral storage usage metrics |
| `k8s-nim-operator` | NVIDIA NIM Operator for inference microservice deployments |
| `k8s-nim-operator-ocp` | NIM Operator variant for OpenShift (OCP) |
| `kai-scheduler` | DRA-aware gang scheduler with topology-aware placement |
| `kube-prometheus-stack` | Prometheus, Grafana, Alertmanager monitoring stack |
| `kubeflow-trainer` | Kubeflow Training Operator for distributed training |
| `kueue` | Kubernetes-native job queuing for batch and AI workloads |
| `mariadb-operator` | Official MariaDB Operator; installed only for AICR-provided Slurm accounting |
| `mariadb-operator-crds` | Official MariaDB Operator CRDs; installed only for AICR-provided Slurm accounting |
| `network-operator` | NVIDIA Network Operator — RDMA, SR-IOV, host networking |
| `network-operator-ocp` | Network Operator variant for OpenShift (OCP) |
| `network-operator-ocp-olm` | Network Operator for OpenShift via Operator Lifecycle Manager (OLM) |
| `nfd` | Node Feature Discovery — labels nodes with hardware features; publishes per-node `NodeResourceTopology` CRDs on production GPU recipes |
| `nfd-ocp` | Node Feature Discovery variant for OpenShift (OCP) |
| `nfd-ocp-olm` | Node Feature Discovery for OpenShift via Operator Lifecycle Manager (OLM) |
| `nodewright-customizations` | Environment-specific node tuning profiles |
| `nodewright-operator` | OS-level node tuning and kernel configuration |
| `nvidia-dra-driver-gpu` | Dynamic Resource Allocation driver for GPUs |
| `nvidia-dra-driver-gpu-ocp` | DRA GPU driver variant for OpenShift (OCP) |
| `nvsentinel` | GPU health monitoring; remediation components off by default |
| `prometheus-adapter` | Custom metrics for HPA scaling |
| `prometheus-adapter-ocp` | Prometheus Adapter variant for OpenShift (OCP) |
| `prometheus-operator-crds` | CRDs for the prometheus-operator (`Alertmanager`, `Prometheus`, `ServiceMonitor`, etc.) |
| `slinky-slurm` | Slinky-managed Slurm cluster instance (Controller, LoginSet, NodeSet, RestApi); reconciled by `slinky-slurm-operator` |
| `slinky-slurm-operator` | SchedMD Slinky Slurm operator and admission webhook |
| `slinky-slurm-operator-crds` | CRDs for the SchedMD Slinky Slurm operator (`slinky.slurm.net`) |
| `slinky-topograph` | Generates Slurm `topology.conf` from cloud topology APIs for topology-aware Slinky placement |
| `slurm-accounting-mariadb` | Installation-managed MariaDB instance and Secret generation contract for Slurm accounting; installed only for AICR-provided Slurm accounting |

**Examples:**

> **Note:** The POST body must be a **fully-hydrated** `RecipeResult` — the
> server adopts the body as-is and does **not** hydrate registry defaults, so a
> hand-authored partial body (missing `namespace`, `valuesFile`, `overrides`,
> `dependencyRefs`) yields empty values and namespaces in the generated bundle.
> Obtain a complete body from `aicr recipe ... --format json --output -` (the
> CLI defaults to YAML, but `POST /v1/bundle` JSON-decodes its body) or `GET /v1/recipe`
> and pass it unchanged. The inline bodies below are **elided** for brevity (only
> a few component fields shown) — use a generated `RecipeResult`, not these
> literals.
>
> To bundle a subset of the recipe's components, use the `bundlers` query
> parameter (e.g. `?bundlers=gpu-operator,network-operator`) rather than
> hand-trimming `componentRefs` — trimming the body silently drops required
> dependencies and breaks deployers like Helmfile on dangling
> `dependencyRefs`. The filter prunes those edges safely (a filtered-out
> dependency is assumed satisfied externally) and rejects unknown or disabled
> component names with HTTP 400.
> Slurm accounting adds required-component checks: customer-managed mode
> requires `slinky-slurm`, while AICR-provided mode also requires
> `mariadb-operator-crds`, `mariadb-operator`, and
> `slurm-accounting-mariadb`. A `bundlers` filter that omits any required
> component is rejected with HTTP 400; required components are not
> automatically added to the selection.
>
> Enabled Helm refs must reference a deployable primary: an external chart
> (a `source` repository plus an effective `version` — empty, whitespace-only,
> or a bare `v` is rejected; the chart name falls back to the component name
> when `chart` is unset, but a `chart` without a `source` is rejected) or
> local primary `manifestFiles`. `chart`, `source`, and `version` values
> carrying surrounding whitespace are rejected — deployers consume them
> verbatim.
> Incoherent refs are rejected with HTTP 400 naming the component.
> Component ref names must also be unique within a recipe (enabled or
> disabled refs) when non-empty; a duplicate non-empty name is rejected
> with HTTP 400 naming the conflicting positions. Refs with an empty
> name are exempt from the uniqueness check.

```shell
# Basic: pipe recipe to bundle
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# Advanced: with value overrides and Argo CD deployer
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle?deployer=argocd&repo=https://github.com/my-org/my-gitops-repo.git&set=gpuoperator:gds.enabled=true" \
    -H "Content-Type: application/json" -d @- -o bundles.zip

# With node scheduling for system and GPU nodes
# (recipe.json must be a fully-hydrated RecipeResult, e.g. from GET /v1/recipe)
curl -X POST "http://localhost:8080/v1/bundle?system-node-selector=nodeGroup=system&system-node-toleration=dedicated=system:NoSchedule&accelerated-node-selector=nvidia.com/gpu.present=true&accelerated-node-toleration=nvidia.com/gpu=present:NoSchedule" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Override the shared DRA eviction label when bundling DRA with GPU Operator
curl -X POST "http://localhost:8080/v1/bundle?dra-eviction-node-label=example.com%2Fdra-ready%3Denabled" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Generate bundles from a saved (fully-hydrated) recipe
curl -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Elided literal body (NOT complete — use a generated RecipeResult instead)
curl -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" \
  -d '{
    "apiVersion": "aicr.run/v1alpha2",
    "kind": "RecipeResult",
    "componentRefs": [
      {"name": "gpu-operator", "type": "Helm", "chart": "gpu-operator", "source": "https://helm.ngc.nvidia.com/nvidia", "version": "v26.7.0", "namespace": "gpu-operator", "valuesFile": "components/gpu-operator/values.yaml"},
      {"name": "network-operator", "type": "Helm", "chart": "network-operator", "source": "https://helm.ngc.nvidia.com/nvidia", "version": "26.1.1", "namespace": "nvidia-network-operator", "valuesFile": "components/network-operator/values.yaml"}
    ],
    "deploymentOrder": ["gpu-operator", "network-operator"]
  }' \
  -o bundles.zip
```

**Response Headers:**

| Header | Description | Example |
|--------|-------------|---------|
| `Content-Type` | Always `application/zip` | `application/zip` |
| `Content-Disposition` | Download filename | `attachment; filename="bundles.zip"` |
| `X-Bundle-Files` | Number of verified regular files streamed into the archive | `10` |
| `X-Bundle-Size` | Aggregate uncompressed bytes of those verified regular files | `45678` |
| `X-Bundle-Duration` | Generation time | `1.234s` |

Before writing the response, the server stages a private, revalidated
closed-world inventory. The ZIP contains only the inventory-derived
directories and regular files, including `recipe.yaml` when present;
unverified entries are rejected rather than archived. `X-Bundle-Files` and
`X-Bundle-Size` are derived from that same frozen inventory.

#### Bundle Structure

```text
bundles.zip
├── deploy.sh                    # root automation script (executable)
├── README.md                    # root deployment guide
├── checksums.txt                # SHA256 for every regular payload file in the archive
├── recipe.yaml                  # canonical post-resolution recipe (helm deployer)
├── 001-<component>/             # per-component folder (NNN-prefixed)
│   ├── install.sh               # component install script
│   ├── values.yaml              # static Helm values
│   ├── cluster-values.yaml      # per-cluster dynamic values
│   └── upstream.env             # CHART/REPO/VERSION (upstream-helm only)
└── 002-<component>/
    ├── install.sh
    ├── values.yaml
    └── cluster-values.yaml
```

Checksums are root-level only; component folders carry `install.sh` at their
root (no `scripts/` subdirectory), and no `uninstall.sh`/`undeploy.sh` is
generated. After extraction, `aicr verify .` performs full closed-world
verification: every manifest digest must match and every additional file or
directory, symlink, or other non-regular object is rejected, except the exact
allowed inventory metadata paths.

#### Server-Side Signing

Pass `?attest=true` to `POST /v1/bundle` to receive a cryptographically signed
bundle. The server signs the bundle as **itself** using an operator-configured
signing identity. This is the trust boundary: no signing key, token, or identity
is ever taken from the request, so any client that can reach the endpoint gets
bundles signed under the server's identity, never its own.

A signed bundle additionally carries, inside the returned zip:

```text
attestation/bundle-attestation.sigstore.json   # signature over the bundle
attestation/aicr-attestation.sigstore.json     # aicrd tool-provenance attestation
```

`attest=true` requires a configured signing identity. If none is configured the
request is rejected with HTTP 400 (`Server is not configured for attestation`).
An unparseable `attest` value is also HTTP 400. Absent or `false` returns an
unsigned bundle, as before.

The server supports two mutually exclusive signing modes, selected by
environment variables at startup. The configuration is validated fail-fast: a
malformed or ambiguous setting stops the server from starting.

**Mode A: KMS key.** Set `AICR_SIGNING_KEY` to a cosign KMS URI. The bundle is
signed with a long-lived key held in the KMS; no OIDC identity is involved.

**Mode B: keyless against a private Sigstore.** Set `AICR_FULCIO_URL` to a
private Fulcio CA plus a token source. The server obtains a short-lived signing
certificate from Fulcio using its own OIDC identity. Operator setup for Mode B:

- Run a private Fulcio that trusts the cluster's ServiceAccount token issuer.
- Mount a projected ServiceAccount token with audience `sigstore` into the aicrd
  pod, and point `AICR_IDENTITY_TOKEN_FILE` at it. The token is read fresh for
  every signed request because ServiceAccount tokens rotate.
- Alternatively, when aicrd itself runs inside GitHub Actions, its ambient OIDC
  environment is used as the token source.

| Variable | Mode | Purpose |
|----------|------|---------|
| `AICR_SIGNING_KEY` | A | cosign KMS URI (`awskms://`, `gcpkms://`, `azurekms://`, `hashivault://`). Its presence selects Mode A. |
| `AICR_FULCIO_URL` | B | Private Fulcio CA endpoint. Its presence (with a token source) selects Mode B. |
| `AICR_IDENTITY_TOKEN_FILE` | B | Path to the server's OIDC token (projected ServiceAccount token, audience `sigstore`). Read fresh per request. |
| `AICR_REKOR_URL` | A, B | Rekor transparency-log endpoint override. |
| `AICR_SIGNING_CONFIG_PATH` | A, B | Sigstore SigningConfig JSON for Rekor v2 targeting. |
| `AICR_TLOG_UPLOAD` | A | Set `false` to skip the Rekor upload for air-gapped KMS signing. KMS-only; keyless always uploads. |
| `AICR_BINARY_ATTESTATION_FILE` | A, B | Absolute path to the aicrd binary attestation. Unset defaults to the conventional `<executable>-attestation.sigstore.json` next to the running binary. Set it when the attestation ships elsewhere in the image, e.g. a ko build stages assets under `KO_DATA_PATH` (`/var/run/ko/aicrd-attestation.sigstore.json`) rather than next to the binary. |
| `AICR_BINARY_ATTESTATION_IDENTITY_REGEXP` | A, B | Certificate-identity pattern the server pins its own binary attestation to. Unset uses the release-workflow default (`on-tag.yaml`). A custom value MUST begin with `https://github.com/NVIDIA/aicr/` (leading `^` allowed) and must not use top-level alternation, so it stays confined to the NVIDIA repository; it retargets which NVIDIA workflow attested the binary (e.g. an e2e workflow), not the org, and a value that is not so pinned fails startup. Mirrors the CLI's `--certificate-identity-regexp`. |

Setting both `AICR_SIGNING_KEY` and the keyless variables is ambiguous and the
server refuses to start.

Server signing also requires the aicrd **binary attestation**
(`aicrd-attestation.sigstore.json`, issued under the NVIDIA-CI identity and
bound to the aicrd binary digest) to be shipped inside the container image. The
server verifies it once at startup and embeds it as tool provenance
(`attestation/aicr-attestation.sigstore.json`) in every signed bundle. If
signing is enabled but that attestation is missing or invalid, the server fails
to start. Producing that attestation in the CI/release pipeline is a separate
dependency, tracked outside this feature.

By default the server discovers that attestation next to its own executable. Set
`AICR_BINARY_ATTESTATION_FILE` to point at an explicit path when the image stages
it elsewhere: a ko-built image places assets under `KO_DATA_PATH`
(`/var/run/ko/aicrd-attestation.sigstore.json`), not next to the binary. Only the
attestation file path changes; it is still verified against the aicrd binary's
own digest.

---

### GET /health

Service health check (liveness probe).

```shell
curl "http://localhost:8080/health"
```

**Response:**
```json
{
  "status": "healthy",
  "timestamp": "2026-01-11T10:30:00Z"
}
```

---

### GET /ready

Service readiness check (readiness probe).

```shell
curl "http://localhost:8080/ready"
```

**Response:**
```json
{
  "status": "ready",
  "timestamp": "2026-01-11T10:30:00Z"
}
```

---

### GET and HEAD /metrics

Prometheus metrics endpoint. `HEAD` returns the same headers with no body;
every other method is rejected with `405` and an `Allow: GET, HEAD` header.

```shell
curl "http://localhost:8080/metrics"
```

**Key Metrics:**

| Metric | Type | Description |
|--------|------|-------------|
| `aicr_http_requests_total` | counter | Total HTTP requests by method, path, status |
| `aicr_http_request_duration_seconds` | histogram | Request latency distribution |
| `aicr_http_requests_in_flight` | gauge | Current concurrent requests |
| `aicr_rate_limit_rejects_total` | counter | Rate limit rejections |

## Complete Workflow Example

Fetch a recipe and generate bundles in one workflow:

```shell
#!/bin/bash

# Step 1: Get recipe for H100 on EKS for training
echo "Fetching recipe..."
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks&intent=training" \
  -o recipe.json

# Display recipe summary
echo "Recipe components:"
jq -r '.componentRefs[] | "  - \(.name): \(.version)"' recipe.json

# Step 2: Generate bundles from recipe (pipe directly)
# recipe.json is the fully-hydrated RecipeResult fetched in Step 1.
echo "Generating bundles..."
curl -s -X POST "http://localhost:8080/v1/bundle" \
  -H "Content-Type: application/json" \
  -d @recipe.json \
  -o bundles.zip

# Alternative: one-liner without intermediate file
# curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
#   curl -X POST "http://localhost:8080/v1/bundle" \
#     -H "Content-Type: application/json" -d @- -o bundles.zip

# Step 3: Extract and verify
echo "Extracting bundles..."
unzip -q bundles.zip -d ./deployment

# Verify the complete inventory (checksums.txt is at the bundle root)
echo "Verifying bundle inventory..."
cd deployment
aicr verify .

# Step 4: Deploy (example)
echo "Bundle ready for deployment:"
ls -la
```

## Error Handling

### Error Response Format

```json
{
  "code": "ERROR_CODE",
  "message": "Human-readable error description",
  "details": { ... },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-01-11T10:30:00Z",
  "retryable": true
}
```

### Error Codes

| Code | HTTP Status | Description | Retryable |
|------|-------------|-------------|-----------|
| `INVALID_REQUEST` | 400 | Invalid query parameters, request body, or disallowed criteria value | No |
| `UNAUTHORIZED` | 401 | Authentication or authorization failure | No |
| `NOT_FOUND` | 404 | Selector path not found in the resolved configuration | No |
| `METHOD_NOT_ALLOWED` | 405 | Wrong HTTP method | No |
| `CONFLICT` | 409 | Resource state conflict (e.g., already exists or version mismatch) | No |
| `CANCELED` | 408 | The operation was aborted before completion (CLI-originated; not expected from the HTTP API) | No |
| `RATE_LIMIT_EXCEEDED` | 429 | Too many requests | Yes |
| `INTERNAL` | 500 | Server error | Yes |
| `SERVICE_UNAVAILABLE` | 503 | Server temporarily unavailable | Yes |
| `TIMEOUT` | 504 | Operation exceeded its time limit | Yes |

> `INVALID_REQUEST` is not always `400`: `POST /v1/query` and `POST /v1/recipe`
> return it with HTTP **413 Request Entity Too Large** when the request body
> exceeds the server's body-size limit (`MaxRecipePOSTBytes`).

### Handling Rate Limits

```shell
# Check rate limit headers. HEAD resolves the recipe exactly as GET does and
# returns the same headers without the body -- it is not a cheap probe.
curl -I "http://localhost:8080/v1/recipe?accelerator=h100"

# Response headers:
# X-RateLimit-Limit: 100
# X-RateLimit-Remaining: 95
# X-RateLimit-Reset: 1736589000
```

When rate limited (HTTP 429), use the `Retry-After` header:

```shell
# Retry with backoff
response=$(curl -s -w "%{http_code}" "http://localhost:8080/v1/recipe?accelerator=h100")
if [ "${response: -3}" = "429" ]; then
  # HTTP headers end in CRLF, so strip the carriage return before sleep reads
  # the value.
  retry_after=$(curl -sI "http://localhost:8080/v1/recipe?accelerator=h100" | grep -i "Retry-After" | awk '{print $2}' | tr -d '\r')
  echo "Rate limited. Retrying after ${retry_after}s..."
  sleep "$retry_after"
fi
```

## Rate Limiting

- **Limit**: 100 requests per second (a single process-global token bucket shared across all clients, not per-IP)
- **Burst**: 200 requests
- **Headers**: `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`
- **429 Response**: Includes `Retry-After` header

## Criteria Allowlists

The API server can be configured to restrict which criteria values are allowed. This enables operators to limit the API to specific accelerators, services, intents, or OS types.

### Configuration

Allowlists are configured via environment variables when starting the server:

| Environment Variable | Description | Example |
|---------------------|-------------|---------|
| `AICR_ALLOWED_ACCELERATORS` | Comma-separated list of allowed GPU types | `h100,l40` |
| `AICR_ALLOWED_SERVICES` | Comma-separated list of allowed K8s services | `eks,gke` |
| `AICR_ALLOWED_INTENTS` | Comma-separated list of allowed workload intents | `training` |
| `AICR_ALLOWED_OS` | Comma-separated list of allowed OS types | `ubuntu,rhel` |

**Behavior:**
- If an environment variable is **not set**, all values for that criteria are allowed
- If an environment variable is **set**, only the specified values are permitted
- The `any` value is always allowed regardless of allowlist configuration
- Allowlists apply to `/v1/recipe`, `/v1/query`, and `/v1/bundle`

### Example Configuration

```shell
# Start server allowing only H100 and L40 GPUs on EKS
docker run -p 8080:8080 \
  -e AICR_ALLOWED_ACCELERATORS=h100,l40 \
  -e AICR_ALLOWED_SERVICES=eks \
  ghcr.io/nvidia/aicrd:latest
```

### Error Response

When a disallowed criteria value is requested:

```shell
curl "http://localhost:8080/v1/recipe?accelerator=gb200&service=eks"
```

**Response** (HTTP 400):
```json
{
  "code": "INVALID_REQUEST",
  "message": "accelerator type not allowed",
  "details": {
    "requested": "gb200",
    "allowed": ["h100", "l40"]
  },
  "requestId": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-01-27T10:30:00Z",
  "retryable": false
}
```

### CLI Behavior

The CLI (`aicr`) is **not affected** by allowlists. Allowlists only apply to the API server, allowing operators to restrict API access while maintaining full CLI functionality for administrative tasks.

## Programming Language Examples

### Python

```python
import requests
import zipfile
import io

BASE_URL = "http://localhost:8080"

# Get recipe
params = {
    "accelerator": "h100",
    "service": "eks",
    "intent": "training",
    "os": "ubuntu"
}

resp = requests.get(f"{BASE_URL}/v1/recipe", params=params)
resp.raise_for_status()
recipe = resp.json()

print(f"Recipe has {len(recipe['componentRefs'])} components")

# Generate bundles — the (fully-hydrated) recipe is the request body.
resp = requests.post(
    f"{BASE_URL}/v1/bundle",
    json=recipe,
)
resp.raise_for_status()

# Extract zip
with zipfile.ZipFile(io.BytesIO(resp.content)) as zf:
    zf.extractall("./deployment")
    print(f"Extracted {len(zf.namelist())} files")
```

### Go

```go
package main

import (
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
)

func main() {
    baseURL := "http://localhost:8080"

    // Get recipe
    params := url.Values{}
    params.Add("accelerator", "h100")
    params.Add("service", "eks")
    
    resp, err := http.Get(baseURL + "/v1/recipe?" + params.Encode())
    if err != nil {
        panic(err)
    }
    defer resp.Body.Close()

    var recipe map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&recipe)
    
    fmt.Printf("Got recipe with %d components\n", 
        len(recipe["componentRefs"].([]interface{})))
}
```

### JavaScript/Node.js

```javascript
const BASE_URL = "http://localhost:8080";

async function main() {
    // Get recipe
    const params = new URLSearchParams({
        accelerator: "h100",
        service: "eks",
        intent: "training"
    });
    
    const recipeResp = await fetch(`${BASE_URL}/v1/recipe?${params}`);
    const recipe = await recipeResp.json();
    
    console.log(`Recipe has ${recipe.componentRefs.length} components`);
    
    // Generate bundles — the (fully-hydrated) recipe is the request body.
    const bundleResp = await fetch(`${BASE_URL}/v1/bundle`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(recipe),
    });
    
    // Save zip
    const buffer = await bundleResp.arrayBuffer();
    require("fs").writeFileSync("bundles.zip", Buffer.from(buffer));
    console.log("Bundles saved to bundles.zip");
}

main();
```

### Shell Script (Batch Processing)

```bash
#!/bin/bash
# Generate recipes for multiple environments

# /v1/recipe accepts every family, profiled and unprofiled alike.
environments=(
  "os=ubuntu&accelerator=h100&service=eks"
  "os=cos&accelerator=h100&service=gke"
  "os=ubuntu&accelerator=h100&service=aks"
)

for env in "${environments[@]}"; do
  echo "Fetching recipe for: $env"

  curl -s "http://localhost:8080/v1/recipe?${env}" \
    | jq -r '.componentRefs[] | "\(.name): \(.version)"'

  echo ""
done
```

## OpenAPI Specification

The full OpenAPI 3.1 specification is available at:
[api/aicr/v1/server.yaml](https://github.com/NVIDIA/aicr/blob/main/api/aicr/v1/server.yaml)

Generate client SDKs:

```shell
# Download spec
curl https://raw.githubusercontent.com/NVIDIA/aicr/main/api/aicr/v1/server.yaml \
  -o openapi.yaml

# Generate Python client
openapi-generator-cli generate -i openapi.yaml -g python -o ./python-client

# Generate Go client
openapi-generator-cli generate -i openapi.yaml -g go -o ./go-client

# Generate TypeScript client
openapi-generator-cli generate -i openapi.yaml -g typescript-fetch -o ./ts-client
```

## Troubleshooting

### Common Issues

**"Invalid accelerator type" error:**
```shell
# Use valid values: h100, h200, gb200, gb300, b200, a100, l40, l40s, rtx-pro-6000, any
curl "http://localhost:8080/v1/recipe?accelerator=h100"
```

**"Recipe is required" error:**
```shell
# The body IS the RecipeResult itself — not wrapped in a {"recipe": ...} field.
# Pass a fully-hydrated RecipeResult (e.g. from GET /v1/recipe) directly:
curl -s "http://localhost:8080/v1/recipe?accelerator=h100&service=eks" | \
  curl -X POST "http://localhost:8080/v1/bundle" \
    -H "Content-Type: application/json" -d @- -o bundles.zip
```

**Empty zip file:**
```shell
# Check recipe has componentRefs
curl -s "http://localhost:8080/v1/recipe?accelerator=h100" | jq '.componentRefs'
```

**Connection refused (local):**
```shell
# Start local server first
make server
```

## See Also

- [CLI Reference](cli-reference.md) - Command-line interface
- [Agent Deployment](agent-deployment.md) - Kubernetes agent for snapshot capture
- [Installation Guide](installation.md) - Setup instructions
- [Data Flow](../integrator/data-flow.md) - Understanding recipe data architecture
- [Automation Guide](../integrator/automation.md) - CI/CD integration patterns
- [Kubernetes Deployment](../integrator/kubernetes-deployment.md) - Self-hosted API server deployment

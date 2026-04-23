# ADR-004: Hydrated Recipe Query Command

## Status

**Proposed** — 2026-03-19

## Scope

This ADR covers a new `aicr query` CLI command (and corresponding API endpoint)
that resolves a recipe from criteria — identical to `aicr recipe` — then lets the
user extract a specific configuration value by dot-path selector. The result is
the fully hydrated value at that path, not a reference to an external file.

## Context

AICR embeds a significant amount of curated metadata into the binary at build
time — component registries, overlay definitions, Helm values, version matrices,
and constraint rules. This metadata is the product of extensive validation and
testing, but **users have no way to inspect it directly**. The only path to see
actual resolved configuration today is to run `aicr bundle`, which generates a
full deployment artifact (Helm values files, Kustomize manifests, etc.) on disk.
There is no read-only introspection path.

`aicr recipe` gets partway there — it returns a `RecipeResult` that describes
the desired cluster configuration for a given set of criteria. However, the
recipe output is deliberately abstract: component entries contain **references**,
not resolved content. A `ComponentRef` lists a chart name, repository URL,
version string, and `ValuesFile` path, but the actual Helm values those
references resolve to are absent from the output. The recipe describes *what*
should be deployed, not *how it is configured*.

This means that to answer a simple question like "what driver version does the
GPU operator use for H100 on EKS with Ubuntu?", a user must either:

1. Run `aicr bundle` to generate the full deployment artifact, then dig through
   the output files to find the value — a heavyweight, write-to-disk operation
   for a read-only question
2. Or manually reconstruct the merge pipeline:
   a. Run `aicr recipe` to get the `RecipeResult`
   b. Find the component of interest in `componentRefs`
   c. Locate the referenced `ValuesFile` on disk (embedded in the binary)
   d. Manually merge base values, overlay values, and inline overrides
   e. Navigate the resulting YAML to the desired key

Both paths are error-prone and opaque, especially when overlays stack multiple
layers of value overrides. The embedded metadata — the most valuable part of
AICR — is effectively a black box until bundle time.

### Requirements

1. Accept the same criteria flags as `recipe` (`--service`, `--accelerator`,
   `--intent`, `--os`, `--platform`, `--snapshot`, `--criteria`)
2. Require a `--selector` flag with a dot-path expression
3. Resolve the recipe identically to `aicr recipe`
4. Hydrate all component values (base + overlay + inline overrides merged)
5. Walk the dot-path and return the value at that node
6. Return scalar values as plain text; complex values as YAML (or JSON with `--format`)

## Decision

Add a `query` command that builds a fully hydrated intermediate representation of
the resolved recipe, then extracts a subtree using a dot-path selector.

### Hydrated structure

After recipe resolution, `query` builds a single `map[string]any` that inlines
every component's merged values:

```yaml
criteria:
  service: eks
  accelerator: h100
  intent: training
  os: ubuntu
  platform: any

metadata:
  version: "1.5.0"
  appliedOverlays:
    - base
    - eks
    - h100-eks-ubuntu-training

deploymentOrder:
  - gpu-operator
  - network-operator
  - ...

components:
  gpu-operator:
    name: gpu-operator
    namespace: gpu-operator
    type: Helm
    chart: gpu-operator
    source: https://helm.ngc.nvidia.com/nvidia
    version: v24.9.0
    values:                       # fully hydrated
      driver:
        version: "570.86.16"
        repository: nvcr.io/nvidia
      devicePlugin:
        enabled: true
      toolkit:
        enabled: true
    dependencyRefs:
      - network-operator
  network-operator:
    name: network-operator
    namespace: network-operator
    type: Helm
    chart: network-operator
    source: https://helm.ngc.nvidia.com/nvidia
    version: v25.1.0
    values:
      ...

constraints:
  - name: min-gpu-count
    ...
```

Each component's `values` key is the result of calling `GetValuesForComponent`,
which merges base values file, overlay values file, and inline overrides in
precedence order. All other `ComponentRef` fields (chart, source, version,
namespace, etc.) are inlined directly.

### Selector syntax

The `--selector` flag accepts a **dot-delimited path**, consistent with Helm's
`--set` notation and `yq` path syntax:

```bash
# Scalar — returns plain value
aicr query --service eks --accelerator h100 --intent training \
  --selector 'components.gpu-operator.values.driver.version'
# stdout: 570.86.16

# Subtree — returns YAML block
aicr query --service eks --accelerator h100 --intent training \
  --selector 'components.gpu-operator.values.driver'
# stdout:
#   version: "570.86.16"
#   repository: nvcr.io/nvidia

# Component-level — full hydrated component
aicr query --service eks --accelerator h100 --intent training \
  --selector 'components.gpu-operator'

# Recipe-level metadata
aicr query --service eks --accelerator h100 --intent training \
  --selector 'deploymentOrder'

# Criteria echo
aicr query --service eks --accelerator h100 --intent training \
  --selector 'criteria.service'
# stdout: eks

# All
aicr query --service eks --accelerator h100 --intent training --selector ''
```

Path resolution rules:

| Input | Behavior |
|-------|----------|
| `components.X.values.a.b` | Walk into component X's hydrated values |
| `components.X.chart` | Return ComponentRef field directly |
| `components.X` | Return entire hydrated component block |
| `criteria.service` | Return criteria field |
| `deploymentOrder` | Return deployment order list |
| `metadata.appliedOverlays` | Return applied overlay list |
| `constraints` | Return merged constraints |
| Non-existent path | Return structured error (ErrCodeNotFound) |

### Output behavior

| Value type | `--format yaml` (default) | `--format json` |
|------------|---------------------------|-----------------|
| Scalar (string, number, bool) | Plain text, no YAML wrapper | JSON primitive |
| List | YAML sequence | JSON array |
| Map | YAML mapping | JSON object |

Scalar values are printed without YAML document markers (`---`) or quoting to
make them directly usable in shell pipelines:

```bash
VERSION=$(aicr query --service eks --accelerator h100 --intent training \
  --selector 'components.gpu-operator.values.driver.version')
echo "Driver version: $VERSION"
```

### Implementation

**New files:**

| File | Purpose |
|------|---------|
| `pkg/cli/query.go` | CLI command definition, flag wiring, output formatting |
| `pkg/recipe/query.go` | `HydrateResult(*RecipeResult) map[string]any` and `Select(hydrated map[string]any, path string) (any, error)` |
| `pkg/recipe/query_test.go` | Table-driven tests for hydration and path selection |

**Changes to existing files:**

| File | Change |
|------|--------|
| `pkg/cli/root.go` | Register `query` subcommand |
| `pkg/api/server.go` | Add `GET/POST /v1/query` endpoint |

**Core functions:**

```go
// pkg/recipe/query.go

// HydrateResult builds a fully hydrated map from a RecipeResult.
// Component values are merged via GetValuesForComponent.
func HydrateResult(result *RecipeResult) (map[string]any, error)

// Select walks a dot-path selector against a hydrated map and returns
// the value at that path. Returns ErrCodeNotFound for invalid paths.
func Select(hydrated map[string]any, selector string) (any, error)
```

`Select` is ~30 lines: split on `.`, iterate keys, descend into nested maps.
No external dependencies required — this avoids pulling in JMESPath or JSONPath
libraries for a deliberately simple path model.

**CLI flag reuse:** `query` embeds the same criteria flag set as `recipe` via
shared option constructors (the `With*` functional options already used by
`recipe.go`). The only addition is `--selector` (required, string).

### API endpoint

```
GET  /v1/query?service=eks&accelerator=h100&intent=training&selector=components.gpu-operator.values.driver.version
POST /v1/query  { "criteria": {...}, "selector": "components.gpu-operator.values.driver.version" }
```

Returns the selected value with `Content-Type` matching the requested format.
Scalar values return `text/plain`; complex values return `application/yaml` or
`application/json`.

## Consequences

### Positive

- **Removes opacity.** Users can inspect any resolved value without manual file
  merging or knowledge of the overlay chain.
- **Shell-friendly.** Scalar output with no YAML wrapper makes `query` composable
  in scripts (`VERSION=$(aicr query ...)`).
- **Zero new dependencies.** Dot-path walking is trivial; no JMESPath/JSONPath
  library needed.
- **Reuses existing machinery.** Recipe resolution, criteria matching, and value
  merging are unchanged — `query` is a thin layer on top.
- **API parity.** The REST endpoint mirrors the CLI, enabling programmatic access
  from CI pipelines and automation.

### Negative

- **No advanced filtering.** Dot-path cannot express array filters (`select(.name == "X")`),
  wildcards, or projections. Users needing those can pipe full output to `yq`/`jq`.
- **Hydration cost.** Every query hydrates all components even if the selector targets
  a single one. Acceptable at current scale (~15-30 components per recipe); may need
  lazy hydration if component counts grow significantly.

### Neutral

- **`recipe` command is unchanged.** `query` is additive; existing workflows are
  unaffected.
- **Selector syntax may expand later.** Array indexing (`tolerations[0]`) and
  wildcards (`components.*.version`) are natural extensions but explicitly out of
  scope for v1.

## Alternatives Considered

### 1. Shallow query on RecipeResult (no hydration)

Apply the selector directly to the serialized `RecipeResult` without merging
component values.

**Rejected:** Does not solve the core problem. `ValuesFile` paths and `Overrides`
maps are implementation details — users want the merged result, not the merge
inputs.

### 2. Two-level `component:path` selector syntax

Use `gpu-operator:driver.version` instead of `components.gpu-operator.values.driver.version`.

**Rejected:** Custom syntax that users must learn. Dot-path is universal (Helm
`--set`, `yq`, Spring, Consul KV). The longer path is more explicit and supports
querying non-component fields (criteria, metadata, constraints) without special-casing.

### 3. Embed yq/jq as a library

Use an existing query language (JMESPath, JSONPath, or embed yq) for full
expressiveness.

**Deferred:** Adds a dependency for power that most users don't need. The 90%
use case is "give me this one value." Users who need advanced filtering can pipe
`aicr recipe --format json | jq '...'`. If demand emerges, a `--jq` flag could
be added later without changing the `--selector` contract.

### 4. Add hydrated output to `recipe` command directly

Add a `--hydrate` flag to `recipe` that inlines all values.

**Rejected:** Conflates two concerns. `recipe` output is consumed by `bundle` and
`validate` — changing its structure risks breaking downstream consumers. A separate
command keeps the contract clean and lets `query` optimize for human-readable output.

## References

- [Recipe command](https://github.com/NVIDIA/aicr/blob/main/pkg/cli/recipe.go)
- [Recipe resolution](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/metadata_store.go)
- [Value hydration](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/adapter.go)
- [Component registry](https://github.com/NVIDIA/aicr/blob/main/recipes/registry.yaml)

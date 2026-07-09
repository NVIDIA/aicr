# Validator v1 API

`pkg/validator/v1` is the canonical home of AICR's validator input format
and the job-plan API external Kubernetes controllers use to render and
deploy AICR validator Jobs.

> **Stability.** This package implements `v1alpha1`. The schema may have
> breaking changes before `v1`. Breaking changes once we reach `v1` will
> require a major version bump (`v2.0.0`). See `doc.go` for the full
> stability contract.
>
> **Provenance.** This package previously lived at
> `pkg/api/validator/v1` and was relocated under `pkg/validator/v1` so the
> on-disk layout matches its position in the validation pipeline. Re-exported
> aliases keep older import paths source-compatible during the transition;
> new code should import this path directly.

## Package surface

The package is intentionally narrow and exports three concerns:

1. **`ValidationInput`** (`validation_input.go`) — the wire format consumed
   by a recipe's `spec.validation` block. Carries phases, checks,
   constraints, criteria, the resolved component refs, and the resolved
   `gpuAllocationPolicy` (`allocation_policy.go`). **Migration note for
   external controllers:** recipe-backed callers should build inputs with
   `ToValidationInputWithContext`, which resolves the GPU allocation policy
   from the recipe's hydrated values; the legacy `ToValidationInput` leaves
   the policy empty, which validators normalize to `unspecified` and keep
   capability-driven selection (also the behavior for inputs from
   pre-policy producers — the field is `omitempty` in both directions).
   Unknown and reserved configured policies fail closed at the gated
   checks.
2. **`ValidatorCatalog` + `ValidatorEntry`** (`catalog.go`) — the catalog
   schema and the `Phase`/filtering helpers. Catalog *loading* lives in
   `pkg/validator/catalog`; this package owns the types.
3. **`JobPlan` + planners + renderers** (`job_plan.go`) — the data shape
   external controllers customize, plus the functions that build one
   (`Plan`, `BuildJobPlan`) and render it into a `batchv1.Job` or an
   apply-config (`RenderPlan`, `RenderPlanToApplyConfig`).
4. **Affinity helpers** (`affinity.go`, `dependency_affinity.go`) —
   support for the catalog's `dependencyAffinity` declaration.

`GenerateRunID` and `ImagePullPolicy` are exported utilities for callers
that build their own renderer.

## Quick start

### 1. Generate a run ID

```go
import v1 "github.com/NVIDIA/aicr/pkg/validator/v1"

runID := v1.GenerateRunID()
// Example: "20260514-143052-a1b2c3d4e5f6g7h8"
```

### 2. Create the snapshot + validation ConfigMaps

```go
import (
    v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
    "github.com/NVIDIA/aicr/pkg/validator"
)

err := validator.EnsureDataConfigMaps(
    ctx,
    clientset,
    namespace,
    runID,
    snapshot,        // *snapshotter.Snapshot
    validationInput, // *v1.ValidationInput
)
```

`EnsureDataConfigMaps` stays in `pkg/validator` because it touches
Kubernetes API objects and is not part of the v1 wire contract.

### 3. Load the catalog and plan

```go
import (
    v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
    "github.com/NVIDIA/aicr/pkg/validator/catalog"
)

cat, err := catalog.LoadWithDataProvider(ctx, nil, version, commit)
if err != nil {
    return err
}

serviceAccount := "my-validator-sa-" + runID

plans, err := v1.Plan(
    cat,
    validationInput,
    runID,
    namespace,
    version,        // controller version
    commit,         // controller commit SHA
    serviceAccount, // SA name your controller manages
    nil,            // imagePullSecrets
    nil,            // tolerations (forwarded to inner workloads)
    nil,            // nodeSelector (forwarded to inner workloads)
    "",             // imageRegistryOverride
    "",             // imageTagOverride
    componentRefs,  // []recipe.ComponentRef, may be nil
)
```

Notes:

- `LoadWithDataProvider(ctx, nil, …)` uses the embedded catalog. Pass a
  layered `recipe.DataProvider` to honor a `--data` overlay.
- `serviceAccount` is yours to name. AICR's own CLI uses
  `aicr-validator-<runID>`; external controllers should pick a strategy
  consistent with their RBAC.
- `tolerations` and `nodeSelector` apply to *inner workloads* (GPU
  benchmarks, NCCL tests). The orchestrator pod itself uses
  tolerate-all scheduling and gets its affinity from
  `BuildOrchestratorAffinity` (prefer-CPU NodeAffinity, plus PodAffinity
  for any `dependencyAffinity` declarations).
- `componentRefs` is the resolved recipe's component list and is used
  exclusively to resolve `dependencyAffinity.componentRef` entries to
  namespaces. Pass `nil` when no component-targeted affinity applies.
- `Plan` returns `ErrCodeInvalidRequest` when an entry declares a
  `required` `dependencyAffinity.componentRef` that is not present in
  `componentRefs`.

### 4a. Deploy with `Create` (simple path)

```go
for _, plan := range plans {
    job := v1.RenderPlan(plan)
    if _, err := clientset.BatchV1().Jobs(namespace).Create(
        ctx, job, metav1.CreateOptions{},
    ); err != nil {
        return err
    }
}
```

### 4b. Deploy with server-side apply (idempotent)

```go
for _, plan := range plans {
    apply := v1.RenderPlanToApplyConfig(plan, plan.JobName)
    if _, err := clientset.BatchV1().Jobs(namespace).Apply(
        ctx, apply, metav1.ApplyOptions{
            FieldManager: "my-controller",
            Force:        true,
        },
    ); err != nil {
        return err
    }
}
```

Use the apply path when the same plan may be reconciled more than once
or when more than one controller owns fields on the same Job.

## JobPlan

```go
type JobPlan struct {
    ValidatorName    string                      // unique validator identifier
    Phase            string                      // "deployment" | "performance" | "conformance"
    JobName          string                      // generated; aicr-{validator}-{hex}
    Namespace        string                      // Kubernetes namespace
    Image            string                      // resolved container image
    Args             []string
    Env              []corev1.EnvVar
    Volumes          []corev1.Volume             // snapshot + validation ConfigMaps
    VolumeMounts     []corev1.VolumeMount
    Resources        corev1.ResourceRequirements
    Timeout          int64                       // activeDeadlineSeconds
    ServiceAccount   string
    Tolerations      []corev1.Toleration         // forwarded; orchestrator pod is tolerate-all
    ImagePullSecrets []string
    Labels           map[string]string
    Affinity         *corev1.Affinity            // orchestrator pod affinity (NodeAffinity + optional PodAffinity)
}
```

`JobName` is generated by `BuildJobPlan` as `aicr-{validatorName}-{hex}`.
It is stable within a plan but unique across invocations.

`Affinity`, when non-nil, is what the renderer applies to the
orchestrator pod. A nil value falls back to the default prefer-CPU
NodeAffinity.

## Grouping plans by phase

`Plan` returns a flat list. Controllers that want phase ordering should
group:

```go
groups := make(map[string][]v1.JobPlan)
for _, p := range plans {
    groups[p.Phase] = append(groups[p.Phase], p)
}

for _, p := range groups[string(v1.PhaseDeployment)] { /* … */ }
for _, p := range groups[string(v1.PhasePerformance)] { /* … */ }
for _, p := range groups[string(v1.PhaseConformance)] { /* … */ }
```

## Customizing a single plan

```go
plan, err := v1.BuildJobPlan(
    entry,
    runID,
    namespace,
    version, commit,
    serviceAccount,
    nil, tolerations, nodeSelector,
    "", "",
    componentRefs,
)
if err != nil {
    return err
}

plan.Timeout = 600 // 10 minutes
plan.Env = append(plan.Env, corev1.EnvVar{Name: "MY_VAR", Value: "x"})

job := v1.RenderPlan(plan)
```

## API choice

| Scenario                          | Use                                      |
|-----------------------------------|------------------------------------------|
| Single-shot controller            | `RenderPlan` + `Create`                  |
| Idempotent reconciliation         | `RenderPlanToApplyConfig` + `Apply`      |
| Multi-controller field ownership  | `RenderPlanToApplyConfig` + `Apply`      |

Server-side apply is the default we recommend for any controller that
will run a reconcile loop.

## End-to-end example

```go
package main

import (
    "context"
    "fmt"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"

    "github.com/NVIDIA/aicr/pkg/snapshotter"
    "github.com/NVIDIA/aicr/pkg/validator"
    "github.com/NVIDIA/aicr/pkg/validator/catalog"
    v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

func RunValidation(
    ctx context.Context,
    clientset kubernetes.Interface,
    namespace, version, commit string,
    validationInput *v1.ValidationInput,
    snapshot *snapshotter.Snapshot,
    componentRefs []v1.ComponentRef,
) error {
    runID := v1.GenerateRunID()

    if err := validator.EnsureDataConfigMaps(
        ctx, clientset, namespace, runID, snapshot, validationInput,
    ); err != nil {
        return err
    }

    cat, err := catalog.LoadWithDataProvider(ctx, nil, version, commit)
    if err != nil {
        return err
    }

    serviceAccount := "my-validator-sa-" + runID

    plans, err := v1.Plan(
        cat, validationInput, runID, namespace,
        version, commit, serviceAccount,
        nil, nil, nil, "", "",
        componentRefs,
    )
    if err != nil {
        return err
    }

    for _, plan := range plans {
        fmt.Printf("apply %s (validator=%s phase=%s)\n",
            plan.JobName, plan.ValidatorName, plan.Phase)
        apply := v1.RenderPlanToApplyConfig(plan, plan.JobName)
        if _, err := clientset.BatchV1().Jobs(namespace).Apply(
            ctx, apply, metav1.ApplyOptions{
                FieldManager: "my-controller",
                Force:        true,
            },
        ); err != nil {
            return fmt.Errorf("apply %s: %w", plan.ValidatorName, err)
        }
    }
    return nil
}
```

## API reference

### Session

**`GenerateRunID() string`**
Returns `{YYYYMMDD-HHMMSS}-{hex16}` — e.g. `20260514-123045-a1b2c3d4e5f6g7h8`.
Panics on entropy failure (preferred over a predictable ID).

### Planning

**`Plan(cat, validationInput, runID, namespace, version, commit, serviceAccount, imagePullSecrets, tolerations, nodeSelector, imageRegistryOverride, imageTagOverride, componentRefs) ([]JobPlan, error)`**

Builds one `JobPlan` per `(phase, validator entry)` pair that matches
`validationInput`. A nil catalog returns an empty slice and no error.

**`BuildJobPlan(entry, runID, namespace, version, commit, serviceAccount, imagePullSecrets, tolerations, nodeSelector, imageRegistryOverride, imageTagOverride, componentRefs) (JobPlan, error)`**

Builds a plan from a single catalog entry. Returns
`ErrCodeInvalidRequest` when a `required` `dependencyAffinity.componentRef`
is not present in `componentRefs`.

Parameter notes shared by both functions:

- `tolerations`, `nodeSelector` — forwarded to inner workloads via the
  `AICR_TOLERATIONS` and `AICR_NODE_SELECTOR` env vars. The orchestrator
  Pod itself is tolerate-all and has no node selector.
- `imageRegistryOverride` — replaces the registry prefix on every
  validator image. Matches `AICR_VALIDATOR_IMAGE_REGISTRY`. Empty
  disables the override.
- `imageTagOverride` — replaces the tag on every tag-based reference.
  Digest-pinned references (`name@sha256:…`) are left untouched.
  Matches `AICR_VALIDATOR_IMAGE_TAG`. Empty disables the override.
- `componentRefs` — resolved component list from the recipe. Used to
  resolve `dependencyAffinity.componentRef` to a namespace. Pass `nil`
  when dependencyAffinity is unused.

### Rendering

**`RenderPlan(plan JobPlan) *batchv1.Job`**
Materializes a `batchv1.Job`. Uses `plan.JobName` and `plan.Namespace`.

**`RenderPlanToApplyConfig(plan JobPlan, jobName string) *applybatchv1.JobApplyConfiguration`**
Materializes an apply-config for server-side apply. Pass `plan.JobName`
as `jobName`.

### Image utilities

**`ImagePullPolicy(image string, imageTagOverride string) corev1.PullPolicy`**

| Input                                  | Policy            |
|----------------------------------------|-------------------|
| `ko.local/…`, `kind.local/…`           | `PullNever`       |
| digest pinned (`…@sha256:…`)           | `PullIfNotPresent`|
| `imageTagOverride != ""`               | `PullAlways`      |
| `:latest`                              | `PullAlways`      |
| anything else (versioned tag)          | `PullIfNotPresent`|

## RBAC

External controllers own their ServiceAccount, Role/ClusterRole, and
binding. Plug the SA name into `Plan` / `BuildJobPlan` via the
`serviceAccount` parameter. AICR's own CLI uses
`aicr-validator-<runID>`, but you are free to choose any convention.

## Where the moving parts live

| Concern                          | Package                              |
|----------------------------------|--------------------------------------|
| Wire types (`ValidationInput`, catalog schema, `JobPlan`) | `pkg/validator/v1` (this package) |
| Catalog loading + image rewriting | `pkg/validator/catalog`             |
| Job dispatch, watch, log streaming | `pkg/validator`                     |
| Snapshot capture                 | `pkg/snapshotter`                    |
| Recipe resolution / overlays     | `pkg/recipe`                         |

## Notes

- The public, semver-stable consumer surface is `pkg/client/v1`. This
  package is the underlying wire format the facade re-exports. Embed
  `ValidationConfig` directly (not `ValidationInput`) if you need to
  drop the wrapper `metadata`/`apiVersion`/`kind` fields in a custom
  resource spec.
- Both Create and server-side Apply deployment strategies are supported.
- `ConfigMap` creation (`EnsureDataConfigMaps`) stays in `pkg/validator`
  because it is an in-cluster side effect, not a wire-format concern.

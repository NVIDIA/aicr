# ADR-029: Required and Install-Time Secret Inputs in Bundles

## Status

**Proposed** — 2026-11-03. Part of the Run:ai integration
([#3302](https://github.com/NVIDIA/aicr/issues/3302), Phase 1); the first
component held to it is `runai-cluster`. Extends the registry's existing
bundle-time requirement, `requireNodeSelector`, from scheduling paths to
arbitrary value paths.

## Decision Summary

A component may declare value paths a bundle must supply (`requiredValues`)
and value paths that carry a credential (`installTimeSecretPaths`). The
bundler rejects a bundle that leaves a required path empty and has not
declared it `--dynamic`, rejects a non-empty value at a secret path from any
source, rejects every deployer but `helm` for a component whose secret path
is required or declared dynamic, and emits a guard into the helm
`install.sh` that refuses to run while a required dynamic path's
`cluster-values.yaml` stub is still empty. No credential value ever appears
in a generated artifact.

## Problem

Some charts cannot do anything useful without a per-cluster input the recipe
cannot know. The Run:ai cluster chart registers the cluster with a control
plane the user already runs, which takes the control plane URL, the cluster
UID that control plane assigned, the FQDN the control plane reaches the
cluster at, and a bootstrap token it issued, valid for two hours. The chart
validates none of them: rendered with all of them empty it
produces the same 381 objects, and its registration hook Jobs run with empty
arguments and fail inside the cluster.

Before this decision AICR had no way to express "this path must be supplied".
The closest mechanism, `requireNodeSelector`, covers scheduling paths only.
`--dynamic` moves a path into `cluster-values.yaml` but nothing requires an
operator to use it, nothing stops `--set` from writing a token into
`values.yaml`, and four deployers (`flux`, `fleet`, `terraform`,
`argocd-helm`) would persist the filled value in the artifacts they generate
or the state they keep. The documentation carried those rules; the bundler
did not enforce them.

## Goals

- A bundle missing a per-cluster input fails at bundle time, naming the
  input and the flag that supplies it.
- No generated bundle, golden, evidence file or install script carries a
  credential value.
- The rule is declared once, in the registry, and enforced on every path to
  the bundler (CLI, REST, SDK), not in a per-leaf comment.
- The install step fails closed when the operator forgot to fill the stub.

## Non-Goals

- Validating the *content* of an input (URL shape, UID format). The chart and
  the control plane do that.
- Reading a secret from a Kubernetes Secret or a secret manager at install
  time. That is an upstream ask on the chart (`controlPlane.existingSecret`
  exists but needs a long-lived credential AICR does not want to require).
- Scrubbing a secret from Helm release records. Helm stores every revision's
  values; the docs say so and the token's expiry bounds it.

## Options

**A. Documentation only.** State the rules in the component catalog and the
values file. Rejected: a `--deployer fleet` bundle would commit the token to
Git with no warning, and an unparameterised bundle installs and fails
inside the cluster.

**B. Bundle flags.** A `--require` flag per path, passed by the operator.
Rejected: the operator is the person who forgets; the requirement belongs
with the component that has it.

**C. Registry declaration, bundler enforcement.** Two list fields on
`ComponentConfig`, checked where every override has already been applied.
Chosen: it mirrors `requireNodeSelector`, reaches REST and SDK callers, and
gives the install script enough to add its own guard.

## Decision

Option C.

## Proposal

**Registry.** `ComponentConfig` gains `requiredValues []string` and
`installTimeSecretPaths []string`. `ComponentRegistry.Validate` rejects an
empty or repeated entry.

**Required paths.** In `extractComponentValues`, after
`validateRequiredNodeSelectors`, every required path must hold a non-empty
value (nil, a blank string, an empty map or an empty list are empty; `false`
and `0` are settings) or be covered by a `--dynamic` declaration on the path,
an ancestor or a descendant. Otherwise the bundle fails with
`INVALID_REQUEST`, listing the missing paths grouped by the flag that
supplies them (`--set` for plain paths, `--dynamic` for secret ones).

**Secret paths.** In the same place, a non-empty value at a secret path fails
the bundle with `INVALID_REQUEST`, whatever wrote it. `--dynamic` is the only
way to supply one.

**Deployers.** Before values are extracted, a component with a secret path
that is required or declared dynamic fails the bundle on every deployer but
`helm`, naming the deployer: `flux`, `fleet`, `terraform` and `argocd-helm`
persist the filled value, and `helmfile` reads `cluster-values.yaml` at
apply time but has no stub check. Ordering matters: on the wrong deployer the
operator must hear that first, not a hint to declare the secret dynamic that
the deployer would then refuse. A secret path that is neither required nor
declared ships as an empty stub on any deployer; nothing is persisted for it.

**Install guard.** `deployer.ResolveRequiredDynamicPaths` returns, per
component, the dynamic paths a required path covers. The helm deployer hands
them to `localformat`, whose primary-folder `install.sh` templates emit, per
path, a check that resolves the full dotted path in `cluster-values.yaml`
and fails, naming the path, when it is missing or holds `""`, `''`, `~` or
`null` (a trailing comment allowed). Matching the leaf key alone is not
enough: `runai-cluster` requires both `controlPlane.url` and `cluster.url`.
The helper returns the required path rather than the declaration, so a
declaration on an ancestor map yields every required leaf beneath it and the
guard judges the leaves the operator wrote, not the map line, which has no
scalar value. The check is skipped under
`--dry-run`, so a bundle can be previewed before a short-lived value is
minted.

**Errors.** All four are `errors.ErrCodeInvalidRequest`; a registry that
cannot be loaded is `ErrCodeInternal`, failing closed.

## Consequences

- The two Run:ai leaves no longer render with default options. The
  stock-render golden supplies placeholder URL and UID and declares the token
  dynamic for them; every other leaf is unaffected.
- A `--data` registry can declare the fields for its own components; the
  enforcement is not tied to the embedded catalog.
- A user overlay that hardcodes a value at a secret path now fails to bundle.
  That is the intended direction: such a value would have been committed.
- `api-diff` sees two new exported struct fields and one new exported
  function; both are additive.

## Adoption plan

1. This ADR, the fields, the four enforcement points and their tests land
   with the Run:ai Phase 1 PR, with `runai-cluster` as the only declaring
   component.
2. Components that today document a required input in prose (`nvcre`'s
   Trainer source, the agentgateway exposure settings) are candidates for
   `requiredValues` in follow-ups, each weighed on whether an empty value is
   a mistake or a valid default.
3. If a chart learns to read its secret from a Kubernetes Secret, the
   `installTimeSecretPaths` entry moves to the Secret reference and the
   deployer restriction lifts for that component.

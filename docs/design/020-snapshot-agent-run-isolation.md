# ADR-020: Snapshot Agent Run Isolation

## Status

**Proposed** — 2026-08-21. Addresses
[#2120](https://github.com/NVIDIA/aicr/issues/2120).

## Decision Summary

Every `aicr snapshot` and `aicr validate` invocation generates a run ID and
suffixes it onto every **run-owned** object. User-supplied resource names are
**prefixes**, never exact names, so no run-owned object can collide with another
run's. The Namespace and an explicitly requested output ConfigMap are shared by
design and are not suffixed.

Object lifecycles fall into exactly three classes — **run-owned**, **ensured**,
and **delivered** — and every object the snapshot agent touches belongs to one.

## Context

`Client.CollectSnapshot` documents concurrent calls as safe and independent
(`pkg/client/v1/aicr.go:1834-1836`). That promise is currently false: every run
builds the same fixed object names, and the paths that create them are
destructive.

`pkg/validator` already solved this for validation Jobs. It generates a per-run
ID (`pkg/validator/v1/job_plan.go:107`), derives resource names from it
(`pkg/validator/job/rbac.go:42,49`), and labels resources with `labels.RunID` —
already `aicr.run/run-id` (`pkg/validator/labels/labels.go:30`,
`pkg/header/header.go:36`). The snapshot agent in `pkg/k8s/agent` predates that
work and never adopted it. This ADR extends the existing pattern rather than
introducing a new one.

## Problem

| Resource | Name today | Create path | Failure mode |
|---|---|---|---|
| Job | `config.JobName` (default `aicr`) | `job.go:35-55` | Deletes any existing Job of that name, waits, recreates. Run B kills run A's Job. |
| ServiceAccount | `config.ServiceAccountName` (default `aicr`) | `rbac.go:81-93` | `IgnoreAlreadyExists` — shared. Cleanup deletes it, invalidating run B's projected token mid-flight. |
| Role / RoleBinding | `config.ServiceAccountName` | `rbac.go:95-160` | create-or-update, then deleted by either run's cleanup. The Role grants `configmaps: create/get/update/patch` — what the agent needs to stage its result. |
| ClusterRole / ClusterRoleBinding | `aicr-node-reader` | `rbac.go:162-264` | create-or-update with a rule set conditioned on `DiscoverNetwork` (`rbac.go:196-199`). A discovery run grants `pods/exec` and `nodes/patch` to a non-discovery run's identity; a non-discovery run revokes them mid-flight. |
| Staging ConfigMap | `cm://<ns>/aicr-snapshot` | in-pod agent | Both runs write the same key; the winner's bytes are returned to both callers. |
| Pod discovery | `app.kubernetes.io/name=aicr`, youngest live pod | `wait.go:111-149` | Logs stream from the other run's pod. |

Three further properties of the current code shape the decision:

1. **Cleanup is name-scoped and runs on the failure path.**
   `pkg/snapshotter/agent.go:196-207` registers the deferred `Cleanup` before
   `Deploy` at `:211`; `deployer.go:111-121` builds its delete list from
   configured names with no ownership predicate.
2. **`aicr validate` races `aicr snapshot`.** Both default
   `--service-account-name` to `aicr` (`cli/snapshot.go:338`,
   `cli/validate.go:459`) and both create `aicr-node-reader`, so the two commands
   collide even though their Job names differ.
3. **The internal staging ConfigMap is never cleaned up**, by explicit comment in
   `pkg/snapshotter/agent.go`.

## Decision

### 1. Reuse the existing run-ID generator

`GenerateRunID()` produces `<UTC timestamp>-<16 hex>`, e.g.
`20260821-142233-9f3a1c0b7e2d4a55`. Promote it and the `labels` key constants out
of `pkg/validator` into neutral packages (`pkg/runid`, `pkg/k8s/labels`) so
`pkg/k8s/agent` does not depend on a validator-domain package, and so both
subsystems share one ID format under one label key.

`AgentConfig.RunID` is injectable so e2e and chainsaw runs can pin a value and
unit tests stay deterministic. The run ID is logged at `slog.Info` on deploy; its
timestamp prefix makes runs sortable in `kubectl get` output and in orphan
triage.

**One run ID per invocation.** `aicr validate` both collects a snapshot and
deploys validation Jobs, and both must carry the same ID. It is generated once at
the top of the command and passed into collection. Today the two are independent
— `pkg/cli/validate.go:191` collects, `:265` generates — which would split names,
labels, logs, and cleanup across two IDs.

### 2. Every user-supplied name is a prefix

`--job-name`, `--service-account-name`, and their `pkg/config` and SDK
equivalents are prefixes:

| Object | Name |
|---|---|
| Job | `<job-prefix>-<runID>`, default `aicr` (`aicr-validate` for `aicr validate`) |
| ServiceAccount, Role, RoleBinding | `<sa-prefix>-<runID>`, default `aicr` |
| ClusterRole / ClusterRoleBinding | `aicr-node-reader-<runID>` |
| Staging ConfigMap | `aicr-agent-snapshot-<runID>` |

**Why the staging ConfigMap prefix is `aicr-agent-snapshot-`, not `aicr-snapshot-`.**
`pkg/validator` independently names its own snapshot data ConfigMap
`aicr-snapshot-<runID>`. Because `aicr validate` hands the *same* run ID to both the
snapshot agent and the validator, and both resolve to the same namespace, a shared
`aicr-snapshot-` prefix would put two owners on one object: under `--no-cleanup` the
validator adopts the agent's ConfigMap and overwrites its data and labels, silently
replacing the artifact `--no-cleanup` promised to keep. The distinct prefix keeps the
two subsystems' name spaces disjoint.

Because nothing aicr creates can already exist:

- `ensureJob`'s delete-and-recreate (`job.go:35-55`) and `waitForJobDeletion` are
  removed, as is the create-or-update logic in `ensureRole`, `ensureRoleBinding`,
  `ensureClusterRole`, and `ensureClusterRoleBinding`. Those `Update` calls were
  the mechanism by which one run rewrote another's permissions.
  `ensureServiceAccount` drops `IgnoreAlreadyExists`.
- An `AlreadyExists` on any create implies a 16-byte random collision or a caller
  pinning a duplicate `RunID`. Return `ErrCodeInternal`; it is a defensive
  assertion, not a supported state.
- **Permission isolation is structural.** Every run has its own ServiceAccount,
  so RBAC additivity across concurrent runs is impossible: a `DiscoverNetwork`
  run's mutating grants bind to an identity no other run uses.

**Name length.** The run ID is 32 characters, so a prefix truncates to 30 to keep
generated names within the 63-character ceiling imposed by the Job's
`batch.kubernetes.io/job-name` label. If 30 proves tight, halving the random half
of the run ID buys 8 more — a one-line change to the shared generator, not made
here.

### 3. Three lifecycle classes

| Class | Objects | Rule |
|---|---|---|
| **Run-owned** | Job, ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding, staging ConfigMap | Run-ID-suffixed. Created and deleted by this run. |
| **Ensured** | Namespace | Created if absent, labeled `managed-by`, never deleted, never suffixed. |
| **Delivered** | Explicit `cm://namespace/name` output ConfigMap | The user's artifact. Written on purpose, never deleted, never suffixed. |

At runtime aicr deletes an object if and only if it is run-owned. Exactly one
object kind falls in each of the other two classes. `tools/cleanup` sits outside
this boundary as a developer utility; its name-based removal of the legacy
`aicr-node-reader` pair is a one-time migration step, not run cleanup.

### 4. The Namespace stays user-specified

`--namespace` has no default and names a namespace the operator chose;
`ensureNamespace` (`rbac.go:30-79`) creates or labels it. It remains **ensured**
and is not renamed to `aicr-<runID>`:

- The documented workflow writes `cm://gpu-operator/aicr-snapshot` into a
  namespace the operator picked. A per-run agent namespace would need
  cross-namespace ConfigMap write permission for that artifact, reintroducing the
  broad grant this ADR removes.
- Pod Security Standards labels, ResourceQuotas, and the documented `aicr-agent`
  NetworkPolicy attach to the operator's namespace; a generated one inherits
  none of them.
- Namespace creation needs cluster-scoped permission some callers lack, and
  deletion adds finalizer latency to every run.

### 5. Pod selection by Job ownership

`Deploy` records the UID returned by the Job `Create`. `findPodName` and
`findOrWatchPodName` (`wait.go:111-149`) narrow by
`app.kubernetes.io/name=aicr,aicr.run/run-id=<runID>`, then confirm ownership.
Existing `DeletionTimestamp` / `PodFailed` / youngest-first filtering is retained
as a tiebreaker.

**Ownership must be established from the pod's controlling `ownerReference`, not
from a label.** Pod labels — `batch.kubernetes.io/controller-uid` included — are
writable by anything that can update pods in the namespace, so they narrow the
candidate set but cannot authorize it. The authority is a `controller: true`
`ownerReference` of kind `Job` carrying the recorded Job UID.

### 6. Cleanup is ownership-scoped

The `Deployer` records `(kind, name, UID)` on each successful `Create`; `Cleanup`
iterates that set and passes
`metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}`. A UID
mismatch is treated as "already replaced, not ours" and ignored. This matters
because `Cleanup` runs on the `Deploy` failure path (Problem, note 1).

`Cleanup` also deletes the internal staging ConfigMap when
`agentConfigMapTarget` reports the run does not own the user's output, carried by
a new `agent.Config.OwnsOutputConfigMap`. This closes the leak in Problem note 3,
which per-run naming would otherwise turn into one leaked object per run.
The verb this needs is covered by decision 10's gate.

The staging ConfigMap is created by the in-pod agent, not the controller, so its
UID is not captured at create time. It must be recorded when the controller reads
it so its deletion is UID-pinned like every other run-owned object.

### 7. Labels

Applied to all seven objects **and to the Job's pod template** — Job
`metadata.labels` do not propagate to pods, and decision 5's selector depends on
them:

- `app.kubernetes.io/name: aicr`
- `app.kubernetes.io/managed-by: aicr`
- `app.kubernetes.io/component: snapshot-agent`
- `aicr.run/run-id: <runID>`

`managed-by` is required, not decorative: `tools/cleanup:336-337` already sweeps
cluster-scoped RBAC with it, and without it every per-run ClusterRole would be
invisible to that tool. `component: snapshot-agent` is the **stable selector**
for consumers targeting agent pods across runs, replacing `job-name`-keyed
selectors whose value now changes per run.

### 8. Public contract and configuration schema

- `AgentConfig` (`pkg/client/v1/types.go:112-130`) gains `RunID string` and
  documents `JobName` and `ServiceAccountName` as prefixes, empty meaning "use
  the default". Additive; `make api-diff` stays green.
- `docs/integrator/go-library.md:208-222`, which sets both fields and states they
  are required, is rewritten to omit them.
- `CollectSnapshot`'s concurrency godoc (`aicr.go:1834-1836`) states what
  independence means — distinct objects, permissions, results, and cleanup — and
  names the one shared effect that remains: two runs targeting the same explicit
  `cm://` URI still overwrite one another, because that object is *delivered*.
- `pkg/config`'s `spec.snapshot.agent` and `spec.validate.agent` document the same
  prefix semantics; shipped examples pinning `jobName`/`serviceAccountName` are
  updated to omit them.
- The `aicr-agent` NetworkPolicy (`docs/integrator/automation.md:488-506`) moves
  its `podSelector` from `job-name: aicr` to `app.kubernetes.io/name: aicr` +
  `app.kubernetes.io/component: snapshot-agent`, shipped with this change.

### 9. Running as an existing ServiceAccount

`--service-account-name` is **exact-if-exists**: when a ServiceAccount of exactly
that name already exists in the namespace it is used verbatim; otherwise the value
is a prefix and the run creates `<prefix>-<runID>` as decision 2 describes.

In the exact case aicr **manages no permissions for that run** — no ServiceAccount,
Role, RoleBinding, ClusterRole or ClusterRoleBinding is created, bound or deleted.
Permissions are granted out of band by `--add-roles-to-service-account <name>`:
a separate generate-and-exit invocation that **renders** the generic,
non-run-scoped RBAC for that ServiceAccount into `snapshot-rbac-<runID>/` and
applies nothing. Applying it is the operator's own `kubectl apply`, and so is
teardown; no run's cleanup touches it. Combined with `--discover-network` it
renders the mutating discovery rules too.

Rendering rather than provisioning is the point. Under `--discover-network`
these rules include cluster-scoped mutating permissions that outlive any run,
and an operator consenting to that should be able to read exactly what they are
granting first. It also means the command needs no cluster and no privileges of
its own. The rules must be generated from the same definitions the run-scoped
Role and ClusterRole are built from, so a rendered manifest cannot drift from
what a run-owned grant carries.

**Why this exists.** IRSA and GKE Workload Identity both pin trust to the
ServiceAccount *name*: IRSA's trust policy conditions on
`system:serviceaccount:<ns>:<name>`, and GKE's IAM binding names the KSA as
`PROJECT.svc.id.goog[<ns>/<name>]` and accepts no wildcard. A per-run name cannot
be trusted by either, and copying the annotations onto a run-scoped ServiceAccount
does not help, because it is the name the provider refuses. Without this mode,
upgrading silently strips those identities' cloud credentials.

**What it costs.** This is the one mode that waives per-run permission isolation:
concurrent runs sharing an external ServiceAccount share its grants, and a
ServiceAccount granted the `--discover-network` rules carries mutating cluster
permissions permanently rather than for one run's lifetime. That is the trade the
mode exists to make, it is opt-in, and it is documented in
`docs/user/agent-deployment.md` so an operator meets it before choosing it.

### 10. One pre-flight gate for the whole run

Permission checking is a single authoritative gate that runs before anything is
created, and it fails the run when either subject is short of what the run will
actually do. Checking a subset is what let two separate failures through: an
identity holding `create` but not `delete` on the RBAC kinds passed, then leaked
a full run-scoped set — cluster-scoped objects included — on every run; and an
identity that could not read ServiceAccounts passed, then silently ran under a
generated account instead of the annotated one it was told to use.

Two subjects, because two identities matter. The **caller** must hold what the
run itself performs. The **agent ServiceAccount** must already hold the rules
the agent needs, which matters in decision 9's exact mode, where aicr grants
nothing and "you rendered the manifests but never applied them" should fail at
the gate rather than inside a pod minutes later. Answering for a subject other
than the caller requires a `SubjectAccessReview`, which is itself a privilege —
when the caller cannot create one, the gate must say the checks are unverified
rather than pass over them silently.

The required verb set depends on the mode, so the gate resolves the mode first.
That resolution is a read-only lookup, which keeps the ordering compatible with
failing before the first write. Exact mode must not demand the RBAC verbs at
all: aicr creates and deletes nothing there, and requiring them would lock out
precisely the operators decision 9 exists for.

Implementation must remember: report every missing permission in one failure,
naming the verb, the resource, the scope, and which of the two subjects lacked
it. An operator fixing a constrained identity should need one run to see the
whole list, not one run per missing verb.

## Non-Goals

- No change to the user-facing `cm://namespace/name` output contract.
- No new HTTP surface; `pkg/server` exposes no snapshot endpoint.
- No change to what the agent collects or to the in-pod agent binary.
- No automated sweep of cluster-scoped RBAC orphaned by a hard kill.
- No modification to `pkg/validator`'s run isolation; this ADR consumes its
  primitives after they move to neutral packages.
- No applying, and no teardown, of the RBAC `--add-roles-to-service-account`
  renders; both are the operator's own command (decision 9).

## Consequences

### Positive

- The documented concurrency contract becomes true, including for SDK callers
  following the current documented example.
- Every existing invocation still succeeds: no new flags, no new user-facing
  error codes, and no new way for a well-formed command to fail. The one
  observable behavior change is the ServiceAccount drift noted below.
- The `aicr validate` / `aicr snapshot` cross-command collision is fixed.
- Least-privilege improves: a `DiscoverNetwork` run's mutating cluster
  permissions bind to an identity that exists for one run and is revoked at
  cleanup.
- Two pre-existing leaks close: the staging ConfigMap, and cluster-scoped RBAC
  that `tools/cleanup` could not sweep.

### Negative

- **Object names are no longer predictable.** `kubectl logs job/aicr` stops
  resolving, and selectors keyed on `job-name` /
  `batch.kubernetes.io/job-name` match zero pods. A `podSelector` matching
  nothing is not an error in Kubernetes, so an out-of-tree NetworkPolicy written
  from the current docs silently stops fencing the agent. This is the sharpest
  edge in the change and needs a release-note callout.
- A prefix is capped at 30 characters before truncation.
- **Pointing at a pre-created ServiceAccount changes meaning.** A caller passing
  `--service-account-name` for an out-of-band ServiceAccount got adoption via
  `IgnoreAlreadyExists`. See decision 9: the flag is now *exact-if-exists*, so
  that workflow is supported again — but deliberately, rather than as a side
  effect of a create call that tolerated `AlreadyExists`.
- Two extra cluster-scoped objects are created and deleted per run.
- A hard kill still orphans a ClusterRole and ClusterRoleBinding, since
  cluster-scoped objects cannot have a namespaced owner.

### Neutral

- In-tree call sites hardcoding `aicr` are updated in the same change:
  `.github/actions/gpu-snapshot-validate/debug-snapshot-job.sh:23,27,35`,
  `tools/cleanup:351-354`, `tests/e2e/run.sh:1625`,
  `docs/user/agent-deployment.md:156,159,352,372`,
  `docs/integrator/automation.md:500`.
- The legacy unlabeled `aicr-node-reader` pair survives upgrade unreferenced; a
  name-based delete for it is added to `tools/cleanup`.

## Alternatives Considered

- **Per-run namespace `aicr-<runID>`** — cascade-deletes every namespaced object,
  but breaks the documented output-ConfigMap workflow and discards
  admin-applied namespace policy. See decision 4.
- **Capability-split fixed ClusterRoles with per-run bindings** — fewer objects,
  but turns "cleanup removes the RBAC" into "cleanup removes the binding".
- **Serialize runs with a coordination Lease** — cheapest, but downgrades the
  contract from independent to queued and adds a killed-process-holds-lease
  failure mode.

## Implementation Deliverables

1. `pkg/runid` and `pkg/k8s/labels`, with `pkg/validator` switched to them.
2. Prefix naming and label application in `pkg/k8s/agent`; removal of
   `ensureJob`'s delete-and-recreate, `waitForJobDeletion`, and the
   create-or-update logic in `rbac.go`.
3. Created-set tracking and UID-pinned `Cleanup`, including the staging ConfigMap.
4. Controller-UID pod selection in `wait.go`.
5. CLI flag default removal, `AgentConfig.RunID`, godoc and config-schema updates.
   The gate of decision 10, covering both subjects and both modes.
6. The NetworkPolicy selector change and the call-site updates listed under
   Consequences → Neutral.
7. Tests: an overlapping-run test with one run under `DiscoverNetwork` asserting
   distinct objects, distinct ClusterRole rules, correct per-run logs and result
   bytes, and cleanup that leaves the other run intact; plus prefix truncation,
   ownership-scoped cleanup, the adoption-drift warning, and pod selection
   rejecting a foreign-run pod. The fake clientset runs no Job controller and its
   watch ignores label selectors, so pods are created explicitly with the
   required labels and selector strings are asserted directly where
   apiserver-side filtering cannot be simulated.

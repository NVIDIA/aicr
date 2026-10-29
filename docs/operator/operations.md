# Day-2 operations

The operator acts on one cluster-scoped `ClusterRuntime` named `cluster`. Each
change it makes to the GPU stack is a cluster-scoped `RuntimeRollout` between
two plans, and each plan is an immutable ConfigMap in the operator namespace
(`aicr-system` below). Everything it decides can be read from those objects,
their conditions and their Events, without the operator's logs.

## Reading status

```shell
kubectl get clusterruntime cluster -o wide     # short name aicrrt; -o wide adds the active plan
kubectl describe clusterruntime cluster        # conditions, status and Events
kubectl get runtimerollouts                    # short name aicrro; cluster-scoped, so no -n
kubectl aicr describe "<rollout>"              # steps, verdicts, assessment report, approval
kubectl get configmaps -n aicr-system -l operator.aicr.run/role=plan
```

### Runtime conditions

| Condition | `True` means | Reasons to expect |
| --- | --- | --- |
| `Ready` | The target plan is rolled out, every release is ready at its current revision, and the rollout passed deployment validation when required | `RolledOut`; `ValidationDisabled` when `spec.validation.deployment` is `Disabled`. Not ready: `RollingOut`, `NotReady`, `NoActivePlan`, `Paused`, `EngineSuspended`, an engine reason, `PlanUnreadable`, `Deleting`. During a rollout, `RollingOut` names it and its progress: `rollout <name>: step 9/18 gpu-operator (Upgrade)`, `rollout <name> awaiting approval`, `rollout <name>: deployment validation run <id>` or `rollout <name> stalled: <message>`; before it exists, `the target plan is not rolled out yet`, with `; its stabilization window has not passed` while it stabilizes |
| `Available` | Every release of the active plan is ready | `Healthy`; not: `NotReady`, `NoActivePlan`. The message names the release that is not ready and why, as `release <id>: <reason>`. During a rollout, a release it already moved is checked at the rollout's target plan, one it is removing no longer counts, and the step in flight counts only once it fails. While the engine is suspended, `Available` reports `NotReady` whatever the stack's health |
| `Reconciling` | Work is in progress | `Resolving`, `Stabilizing`, `Assessing`, `RollingOut`, `Validating`. `False` with `RolledOut` only means nothing is in progress: it is also reported while paused or stalled |
| `Stalled` | Progress needs you; the reason says why ([troubleshooting](troubleshooting.md)) | `Progressing` when not stalled |
| `Resolved` | A plan exists for the current inputs | `Resolved`; `Resolving` while AICR Jobs run or a transient failure is retried; otherwise a terminal reason |
| `EngineAvailable` | Argo CD's CRDs and the project are present and, with the `aws` registry provider, the registry token is fresh | `EngineMissing`, `ProjectMissing`, `RegistryCredentialsStale` |

A failure carries its error code in the message, for example `[NOT_FOUND] plan
aicr-plan-1a2b… is missing`. A failure inside an AICR Job carries AICR's exit
code and the end of its error output: `aicr render exited 3: …`.

### Runtime status

| Field | Meaning |
| --- | --- |
| `status.activePlanDigest` | The plan of the newest `Succeeded` rollout: what is installed |
| `status.candidatePlanDigest` | The newest plan resolved from the current inputs |
| `status.rollout` | The rollout in progress, if any |
| `status.discovery` | The snapshot ConfigMap, the resolved criteria, and the accelerated and system node counts |
| `status.aicr` | The AICR version and image the resolver ran |

### Rollouts

`kubectl get runtimerollouts` shows each rollout's phase, approval state,
target plan (first 16 hex characters) and deadline. The phases are
`AwaitingApproval`, `Progressing`, `AwaitingAcknowledgement` (a `manual`
release is applied and waits for `kubectl aicr acknowledge` of its post-apply
steps), `Stalled` and `Validating`, and the terminal
`Succeeded`, `Failed`, `Expired`, `Revoked`, `Superseded` and `Orphaned`. A
rollout that has not started shows no phase (`kubectl aicr describe` prints
`Pending`). The terminal outcome is also set once as the
`operator.aicr.run/outcome` label, and a validated success carries its evidence
reference as the `operator.aicr.run/evidence` annotation.

`status.message` says why a rollout is where it is (`release gpu-operator:
HealthProgressing`, `no step starts: Paused`), `status.current` names the step
in flight, and `status.releases` the state of each release (`Progressing`,
`Ready`, `Failed`, `Suspended` for the step in flight while halted,
`Deleting`, `Removed`).

### Events

`ClusterRuntime` and `RuntimeRollout` are cluster-scoped, so their Events are
in the `default` namespace; `kubectl describe` shows them.

| Event | Type | On |
| --- | --- | --- |
| `RolloutCreated` | Normal | The runtime: a rollout, an `Adopt` rollout or a removal rollout was created |
| `RolloutSucceeded`, `RolloutSuperseded` | Normal | The rollout |
| `RolloutFailed`, `RolloutExpired`, `RolloutRevoked`, `RolloutOrphaned` | Warning | The rollout |
| `EngineSuspended`, `EngineResumed` | Normal | The runtime, naming the Applications changed |
| `RuntimeOrphaned` | Normal | The runtime, when deleted with `deletionPolicy: Orphan` |
| `PlanRebuilt` | Normal | The runtime, when a lost active plan was rebuilt from the Applications |
| `AICRRunFailed` | Warning | The runtime: an AICR run failed, naming its stage, run, attempt, retry class and error; once per run |
| `Stalled` | Warning | The runtime: it turned `Stalled`, or stalled for another reason; once per stall |
| `ReconcileFailed` | Warning | The runtime: a reconcile ended in an error the operator retries, such as an API server timeout. Conditions keep what was last observed; a step in flight still fails at its action timeout, naming the error |
| `FieldOwnershipOverridden` | Warning | An Argo CD Application whose hand edits the operator forced back, naming the other field managers |
| `ApplicationRecreated` | Warning | An Argo CD Application deleted by hand and written again; its resources stayed in place |
| `EngineSuspensionRefused` | Warning | The runtime: the API server refused for good to change an Application's automated sync for `spec.suspendEngine`, as for an Application edited outside the admission policy; the other Applications are changed and the operator keeps reconciling |
| `OrphanAnnotationRefused` | Warning | The runtime: while orphaning on deletion, Applications whose `orphaned-from` annotation was refused, such as by admission; they are left in place and the runtime is still released |

### Metrics

The operator serves Prometheus metrics on port 8080; the chart creates no
Service, so scrape the pod or port-forward
(`kubectl port-forward -n aicr-system deploy/aicr-operator 8080`):

| Metric | Meaning |
| --- | --- |
| `aicr_operator_rollout_outcomes_total{outcome}` | Rollouts ended, by outcome |
| `aicr_operator_job_duration_seconds{stage,result}` | AICR Job durations by stage and result |
| `aicr_operator_validation_runs_total{result}` | Deployment validation runs by result |
| `aicr_operator_job_runs_total{stage,result,class}` | AICR runs when first seen finished, by stage, result and, for failures, retry class (`never`, `bounded`, `transient`, `authorization`) |
| `aicr_operator_registry_token_expiry_timestamp_seconds`, `aicr_operator_registry_token_refresh_timestamp_seconds` | When the Argo CD registry token expires and when the operator last refreshed it, as Unix times (`aws` provider); `0` until known. Only the leader refreshes the token, so a standby replica stays at `0`: alert on `max()` across operator pods |

Every outcome, result, stage and retry class is exported at zero from
startup, so a rate reads zero rather than nothing before its first event.

## How a change reaches the cluster

1. **An input changes.** The runtime's `spec` (intent, platform, profile,
   criteria, placement, configuration, overrides), the AICR image (a chart
   upgrade), or the discovery snapshot. A new snapshot is taken by the first resolution of each
   UTC day, when the accelerated placement, the AICR image, the set of
   distinct accelerated node types, or the active plan's `gpu-operator`
   release changes, and on request.
2. **The resolver renders a candidate.** It runs `aicr snapshot`, `aicr
   recipe` and `aicr bundle` as Jobs, mirrors the charts into your registry,
   and writes the plan (`status.candidatePlanDigest`, `Resolved=True`). The same
   rendered output is the same digest, so nothing further happens.
3. **The candidate stabilizes** for `spec.rollout.stabilizationWindow`
   (default 10 minutes) from its creation (`Reconciling=True`, `Stabilizing`).
4. **The transition is assessed.** An `aicr upgrade-check` Job compares the
   active plan's recipe with the target's (`Assessing`). A blocked verdict, or
   a release that changes namespace or chart name, stops here with
   `TransitionBlocked`. A renamed release is a `Remove` of the old and an
   `Install` of the new.
5. **A rollout is created** from the assessment and the live Applications
   (`RolloutCreated`). Its steps, deadline and starting inventory are fixed.
6. **It waits for approval** when any step needs it (below).
7. **Steps run one at a time** in dependency order, each with one write to
   Argo CD, and each must be ready at its current revision within
   `spec.rollout.actionTimeout` (default 30 minutes). Removals run last, in
   reverse order. A `manual` release with recorded steps after the apply stops
   the rollout in `AwaitingAcknowledgement` until those steps are done and an
   approver runs `kubectl aicr acknowledge`.
8. **Deployment validation** runs once when `spec.validation.deployment` is
   `Required`, against a new snapshot taken after every step is ready.
9. **The rollout succeeds** and its target becomes the active plan. The whole
   rollout must finish within `spec.rollout.deadline` (default 6 hours) of its
   creation.

Changing `spec.rollout` affects rollouts created afterwards; a rollout keeps
the deadline and action timeout it was created with.

## Approving a rollout

A rollout needs approval when any step does:

| Step | Approval |
| --- | --- |
| Install, of the first plan or of an added component | Required |
| Upgrade to a new chart that AICR assesses `safe`, with no CRD change | Not required |
| Upgrade to a new chart assessed `manual`, `unknown` or `unversioned`, or without a verdict | Required |
| Upgrade with the same chart and new values (AICR assesses versions, not values) | Required (`Unknown`) |
| A new chart for a component whose old or new chart owns CRDs (Argo CD applies CRD changes on every sync, and prunes CRDs a new chart drops) | Required |
| Remove, adoption and runtime removal | Required |
| Verify (re-run a readiness gate) | Not required |

AICR has transition records for few components, so most upgrades need
approval. Approval is the approver's statement that workloads are prepared:
the operator never drains nodes, and GPU Operator owns driver-upgrade drains.
For an upgrade AICR assesses `manual`, approving also takes on the record's
precondition and every step but the apply, which is the only one the operator
performs; `kubectl aicr describe` lists them under "Recorded steps".

Approvers are the users and groups in the chart's `approvers` value. They hold
the `aicr-operator-approver` roles, which grant no `ClusterRuntime` edits, so
the person who changes the runtime cannot approve the result alone. With the
[plugin](cli-reference.md):

```shell
kubectl get runtimerollouts
kubectl aicr describe rollout-3f9a1c0d2b7e4a51-1
kubectl aicr approve rollout-3f9a1c0d2b7e4a51-1 --expires 2h --reason "Maintenance window"
```

`describe` shows the transition, every step with its operation, AICR verdict
and CRD change, and AICR's assessment report. An approval binds the rollout's
target plan, starting inventory (each Application's identity, plan and a
fingerprint of its source, values and destination), steps and deadline,
records the approver's username as the API server reports it, and can be
neither changed nor extended. If the inventory changes after approval,
including a hand edit of an Application's source, values or destination, the
rollout stalls with `InventoryDrift` instead of acting on a state nobody
approved. An edit of the release whose step is in flight instead stays
`SpecDrifted` until it is undone or the step's action timeout fails the
rollout ([troubleshooting](troubleshooting.md#rollout-stalled)).

When an approval expires, the step in flight finishes and the rollout ends
`Expired`. The rollout's own deadline still applies while it waits: a rollout
nobody approves ends `Expired` at its deadline.

## Revoking an approval

```shell
kubectl aicr revoke rollout-3f9a1c0d2b7e4a51-1 --reason "Training job not checkpointed"
```

Any approver can revoke, once. The step in flight finishes, no further step
starts, and the rollout ends `Revoked`. The record keeps who approved and who
revoked. A rollout that has ended or is in `Validating` can no longer be
revoked: every change has been made by then.

## Retrying

A `Failed`, `Expired` or `Revoked` rollout leaves the runtime `Stalled`
(`RolloutFailed`, `RolloutExpired`, `RolloutRevoked`) until the target changes
or you ask for a retry with a new token:

```shell
kubectl annotate clusterruntime cluster operator.aicr.run/retry="$(date +%s)" --overwrite
```

The retry creates a new rollout to the same target from the live state, which
needs a fresh approval. A retry while a rollout is `Stalled` (for example on
`InventoryDrift`) first ends it `Superseded`. A token acts once, even when the
stalled rollout recovers before the retry ends it; each retry needs a new
value. Releases a failed rollout already moved stay on its target
plan; nothing rolls back automatically.

## Pausing versus suspending the engine

| | `spec.paused: true` | `spec.suspendEngine: true` |
| --- | --- | --- |
| Purpose | Hold the operator's changes, for example during a maintenance freeze | Stop Argo CD itself from changing the stack, during an incident |
| New rollouts and steps | None start | None start |
| A step in flight | Not abandoned; Argo CD finishes its sync | Not abandoned; Argo CD finishes an operation it started |
| Argo CD automated sync and drift repair | Continue. With no rollout held, the operator also forces hand edits of active Applications back; with one held, an edit of a release's source, values or destination stalls it with `InventoryDrift` when it resumes | Stopped: the operator sets `syncPolicy.automated.enabled: false` on every owned Application |
| Approval | Kept; the rollout resumes under it | Kept |
| Runtime | `Ready=False` with `Paused` while a rollout is held or due; a runtime with nothing to do stays `Ready` | `Ready=False` with `EngineSuspended`, `Available=False`, the message naming the first release that drifted or is not healthy; `EngineSuspended` and `EngineResumed` Events |

```shell
kubectl patch clusterruntime cluster --type merge -p '{"spec":{"paused":true}}'
kubectl patch clusterruntime cluster --type merge -p '{"spec":{"suspendEngine":true}}'
```

Clearing the field resumes; neither deletes anything. Both are different from
revocation, which withdraws the approval itself.

Time paused or suspended does not count against the step in flight. The
operator keeps observing it without writing: its release reads `Suspended`,
`status.message` names its last observation, and a step that becomes ready
meanwhile is recorded. When you clear the field, the step's `actionTimeout`
starts again (`status.current.timeoutFrom`), so a release that never becomes
ready fails with its own reason a full timeout later. The rollout's deadline
keeps running throughout.

A stalled resolution, such as a cluster that no longer matches its criteria,
holds a rollout in progress at its next step boundary, as a pause does, and
the rollout's deadline keeps running.

## Rolling back by pinning

Nothing rolls back on its own. To go back to an earlier plan, pin it:

```shell
kubectl get configmaps -n aicr-system -l operator.aicr.run/role=plan \
  -o custom-columns='NAME:.metadata.name,DIGEST:.metadata.annotations.operator\.aicr\.run/plan-digest,CREATED:.metadata.creationTimestamp'
kubectl patch clusterruntime cluster --type merge -p \
  '{"spec":{"selection":{"policy":"Pinned","pinnedPlanDigest":"sha256:<digest>"}}}'
```

The pinned plan becomes the target. After the stabilization window, the
operator assesses the transition from the active plan and creates a rollout,
which needs approval like any other. A rollout's `spec.fromPlanDigest` names
the plan it replaced. The operator keeps the active, candidate and pinned
plans and the five most recent others; pinning a plan whose ConfigMap is gone
stalls with `NotFound`. Return to automatic selection with
`{"spec":{"selection":{"policy":"Automatic","pinnedPlanDigest":""}}}`.

A rollout is created only while the runtime's current inputs resolve
(`Resolved=True`). If an input change stalls resolution (an AICR upgrade that
fails, unavailable registry credentials), no pinned rollout starts until that
input is fixed or reverted; the active plan is unaffected meanwhile, since a
stalled resolution writes nothing.

Reverting the input that changed (an override, the AICR version) is the other
way back: when it renders the earlier output again, the candidate is the
earlier plan's digest.

## Changing inputs

- **Overrides** (`spec.overrides`) are AICR `--set` values per component. A
  path absent from the base render and the chart's defaults fails resolution
  with `OverridePathUnknown`, unless the override sets `additive: true`. A
  values change assesses `Unknown` and needs approval.
- **Criteria** (`spec.criteria`) declare what discovery cannot yet measure,
  such as the accelerator before the GPU stack is installed. They must agree
  with the snapshot (`CriteriaConflict`).
- **A new snapshot** on request:

  ```shell
  kubectl annotate clusterruntime cluster operator.aicr.run/resnapshot="$(date +%s)" --overwrite
  ```

## Upgrading AICR within v1

AICR is an installation value, not a runtime field. Upgrade it with a chart
upgrade that sets the version, the image and, with validation, the validator
images together:

```yaml
aicr:
  version: v1.x.y
  image: ghcr.io/nvidia/aicr:v1.x.y@sha256:<digest>
  validatorImages:          # with validation.enabled: every validator image AICR names, at that release's tag
    - ghcr.io/nvidia/aicr-validators/deployment:v1.x.y@sha256:<digest>
    - ghcr.io/nvidia/aicr-validators/conformance:v1.x.y@sha256:<digest>
    - ghcr.io/nvidia/aicr-validators/performance:v1.x.y@sha256:<digest>
```

For example, from v1.0.0 to v1.1.2, the newest release tested in CI:

```yaml
aicr:
  version: v1.1.2
  image: ghcr.io/nvidia/aicr:v1.1.2@sha256:611cbcb8a44b30ccfbbf77a797ed0a8e7fae228c97088a849f70b042709a62fb
  validatorImages:
    - ghcr.io/nvidia/aicr-validators/deployment:v1.1.2@sha256:f9d1cd2843ab0271d7b763c485558c1f3d4b37b984e96ad73e1ce6ae77365928
    - ghcr.io/nvidia/aicr-validators/conformance:v1.1.2@sha256:ce74905d3df0f6a5d8bb6c7f53e979fdf02e1d778b76817f98d964d9ad913312
    - ghcr.io/nvidia/aicr-validators/performance:v1.1.2@sha256:2d847aa5e4d0a547a86022eb5a71ec7ad8b5a801906578c77adedcdcb9083d07
```

The same chart upgrade moves validation to the v1.1 identities: the launcher
loses `cluster-admin`, the `aicr-validator` ServiceAccount gains it, and the
snapshot launcher loses its namespace grant. For
`h100-eks-ubuntu-training-kubeflow` the new recipe moves gpu-operator and nodewright-operator, which AICR assesses `safe`,
and nvsentinel, which it assesses `manual` with recorded steps, so the
rollout needs approval.

The chart refuses a version outside `>=1.0.0, <2.0.0`, and the operator stops
on an image that reports a version other than `aicr.version`
(`AICRVersionMismatch`). If the new AICR renders the same output, no rollout
results. Otherwise the new plan follows the usual path: stabilization,
assessment, approval. An AICR version the operator was not qualified with is in
range but unqualified ([support policy](support.md)).

To go back to an older AICR, pin an earlier plan first, while the newer AICR is
still installed ([rolling back by pinning](#rolling-back-by-pinning)), and lower
`aicr.version` only once that rollout has succeeded. Lowering it first stalls
assessment with `AICRInvalidRequest`: upgrade-check runs from the recipe the
live releases carry, and an older AICR rejects a recipe a newer one wrote (from
v1.1.2 to v1.0.0: `ownedPaths["nvsentinel"] must include synthetic path
"enabled"`). Nothing is written while it stalls; raising
`aicr.version` again clears it.

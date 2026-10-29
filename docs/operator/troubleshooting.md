# Troubleshooting

Runbooks by symptom. Each one starts from what the runtime reports, so begin
with its conditions:

```shell
kubectl get clusterruntime cluster \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
kubectl describe clusterruntime cluster   # adds the Events
```

`Stalled=True` means progress needs you, and its reason picks the runbook. A
`Resolved=False` reason other than `Resolving` stalls the runtime with the same
reason and message.

| Reason | Runbook |
| --- | --- |
| `NodeGroupEmpty`, `PlacementOverlap`, `AcceleratedGroupHeterogeneous`, `SnapshotIncomplete`, `CriteriaConflict` | [Discovery](#discovery) |
| `OverridePathUnknown`, `InvalidOverride`, `InvalidPlacement`, `NamespaceNotAllowed`, `ChartSourceUnsupported`, `ContentTooLarge`, `Conflict` | [Render and registry content](#render-and-registry-content) |
| `AICRInvalidRequest`, `AICRNotFound`, `AICRUnauthorized`, `AICRInternal`, `AICRTimeout` | [AICR Job failures](#aicr-job-failures) |
| `retrying after a transient failure` or `[SERVICE_UNAVAILABLE]` in the `Resolved` message, `RegistryCredentialsUnavailable`, `Unauthorized`, `RegistryCredentialsStale` | [Registry and credential outages](#registry-and-credential-outages) |
| `AICRVersionMismatch`, `AICRArtifactUnsupported` | [AICR version mismatch](#aicr-version-mismatch) |
| `EngineMissing`, `ProjectMissing` | [Argo CD not available](#argo-cd-not-available) |
| `AwaitingApproval`, `AwaitingAcknowledgement` | [Waiting for approval](#waiting-for-approval) |
| `TransitionBlocked` | [Transition blocked](#transition-blocked) |
| `InventoryDrift`, `StepUnknown` | [Rollout stalled](#rollout-stalled) |
| `RolloutFailed`, `RolloutExpired`, `RolloutRevoked`; `ForeignObject` on `Ready` | [Rollout failed, expired or revoked](#rollout-failed-expired-or-revoked) |
| `ValidationUnavailable` | [Validation](#validation) |
| `PlanUnreadable`, `PlanMissing`, `PlanOfAnotherRuntime`, `PlanWithoutRecipe`, `NotFound` | [Unreadable or missing plans](#unreadable-or-missing-plans) |
| `RemovalFailed`, or a runtime that stays `Terminating` | [Stuck finalizers](#stuck-finalizers) |
| `OrphanedObjectsPresent`, `AdoptionBlocked` | [Orphaned objects and re-adoption](#orphaned-objects-and-re-adoption) |

Nothing the operator refuses is written to Argo CD: a stalled resolution,
version mismatch, blocked transition or unreadable plan leaves the running
stack as it is.

## Discovery

The resolver checks the node groups and AICR's snapshot before it renders.

| Reason | Cause | Fix |
| --- | --- | --- |
| `NodeGroupEmpty` | No node matches `spec.placement.system` or `.accelerated` | Correct the selector, or label the nodes |
| `PlacementOverlap` | Nodes match both selectors; the message lists them | Make the selectors disjoint |
| `AcceleratedGroupHeterogeneous` | The accelerated nodes differ, or AICR sampled more than one GPU product. A node whose container runtime is restarting (`containerd://Unknown`) is not counted | One runtime manages one homogeneous profile; narrow the accelerated selector |
| `SnapshotIncomplete` | AICR measured no service, accelerator or OS; before the GPU stack is installed the accelerator is often unmeasured | Declare the missing dimension in `spec.criteria` |
| `CriteriaConflict` | The snapshot disagrees with `spec.criteria`, or with the criteria of the active plan | Correct `spec.criteria`, or find out why the cluster changed |

The latest snapshot is a ConfigMap in the operator namespace
(`status.discovery.snapshot.configMap`). Once the cause is fixed, the next
resolution picks it up; to re-run discovery now, request a snapshot:

```shell
kubectl annotate clusterruntime cluster operator.aicr.run/resnapshot="$(date +%s)" --overwrite
```

## Render and registry content

| Reason | Cause | Fix |
| --- | --- | --- |
| `OverridePathUnknown` | An override names a path absent from the base render and the chart's defaults, often a typo | Fix the path, or set `additive: true` on the override if the path is new on purpose |
| `InvalidOverride` | The override cannot be passed to AICR as a value | Fix the override's `value` |
| `InvalidPlacement` | A toleration cannot be passed to AICR | Fix `spec.placement` |
| `NamespaceNotAllowed` | The recipe renders a release into a namespace missing from the chart's `namespaces.components`; the message names each release | Add the namespace to `namespaces.components` with its Pod Security level and `helm upgrade` |
| `ChartSourceUnsupported` | A bundle release names no chart the operator can serve | A recipe the operator does not support yet; report it |
| `ContentTooLarge` | A values or recipe document exceeds a ConfigMap (1 MiB) | Reduce the overrides |
| `Conflict` | A chart tag in your registry already holds different bytes, or an upstream archive does not match its index digest | Upstream content changed after it was mirrored, or the registry was modified. Find out which before deleting the tag; the operator never overwrites it |

Fixing the runtime's `spec` starts a new resolution.

## AICR Job failures

Every AICR command runs as a Job: snapshots in `aicr-discovery`, recipe,
render and assessment in `aicr-jobs`, validation in `aicr-validation`. The
condition message carries AICR's exit code and the end of its error output:

```shell
kubectl get jobs -n aicr-jobs -L operator.aicr.run/stage,operator.aicr.run/input
kubectl logs -n aicr-jobs "job/<job>"
```

| Exit | Reason | Handling |
| --- | --- | --- |
| 2 | `AICRInvalidRequest` | Not retried; AICR refused the input (an unsupported OS, a recipe constraint). Change the input |
| 3 | `AICRNotFound` | Not retried; usually no recipe matches the criteria |
| 4 | `AICRUnauthorized` | Retried after a backoff, three runs in all, then stalled; usually registry permissions for a pushing Job, or a ServiceAccount recreated moments before |
| 5, 6, 7, 9 | none: `Resolving`, `retrying after a transient failure` | Retried with backoff from 10 seconds to 5 minutes, without a bound |
| 1, 8, other | `AICRInternal` | Three runs for the same input, then stalled |
| none (deadline, a pod that never started) | `AICRTimeout` | Three runs for the same input, then stalled. A pod evicted by a drain or node repair is replaced within the run and spends nothing. The message keeps the last reason the pod was not running (`last: Job <name> is not scheduled: …`) |
| none (`InvalidImageName`, `CreateContainerConfigError`) | `AICRInvalidRequest` | Not retried: the cluster can never start the container as configured. Fix the AICR image reference or the missing configuration, then request a snapshot |

While a run waits, the runtime's message names its Job and why its pod is
not running, instead of `AICR Jobs are running`:
`Job <name> is not scheduled: 0/9 nodes are available: …` (taints, node
selectors, capacity), `Job <name>: ImagePullBackOff: …` (registry access to
the AICR image), or `Job <name> has no pod` (a quota or Pod Security refusal;
`kubectl describe job <name>` shows it). AICR Jobs carry no node selector or tolerations unless
the chart's `aicr.jobs.nodeSelector` and `aicr.jobs.tolerations` give them, so
on a cluster whose nodes are all tainted, set those values.

Each failed run is a Warning Event `AICRRunFailed` on the runtime
(`kubectl describe clusterruntime cluster`). A stage that stalls keeps its
failed Jobs, with their pods' logs, for seven days instead of 24 hours, so
the record and the three-run bound outlive the usual TTL. A stalled stage is
not run again for the same inputs. After fixing a cause
outside the runtime (registry permissions, node capacity), request a snapshot
as above: each snapshot is new, so the snapshot, recipe and render stages run
again. The assessment (`upgrade-check`) is keyed on the two plans' recipes
instead; to run a stalled one again, delete its failed `aicr-assessment-*`
Jobs in `aicr-jobs`.

A successful Job is deleted once its output is read. A failed one stays, with
its pod and logs, until the inputs change or its TTL ends: 24 hours, or seven
days once its stage stalls.

## Registry and credential outages

**Registry unreachable.** Retried with backoff, and resolution resumes by
itself when the registry returns. What the runtime shows depends on who hit
it:

- an AICR Job (exit 6): the `Resolved` message reads
  `retrying after a transient failure: aicr <stage> exited 6: …`. The status stays `True` with
  the earlier plan while resolution only re-runs for changed inputs, and is
  `False` with `Resolving` after a `spec` change;
- the operator's own mirroring or pulls: `Resolved=False` with `Resolving` and
  the error, `[SERVICE_UNAVAILABLE] …`.

The active plan and the running stack are unaffected, but Argo CD cannot fetch
charts for new comparisons until the registry returns.

**`Unauthorized`** (`Resolved=False`, stalled). The registry refused the
operator's own push or pull (HTTP 401 or 403). Check the identity's policy and
the repository prefix in `registry.repository` ([installing on
EKS](eks-installation.md#2-the-operators-aws-identity)), then request a
snapshot to resolve again.

**`RegistryCredentialsUnavailable`** (`Resolved=False`). The operator could not
get registry credentials:

- `aws`: minting an ECR token failed. Check the operator's AWS identity: the
  EKS Pod Identity association or the IRSA annotation
  (`operator.serviceAccountAnnotations`), and the role's policy
  ([installing on EKS](eks-installation.md)).
- `static`: the docker config Secret `registry.auth.staticSecretRef` is missing
  or unreadable in the operator namespace. Recreate it there, and in the Job
  and validation namespaces.

Resolution resumes once credentials work.

**`RegistryCredentialsStale`** (`EngineAvailable=False`, `aws` only). Argo CD's
`repo-creds` Secret holds a token that expires within an hour, or none. The
operator refreshes it at start and every 6 hours; after a failed refresh it
retries with backoff from 10 seconds to 5 minutes, logging each failure. No new
rollout or step starts while it is stale. Fix the AWS identity; the next retry,
within 5 minutes, refreshes it. To refresh at once, restart the operator:

```shell
kubectl -n aicr-system rollout restart deployment/aicr-operator
kubectl -n argocd get secret aicr-registry \
  -o jsonpath='{.metadata.annotations.operator\.aicr\.run/token-expires-at}'
```

`aicr_operator_registry_token_expiry_timestamp_seconds` shows the same expiry
as a Unix time. Once a token has expired, Argo CD reports `ComparisonError` on
the Applications; running workloads are unaffected, and comparisons recover
after the refresh.

## AICR version mismatch

| Reason | Cause | Fix |
| --- | --- | --- |
| `AICRVersionMismatch` | `aicr.version` is outside `>=1.0.0, <2.0.0` or not a release version, or the AICR image reports a different version than `aicr.version` | Set `aicr.version` and `aicr.image` to the same release in one `helm upgrade` |
| `AICRArtifactUnsupported` | AICR produced output the operator cannot parse: an unknown `apiVersion` or a missing required field | Use a supported AICR release ([support policy](support.md)); report it if it is one |

## Argo CD not available

| Reason (`EngineAvailable=False`) | Cause | Fix |
| --- | --- | --- |
| `EngineMissing` | The Argo CD `Application` CRD is not served | Install Argo CD; the operator checks every 30 seconds and starts watching once it is |
| `ProjectMissing` | The `aicr` AppProject cannot be read | `helm upgrade` the operator chart to recreate it |

Nothing is written to Argo CD while the engine is unavailable. See
[Argo CD](argocd.md).

## Waiting for approval

`Stalled=True` with `AwaitingAcknowledgement` means a `manual` release is
applied and its record has steps after the apply: do them (`kubectl aicr
describe` lists them under "Recorded steps"), then `kubectl aicr acknowledge`
the rollout. Nothing further starts until then.

`Stalled=True` with `AwaitingApproval` means a rollout needs an approver.
Nothing has been written. Approve it with `kubectl aicr approve`
([operations](operations.md#approving-a-rollout)). A rollout nobody approves
ends `Expired` at its deadline.

If approval is refused, the message says why:

| Message | Fix |
| --- | --- |
| `[UNAUTHORIZED] read rollout <name>: … is forbidden` (plugin) | You are not an approver: add the user or group to the chart's `approvers` and `helm upgrade`, or bind them to the `aicr-operator-approver` roles yourself |
| `approving, revoking or acknowledging <rollout> needs update on runtimerollouts/approve` | You can patch rollouts but are not an approver; as above |
| `approvedBy must be the approver's own username` | Approve with the plugin, which records the username the API server reports |
| `acknowledge only while the rollout awaits acknowledgement` | The release is not applied yet; acknowledge once the rollout is `AwaitingAcknowledgement` |
| `rollout <name> needs no approval` (plugin) | Nothing to do; the rollout proceeds on its own |

## Transition blocked

`TransitionBlocked` means the move from the active plan to the target needs a
migration the operator does not perform: AICR assessed a component `blocked`,
or a release would change namespace or chart name. No rollout is created. Return to a plan without that change (revert the input, or
[pin](operations.md#rolling-back-by-pinning) the active plan); a supported
migration is a separate procedure, outside the operator.

## Rollout stalled

`kubectl aicr describe <rollout>` shows the message and the step in flight.

- **`InventoryDrift`.** Applications were deleted, created or recreated,
  moved to another plan, or edited after the rollout was created and
  approved; the message lists each change (`appeared`, `disappeared`,
  `was recreated`, `is at plan …`, `was edited`). An edit is a change to an
  Application's source, Helm values or destination, whether the rollout has
  not reached the release yet or already wrote it; its sync policy and
  status are not compared. The check repeats, so undoing the change resumes
  the rollout. Otherwise, a retry ends it `Superseded` and creates a new
  rollout from the current state, which needs a fresh approval. A hand edit
  of the release whose step is in flight is not written back: its step stays
  `SpecDrifted` until the edit is undone or its action timeout fails the
  rollout.
- **`StepUnknown`.** A step names a release the target plan lacks. Retry.

## Rollout failed, expired or revoked

The rollout's message and its `RolloutFailed`, `RolloutExpired` or
`RolloutRevoked` Event say why. Releases the rollout already moved stay on
its target plan, and the runtime stays `Stalled` until the target changes or a
[retry](operations.md#retrying).

| Message | Cause | Next |
| --- | --- | --- |
| `release <id>: SyncFailed` or `SyncError` | Argo CD's sync failed | `kubectl -n argocd get application <id> -o yaml`: read `status.operationState` and `status.conditions`; fix, then retry |
| `[CONFLICT] Application argocd/<name> exists without this runtime's ownership` | An Application with the release's name exists without the operator's labels and this runtime's UID annotation (`ForeignObject`); the operator never writes or deletes it | Remove or rename it, then retry. If an earlier runtime left it, [adopt](#orphaned-objects-and-re-adoption) instead. On an active-plan release it shows as `Ready=False` with `ForeignObject` |
| `release <id> not ready within 30m0s (<reason>)` | The release did not become ready at its current revision within `actionTimeout`; the reason is the last observation, such as `HealthProgressing`, `HealthDegraded`, or `SpecDrifted` when its Application was edited while the step ran | Inspect the component's workloads in its namespace; fix, then retry. Raise `spec.rollout.actionTimeout` if the component is slow but healthy |
| `release <id> not removed within …` | Argo CD did not finish deleting the release's resources | See [Argo CD Application](#argo-cd-application) |
| `the approval expired` | The approval expired before every step ran | Retry, and approve with a longer `--expires` |
| `the rollout deadline passed` | The whole rollout, including the wait for approval, exceeded `spec.rollout.deadline` | Retry; raise the deadline if the change needs more time |
| `the approval was revoked` | An approver revoked it | Retry when ready |

A step in flight while the runtime was paused or the engine suspended for
longer than `actionTimeout` can fail on resume
([operations](operations.md#pausing-versus-suspending-the-engine)).

## Validation

**`ValidationUnavailable`.** The rollout is in `Validating` and cannot run
deployment validation:

| Message | Fix |
| --- | --- |
| `deployment validation is not installed (validation.enabled)` | Install the validation identities (`validation.enabled: true` with `aicr.validatorImages`) or set `spec.validation.deployment: Disabled` |
| `plan <digest> records no recipe to validate against` | The rollout targets a plan rebuilt from Applications and was created by an operator that predates `PlanWithoutRecipe`; set `Disabled` for this rollout, or roll to a resolved plan. Current operators refuse such a rollout before any step |
| `taking the deployment snapshot` (rollout message, not a stall) | Validation waits for the snapshot the rollout takes after its last step; it runs in `aicr-discovery` like discovery's. If it fails, the rollout ends `Failed` with `the deployment snapshot failed: <cause>`: fix the cause as for a discovery snapshot, then retry |

**Validation failed.** The rollout ends `Failed`. AICR exits 8 when checks
fail, after pushing their evidence, and 2 when it refuses the recipe or its
readiness pre-flight. `status.validation` names the Job, the run and, when
evidence was pushed, its reference. Read the Job in `aicr-validation`, fix
the cause, and retry: a retry is a new rollout and a new run. Validation
checks the components the bundle deploys: it runs against the recipe the
bundle records, so a component an override turns off is not required.

A validator image that is not pinned in `aicr.validatorImages` at the tag AICR
names, or a pinned tag that resolves to another digest, also fails the run.

**Running AICR's other phases yourself.** Rollouts run only the deployment
phase. To run conformance or performance against the cluster with the AICR CLI,
give `aicr validate` its own namespace, not `aicr-validation`, so its Jobs and
evidence never mix with the operator's runs.

**`helm upgrade` fails with a server-side apply conflict** on
`app.kubernetes.io/managed-by` (or another `app.kubernetes.io` label) of
`aicr-discovery` or `aicr-validation`, with field manager `aicr`. AICR v1.0.x's
launchers, and the AICR CLI run by hand, relabel the namespace they run in,
and Helm 4 always applies its own `managed-by` label. Run the upgrade with
`--force-conflicts`; it takes back only the fields the chart declares. From
AICR v1.1.0 the operator's own runs leave namespace labels alone.

**`helm install` fails after an uninstall** with `Namespace "aicr-discovery"
... exists and cannot be imported into the current release: ... key
"app.kubernetes.io/managed-by" must equal "Helm": current value is "aicr"`.
The same relabeling left the kept namespaces with AICR's label, and on an
install `--force-conflicts` does not help: Helm checks the label before it
applies. Give the label back to Helm, then install:

```shell
kubectl label namespace aicr-discovery aicr-validation app.kubernetes.io/managed-by=Helm --overwrite
```

The operator's AICR v1.1.0 and later runs do not relabel, so this applies
only after AICR v1.0.x ran.

## Unreadable or missing plans

- **`PlanUnreadable`** with `rollouts written by a newer operator: …;
  upgrade the operator`. An operator was downgraded after a newer one wrote
  rollouts or plans in a newer schema. It writes nothing to Argo CD. Upgrade
  the operator again ([lifecycle](lifecycle.md#downgrade)); the newer objects
  are never rewritten down.
- **`PlanUnreadable`** with a message about a plan, such as `unsupported plan
  schema version label "2"`. A plan ConfigMap's schema label, payload or
  digest annotation disagree. Only the operator can create or update these
  ConfigMaps, so this follows a downgrade or a restore. Find it by its schema
  label:

  ```shell
  kubectl get configmaps -n aicr-system -l operator.aicr.run/role=plan -L operator.aicr.run/schema-version
  ```

  After a downgrade, upgrade again. Otherwise deleting the ConfigMap makes the
  operator re-resolve it, or rebuild it from the Applications when it is the
  active plan.
- **`PlanMissing`.** The active plan's ConfigMap is gone and cannot be written
  back: the Applications rebuild to a different digest (the message names
  both), so one was edited outside the operator, or they cannot be rebuilt at
  all (`is missing and cannot be rebuilt`, with the cause). The next rollout
  assesses every upgrade `unknown` and needs approval. A plan that rebuilds to
  the same digest is written back without intervention, with a `PlanRebuilt`
  Event.
- **`PlanOfAnotherRuntime`.** The target plan records the UID of an earlier
  runtime of the same name (the message names both), so its Applications
  would belong to that runtime. A pinned plan must be one this runtime
  resolved: pin another, or return to `Automatic`. A candidate is resolved
  again for this runtime, which replaces the ConfigMap.
- **`PlanWithoutRecipe`.** The target plan was rebuilt from the Applications,
  by adoption or after its ConfigMap was lost, so it has no recipe, and
  deployment validation is `Required`. Nothing was written. Change the inputs
  so that resolution reproduces the plan, which writes the resolved plan with
  its recipe in its place; pin another plan; or set
  `spec.validation.deployment: Disabled` for this rollout, which leaves it
  without validation evidence.
- **`NotFound`** with `plan aicr-plan-… is missing`. `spec.selection` pins a
  plan that retention has removed. Pin a plan that exists, or return to
  `Automatic`.

## Stuck finalizers

### ClusterRuntime

Deleting the runtime runs its `operator.aicr.run/cleanup` finalizer.

- **`deletionPolicy: Orphan`** (default) annotates every Application with
  `operator.aicr.run/orphaned-from`, ends any unfinished rollout `Orphaned`,
  records `RuntimeOrphaned` and removes the finalizer. If the runtime stays
  `Terminating`, check that the operator is running and that Argo CD's
  `Application` CRD is served; on this path a failure to list Applications is
  reported only in the operator's log. `PlanUnreadable` also blocks it.
- **`deletionPolicy: Delete`** creates a removal rollout that deletes every
  release in reverse order. It always needs approval (`AwaitingApproval`). It
  does not start while the runtime is paused or the engine suspended. If it
  fails or expires, the runtime is `Stalled` with `RemovalFailed` and keeps the
  finalizer until a [retry](operations.md#retrying).

Removing the finalizer by hand is safe for the stack: nothing has an owner
reference, so nothing cascades, and Argo CD keeps reconciling. The Applications
keep the old runtime's UID, and a new runtime stalls with
`OrphanedObjectsPresent` until it adopts them.

```shell
kubectl patch clusterruntime cluster --type merge -p '{"metadata":{"finalizers":null}}'
```

### Argo CD Application

An owned Application carries Argo CD's `resources-finalizer.argocd.argoproj.io`
only during an approved `Remove` step, together with the
`operator.aicr.run/delete-rollout` annotation. Argo CD then deletes the
release's resources and the Application.

- **Stuck in a `Remove` step.** Argo CD cannot delete a resource, often one
  held by its own finalizer. The step fails after `actionTimeout`. Fix the
  resource, then retry. Argo CD never deletes CRDs or resources annotated
  `helm.sh/resource-policy: keep`, so a removed component's custom resources
  stay; deleting them is your decision.
- **Added by hand.** The admission policy denies every operator write to an
  Application that carries the finalizer without the annotation, so the
  operator can no longer update it. Remove the finalizer. Deleting the
  Application with the finalizer in place would delete the release's
  resources.

  ```shell
  kubectl -n argocd patch application "<name>" --type merge -p '{"metadata":{"finalizers":null}}'
  ```

Deleting an owned Application without the finalizer leaves its resources
running. While no rollout is in progress the operator writes it again
(`ApplicationRecreated`); during a rollout, the deletion is
[drift](#rollout-stalled).

## Orphaned objects and re-adoption

A runtime that finds Applications annotated with another runtime's UID stalls
with `OrphanedObjectsPresent`, naming the UIDs and Applications. It never adopts
silently. This follows a new `ClusterRuntime` after an `Orphan` deletion or a
finalizer removed by hand. Deleting only the operator namespace does not cause
it: the same runtime rebuilds its active plan from the Applications
(`PlanRebuilt`).

```shell
kubectl -n argocd get applications -l app.kubernetes.io/managed-by=aicr-operator -o custom-columns=\
'NAME:.metadata.name,RUNTIME:.metadata.annotations.operator\.aicr\.run/runtime-uid,ORPHANED:.metadata.annotations.operator\.aicr\.run/orphaned-from,PLAN:.metadata.annotations.operator\.aicr\.run/plan-digest'
```

To adopt them, name the earlier runtime:

```shell
kubectl patch clusterruntime cluster --type merge -p '{"spec":{"adoption":{"runtimeUID":"<uid>"}}}'
```

The operator rebuilds the plan from the Applications alone (no plan
ConfigMap, recipe or operator namespace is needed) and creates an `Adopt`
rollout whose only change is the ownership annotations. Its `RolloutCreated`
Event says whether every Application was written by one plan or, after an
orphaning mid-rollout, by several. It needs approval. After it succeeds, the
runtime rolls forward to its candidate as usual; that first rollout cannot be
assessed by AICR (a rebuilt plan has no recipe), so every changed step is
`unknown` and needs approval. A later rollout back to the rebuilt plan cannot
be validated either; see `PlanWithoutRecipe` under
[Unreadable or missing plans](#unreadable-or-missing-plans).

**`AdoptionBlocked`** means the Applications do not rebuild into a plan: one
was edited outside the operator (values, chart or namespace), an annotation is
missing, or a dependency names an Application that is not there. The message
names the object. Undo the edit; the operator does not adopt a stack it cannot
verify. Applications of a runtime that `spec.adoption` does not name keep the
runtime stalled. Adopting a stack installed by the AICR CLI, without operator
annotations, is not supported.

To leave the stack unmanaged instead, delete the old Applications. Without the
resources finalizer their resources keep running, and nothing reconciles them
any more.

## When status does not explain it

A transient failure in the rollout controller, such as an Argo CD or API
server error, is retried with backoff and reported only in the operator's log;
the runtime's conditions keep their last values until the retry succeeds:

```shell
kubectl -n aicr-system logs deployment/aicr-operator
```

Log lines carry the runtime UID, plan digest, rollout and run ID, and never
values or credentials.

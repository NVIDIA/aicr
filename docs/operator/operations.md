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
| `Ready` | The target plan is rolled out, every release is ready at its current revision, and the rollout passed deployment validation when required | `RolledOut`; `ValidationDisabled` when `spec.validation.deployment` is `Disabled`. Not ready: `RollingOut`, `NotReady`, `NoActivePlan`, `Paused`, `EngineSuspended`, `EngineSuspensionRefused`, an engine reason, `PlanUnreadable`, `Deleting`. During a rollout, `RollingOut` names it and its progress: `rollout <name>: step 9/18 gpu-operator (Upgrade)`, `rollout <name> awaiting approval`, `rollout <name>: deployment validation run <id>` or `rollout <name> stalled: <message>`; before it exists, `the target plan is not rolled out yet`, with `; its stabilization window has not passed` while it stabilizes |
| `Available` | Every release of the active plan is ready | `Healthy`; not: `NotReady`, `NoActivePlan`. The message names the release that is not ready and why, as `release <id>: <reason>`. During a rollout, a release it already moved is checked at the rollout's target plan, one it is removing no longer counts, and the step in flight counts only once it fails. While the engine is suspended, `Available` reports `NotReady` whatever the stack's health |
| `Reconciling` | Work is in progress | `Resolving`, `Stabilizing`, `Assessing`, `RollingOut`, `Validating`. While `Assessing` or `Validating`, the message is set when an AICR Job waits: why its pod is not running, or `retrying after a transient failure: <cause>`. `False` with `RolledOut` only means nothing is in progress: it is also reported while paused or stalled |
| `Stalled` | Progress needs you; the reason says why ([troubleshooting](troubleshooting.md)) | `Progressing` when not stalled |
| `Resolved` | A plan exists for the current inputs | `Resolved`; `Resolving` while AICR Jobs run or a transient failure is retried; otherwise a terminal reason |
| `EngineAvailable` | Argo CD's CRDs and the project are present and, with the `aws` registry provider, the registry token is fresh | `EngineMissing`, `ProjectMissing`, `RegistryCredentialsStale`. When minting a new ECR token fails while `True`, the message names the failure and whether the operator still holds a token it can use |

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
steps, or of all its record's steps when the record names no `apply` step),
`Stalled` and `Validating`, and the terminal
`Succeeded`, `Failed`, `Expired`, `Revoked`, `Superseded` and `Orphaned`. A
rollout that has not started shows no phase (`kubectl aicr describe` prints
`Pending`). The terminal outcome is also set once as the
`operator.aicr.run/outcome` label, and a validated success carries its evidence
reference as the `operator.aicr.run/evidence` annotation.

`status.message` says why a rollout is where it is (`release gpu-operator:
HealthProgressing`, `no step starts: Paused`), `status.current` names the step
in flight, and `status.releases` the state of each release (`Progressing`,
`Ready`, `Failed`, `Suspended` for the step in flight while halted,
`Deleting`, `Removed`), with when its step started and ended (`startedAt`,
`finishedAt`) and the last reason it waited on (`waitedOn`), so a slow
rollout shows afterwards which step took the time and why.

### Events

`ClusterRuntime` and `RuntimeRollout` are cluster-scoped, so their Events are
in the `default` namespace; `kubectl describe` shows them.

| Event | Type | On |
| --- | --- | --- |
| `RolloutCreated` | Normal | The runtime: a rollout, an `Adopt` rollout or a removal rollout was created |
| `RolloutSucceeded`, `RolloutSuperseded` | Normal | The rollout |
| `StepStarted`, `StepWaiting` | Normal | The rollout: a release's step started, or waits on another reason than it last reported |
| `StepFinished` | Normal, or Warning when it or the rollout failed | The rollout: a release's step ended, or the rollout ended with it in flight, naming the outcome, how long it took and what it last waited on |
| `RolloutFailed`, `RolloutExpired`, `RolloutRevoked`, `RolloutOrphaned` | Warning | The rollout |
| `EngineSuspended`, `EngineResumed` | Normal | The runtime, naming the Applications changed |
| `RuntimeOrphaned` | Normal | The runtime, when deleted with `deletionPolicy: Orphan` |
| `PlanRebuilt` | Normal | The runtime, when a lost active plan was rebuilt from the Applications |
| `AICRRunFailed` | Warning | The runtime: an AICR run failed, naming its stage, run, attempt, retry class and error; once per run |
| `Stalled` | Warning | The runtime: it turned `Stalled`, or stalled for another reason; once per stall |
| `ReconcileFailed` | Warning | The runtime: a reconcile ended in an error the operator retries, such as an API server timeout. Conditions keep what was last observed; a step in flight still fails at its action timeout, naming the error |
| `FieldOwnershipOverridden` | Warning | An Argo CD Application whose hand edits the operator forced back, naming the other field managers |
| `ApplicationRecreated` | Warning | An Argo CD Application deleted by hand and written again; its resources stayed in place |
| `EngineSuspensionRefused` | Warning | The runtime: the API server refused for good to change an Application's automated sync for `spec.suspendEngine`, as for an Application edited outside the admission policy; the other Applications are changed, the runtime stalls with `EngineSuspensionRefused`, and the operator keeps reconciling |
| `OrphanAnnotationRefused` | Warning | The runtime: while orphaning on deletion, Applications whose `orphaned-from` annotation was refused, such as by admission; they are left in place and the runtime is still released |

### Metrics

The operator serves Prometheus metrics on port 8080; the chart creates no
Service. With the Prometheus Operator, set `operator.podMonitor.enabled` and
the `operator.podMonitor.labels` your Prometheus selects; the chart creates the
`PodMonitor` only while the cluster serves that kind, so on a new cluster run
`helm upgrade` again once the recipe has installed `prometheus-operator-crds`.
The stock AICR Prometheus keeps only the series its own consumers query, so
scrape the operator from your own. Otherwise scrape the pod, or port-forward
(`kubectl port-forward -n aicr-system deploy/aicr-operator 8080`):

| Metric | Meaning |
| --- | --- |
| `aicr_operator_rollout_outcomes_total{outcome}` | Rollouts ended, by outcome |
| `aicr_operator_job_duration_seconds{stage,result}` | AICR Job durations by stage and result |
| `aicr_operator_validation_runs_total{result}` | Rollouts whose deployment validation finished, by result. A run that is retried is not counted here; it is in `aicr_operator_job_runs_total` with class `transient` |
| `aicr_operator_job_runs_total{stage,result,class}` | AICR runs when first seen finished, by stage, result and, for failures, retry class (`never`, `bounded`, `transient`, `authorization`) |
| `aicr_operator_registry_token_expiry_timestamp_seconds`, `aicr_operator_registry_token_refresh_timestamp_seconds` | When the Argo CD registry token expires and when the operator last refreshed it, as Unix times (`aws` provider); `0` until known. Only the leader refreshes the token, so a standby replica stays at `0`: alert on `max()` across operator pods |
| `aicr_operator_registry_token_mint_failures_total` | Failed ECR token mints since the operator started (`aws` provider), including those it rode out on a still-valid token |
| `aicr_operator_runtime_stalled{reason}` | `1` while the runtime is `Stalled=True` with this reason, else `0`; the reasons are the [troubleshooting](troubleshooting.md) ones |
| `aicr_operator_rollout_approval_wait_seconds` | How long the rollout in progress has waited for approval, including while the engine is unavailable; `0` while none waits |
| `aicr_operator_runtime_resolved_timestamp_seconds` | When the operator last saw the runtime resolved for its current inputs, as a Unix time, checked every 30 seconds. It stops while a resolution runs or retries, including one that keeps an already resolved runtime `Resolved=True`, such as a snapshot refresh; `0` until it has seen one since the operator started |
| `aicr_operator_registry_failures_total{operation,code}` | Failed runtime registry and chart delivery operations (`resolve`, `pull_bundle`, `reuse_chart`, `mirror_oci_chart`, `mirror_http_chart`, `push_wrapper`, `sigstore_bundles`, `describe`, `push_statement`, `pull_statement`), by error code. `reuse_chart` with `NOT_FOUND` is a chart the operator mirrors again, not an outage |

Every outcome, result, stage, retry class, stall reason, registry operation
and error code is exported at zero from startup, so a rate reads zero rather
than nothing before its first event. Only the leader reconciles: the runtime
gauges of a standby replica stay at `0`, so alert on `max()` across operator
pods.

### Alerts

Example Prometheus rules; adjust the thresholds, and add the `severity` and
routing labels your Alertmanager expects. Each rule takes the `max()` across
the operator's replicas and keeps `namespace`, which names the installation;
when one Prometheus scrapes several clusters, add the label that names the
cluster to every `by (...)`, or one cluster's values hide another's:

```yaml
groups:
  - name: aicr-operator
    rules:
      # Progress needs someone. Approvals have their own alert below.
      - alert: AICROperatorRuntimeStalled
        expr: max by (namespace, reason) (aicr_operator_runtime_stalled{reason!~"AwaitingApproval|AwaitingAcknowledgement"}) == 1
        for: 15m
        annotations:
          summary: "The AICR runtime is Stalled ({{ $labels.reason }}); see docs/operator/troubleshooting.md"
      - alert: AICROperatorApprovalPending
        expr: max by (namespace) (aicr_operator_rollout_approval_wait_seconds) > 4 * 3600
        annotations:
          summary: "An AICR rollout has waited over 4 hours for approval"
      # A new installation fires this until its first plan resolves, and so
      # does an operator with no ClusterRuntime: 0 reads as long ago.
      - alert: AICROperatorNotResolved
        expr: time() - max by (namespace) (aicr_operator_runtime_resolved_timestamp_seconds) > 3600
        for: 10m
        annotations:
          summary: "The AICR runtime has not been resolved for its current generation in over an hour"
      # reuse_chart with NOT_FOUND is a chart mirrored again; CANCELED is a
      # shutdown.
      - alert: AICROperatorRegistryFailing
        expr: |
          sum by (namespace, operation, code) (
            increase(aicr_operator_registry_failures_total{code!="CANCELED"}[15m])
              unless on (operation, code) aicr_operator_registry_failures_total{operation="reuse_chart", code="NOT_FOUND"}
          ) > 0
        for: 15m
        annotations:
          summary: "AICR registry operation {{ $labels.operation }} keeps failing with {{ $labels.code }}"
```

### Liveness

The liveness probe (`/healthz` on port 8081) fails while a rollout reconcile
has run longer than 32 minutes: its 30-minute timeout plus 2 minutes to
return once canceled. The chart leaves the probe at the Kubernetes defaults,
so the kubelet restarts the operator container after three failed probes,
10 seconds apart; a standby replica, or the restarted operator, then takes
the lease. Reconciles that fail and retry, as through a registry outage, end
each time and never fail the probe. With leader election, the default, only
the leader reconciles, so a standby passes. The resolver has no reconcile
timeout and is not checked.

## How a change reaches the cluster

1. **An input changes.** The runtime's `spec` (intent, platform, profile,
   criteria, placement, configuration, overrides), the AICR image (a chart
   upgrade), or the discovery snapshot. A new snapshot is taken by the first resolution of each
   UTC day, when the accelerated placement, the AICR image, the set of
   distinct accelerated node types, or the active plan's `gpu-operator`
   release changes, and on request.
2. **The resolver renders a candidate.** It runs `aicr snapshot`, `aicr
   recipe` and `aicr bundle` as Jobs, mirrors the charts into your registry,
   and writes the plan (`status.candidatePlanDigest`, `Resolved=True`). A
   chart the candidate or active plan already mirrored is reused only when its
   upstream reference, version and target `registry.repository` all match.
   Otherwise it is requested from upstream. The same rendered output is the
   same digest, so nothing further happens.
3. **The candidate stabilizes** for `spec.rollout.stabilizationWindow`
   (default 10 minutes) from its creation (`Reconciling=True`, `Stabilizing`).
4. **The transition is assessed.** An `aicr upgrade-check` Job compares the
   active plan's recipe with the target's (`Assessing`). A blocked verdict, a
   release that changes namespace or chart name, or a `manual` record that
   holds more steps, or a longer step id, for acknowledgement than a rollout
   records stops here with `TransitionBlocked`. A renamed release is a
   `Remove` of the old and an `Install` of the new.
5. **A rollout is created** from the assessment and the live Applications
   (`RolloutCreated`). Its steps, deadline and starting inventory are fixed.
6. **It waits for approval** when any step needs it (below).
7. **Steps run one at a time** in dependency order, each with one write to
   Argo CD, and each must be ready at its current revision within
   `spec.rollout.actionTimeout` (default 30 minutes). Removals run last,
   dependents first as the Applications' `depends-on` and `readiness-for`
   annotations record them. A `manual` release with recorded steps after its
   `apply` step stops the rollout in `AwaitingAcknowledgement` until those
   steps are done and an approver runs `kubectl aicr acknowledge`. A record
   that names no `apply` step, as most do in AICR v1.3.0 and earlier, stops it
   there for all of its steps, since the operator cannot tell which follow the
   apply.
8. **Deployment validation** runs when `spec.validation.deployment` is
   `Required`, against a new snapshot taken after every step is ready. It is
   run again, at most three times in all, only when AICR could not finish a
   run and that run pushed no evidence of a failed check
   ([troubleshooting](troubleshooting.md#validation)).
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

A chart owns CRDs when it, or a subchart it packages, has a `crds/`
directory or a template or other file that declares one. A chart the
operator cannot scan in full counts as owning them: one with a template or
other file over 8 MiB, or with subcharts packaged more than two levels deep.
A component's chart too large for the operator to read at all stops
resolution instead; see [Render and registry
content](troubleshooting.md#render-and-registry-content).

AICR has transition records for few components, so most upgrades need
approval. Approval is the approver's statement that workloads are prepared:
the operator never drains nodes, and GPU Operator owns driver-upgrade drains.
For an upgrade AICR assesses `manual`, approving also takes on the record's
precondition and every step but the apply, which is the only one the operator
performs; `kubectl aicr describe` lists them under "Recorded steps". It
lists each `safe` record's summary there too: a `safe` upgrade with no CRD
change needs no approval of its own, but when it shares a rollout with one
that does, its summary can name a check to make before approving. `breaking`
beside a verdict means the versions cross a major, a minor below 1.0, or a
prerelease of the same version; it does not change whether a step needs
approval.

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
`InventoryDrift`) first ends it `Superseded`. A token acts once: if the
stalled rollout recovers before the retry ends it, the token is spent, and a
later stall needs a new one. Releases a failed rollout already moved stay on its target
plan; nothing rolls back automatically.

## Pausing versus suspending the engine

| | `spec.paused: true` | `spec.suspendEngine: true` |
| --- | --- | --- |
| Purpose | Hold the operator's changes, for example during a maintenance freeze | Stop Argo CD itself from changing the stack, during an incident |
| New rollouts and steps | None start | None start |
| A step in flight | Not abandoned; Argo CD finishes its sync | Not abandoned; Argo CD finishes an operation it started |
| Argo CD automated sync and drift repair | Continue. With no rollout held, the operator also forces hand edits of active Applications back; with one held, an edit of a release's source, values or destination stalls it with `InventoryDrift` when it resumes | Stopped: the operator sets `syncPolicy.automated.enabled: false` on every owned Application |
| Approval | Kept; the rollout resumes under it | Kept |
| Runtime | `Ready=False` with `Paused` while a rollout is held or due; a runtime with nothing to do stays `Ready` | `Ready=False` with `EngineSuspended`, `Available=False`, the message naming the first release that drifted or is not healthy; `EngineSuspended` and `EngineResumed` Events. An Application whose suspension the API server refuses keeps syncing, and `Ready` and `Stalled` report `EngineSuspensionRefused`, naming it |

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

An operator release's chart defaults to the AICR release of the same tag,
qualified with it in the release workflow. A release candidate's chart
defaults to that candidate's AICR and sets `aicr.allowPrerelease`. An earlier
chart, such as v1.2.0's, can default to an earlier AICR release; `helm show
values` shows the default.

The default applies only while your values set none of `aicr.version`,
`aicr.image` and `aicr.validatorImages`: Helm applies your values over the
chart's. If you set any of them, an operator upgrade keeps your AICR, a pairing
that release did not qualify, and pinning only `aicr.image` stalls resolution
with `AICRVersionMismatch`. On each operator upgrade, either remove all three
from your values or set all three from the new chart's `helm show values`.

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

For example, from v1.2.0 to v1.3.0, the newest release tested in CI:

```yaml
aicr:
  version: v1.3.0
  image: ghcr.io/nvidia/aicr:v1.3.0@sha256:4c7ac0f9461c14b7d9a4e86d7742b968a25149ed83bcbc52cd87e11d22871304
  validatorImages:
    - ghcr.io/nvidia/aicr-validators/deployment:v1.3.0@sha256:46f13d013f73280f216c531d6cb758ec15442aa832310635a0d62f81c7c50b51
    - ghcr.io/nvidia/aicr-validators/conformance:v1.3.0@sha256:1f40feb5e8be3f86dd9cf21cdda7701cf552bb915b8687c952de7604c9446e5a
    - ghcr.io/nvidia/aicr-validators/performance:v1.3.0@sha256:3e6377cffd2f640c7e201cadd1f18d6a87bcdcb38b1672c4626a89e0dacef98e
```

For `h100-eks-ubuntu-training-kubeflow` the new recipe adds nktp, in its own
`nktp` namespace: add `{name: nktp, podSecurity: privileged}` to
`namespaces.components` in the same chart upgrade, or the operator refuses the
plan (`NamespaceNotAllowed`) before any rollout. nktp also runs its own
metrics-server, which registers the cluster-wide
`v1beta1.metrics.k8s.io` APIService. If
`kubectl get apiservice v1beta1.metrics.k8s.io` shows that another provider
already serves it, such as the EKS metrics-server add-on, resolve that before
approving: either add a `spec.overrides` entry that turns nktp's off
(`component: nktp`, `path: metrics-server.enabled`, `value: false`), or remove
the other provider. The recipe also moves
nodewright-operator, which AICR assesses `safe`, and kube-prometheus-stack and
nvsentinel, which it assesses `manual` with recorded steps, so the rollout
needs approval. nodewright-operator's upgrade is `safe` but crosses a
breaking boundary, and its record's summary, which `kubectl aicr describe`
shows, names a check: its drains now evict kube-system pods, so before
approving confirm
`kubectl get pdb -n kube-system` shows ALLOWED DISRUPTIONS of at least 1 for
workloads on nodes NodeWright interrupts. Do nvsentinel's checks before
approving too; its record ends with its `apply` step, so nothing waits after
it. AICR v1.3.0's kube-prometheus-stack record names no `apply` step, so after
it is applied the rollout pauses for acknowledgement of all three steps. Do
`export` before approving; after the pause, do `clean-up`, then acknowledge.

From v1.0.x, the same chart upgrade also moves validation to the v1.1
identities: the launcher loses `cluster-admin`, the `aicr-validator`
ServiceAccount gains it, and the snapshot launcher loses its namespace grant.

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

# API reference

The `operator.aicr.run/v1alpha1` API and the operator's installation values.
Field names may change while the API is `v1alpha1`; ownership, mutability and
durability rules do not change without a design record.

## Conventions

- Group `operator.aicr.run`, not `aicr.run`. AICR uses `aicr.run/<version>` for
  its own artifact `apiVersion` values ([AICR ADR-011][aicr-adr-011]) and
  `aicr.run/*` label keys; a separate group keeps the two version policies apart.
- `v1alpha1` carries no conversion guarantee. A breaking change before `v1beta1`
  ships with a documented manual migration, not a conversion webhook.
- Admission uses CRD validation rules and ValidatingAdmissionPolicy only; no
  webhooks and no mutating admission.
- Both kinds are cluster-scoped. Durable decisions live in immutable `spec`
  fields, the set-once approval, operator-set outcome labels and immutable plan
  ConfigMaps; `status` holds observations that can be rebuilt from those and
  live objects.
- Plans are referenced by `desiredStateDigest` everywhere, never by object name.
- Engine objects, plan ConfigMaps and rollouts carry no `ownerReferences`.
  The one exception is an AICR Job's run-scoped objects (its output ConfigMap, input copies and
  credential Secret), which are owned by the Job in the same namespace so
  garbage collection removes them even when the operator is down.
- Every list, map and string in both CRDs has `maxItems`, `maxProperties` or
  `maxLength`, so validation rules stay within the CEL cost budget. Set-once and
  immutability rules sit on `spec`, which always exists, so `oldSelf` is always
  available.
- Infrastructure (AICR image, deployment engine, registry, namespaces,
  identities) is installation configuration in Helm values. `ClusterRuntime`
  holds only runtime intent and policy.

| Object | Written by | Mutability | Purpose |
| --- | --- | --- | --- |
| `ClusterRuntime` | Administrator | Spec mutable | Desired runtime intent; singleton `cluster` |
| `RuntimeRollout` | Operator (rollout controller); approver writes `spec.approval` | Spec immutable except the set-once `spec.approval` | One transition between plans, its steps, deadline and approval |
| Plan ConfigMap | Operator (resolver) | `immutable: true` | Rendered desired state for one digest; operator-internal, not an API kind |

## Installation values

The installation chart is `charts/aicr-operator`
([README](https://github.com/NVIDIA/aicr/blob/main/charts/aicr-operator/README.md)); its `values.schema.json`
rejects unknown keys. The operator namespace is the release namespace.

```yaml
aicr:
  version: v1.0.0                     # chart and operator require >=1.0.0, <2.0.0
  image: ghcr.io/nvidia/aicr@sha256:<digest>       # CLI and snapshot agent
  validatorImages: []                 # required with validation.enabled: each validator image as repository:tag@sha256:<digest>
  jobs:                               # placement of every AICR Job's pod; empty places none
    nodeSelector: {}
    tolerations: []
operator:
  image: ghcr.io/nvidia/aicr-operator@sha256:<digest>
  replicas: 1                         # a second replica waits on the leader-election Lease
  logLevel: info
  resources: {}                       # requests and limits; defaults in values.yaml
  serviceAccountAnnotations: {}       # e.g. the IRSA role ARN of the operator's AWS identity
engine:
  type: ArgoCD                        # the only engine in this release
  namespace: argocd                   # Argo CD control namespace
  project: aicr                       # AppProject created by the chart; nothing is written to argocd-cm
registry:
  repository: oci://<account>.dkr.ecr.<region>.amazonaws.com/aicr/<cluster>
  plainHTTP: false                    # test registries only
  auth:
    provider: aws                     # aws (ECR via EKS Pod Identity or IRSA) | static
    staticSecretRef: ""               # static: docker config Secret, same name in the operator, Job and validation namespaces; empty is anonymous
    repoCredsSecret: aicr-registry    # the Argo CD repo-creds Secret: created by the chart and refreshed by the operator (aws), or provided (static)
namespaces:
  discovery: aicr-discovery           # Pod Security privileged; snapshot agent only
  jobs: aicr-jobs                     # recipe, bundle and upgrade-check Jobs; restricted
  validation: aicr-validation         # created only when validation.enabled
  components: []                      # [{name, podSecurity: privileged|baseline|restricted, create: true}]; created with their Pod Security labels and kept on uninstall
discovery:
  agentRequests: ""                   # snapshot agent requests (name=quantity,...); empty keeps AICR's defaults
approvers:                            # bound to the approver roles; keep apart from ClusterRuntime editors
  users: []
  groups: []
validation:
  enabled: false                      # installs the validation launcher and, for AICR >= v1.1.0, the cluster-admin validator ServiceAccount
  serviceAccountAnnotations: {}       # the launcher's workload identity, e.g. to sign evidence with a KMS key
  signingKey: ""                      # AICR >= v1.1.0: KMS key URI (awskms://, gcpkms://, azurekms://, hashivault://) that signs evidence; empty pushes it unsigned
  transparencyLog: true               # false passes --tlog-upload=false, keeping key signatures out of the public Rekor log
development:
  bypassApprovals: false              # development builds only; a release build refuses to start with it set
  skipDiscovery: false                # development builds only: declared criteria, no node gates or snapshot
```

The chart fails to render when `aicr.version` is outside `>=1.0.0, <2.0.0`.
The operator also checks the version each AICR Job reports and stops on a
mismatch. `engine.type` accepts only `ArgoCD` in this release; the chart schema
and the operator reject any other value. CRDs, namespaces, the AppProject and
the `repo-creds` Secret survive `helm uninstall`.

## ClusterRuntime

```yaml
apiVersion: operator.aicr.run/v1alpha1
kind: ClusterRuntime
metadata:
  name: cluster                       # required singleton name
spec:
  intent: training                    # required AICR criteria value
  platform: kubeflow                  # optional AICR criteria value
  profile: ""                         # optional AICR configuration profile value (AICR ADR-015)
  criteria:                           # optional administrator-declared facts; must agree with discovery
    service: eks
    accelerator: h100
    os: ubuntu
  placement:                          # maps to AICR system/accelerated scheduling flags
    system:
      nodeSelector: {example.com/pool: system}
      tolerations: []
    accelerated:
      nodeSelector: {example.com/pool: gpu}
      tolerations:
      - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
  configuration: {}                   # optional AICR recipe configuration, such as gke.tcpxoInterfaces (below)
  selection:
    policy: Automatic                 # Automatic | Pinned
    pinnedPlanDigest: ""              # plan desiredStateDigest; required when policy is Pinned
  overrides:                          # typed values passed to AICR
  - component: gpu-operator
    path: dcgmExporter.enabled
    value: false
    additive: false                   # true allows a path absent from the base render and chart defaults
  rollout:
    stabilizationWindow: 10m
    actionTimeout: 30m
    deadline: 6h
  validation:
    deployment: Required              # Required | Disabled; Required needs validation.enabled at install
  deletionPolicy: Orphan              # Orphan | Delete
  adoption:
    runtimeUID: ""                    # explicit re-adoption of engine objects owned by this earlier runtime
  suspendEngine: false                # emergency stop of automated engine syncs; no step starts, no drift repair, Ready=False EngineSuspended
  paused: false                       # stops new rollout steps; observation continues
status:
  observedGeneration: 7
  conditions: []                      # see below
  aicr:
    version: v1.0.0                   # as reported by the AICR image
    image: ghcr.io/nvidia/aicr@sha256:<digest>
  discovery:
    snapshot: {configMap: <name>, digest: sha256:<...>, observedAt: <time>}
    criteria: {service: eks, accelerator: h100, os: ubuntu, intent: training, platform: kubeflow}
    acceleratedNodes: 32
    systemNodes: 3
  activePlanDigest: sha256:<...>
  candidatePlanDigest: sha256:<...>
  inputDigest: sha256:<...>           # resolver-owned cache: the inputs the candidate was resolved for
  rollout: <RuntimeRollout name>
```

Validation rules (CRD `x-kubernetes-validations`, stable since Kubernetes 1.29):

- Root: `self.metadata.name == 'cluster'` (root rules may read `metadata.name`).
- `spec.placement.*.nodeSelector`: at least one entry; system and accelerated
  selectors must differ.
- `spec.selection`: `pinnedPlanDigest` is non-empty if and only if
  `policy == 'Pinned'`.
- `spec.overrides`: unique `(component, path)`. Encoded value size is enforced
  at reconcile time.
- `spec.selection.pinnedPlanDigest`, when set, matches `^sha256:[a-f0-9]{64}$`;
  `spec.adoption.runtimeUID`, when set, is a UUID.

Bounds: `intent`, `platform`, `profile`, criteria values and `overrides[].component`
at most 63 characters; `overrides` at most 64 items with `path` at most 256
characters; `nodeSelector` at most 16 entries within Kubernetes label limits;
`tolerations` at most 16 items; `configuration.gke.tcpxoInterfaces` at most 16
items with unique `interfaceName` (at most 15 characters) and `network` at most
63 characters; `conditions` at most 16 items with messages at most 1024
characters.

**Recipe configuration.** `spec.configuration` mirrors AICR's recipe
`configuration`: cluster-specific desired state AICR records in the recipe
without a criteria axis. Today it holds the GKE GPUDirect-TCPXO interface
mapping, which recipes that ship the TCPXO training runtime
(`h100-gke-cos-training-kubeflow`) require. Provisioning decides it, so the
administrator copies it from the cluster:

```yaml
configuration:
  gke:
    tcpxoInterfaces:                  # passed as aicr recipe --gke-tcpxo-interfaces, in this order
    - {interfaceName: eth1, network: <cluster>-gpu-nic-0}
    - {interfaceName: eth2, network: <cluster>-gpu-nic-1}
    # ... through eth8
```

The CRD bounds only its shape. AICR validates the mapping when it resolves
the recipe (on a3-megagpu-8g: `eth1` to `eth8` exactly once, eight distinct
Device Network names containing `gpu-nic`), and a rejected mapping fails the
recipe Job with AICR's message. A change resolves again; it does not take a
new snapshot.

Conditions follow kstatus so Argo CD or other tools can health-check the runtime
itself:

| Type | Owner | Meaning |
| --- | --- | --- |
| `Ready` | Rollout controller | The candidate for the current generation is fully rolled out, current-revision ready, and validated when required |
| `Available` | Rollout controller | The active plan's releases are currently healthy; the message names the first release that is not |
| `Reconciling` | Rollout controller | Work is in progress for the current generation, including resolution reported through `Resolved` |
| `Stalled` | Rollout controller | Progress needs intervention (approval, conflict, blocked transition, version mismatch, expired deadline), including a resolver stall reported through `Resolved` |
| `Resolved` | Resolver | A plan was produced for the current generation and snapshot |
| `EngineAvailable` | Rollout controller | Engine CRDs and project are present |

While a rollout is in progress, `Ready=False` has reason `RollingOut` and a
message that names the rollout and where it is
(`rollout <name>: step 9/18 gpu-operator (Upgrade)`,
`rollout <name> awaiting approval`,
`rollout <name>: deployment validation run <id>`,
`rollout <name> stalled: <message>`); before
the rollout exists it says the target plan is not rolled out yet. `Available`
observes a release whose Application already carries another plan at that
plan, since a rollout moved it, whether the rollout is in progress or failed,
and no longer counts a release whose Remove step started; a release a rollout
installs counts once the rollout succeeds. After a failed rollout `Stalled`
reports the failure, and `Available` stays true while every release is healthy
at the plan it carries. The release of the step in flight leaves `Available` alone while it
progresses, as a rolling update leaves a Deployment available; the step's
action timeout reports one that never settles.

Each condition carries `observedGeneration`. The two controllers write disjoint
condition types with separate server-side apply field managers, which works only
because:

- `status.conditions` is a map list keyed by type (`+listType=map`,
  `+listMapKey=type`), so each entry has its own owner;
- each controller applies only the condition types it owns, never the full list
  read back from the object. Applying the full list would transfer ownership of
  every entry, and a later apply without an entry would remove it.

The resolver owns only `Resolved` and reports its failures there, with AICR's
exit category as the reason. The rollout controller derives the kstatus
`Reconciling` and `Stalled` conditions from its own state and from `Resolved`,
so no condition type has two writers.

## Plan ConfigMap

Plans are operator-internal: only the resolver writes them, only the operator
reads them, and they have no status. They are immutable ConfigMaps in the
operator namespace rather than an API kind, so their format evolves inside the
operator through `schemaVersion`, without CRD versions or conversion.
They are ConfigMaps, not Secrets,
because plans and values never hold credentials.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: aicr-plan-<first 16 hex of desiredStateDigest>
  namespace: aicr-system
  labels:
    app.kubernetes.io/managed-by: aicr-operator
    operator.aicr.run/runtime: cluster
    operator.aicr.run/role: plan
    operator.aicr.run/schema-version: "1"    # must equal schemaVersion in the payload
  annotations:
    operator.aicr.run/runtime-uid: <ClusterRuntime UID>
    operator.aicr.run/plan-digest: sha256:<desiredStateDigest>
immutable: true
data:
  plan.json: <plan document, canonical JSON>
```

The plan document, shown as YAML:

```yaml
schemaVersion: 1
runtimeUID: <ClusterRuntime UID>
engine: ArgoCD
desiredStateDigest: sha256:<...>    # identity: names the ConfigMap and keys rollouts
inputDigest: sha256:<...>           # provenance only
provenance:
  source: Resolved                  # Resolved (AICR Jobs) | Rebuilt (from live engine objects)
  operatorVersion: <version>
  aicrVersion: v1.0.0
  aicrImage: ghcr.io/nvidia/aicr@sha256:<digest>
  snapshotDigest: sha256:<...>
  bundle: oci://<registry>/bundles/<runtime>@sha256:<digest>
  runtimeGeneration: 7
  renderFlags: [--deployer, helm, --readiness-hooks, --accelerated-node-selector, pool=gpu]  # the render's other flags
recipe:                             # absent on a Rebuilt plan
  name: h100-eks-ubuntu-training-kubeflow
  criteria: {service: eks, accelerator: h100, os: ubuntu, intent: training, platform: kubeflow}
  configMapRef: {configMap: aicr-recipe-<digest prefix>, digest: sha256:<...>}            # the recipe Job's output: upgrade-check and reproduce
  validationConfigMapRef: {configMap: aicr-recipe-<digest prefix>, digest: sha256:<...>}  # the bundle's recipe: deployment validation
  warnings: []                      # bounded AICR bundle notes and warnings
releases:                           # AICR normative order; DAG edges explicit
- id: gpu-operator
  component: gpu-operator
  phase: Primary                    # Pre | Primary | Post | Readiness
  dependsOn: [nfd, cert-manager, kube-prometheus-stack, nodewright-customizations]
  targetNamespace: gpu-operator
  chart:
    source: Mirrored                # Mirrored (upstream) | Wrapper (rendered local chart)
    repository: oci://<registry>/charts/gpu-operator
    version: <chart version pinned by AICR>
    digest: sha256:<OCI manifest digest in the runtime registry>
    upstream:
      repository: <AICR registry chart repository>
      archiveDigest: sha256:<index digest or upstream layer digest>
  values: {configMap: aicr-values-gpu-operator-<digest prefix>, digest: sha256:<...>}
  ownsCRDs: true
- id: gpu-operator-readiness
  component: gpu-operator
  phase: Readiness
  readinessFor: gpu-operator
  dependsOn: [gpu-operator]
  targetNamespace: gpu-operator
  chart:
    source: Wrapper
    repository: oci://<registry>/wrappers/gpu-operator-readiness
    version: <chart version>
    digest: sha256:<...>
  values: {configMap: aicr-values-gpu-operator-readiness-<digest prefix>, digest: sha256:<...>}
  ownsCRDs: false
```

Bounds: at most 128 releases, with release IDs of at most 53 characters (the
Helm release-name limit), stored as plain JSON (no compression) so
`kubectl get -o yaml` stays readable; a plan that does not fit one ConfigMap
(1 MiB) is rejected. The recipe and values live in their own immutable
ConfigMaps, so the plan stays small. A plan keeps two recipes: the one AICR
resolved, which `upgrade-check` compares and `kubectl aicr reproduce` renders
from, and the one the bundle records after AICR applied the render flags,
which deployment validation runs against, so a component an override turns
off is not required. A plan written before it recorded `validationConfigMapRef`
validates against `configMapRef`. Neither reference is part of the plan's
digest. Every chart is served from the runtime
registry and pinned by manifest digest; mirrored charts also record the verified
upstream archive digest.

Rules:

- The resolver creates the ConfigMap once. `AlreadyExists` with a matching
  plan-digest annotation is success; a mismatch is a conflict.
- A plan belongs to the runtime UID it records. A matching plan that records
  another runtime's UID, left by an earlier runtime, is replaced rather than
  reused, and a plan recording another UID is never rolled out (`Stalled` with
  `PlanOfAnotherRuntime`).
- A resolved plan replaces a `Rebuilt` plan of its digest; a rebuilt plan never
  replaces a resolved one. A plan without a recipe is not rolled out while
  deployment validation is `Required` (`Stalled` with `PlanWithoutRecipe`).
- `immutable: true` freezes `data`; labels and annotations stay writable. A
  ValidatingAdmissionPolicy limits creating and updating operator-labeled
  ConfigMaps in the operator namespace to the operator's ServiceAccount.
- The operator reads a plan only when the `schema-version` label is a version it
  supports and equals `schemaVersion` in the payload, and the payload's
  `desiredStateDigest` equals the plan-digest annotation. Anything else sets
  `Stalled` with reason `PlanUnreadable`; the operator never guesses.
- A plan ConfigMap is a cache of state the engine objects also carry. If it is
  lost, the plan is re-resolved (candidate) or rebuilt from the live engine
  objects (active, adopted), as [re-adoption](troubleshooting.md#orphaned-objects-and-re-adoption) describes.
- List plans with `kubectl get configmaps -n aicr-system -l operator.aicr.run/role=plan`.

## RuntimeRollout

```yaml
apiVersion: operator.aicr.run/v1alpha1
kind: RuntimeRollout
metadata:
  name: rollout-<transition>-<n>      # deterministic; see naming below
  labels:
    operator.aicr.run/runtime: cluster
    operator.aicr.run/to-plan: <first 16 hex of toPlanDigest>
    operator.aicr.run/transition: <transition>
    operator.aicr.run/outcome: Succeeded    # set once by the operator when the rollout ends
  annotations:
    operator.aicr.run/evidence: oci://<ref>@sha256:<...>   # set once with a Succeeded outcome when validation ran
spec:                                 # immutable, except the set-once approval below
  schemaVersion: 1
  runtimeUID: <uid>
  fromPlanDigest: sha256:<...>        # empty for initial install
  toPlanDigest: sha256:<...>
  startingInventoryDigest: sha256:<...>
  startingInventory:                  # the owned engine objects the digest covers; drift is judged against them
  - {releaseID: gpu-operator, name: gpu-operator, uid: <uid>, planDigest: sha256:<...>, revision: sha256:<...>, specDigest: sha256:<...>}
                                      # specDigest fingerprints the Application's source, Helm values and destination
  assessmentRef: {configMap: aicr-upgrade-check-<run id>, digest: sha256:<...>}
  deadline: "2026-10-15T04:00:00Z"
  actionTimeout: 30m
  approvalRequired: true
  steps:
  - release: gpu-operator
    operation: Upgrade                # Install | Upgrade | Verify | Remove | Adopt
    assessment: Safe                  # AICR upgrade-check verdict or NotApplicable
    crdChange: false
  - release: gpu-operator-readiness
    operation: Verify                 # re-run the readiness gate for the new parent revision
    assessment: NotApplicable
    crdChange: false
  - release: kai-scheduler
    operation: Upgrade
    assessment: Manual
    crdChange: false
    postApplySteps: [reset-breaker, verify]   # a manual record's steps after the apply; acknowledged before the plan is active
  - release: nvsentinel
    operation: Remove                 # after installs and upgrades, in reverse active-plan order
    assessment: NotApplicable
    uid: 3f2b…                        # the engine object to delete; set exactly on Remove steps
  approval:                           # written by an approver, never by the operator
    approvedBy: alice@example.com     # must equal the requester's username
    expiresAt: "2026-10-15T06:00:00Z"
    reason: Scheduled maintenance window
    revokedBy: ""                     # set once by any approver; ends the rollout
    revokedAt: ""                     # informational, written by the client
    revokeReason: ""
    acknowledgements:                 # append-only; one per release with postApplySteps, while AwaitingAcknowledgement
    - {release: kai-scheduler, acknowledgedBy: bob@example.com, acknowledgedAt: <time>}
status:
  phase: Succeeded                    # the rollout state machine; AwaitingAcknowledgement waits on postApplySteps
  observedGeneration: 2
  conditions: []
  approval: Approved                  # NotRequired | Pending | Approved | Bypassed | Revoked | Expired
  current: {release: gpu-operator-readiness, operation: Verify, startedAt: <time>}
                                      # timeoutFrom: <time> once a halt ended; the action timeout runs from it
  releases:                           # state: Pending | Progressing | Ready | Failed | Suspended (step in flight while halted) | Deleting | Removed | Unknown
  - {id: cert-manager, state: Ready, revision: sha256:<...>, specDigest: sha256:<...>}   # specDigest: the spec this rollout wrote, from when it became ready
  - {id: nvsentinel, state: Removed}
  validation: {job: <name>, runID: <id>, result: Passed, evidence: oci://<ref>@sha256:<...>}
  message: ""
```

Naming. `<transition>` is the first 16 hex characters of SHA-256 over the
runtime UID, `fromPlanDigest` and `toPlanDigest`. Before creating a rollout,
the controller lists the runtime's rollouts through the uncached reader,
keeping only those whose `spec.runtimeUID` is the current runtime's. A
non-terminal rollout is the one in progress, and no new one is created.
Otherwise `<n>` is one more than the highest suffix among the listed rollouts
carrying that transition label. A retried create after a lost response finds
the first through the uncached list. Two concurrent creators collide on the
name even when they read live state at different times, because neither the
name nor the suffix depends on it: a creator whose list predates another's
create derives that rollout's name. `AlreadyExists` triggers a fresh list,
never a second object. The live inventory is recorded in
`startingInventoryDigest`, not in the name.

Outcome. When a rollout ends, the operator sets the `outcome` label
(`Succeeded`, `Failed`, `Expired`, `Revoked`, `Superseded` or `Orphaned`) and, for a
validated success, the `evidence` annotation. Both are set once, only by the
operator, and survive the loss of
status, so the active plan and terminal states can be rebuilt after a restore.

CRD validation rules:

- Every `spec` field other than `approval` is immutable (`self == oldSelf`).
- `approval.approvedBy`, `expiresAt` and `reason` are set together and once.
- `approval.revokedBy`, `revokedAt` and `revokeReason` are set together and
  once, only after `approvedBy`. An approval can be neither cleared nor
  extended.
- `approval.acknowledgements` are append-only, one per release, and need an
  existing approval.

Bounds: `steps` at most 256 items (128 target releases plus removals from the
active plan); `status.releases` at most 256 items; release IDs at most 53
characters; `approvedBy` and `revokedBy` at most 1024 characters, which covers
IAM-derived usernames; `reason`, `revokeReason` and `message` at most 1024
characters; `conditions` at most 16 items.

### Approval

Approval is the set-once `spec.approval` on the rollout it authorizes. The rest
of the spec already fixes the target plan digest, starting inventory, steps and
deadline, so the approval is bound to all of them without copying them.
CRDs support only the
`status` and `scale` subresources, so there is no `/approve` endpoint. RBAC grants
`update` on the virtual subresource `runtimerollouts/approve`, which RBAC matches
as a string, and a ValidatingAdmissionPolicy checks it on every UPDATE that
changes `spec.approval`:

- `authorizer.group('operator.aicr.run').resource('runtimerollouts').subresource('approve').name(object.metadata.name).check('update').allowed()`.
  Approvers are granted this separately from `ClusterRuntime` editing, so
  editors of the runtime cannot approve their own changes.
- A newly set `approvedBy` or `revokedBy` equals `request.userInfo.username`, so
  the object carries a verified identity without an audit-log lookup.
- Revocation is rejected when the rollout carries an `outcome` label or its
  status phase is `Validating`. Status can be empty after a restore, so the
  controller also ignores a revocation that arrives after the last step
  finished: a revoked or expired approval ends the rollout only before a step
  whose mutation this rollout has not made, judged from the engine objects
  (the spec written, or a sync since the rollout was created).
- The request changes nothing but `spec.approval`; the operator's own identity
  can never write it, and a rollout cannot be created with it.
- Only the operator's identity creates rollouts, so nobody else can create one
  that skips approval with `approvalRequired: false` or an `outcome` label
  already set.

To restore RuntimeRollouts from a backup under another identity, as Velero
does:

1. Stop the operator, so no rollout advances while admission is relaxed:
   `kubectl -n aicr-system scale deployment/aicr-operator --replicas=0`.
2. Delete the `aicr-operator-approval` ValidatingAdmissionPolicyBinding.
3. Restore the rollouts.
4. Run `helm upgrade` on the operator chart with the same values. It recreates
   the binding and restores the operator's replica count.

A restored rollout comes back without its status. The operator rebuilds its
phase from the `outcome` label and records each release in its steps as
`Unknown`, because which ones it wrote is lost. Until a later rollout succeeds,
a missing Application of such a release stalls the runtime with
`MovedReleaseMissing` instead of being repaired from the active plan.

Approvers also need `get`, `list`, `watch` and `patch` on `runtimerollouts`,
and `get` on ConfigMaps in the operator namespace to read assessment reports.
The chart's `aicr-operator-approver` roles grant exactly that plus reading the
runtime, bound to `approvers.users` and `approvers.groups`; the policy
(`aicr-operator-approval`) is kept on uninstall. They
write their own username: a ValidatingAdmissionPolicy cannot set fields, and
MutatingAdmissionPolicy is beta and disabled by default at the Kubernetes 1.34
floor (GA in 1.36). The `kubectl aicr` plugin fills it from `kubectl auth
whoami`:

```sh
kubectl aicr describe rollout-3f9a1c0d2b7e4a51-1   # steps, verdicts, CRD changes, assessment report, drift
kubectl aicr approve  rollout-3f9a1c0d2b7e4a51-1 --expires 2h --reason "Scheduled maintenance window"
kubectl aicr revoke   rollout-3f9a1c0d2b7e4a51-1 --reason "Training job not checkpointed"
kubectl aicr reproduce                             # the active plan's recipe and the AICR CLI command that renders its bundle
```

Rollout names are computed at runtime, so approvals are made against the live
object, not committed through GitOps.

## Engine object metadata

The operator uses one Argo CD `Application` per plan release in the engine
namespace.
Every engine object, and every
operator-owned ConfigMap or Job, carries:

| Key | Kind | Value |
| --- | --- | --- |
| `app.kubernetes.io/managed-by` | label | `aicr-operator` |
| `operator.aicr.run/runtime` | label | `cluster` |
| `operator.aicr.run/runtime-uid` | annotation | ClusterRuntime UID |
| `operator.aicr.run/plan-digest` | annotation | `desiredStateDigest` that last wrote the object |
| `operator.aicr.run/release-id` | annotation | Plan release ID |
| `operator.aicr.run/run-id` | label | AICR Job run ID (Jobs and their run-scoped objects) |
| `operator.aicr.run/stage` | label | AICR stage an operator Job runs |
| `operator.aicr.run/input` | label | First 16 hex of the resolution `inputDigest` (AICR Jobs, recipe and plan ConfigMaps) |
| `operator.aicr.run/digest` | annotation | Content digest of a recipe or values ConfigMap |
| `operator.aicr.run/attempt` | annotation | Run number of an AICR Job within its stage and input; orders runs |
| `operator.aicr.run/token-expires-at` | annotation | Expiry of the registry token in the engine's `repo-creds` Secret (aws provider) |
| `operator.aicr.run/role` | label | `plan`, `recipe`, `values`, `snapshot` or `assessment` (operator ConfigMaps) |
| `operator.aicr.run/schema-version` | label | Plan document schema version (plan ConfigMaps) |
| `operator.aicr.run/orphaned-from` | annotation | Runtime UID, set only by Orphan deletion |

Engine objects also carry the plan graph, so a plan can be rebuilt from live
state:

| Key | Kind | Value |
| --- | --- | --- |
| `operator.aicr.run/phase` | annotation | `Pre`, `Primary`, `Post` or `Readiness` |
| `operator.aicr.run/depends-on` | annotation | Sorted, comma-separated release IDs |
| `operator.aicr.run/readiness-for` | annotation | Parent release ID (readiness releases) |
| `operator.aicr.run/owns-crds` | annotation | `true` or `false` |
| `operator.aicr.run/adopted-from` | annotation | Earlier runtime UID an `Adopt` rollout moves objects from (rollouts) |
| `operator.aicr.run/release-digest` | annotation | Digest of the release identity the object was written from, so a rebuild names an object edited out of band |
| `operator.aicr.run/delete-rollout` | annotation | Name of the rollout whose approved `Remove` step is deleting the object; present only with the Argo CD resources finalizer |

Every other input to `desiredStateDigest` is in the engine object's own spec:
namespace, chart repository and manifest digest, values and engine policy
fields.

Labels select objects for watches. Annotations plus the recorded UID authorize
mutation; a label match alone never does. Argo CD Applications never carry the
`resources-finalizer.argocd.argoproj.io` finalizer except during an approved
`Remove` step.

## Request annotations on ClusterRuntime

| Annotation | Effect |
| --- | --- |
| `operator.aicr.run/retry: <token>` | After a `Failed`, `Expired` or `Revoked` rollout, or while one is `Stalled` (which ends it as `Superseded`), create a new rollout to the same target from current live state; it needs a fresh approval. A new token is needed for each retry. |
| `operator.aicr.run/resnapshot: <token>` | Run a new AICR snapshot outside the periodic schedule. |

## Retention

The operator deletes its own objects by label; only AICR Jobs' run-scoped
objects rely on garbage collection:

- Plans: keep active, candidate and pinned plan ConfigMaps plus the five most
  recent superseded ones, together with their recipe and values ConfigMaps.
- Rollouts: keep, per runtime UID, the newest rollout labeled `outcome: Succeeded`, any rollout
  without an outcome, and the ten most recent terminal ones.
- AICR Jobs: delete a Job with `propagationPolicy: Foreground` after its outputs
  are recorded; the `batch/v1` default orphans the Job's pods. Every Job also
  sets `ttlSecondsAfterFinished` (24 hours, raised to seven days on a stalled
  stage's failed Jobs) as a backstop while the operator is
  down or uninstalled. A Job's run-scoped objects are owned by the Job and go
  with it, so a result the operator keeps (snapshot, recipe, assessment report)
  is first copied into a ConfigMap without an owner. Keep those copies while a
  retained plan or rollout references them, plus the newest successful
  snapshot.

[aicr-adr-011]: https://github.com/NVIDIA/aicr/blob/main/docs/design/011-artifact-apiversion-policy.md

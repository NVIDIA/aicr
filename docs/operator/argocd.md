# Argo CD

Argo CD installs the GPU stack and repairs drift; the operator decides what it
installs, in what order and when, and judges readiness at the current
revision. This page
lists what the operator needs from Argo CD and what to leave alone.

## Version

The operator is tested against the Argo CD release AICR's own CI pins: Argo CD
v3.4.2, installed by the Argo Helm chart 9.5.14. It needs native OCI Helm
sources pinned by digest. The operator does not check the Argo CD version at
run time; other releases are untested.

Install Argo CD in the namespace the chart's `engine.namespace` names
(default `argocd`) before installing the operator. If its CRDs are missing, the
runtime reports `EngineAvailable=False` with `EngineMissing`, and the operator
starts watching Applications once they appear.

## What the operator needs in Argo CD

| Resource | Created by | Purpose |
| --- | --- | --- |
| AppProject `aicr` (`engine.project`) | The chart | Sources limited to `<registry>/charts/*` and `<registry>/wrappers/*`; destinations limited to the component namespaces on the in-cluster server; all cluster-scoped kinds allowed. Kept on uninstall |
| `repo-creds` Secret `aicr-registry` (`registry.auth.repoCredsSecret`) | The chart with `aws`; you with `static` | Registry credentials for every chart path under `registry.repository`. With `aws` the operator writes a fresh ECR token into it at start and every 6 hours. Kept on uninstall |

With `static`, create the Secret yourself in the Argo CD namespace:

```shell
REGISTRY_URL="oci://registry.example.com/aicr"   # registry.repository
REGISTRY_USER="<user>"
REGISTRY_PASSWORD="<password>"
kubectl -n argocd create secret generic aicr-registry \
  --from-literal=type=oci \
  --from-literal=url="${REGISTRY_URL}" \
  --from-literal=username="${REGISTRY_USER}" --from-literal=password="${REGISTRY_PASSWORD}"
kubectl -n argocd label secret aicr-registry argocd.argoproj.io/secret-type=repo-creds
```

### Comparison exceptions, and `argocd-cm`

The operator never reads or writes `argocd-cm`: its keys are shared by every
Argo CD user, and a key for a kind holds one rule for all applications. The
exceptions AICR's releases need are on each Application the operator writes
instead, in `spec.ignoreDifferences` with the sync option
`RespectIgnoreDifferences=true`, so neither comparison nor sync touches them:

```yaml
ignoreDifferences:
  - {kind: Secret, name: kubeflow-trainer-webhook-cert, jsonPointers: [/data]}
  - {kind: Secret, name: jobset-webhook-server-cert, jsonPointers: [/data]}
  - {group: storage.k8s.io, kind: CSIDriver, jsonPointers: [/spec/preventPodSchedulingIfMissing]}
```

The Secrets are the webhook certificates the Kubeflow Trainer and JobSet
controllers fill at run time; without the exception Argo CD resets them and
the trainer's webhooks fail TLS. Kubernetes reports the CSIDriver field before
Argo CD v3.4.2's schema declares it. No Lua health check is needed: AICR's
readiness gates are Jobs, which Argo CD assesses natively. Your own
`argocd-cm` rules are left alone.

Upgrading from an operator that wrote these as `argocd-cm` keys
(`resource.customizations.ignoreDifferences.Secret` and
`...storage.k8s.io_CSIDriver`, field manager `aicr-operator-customizations`)
rolls each runtime out once to the same charts and values, without approval,
as its engine policy moves to `argocd.v2`. The old keys stay; once that
rollout succeeds, remove them, or restore the rules they replaced. The chart
value `engine.manageCustomizations` is ignored.

## What the operator writes

One Application per plan release, named after the release, in the Argo CD
namespace:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: gpu-operator
  namespace: argocd
  labels: {app.kubernetes.io/managed-by: aicr-operator, operator.aicr.run/runtime: cluster}
  annotations:                       # ownership and the plan graph; abridged
    operator.aicr.run/runtime-uid: <ClusterRuntime UID>
    operator.aicr.run/plan-digest: sha256:<plan>
    operator.aicr.run/release-id: gpu-operator
    operator.aicr.run/depends-on: cert-manager,kube-prometheus-stack,nfd,nodewright-customizations
spec:
  project: aicr
  source:
    repoURL: oci://<registry>/charts/gpu-operator
    path: .
    targetRevision: sha256:<chart manifest digest>
    helm:
      valuesObject: {}               # the bundle's values.yaml merged with cluster-values.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: gpu-operator
  syncPolicy:
    automated: {enabled: true, prune: true, selfHeal: true}
    retry: {limit: 3, backoff: {duration: 30s, factor: 2, maxDuration: 5m}}
    syncOptions: [ServerSideApply=true]
```

- Charts come only from your registry, pinned by digest; Argo CD never
  contacts upstream chart repositories.
- No `CreateNamespace`: the chart pre-creates the component namespaces.
- No resources finalizer, so deleting an Application never deletes the stack.
  It is added, with the `operator.aicr.run/delete-rollout` annotation, only
  during an approved `Remove` step.
- A readiness release (an AICR readiness gate) is synced explicitly in each of
  its steps, with a sync operation initiated by `aicr-operator`, because
  Argo CD starts no automated sync for a change that touches only hooks.
- When a write leaves the spec unchanged, the operator sets
  `argocd.argoproj.io/refresh: normal` so Argo CD compares at once; Argo CD
  removes the annotation.

### Readiness at the current revision

A release counts as ready only when its Application is `Synced` at the plan's
chart digest, `comparedTo` matches the current spec including values, health
is `Healthy`, a sync or reconcile happened after the step began, and no
`ComparisonError`, `SyncError` or `InvalidSpecError` condition is present. An
Application that still reports `Synced` and `Healthy` from before the step
never counts. Argo CD has no `observedGeneration`, so this is how the operator
avoids reading stale health.

## What to leave alone

| Don't | Because | Instead |
| --- | --- | --- |
| Edit an owned Application | The operator forces its fields back and records `FieldOwnershipOverridden`: when no rollout is in progress, or in the release's own step. Edit a release whose step is in flight and the step stays `SpecDrifted` until its action timeout fails the rollout | Change the runtime: overrides, placement, or the AICR version |
| Disable automated sync on an owned Application | The operator states `enabled: true` and repairs it | `spec.suspendEngine: true` on the runtime ([operations](operations.md#pausing-versus-suspending-the-engine)) |
| Add the resources finalizer | The admission policy then denies every operator write to that Application, and deleting it would delete the release's resources | Remove a component through the plan; the operator adds the finalizer only for an approved `Remove` step |
| Delete an owned Application to remove a component | Its resources stay, and the operator writes it again | Remove the component from the recipe or plan |
| Manage these Applications from another tool (an app-of-apps, an ApplicationSet, Flux) | Two writers fight over the same objects | Leave them to the operator |
| Create an Application with a release's name yourself | It is foreign: the operator never writes or deletes it, and the rollout that reaches that release fails (`ForeignObject`) | Use another name |
| Delete the `aicr` AppProject | Every owned Application references it | `helm upgrade` recreates it |
| Add fields to a component's resources by hand (an env var on a Deployment, a label) | With server-side apply, Argo CD compares and self-heals only the fields its manifests set, so an added field stays, `Synced` and `Healthy`, and nothing reverts it. Fields the manifests set are reverted. Found on the GKE lane, 2026-10-28 | Change the component through the runtime's overrides; to undo an addition, remove the field by hand |

Argo CD rolls nothing back on its own, and neither does the operator: a failed
release stays where it failed until a new rollout moves it.

## Removal

A `Remove` step, in a rollout or a runtime deletion with `deletionPolicy:
Delete`, annotates the Application with `operator.aicr.run/delete-rollout`,
adds the resources finalizer, and deletes it with a UID precondition, so an
Application recreated under the same name is never deleted. Argo CD then
deletes the release's resources. It never deletes CRDs or resources annotated
`helm.sh/resource-policy: keep`, so a removed component's custom resources stay
until you delete them.

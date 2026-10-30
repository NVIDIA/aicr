# Operator lifecycle

Installing, upgrading, downgrading and uninstalling the operator itself. The
GPU stack it manages is never removed by any of these: it is owned by Argo CD
Applications that survive the operator.

## Install

The [overview](index.md#installation) installs the published chart, and
[installing on EKS](eks-installation.md) covers ECR. Each chart version
defaults to its operator image and to the AICR and validator images the
operator was qualified against, pinned by digest. The operator namespace is the Helm release namespace;
create it first with the `restricted` Pod Security label, because Helm cannot
template its own release namespace.

## Upgrade

An operator upgrade is a chart upgrade, and it moves the image, CRDs, RBAC and
admission policies together:

```shell
VERSION="<new version>"
helm upgrade aicr-operator oci://ghcr.io/nvidia/charts/aicr-operator \
  --version "${VERSION}" --namespace aicr-system -f values.yaml
```

- The CRDs are chart templates, not `crds/` files, so `helm upgrade` updates
  them. Their schema reaches existing objects at once.
- With `aicr.version` v1.0.x, add `--force-conflicts`: AICR v1.0.x relabels
  the discovery and validation namespaces, and Helm 4 then reports a
  server-side apply conflict on their `app.kubernetes.io/managed-by` label
  ([troubleshooting](troubleshooting.md)).
- A rollout in progress resumes from its durable state under the new operator
  with the same approval, and its in-flight step is not written twice.
- The upgrade itself creates no rollout unless the new operator renders
  different output. An operator version is a resolution input, so the
  resolver runs again; an identical render is the same plan.
- The new operator reads plans and rollouts written by the previous version.
  Existing plan ConfigMaps are immutable and never rewritten; new plans use
  the new schema.
- `v1alpha1` carries no conversion guarantee. A breaking API change ships with
  a manual migration in its release notes, never a conversion webhook.

Upgrading AICR is a values change inside the same chart
([operations](operations.md#upgrading-aicr-within-v1)).

## Downgrade

Downgrading is supported only while no plan or rollout carries a schema
version newer than the older operator knows. Otherwise the older operator
reports `Stalled` with `PlanUnreadable` and writes nothing to Argo CD; for
rollouts, the message names them. Recovery is to upgrade again; the newer objects are never
rewritten down.

`helm rollback` is a downgrade: it also returns the CRDs to the older
release's schema.

## Uninstall

The supported order:

1. **Delete the `ClusterRuntime`** and wait until it is gone. With
   `deletionPolicy: Orphan` (default), the Applications are annotated with
   `operator.aicr.run/orphaned-from` and keep reconciling. With `Delete`, an
   approved removal rollout deletes every release first
   ([stuck finalizers](troubleshooting.md#clusterruntime)).
2. **Uninstall the chart:** `helm uninstall aicr-operator --namespace aicr-system`.
   To reinstall later after AICR v1.0.x ran, first give the kept discovery and
   validation namespaces their `managed-by=Helm` label back
   ([troubleshooting](troubleshooting.md)).
3. **Optionally, remove what Helm keeps,** once nothing will adopt the stack
   again: the operator, Job, discovery and validation namespaces (the
   operator namespace holds the in-cluster registry and its volume), the
   Argo CD `repo-creds` Secret, the policies `aicr-operator-state` and
   `aicr-operator-approval` with their bindings, and the CRDs. Delete the CRDs
   last; deleting a CRD deletes every object of its kind.

### What `helm uninstall` keeps

| Resource | On uninstall | Why |
| --- | --- | --- |
| CRDs `ClusterRuntime`, `RuntimeRollout`, and their objects | Kept | Deleting a CRD deletes every runtime and rollout |
| Job, discovery, validation and component namespaces | Kept | The GPU stack runs in the component namespaces |
| Operator namespace and its ConfigMaps (plans, recipes, values, snapshots, assessments) | Kept; Helm never deletes its release namespace | Provenance for a reinstall |
| `aicr` AppProject | Kept | Every owned Application references it |
| Argo CD `repo-creds` Secret (`aws` provider and the in-cluster registry) | Kept | Argo CD keeps fetching charts: with `aws` until its token expires |
| The in-cluster registry: its Deployment, Service, ConfigMap, NetworkPolicy, login Secret and volume | Kept | It keeps serving the stack's charts to Argo CD, and a reinstall adopts it with the same content and login |
| Policies `aicr-operator-state` and `aicr-operator-approval` and their bindings | Kept | Plans and rollouts stay protected while no operator runs |
| `argocd-cm` keys an operator before engine policy `argocd.v2` applied | Kept | The current operator never touches `argocd-cm` ([Argo CD](argocd.md#comparison-exceptions-and-argocd-cm)) |
| Operator Deployment, ServiceAccounts, roles and bindings; the in-cluster registry's login copies in the operator, Job and validation namespaces | Deleted | |
| Policies `aicr-operator-applications`, `aicr-operator-application-deletes`, `aicr-operator-jobs`, `aicr-operator-rollout-outcome` and `aicr-operator-runtime` | Deleted | They constrain the operator, which is gone |

After uninstall, Argo CD keeps reconciling the last synced state. The
in-cluster registry keeps serving it until you delete the operator namespace.
With the `aws` provider, nothing refreshes the token in `repo-creds`, so within
12 hours Argo CD can no longer fetch charts: comparisons fail with
`ComparisonError`, and running workloads are unaffected. To keep the stack
unmanaged for longer, replace the Secret's credentials with your own.

A reinstall with the same release name adopts the kept resources when they
still match the chart.

### Uninstalling with the runtime still present

If you uninstall first, the `ClusterRuntime` and its finalizer stay, and
nothing changes in the cluster. Reinstalling the operator resumes managing it
without a new rollout; it re-verifies every plan ConfigMap it reads, so nothing
written while it was absent is trusted unchecked. A runtime delete issued while
no operator runs waits for the operator to return. Removing the finalizer by
hand instead cascades to nothing, and a later runtime
[adopts](troubleshooting.md#orphaned-objects-and-re-adoption) the Applications
by the old UID.

## Data retention

| Data | Where | Kept |
| --- | --- | --- |
| Plans | Immutable ConfigMaps `aicr-plan-<first 16 hex of the digest>` in the operator namespace | The active, candidate and pinned plans, plans referenced by a kept rollout, and the five most recent others |
| Recipes, values, assessment reports | Immutable ConfigMaps in the operator namespace | While a kept plan or rollout references them; any created in the last hour |
| Snapshots | ConfigMaps `aicr-snapshot-*` in the operator namespace | Discovery's newest one, and each kept rollout's deployment snapshot, owned by the rollout and removed with it |
| Rollouts | Cluster-scoped `RuntimeRollout` objects | Every unfinished rollout, the newest `Succeeded` one, and the ten most recent that ended |
| AICR Jobs | The Job, discovery and validation namespaces | Deleted once read; failed Jobs until the inputs change, and at most 24 hours after finishing (seven days for a stalled stage's failed Jobs), even with no operator running |
| Bundles, mirrored charts, evidence | The runtime registry: `bundles/`, `charts/`, `wrappers/`, `evidence/` | Never deleted by the operator. The in-cluster registry deletes nothing either, and its volume is kept on uninstall; an external registry's retention applies |

The operator-namespace ConfigMaps are caches. Losing them, even by deleting
the namespace, loses provenance (the recipe and the AICR version a plan came
from) but not the stack: the active plan can be rebuilt from the Applications'
annotations, and adoption needs nothing else. Evidence stays in the registry
and is referenced from the succeeded rollout's `operator.aicr.run/evidence`
annotation.

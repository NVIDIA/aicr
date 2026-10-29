# Privilege boundaries

The operator writes Argo CD Applications that Argo CD applies with its own
broad identity, and it pushes the charts they install. Treat it as a tier-0
component, like Argo CD itself: installed and upgraded by platform
administrators, with its images verified and its actions audited. Its RBAC and
admission policies keep each identity's privileges explicit and stop
accidental or casual misuse; they do not contain a fully compromised operator.

## Identities

| Identity | Namespace | May | May not |
| --- | --- | --- | --- |
| Operator (`aicr-operator`) | Operator | Read and patch `ClusterRuntime` (only its cleanup finalizer, by policy) and write its status; create, update and delete `RuntimeRollout`; read Nodes; manage labeled ConfigMaps, Leases and Events in its namespace and read its own pod; create, watch and delete Jobs and read their pods and ConfigMaps in the Job, discovery and validation namespaces; create per-run registry Secrets in the Job and validation namespaces; write Applications in the Argo CD namespace; read the `aicr` AppProject; get and update the `repo-creds` Secret, by name (`aws`); read its docker config Secret, by name (`static`) | Write a `ClusterRuntime` spec, or any rollout approval; read or write `argocd-cm`; list or watch Secrets, or read any other Secret; create Namespaces; `escalate`, `bind`, `impersonate` or `pods/exec`; create Pods directly |
| AICR Jobs (`aicr-jobs`) | Job | Read and write ConfigMaps in their namespace; push to the registry with the per-run Secret mounted into their pod | Anything else |
| Snapshot launcher (`aicr-snapshot`) | Discovery | Run AICR's snapshot agent: Jobs, Pods, logs and ConfigMaps in its namespace; read ServiceAccounts. With AICR v1.0.x, also cluster-wide `create` on Namespaces, which AICR requires even though the namespace exists | |
| Snapshot agent (`aicr-snapshot-agent`) | Discovery (`privileged`) | Run privileged with host PID, network and IPC and read-only host paths, as AICR's agent requires; read Nodes, Pods, DaemonSets and GPU Operator `ClusterPolicy` cluster-wide | Write anything but ConfigMaps in its namespace |
| Validation launcher (`aicr-validation`) | Validation (`privileged`) | AICR v1.0.x: `cluster-admin`, because AICR binds its validators per run. AICR v1.1.0 and later: Jobs, Pods, logs and ConfigMaps in its namespace, and SubjectAccessReviews | Installed only with `validation.enabled` |
| Validators (`aicr-validator`) | Validation | AICR v1.1.0 and later: `cluster-admin`, which AICR's validators require | Installed only with `validation.enabled` |
| Readiness gates | Component namespaces | Read-only access shipped in AICR's readiness charts | |
| Argo CD | Argo CD | Its own identity, from its installation | |
| Approvers (`aicr-operator-approver`) | — | Read rollouts and the runtime; set a rollout's approval or revocation; read ConfigMaps in the operator namespace (assessment reports) | Edit the `ClusterRuntime`; read Secrets |

The chart creates no role for editing the `ClusterRuntime`. Grant it to the
runtime administrators yourself, and keep them apart from approvers, so nobody
both changes the runtime and approves the result.

Deployment validation is cluster-admin-equivalent with every AICR version:
enable it (`validation.enabled`) only where that is acceptable.

## Admission policies

All are ValidatingAdmissionPolicies; the operator uses no webhooks and no
mutating admission. Their parameters (registry prefix, namespace allowlist,
AICR image) are rendered into the policies themselves, which only
administrators can edit.

**Constraints on the operator.** These bind only the operator's ServiceAccount
and are deleted with it:

| Policy | Holds the operator to |
| --- | --- |
| `aicr-operator-applications` | Applications in the `aicr` project, in the Argo CD namespace, with one source under the registry's `charts/` or `wrappers/` pinned by `sha256:` digest; no plugin, value files or Helm parameters; the in-cluster server and a namespace from `namespaces.components`; Argo CD's resources finalizer only together with the `operator.aicr.run/delete-rollout` annotation |
| `aicr-operator-application-deletes` | Deletes only of its own Applications, already marked by an approved `Remove` step (the resources finalizer and the `operator.aicr.run/delete-rollout` annotation) |
| `aicr-operator-jobs` | Jobs only in the Job, discovery and validation namespaces, each with one container running the pinned AICR image, that namespace's ServiceAccount, an allowed AICR subcommand (`recipe`, `bundle` or `upgrade-check`; `snapshot`; `validate`) and a TTL; a snapshot agent `--image` naming the pinned AICR image too |
| `aicr-operator-rollout-outcome` | A rollout's `operator.aicr.run/outcome` label and `operator.aicr.run/evidence` annotation never change once set |
| `aicr-operator-runtime` | A `ClusterRuntime` changes only through the operator's `operator.aicr.run/cleanup` finalizer; the operator never creates one |

**Protection of operator state.** These bind everyone and are kept on
uninstall:

| Policy | Holds |
| --- | --- |
| `aicr-operator-state` | Only the operator creates or updates a ConfigMap labeled `app.kubernetes.io/managed-by: aicr-operator` in the operator namespace, including removing the label. Deletion is left to RBAC, so deleting the namespace still works; a deleted plan is rebuilt or resolved again |
| `aicr-operator-approval` | Only the operator creates rollouts, never with an approval, and it never writes one. Anyone else changes only `spec.approval`, holds `update` on `runtimerollouts/approve` for that rollout, records their own username, and cannot revoke a rollout that has ended or is validating |

The CRDs add their own rules: a rollout's spec is immutable except its
approval, and an approval or revocation is set once and never cleared.

The `aicr` AppProject is enforced by Argo CD independently: sources limited to
the registry's `charts/` and `wrappers/`, destinations limited to the component
namespaces on the in-cluster server.

## Namespaces and Pod Security

| Namespace | Pod Security | Created by |
| --- | --- | --- |
| Operator (`aicr-system`) | `restricted` | You, before installing |
| Job (`aicr-jobs`) | `restricted` | The chart |
| Discovery (`aicr-discovery`) | `privileged`, for AICR's snapshot agent | The chart |
| Validation (`aicr-validation`) | `privileged`, for AICR's validators, until what they need is measured | The chart, with `validation.enabled` |
| Component namespaces | As each `namespaces.components` entry declares | The chart, unless the entry sets `create: false` |

The chart keeps every namespace it creates on uninstall, so removing the
chart never removes the GPU stack. The operator creates no namespaces, and
Argo CD's `CreateNamespace` stays off: a plan that renders a release into any
other namespace is refused before it reaches Argo CD (`NamespaceNotAllowed`).

The operator and AICR's recipe, bundle and upgrade-check Jobs run as non-root
with a read-only root file system, all capabilities dropped, `RuntimeDefault`
seccomp and size-limited scratch space.

## Credentials

- With the `aws` provider, the operator's AWS identity mints ECR tokens. It
  refreshes Argo CD's `repo-creds` Secret, and gives each AICR Job that pushes a
  per-run docker config Secret, owned by the Job and deleted with it, even when
  the operator is stopped. Scope the identity's repository prefix to one
  cluster: it can push there.
- With `static`, you provide every Secret, and the operator only reads its own
  copy by name.
- Credentials never appear in plans, values, status, Events, logs or evidence;
  errors name the Secret, never its content. Approvers can read assessment
  reports but no Secret.

## Network egress

The chart ships no NetworkPolicy. If you restrict egress, allow:

| Component | Destinations |
| --- | --- |
| Operator | The Kubernetes API; your registry; the upstream chart repositories the recipes pin; the validator images' registry; AWS STS and ECR endpoints (`aws`) |
| AICR render Jobs | The Kubernetes API; your registry |
| Argo CD repo-server | Your registry only |
| Snapshot agent | The Kubernetes API |
| Validation Jobs | The Kubernetes API; your registry, for evidence |

## Tenants

Tenants should have no access to the operator's kinds, the Argo CD
Applications, the operator, Job, discovery and validation namespaces, or GPU
Operator operands. Give them dedicated namespaces with the `restricted` Pod
Security profile and quotas. Namespace isolation is not GPU or fabric
isolation; what each profile isolates is part of its qualification
([support policy](support.md)).

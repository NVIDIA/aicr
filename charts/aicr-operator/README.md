# aicr-operator chart

Installs the AICR Operator
([ADR-027](../../docs/design/027-aicr-operator.md)), its identities, admission
policies, Argo CD project and namespaces. Values are described in
[values.yaml](values.yaml) and the
[API reference](../../docs/operator/api.md#installation-values); the schema
rejects unknown keys, and rendering fails for an AICR version outside
`>=1.0.0, <2.0.0`.

## Prerequisites

- Kubernetes 1.34 or later.
- Argo CD installed (the pinned release is in `.settings.yaml`), with
  `CreateNamespace` off.
- The runtime registry. With `registry.auth.provider: aws`, an IAM role for the
  operator through EKS Pod Identity or IRSA (`operator.serviceAccountAnnotations`)
  carrying [files/ecr-iam-policy.json](files/ecr-iam-policy.json) for the
  runtime prefix. With `static`, the docker config Secret
  `registry.auth.staticSecretRef` in the operator and Job namespaces (and the
  validation namespace with `validation.enabled`), and the Argo CD
  `repo-creds` Secret, all created by the administrator.
- The operator namespace, which is the release namespace, created with the
  restricted Pod Security profile. Helm cannot template its own release
  namespace, so the chart does not create it:

  ```shell
  kubectl create namespace aicr-system
  kubectl label namespace aicr-system pod-security.kubernetes.io/enforce=restricted
  ```

## Install and upgrade

```shell
helm upgrade --install aicr-operator oci://ghcr.io/nvidia/charts/aicr-operator \
  --version <version> --namespace aicr-system -f values.yaml
```

A published chart defaults to the operator, AICR and validator images its
release was built and tested with, pinned by digest. Installing from this
source tree instead needs `operator.image` and `aicr.image` set, and
`aicr.validatorImages` with `validation.enabled`.

An operator upgrade is a chart upgrade: image, CRDs, RBAC and policies
together. The CRDs are templates, not `crds/` files, so `helm upgrade` updates
them. AICR upgrades are a values change.

## Values

Every value, with its default and when to set it. **Required** values have no
usable default; a published chart already pins `aicr.image`,
`aicr.validatorImages` and `operator.image` to the images its release was
tested with, so those are required only when installing from source. The
[API reference](../../docs/operator/api.md#installation-values) shows them in
context; the schema rejects unknown keys.

| Key | Default | Set it | Meaning |
| --- | --- | --- | --- |
| `aicr.version` | `v1.0.0`; a published chart, the version it was tested with | To change AICR | The declared AICR release, `>=1.0.0, <2.0.0`. Changing it is an AICR upgrade, rolled out like any other change |
| `aicr.image` | A published chart's tested image | Required from source | AICR CLI and snapshot agent image, pinned by digest |
| `aicr.validatorImages` | A published chart's tested images | Required from source with `validation.enabled` | AICR's validator images for `aicr.version`, each `repository:tag@sha256:<digest>` |
| `aicr.jobs.nodeSelector` | `{}` | When every node is tainted | Node selector for every AICR Job the operator runs; empty places none |
| `aicr.jobs.tolerations` | `[]` | When every node is tainted | Tolerations for every AICR Job the operator runs, such as `CriticalAddonsOnly` on EKS system nodes |
| `operator.image` | A published chart's tested image | Required from source | Operator image; pin it by digest on a shared cluster |
| `operator.imagePullPolicy` | `IfNotPresent` | Optional | |
| `operator.replicas` | `1` | Optional | 1 to 3; extra replicas wait on the leader-election Lease |
| `operator.logLevel` | `info` | Optional | `debug`, `info`, `warn` or `error` |
| `operator.resources` | Requests `100m`, `128Mi`; limit `512Mi` | Optional | The operator container's requests and limits |
| `operator.serviceAccountAnnotations` | `{}` | With `registry.auth.provider: aws` | Annotations on the operator's ServiceAccount, such as the IRSA role ARN |
| `operator.nodeSelector` | `{}` | Optional | Node selector for the operator pod |
| `operator.tolerations` | `[]` | Optional | Tolerations for the operator pod |
| `operator.affinity` | `{}` | Optional | Affinity for the operator pod |
| `engine.type` | `ArgoCD` | No | The supported engine; Flux follows |
| `engine.namespace` | `argocd` | When Argo CD runs elsewhere | Argo CD's control namespace |
| `engine.project` | `aicr` | Optional | The AppProject the chart creates and every owned Application uses |
| `registry.repository` | `""` | **Required** | Runtime registry prefix, `oci://host/path`, for bundles, mirrored charts and evidence |
| `registry.plainHTTP` | `false` | Test registries only | Plain HTTP to the registry |
| `registry.auth.provider` | `aws` | Optional | `aws`: ECR through the operator's AWS identity, with a token the operator refreshes. `static`: Secrets you provide |
| `registry.auth.staticSecretRef` | `""` | With `static` and a private registry | Docker config Secret name in the operator, Job and validation namespaces |
| `registry.auth.repoCredsSecret` | `aicr-registry` | Optional | Argo CD `repo-creds` Secret for the runtime registry; the chart creates it with `aws` |
| `namespaces.discovery` | `aicr-discovery` | Optional | Snapshot namespace, Pod Security `privileged` |
| `namespaces.jobs` | `aicr-jobs` | Optional | Recipe, render and upgrade-check namespace, Pod Security `restricted` |
| `namespaces.validation` | `aicr-validation` | Optional | Validation namespace, created with `validation.enabled` |
| `namespaces.components` | `[]` | **Required** | Every namespace the recipe deploys into (`name`, `podSecurity`, `create`); a release outside them stalls with `NamespaceNotAllowed` |
| `discovery.agentRequests` | `""` | Optional | Snapshot agent requests, `name=quantity,...`; empty keeps AICR's GPU-node defaults |
| `validation.enabled` | `false` | Recommended | Installs deployment validation, which gates every rollout by default; without it, set `spec.validation.deployment: Disabled` |
| `validation.serviceAccountAnnotations` | `{}` | With `validation.signingKey` | Annotations on the validation launcher's ServiceAccount, such as the workload identity that signs evidence |
| `validation.signingKey` | `""` | Optional, AICR v1.1.0 or later | KMS key URI that signs evidence; empty pushes it unsigned |
| `validation.transparencyLog` | `true` | Optional | Upload key signatures to Rekor; `false` keeps them out of the public log |
| `approvers.users` | `[]` | **Required**, with `approvers.groups` | Users bound to the approver roles; without an approver no rollout that needs approval can proceed |
| `approvers.groups` | `[]` | **Required**, with `approvers.users` | Groups bound to the approver roles; keep them apart from `ClusterRuntime` editors |
| `development.bypassApprovals` | `false` | Development only | Proceed without approvals; release builds refuse to start with it |
| `development.skipDiscovery` | `false` | Development only | Resolve from declared criteria without a snapshot; release builds refuse it |

`engine.manageCustomizations` is still accepted and ignored: the operator no
longer writes `argocd-cm`
([Argo CD](../../docs/operator/argocd.md#comparison-exceptions-and-argocd-cm)).

## What it installs

| Resource | On `helm uninstall` |
| --- | --- |
| CRDs `ClusterRuntime`, `RuntimeRollout` | Kept; runtime and rollout objects survive |
| Job (`restricted`), discovery (`privileged`), validation (`privileged`, when enabled) and component namespaces | Kept; the GPU stack survives |
| `aicr` AppProject; with `aws`, the `repo-creds` Secret the operator refreshes | Kept; owned Applications stay valid |
| Policies `aicr-operator-state` and `aicr-operator-approval` and their bindings | Kept; plans and rollouts stay protected while no operator runs |
| Policies `aicr-operator-applications`, `aicr-operator-application-deletes`, `aicr-operator-jobs`, `aicr-operator-rollout-outcome`, `aicr-operator-runtime` | Deleted with the operator they constrain |
| Deployment, ServiceAccounts, roles and bindings | Deleted |

With `validation.enabled`, deployment validation also needs
`aicr.validatorImages`: AICR's validator images for `aicr.version`, each as
`repository:tag@sha256:<digest>` (the pins for the supported releases are in
`.settings.yaml`). AICR names the tags in its evidence; the operator fails a run
whose evidence names any other validator image, and checks that each tag still
resolves to its digest before binding the evidence.
With AICR v1.1.0 or later, `validation.signingKey` (a KMS key URI) signs the
evidence through the launcher's workload identity
(`validation.serviceAccountAnnotations`), and `validation.transparencyLog:
false` keeps the signatures out of the public Rekor log. The chart refuses a
signing key with AICR v1.0.x or without `validation.enabled`.

`namespaces.components` is also the allowlist the operator enforces itself: a
plan that renders a release into any other namespace stalls resolution with
`NamespaceNotAllowed` and never reaches Argo CD.

Admission policies, all ValidatingAdmissionPolicies without webhooks:

- `aicr-operator-applications` (the operator only): project `aicr`; one source
  under the runtime registry's `charts/` or `wrappers/` pinned by `sha256:`;
  no plugin, value files or parameters; the in-cluster server and a component
  namespace; the resources finalizer only with the `delete-rollout`
  annotation.
- `aicr-operator-application-deletes` (the operator only): deletes only of
  its own Applications already marked by an approved `Remove` step.
- `aicr-operator-jobs` (the operator only): one container running the pinned
  AICR image with its entrypoint; the namespace's AICR identity and allowed
  subcommand (`recipe`, `bundle`, `upgrade-check` in the Job namespace,
  `snapshot` in discovery, `validate` in validation); a TTL; a snapshot agent
  `--image` naming the pinned AICR image.
- `aicr-operator-rollout-outcome` (the operator only): the `outcome` label and
  `evidence` annotation never change once set.
- `aicr-operator-runtime` (the operator only): a `ClusterRuntime` changes only
  through the operator's `operator.aicr.run/cleanup` finalizer.
- `aicr-operator-state` (everyone but the operator): no create or update of an
  operator-labeled ConfigMap in the operator namespace, including removing the
  label. Deletion is left to RBAC, so deleting the namespace works.
- `aicr-operator-approval` (kept on uninstall): no rollout is created with an
  approval and the operator never writes one; anyone else changes only
  `spec.approval`, needs `update` on `runtimerollouts/approve`, records their
  own username, and cannot revoke a rollout that has ended or is validating.

Approvers get the `aicr-operator-approver` ClusterRole and Role (read
rollouts, the runtime and assessment reports in the operator namespace;
approve), bound to `approvers.users` and `approvers.groups`. Keep them apart
from `ClusterRuntime` editors.

## Pod Security

The operator and AICR's recipe, bundle and upgrade-check Jobs run under the
restricted profile: non-root, read-only root file system, all capabilities
dropped, `RuntimeDefault` seccomp and size-limited `emptyDir` scratch space.
Two namespaces are documented exceptions:

- discovery is `privileged`: AICR's snapshot agent runs privileged with host
  PID, network and IPC and read-only host paths;
- validation is `privileged`: AICR's validators are cluster-admin-equivalent;
  the level stays until the profile they need is measured.

## Network egress

| Component | Destinations |
| --- | --- |
| Operator | Kubernetes API; runtime registry; pinned upstream chart repositories; the validator images' registry; AWS STS and ECR endpoints |
| AICR render Jobs | Kubernetes API; runtime registry |
| Argo CD repo-server | Runtime registry only |
| Snapshot agent | Kubernetes API |
| Validation Jobs | Kubernetes API; runtime registry for evidence |

The operator makes no Sigstore or TUF calls.

## Uninstall

1. Delete the `ClusterRuntime` and wait for its finalizer.
2. `helm uninstall aicr-operator --namespace aicr-system`.
3. Optionally, once nothing will re-adopt the stack, delete the operator, Job,
   discovery and validation namespaces, the kept policies and bindings, and the
   CRDs by hand.

A reinstall with the same release name adopts the kept resources.

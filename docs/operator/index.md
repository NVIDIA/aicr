# AICR Operator

A Kubernetes operator that runs AICR in your cluster and rolls the GPU stack it
renders out through Argo CD: in dependency order, with approval for risky
changes and validation evidence for each rollout. The operator is opt-in; the
CLI, API server and bundles work as they always have without it
([ADR-027](https://github.com/NVIDIA/aicr/blob/main/docs/design/027-aicr-operator.md)).

**Status: preview.** The operator is in preview: its API
(`operator.aicr.run/v1alpha1`) may change without a conversion path, and no
hardware profile is qualified yet. The configuration it deploys is not a preview. The
operator renders nothing itself: every bundle it rolls out is the one the
pinned AICR release renders, with zero diff, from the same validated and
optimized recipes as the AICR CLI. The operator supports AICR `>=1.0.0, <2.0.0`,
Kubernetes 1.34 or later, Argo CD as the deployment engine, and one homogeneous
accelerator and OS profile per cluster. A GB200 profile, a Flux adapter and
greenfield bootstrap follow later ([support policy](support.md)).

## Why an operator

AICR decides what a GPU cluster should run: it resolves a validated recipe for
your hardware and renders it into a bundle. Without the operator you run AICR
from a workstation or CI and hand the bundle to Argo CD or Flux through Git.
That gets you a first install. Keeping the stack correct as the cluster and
AICR change also takes deciding when each change goes in, whether it is safe,
and whether it worked. Neither AICR nor GitOps decides that, so each team
scripts it.

| With the AICR CLI and Argo CD | With the operator |
| --- | --- |
| `aicr snapshot` captures one moment; new nodes or drivers go unseen until someone reruns it | Discovery reruns as the cluster changes and resolves the matching recipe |
| Sync waves advance on each Application's last health, which can still describe the previous revision | Releases roll out in dependency order; each advances only when ready at the revision just written, including AICR's readiness gates |
| `aicr upgrade-check` reports `manual` and `blocked` transitions; enforcing them is up to you | Risky transitions wait for an approver; `blocked` transitions never start |
| `aicr validate` runs on its own; its evidence names no rollout | Validation gates each rollout, and its evidence is bound to the deployed plan and its approval |
| Removing a component from Git deletes its Application and leaves the component running, unmanaged | Removing a component is an approved step that uninstalls it in reverse dependency order |

**Same configuration as the AICR CLI.** The operator adds coordination, not
configuration. It runs `aicr snapshot`, `aicr recipe` and `aicr bundle` from
the pinned AICR image with inputs from your `ClusterRuntime`, so each bundle it
deploys has zero diff with the one `aicr bundle --deployer helm` renders from
the same recipe and flags, and Argo CD installs each release with the values
that bundle's `deploy.sh` would pass to Helm. CI checks this on every change,
and you can [check it on your cluster](#verify-the-bundle-yourself).

## Validated scenarios

Each scenario below ran on two H100 nodes (`a3-megagpu-8g`, GPUDirect-TCPXO) on
GKE with a training job running throughout.

| Scenario | What you see |
| --- | --- |
| **Install on bare GPU nodes** | One approval installs 18 components in dependency order, each only after the one before is healthy at the revision just written. Deployment validation passes, and its evidence records the plan and the approver. 15 to 20 minutes |
| **Upgrade AICR by changing one value** | A new plan resolves, `aicr upgrade-check` rates each component change (`safe`, `manual`), and one approval rolls it out and validates it |
| **Roll back** | Pinning an earlier plan is assessed and approved like any other change; nothing rolls back on its own |
| **Stop a rollout mid-way** | Revoking the approval lets the step in flight finish and starts nothing else; a retry needs a fresh approval |
| **Upgrade or crash the operator mid-rollout** | The same rollout resumes where it stopped, and no write is repeated |
| **Lose the operator, keep the stack** | Deleting the operator, its namespace and every saved plan leaves the GPU stack and the training job running. A reinstall rebuilds the plan from what is running, confirms nothing was edited by hand, and takes the stack back with one approval in under a minute |
| **Freeze everything during an incident** | `suspendEngine` stops every sync and drift repair within seconds and deletes nothing; clearing it resumes syncs and repairs the drift |
| **Nobody approves their own change** | Approving without the approver role, approving your own runtime edit, approving in someone else's name, the operator approving itself, and tenants reaching admin resources are all refused |
| **Status you can act on** | While a rollout runs, `Ready` names the step, such as `step 9/18 gpu-operator (Upgrade)`. A node reset or a registry outage shows up in the conditions with its cause and clears on recovery |

## Responsibilities

| Concern | Owner |
| --- | --- |
| Cluster snapshot and recipe selection | AICR (`aicr snapshot`, `aicr recipe`) |
| Chart versions, values, manifests and readiness gates | AICR (`aicr bundle`) |
| Transition risk between the active and the new plan's recipes | AICR (`aicr upgrade-check`) |
| Deployment validation and its evidence | AICR (`aicr validate`) |
| Installing charts and repairing drift | Argo CD |
| Device lifecycles | Component operators, such as GPU Operator |
| When to resolve and roll out, in what order, approvals, readiness at the current revision, removal | The operator |
| Serving every chart from your registry, pinned by digest | The operator |

The operator does not replace GPU Operator, allocate tenant GPUs, infer
training performance from healthy pods, roll back automatically, or run its own
node drains.

## Installation

### Prerequisites

- Kubernetes 1.34 or later, with Argo CD installed at the pinned release.
- An OCI registry for charts, bundles and evidence. On EKS, an ECR repository
  prefix plus an IAM role for the operator through EKS Pod Identity or IRSA;
  the operator refreshes Argo CD's ECR credentials.
- The namespaces your recipe deploys into, listed in the chart values; the
  chart creates them with their Pod Security labels and keeps them on
  uninstall.

The [chart README](https://github.com/NVIDIA/aicr/blob/main/charts/aicr-operator/README.md)
details each prerequisite, what the chart installs, and its admission policies;
[installing on EKS](eks-installation.md) walks through ECR, the operator's AWS
identity and the component namespaces.

### Install the chart

Each AICR release publishes the operator image and its chart, tagged with the
release version. The chart defaults to that release's operator image and to the
AICR and validator images the operator was qualified against, all pinned by
digest. That AICR release can be earlier than the one the chart ships in:
`helm show values` shows it, and [operations](operations.md#upgrading-aicr-within-v1)
moves it.

```shell
VERSION="<release>"   # an AICR release that ships the operator, without the leading v

gh attestation verify "oci://ghcr.io/nvidia/charts/aicr-operator:${VERSION}" \
  --repo NVIDIA/aicr \
  --signer-workflow NVIDIA/aicr/.github/workflows/on-tag.yaml --source-ref "refs/tags/v${VERSION}"

kubectl create namespace aicr-system
kubectl label namespace aicr-system pod-security.kubernetes.io/enforce=restricted
```

Create `values.yaml` with your cluster's inputs:

```yaml
registry:
  repository: oci://<account>.dkr.ecr.<region>.amazonaws.com/aicr
operator:
  serviceAccountAnnotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::<account>:role/aicr-operator
namespaces:
  components:          # every namespace the recipe deploys into
    - name: gpu-operator
      podSecurity: privileged
approvers:
  groups: [gpu-platform-approvers]
validation:
  enabled: true        # AICR deployment validation, which gates every rollout by default
```

Deployment validation runs AICR's validators with `cluster-admin`. If you
leave `validation.enabled` off, set `spec.validation.deployment: Disabled` on
the `ClusterRuntime` below; otherwise every rollout waits in `Validating` with
`ValidationUnavailable`.

Then install:

```shell
helm install aicr-operator oci://ghcr.io/nvidia/charts/aicr-operator \
  --version "${VERSION}" --namespace aicr-system -f values.yaml
```

Required: `registry.repository`, `namespaces.components` and at least one of
`approvers.users` or `approvers.groups`; with the `aws` registry provider, the
operator's AWS identity in `operator.serviceAccountAnnotations`. Everything
else has a working default; `validation.enabled` is recommended. Every value,
with its default and when to set it, is in the
[chart's values reference](https://github.com/NVIDIA/aicr/blob/main/charts/aicr-operator/README.md#values).

### Install the kubectl plugin

Approvers use the `kubectl aicr` plugin. Each AICR
[GitHub release](https://github.com/NVIDIA/aicr/releases) carries its binaries
for Linux and macOS (`kubectl-aicr-<os>-<arch>`):

```shell
gh release download "v${VERSION}" --repo NVIDIA/aicr --pattern kubectl-aicr-darwin-arm64
gh attestation verify kubectl-aicr-darwin-arm64 --repo NVIDIA/aicr \
  --signer-workflow NVIDIA/aicr/.github/workflows/on-tag.yaml --source-ref "refs/tags/v${VERSION}"
install -m 0755 kubectl-aicr-darwin-arm64 /usr/local/bin/kubectl-aicr
```

## Usage

### Describe the cluster

The operator acts on one `ClusterRuntime` named `cluster`:

```yaml
apiVersion: operator.aicr.run/v1alpha1
kind: ClusterRuntime
metadata:
  name: cluster
spec:
  intent: training
  platform: kubeflow
  # Declare what the cluster is when its GPU nodes have no driver yet; the
  # snapshot taken after the GPU stack is installed must confirm it.
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
  placement:
    system:
      nodeSelector: {example.com/pool: system}
    accelerated:
      nodeSelector: {example.com/pool: gpu}
      tolerations:
        - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
```

### Approve rollouts

The operator discovers the node groups, resolves the matching AICR recipe and
plans the rollout. Risky transitions wait for an approver:

```shell
kubectl get clusterruntime cluster
kubectl get runtimerollouts
ROLLOUT="<rollout>"   # the name from the list
kubectl aicr describe "${ROLLOUT}"
kubectl aicr approve "${ROLLOUT}" --expires 2h --reason "Maintenance window"
```

A `manual` transition whose record has steps after the apply pauses once that
release is applied; do those steps, then run `kubectl aicr acknowledge` on the
rollout.

[Day-2 operations](operations.md) covers reading status, approvals, pausing,
rollback and AICR upgrades, and [troubleshooting](troubleshooting.md) every
stalled state. The [API reference](api.md) describes every field.

### Verify the bundle yourself

`kubectl aicr reproduce` writes a plan's recipe (by default the active plan's)
to `aicr-reproduce/recipe.json` and prints the commands that render the same
bundle with the AICR CLI and compare it with the operator's. With the
placeholders filled in from its output:

```shell
kubectl aicr reproduce

# From its output: the AICR image the operator ran, the plan's flags, and the bundle.
AICR_IMAGE="<aicr image>"
PLAN_FLAGS=("<flag>" "<value>")
BUNDLE="<registry>/bundles/cluster@sha256:<digest>"

# Render with that AICR image, from the plan's recipe and flags.
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/aicr-reproduce:/work" -w /work \
  "${AICR_IMAGE}" bundle --recipe recipe.json "${PLAN_FLAGS[@]}" --output cli-bundle

# Pull the bundle the operator rendered (log in to your registry first).
oras pull "${BUNDLE}" -o aicr-reproduce/operator-bundle

diff -r aicr-reproduce/operator-bundle aicr-reproduce/cli-bundle && echo "zero diff"
```

The plugin only reads the plan's record; the AICR image renders and `diff`
compares, so a record that did not match what the operator rendered would show
up as a diff. Each Argo CD Application's `spec.source.helm.valuesObject` is
that release's `values.yaml` merged with the bundle's `cluster-values.yaml`.
The plugin needs `get` on the `ClusterRuntime` and on ConfigMaps in the
operator namespace.

## Upgrade and uninstall

An operator upgrade is a chart upgrade (`helm upgrade` with the new
`--version`). `helm uninstall` leaves CRDs, namespaces and the Argo CD project
in place, so it never removes the GPU stack. Delete the `ClusterRuntime` first;
[operator lifecycle](lifecycle.md) covers upgrade, downgrade, uninstall order
and what each keeps.

## Pages

| Page | Covers |
| --- | --- |
| [Installing on EKS](eks-installation.md) | ECR, the operator's AWS identity, the operator image, component namespaces and the first `ClusterRuntime` |
| [Day-2 operations](operations.md) | Reading status; approving, revoking and retrying; pausing versus suspending the engine; rolling back by pinning; changing inputs and the AICR version |
| [Troubleshooting](troubleshooting.md) | Runbooks by symptom: blocked plans, approvals, registry and credential outages, version mismatches, stuck finalizers, orphaned objects and re-adoption |
| [Operator lifecycle](lifecycle.md) | Upgrading, downgrading and uninstalling the operator, what each keeps, and data retention |
| [Privilege boundaries](security.md) | Every identity, what it may do, and the admission policies that hold it there |
| [Argo CD](argocd.md) | The Argo CD release and project the operator needs, its per-Application comparison exceptions, and what not to change by hand |
| [kubectl aicr](cli-reference.md) | The plugin's commands, flags, output and permissions |
| [Support policy](support.md) | Supported AICR, Kubernetes and Argo CD versions, qualified profiles, and when qualification goes stale |
| [API reference](api.md) | Every field of `ClusterRuntime` and `RuntimeRollout`, the plan ConfigMap, and the installation values |

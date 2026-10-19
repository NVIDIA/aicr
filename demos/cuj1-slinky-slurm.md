# AICR - Critical User Journey (CUJ) 1 — Slinky Slurm

End-to-end walkthrough: **generate recipe → bundle → deploy → validate → `srun` smoke job**.

Use either direct criteria flags or a cluster snapshot plus explicit
`--intent training --platform slurm`. Tier-1 snapshots detect declared Slinky
Controller presence but do not infer the platform or reconstruct installed
Helm values. See [aicr recipe](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md#aicr-recipe) in the
CLI reference.

## Assumptions

- `kubectl` is configured for the target cluster.
- Cloud leaves assume GPU nodes with drivers. The Kind leaf runs on simulated GPUs instead; see [Kind (simulated GPUs)](#kind-simulated-gpus).
- On EKS, an AWS credential path for the EBS CSI driver. The bundle installs the driver but not its credentials, and without them no volume can be provisioned — see [EBS CSI Driver Credentials](../docs/user/component-catalog.md#ebs-csi-driver-credentials).
- Node pools use a `nodeGroup` label (adjust if your cluster uses different keys).
- Inspect taints before bundling: `kubectl get nodes -o custom-columns=NAME:.metadata.name,GROUP:.metadata.labels.nodeGroup,TAINTS:.spec.taints`

## Workflow

```text
  aicr recipe          aicr bundle        ./deploy.sh       aicr validate       srun smoke
  (query/snapshot) ──▶ (scheduling)  ──▶  (install)    ──▶  (phases)       ──▶  (manual)
```

1. **Generate recipe** — direct criteria or snapshot-derived infrastructure criteria plus `--platform slurm` resolve a Slurm leaf overlay to `recipe.yaml`.
2. **Generate bundle** — apply `--system-*` / `--accelerated-*` scheduling and optional `--set` / `--set-json` / `--set-file` on `slinkyslurm`.
3. **Install** — run `deploy.sh`; cert-manager and Slinky operator come up, then the cluster chart in `slurm`.
4. **Validate** — run `deployment` (Chainsaw component health) and `conformance`: the AI conformance checks the leaf inherits, plus `slinky-slurm-health` from the login pod (including a conditional `sacct` probe when accounting is enabled) and, on cloud leaves, `slinky-slurm-gpu-access` for Slurm GPU access and isolation. **Performance validation is not supported yet** on slurm leaves.
5. **Smoke job** — `kubectl exec` into the login pod and run `srun` to confirm scheduling.

## Generate Recipe

Pick the row that matches your cluster. Each resolves to a slurm leaf with at least three inline Slinky components: `slinky-slurm-operator-crds`, `slinky-slurm-operator`, and `slinky-slurm`. The H100 GKE and Kind leaves also include `slinky-topograph` (topology-aware scheduling): GKE with the `gcp` provider, Kind with the `dra` provider reading each node's `nvidia.com/gpu.clique` label. The other leaves do not include it today; see [Slinky Slurm Inline Components](https://github.com/NVIDIA/aicr/blob/main/docs/integrator/recipe-development.md#slinky-slurm-inline-components) to add it to another leaf.


| Cloud    | Command                                                                                                      | Leaf overlay                                               |
| -------- | ------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------- |
| **AKS**  | `aicr recipe --service aks --accelerator h100 --intent training --os ubuntu --platform slurm -o recipe.yaml` | `h100-aks-ubuntu-training-slurm`                           |
| **EKS**  | `aicr recipe --service eks --accelerator h100 --intent training --os ubuntu --platform slurm -o recipe.yaml` | `h100-eks-ubuntu-training-slurm`                           |
| **GKE**  | `aicr recipe --service gke --accelerator h100 --intent training --os cos --platform slurm -o recipe.yaml`    | `h100-gke-cos-training-slurm`                              |
| **EKS GB200** | `aicr recipe --service eks --accelerator gb200 --intent training --os ubuntu --platform slurm -o recipe.yaml` | `gb200-eks-ubuntu-training-slurm`                     |
| **EKS GB300** | `aicr recipe --service eks --accelerator gb300 --intent training --os ubuntu --platform slurm -o recipe.yaml` | `gb300-eks-ubuntu-training-slurm`                     |
| **GKE GB200** | `aicr recipe --service gke --accelerator gb200 --intent training --os cos --platform slurm -o recipe.yaml`    | `gb200-gke-cos-training-slurm`                        |
| **Kind** | `aicr recipe --service kind --accelerator h100 --intent training --platform slurm -o recipe.yaml`            | `h100-kind-training-slurm` (simulated GPUs; no GPU GRES)   |


H100 cloud leaves bake in `Gres=gpu:h100:8` and matching `nvidia.com/gpu: 8` slurmd limits so `srun --gres=gpu:N` works after deploy; the GB200 and GB300 leaves bake in `Gres=gpu:gb200:4` / `Gres=gpu:gb300:4` with `nvidia.com/gpu: 4`.

### Hybrid snapshot mode

For an existing cluster, capture infrastructure criteria and resolve the Slurm
leaf while keeping workload intent explicit:

```shell
aicr snapshot -o system.yaml
aicr recipe \
  --snapshot system.yaml \
  --intent training \
  --platform slurm \
  -o recipe.yaml
```

> **AKS:** capture the snapshot with the GPU pool projection or the
> snapshot-qualified resolve (and validate readiness) fails closed:
> `az aks nodepool list -g <rg> --cluster-name <cluster> -o json > pools.json`
> then `aicr snapshot --aks-gpu-pools pools.json -o system.yaml`; add
> `--profile gpuStack=operator-managed` to `aicr recipe` when the GPU pools were
> created with `--gpu-driver none`. Criteria-only generation (the AKS row
> above) needs no pool dump, but the `--profile gpuStack=operator-managed` flag is
> still required for `--gpu-driver none` pools — the default profile is
> azure-managed, and without a snapshot nothing catches the mismatch until
> `aicr validate`.

The `K8s.slinky-slurm` summary distinguishes absent, detected,
unsupported-multicluster, and unknown outcomes. It does not prove operator/runtime
health and does not reconstruct the current Slinky chart values. Review and
apply any required `slinkyslurm` overrides during bundle generation. Snapshots
created before this subtype existed continue to work with the same explicit
intent and platform flags.

## Generate Bundle

### Scheduling model

AICR injects placement from bundle flags using each component's registry paths:


| Flag                                                            | Typical targets                                     |
| --------------------------------------------------------------- | --------------------------------------------------- |
| `--system-node-selector` / `--system-node-toleration`           | cert-manager, **slurm-operator**, prometheus, …     |
| `--accelerated-node-selector` / `--accelerated-node-toleration` | `nodesets.slinky` (slurmd workers)                  |
| `--set-json slinkyslurm:…`                                      | Per-leaf overrides on the cluster chart (see below) |


**Registry default for `slinky-slurm`:** `controller`, `restapi`, and `loginsets.slinky` use the **system** paths; `nodesets.slinky` uses **accelerated** paths. On split clusters (system pool + GPU pool), override the control plane onto the pool you want with `--set-json` (runs **after** selector injection and wins on those paths).

**Operator note:** slurm-operator chart v1.2.0 honors `nodeSelector`, `tolerations`, and `affinity` for both the operator and webhook. AICR's `--system-node-selector` and `--system-node-toleration` flags fan out to both deployments. Set affinity through `--set-json slurmoperator:operator.affinity=...` and `--set-json slurmoperator:webhook.affinity=...`. On EKS, include **both** `NoSchedule` and `NoExecute` for each taint key — nodes often carry both effects.

**Override aliases:** `slinkyslurm`, `slurmcluster` (cluster chart); `slurm`, `slurmoperator`, `slinkyslurmoperator` (operator chart). See `valueOverrideKeys` in `recipes/registry.yaml`.

**Scalar vs structured overrides:**

- `--set slinkyslurm:nodesets.slinky.replicas=2` — replicas, simple scalars.
- `--set-json slinkyslurm:controller.podSpec=…` — full `nodeSelector` / `tolerations` objects (required when overriding system-injected scheduling on control-plane paths).

### Prolog and epilog scripts

Slinky represents each hook collection as a map from the script filename to its
contents. Create YAML files containing those maps; do not pass a raw `.sh` file
directly because `--set-file` parses the file as one JSON or YAML value. Every
script must include a shebang.

```yaml
# prolog-scripts.yaml
00-site-prolog.sh: |
  #!/usr/bin/env bash
  set -euo pipefail
  # Site-specific setup commands go here.
  exit 0
```

```yaml
# epilog-scripts.yaml
00-site-epilog.sh: |
  #!/usr/bin/env bash
  set -euo pipefail
  # Site-specific cleanup commands go here.
  exit 0
```

Set one or both NodeSet hooks while generating the bundle:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set-file slinkyslurm:prologScripts=./prolog-scripts.yaml \
  --set-file slinkyslurm:epilogScripts=./epilog-scripts.yaml \
  --output bundle
```

`prologScripts` and `epilogScripts` run on the NodeSets. To run hooks on
`slurmctld` instead, use the same file format with
`slinkyslurm:prologSlurmctldScripts` and
`slinkyslurm:epilogSlurmctldScripts`. The map key becomes the script filename,
so add more entries to a file when a hook needs multiple scripts. See
[Slurm Prolog and Epilog](https://slurm.schedmd.com/prolog_epilog.html) for hook
ordering, environment, timeouts, and failure behavior.

### Enroot configuration

AICR supplies cluster-wide Enroot configuration to the Slinky login and compute
containers. It sets `NCCL_DEBUG=WARN` by default. To override that environment
default for one job, export the value and name it with Pyxis's
`--container-env` option:

```shell
export NCCL_DEBUG=INFO
export NCCL_DEBUG_SUBSYS=INIT,NET

srun --container-image=docker://alpine:latest \
  --container-env=NCCL_DEBUG,NCCL_DEBUG_SUBSYS \
  env | grep '^NCCL_DEBUG'
```

For a persistent cluster-wide change, override the component value while
generating the bundle:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:enroot.env.NCCL_DEBUG=INFO \
  --set slinkyslurm:enroot.env.NCCL_DEBUG_SUBSYS=INIT,NET \
  --output bundle
```

For the complete `enroot.config` and `enroot.env` interface, environment
precedence, verification, and the required drain-and-replace procedure for an
existing cluster, see
[Slinky Slurm Enroot Configuration](../docs/user/slinky-slurm-enroot.md).

### EKS (dual taints: `system-workload` / `worker-workload`)

Example layout: 3× `system-worker`, 1× `gpu-worker`. Operator + platform stack on system nodes; slurmd on GPU; controller / login / restapi pinned to GPU via `--set-json`.

```shell
WORKER_TOLS='[{"key":"dedicated","operator":"Equal","value":"worker-workload","effect":"NoSchedule"},{"key":"dedicated","operator":"Equal","value":"worker-workload","effect":"NoExecute"}]'

aicr bundle \
  --recipe recipe.yaml \
  --deployer helm \
  --system-node-selector nodeGroup=system-worker \
  --system-node-toleration dedicated=system-workload:NoSchedule \
  --system-node-toleration dedicated=system-workload:NoExecute \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --storage-class <storage-class> \
  --set slinkyslurm:nodesets.slinky.replicas=1 \
  --set-json "slinkyslurm:controller.podSpec={\"nodeSelector\":{\"nodeGroup\":\"gpu-worker\"},\"tolerations\":${WORKER_TOLS}}" \
  --set-json "slinkyslurm:restapi.podSpec={\"nodeSelector\":{\"nodeGroup\":\"gpu-worker\"},\"tolerations\":${WORKER_TOLS}}" \
  --set-json "slinkyslurm:loginsets.slinky.podSpec={\"nodeSelector\":{\"nodeGroup\":\"gpu-worker\"},\"tolerations\":${WORKER_TOLS}}" \
  --output bundle
```

Set `replicas` to your GPU node count when you have multiple workers.

### GKE (system + cpu + gpu pools; GPU taint only)

Example layout: 3× `system-worker` (no taints), 1× `cpu-worker` (no taints), 2× `gpu-worker` (`dedicated=gpu-workload:NoSchedule`). Control plane on **cpu-worker**; slurmd on **gpu-worker**.

```shell
aicr bundle \
  --recipe recipe.yaml \
  --deployer helm \
  --system-node-selector nodeGroup=system-worker \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=gpu-workload:NoSchedule \
  --storage-class <storage-class> \
  --set slinkyslurm:nodesets.slinky.replicas=2 \
  --set-json 'slinkyslurm:controller.podSpec={"nodeSelector":{"nodeGroup":"cpu-worker"}}' \
  --set-json 'slinkyslurm:restapi.podSpec={"nodeSelector":{"nodeGroup":"cpu-worker"}}' \
  --set-json 'slinkyslurm:loginsets.slinky.podSpec={"nodeSelector":{"nodeGroup":"cpu-worker"}}' \
  --output bundle
```

GKE system nodes should **not** carry custom taints (konnectivity and other managed pods break). No `--system-node-toleration` on GKE when system/cpu pools are untainted.

Optional: `--accelerated-node-toleration nvidia.com/gpu=present:NoSchedule` (harmless if that taint is absent).

### AKS (system + cpu + gpu pools; CriticalAddonsOnly + GPU taint)

Example layout: 3× `system` (`CriticalAddonsOnly=true:NoSchedule`), 1× `cpuworker1` (untainted), 2× `gpuworker1` (`nvidia.com/gpu=present:NoSchedule`). Operator on **system**; controller / login / restapi on **cpuworker1**; slurmd on **gpuworker1**.

```shell
aicr bundle \
  --recipe recipe.yaml \
  --deployer helm \
  --system-node-selector agentpool=system \
  --system-node-toleration CriticalAddonsOnly=true:NoSchedule \
  --accelerated-node-selector agentpool=gpuworker1 \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --set slinkyslurm:nodesets.slinky.replicas=2 \
  --set-json 'slinkyslurm:controller.podSpec={"nodeSelector":{"agentpool":"cpuworker1"}}' \
  --set-json 'slinkyslurm:restapi.podSpec={"nodeSelector":{"agentpool":"cpuworker1"}}' \
  --set-json 'slinkyslurm:loginsets.slinky.podSpec={"nodeSelector":{"agentpool":"cpuworker1"}}' \
  --output bundle
```

AKS ships `managed-csi` as the default StorageClass; omit `--storage-class` unless you need a non-default class.

### Kind (simulated GPUs)

The Kind leaf runs Topograph with the `dra` provider, which builds Slurm blocks from each node's `nvidia.com/gpu.clique` label and drops nodes that have none. A stock Kind cluster has neither GPUs nor that label, so build the cluster the way the gated [`uat-kind-sim`](../.github/workflows/uat-kind-sim.yaml) lane does, with the lane's own files. Run these from the root of a checkout of this repository:

```shell
# Four workers with simulated H100s (mocked NVML, the NVIDIA device plugin, and
# a real DCGM host engine reading the mock) in two nvidia.com/gpu.clique cliques
# of two; the control plane stays GPU-free. Expect large pulls: the host engine
# image is 1.9GB (up to 900s to roll out), and the script also pre-pulls
# NVSentinel's 2.5GB GPU health monitor image (up to 1200s), so that the
# helmfile apply below does not hit nvsentinel's 600s wait.
# Needs kind, kubectl, helm 4, and yq (.settings.yaml pins the lane's versions).
tests/uat/kind/bootstrap-cluster.sh
kubectl config use-context kind-aicr-uat-slurm

# The lane's AICRConfig without its evidence block, which would sign the
# validation result and push it to the project's registry.
yq 'del(.spec.validate.evidence)' tests/uat/kind/tests/h100-training-slurm-config.yaml > kind-slurm.yaml

aicr snapshot --config kind-slurm.yaml   # snapshot.yaml, read by validate below
aicr recipe --config kind-slurm.yaml     # recipe.yaml, the same leaf as the Kind row above
aicr bundle --config kind-slurm.yaml     # ./bundle, helmfile deployer
```

The config's `spec.bundle.deployment.set` fits the bundle to this cluster; its header gives the full reasoning:

- `slinkyslurm:nodesets.slinky.replicas=4`: one slurmd pod per worker, so the leaf's `blockSizes: [2]` fills two blocks. The leaf itself ships one replica.
- `gpuoperator:enabled=false` and `dradriver:enabled=false`: gpu-operator needs the `nvidia` container runtime and the DRA driver needs a driver root on the node, and `kindest/node` has neither, so neither would become ready. `aicr bundle` prints a deprecation notice for these two keys, which the config header explains.

Install the bundle and check the topology and the GPU health monitors, in place of the `deploy.sh` step under [Install Bundle](#install-bundle):

```shell
(cd bundle && helmfile apply --skip-diff-on-install)   # prerequisites: bundle/README.md
tests/uat/kind/verify-topology.sh
tests/uat/kind/verify-gpu-health-monitors.sh
```

`verify-topology.sh` derives the expected blocks from where the slurmd pods landed and each node's clique label, and compares them with the `topology.conf` Topograph writes, retrying for up to five minutes while Topograph syncs.

`verify-gpu-health-monitors.sh` is the lane's `gpu_health` gate. It fails unless NVSentinel's labeler has stamped `dcgm.version` on every worker and the GPU health monitor is Ready on each and connected to that worker's host engine. It fails rather than skips when the monitors sit at zero pods, because zero pods is what a missing host engine looks like.

Validate the conformance phase only:

```shell
aicr validate --config kind-slurm.yaml --phase conformance --output report.json
```

The deployment phase does not apply here: it checks the excluded gpu-operator and DRA driver, and waits for them until the validator Job's deadline. The config's `spec.validate.execution.skipChecks` withholds the seven conformance checks that need the real GPU stack and reports them as skipped; `slinky-slurm-health`, `gang-scheduling`, and `cluster-autoscaling` still run. Then continue at [Run Job](#run-job).

> No `nv-sentinel` flag is needed on any of these platforms. The driver is
> host-installed on Kind (and node-image-installed on GKE COS and AKS
> above), so no driver pod is observable by the NVSentinel labeler — and
> the recipes now assign `labeler.assumeDriverInstalled` themselves. See
> [NVSentinel on provider-installed-driver platforms](../docs/user/component-catalog.md#nvsentinel-on-provider-installed-driver-platforms).

The automated run of the Kind path is the `uat-kind-sim` workflow, dispatched manually on `main`. `make kwok-e2e` does not cover this leaf: KWOK has no `kind` node profile, and its nodes have no kubelet to run slurmd. `make check-health COMPONENT=slinky-slurm` runs that component's Chainsaw health check against the current cluster.

### Storage class

Set `--storage-class` to a StorageClass that exists (`kubectl get storageclass`). The kube-prometheus-stack overlay uses a `volumeClaimTemplate` without a default `storageClassName`; a missing/default SC leaves PVCs Pending.

## Install Bundle

```shell
(cd ./bundle && chmod +x deploy.sh && ./deploy.sh)
```

Deploy order: `cert-manager` → `slinky-slurm-operator-crds` → `slinky-slurm-operator` → `slinky-slurm` (→ `slinky-topograph` on the H100 GKE and Kind leaves, after `slinky-slurm` so the slurm chart owns the ConfigMap Topograph patches).

```shell
kubectl rollout status -n slinky deploy/slurm-operator
kubectl get pods -n slurm
kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Available")].status}'=True \
  -n slurm deploy/slinky-slurm-login-slinky --timeout=10m
```

If nodewright is already installed, generate the bundle without the nodewright components (`--set nodewright:enabled=false --set nodewrightcustomizations:enabled=false`) to avoid upgrade conflicts.

## Validate Cluster

> **AKS note:** the validate commands below capture cluster state inline, and the
> gpuStack profile qualifies against the `K8s.aks-gpu-pools.gpu-driver` reading. On
> AKS either pass the already-captured `system.yaml` via `--snapshot`, or export
> `AICR_AKS_GPU_POOLS_PATH` (or pass `--aks-gpu-pools`) so live capture carries the
> reading — otherwise the profiled readiness check fails closed.

Use **deployment** and **conformance**. Performance validation is **not supported yet** on slurm leaves — there is no Slurm-native NCCL (or equivalent) check in AICR today; a K8s Pod benchmark would bypass slurmd and is the wrong path on a Slinky-managed cluster.


| Phase         | What it checks                                                                                                         |
| ------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `deployment`  | Component Chainsaw health (CRs, Deployments, DaemonSets ready), including `slinky-slurm` readiness (long retry budget) |
| `conformance` | The inherited AI conformance checks (`platform-health`, `gpu-operator-health`, `dra-support`, `gang-scheduling`, and the rest; run `yq .validation.conformance.checks recipe.yaml` for the leaf's list) plus `slinky-slurm-health`: controller and node health, bounded `srun`, and completed-job persistence through `sacct` when accounting is enabled. The cloud leaves add `slinky-slurm-gpu-access`: in a one-GPU Slurm job, `nvidia-smi` lists exactly one GPU and exactly one GPU device node opens; in a job with no GPU allocation on the same node, `nvidia-smi` lists no GPU and no GPU device node opens. `secure-accelerator-access` is inherited but skips on slurm leaves; `slinky-slurm-gpu-access` produces the GPU access and isolation evidence instead. The GB200 and GB300 leaves add `slinky-slurm-imex-channel` |
| `performance` | **Not supported yet** on slurm leaves                                                                                  |
| `all`         | Runs deployment → conformance → performance in sequence; the performance step has nothing to run on slurm leaves       |


Every post-install run below reads `bundle/recipe.yaml`, the recipe the bundle
deployed, rather than the original `recipe.yaml`.

### All phases

```shell
aicr validate \
  --recipe bundle/recipe.yaml \
  --phase all \
  --output report.json
```

Prefer `--phase deployment --phase conformance` when you only want the supported checks.

### Specific phases

```shell
# After deploy.sh — component + CR readiness (Chainsaw)
aicr validate \
  --recipe bundle/recipe.yaml \
  --phase deployment \
  --output report-deployment.json

# Slurm behavior from login pod (conformance Job)
aicr validate \
  --recipe bundle/recipe.yaml \
  --phase conformance \
  --output report-conformance.json

# Both — common after install
aicr validate \
  --recipe bundle/recipe.yaml \
  --phase deployment \
  --phase conformance \
  --output report.json
```

### Scheduling flags on validate

When validate captures cluster state inline (no `-s`), pass `--node-selector` and `--toleration` so the snapshot agent Job can schedule on tainted nodes. Match your **system** pool (not the GPU pool) unless you intend to run the agent on GPU nodes.

**EKS example** (agent on system nodes):

```shell
aicr validate \
  --recipe bundle/recipe.yaml \
  --node-selector nodeGroup=system-worker \
  --toleration dedicated=system-workload:NoSchedule \
  --toleration dedicated=system-workload:NoExecute \
  --phase deployment \
  --phase conformance \
  --output report.json
```

**GKE example** (untainted system pool; `--toleration` optional):

```shell
aicr validate \
  --recipe bundle/recipe.yaml \
  --node-selector nodeGroup=system-worker \
  --toleration dedicated=gpu-workload:NoSchedule \
  --phase deployment \
  --phase conformance \
  --output report.json
```

`--toleration` on validate applies to inner conformance/deployment Jobs; pair it with `--node-selector` when the default GPU auto-selector (`nvidia.com/gpu.present=true`) would land on tainted nodes you cannot tolerate.

Readiness constraints (K8s version, OS, …) still run before any phase; they use measurements from the inline capture path above.

## Run Job

SSH is disabled by default on the login chart; use `kubectl exec`.

```shell
kubectl exec -n slurm deploy/slinky-slurm-login-slinky -- sinfo
kubectl exec -n slurm deploy/slinky-slurm-login-slinky -- \
  srun --immediate=5 --time=0:03 hostname
```

Multi-node (when `replicas >= 2`):

```shell
kubectl exec -n slurm deploy/slinky-slurm-login-slinky -- srun -N2 hostname
```

GPU GRES smoke (H100 cloud leaves):

```shell
kubectl exec -n slurm deploy/slinky-slurm-login-slinky -- \
  sh -c 'srun -N2 --gres=gpu:8 nvidia-smi -L | sort -u | wc -l'
```

## Cleanup

Cluster instance only (keep operator + CRDs):

```shell
helm uninstall slinky-slurm -n slurm
```

Full Slurm stack (without topograph):

```shell
helm uninstall slinky-slurm -n slurm
helm uninstall slinky-slurm-operator -n slinky
helm uninstall slinky-slurm-operator-crds -n slinky
kubectl delete ns slurm slinky --ignore-not-found
```

Full Slurm stack with topograph:

```shell
helm uninstall slinky-topograph -n topograph
helm uninstall slinky-slurm -n slurm
helm uninstall slinky-slurm-operator -n slinky
helm uninstall slinky-slurm-operator-crds -n slinky
kubectl delete ns slurm topograph slinky --ignore-not-found
```

Helm does not remove CRDs or PVCs by default; delete manually when you need a clean re-install.

## Success

- `deployment` + `conformance` phases pass in the CTRF report (`conformance` only on Kind).
- `sinfo` shows NodeSet nodes idle.
- `srun hostname` returns worker hostnames.
- On GPU leaves, `srun --gres=gpu:N nvidia-smi -L` reaches all GPUs per node (`N` is 8 on H100, 4 on GB200 and GB300).

> Multi-node NCCL via `srun` + Pyxis/Enroot is the natural Slurm-native performance path; it is out of scope for this smoke CUJ and not covered by `aicr validate --phase performance` today.

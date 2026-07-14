# AICR - Critical User Journey (CUJ) 1 — Slinky Slurm

End-to-end walkthrough: **generate recipe (Query Mode) → bundle → deploy → validate → `srun` smoke job**.

Slurm leaves are built from criteria flags (`--service`, `--platform slurm`, …), not from `aicr snapshot` — snapshot intake for Slurm is not supported today. See [Query Mode](../docs/user/cli-reference.md#aicr-recipe) in the CLI reference.

## Assumptions

- `kubectl` is configured for the target cluster.
- GPU leaves assume H100 nodes with drivers (or Kind for the CPU-only path).
- Node pools use a `nodeGroup` label (adjust if your cluster uses different keys).
- Inspect taints before bundling: `kubectl get nodes -o custom-columns=NAME:.metadata.name,GROUP:.metadata.labels.nodeGroup,TAINTS:.spec.taints`

## Workflow

```text
  aicr recipe          aicr bundle        ./deploy.sh       aicr validate       srun smoke
  (Query Mode)    ──▶  (scheduling)  ──▶  (install)    ──▶  (phases)       ──▶  (manual)
```

1. **Generate recipe (Query Mode)** — `aicr recipe --service … --platform slurm` resolves a slurm leaf overlay to `recipe.yaml`.
2. **Generate bundle** — apply `--system-*` / `--accelerated-*` scheduling and optional `--set` / `--set-json` on `slinkyslurm`.
3. **Install** — run `deploy.sh`; cert-manager and Slinky operator come up, then the cluster chart in `slurm`.
4. **Validate** — run `deployment` (Chainsaw component health) and `conformance` (`slinky-slurm-health` from the login pod). **Performance validation is not supported yet** on slurm leaves.
5. **Smoke job** — `kubectl exec` into the login pod and run `srun` to confirm scheduling.

## Generate Recipe (Query Mode)

Pick the row that matches your cluster. Each resolves to a slurm leaf with at least three inline Slinky components: `slinky-slurm-operator-crds`, `slinky-slurm-operator`, and `slinky-slurm`. The GKE and Kind leaves also include `slinky-topograph` (topology-aware scheduling) — GKE with the `gcp` provider, Kind with the `test` provider and a fixed topology fixture. The EKS leaf does not include it today; see [Slinky Slurm Inline Components](../docs/integrator/recipe-development.md#slinky-slurm-inline-components) to add it to another leaf.


| Cloud    | Command                                                                                                      | Leaf overlay                                               |
| -------- | ------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------- |
| **EKS**  | `aicr recipe --service eks --accelerator h100 --intent training --os ubuntu --platform slurm -o recipe.yaml` | `h100-eks-ubuntu-training-slurm`                           |
| **GKE**  | `aicr recipe --service gke --accelerator h100 --intent training --os cos --platform slurm -o recipe.yaml`    | `h100-gke-cos-training-slurm`                              |
| **Kind** | `aicr recipe --service kind --accelerator h100 --intent training --platform slurm -o recipe.yaml`            | `h100-kind-training-slurm` (CPU-only NodeSet; no GPU GRES) |


H100 cloud leaves bake in `Gres=gpu:h100:8` and matching `nvidia.com/gpu: 8` slurmd limits so `srun --gres=gpu:N` works after deploy.

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

**Override aliases:** `slinkyslurm`, `slurmcluster` (cluster chart); `slurm`, `slurmoperator` (operator chart). See `valueOverrideKeys` in `recipes/registry.yaml`.

**Scalar vs structured overrides:**

- `--set slinkyslurm:nodesets.slinky.replicas=2` — replicas, simple scalars.
- `--set-json slinkyslurm:controller.podSpec=…` — full `nodeSelector` / `tolerations` objects (required when overriding system-injected scheduling on control-plane paths).

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

### Kind (CPU-only smoke / CI)

No GPU pools or taints; omit accelerated flags unless your Kind config adds them.

```shell
aicr bundle \
  --recipe recipe.yaml \
  --deployer helm \
  --output bundle
```

For automated no-GPU checks, see `make kwok-e2e` / `make check-health COMPONENT=slinky-slurm` in the repo Makefile.

### Storage class

Set `--storage-class` to a StorageClass that exists (`kubectl get storageclass`). The kube-prometheus-stack overlay uses a `volumeClaimTemplate` without a default `storageClassName`; a missing/default SC leaves PVCs Pending.

## Install Bundle

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

Deploy order: `cert-manager` → `slinky-slurm-operator-crds` → `slinky-slurm-operator` → `slinky-slurm` (→ `slinky-topograph` on the GKE and Kind leaves, after `slinky-slurm` so the slurm chart owns the ConfigMap Topograph patches).

```shell
kubectl rollout status -n slinky deploy/slurm-operator
kubectl get pods -n slurm
kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Available")].status}'=True \
  -n slurm deploy/slinky-slurm-login-slinky --timeout=10m
```

If nodewright is already installed, skip those sections in `deploy.sh` to avoid upgrade conflicts.

## Validate Cluster

Use **deployment** and **conformance**. Performance validation is **not supported yet** on slurm leaves — there is no Slurm-native NCCL (or equivalent) check in AICR today; a K8s Pod benchmark would bypass slurmd and is the wrong path on a Slinky-managed cluster.


| Phase         | What it checks                                                                                                         |
| ------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `deployment`  | Component Chainsaw health (CRs, Deployments, DaemonSets ready), including `slinky-slurm` readiness (long retry budget) |
| `conformance` | `slinky-slurm-health`: `scontrol ping`, idle/mix node gate, bounded `srun --immediate=5 --time=0:03 hostname`          |
| `performance` | **Not supported yet** on slurm leaves                                                                                  |
| `all`         | Runs deployment → conformance → performance in sequence; the performance step has nothing to run on slurm leaves       |


### All phases

```shell
aicr validate \
  --recipe recipe.yaml \
  --phase all \
  --output report.json
```

Prefer `--phase deployment --phase conformance` when you only want the supported checks.

### Specific phases

```shell
# After deploy.sh — component + CR readiness (Chainsaw)
aicr validate \
  --recipe recipe.yaml \
  --phase deployment \
  --output report-deployment.json

# Slurm behavior from login pod (conformance Job)
aicr validate \
  --recipe recipe.yaml \
  --phase conformance \
  --output report-conformance.json

# Both — common after install
aicr validate \
  --recipe recipe.yaml \
  --phase deployment \
  --phase conformance \
  --output report.json
```

### Scheduling flags on validate

When validate captures cluster state inline (no `-s`), pass `--node-selector` and `--toleration` so the snapshot agent Job can schedule on tainted nodes. Match your **system** pool (not the GPU pool) unless you intend to run the agent on GPU nodes.

**EKS example** (agent on system nodes):

```shell
aicr validate \
  --recipe recipe.yaml \
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
  --recipe recipe.yaml \
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

- `deployment` + `conformance` phases pass in the CTRF report.
- `sinfo` shows NodeSet nodes idle.
- `srun hostname` returns worker hostnames.
- On GPU leaves, `srun --gres=gpu:8 nvidia-smi -L` reaches all GPUs per node.

> Multi-node NCCL via `srun` + Pyxis/Enroot is the natural Slurm-native performance path; it is out of scope for this smoke CUJ and not covered by `aicr validate --phase performance` today.

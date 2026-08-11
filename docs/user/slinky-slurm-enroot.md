# Slinky Slurm Enroot Configuration

AICR configures Enroot for OCI containers launched through Pyxis on Slinky
Slurm. The same configuration is mounted into the login and `slurmd`
containers so interactive and batch jobs receive consistent defaults.

## Shipped defaults

The Slinky Slurm component ships these provider-neutral defaults:

| Component value | Default | Purpose |
| --- | --- | --- |
| `enroot.config.ENROOT_MOUNT_HOME` | `yes` | Mount the user's home directory in the container |
| `enroot.config.ENROOT_REMAP_ROOT` | `yes` | Remap the current user to root inside the container, equivalent to Enroot's `--root` option |
| `enroot.env.NCCL_DEBUG` | `WARN` | Retain actionable NCCL failure diagnostics without `INFO`-level log volume |

AICR renders `enroot.config` into `/etc/enroot/enroot.conf` and `enroot.env`
into `/etc/enroot/environ.d/99-aicr-defaults.env`. Both files come from the
`slinky-slurm-enroot-config` ConfigMap and are mounted read-only into the
default LoginSet and NodeSet pods.

## Configure cluster-wide defaults

Override an existing key or add a site-specific key while generating the
bundle:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:enroot.config.ENROOT_MOUNT_HOME=no \
  --set slinkyslurm:enroot.env.NCCL_DEBUG=INFO \
  --set slinkyslurm:enroot.env.NCCL_DEBUG_SUBSYS=INIT,NET \
  --output bundle
```

Use `enroot.config` for Enroot runtime settings and `enroot.env` for
cluster-wide environment defaults that should apply to every Enroot container.
Keep provider-, partition-, and workload-specific settings job-scoped unless
they are intentionally a cluster-wide policy.

Keys and values in both maps must be single-line. Newline characters can
produce an invalid ConfigMap or malformed Enroot configuration.

## Manage the generated releases together

The generated `slinky-slurm-pre` release owns the
`slinky-slurm-enroot-config` ConfigMap, while the main `slinky-slurm` release
mounts it into its LoginSet and NodeSet pods. Treat both releases as one logical
component: deploy and upgrade them from the same generated bundle, and do not
prune or uninstall `slinky-slurm-pre` while `slinky-slurm` still exists.

The generated deployers install the pre-release before the main release. If
tearing the deployment down manually, reverse that order: uninstall
`slinky-slurm` before `slinky-slurm-pre`.

## Override an environment default for one job

An `enroot.env` entry is part of the imported container environment. It takes
precedence over an ordinary job export of the same name. To override one for a
job, export the new value and name the variable with Pyxis's `--container-env`
option:

```shell
export NCCL_DEBUG=INFO
export NCCL_DEBUG_SUBSYS=INIT,NET

srun --container-image=docker://alpine:latest \
  --container-env=NCCL_DEBUG,NCCL_DEBUG_SUBSYS \
  env | grep '^NCCL_DEBUG'
```

`srun --export` alone does not override a value already supplied by
`enroot.env`. Settings such as `FI_PROVIDER`, `NCCL_SOCKET_IFNAME`,
`NCCL_NVLS_ENABLE`, and `NCCL_MNNVL_ENABLE` should normally remain job-scoped
so tuning for one workload or partition does not affect another.

## Apply changes to an existing cluster

Kubernetes does not refresh ConfigMap-backed `subPath` mounts in running pods.
After deploying an updated Enroot ConfigMap, replace the affected LoginSet and
NodeSet pods before verifying the new value. This is a maintenance operation:
deleting a worker pod terminates jobs running on that node, and deleting a
login pod terminates active login sessions.

Before replacing any pods, drain every Slurm node to prevent new allocations
and wait for existing jobs to finish. Inspect the initial node states first and
stop if any node is already drained, down, or failed. A cluster-wide
maintenance operation must not overwrite an unrelated state or reason and
later return that node to service.

```shell
sinfo --Node --format='%N %T %E'

# Proceed only when every worker is healthy and available before this rollout.
scontrol update NodeName=ALL State=DRAIN \
  Reason="Enroot configuration rollout"

WORKER_SELECTOR='app.kubernetes.io/instance=slinky-slurm-worker-slinky'

kubectl wait pod -n slurm -l "${WORKER_SELECTOR}" \
  --for=condition=SlurmNodeStateDrain=True --timeout=10m
kubectl wait pod -n slurm -l "${WORKER_SELECTOR}" \
  --for=condition=SlurmNodeStateIdle=True --timeout=24h

squeue --noheader \
  --states=RUNNING,COMPLETING,CONFIGURING,SUSPENDED,STOPPED
```

Do not delete any pod until both waits succeed and `squeue` produces no output.
Pending jobs may remain queued because the drain prevents them from starting.

Replace worker pods one at a time. Wait for each replacement to become Ready
and register with Slurm before continuing. Worker shutdown can leave the
replacement Slurm node `DOWN`; do not return it to service yet.

```shell
kubectl delete pod -n slurm <worker-pod>
kubectl wait --for=condition=Ready -n slurm \
  pod/<replacement-worker-pod> --timeout=10m

scontrol show node <slurm-node>
```

After every worker is healthy, notify connected users and replace the login
pods. The default AICR configuration has one login replica:

```shell
LOGIN_SELECTOR='app.kubernetes.io/instance=slinky-slurm-login-slinky'
LOGIN_POD=$(kubectl get pod -n slurm -l "${LOGIN_SELECTOR}" \
  -o jsonpath='{.items[0].metadata.name}')

kubectl delete pod -n slurm "${LOGIN_POD}"
kubectl wait pod -n slurm -l "${LOGIN_SELECTOR}" \
  --for=condition=Ready --timeout=10m
```

If the deployment has multiple login replicas, replace and verify them one at
a time to avoid dropping all login endpoints simultaneously.

Only after every worker and login pod is healthy, each replacement worker has
registered with Slurm, and its state and reason reflect only this maintenance,
return the nodes to service. Then verify the effective value through the Slurm
container path:

```shell
scontrol update NodeName=ALL State=RESUME

srun --container-image=docker://alpine:latest \
  env | grep '^NCCL_DEBUG'
```

# Cloud Native Stack Deployment

Recipe Version: v0.1.0
Bundler Version: v1.0.0

Per-component bundle for deploying NVIDIA Cloud Native Stack components
for GPU-accelerated Kubernetes workloads.

## Configuration



## Components

The following components are included (deployed in order). Each component
lives in a numbered `NNN-<name>/` folder and is installed as a Helm release
via its own `install.sh`:

| Component | Version | Namespace | Source |
|-----------|---------|-----------|--------|
| skyhook-customizations | N/A | skyhook | local |




## Quick Start

Run the included deployment script:

```bash
chmod +x deploy.sh
./deploy.sh
```

Use `--no-wait` to skip Helm chart-level waiting where AICR uses `--wait` (keeps `--timeout` for hooks):

```bash
./deploy.sh --no-wait
```

> **Note:** The deploy script's final status reflects install/apply results. If `--best-effort` was used, one or more components may still have failed; check warning lines and logs. This does **not** guarantee the cluster is ready to schedule workloads — operator-driven cluster convergence (CRD reconciliation, node tuning, plugin registration, etc.) continues asynchronously after the script exits, in operator-specific ways. See the [AICR CLI Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md#deploy-script-behavior-deploysh) for details.

## Manual Installation

Each component folder contains an `install.sh` that runs `helm upgrade --install`
with the right arguments baked in. To install a single component manually:

```bash
cd NNN-<component-name>
bash install.sh
```

## Customization

Each component folder has its own `values.yaml` (static) and `cluster-values.yaml`
(dynamic, per-cluster). Edit either before deploying:

```bash
vim NNN-<component-name>/values.yaml
vim NNN-<component-name>/cluster-values.yaml
```

## Upgrade

Re-run the per-component install.sh to upgrade an already-installed release:

```bash
cd NNN-<component-name>
bash install.sh
```

## Uninstall

To remove components (reverse order):

```bash
./undeploy.sh
```

Or remove a single release manually:

```bash
helm uninstall skyhook-customizations -n skyhook
```


## Troubleshooting

### Check deployment status

```bash
kubectl get pods -A | grep -E 'skyhook-customizations'
```

### View component logs

Inspect a single component's pods (replace `<component>` and `<namespace>`
with one of the entries from the table above):

```bash
kubectl logs -n <namespace> -l app.kubernetes.io/instance=<component>
```


## References

- [AICR CLI Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md)

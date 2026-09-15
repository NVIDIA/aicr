# AI Cluster Runtime Deployment

Recipe Version: v0.1.0
Bundler Version: v1.0.0

Per-component bundle for deploying NVIDIA AI Cluster Runtime components
for GPU-accelerated Kubernetes workloads.

## Configuration



## Components

The following components are included (deployed in order). Each component
lives in a numbered `NNN-<name>/` folder and is installed as a Helm release
via its own `install.sh`:

| Component | Version | Namespace | Source |
|-----------|---------|-----------|--------|
| k8s-aibom | 1.3.0 | k8s-aibom-system | k8s-aibom (oci://ghcr.io/googlecloudplatform/charts) |




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

> **Helm 4 vs Helm 3:** On Helm 4 (server-side apply by default), each
> `install.sh` automatically passes `--force-conflicts` so the upgrade can
> overwrite fields that operators (cert-manager, gpu-operator, nvsentinel,
> grove, ...) own on their rotated webhook cert Secrets — without it the
> upgrade fails on field-manager conflicts. On Helm 3 (client-side apply,
> no field-manager conflicts) the flag is omitted; the script detects the
> Helm major version at run time, so the same bundle works with either
> binary.

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

> **CRDs on upgrade.** Helm installs a chart's `crds/` directory on first
> install and never touches it again, so a chart bump whose CRDs changed would
> otherwise run the new controller against the old schema. Folders that also
> contain an `apply-crds.sh` have `install.sh` run it first, which applies the
> pinned chart's CRDs with `kubectl apply --server-side --force-conflicts`.
> Those folders need `kubectl` and `timeout` (GNU coreutils) on `$PATH` in
> addition to `helm`; the script refuses to run rather than run unbounded
> inside a deploy, so on macOS install coreutils or apply the CRDs by hand.
> Every helm and kubectl call it makes is bounded, 90s by default and
> overridable with `AICR_CRD_STEP_TIMEOUT`. The step is skipped when
> `DRY_RUN_FLAG` is set, and on a release that does not exist yet, since
> `helm install` creates the chart's CRDs itself. Only components audited as
> the sole owner of every CRD they ship get the script; for the rest, replacing
> CRDs is unsafe because another component's release ships the same CRD with a
> different schema.

## Uninstall

Bundles do not ship an `undeploy.sh`. Uninstall releases in reverse
deployment order using `helm uninstall` directly — one command per
`NNN-<release>/` folder the deploy script installs, including any
injected `*-pre` / `*-post` auxiliaries:

```bash
helm uninstall k8s-aibom -n k8s-aibom-system
```

CRDs installed by these charts are intentionally not deleted by Helm; remove
them only when you are sure no other release depends on them. See the
[deployer-native uninstall walkthrough](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md#bundle-uninstall) in the AICR CLI reference for details on
PVC handling, namespace teardown, and the equivalent paths for ArgoCD and
ArgoCD+Helm bundles.

## Troubleshooting

### Check deployment status

```bash
kubectl get pods -A | grep -E 'k8s-aibom'
```

### View component logs

Inspect a single component's pods (replace `<component>` and `<namespace>`
with one of the entries from the table above):

```bash
kubectl logs -n <namespace> -l app.kubernetes.io/instance=<component>
```


## References

- [AICR CLI Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md)

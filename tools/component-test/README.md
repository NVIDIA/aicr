# Component Test Harness

Validate AICR components end-to-end with a single command. No GPU hardware required for most components.

## Quick Start

```bash
make build
make component-test COMPONENT=cert-manager
```

Auto-detects the test tier, creates a Kind cluster, deploys the component, and runs its health check. Components detected as `scheduling` tier are redirected to the KWOK infrastructure (`make kwok-e2e`) and exit with code 2 — no Kind cluster is created for those.

## Test Tiers

| Tier | What it validates | Cluster needs | Example |
|------|-------------------|---------------|---------|
| `scheduling` | Pods schedule on correct nodes | Kind + KWOK | Any component with overlays |
| `deploy` | Component deploys and health checks pass | Kind | cert-manager, kai-scheduler |
| `gpu-aware` | GPU-dependent components init against fake GPUs | Kind + nvml-mock | gpu-operator |

### Auto-Detection

The harness reads `recipes/registry.yaml` to determine the tier:

| Has health check? | GPU references? | Detected tier |
|--------------------|-----------------|---------------|
| No | No | `scheduling` |
| Yes | No | `deploy` |
| Yes | Yes | `gpu-aware` |
| No | Yes | `gpu-aware` (warns about missing health check) |

Override with `TIER=`:

```bash
make component-test COMPONENT=gpu-operator TIER=gpu-aware
```

Or set `testTier` in `registry.yaml`:

```yaml
- name: my-component
  testTier: gpu-aware
  helm: ...
```

**Dependencies are not deployed.** The harness bundles and deploys exactly one
component; `dependencyRefs` declared on the component's overlay/mixin refs are
NOT installed — for chart-backed and manifest-only components alike. If the
component hard-depends on another component (e.g. `nodewright-customizations`
applies a Skyhook CR whose CRD ships with `nodewright-operator`;
`kubeflow-trainer` needs `cert-manager` webhooks), install the dependency
first with `KEEP_CLUSTER=true` so the EXIT-trap cleanup does not immediately
uninstall it — for example
`make component-test COMPONENT=nodewright-operator KEEP_CLUSTER=true` — or
expect the deploy step to fail on the bare test cluster. The deploy script
prints a warning listing undeployed dependencies: for manifest-only
components these come from the exact overlay ref the recipe was synthesized
from; for chart-backed components (whose synthesized recipe is registry
defaults, tied to no variant) the warning lists the union across recipe
variants, so some entries may be variant-only (e.g. a GB200 leaf's DRA
driver) and not needed by the default configuration. Platform-specific
dependencies (e.g. the `*-ocp*`/OLM chains, whose `OperatorGroup` and
`Subscription` manifests need OLM CRDs) cannot be pre-installed with this
Kind-based harness — test those against a matching cluster.

## Make Targets

```bash
# Full test (auto-detect tier, create cluster, deploy, health check, cleanup)
make component-test COMPONENT=cert-manager

# Individual steps (for debugging)
make component-detect COMPONENT=cert-manager     # Show detected tier
make component-cluster                     # Create/reuse Kind cluster
make component-deploy COMPONENT=cert-manager     # Deploy component only
make component-health COMPONENT=cert-manager     # Run health check only
make component-cleanup COMPONENT=cert-manager    # Uninstall component

# Keep cluster for debugging
KEEP_CLUSTER=true make component-test COMPONENT=cert-manager

# Delete cluster entirely
make component-cleanup DELETE_CLUSTER=true
```

## Environment Variables

### Global

| Variable | Default | Purpose |
|----------|---------|---------|
| `COMPONENT` | (required) | Component name from registry.yaml |
| `TIER` | (auto-detected) | Override: `scheduling`, `deploy`, `gpu-aware` |
| `CLUSTER_NAME` | `aicr-component-test` | Kind cluster name |
| `KUBECONFIG` | (auto) | Path to kubeconfig |
| `KEEP_CLUSTER` | `false` | Preserve cluster after test |
| `DEBUG` | `false` | Extra debug logging |

### Cluster (ensure-cluster.sh)

| Variable | Default | Purpose |
|----------|---------|---------|
| `KIND_NODE_IMAGE` | from `.settings.yaml` | Kind node image |
| `KIND_CONFIG` | `tools/component-test/kind-config.yaml` | Kind config file |
| `CLUSTER_WAIT_TIMEOUT` | `120s` | Node readiness timeout |

### GPU Mock (setup-gpu-mock.sh)

| Variable | Default | Purpose |
|----------|---------|---------|
| `NVML_MOCK_VERSION` | from `.settings.yaml` | nvml-mock image tag only; the chart is pinned separately. Setting it drops the pinned image digest unless `NVML_MOCK_IMAGE_DIGEST` is also set |
| `NVML_MOCK_IMAGE_DIGEST` | from `.settings.yaml` | Image index digest the image is pulled by; empty pulls by tag |
| `NVML_MOCK_IMAGE` | `ghcr.io/nvidia/nvml-mock` | Image repository override, such as a mirror; the image digest still applies |
| `NVML_MOCK_CHART` | from `.settings.yaml` | OCI chart reference, without `oci://` |
| `NVML_MOCK_CHART_VERSION` | from `.settings.yaml` | Chart version. Setting it drops the pinned chart digest unless `NVML_MOCK_CHART_DIGEST` is also set. The manifest fallback refuses any version other than the one it was rendered from |
| `NVML_MOCK_CHART_DIGEST` | from `.settings.yaml` | Chart digest the chart is pulled by; empty pulls by `NVML_MOCK_CHART_VERSION` |
| `GPU_PROFILE` | `a100` | GPU profile: `a100`, `h100`, `gb200`. The manifest fallback, used when Helm is missing or the chart install fails, serves `a100` only |
| `GPU_COUNT` | `8` | GPUs per node |
| `DRIVER_VERSION` | auto from profile | Mock driver version (e.g., `550.163.01`) |
| `MOCK_READY_TIMEOUT` | `300s` | DaemonSet readiness timeout |

### Deploy (deploy-component.sh)

| Variable | Default | Purpose |
|----------|---------|---------|
| `HELM_TIMEOUT` | `300s` | Helm install timeout |
| `HELM_NAMESPACE` | from registry.yaml | Override namespace |
| `HELM_VALUES` | (none) | Extra `--values` file |
| `HELM_SET` | (none) | Extra `--set` overrides (comma-separated) |
| `AICR_BIN` | auto-detected from `dist/` | Path to aicr binary |

### Health Check (run-health-check.sh)

| Variable | Default | Purpose |
|----------|---------|---------|
| `HEALTH_CHECK_TIMEOUT` | `5m` | Chainsaw assert timeout |
| `HEALTH_CHECK_FILE` | from registry.yaml | Override health check path |
| `CHAINSAW_BIN` | `chainsaw` | Path to chainsaw binary |

### Cleanup (cleanup.sh)

| Variable | Default | Purpose |
|----------|---------|---------|
| `DELETE_CLUSTER` | `false` | Delete the Kind cluster |
| `FORCE_CLEANUP` | `false` | Skip confirmation prompts |

## Debugging a Failure

```bash
# 1. Run with KEEP_CLUSTER to preserve state
KEEP_CLUSTER=true make component-test COMPONENT=cert-manager

# 2. Inspect pods
kubectl -n cert-manager get pods
kubectl -n cert-manager describe pod <pod-name>
kubectl -n cert-manager logs <pod-name>

# 3. Re-run just the health check
make component-health COMPONENT=cert-manager

# 4. Re-deploy after fixing
COMPONENT=cert-manager bash tools/component-test/cleanup.sh
make component-deploy COMPONENT=cert-manager
make component-health COMPONENT=cert-manager

# 5. Clean up when done
make component-cleanup COMPONENT=cert-manager DELETE_CLUSTER=true
```

## Adding GPU-Aware Testing

For components requiring GPU resources: ensure `.settings.yaml` has `testing.component_test.nvml_mock_version`. GPU references in `values.yaml` or registry entries auto-detect; override via `TIER=gpu-aware` or set `testTier: gpu-aware` in `registry.yaml`. Customize: `GPU_PROFILE=h100 GPU_COUNT=4 make component-test ...`.

This harness deploys one component at a time with its chart's values. To deploy the NFD, gpu-operator, and DRA driver folders of a Kind bundle on mocked GPUs, the cluster needs the Mokka node image and the bundle needs eight bundle-time overrides. Four point the device plugin, GFD, DCGM exporter, and DRA driver at the mock driver root. The other four turn off the validator's CUDA workload and its dev-char symlink creation, turn off the DRA driver's `ComputeDomainCliques` and `IMEXDaemonsWithDNSNames` feature gates, and set its `altProcDevices` to the mock IMEX `proc-devices` file. [`.github/workflows/gpu-operator-mokka-test.yaml`](../../.github/workflows/gpu-operator-mokka-test.yaml) runs that end to end; its header lists the setup steps and its "Generate the Kind recipe and bundle it for Mokka" step carries the overrides and why each is needed.

## Troubleshooting

| Issue | Check |
|-------|-------|
| `aicr binary not found` | Run `make build` first |
| `Component not found in registry` | Verify component name matches `recipes/registry.yaml` |
| `Health check file not found` | Create `recipes/checks/<component>/health-check.yaml` |
| `Kind cluster creation fails` | Check Docker is running, `kind` is installed |
| `Helm install timeout` | Increase `HELM_TIMEOUT`, check pod events |
| `chainsaw not found` | Run `make tools-setup` |
| `nvml-mock not ready` | Increase `MOCK_READY_TIMEOUT`, check DaemonSet logs |

## Architecture

```
make component-test COMPONENT=cert-manager
         │
    detect-tier.sh          → scheduling | deploy | gpu-aware
         │
    ensure-cluster.sh       → Reuse or create Kind cluster
         │
    setup-gpu-mock.sh       → (gpu-aware only) Deploy nvml-mock
         │
    deploy-component.sh     → Bundle + helm install
         │
    run-health-check.sh     → Chainsaw health check
         │
    cleanup.sh              → Uninstall + optionally delete cluster
```

All scripts are independently runnable and accept environment variables for
override. Configuration defaults come from `.settings.yaml`.

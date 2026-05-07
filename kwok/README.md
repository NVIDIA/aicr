# KWOK-Based Cluster Simulation

KWOK (Kubernetes WithOut Kubelet) tests AICR bundles against simulated GPU clusters without real hardware.

## Prerequisites

Versions are pinned in `.settings.yaml`. **Docker Desktop must be running** — Kind uses it to create the local cluster.

```bash
# Kind, lifecycle, bundle deployment, build
brew install kind tilt-dev/tap/ctlptl helm yq goreleaser
```

The `kwok`/`kwokctl` binaries are not required — `make kwok-cluster` installs the KWOK controller into the cluster via `kubectl apply`.

> If `GITLAB_TOKEN` is set, `make build` will fail. Run `unset GITLAB_TOKEN` first.

## Quick Start

Run from the repo root:

```bash
unset GITLAB_TOKEN
make build

# Test a single recipe
make kwok-e2e RECIPE=h100-eks-ubuntu-training-kubeflow

# Test all recipes (sequential, shared cluster)
make kwok-test-all
```

## Architecture

Cluster configuration is inferred from recipe overlays — no separate config files needed.

```mermaid
flowchart LR
    A[Recipe Overlay] --> B[Node Profile]
    B --> C[KWOK Nodes]
    A --> D[Bundle Generation]
    C --> E[Schedule Test]
    D --> E
```

**Components:**

| Component | Location | Purpose |
|-----------|----------|---------|
| Recipe Overlays | `recipes/overlays/*.yaml` | Define cluster criteria (service, accelerator) |
| Node Profiles | `kwok/profiles/{provider}/*.yaml` | Hardware specs per instance type |
| Scripts | `kwok/scripts/` | Create nodes, validate scheduling |
| CI Workflow | `.github/workflows/kwok-recipes.yaml` | Auto-discover and test recipes |

## Profile Mapping

`apply-nodes.sh` reads recipe criteria and selects matching profiles:

| Service | Accelerator | GPU Profile |
|---------|-------------|-------------|
| eks | h100 (default) | `eks/p5-h100.yaml` |
| eks | gb200 | `eks/p6-gb200.yaml` |
| other | any | `eks/p5-h100.yaml` (fallback) |

**Cluster defaults:** 2 system nodes, 4 GPU nodes (32 GPUs), Kubernetes v1.33.5, region `us-east-1`.

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make kwok-test-all` | Test all recipes in shared cluster (serial) |
| `make kwok-e2e RECIPE=<name>` | Full e2e: cluster, nodes, validate |
| `make kwok-cluster` | Create Kind cluster with KWOK |
| `make kwok-nodes RECIPE=<name>` | Create simulated nodes |
| `make kwok-nodes-delete` | Delete all KWOK-simulated nodes |
| `make kwok-test RECIPE=<name>` | Validate scheduling only |
| `make kwok-status` | Show cluster and node status |
| `make kwok-cluster-delete` | Delete cluster |

## Adding Recipes

A recipe is auto-discovered for KWOK testing if it has `spec.criteria.service` defined. Create `recipes/overlays/your-recipe.yaml`:

```yaml
kind: recipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: your-recipe-name
spec:
  base: eks-training
  criteria:
    service: eks        # Required for KWOK testing
    accelerator: h100
    intent: training
  componentRefs:
    - name: gpu-operator
      type: Helm
      valuesFile: components/gpu-operator/values-eks-training.yaml
```

Test it: `unset GITLAB_TOKEN && make build && make kwok-e2e RECIPE=your-recipe-name`.

## Adding Node Profiles or Cloud Providers

Copy an existing profile and modify, then update the mapping in `kwok/scripts/apply-nodes.sh` (`get_profiles()` around line 52):

```bash
cp kwok/profiles/eks/p5-h100.yaml kwok/profiles/eks/p5-a100.yaml
```

For new cloud providers, copy the `kwok/profiles/eks/` directory structure and update `apply-nodes.sh`.

## CI Integration

`.github/workflows/kwok-recipes.yaml` calls the same `run-all-recipes.sh` used by `make kwok-test-all`. CI uses a single shared Kind cluster with cleanup between recipes.

Manual trigger:
```bash
gh workflow run kwok-recipes.yaml -f recipe=your-recipe-name
```

## Troubleshooting

**Pods stuck Pending** — check tolerations and node selectors:
```bash
kubectl describe pod <pod-name> -n aicr-kwok-test
```
GPU pods need toleration `kwok.x-k8s.io/node=fake:NoSchedule` and selector `nvidia.com/gpu.present: "true"`.

**KWOK controller issues**: `kubectl logs -n kube-system deployment/kwok-controller`

**Recipe not being tested**: verify `yq eval '.spec.criteria.service' recipes/overlays/your-recipe.yaml` is non-empty.

## Limitations

KWOK validates scheduling, not runtime: node selectors, tolerations, resource requests, scheduling decisions, and Helm chart generation are checked. Container execution, GPU functionality, and network connectivity are NOT. For runtime testing, use Tilt (`make dev-env`) or a real cluster.

## Resources

- [KWOK docs](https://kwok.sigs.k8s.io/)
- [Development guide](../DEVELOPMENT.md)

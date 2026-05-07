# E2E Tests

End-to-end tests for the AICR CLI and API: snapshot, recipe, validate, and bundle workflows.

## Quick Start

```bash
# 1. Start dev environment (Kind cluster + aicrd)
make dev-env

# 2. Port forward (separate terminal)
kubectl port-forward -n aicr svc/aicrd 8080:8080

# 3. Run tests
make e2e-tilt
```

## What's Tested

| Test | Description |
|------|-------------|
| `build/aicr` | Binary builds successfully |
| `cli/help` | CLI help and version commands |
| `api/health`, `api/ready`, `api/metrics` | Server endpoints |
| `cli/recipe/*` | Recipe generation (query params, criteria file, overrides) |
| `cli/bundle/*` | Bundle generation (helm, argocd, node selectors) |
| `cli/external-data/*` | External data directory (`--data` flag) |
| `cli/format/*` | Output format variations (`--format json/table`) |
| `cli/deploy-agent/*` | Snapshot `--deploy-agent` flag |
| `api/recipe/*`, `api/bundle/*` | REST endpoints |
| `snapshot/*` | Snapshot with deploy-agent (requires fake GPU) |
| `recipe/from-snapshot` | Recipe from ConfigMap snapshot (`cm://...`) |
| `validate/*` | Recipe validation against snapshot |
| `validate/deployment-constraints` | Deployment phase constraints (GPU operator version) |
| `validate/job-*` | Validation Job deployment, RBAC, namespace, cleanup |
| `bundle/oci-push` | Bundle as OCI image to local registry |

## Fake GPU Testing

E2E tests simulate GPU nodes via `tools/fake-nvidia-smi`, returning realistic output for **8x NVIDIA B200 192GB GPUs** (Blackwell). An optional fake-gpu-operator provides K8s-level GPU resource simulation.

### Local Setup

```bash
# Inject fake nvidia-smi into Kind workers
for node in $(docker ps --filter "name=-worker" --format "{{.Names}}"); do
  docker cp tools/fake-nvidia-smi "${node}:/usr/local/bin/nvidia-smi"
  docker exec "$node" chmod +x /usr/local/bin/nvidia-smi
done

# Build aicr image to local registry
KO_DOCKER_REPO=localhost:5001/aicr ko build --bare --tags=local ./cmd/aicr

# Run tests
FAKE_GPU_ENABLED=true AICR_IMAGE=localhost:5001/aicr:local ./tests/e2e/run.sh
```

## Prerequisites

```bash
brew install kind tilt-dev/tap/tilt tilt-dev/tap/ctlptl ko
```
Plus: Docker, kubectl.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AICRD_URL` | `http://localhost:8080` | API URL |
| `OUTPUT_DIR` | temp dir | Test artifacts directory |
| `AICR_IMAGE` | `localhost:5001/aicr:local` | AICR image for snapshot agent |
| `FAKE_GPU_ENABLED` | `false` | Enable fake GPU tests |
| `SNAPSHOT_NAMESPACE` | `default` | Namespace for snapshot tests |
| `SNAPSHOT_CM` | `aicr-e2e-snapshot` | ConfigMap name for snapshot |

## Manual Run

```bash
./tests/e2e/run.sh
```

## CI/CD

The `e2e` job runs in `.github/workflows/on-push.yaml` after unit tests and lint pass, on push to `main` and PRs targeting `main`.

## Cleanup

```bash
make dev-env-clean
```

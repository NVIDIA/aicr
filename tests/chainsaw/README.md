# Chainsaw E2E Tests

End-to-end tests using [Kyverno Chainsaw](https://github.com/kyverno/chainsaw). Declarative YAML assertions replace the bash grep/sed chains previously in `tools/e2e`.

## Install Chainsaw

To install Chainsaw at the version pinned in `.settings.yaml`, run:

```bash
make tools-setup
```

## Running Tests

All CLI tests (no cluster required):

```bash
make e2e
```

Or manually:

```bash
make build
AICR_BIN=$(pwd)/dist/aicr_darwin_arm64_v8.0/aicr \
  chainsaw test --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/cli/
```

Single test:

```bash
AICR_BIN=$(pwd)/dist/aicr_darwin_arm64_v8.0/aicr \
  chainsaw test --no-cluster --test-dir tests/chainsaw/cli/recipe-generation
```

Snapshot tests (cluster required):

```bash
# Setup (one-time)
make cluster-create
make image IMAGE_REGISTRY=localhost:5001/aicr IMAGE_TAG=local
for node in $(docker ps --filter "name=aicr-worker" --format "{{.Names}}"); do
  docker cp tools/fake-nvidia-smi "${node}:/usr/local/bin/nvidia-smi"
  docker exec "$node" chmod +x /usr/local/bin/nvidia-smi
done
chainsaw test --test-dir tests/chainsaw/snapshot/deploy-agent
```

## CLI Tests

No cluster needed. All tests receive `AICR_BIN` and `REPO_ROOT` from the environment.

| Test | Replaces | What it tests |
|------|----------|---------------|
| `cli/recipe-generation` | `generate_recipe()` in `tools/e2e` | Recipe generation via query-mode flags, structural assertion |
| `cli/bundle-variants` | `run_bundle_tests()` in `tools/e2e` | All bundle flag combinations: node selectors, tolerations, value overrides, ArgoCD deployer |
| `cli/bundle-scheduling` | `test_cli_bundle()` scheduling in `tests/e2e/run.sh` | Scheduling injection at correct Helm value paths |
| `cli/cuj1-training` | `test_cuj1()` in `tools/e2e` | Full CUJ1 journey: recipe with kubeflow, validate, bundle, multi-phase |
| `cli/criteria-file` | `test_criteria_file_flag()` in `tools/e2e` | Valid YAML/JSON criteria, CLI overrides, invalid files, partial criteria |
| `cli/validate-phases` | `test_validate_phases()` in `tools/e2e` | Individual phases, --phase all, invalid phase, multiple --phase flags |
| `cli/duplicate-flags` | `test_duplicate_flag_validation()` in `tools/e2e` | Duplicate --recipe, --service flags rejected |
| `cli/validate-agent-flags` | `test_validate_agent_flags()` in `tools/e2e` | Agent flags present in validate --help |
| `cli/external-data` | `test_external_data_flag()` in `tools/e2e` | --data flag: non-existent, missing registry, valid, custom overlay, bundle |
| `cli/snapshot-template` | `test_snapshot_template_flags()` in `tools/e2e` | --template/--format flags, invalid paths, example template |
| `cli/recipe-overlays` | `test_all_recipe_data_files()` in `tools/e2e` | Smoke-test all leaf overlay files: recipe + bundle for each |

## Snapshot Tests

Requires a Kind cluster with fake nvidia-smi injected.

| Test | Replaces | What it tests |
|------|----------|---------------|
| `snapshot/deploy-agent` | `test_snapshot()` in `tests/e2e/run.sh` | Job completion, ConfigMap creation, snapshot document structure |

## File Structure

```
tests/chainsaw/
├── chainsaw-config.yaml                          # Global config (timeouts, parallel, reporting)
├── README.md
├── cli/
│   ├── bundle-scheduling/                        # Scheduling injection at Helm paths
│   ├── bundle-variants/                          # All bundle flag combinations
│   ├── criteria-file/                            # --criteria flag validation
│   ├── cuj1-training/                            # Full CUJ1 user journey
│   ├── duplicate-flags/                          # Duplicate flag rejection
│   ├── external-data/                            # --data flag for external registries
│   ├── recipe-generation/                        # Query-mode recipe generation
│   ├── recipe-overlays/                          # Leaf overlay smoke tests
│   ├── snapshot-template/                        # --template/--format flags
│   ├── validate-agent-flags/                     # Agent flag presence
│   └── validate-phases/                          # Multi-phase validation
└── snapshot/
    └── deploy-agent/                             # K8s Job + ConfigMap assertions
```

## Why Chainsaw?

- **Declarative YAML assertions** -- validate document structure, not just string matching
- **Partial map matching** -- specify only the fields you care about
- **K8s-native** -- apply resources, assert state, cleanup with `cleanup` blocks
- **Parallel execution** -- independent tests run concurrently
- **JUnit reporting** -- CI-friendly test output
- **Consistent with skyhook** -- same patterns used in [skyhook/k8s-tests/chainsaw](https://github.com/NVIDIA/skyhook/tree/main/k8s-tests/chainsaw)

## References

- [Kyverno Chainsaw](https://github.com/kyverno/chainsaw)
- [Chainsaw Documentation](https://kyverno.github.io/chainsaw/)
- [Skyhook Chainsaw Tests](https://github.com/NVIDIA/skyhook/tree/main/k8s-tests/chainsaw)

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
go build -o dist/e2e/aicr ./cmd/aicr
AICR_BIN=$(pwd)/dist/e2e/aicr \
  chainsaw test --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/cli/
```

Single test:

```bash
AICR_BIN=$(pwd)/dist/e2e/aicr \
  chainsaw test --no-cluster --test-dir tests/chainsaw/cli/recipe-generation
```

## CLI Tests

No cluster needed. All tests receive `AICR_BIN` and `REPO_ROOT` from the environment.

Each subdirectory under `cli/` is one suite; see the `File Structure` tree
below for the current set. Together they replace the per-function CLI checks
that used to live in `tools/e2e` and `tests/e2e/run.sh`.

## Snapshot Tests

Snapshot collection against a cluster is covered by `test_snapshot()` in `tests/e2e/run.sh`, run in the e2e CI job (cloud/Kind path).

A Talos-specific chainsaw variant lives under `snapshot/deploy-agent-talos/` and is run manually via `make talos-snapshot-test`; see `tools/talos-test/README.md` for setup.

## File Structure

```
tests/chainsaw/
├── chainsaw-config.yaml                          # Global config (timeouts, parallel, reporting)
├── README.md
├── ai-conformance/                               # CNCF AI Conformance evidence suites
├── bundle-templates/                             # Generated-bundle template assertions
├── kwok/                                         # KWOK simulated-cluster suites
├── signing/                                      # Bundle attestation: KMS, Vault, private Sigstore
├── snapshot/
│   └── deploy-agent-talos/                       # Talos snapshot Job + ConfigMap assertions
└── cli/
    ├── bundle-dynamic/
    ├── bundle-flux/
    ├── bundle-helmfile/
    ├── bundle-ocp/
    ├── bundle-scheduling/
    ├── bundle-slinky-storage/
    ├── bundle-variants/
    ├── bundle-vendor-charts/
    ├── config-file/
    ├── criteria-registry/
    ├── cuj1-training/
    ├── diff/
    ├── duplicate-flags/
    ├── evidence-publish/
    ├── external-data/
    ├── query/
    ├── recipe-generation/
    ├── recipe-overlays/
    ├── snapshot-template/
    ├── upgrade-check/
    ├── validate-agent-flags/
    ├── validate-chainsaw-healthcheck/
    └── validate-phases/
```

## References

- [Kyverno Chainsaw](https://github.com/kyverno/chainsaw) — declarative K8s YAML assertions, partial map matching, JUnit output
- [Chainsaw Documentation](https://kyverno.github.io/chainsaw/)
- [Nodewright Chainsaw Tests](https://github.com/NVIDIA/nodewright/tree/main/k8s-tests/chainsaw) — pattern reference

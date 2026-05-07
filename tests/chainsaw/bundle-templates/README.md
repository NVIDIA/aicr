# Bundle Template Tests

Template rendering tests for AICR component manifests. Verifies that Go template conditionals (`if`/`else`, `default`, `toYaml`) in manifest files produce correct output across value combinations — catching wrong defaults, broken conditionals, or typos before live deployment.

## Why These Tests Matter

Without rendering tests, template bugs only surface during live deployments. These tests run `aicr bundle` and assert on the rendered output.

## Pattern

Each component has its own subdirectory:

1. **`chainsaw-test.yaml`** — Generates a recipe, runs `aicr bundle` with various `--set` flags and scheduling options, asserts on the rendered manifest at `${WORK}/bundle/<component>/manifests/<manifest>.yaml`.
2. **`assert-*.yaml`** — Structural YAML assertions. Rendered manifests are valid K8s resources, so chainsaw parses them directly.

Pattern modeled on [nodewright helm-template-test](https://github.com/NVIDIA/nodewright/blob/main/k8s-tests/chainsaw/helm/helm-template-test/chainsaw-test.yaml), adapted for `aicr bundle` instead of `helm template`.

## Running

```bash
# Build the binary first
unset GITLAB_TOKEN && make build

# Run all bundle template tests
AICR_BIN=$(pwd)/dist/e2e/aicr chainsaw test --no-cluster --test-dir tests/chainsaw/bundle-templates/

# Run a specific component's tests
AICR_BIN=$(pwd)/dist/e2e/aicr chainsaw test --no-cluster --test-dir tests/chainsaw/bundle-templates/nodewright-customizations
```

## Adding Tests for a New Component

1. Create `tests/chainsaw/bundle-templates/<component-name>/`
2. Add a `chainsaw-test.yaml` that generates a recipe, bundles with different
   flag combinations, and asserts on the rendered output at
   `${WORK}/bundle/<component-name>/manifests/<manifest>.yaml`
3. Add `assert-*.yaml` files with the expected structural YAML

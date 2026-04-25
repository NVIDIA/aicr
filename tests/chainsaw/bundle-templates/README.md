# Bundle Template Tests

Template rendering tests for AICR component manifests. These tests verify that
Go template conditionals in manifest files produce correct output across
different value combinations.

## Why These Tests Matter

Component manifests use Go templates with conditionals (`if`/`else`, `default`,
`toYaml`) to support dynamic configuration. Without rendering tests, template
bugs (wrong defaults, broken conditionals, typos in value keys) would only
surface during live deployments.

## Pattern

Each component gets its own subdirectory with:

1. **`chainsaw-test.yaml`** — Generates a recipe, runs `aicr bundle` with
   different `--set` flags and scheduling options, then asserts on the rendered
   manifest output.
2. **`assert-*.yaml`** — Structural YAML assertions validated by
   `chainsaw assert`. Since rendered manifests are valid K8s resources
   (`apiVersion`/`kind`), chainsaw can parse them directly.

This follows the same pattern used in the
[nodewright project](https://github.com/NVIDIA/nodewright/blob/main/k8s-tests/chainsaw/helm/helm-template-test/chainsaw-test.yaml),
adapted for AICR's `aicr bundle` rendering pipeline instead of `helm template`.

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

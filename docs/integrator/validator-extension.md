# Validator Extension Guide

Learn how to add custom validators and override embedded ones using the `--data` flag.

## Overview

Validators follow the same extensibility model as components. The `--data` flag points to a directory containing custom resources that merge with (or override) the embedded ones. For validators, this means providing a `validators/catalog.yaml` in your data directory.

```
my-data/
├── validators/
│   └── catalog.yaml          # Custom/override validator entries
├── overlays/                  # Custom recipe overlays (optional)
├── components/                # Custom component values (optional)
└── registry.yaml              # Custom component registry (optional)
```

External catalog entries merge with embedded entries at load time. If an external entry has the same `name` as an embedded one, the external entry replaces it.

## Adding a Custom Validator

### Step 1: Write the Validator

A validator is any container that follows the exit code contract:

| Exit Code | Meaning |
|-----------|---------|
| `0` | Check passed |
| `1` | Check failed |
| `2` | Check skipped |

The container receives:

- Snapshot at `/data/snapshot/snapshot.yaml`
- Recipe at `/data/recipe/recipe.yaml`
- Kubernetes API access via in-cluster ServiceAccount

Evidence output goes to **stdout**. Debug logs go to **stderr**. On failure, write a reason to `/dev/termination-log` (max 4096 bytes).

### Step 2: Build and Push the Image

```shell
docker build -t my-registry.example.com/my-validator:v1.0.0 .
docker push my-registry.example.com/my-validator:v1.0.0
```

### Step 3: Create a Catalog Entry

Create `my-data/validators/catalog.yaml`:

```yaml
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: custom-validators
  version: "1.0.0"
validators:
  - name: my-custom-check
    phase: deployment
    description: "Verify my custom deployment requirement"
    image: my-registry.example.com/my-validator:v1.0.0
    timeout: 5m
    args: ["check"]
    env: []
```

### Step 4: Reference in Recipe

Add the check to your recipe's validation section:

```yaml
validation:
  deployment:
    checks:
      - operator-health        # Embedded validator
      - expected-resources     # Embedded validator
      - my-custom-check        # Your custom validator
```

If you omit the `checks` list, all catalog entries for the phase run (embedded + custom).

### Step 5: Run Validation

```shell
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml \
  --data ./my-data \
  --phase deployment
```

## Overriding Embedded Validators

To replace an embedded validator with a custom implementation, use the same `name`:

```yaml
# my-data/validators/catalog.yaml
apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: custom-validators
  version: "1.0.0"
validators:
  - name: operator-health              # Same name as embedded entry
    phase: deployment
    description: "Custom operator health check with extended diagnostics"
    image: my-registry.example.com/custom-operator-health:v1.0.0
    timeout: 5m
    args: ["check"]
    env: []
```

The external entry replaces the embedded `operator-health` validator entirely.

## Language-Agnostic Contract

The validator contract is a process convention, not a Go interface. Any language works as long as the container follows the exit code and I/O contract.

### Bash Example

```bash
#!/usr/bin/env bash
set -euo pipefail

# Read snapshot data (mounted by the validator engine)
SNAPSHOT="/data/snapshot/snapshot.yaml"

if [[ ! -f "$SNAPSHOT" ]]; then
  echo "snapshot not found" > /dev/termination-log
  exit 1
fi

# Check: verify GPU driver version from snapshot
DRIVER_VERSION=$(yq '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.name == "smi") | .data.driver_version' "$SNAPSHOT")

if [[ -z "$DRIVER_VERSION" ]]; then
  echo "GPU driver version not found in snapshot" > /dev/termination-log
  exit 1
fi

REQUIRED="550.90"

# Evidence to stdout
echo "GPU driver version: $DRIVER_VERSION"
echo "Required minimum:   $REQUIRED"

# Compare versions
if printf '%s\n%s' "$REQUIRED" "$DRIVER_VERSION" | sort -V | head -1 | grep -qx "$REQUIRED"; then
  echo "PASS: driver version meets requirement"
  exit 0
else
  MSG="FAIL: driver $DRIVER_VERSION < required $REQUIRED"
  echo "$MSG"
  echo "$MSG" > /dev/termination-log
  exit 1
fi
```

**Dockerfile:**

```dockerfile
FROM alpine:3.21
RUN apk add --no-cache bash yq
COPY check.sh /check.sh
RUN chmod +x /check.sh
ENTRYPOINT ["/check.sh"]
```

**Catalog entry:**

```yaml
- name: gpu-driver-version
  phase: deployment
  description: "Verify GPU driver meets minimum version"
  image: my-registry.example.com/gpu-driver-check:v1.0.0
  timeout: 1m
  args: []
  env: []
```

## Image Requirements

- Must run as non-root (validator Jobs use `runAsNonRoot: true`)
- Must handle the mounted data paths (`/data/snapshot/`, `/data/recipe/`)
- Should respect timeout — the Job has `activeDeadlineSeconds` set from the catalog entry
- Should write meaningful evidence to stdout for the CTRF report
- Must use explicit image tags (not `:latest`) for reproducibility in external catalogs

## Private Registries

If your validator image is in a private registry, use `--image-pull-secret`:

```shell
aicr validate \
  --recipe recipe.yaml \
  --data ./my-data \
  --image-pull-secret my-registry-secret
```

The secret must exist in the validation namespace and be of type `kubernetes.io/dockerconfigjson`.

## See Also

- [Validator Development Guide](../contributor/validator.md) — Writing upstream Go checks
- [Validator Catalog Reference](../../recipes/validators/README.md) — Catalog schema
- [CLI Reference](../user/cli-reference.md#aicr-validate) — Validate command flags
- [Data Architecture](../contributor/data.md) — External data provider system

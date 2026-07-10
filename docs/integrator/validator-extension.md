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
- Validation input at `/data/validation/validation.yaml`
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
apiVersion: validator.nvidia.com/v1alpha1
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

### Co-locating with a dependency (dependencyAffinity)

A validator catalog entry can declare `dependencyAffinity` to control where its
orchestrator Pod is scheduled relative to the components it queries. Use this
when a check's correctness depends on pod-to-pod network reachability — the
canonical case is `ai-service-metrics`, which dials Prometheus over a ClusterIP
Service.

```yaml
- name: ai-service-metrics
  phase: conformance
  image: ghcr.io/nvidia/aicr-validators/conformance:latest
  timeout: 5m
  args: ["ai-service-metrics"]
  dependencyAffinity:
    - componentRef: kube-prometheus-stack
      podLabelSelector:
        app.kubernetes.io/name: prometheus
      requirement: preferred       # or "required"; default "preferred"
      topologyKey: kubernetes.io/hostname  # default
```

**Fields:**

- `componentRef` *(required)* — the name of a component in the recipe.
  The deployer resolves it to a namespace at spawn time using the resolved
  recipe's `componentRefs`. If the named component is not in the recipe and
  `requirement: required` is set, the run fails before any Job is deployed
  with `ErrCodeInvalidRequest` — fix the recipe (add the component) or drop
  the validator from the validation phase.
- `podLabelSelector` *(required)* — labels that match the dependency's pods.
  All key/value pairs must match.
- `requirement` *(optional, default `preferred`)* — `required` emits
  `requiredDuringSchedulingIgnoredDuringExecution`; the scheduler will refuse
  to place the orchestrator anywhere else. `preferred` emits
  `preferredDuringSchedulingIgnoredDuringExecution` with weight 100; the
  scheduler treats it as the dominant scoring signal but can fall back to
  another node if the dependency is unschedulable.
- `topologyKey` *(optional, default `kubernetes.io/hostname`)* — the node
  label whose value defines co-location. The default pins to the same node;
  use `topology.kubernetes.io/zone` for zone-level locality.

When in doubt, prefer `preferred`. The high weight (100) has a strong
influence on the scheduler's scoring on the first run, after which image
locality can support (rather than oppose) the affinity. Use `required`
only when the check has no chance of succeeding without exact co-location.

See [#933](https://github.com/NVIDIA/aicr/issues/933) for the motivating case:
on multi-Security-Group EKS clusters where customer-workload and system-workload
ENIs sit in separate SGs with asymmetric ingress rules, the orchestrator's
ability to dial Prometheus depends entirely on which node the scheduler picks.

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
apiVersion: validator.nvidia.com/v1alpha1
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

# Check: verify the detected GPU SKU from the snapshot. GPU detection is
# driver-free — the accelerator SKU is resolved from the PCI device ID and
# recorded in the "hardware" subtype's "model" key.
GPU_MODEL=$(yq '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.subtype == "hardware") | .data.model' "$SNAPSHOT")

if [[ -z "$GPU_MODEL" || "$GPU_MODEL" == "null" ]]; then
  echo "GPU SKU not found in snapshot" > /dev/termination-log
  exit 1
fi

# Allowed accelerators for this workload.
ALLOWED="h100 h200 b200"

# Evidence to stdout
echo "Detected GPU SKU: $GPU_MODEL"
echo "Allowed:          $ALLOWED"

if grep -qw "$GPU_MODEL" <<<"$ALLOWED"; then
  echo "PASS: $GPU_MODEL is an allowed accelerator"
  exit 0
else
  MSG="FAIL: $GPU_MODEL is not in allowed set ($ALLOWED)"
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
- Must handle the mounted data paths (`/data/snapshot/`, `/data/validation/`)
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
- [Validator Catalog Reference](https://github.com/NVIDIA/aicr/tree/main/recipes/validators) — Catalog schema
- [CLI Reference](../user/cli-reference.md#aicr-validate) — Validate command flags
- [Data Architecture](../contributor/recipe.md) — External data provider system

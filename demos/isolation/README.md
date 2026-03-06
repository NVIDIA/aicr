# Validator Isolation Demo

Demonstrates the three-tier validation execution model on a local Kind cluster:

| Tier | Job | Image | What it proves |
|------|-----|-------|----------------|
| Shared | `aicr-{runID}-deployment` | validator image | Multiple checks combined in one Job |
| Isolated | `aicr-{runID}-deployment-expected-resources` | validator image | Same check runs alone in its own Job |
| External | `aicr-{runID}-deployment-cluster-dns-check` | `cluster-dns-check:v1` | Bring-your-own OCI container |

## Prerequisites

- Kind cluster with local registry at `localhost:5001`
- `aicr` binary built (`make build`)
- Validator image built and pushed (`make image-validator`)
- A snapshot file (any valid AICR snapshot)

## Recipe

```yaml
# demos/isolation/recipe.yaml
validation:
  deployment:
    checks:
      - expected-resources                     # Tier 1: shared (default)
      - name: expected-resources               # Tier 2: isolated override
        isolated: true
        timeout: 3m
    constraints:
      - name: Deployment.gpu-operator.version  # Tier 1: shared (default)
        value: ">= v24.6.0"
    validators:
      - name: cluster-dns-check               # Tier 3: external BYO image
        image: localhost:5001/cluster-dns-check:v1
        timeout: 2m
```

## Steps

### 1. Build the external validator image

```bash
docker build -t localhost:5001/cluster-dns-check:v1 demos/isolation/external-validator/
docker push localhost:5001/cluster-dns-check:v1
```

### 2. Build the validator image

```bash
make build
make image-validator IMAGE_REGISTRY=localhost:5001 IMAGE_TAG=local
```

### 3. Deploy a fake GPU operator

```bash
kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: gpu-operator
    app.kubernetes.io/version: v24.6.0
spec:
  replicas: 1
  selector:
    matchLabels: { app: gpu-operator }
  template:
    metadata:
      labels: { app: gpu-operator }
    spec:
      containers:
      - name: gpu-operator
        image: nvcr.io/nvidia/gpu-operator:v24.6.0
        imagePullPolicy: IfNotPresent
EOF
```

### 4. Run validation

```bash
aicr validate \
  --recipe demos/isolation/recipe.yaml \
  --snapshot snapshot.yaml \
  --image localhost:5001/aicr-validator:local \
  --phase deployment \
  --output demos/isolation/result.yaml \
  --cleanup=false \
  --validation-namespace aicr-validation
```

### 5. Inspect Jobs and labels

```bash
kubectl get jobs -n aicr-validation -o wide
kubectl get pods -n aicr-validation -o custom-columns=\
'NAME:.metadata.name,TIER:.metadata.labels.aicr\.nvidia\.com/tier,PHASE:.metadata.labels.aicr\.nvidia\.com/phase,CHECK:.metadata.labels.aicr\.nvidia\.com/check,VALIDATOR:.metadata.labels.aicr\.nvidia\.com/validator'
```

## Expected Output

### Console

```
[cli] running deployment validation phase

# --- Tier 1: Shared Job ---
[cli] built test pattern from items: pattern=^(TestGPUOperatorVersion|TestCheckExpectedResources)$ tests=2
[cli] --- BEGIN TEST OUTPUT ---
[cli]     expected_resources_check_test.go:51: ✓ Check passed: expected-resources
[cli] --- PASS: TestCheckExpectedResources (0.02s)
[cli] --- PASS: TestGPUOperatorVersion (0.00s)
[cli] PASS
[cli] --- END TEST OUTPUT ---

# --- Tier 2: Isolated Job ---
[cli] built test pattern from items: pattern=^(TestCheckExpectedResources)$ tests=1
[cli] --- BEGIN TEST OUTPUT ---
[cli]     expected_resources_check_test.go:51: ✓ Check passed: expected-resources
[cli] --- PASS: TestCheckExpectedResources (0.01s)
[cli] PASS
[cli] --- END TEST OUTPUT ---

# --- Tier 3: External Job ---
[cli] deploying external validator: name=cluster-dns-check image=localhost:5001/cluster-dns-check:v1 phase=deployment
[cli] === External Validator: Cluster DNS Check ===
[cli] Checking if kubernetes.default.svc.cluster.local resolves...
[cli] PASS: DNS resolution works
[cli] Resolved: Address: 10.96.0.1
[cli] external validator passed: name=cluster-dns-check image=localhost:5001/cluster-dns-check:v1

[cli] deployment validation completed: status=pass checks=4 duration=8.926139959s
[cli] validation completed: status=pass passed=4 failed=0 skipped=0 duration=8.926139959s
```

### Jobs

```
NAME                                                      STATUS     COMPLETIONS   DURATION   IMAGES
aicr-20260305-223332-f734-deployment                      Complete   1/1           3s         localhost:5001/aicr-validator:local
aicr-20260305-223332-f734-deployment-expected-resources   Complete   1/1           3s         localhost:5001/aicr-validator:local
aicr-20260305-223332-f734-deployment-cluster-dns-check    Complete   1/1           3s         localhost:5001/cluster-dns-check:v1
```

### Structured Pod Labels

Each pod gets structured labels for querying by tier, phase, check name, or run ID:

```
NAME                                                            TIER       PHASE        CHECK                VALIDATOR
aicr-...-deployment-xd25j                                       shared     deployment   <none>               <none>
aicr-...-deployment-expected-resources-ln7v2                     isolated   deployment   expected-resources   <none>
aicr-...-deployment-cluster-dns-check-2qkcb                     external   deployment   <none>               cluster-dns-check
```

Label queries:

```bash
# All pods for a specific run
kubectl get pods -n aicr-validation -l aicr.nvidia.com/run-id=20260305-223332-f734

# All isolated checks
kubectl get pods -n aicr-validation -l aicr.nvidia.com/tier=isolated

# All external validators
kubectl get pods -n aicr-validation -l aicr.nvidia.com/tier=external

# Specific check by name
kubectl get pods -n aicr-validation -l aicr.nvidia.com/check=expected-resources
```

### Result YAML

```yaml
summary:
  passed: 4
  failed: 0
  skipped: 0
  total: 4
  status: pass
phases:
  deployment:
    status: pass
    checks:
      - name: TestCheckExpectedResources
        status: pass
        source: shared                          # <-- Tier 1
      - name: TestGPUOperatorVersion
        status: pass
        source: shared                          # <-- Tier 1
      - name: TestCheckExpectedResources
        status: pass
        source: isolated                        # <-- Tier 2
      - name: cluster-dns-check
        status: pass
        source: external                        # <-- Tier 3
```

## Cleanup

```bash
kubectl delete jobs -l app.kubernetes.io/name=aicr -n aicr-validation
kubectl delete deployment gpu-operator -n gpu-operator
```

## Writing External Validators

External validators are OCI containers that follow a simple exit-code protocol:

| Exit Code | Meaning |
|-----------|---------|
| 0 | Pass |
| non-zero | Fail |

The framework:
- Mounts snapshot and recipe as ConfigMap volumes at `/data/snapshot/` and `/data/recipe/`
- Sets `AICR_SNAPSHOT_PATH`, `AICR_RECIPE_PATH`, `AICR_NAMESPACE` environment variables
- Captures stdout as evidence
- Reads `/dev/termination-log` (or last 10 lines of stdout) for failure reason

See `demos/isolation/external-validator/` for a minimal example.

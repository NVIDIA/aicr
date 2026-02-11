# Chainsaw E2E Tests

Example end-to-end tests using [Kyverno Chainsaw](https://github.com/kyverno/chainsaw), showing how Chainsaw's declarative YAML assertions can replace the bash grep/kubectl chains in `tests/e2e/run.sh`.

These two tests are a proof-of-concept. See the existing `tests/e2e/run.sh` for the full test suite.

## Install Chainsaw

```bash
brew install kyverno/tap/chainsaw
```

Or with Go:

```bash
go install github.com/kyverno/chainsaw@latest
```

## Tests

### 1. `snapshot/deploy-agent` — K8s Resource Assertions

Deploys the eidos snapshot agent as a Job, then asserts on the K8s resources it creates.

**Replaces**: `test_snapshot()` in `tests/e2e/run.sh` (lines 598–717)

**What it tests**:
- Job completes successfully (`assert-job-complete.yaml`)
- ConfigMap created with correct labels and format (`assert-configmap.yaml`)
- Snapshot content is a valid document with OS, SystemD, GPU, K8s measurements (`assert-snapshot-content.yaml`)

**How the current bash test does it**:
```bash
kubectl wait --for=condition=complete job/eidos-e2e-snapshot --timeout=120s
kubectl get cm eidos-e2e-snapshot -n gpu-operator
snapshot_data=$(kubectl get cm ... -o jsonpath='{.data.snapshot\.yaml}')
echo "$snapshot_data" | grep "gpu-product:" | head -1 | sed 's/.*gpu-product: //'
# ... 4 more grep+sed chains for count, memory, driver, cuda
```

**How Chainsaw does it** — `assert-snapshot-content.yaml`:
```yaml
kind: Snapshot
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  source-node: eidos-worker
  version: dev
measurements:
  - type: OS
    subtypes:
      - subtype: grub
      - subtype: sysctl
      - subtype: kmod
      - subtype: release
  - type: SystemD
    subtypes:
      - subtype: containerd.service
      - subtype: docker.service
      - subtype: kubelet.service
  - type: GPU
    subtypes:
      - subtype: smi
        data:
          gpu.model: NVIDIA B200
          driver: "560.35.03"
          gpu-count: 8
          gpu.product-architecture: Blackwell
  - type: K8s
    subtypes:
      - subtype: server
      - subtype: image
      - subtype: policy
      - subtype: node
```

Plain YAML, no bash. Chainsaw does partial map matching — it checks the fields listed and ignores everything else.

**Prerequisites**: Kind cluster, fake nvidia-smi injected, eidos image pushed to local registry.

**Run**:
```bash
# Setup (one-time)
make cluster-create
make image IMAGE_REGISTRY=localhost:5001/eidos IMAGE_TAG=local
for node in $(docker ps --filter "name=eidos-worker" --format "{{.Names}}"); do
  docker cp tools/fake-nvidia-smi "${node}:/usr/local/bin/nvidia-smi"
  docker exec "$node" chmod +x /usr/local/bin/nvidia-smi
done
kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f -

# Run test
chainsaw test --test-dir tests/chainsaw/snapshot/deploy-agent
```

---

### 2. `cli/bundle-scheduling` — CLI Output Assertions

Runs `eidos recipe` and `eidos bundle` with scheduling flags, then asserts the generated YAML files have the correct structure.

**Replaces**: `test_cli_recipe()` (lines 182–251) and `test_cli_bundle()` scheduling sub-test (lines 353–378) in `tests/e2e/run.sh`

**What it tests**:
- Recipe has correct kind, apiVersion, criteria fields (`assert-recipe.yaml`)
- GPU operator values.yaml has system node selector at `operator.nodeSelector`, accelerated selector at `daemonsets.nodeSelector`, and tolerations at `daemonsets.tolerations` (`assert-gpu-operator-values.yaml`)

**How the current bash test does it**:
```bash
grep -q "kind: recipeResult" "$basic_recipe"
for vfile in "${sched_bundle}"/*/values.yaml; do
  if grep -q "system-pool" "$vfile"; then found=true; break; fi
done
```

**How Chainsaw does it** — `assert-gpu-operator-values.yaml`:
```yaml
# System node selector: where the operator pod runs
operator:
  nodeSelector:
    nodeGroup: system-pool

# Accelerated node selector: where GPU daemonsets run
daemonsets:
  nodeSelector:
    nodeGroup: customer-gpu
  tolerations:
    - key: nvidia.com/gpu
      value: present
      effect: NoSchedule
```

The bash test only proves the string `system-pool` exists somewhere. Chainsaw proves it's at the correct Helm value path, and also validates the accelerated selector and tolerations which the bash test doesn't check at all.

> **Note**: `chainsaw assert --resource` requires `apiVersion`/`kind` to parse YAML. Helm values files don't have these natively, so the test script prepends a `apiVersion: helm/v1` / `kind: Values` header before asserting. The assertion file includes matching fields. This is a known limitation of chainsaw's `--resource` flag.

**Prerequisites**: Built eidos binary. No cluster needed.

**Run**:
```bash
go build -o dist/e2e/eidos ./cmd/eidos
EIDOS_BIN=$(pwd)/dist/e2e/eidos chainsaw test --no-cluster --test-dir tests/chainsaw/cli/bundle-scheduling
```

---

## File Structure

```
tests/chainsaw/
├── chainsaw-config.yaml                          # Global config (timeouts, parallel, reporting)
├── README.md
├── cli/
│   └── bundle-scheduling/
│       ├── chainsaw-test.yaml                    # Test orchestration
│       ├── assert-recipe.yaml                    # Recipe structure assertion
│       └── assert-gpu-operator-values.yaml       # Scheduling injection assertion
└── snapshot/
    └── deploy-agent/
        ├── chainsaw-test.yaml                    # Test orchestration
        ├── rbac.yaml                             # ServiceAccount, ClusterRole, ClusterRoleBinding
        ├── snapshot-job.yaml                     # Snapshot agent Job
        ├── assert-job-complete.yaml              # Job succeeded assertion
        ├── assert-configmap.yaml                 # ConfigMap metadata assertion
        └── assert-snapshot-content.yaml          # Snapshot document structure assertion
```

## Why Chainsaw?

- **Declarative YAML assertions** — validate document structure, not just string matching
- **Partial map matching** — specify only the fields you care about
- **K8s-native** — apply resources, assert state, cleanup with `finally` blocks
- **Parallel execution** — independent tests run concurrently
- **JUnit reporting** — CI-friendly test output
- **Consistent with skyhook** — same patterns used in [skyhook/k8s-tests/chainsaw](https://github.com/NVIDIA/skyhook/tree/main/k8s-tests/chainsaw)

## References

- [Kyverno Chainsaw](https://github.com/kyverno/chainsaw)
- [Chainsaw Documentation](https://kyverno.github.io/chainsaw/)
- [Skyhook Chainsaw Tests](https://github.com/NVIDIA/skyhook/tree/main/k8s-tests/chainsaw)

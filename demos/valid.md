# Validation

## Prerequisites

Download the latest binary and verify version:

```shell
# Homebrew (macOS/Linux)
brew tap NVIDIA/aicr
brew install aicr

# Or use the install script
curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s --
```

## Snapshot (prior to deploy)

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --output snapshot.yaml
```

Expected:
- Completes in <1m
- `snapshot.yaml` created
- No error on GPU not found
- Topology discovered (nodes, taints, labels)

## Recipe

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
  --platform kubeflow \
  --output recipe.yaml
```

Expected:
- Completes in <10s
- `recipe.yaml` created
- Criteria matching flags
- 13 components, 7 overlays

## Bundle

```shell
aicr bundle \
  --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --system-node-selector dedicated=system-workload \
  --system-node-toleration dedicated=system-workload:NoSchedule \
  --system-node-toleration dedicated=system-workload:NoExecute \
  --output bundle
```

Expected:
- Completes in <10s
- `bundle` created
- Skyhook Warning about workload selector

## Validate (dry run, no-cluster)

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --no-cluster \
    --phase deployment \
    --output dry-run.json
```

Expected:
- Instant completion
- All checks show `"status": "skipped"` with message `"skipped - no-cluster mode"`
- Exit code 0
- Valid CTRF JSON output

## Validate Deployment

```shell
aicr validate \
    --recipe recipe.yaml \
    --namespace aicr-validation \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --phase deployment \
    --output deployment-report.json \
    --debug
```

Expected:
- Recipe defines deployment checks
- Image tags equal to the CLI version (version-lock working)
- Total duration <30s (excluding nvidia-smi which depends on GPU pod lifecycle)

## Validate Conformance

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --namespace aicr-validation \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --phase conformance \
    --output conformance-report.json \
    --debug
```

Expected:
- Skipped working (e.g. `robust-controller`, Dynamo operator not installed)

## Validate Performance

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --namespace aicr-validation \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --phase performance \
    --output performance-report.json \
    --debug
```

Expected:
- NCCL tests run

## Validate All (no --phase flag)

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --namespace aicr-validation \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --output all-phases-report.json \
    --debug
```

Expected:
- All 3 phases run (deployment, performance, conformance)
- Only checks defined in the recipe are executed per phase
- Phase failure does NOT block subsequent phases in the report
- Report contains tests from all phases
- `reportFormat` is `"CTRF"`

## Validate (phase not in recipe warning)

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --no-cluster \
    --phase performance \
    --output phase-warn.json
```

Expected:
- Warning logged: `"phase requested but no checks defined in recipe; phase will be empty"`
- Phase shows `status=skipped`, 0 tests
- Exit code 0

## Verify (all reports are valid CTRF JSON)

```shell
for f in dry-run.json deployment-report.json conformance-report.json \
         performance-report.json all-phases-report.json phase-warn.json; do
  echo -n "$f: "
  if [ -f "$f" ] && jq -e '.reportFormat == "CTRF"' "$f" > /dev/null 2>&1; then
    tests=$(jq '.results.summary.tests' "$f")
    passed=$(jq '.results.summary.passed' "$f")
    failed=$(jq '.results.summary.failed' "$f")
    skipped=$(jq '.results.summary.skipped' "$f")
    echo "VALID (tests=$tests passed=$passed failed=$failed skipped=$skipped)"
  else
    echo "INVALID or missing"
  fi
done
```

## Verify (version-locked image tags)

```shell
# Check that validator images use the CLI version tag (not :latest)
VERSION=$(aicr version -s)
jq -r '.results.tests[] | select(.suite[] == "deployment") | .stdout[]' \
  deployment-report.json 2>/dev/null | grep "deploying.*image=.*:v${VERSION}" || \
  echo "Run deployment test with --debug to verify image tags"
```

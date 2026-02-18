# check-nvidia-smi

Check nvidia smi works on GPU Nodes and that means GPU nodes are configured correctly

## Files

- `check_nvidia_smi_check.go` - Check registration and validator function
- `check_nvidia_smi_check_test.go` - Integration test (runs in Kubernetes Jobs)
- `check_nvidia_smi_check_unit_test.go` - Unit test (runs locally with mocked context)
- `check_nvidia_smi_recipe.yaml` - Sample recipe for testing

## Implementation

1. Edit `check_nvidia_smi_check.go` and implement `validateCheckNvidiaSmi()`:

```go
func validateCheckNvidiaSmi(ctx *checks.ValidationContext, t *testing.T) error {
    // Your validation logic here
    // Return nil if check passes, error if it fails
    return nil
}
```

2. Unit test locally:

```bash
go test -v -short -run TestCheckCheckNvidiaSmi ./pkg/validator/checks/deployment/...
```

## Build and Run

Use a unique image tag (timestamp) to avoid caching issues:

```bash
# Generate unique tag
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}

# Build and push
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}

# Run validation
eidos validate \
  --recipe pkg/validator/checks/deployment/check_nvidia_smi_recipe.yaml \
  --snapshot cm://gpu-operator/eidos-e2e-snapshot \
  --phase deployment \
  --image ${IMAGE}
```

## Debugging

Verify the test is compiled into the image:

```bash
docker run --rm ${IMAGE} \
  go test -list ".*" ./pkg/validator/checks/deployment/... 2>/dev/null | grep -i CheckNvidiaSmi
```

Keep resources for debugging:

```bash
eidos validate \
  --recipe pkg/validator/checks/deployment/check_nvidia_smi_recipe.yaml \
  --snapshot snapshot.yaml \
  --phase deployment \
  --image ${IMAGE} \
  --cleanup=false --debug

# Inspect Job logs
kubectl logs -l eidos.nvidia.com/job -n eidos-validation

# List Jobs
kubectl get jobs -n eidos-validation
```

## Troubleshooting

**"unregistered validations" error:**

This means the check is not found in the validator image. Use a new tag:

```bash
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}
```

**"0 tests passed" or "no tests to run":**

The test function is not in the image. Verify and rebuild with a new tag:

```bash
# Verify test exists in image
docker run --rm ${IMAGE} \
  go test -list "TestCheckCheckNvidiaSmi" ./pkg/validator/checks/deployment/...

# If not found, rebuild with new tag
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}
```

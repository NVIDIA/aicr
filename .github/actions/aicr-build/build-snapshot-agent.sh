#!/usr/bin/env bash
set -euo pipefail

# Build snapshot agent image with CUDA base (provides nvidia-smi for GPU detection).
# Uses cuda:base (~250MB) instead of cuda:runtime (~1.8GB) because only nvidia-smi is needed.
docker build -t ko.local:smoke-test -f - . <<'DOCKERFILE'
FROM nvcr.io/nvidia/cuda:13.1.0-base-ubuntu24.04
COPY dist/aicr /usr/local/bin/aicr
ENTRYPOINT ["/usr/local/bin/aicr"]
DOCKERFILE

# Load onto all nodes. The snapshot agent requests nvidia.com/gpu but does not
# set a node selector, so it can land on any GPU-capable node including the
# control-plane in the L40G smoke test.
timeout 900 kind load docker-image ko.local:smoke-test --name "${KIND_CLUSTER_NAME}" || {
  echo "::warning::kind load attempt 1 failed for ko.local:smoke-test, retrying..."
  timeout 900 kind load docker-image ko.local:smoke-test --name "${KIND_CLUSTER_NAME}"
}

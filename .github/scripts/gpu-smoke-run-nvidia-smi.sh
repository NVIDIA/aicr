#!/usr/bin/env bash
set -euo pipefail

cat <<'EOF' | kubectl --context="kind-${KIND_CLUSTER_NAME}" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: gpu-smoke-test
spec:
  restartPolicy: Never
  containers:
  - name: nvidia-smi
    image: ubuntu:22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
EOF

echo "Waiting for gpu-smoke-test pod to complete..."
kubectl --context="kind-${KIND_CLUSTER_NAME}" wait pod/gpu-smoke-test \
  --for=condition=Ready --timeout=120s || true
kubectl --context="kind-${KIND_CLUSTER_NAME}" wait pod/gpu-smoke-test \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s

#!/usr/bin/env bash
set -euo pipefail

AICR_VALIDATOR_IMAGE_REGISTRY=ko.local \
./aicr validate \
  --recipe recipe.yaml \
  --phase conformance \
  --namespace gpu-operator \
  --kubeconfig="${HOME}/.kube/config" \
  --require-gpu \
  --image=ko.local:smoke-test \
  --timeout=10m \
  --toleration '*' \
  --output=validation-result.yaml \
  --evidence-dir=conformance-evidence

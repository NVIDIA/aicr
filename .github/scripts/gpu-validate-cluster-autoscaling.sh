#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Runs cluster-autoscaling alone, after the main conformance pass and after
# Karpenter + KWOK are installed. It runs by itself because validator pods
# tolerate every taint: any other check running while KWOK nodes exist could be
# scheduled onto one and never execute (#3818). Every other check the recipe
# declares is withheld with --skip-check, derived from the recipe so the list
# cannot go stale.

readonly RECIPE=bundle/recipe.yaml
readonly CHECK=cluster-autoscaling
readonly OUTPUT=validation-result-cluster-autoscaling.yaml

checks=$(yq -r '.validation.conformance.checks[]' "${RECIPE}")
if ! grep -qx "${CHECK}" <<<"${checks}"; then
  echo "::error::${RECIPE} does not declare ${CHECK}; this pass would exercise nothing"
  exit 1
fi

skip_args=()
while IFS= read -r check; do
  [[ "${check}" == "${CHECK}" ]] || skip_args+=(--skip-check "${check}")
done <<<"${checks}"

AICR_VALIDATOR_IMAGE_REGISTRY=ko.local \
./aicr validate \
  --recipe "${RECIPE}" \
  --snapshot snapshot.yaml \
  --phase conformance \
  "${skip_args[@]}" \
  --namespace gpu-operator \
  --kubeconfig="${HOME}/.kube/config" \
  --require-gpu \
  --image=ko.local:smoke-test \
  --timeout=10m \
  --toleration '*' \
  --output="${OUTPUT}"

# The phase passes when the check is skipped, so require an explicit pass:
# a missing Karpenter must fail this step, not read as green.
status=$(yq -r ".results.tests[] | select(.name == \"${CHECK}\") | .status" "${OUTPUT}")
if [[ "${status}" != "passed" ]]; then
  echo "::error::${CHECK} status is '${status:-absent}', want 'passed'"
  exit 1
fi
echo "${CHECK}: passed"

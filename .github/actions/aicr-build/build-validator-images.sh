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

VALIDATOR_PHASES="${VALIDATOR_PHASES:-}"
if [[ -n "${VALIDATOR_PHASES}" ]]; then
  if [[ "${VALIDATOR_PHASES}" == "none" ]]; then
    echo "Skipping validator builds (validator_phases=none)"
    exit 0
  fi
  PHASES="${VALIDATOR_PHASES}"
else
  # Default: build all phases (backwards compatible).
  PHASES="deployment,performance,conformance"
fi

: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"

mkdir -p dist/validator
for phase in ${PHASES//,/ }; do
  if ! [[ "${phase}" =~ ^[a-z][a-z0-9_-]*$ ]]; then
    echo "::error::invalid validator phase '${phase}'; expected ^[a-z][a-z0-9_-]*$"
    exit 1
  fi
  echo "Building validator binary: ${phase}"
  CGO_ENABLED=0 go build -trimpath -o "dist/validator/${phase}" "./validators/${phase}"
done

for phase in ${PHASES//,/ }; do
  if [[ ! -d "validators/${phase}/testdata" ]]; then
    echo "::error::validators/${phase}/testdata is missing"
    exit 1
  fi
  docker build -t "ko.local/aicr-validators/${phase}:latest" -f - . <<DOCKERFILE
FROM gcr.io/distroless/static-debian12:nonroot
COPY dist/validator/${phase} /${phase}
COPY validators/${phase}/testdata /app/testdata
WORKDIR /app
USER nonroot
ENTRYPOINT ["/${phase}"]
DOCKERFILE
  timeout 600 kind load docker-image "ko.local/aicr-validators/${phase}:latest" --name "${KIND_CLUSTER_NAME}" || {
    echo "::warning::kind load attempt 1 failed for ko.local/aicr-validators/${phase}:latest, retrying..."
    timeout 600 kind load docker-image "ko.local/aicr-validators/${phase}:latest" --name "${KIND_CLUSTER_NAME}"
  }
done

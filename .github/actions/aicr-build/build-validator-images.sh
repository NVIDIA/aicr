#!/usr/bin/env bash
set -euo pipefail

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

mkdir -p dist/validator
for phase in ${PHASES//,/ }; do
  echo "Building validator binary: ${phase}"
  CGO_ENABLED=0 go build -trimpath -o "dist/validator/${phase}" "./validators/${phase}"
done

for phase in ${PHASES//,/ }; do
  mkdir -p "validators/${phase}/testdata"
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

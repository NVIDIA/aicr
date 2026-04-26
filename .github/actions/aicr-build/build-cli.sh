#!/usr/bin/env bash
set -euo pipefail

mkdir -p dist
if [[ -x dist/aicr ]]; then
  echo "Reusing existing dist/aicr"
  exit 0
fi

CGO_ENABLED=0 go build -trimpath -o dist/aicr ./cmd/aicr

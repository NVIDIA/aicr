#!/usr/bin/env bash
# Guards against the /validators exclusion regression fixed in #1752:
# asserts the package set make test runs still includes the packages
# carrying the #1745/#1748 regression suites.
set -euo pipefail
cd "$(dirname "$0")/.."

packages=$(GOFLAGS="-mod=vendor" go list ./... | grep -v -e /tests/chainsaw/)

for pkg in \
  github.com/NVIDIA/aicr/validators/deployment \
  github.com/NVIDIA/aicr/validators/performance \
do
  if ! grep -qx "$pkg" <<<"$packages"; then
    echo "FAIL: $pkg missing from 'make test' package set"
    exit 1
  fi
  echo "PASS: $pkg present in test package set"
done
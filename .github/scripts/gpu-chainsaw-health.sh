#!/usr/bin/env bash
set -euo pipefail

test_dir="$1"

chainsaw test \
  --test-dir "${test_dir}" \
  --config tests/chainsaw/chainsaw-config.yaml \
  --cleanup-timeout 120s \
  --delete-timeout 120s

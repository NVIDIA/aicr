#!/usr/bin/env bash
# Destroy the local Talos cluster created by tools/talos-test/up.sh.
set -euo pipefail
CLUSTER_NAME="${TALOS_CLUSTER_NAME:-aicr-talos}"
talosctl cluster destroy --name "${CLUSTER_NAME}"

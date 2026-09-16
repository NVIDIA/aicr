#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Shared resolver for the pinned documentation parser toolchain. This file is
# sourced by the public gate scripts; it is not intended to be run directly.

DOCS_NODE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCS_NODE_MANIFEST="${DOCS_NODE_DIR}/package.json"
DOCS_NODE_LOCKFILE="${DOCS_NODE_DIR}/package-lock.json"
DOCS_NODE_STAMP="${DOCS_NODE_DIR}/node_modules/.aicr-lock-digest"
DOCS_NODE_INSTALL_LOCK="${DOCS_NODE_DIR}/.aicr-install-lock"

docs_node_unavailable() {
  local gate_name="$1"
  local reason="$2"

  if [[ -n "${CI:-}" || -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::error::${gate_name} cannot run: ${reason}" >&2
    exit 1
  fi

  echo "WARN: skipping ${gate_name} — ${reason}" >&2
  echo "WARN: this check is enforced in CI (merge-gate 'docs-mdx'); install Node 20+ to run it locally." >&2
  exit 0
}

# A lock timeout is broken local state, not an unavailable optional toolchain,
# so it fails everywhere instead of using the local warn-and-skip path above.
docs_node_install_lock_timeout() {
  local gate_name="$1"

  if [[ -n "${CI:-}" || -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::error::${gate_name} cannot run: timed out waiting for another docs parser install" >&2
  else
    echo "ERROR: ${gate_name} cannot run: timed out waiting for another docs parser install" >&2
  fi
  echo "Recovery: first verify no docs parser or npm ci is running; then remove the empty lock with:" >&2
  echo "  rmdir \"${DOCS_NODE_INSTALL_LOCK}\"" >&2
  exit 1
}

docs_node_toolchain_digest() {
  if command -v shasum >/dev/null 2>&1; then
    LC_ALL=C LANG=C shasum -a 256 "${DOCS_NODE_MANIFEST}" "${DOCS_NODE_LOCKFILE}" \
      | LC_ALL=C LANG=C shasum -a 256 | awk '{print $1}'
  else
    LC_ALL=C LANG=C sha256sum "${DOCS_NODE_MANIFEST}" "${DOCS_NODE_LOCKFILE}" \
      | LC_ALL=C LANG=C sha256sum | awk '{print $1}'
  fi
}

docs_node_release_install_lock() {
  rmdir "${DOCS_NODE_INSTALL_LOCK}" 2>/dev/null || true
}

ensure_docs_node_toolchain() {
  local gate_name="$1"
  local toolchain_digest
  local npm_output
  local waits=0

  command -v node >/dev/null 2>&1 || docs_node_unavailable "${gate_name}" "node not found in PATH"
  command -v npm >/dev/null 2>&1 || docs_node_unavailable "${gate_name}" "npm not found in PATH"

  if [[ ! -f "${DOCS_NODE_MANIFEST}" || ! -f "${DOCS_NODE_LOCKFILE}" ]]; then
    echo "ERROR: tools/mdx/package.json and package-lock.json are both required for the pinned docs parser toolchain" >&2
    exit 1
  fi

  toolchain_digest="$(docs_node_toolchain_digest)"
  if [[ -f "${DOCS_NODE_STAMP}" ]] && [[ "$(<"${DOCS_NODE_STAMP}")" == "${toolchain_digest}" ]]; then
    return
  fi

  # The MDX and YAML gates share one node_modules tree. A directory lock keeps
  # parallel `make -j lint` runs from launching two destructive npm ci installs.
  while ! mkdir "${DOCS_NODE_INSTALL_LOCK}" 2>/dev/null; do
    waits=$((waits + 1))
    if (( waits >= 600 )); then
      docs_node_install_lock_timeout "${gate_name}"
    fi
    sleep 0.1
  done
  trap docs_node_release_install_lock EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # A process that held the lock may have completed the install while this one
  # waited, so check the stamp again before invoking npm.
  if [[ ! -f "${DOCS_NODE_STAMP}" ]] || [[ "$(<"${DOCS_NODE_STAMP}")" != "${toolchain_digest}" ]]; then
    echo "Installing pinned docs parser toolchain from tools/mdx/package-lock.json..."
    if ! npm_output="$(cd "${DOCS_NODE_DIR}" && npm ci --ignore-scripts --no-audit --no-fund 2>&1)"; then
      printf '%s\n' "${npm_output}" >&2
      docs_node_unavailable "${gate_name}" "npm ci in tools/mdx failed (offline, or package.json and package-lock.json have drifted)"
    fi
    echo "${toolchain_digest}" >"${DOCS_NODE_STAMP}"
  fi

  docs_node_release_install_lock
  trap - EXIT INT TERM
}

#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Discover committed-but-unsigned evidence pointers, sign each, and relocate
# it to its canonical per-source path. Consumed by
# .github/workflows/evidence-publish.yaml (the fork-based signing leg).
#
# The two-phase publish flow (#1530): a contributor pushes an unsigned bundle
# and commits the pointer FLAT at recipes/evidence/<recipe>.yaml
# (`aicr ... --no-sign`). A flat path is the only committable state for an
# unsigned pointer — the nested <source> path segment derives from the signer,
# which a `--no-sign` pointer does not have. This script then:
#
#   1. signs the bundle each flat pointer references with the runner's ambient
#      OIDC token (patching the pointer's signer block in place), and
#   2. relocates the now-signed pointer to its canonical per-source path
#      recipes/evidence/<recipe>/<source>/<digest>.yaml — the layout the
#      per-source contract gate requires.
#
# Both steps run inside `aicr evidence sign --relocate`, so the SourceSlug
# derivation stays in one place (Go) rather than being reimplemented here.
# `--relocate` is idempotent: an already-signed flat pointer (a prior run
# signed it but the relocation didn't land) is moved without re-signing, so
# re-running is safe.
#
# Pointers already relocated under <recipe>/<source>/ are not scanned (the
# glob is flat), so they are never re-processed. allowlist.yaml is skipped.
#
# Required env:
#   AICR   path to the aicr binary (built from a trusted source)
#
# Any extra arguments are forwarded to `aicr evidence sign` (e.g.
# --plain-http / --insecure-tls for local-registry tests).
#
# Outputs (and, when set, appends to $GITHUB_OUTPUT):
#   signed=<n>   flat pointers signed and/or relocated this run
#   failed=<n>   flat pointers whose signing/relocation failed
#
# Exit status is non-zero when any pointer failed, so the workflow surfaces
# the cause (commonly a private fork registry returning HTTP 403 on the
# pre-sign pull — make the aicr-evidence package public).

set -euo pipefail

: "${AICR:?AICR is required (path to aicr binary)}"

extra_args=("$@")

signed=0
failed=0

shopt -s nullglob
# The evidence root is also defined in Go as verifier.EvidenceDirName
# ("recipes/evidence"); a shell script can't import the constant, so keep this
# glob in sync if that path ever changes.
for pointer in recipes/evidence/*.yaml; do
  # allowlist.yaml is not a pointer; never feed it to `evidence sign`.
  if [[ "${pointer##*/}" == "allowlist.yaml" ]]; then
    continue
  fi

  echo "signing + relocating flat pending pointer: ${pointer}"
  # --relocate: sign in place, then move to the canonical per-source path.
  # --yes: never pause for the interactive identity-disclosure prompt in CI
  # (ambient OIDC does not prompt, but this is belt-and-suspenders).
  if "$AICR" evidence sign "$pointer" --relocate --yes "${extra_args[@]}"; then
    signed=$((signed + 1))
  else
    echo "::error::failed to sign/relocate ${pointer} — is the bundle's registry package public? (a private fork package returns HTTP 403 on the pre-sign pull)"
    failed=$((failed + 1))
  fi
done

echo "signed=${signed}"
echo "failed=${failed}"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "signed=${signed}"
    echo "failed=${failed}"
  } >> "$GITHUB_OUTPUT"
fi

# Fail closed if any pointer could not be signed/relocated.
[[ "$failed" -eq 0 ]]

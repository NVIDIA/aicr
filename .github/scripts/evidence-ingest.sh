#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# GP2 evidence ingest (#1402). Verifies signed evidence bundles and
# synthesizes the source-keyed tree under OUT_DIR via the evidence-project
# helper. Two modes:
#
#   * BUNDLE_REF set — ingest one first-party bundle by ref, pinning the
#     signature to the NVIDIA/aicr UAT identity.
#   * otherwise — discover community/partner pointer files added or
#     modified in this push (recipes/evidence/*.yaml), and for each pin
#     verification to the signer the pointer claims (the verifier also
#     cross-checks the cert against that claim), classifying via the
#     in-tree allowlist when present.
#
# Holds NO bucket-write credentials: its only output is local files.
#
# Required env:
#   INGEST              path to the evidence-project binary
#   OUT_DIR             output root for the source-keyed tree
#   TRUSTED_REGISTRIES  comma-separated registry/repo prefixes (gate)
# First-party mode:
#   BUNDLE_REF          OCI ref of the bundle to ingest
#   FIRST_PARTY_ISSUER, FIRST_PARTY_IDENTITY  signature pins
#   FIRST_PARTY_RUN_ID  optional runId override (e.g. run-<gha-run-id>);
#                       when empty the runId is derived from attestedAt
# Push mode:
#   BEFORE_SHA, HEAD_SHA  push range for changed-pointer discovery

set -euo pipefail

: "${INGEST:?INGEST is required (path to evidence-project)}"
: "${OUT_DIR:?OUT_DIR is required}"
: "${TRUSTED_REGISTRIES:?TRUSTED_REGISTRIES is required}"

ALLOWLIST="recipes/evidence/allowlist.yaml"

mkdir -p "$OUT_DIR"

# escape_regexp anchors a literal identity string as a regexp so the pin
# matches exactly the claimed signer and nothing else.
escape_regexp() {
  printf '%s' "$1" | sed -e 's/[][\\.^$*+?(){}|]/\\&/g'
}

if [[ -n "${BUNDLE_REF:-}" ]]; then
  echo "first-party ingest: ${BUNDLE_REF}"
  runid_args=()
  [[ -n "${FIRST_PARTY_RUN_ID:-}" ]] && runid_args=(--run-id "$FIRST_PARTY_RUN_ID")
  "$INGEST" -in "$BUNDLE_REF" -out "$OUT_DIR" \
    --expected-issuer "$FIRST_PARTY_ISSUER" \
    --expected-identity-regexp "$FIRST_PARTY_IDENTITY" \
    --trusted-registry "$TRUSTED_REGISTRIES" \
    "${runid_args[@]}"
  echo "done"
  exit 0
fi

: "${BEFORE_SHA:?BEFORE_SHA is required in push mode}"
: "${HEAD_SHA:?HEAD_SHA is required in push mode}"

# Discover added/modified pointer files in this push. A deleted pointer
# is absent at HEAD and is skipped (the tree entry it produced is left
# in the bucket; pruning retired sources is a separate concern).
changed=$(git diff --name-only --diff-filter=AM "${BEFORE_SHA}" "${HEAD_SHA}" -- 'recipes/evidence/*.yaml' || true)

if [[ -z "$changed" ]]; then
  echo "no changed evidence pointers"
  exit 0
fi

failures=0
while IFS= read -r pf; do
  [[ -z "$pf" ]] && continue
  # The allowlist is config, not a pointer.
  [[ "$pf" == "$ALLOWLIST" ]] && continue
  [[ -f "$pf" ]] || continue

  echo "=== ${pf} ==="
  identity=$(yq eval '.attestations[0].signer.identity // ""' "$pf")
  issuer=$(yq eval '.attestations[0].signer.issuer // ""' "$pf")

  if [[ -z "$identity" || -z "$issuer" ]]; then
    echo "::warning::${pf} has no claimed signer — skipping (cannot pin verification)"
    continue
  fi

  allowlist_args=()
  [[ -f "$ALLOWLIST" ]] && allowlist_args=(--allowlist "$ALLOWLIST")

  if "$INGEST" -in "$pf" -out "$OUT_DIR" \
      --expected-issuer "$issuer" \
      --expected-identity-regexp "^$(escape_regexp "$identity")$" \
      --trusted-registry "$TRUSTED_REGISTRIES" \
      "${allowlist_args[@]}"; then
    echo "ingested ${pf}"
  else
    echo "::warning::ingest failed for ${pf} (unverified or out-of-registry)"
    failures=$((failures + 1))
  fi
done <<<"$changed"

if [[ "$failures" -gt 0 ]]; then
  echo "::error::${failures} pointer(s) failed verification — see warnings above"
  exit 1
fi
echo "done"

#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Discover committed-but-unsigned evidence pointers and sign each in place.
# Consumed by .github/workflows/evidence-publish.yaml (the fork-based
# signing leg): a contributor pushes an unsigned bundle and commits the
# pointer locally (`aicr ... --no-sign`), then this signs the bundle each
# pointer references using the runner's ambient OIDC token and patches the
# pointer's signer block in place.
#
# A pointer is "unsigned" iff its first attestation has no `signer` block —
# the state `--no-sign` produces. Already-signed pointers are skipped, so
# re-running is idempotent and safe.
#
# Required env:
#   AICR   path to the aicr binary (built from a trusted source)
#
# Requires the mikefarah `yq` (v4) — the same `yq` the rest of the repo's
# scripts assume. The kislyuk/python `yq` has incompatible syntax and is not
# supported. On GitHub-hosted runners mikefarah yq is preinstalled.
#
# Any extra arguments are forwarded to `aicr evidence sign` (e.g.
# --plain-http / --insecure-tls for local-registry tests).
#
# Outputs (and, when set, appends to $GITHUB_OUTPUT):
#   skipped=<n>  pointers already signed (no-op)
#   signed=<n>   pointers signed this run
#   failed=<n>   pointers whose signing (or parsing) failed
#
# Exit status is non-zero when any signing failed, so the workflow surfaces
# the cause (commonly a private fork registry returning HTTP 403 on the
# pre-sign pull — make the aicr-evidence package public).

set -euo pipefail

: "${AICR:?AICR is required (path to aicr binary)}"

extra_args=("$@")

signed=0
failed=0
skipped=0

shopt -s nullglob
for pointer in recipes/evidence/*.yaml; do
  # signer absent => unsigned (the --no-sign state). A signed-without-Rekor
  # pointer still has a signer block, so it is correctly skipped.
  #
  # Fail closed on a yq error: a missing yq or malformed YAML must NOT look
  # like an unsigned pointer (which would resign an already-signed bundle and
  # break rerun idempotence). Treat a parse failure as a hard failure.
  if ! signer=$(yq eval '.attestations[0].signer // "null"' "$pointer" 2>/dev/null); then
    echo "::error::failed to parse ${pointer} with yq — invalid YAML, or mikefarah yq (v4) not installed"
    failed=$((failed + 1))
    continue
  fi
  if [[ "$signer" != "null" ]]; then
    echo "skip (already signed): ${pointer}"
    skipped=$((skipped + 1))
    continue
  fi

  echo "signing unsigned pointer: ${pointer}"
  # --yes: never pause for the interactive identity-disclosure prompt in CI
  # (ambient OIDC does not prompt, but this is belt-and-suspenders).
  if "$AICR" evidence sign "$pointer" --yes "${extra_args[@]}"; then
    signed=$((signed + 1))
  else
    echo "::error::failed to sign ${pointer} — is the bundle's registry package public? (a private fork package returns HTTP 403 on the pre-sign pull)"
    failed=$((failed + 1))
  fi
done

echo "skipped=${skipped}"
echo "signed=${signed}"
echo "failed=${failed}"

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "skipped=${skipped}"
    echo "signed=${signed}"
    echo "failed=${failed}"
  } >> "$GITHUB_OUTPUT"
fi

# Fail closed if any pointer could not be signed.
[[ "$failed" -eq 0 ]]

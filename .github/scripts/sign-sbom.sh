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
#
# Attest a GoReleaser-produced binary SBOM with cosign (Rekor v2).
#
# Invoked from the `signs:` stanza of .goreleaser.yaml as:
#
#   .github/scripts/sign-sbom.sh "${artifact}" "${signature}"
#
# where ${artifact} is the SPDX document GoReleaser's `sboms:` stanza wrote
# (dist/aicr_<version>_<os>_<arch>.sbom.json) and ${signature} is the bundle
# path GoReleaser expects back (the same name plus .sigstore.json).
#
# WHY A SCRIPT AND NOT AN INLINE `args:` ENTRY. GoReleaser runs every `signs`
# argument through os.Expand before exec, mapping only its own placeholders
# (artifact, signature, certificate, artifactID). Any other `$name`,
# `${VAR:-default}` or `$((expr))` in an inline command is silently replaced
# with the empty string, which would quietly gut the retry loop and both env
# guards. Passing argv to a script keeps the shell syntax intact.
#
# The subject is the SBOM document itself and the predicate is the run's SLSA
# provenance, so the attestation asserts "NVIDIA CI run <id> produced this
# SBOM from commit <sha>": the same claim the sibling aicr/aicrd binary
# attestations make about the binaries, verified the same way:
#
#   cosign verify-blob-attestation \
#     --bundle aicr_<version>_<os>_<arch>.sbom.json.sigstore.json \
#     --type https://slsa.dev/provenance/v1 \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
#     --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.+$' \
#     aicr_<version>_<os>_<arch>.sbom.json

set -euo pipefail

readonly SBOM_PATH="${1:-}"
readonly BUNDLE_PATH="${2:-}"
readonly ATTEMPTS=3

if [[ -z "${SBOM_PATH}" || -z "${BUNDLE_PATH}" ]]; then
  echo "usage: $0 <sbom-path> <bundle-output-path>" >&2
  exit 1
fi

if [[ ! -s "${SBOM_PATH}" ]]; then
  echo "SBOM not found or empty: ${SBOM_PATH}" >&2
  exit 1
fi

# Fail closed, unlike the build post-hooks. Those may skip silently because a
# skipped hook produces nothing; GoReleaser registers the ${signature} path as
# a release artifact whether or not this script writes it, so exiting 0 without
# a bundle would leave a dangling asset that fails at upload time. Run
# `goreleaser release --skip=sbom,sign` for a deliberately unsigned local build.
if [[ -z "${SLSA_PREDICATE:-}" ]]; then
  echo "SLSA_PREDICATE unset while signing ${SBOM_PATH}; run with --skip=sbom,sign for an unsigned build" >&2
  exit 1
fi
if [[ -z "${AICR_SIGNING_CONFIG:-}" ]]; then
  echo "AICR_SIGNING_CONFIG unset while signing; refusing to fall back to Rekor v1" >&2
  exit 1
fi

# Three attempts with 5s/10s backoff (no sleep after the final failure, which
# would only delay the exit) to absorb transient Sigstore Rekor flakes.
# `cosign attest-blob` has no native --retry flag, so this mirrors the loop the
# binary-attestation post-hooks in .goreleaser.yaml already use. See #1249.
for n in $(seq 1 "${ATTEMPTS}"); do
  if cosign attest-blob \
    --predicate "${SLSA_PREDICATE}" \
    --type https://slsa.dev/provenance/v1 \
    --signing-config "${AICR_SIGNING_CONFIG}" \
    --bundle "${BUNDLE_PATH}" \
    --yes "${SBOM_PATH}"; then
    exit 0
  fi
  if [[ "${n}" -lt "${ATTEMPTS}" ]]; then
    echo "cosign attest-blob attempt ${n} failed for ${SBOM_PATH}; retrying" >&2
    sleep $((n * 5))
  fi
done

echo "cosign attest-blob failed after ${ATTEMPTS} attempts for ${SBOM_PATH}" >&2
exit 1

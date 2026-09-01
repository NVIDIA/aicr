#!/bin/bash
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

# provenance-demo.sh — interactive walkthrough of the consumer-side
# verification chain for AICR binary and image attestations:
#
#   Resolve   — tag -> immutable index digest -> platform manifest digest
#               via `crane digest` / `crane digest --platform`
#   Verify    — SLSA Provenance v1 via `gh attestation verify` (index)
#   SBOM      — CycloneDX attestation via `cosign verify-attestation` (platform)
#   VEX       — OpenVEX attestation via `cosign verify-attestation` (platform)
#   Use       — vulnerability scan + license query off the SBOM
#   Audit     — Rekor transparency-log lookup
#
# All attestations are produced upstream by NVIDIA CI on tag release
# (.github/workflows/on-tag.yaml) — there is no producer step here.
# Everything is consumer-side and anchored to a content digest, so
# the demo runs against whatever release is currently latest.
#
# Every step pauses first (unless DEMO_NO_PAUSE=1) and streams the FULL
# output of each command — nothing is suppressed or captured silently.
#
# Usage:  ./demos/provenance-demo.sh
#
# All configuration is via environment variables; every knob has a default:
#
#   IMAGE=ghcr.io/nvidia/aicr        primary image to verify (default: aicr CLI image)
#   IMAGE_AICRD=ghcr.io/nvidia/aicrd second image to verify (default: aicrd server)
#   OWNER=NVIDIA                     identity owner passed to `gh attestation verify`
#   TAG=v0.13.0                      release tag (default: resolve latest from GitHub API)
#   WORKDIR=/tmp/aicr-provenance     scratch dir (sbom.json + predicate.json land here)
#   DEMO_NO_PAUSE=1                  unattended: skip the "Press Enter" prompts
#   NO_COLOR=1                       disable ANSI color
#
# Required tools (all preflight-checked): curl, jq, crane, gh, cosign.
# Optional: grype (vulnerability-scan step is skipped if absent),
#           rekor-cli (Rekor lookup step is skipped if absent).

set -euo pipefail

# --- configuration -----------------------------------------------------------

IMAGE="${IMAGE:-ghcr.io/nvidia/aicr}"
IMAGE_AICRD="${IMAGE_AICRD:-ghcr.io/nvidia/aicrd}"
OWNER="${OWNER:-NVIDIA}"
TAG="${TAG:-}"
WORKDIR="${WORKDIR:-/tmp/aicr-provenance-demo}"

# --- presentation helpers ----------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; CYAN=$'\033[36m'; GREEN=$'\033[32m'
  YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'
else
  BOLD=''; DIM=''; CYAN=''; GREEN=''; YELLOW=''; RED=''; RESET=''
fi

STEP=0

banner() {
  STEP=$((STEP + 1))
  printf '\n%s========================================================================%s\n' "$CYAN" "$RESET"
  printf '%sSTEP %s: %s%s\n' "$BOLD" "$STEP" "$1" "$RESET"
  printf '%s========================================================================%s\n' "$CYAN" "$RESET"
}

note() { printf '%s%s%s\n' "$DIM" "$1" "$RESET"; }

pause() {
  local prompt="${1:-Press Enter to continue...}"
  if [ "${DEMO_NO_PAUSE:-0}" = "1" ]; then return 0; fi
  printf '\n%s▶ %s%s ' "$YELLOW" "$prompt" "$RESET"
  if [ -r /dev/tty ]; then read -r _ </dev/tty; else read -r _ || true; fi
}

run() {
  printf '\n%s$ %s%s\n' "$GREEN" "$*" "$RESET"
  "$@"
  local rc=$?
  printf '%s[exit %s]%s\n' "$DIM" "$rc" "$RESET"
  return "$rc"
}

# --- preflight ---------------------------------------------------------------

banner "Preflight — required tools"
note "Image:      $IMAGE"
note "Image API:  $IMAGE_AICRD"
note "Owner:      $OWNER"
note "Workdir:    $WORKDIR"

missing=()
for tool in curl jq crane gh cosign; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    missing+=("$tool")
  fi
done
if [ "${#missing[@]}" -gt 0 ]; then
  printf '%sERROR: required tools missing: %s%s\n' "$RED" "${missing[*]}" "$RESET" >&2
  printf '%sInstall via: brew install crane gh cosign jq (or distribution equivalents).%s\n' "$DIM" "$RESET" >&2
  exit 1
fi

# Optional tools — note their presence/absence so users know which steps run.
HAVE_GRYPE=0; command -v grype     >/dev/null 2>&1 && HAVE_GRYPE=1
HAVE_REKOR=0; command -v rekor-cli >/dev/null 2>&1 && HAVE_REKOR=1
note "Optional:   grype=$( ((HAVE_GRYPE)) && echo yes || echo NO )  rekor-cli=$( ((HAVE_REKOR)) && echo yes || echo NO )"

mkdir -p "$WORKDIR"
PREDICATE="$WORKDIR/predicate.json"
SBOM="$WORKDIR/sbom.json"
VEX_PREDICATE="$WORKDIR/vex-predicate.json"
VEX="$WORKDIR/openvex.json"

# --- resolve the tag ---------------------------------------------------------

banner "Resolve the latest release tag"
note "Tags are mutable — we resolve once, then never touch them again."
if [ -z "$TAG" ]; then
  pause "Press Enter to query the GitHub API for the latest release tag"
  TAG="$(curl -fsSL https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r .tag_name)"
  note "Latest release tag: $TAG"
fi
note "Using tag: $TAG"

banner "Tag → immutable digest"
note "Every following check is anchored to the digest. The tag never appears again."
pause "Press Enter to resolve the digest"
DIGEST="$(crane digest "$IMAGE:$TAG")"
note "Resolved digest: $DIGEST"
IMAGE_DIGEST="$IMAGE@$DIGEST"
note "Pinned ref: $IMAGE_DIGEST"

note "The tag resolves to a multi-platform index. Provenance describes the whole"
note "build and lives there; the SBOM and the VEX each describe one root filesystem,"
note "so they live on the platform's own child manifest. Resolve it from the pinned"
note "index, never from the tag."
PLATFORM="linux/$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
DIGEST_PLATFORM="$(crane digest --platform "$PLATFORM" "$IMAGE_DIGEST")"
IMAGE_PLATFORM="$IMAGE@$DIGEST_PLATFORM"
note "Pinned $PLATFORM manifest: $IMAGE_PLATFORM"

# --- verify the image (SLSA provenance) --------------------------------------

banner "Verify the image: SLSA Provenance v1 (via gh)"
note "Requires GitHub auth: run 'gh auth login' or set GH_TOKEN first."
note "gh attestation verify fetches the attestation from the GitHub attestations API, validates the Sigstore"
note "signature against Fulcio's cert chain + Rekor's inclusion proof, and asserts the"
note "artifact comes from --repo $OWNER/aicr and was signed by the attest-images.yaml reusable workflow (the SLSA Build L3 trusted builder)."
pause "Press Enter to verify provenance"
run gh attestation verify "oci://$IMAGE_DIGEST" --repo "$OWNER/aicr" --signer-workflow "$OWNER/aicr/.github/workflows/attest-images.yaml" --source-ref "refs/tags/$TAG"

banner "Verify the aicrd server image"
note "Same flow, second subject. Resolve its own digest first — tags can drift independently."
pause "Press Enter to resolve + verify aicrd"
DIGEST_AICRD="$(crane digest "$IMAGE_AICRD:$TAG")"
note "aicrd digest: $DIGEST_AICRD"
run gh attestation verify "oci://$IMAGE_AICRD@$DIGEST_AICRD" --repo "$OWNER/aicr" --signer-workflow "$OWNER/aicr/.github/workflows/attest-images.yaml" --source-ref "refs/tags/$TAG"

# --- verify the SBOM attestation ---------------------------------------------

banner "Verify the CycloneDX SBOM attestation (via cosign)"
note "cosign verify-attestation enforces the OIDC issuer + identity pattern and writes"
note "the verified DSSE envelope to disk. Identity is pinned to NVIDIA/aicr CI workflows."
note "Subject is the platform manifest, not the index; asking the index for this"
note "predicate type returns nothing."
pause "Press Enter to verify the SBOM attestation"
run cosign verify-attestation \
  --type cyclonedx \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  "$IMAGE_PLATFORM" \
  --output-file "$PREDICATE"

note "Extracting the CycloneDX predicate (the actual SBOM JSON) from the DSSE payload."
pause "Press Enter to extract sbom.json"
# The cosign --output-file writes a DSSE envelope; the inner predicate is the SBOM.
# `set -o pipefail` inside the child: a child bash -c does not inherit it, and
# without it a failed jq or base64 mid-pipeline still exits 0, leaving an empty
# sbom.json that the scan below would happily report as clean.
run bash -c "set -o pipefail; jq -r .payload '$PREDICATE' | base64 -d | jq .predicate > '$SBOM' && jq '.bomFormat, .specVersion, (.components|length)' '$SBOM'"

# --- verify the VEX attestation ----------------------------------------------

banner "Verify the OpenVEX attestation (same subject, different predicate type)"
note "The SBOM says what is in the image. The VEX records AICR's triage verdict"
note "for each CVE it covers, and why. Both hang off the same platform manifest; they are"
note "in different formats so a referrers listing can tell them apart without"
note "downloading either one."
pause "Press Enter to verify the VEX attestation"
run cosign verify-attestation \
  --type openvex \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  "$IMAGE_PLATFORM" \
  --output-file "$VEX_PREDICATE"

note "Each statement's product purl carries the platform digest, so the claim binds"
note "to this exact manifest. An empty statements array is a valid answer, not a"
note "failure: it means AICR has asserted no exceptions for this image."
pause "Press Enter to extract openvex.json"
# Report what the document actually says rather than predicting it. The count
# and the status set are properties of the release being verified, and the
# format carries affected / fixed / under_investigation as well as not_affected.
run bash -c "set -o pipefail; jq -r .payload '$VEX_PREDICATE' | base64 -d | jq .predicate > '$VEX' && jq '{statements: (.statements|length), statuses: ([.statements[].status] | unique), products: ([.statements[].products[][\"@id\"]] | unique)}' '$VEX'"

# --- SBOM use cases ----------------------------------------------------------

banner "SBOM use case #1: vulnerability scan"
if [ "$HAVE_GRYPE" = "1" ]; then
  note "Grype reads CycloneDX directly. No re-pull, no re-derive — the SBOM IS the input."
  note "Passing --vex applies the triage above, so already-analyzed CVEs stay quiet."
  pause "Press Enter to run grype against the SBOM"
  run grype "sbom:$SBOM" --vex "$VEX"
else
  note "grype not installed — skipping. (Install: brew install grype)"
  note "The command would be: grype sbom:$SBOM --vex $VEX"
fi

banner "SBOM use case #2: license compliance"
note "Every package's declared license, one jq filter away."
pause "Press Enter to list declared licenses (first 10)"
run bash -c "jq -r '.components[]
  | select(.licenses != null)
  | \"\(.name) \(.version) \(.licenses[0].license.id // .licenses[0].license.name)\"' '$SBOM' | head -10"

# --- Rekor audit lookup ------------------------------------------------------

banner "Audit trail: Rekor transparency log"
if [ "$HAVE_REKOR" = "1" ]; then
  note "Rekor is the public, append-only log of every Sigstore signature ever made."
  note "Searching by the image content digest finds every attestation made against it."
  pause "Press Enter to search Rekor"
  # rekor-cli wants the sha256 hex without the algorithm prefix.
  run rekor-cli search --sha "${DIGEST#sha256:}"
else
  note "rekor-cli not installed — skipping. (Install: go install github.com/sigstore/rekor/cmd/rekor-cli@latest)"
  note "Search URL: https://search.sigstore.dev/?hash=${DIGEST#sha256:}"
fi

# --- done --------------------------------------------------------------------

banner "Done"
printf '%sSix verifications, one trust model.%s\n' "$GREEN" "$RESET"
note "Pinned image:  $IMAGE_DIGEST"
note "Pinned $PLATFORM: $IMAGE_PLATFORM"
note "Pinned aicrd:  $IMAGE_AICRD@$DIGEST_AICRD"
note "SBOM:          $SBOM"
note "VEX:           $VEX"
note "DSSE payload:  $PREDICATE"
note ""
note "Next: enforce these at admission time — see demos/provenance.md §'In-cluster verification'."
note "Related: demos/bundle-attestation.md (bundles) · demos/evidence.md (recipe evidence)."

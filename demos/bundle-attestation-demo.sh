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

# bundle-attestation-demo.sh — interactive walkthrough of bundle attestation:
#
#   Produce — aicr bundle --attest   (Sigstore keyless OIDC)
#   Inspect — bundle layout + attestation/*.sigstore.json
#   Verify  — aicr verify (with default + min-trust-level + tamper paths)
#
# When --attest is passed, the CLI signs `checksums.txt` with SLSA Build
# Provenance v1 via Sigstore keyless OIDC (Fulcio cert + Rekor log entry),
# and copies the binary's own SLSA attestation into the bundle so the
# verifier can walk the full chain back to NVIDIA CI.
#
# Signing requires Fulcio + Rekor egress and an OIDC source — either ambient
# GitHub Actions OIDC, or an interactive browser sign-in. On hosts where
# neither is available (offline, corp VPN blocking Sigstore, no display),
# set SKIP_ATTEST=1 to reuse a bundle that was pre-staged ahead of time.
#
# Every step pauses first (unless DEMO_NO_PAUSE=1) and streams the FULL
# output of each aicr command — nothing is suppressed or captured silently.
#
# Usage:  ./demos/bundle-attestation-demo.sh
#
# All configuration is via environment variables; every knob has a default,
# so a future demo overrides only what differs for its recipe:
#
#   AICR=/path/to/aicr        aicr binary (default: aicr on PATH)
#   WORKDIR=/tmp/aicr-bundle  scratch dir (recipe + bundle land here)
#   BUNDLE=$WORKDIR/my-bundle bundle output directory
#
#   # recipe criteria — what to bundle. REQUIRED: SERVICE ACCELERATOR OS INTENT.
#   # PLATFORM optional (omitted when empty). e.g. eks h100 ubuntu training
#   SERVICE=eks  ACCELERATOR=h100  OS=ubuntu  INTENT=training  PLATFORM=
#
#   # policy enforcement step (the "Enforce" leg of the demo)
#   MIN_TRUST=verified        floor for `aicr verify --min-trust-level`
#
#   # demo behavior
#   SKIP_ATTEST=1             reuse the pre-staged bundle at $BUNDLE; skip bundle --attest
#   DEMO_NO_PAUSE=1           unattended: skip all the "Press Enter" prompts
#   NO_COLOR=1                disable ANSI color

set -euo pipefail

# --- configuration -----------------------------------------------------------

AICR="${AICR:-aicr}"
WORKDIR="${WORKDIR:-/tmp/aicr-bundle-attestation-demo}"
BUNDLE="${BUNDLE:-$WORKDIR/my-bundle}"
RECIPE="$WORKDIR/recipe.yaml"

# Recipe criteria. REQUIRED for the producer leg; not validated until the
# producer branch so SKIP_ATTEST=1 runs don't demand them.
SERVICE="${SERVICE:-}"
ACCELERATOR="${ACCELERATOR:-}"
OS="${OS:-}"
INTENT="${INTENT:-}"
PLATFORM="${PLATFORM:-}"

MIN_TRUST="${MIN_TRUST:-verified}"
SKIP_ATTEST="${SKIP_ATTEST:-0}"

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

run_expect_fail() {
  printf '\n%s$ %s%s\n' "$GREEN" "$*" "$RESET"
  local rc=0
  "$@" || rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s[exit %s — expected failure ✓]%s\n' "$GREEN" "$rc" "$RESET"
    return 0
  else
    printf '%s[exit 0 — UNEXPECTED: command was supposed to fail]%s\n' "$RED" "$RESET"
    return 1
  fi
}

# --- preflight ---------------------------------------------------------------

banner "Preflight — environment & aicr version"
note "Binary:        $AICR"
note "Workdir:       $WORKDIR"
note "Bundle out:    $BUNDLE"
note "Criteria:      service=$SERVICE accelerator=$ACCELERATOR os=$OS intent=$INTENT platform=$PLATFORM"
note "Min trust:     $MIN_TRUST"
note "SKIP_ATTEST:   $SKIP_ATTEST  (set to 1 to reuse the pre-staged $BUNDLE)"
note ""
note "Note: if a verify step reports a Sigstore trusted-root error, run"
note "      'aicr trust update' once to bootstrap the root, then re-run."

if ! command -v "$AICR" >/dev/null 2>&1 && [ ! -x "$AICR" ]; then
  printf '%sERROR: aicr binary not found at %q. Set AICR=/path/to/aicr.%s\n' "$RED" "$AICR" "$RESET" >&2
  exit 1
fi

mkdir -p "$WORKDIR"
run "$AICR" --version
pause

# --- leg 1: recipe + attested bundle -----------------------------------------
# Skipped entirely when SKIP_ATTEST=1 (reuse the pre-staged bundle at $BUNDLE).

if [ "$SKIP_ATTEST" = "1" ]; then
  banner "Reuse pre-staged attested bundle (SKIP_ATTEST=1)"
  note "Skipping recipe + aicr bundle --attest; verifying the bundle already at $BUNDLE."
  if [ ! -f "$BUNDLE/checksums.txt" ] || [ ! -f "$BUNDLE/attestation/bundle-attestation.sigstore.json" ]; then
    printf '%sERROR: SKIP_ATTEST=1 but no attested bundle at %s — run once without it first.%s\n' "$RED" "$BUNDLE" "$RESET" >&2
    exit 1
  fi
  pause
else

banner "Generate the recipe"
note "Resolves criteria into a canonical recipe; no signing or network."
for _c in SERVICE ACCELERATOR OS INTENT; do
  if [ -z "${!_c}" ]; then
    printf '%sERROR: %s is required — set SERVICE, ACCELERATOR, OS, INTENT (and PLATFORM if the recipe has one).%s\n' "$RED" "$_c" "$RESET" >&2
    exit 1
  fi
done
recipe_args=(--service "$SERVICE" --accelerator "$ACCELERATOR" --os "$OS" --intent "$INTENT" --output "$RECIPE")
[ -n "$PLATFORM" ] && recipe_args+=(--platform "$PLATFORM")
pause "Press Enter to generate the recipe"
run "$AICR" recipe "${recipe_args[@]}"

banner "Refresh the Sigstore trusted root (optional)"
note "aicr verify falls back to the embedded root; this only refreshes a stale cache."
if [ -n "${AICR_TRUST_UPDATE:-}" ]; then
  pause "Press Enter to run aicr trust update"
  run "$AICR" trust update
else
  note "Skipped — set AICR_TRUST_UPDATE=1 to refresh (avoids aborting offline under set -e)."
fi

banner "Create the attested bundle (LIVE OIDC SIGN)"
printf '%s%s' "$YELLOW" "$BOLD"
cat <<'EOF'
  ┌────────────────────────────────────────────────────────────────────┐
  │  KEYLESS SIGNING NOW.                                                  │
  │  If running locally a browser will open for OIDC sign-in.            │
  │  In GitHub Actions the ambient OIDC token is detected automatically. │
  │  Either way, Fulcio + Rekor must be reachable.                       │
  │                                                                      │
  │  If signing is not possible from this host, exit with Ctrl-C, set    │
  │  SKIP_ATTEST=1 (after pre-staging $BUNDLE), and re-run.              │
  └────────────────────────────────────────────────────────────────────┘
EOF
printf '%s' "$RESET"
rm -rf "$BUNDLE"
pause "Press Enter to run aicr bundle --attest"
run "$AICR" bundle --recipe "$RECIPE" --output "$BUNDLE" --attest

fi   # end SKIP_ATTEST guard

# --- inspect the bundle -------------------------------------------------------

banner "Inspect the bundle layout"
note "Content files + checksums.txt + attestation/ holding the two signed predicates."
run ls -R "$BUNDLE"

banner "Inspect the bundle attestation predicate"
note "The signed envelope is a Sigstore Bundle (DSSE + Fulcio cert + Rekor inclusion proof)."
note "Showing the in-toto subject + predicate type — the actual claim being signed."
pause "Press Enter to inspect bundle-attestation.sigstore.json"
run bash -c "jq '{ subject_count: (.dsseEnvelope.payload | @base64d | fromjson | .subject | length),
                   predicate_type: (.dsseEnvelope.payload | @base64d | fromjson | .predicateType) }' '$BUNDLE/attestation/bundle-attestation.sigstore.json'"

# --- verify -------------------------------------------------------------------

banner "Verify — default (auto-detect maximum trust level)"
note "Five gates: checksums → bundle signature → bundle predicate → binary attestation chain → identity pin."
note "The reported trust level is the highest level every gate passed at."
pause "Press Enter to run aicr verify (default)"
run "$AICR" verify "$BUNDLE"

banner "Verify — policy floor ($MIN_TRUST)"
note "Same checks, but fail if the resulting trust level is below the floor."
note "Use 'verified' for production gates; 'attested' for pre-prod."
pause "Press Enter to run aicr verify --min-trust-level $MIN_TRUST"
run "$AICR" verify "$BUNDLE" --min-trust-level "$MIN_TRUST"

banner "Verify — JSON output (CI path)"
note "Structured output for pipeline branching: .trustLevel + .bundleCreator + .toolVersion."
pause "Press Enter to run aicr verify --format json"
run bash -c "set -o pipefail; '$AICR' verify '$BUNDLE' --format json | jq '{ trustLevel, bundleCreator, toolVersion }'"

# --- tamper -------------------------------------------------------------------

banner "Tamper-evident: mutate a content file, verify fails"
note "The signature's subject is the digest of checksums.txt, which pins every generated payload file, including recipe.yaml."
note "Mutating any file listed in checksums.txt breaks its checksum; mutating checksums.txt breaks the signature."
# Pick a file the bundle is guaranteed to contain. README.md is always present.
TAMPER_TARGET="$BUNDLE/README.md"
if [ ! -f "$TAMPER_TARGET" ]; then
  # Fallback to any text file under the bundle.
  TAMPER_TARGET="$(find "$BUNDLE" -maxdepth 2 -type f -name '*.md' ! -path "$BUNDLE/attestation/*" | head -n1)"
fi
note "Target: $TAMPER_TARGET"
pause "Press Enter to corrupt the file and re-verify"
run bash -c "printf '\n# tampered demonstration\n' >> '$TAMPER_TARGET'"
run_expect_fail "$AICR" verify "$BUNDLE" --min-trust-level "$MIN_TRUST"

# --- done --------------------------------------------------------------------

banner "Done"
printf '%sBundle signed, verified, tamper checked.%s\n' "$GREEN" "$RESET"
note "Bundle:    $BUNDLE"
note "Attestation:"
note "  $BUNDLE/attestation/bundle-attestation.sigstore.json  (SLSA Provenance v1 — bundle subject)"
note "  $BUNDLE/attestation/aicr-attestation.sigstore.json    (binary attestation, copied in)"
note ""
note "Next: enforce --min-trust-level in CI before kubectl apply."
note "Related: demos/provenance.md (binary/image) · demos/evidence.md (recipe evidence)."

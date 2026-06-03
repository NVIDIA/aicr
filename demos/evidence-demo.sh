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

# evidence-demo.sh — interactive walkthrough of the split-leg
# recipe-evidence workflow:
#
#   Leg 1 (ON VPN, cluster-bound):   aicr validate --emit-attestation   (no --push)
#   Leg 2 (OFF VPN, Sigstore-bound): aicr evidence publish --push <ref>
#   Verify:                          aicr evidence verify <pointer>
#
# The two legs are decoupled so they can run on different networks: validate
# where the cluster is reachable (often a corporate VPN), then publish from a
# host with Fulcio/Rekor egress. See demos/evidence.md and ADR-007.
#
# Every step pauses first (unless DEMO_NO_PAUSE=1) and streams the FULL output
# of each aicr command — nothing is suppressed or captured silently.
#
# Usage:  ./demos/evidence-demo.sh
#
# All configuration is via environment variables; every knob has a default, so
# a future demo overrides only what differs for its cluster/recipe:
#
#   # which binary / where
#   AICR=/path/to/aicr                        aicr binary (default: aicr on PATH)
#   KUBECONFIG=/path/to/kubeconfig            cluster for snapshot + validate
#   WORKDIR=/tmp/aicr-evidence-demo           scratch dir (recipe/snapshot/bundle)
#
#   # recipe criteria — what to attest. REQUIRED: SERVICE ACCELERATOR OS INTENT.
#   # PLATFORM is optional (omitted when empty). e.g. eks gb200 ubuntu inference dynamo
#   SERVICE=eks  ACCELERATOR=gb200  OS=ubuntu  INTENT=inference  PLATFORM=dynamo
#
#   # snapshot node targeting (heterogeneous clusters)
#   NODE_SELECTOR=nvidia.com/gpu.present=true pin the snapshot Job to a GPU node
#                                             (so nvidia-smi data is collected; "" = any node)
#   RUNTIME_CLASS=nvidia                      nvidia-smi access without consuming a GPU ("" = none)
#
#   # where to publish
#   PUSH_REF=ttl.sh/aicr-evidence-<uuid>:72h  OCI ref (default: fresh anonymous ttl.sh, 72h TTL)
#
#   # demo behavior
#   AICR_VALIDATOR_IMAGE_TAG=edge   validator image tag for dev aicr builds
#   SKIP_VALIDATE=1                 reuse the bundle at $WORKDIR/out; skip recipe + snapshot + validate
#   DEMO_NO_PAUSE=1                 unattended: skip all the "Press Enter" prompts
#   NO_COLOR=1                      disable ANSI color

set -euo pipefail

# --- configuration -----------------------------------------------------------

AICR="${AICR:-aicr}"
WORKDIR="${WORKDIR:-/tmp/aicr-evidence-demo}"

# Recipe criteria: the (service, accelerator, os, intent) tuple the evidence
# attests — REQUIRED, no defaults, so a demo never silently attests the wrong
# recipe. PLATFORM is optional (omitted when empty; not every recipe has one).
SERVICE="${SERVICE:-}"
ACCELERATOR="${ACCELERATOR:-}"
OS="${OS:-}"
INTENT="${INTENT:-}"
PLATFORM="${PLATFORM:-}"

# Snapshot node targeting. On a mixed CPU+GPU cluster the snapshot Job must land
# on a GPU node, or nvidia-smi finds nothing and the accelerator fingerprint is
# empty (and won't match the recipe). Defaults pin to a GPU-Operator-labeled node
# and grant nvidia-smi access without consuming a GPU. Set either to "" to
# disable (homogeneous GPU cluster, or a CPU-only recipe).
NODE_SELECTOR="${NODE_SELECTOR:-nvidia.com/gpu.present=true}"
RUNTIME_CLASS="${RUNTIME_CLASS:-nvidia}"

# Reuse the bundle already at $WORKDIR/out and skip the slow recipe + snapshot +
# validate legs — for demoing publish + verify live after pre-staging validate.
SKIP_VALIDATE="${SKIP_VALIDATE:-0}"

# A fresh UUID keeps the public ttl.sh namespace collision-free; on ttl.sh the
# tag IS the TTL, so :72h auto-expires the artifact (and its signature) in 72h.
new_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
  elif [ -r /proc/sys/kernel/random/uuid ]; then
    cat /proc/sys/kernel/random/uuid
  else
    python3 -c 'import uuid; print(uuid.uuid4())'
  fi
}
PUSH_REF="${PUSH_REF:-ttl.sh/aicr-evidence-$(new_uuid):72h}"

# --- presentation helpers -----------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; CYAN=$'\033[36m'; GREEN=$'\033[32m'
  YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'
else
  BOLD=''; DIM=''; CYAN=''; GREEN=''; YELLOW=''; RED=''; RESET=''
fi

STEP=0

# banner <title> — prints a numbered section header.
banner() {
  STEP=$((STEP + 1))
  printf '\n%s========================================================================%s\n' "$CYAN" "$RESET"
  printf '%sSTEP %s: %s%s\n' "$BOLD" "$STEP" "$1" "$RESET"
  printf '%s========================================================================%s\n' "$CYAN" "$RESET"
}

# note <text> — prints an explanatory line.
note() { printf '%s%s%s\n' "$DIM" "$1" "$RESET"; }

# pause [prompt] — waits for Enter. Reads from the terminal directly so it works
# even when the script's stdin is piped. Honors DEMO_NO_PAUSE=1 for unattended runs.
pause() {
  local prompt="${1:-Press Enter to continue...}"
  if [ "${DEMO_NO_PAUSE:-0}" = "1" ]; then return 0; fi
  printf '\n%s▶ %s%s ' "$YELLOW" "$prompt" "$RESET"
  if [ -r /dev/tty ]; then read -r _ </dev/tty; else read -r _ || true; fi
}

# run <cmd...> — echoes the command, then runs it with output streaming straight
# to the terminal (never suppressed, never silently captured). Aborts on failure.
run() {
  printf '\n%s$ %s%s\n' "$GREEN" "$*" "$RESET"
  "$@"
  local rc=$?
  printf '%s[exit %s]%s\n' "$DIM" "$rc" "$RESET"
  return "$rc"
}

# run_expect_fail <cmd...> — like run(), but a NON-zero exit is the expected,
# successful outcome (e.g. demoing the verifier's safety refusals). Output is
# still streamed in full. Kept as a reusable building block for future beats.
run_expect_fail() {
  printf '\n%s$ %s%s\n' "$GREEN" "$*" "$RESET"
  local rc=0
  "$@" || rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s[exit %s — expected failure ✓]%s\n' "$GREEN" "$rc" "$RESET"
  else
    printf '%s[exit 0 — UNEXPECTED: command was supposed to fail]%s\n' "$RED" "$RESET"
  fi
}

# --- preflight ----------------------------------------------------------------

banner "Preflight — environment & aicr version"
note "Binary:        $AICR"
note "Workdir:       $WORKDIR"
note "Push ref:      $PUSH_REF"
note "Criteria:      service=$SERVICE accelerator=$ACCELERATOR os=$OS intent=$INTENT platform=$PLATFORM"
note "Snapshot node: node-selector=${NODE_SELECTOR:-<any>}  runtime-class=${RUNTIME_CLASS:-<none>}"
note "Kubeconfig:    ${KUBECONFIG:-<from default ~/.kube/config>}"
note "Note: if a verify step reports a Sigstore trusted-root error, run"
note "      'aicr trust update' once (OFF VPN) to bootstrap the root, then re-run."

if ! command -v "$AICR" >/dev/null 2>&1 && [ ! -x "$AICR" ]; then
  printf '%sERROR: aicr binary not found at %q. Set AICR=/path/to/aicr.%s\n' "$RED" "$AICR" "$RESET" >&2
  exit 1
fi

mkdir -p "$WORKDIR"
RECIPE="$WORKDIR/recipe.yaml"
SNAPSHOT="$WORKDIR/snapshot.yaml"
OUT="$WORKDIR/out"

run "$AICR" --version
pause

# --- leg 1: generate inputs + validate (ON VPN) ------------------------------
# Skipped entirely when SKIP_VALIDATE=1 (reuse the pre-staged bundle at $OUT).

if [ "$SKIP_VALIDATE" = "1" ]; then
  banner "Reuse pre-staged bundle (SKIP_VALIDATE=1)"
  note "Skipping recipe + snapshot + validate; publishing the bundle already at $OUT."
  if [ ! -d "$OUT/summary-bundle" ]; then
    printf '%sERROR: SKIP_VALIDATE=1 but no bundle at %s — run once without it first.%s\n' "$RED" "$OUT/summary-bundle" "$RESET" >&2
    exit 1
  fi
  pause
else

banner "Generate the recipe from criteria"
note "Produces the recipe whose evidence we will sign. No cluster access yet."
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

banner "Snapshot the live cluster (ON VPN)"
note "Captures cluster state the validators run against. Needs the cluster reachable."
note "Targets a GPU node so nvidia-smi data is collected (NODE_SELECTOR / RUNTIME_CLASS)."
# Build snapshot args so node targeting is opt-out: empty NODE_SELECTOR /
# RUNTIME_CLASS simply omit the flag (homogeneous or CPU-only clusters).
snapshot_args=(--output "$SNAPSHOT")
[ -n "$NODE_SELECTOR" ] && snapshot_args+=(--node-selector "$NODE_SELECTOR")
[ -n "$RUNTIME_CLASS" ] && snapshot_args+=(--runtime-class "$RUNTIME_CLASS")
pause "Press Enter to snapshot the cluster"
run "$AICR" snapshot "${snapshot_args[@]}"

banner "LEG 1 (ON VPN): validate + emit attestation, WITHOUT --push"
note "Runs the validators against the cluster and writes an UNSIGNED bundle to disk."
note "No Fulcio/Rekor contact happens here — this leg only needs the cluster."
rm -rf "$OUT"
pause "Confirm you are ON VPN (cluster reachable), then press Enter to validate"
run "$AICR" validate \
  --recipe "$RECIPE" \
  --snapshot "$SNAPSHOT" \
  --emit-attestation "$OUT" \
  --fail-on-error=false
fi   # end SKIP_VALIDATE guard

banner "Inspect the unsigned bundle on disk"
note "The summary bundle + a pointer.yaml (its oci/digest stay empty until we sign)."
run ls -R "$OUT"
printf '\n%s--- pointer.yaml (pre-publish) ---%s\n' "$DIM" "$RESET"
cat "$OUT/pointer.yaml"
pause

# --- leg 2: sign + push (OFF VPN) ---------------------------------------------

banner "LEG 2 (OFF VPN): sign, push, and write the pointer"
printf '%s%s' "$YELLOW" "$BOLD"
cat <<'EOF'
  ┌────────────────────────────────────────────────────────────────────┐
  │  SWITCH OFF VPN NOW.                                                  │
  │  This leg contacts Fulcio (signing cert) and Rekor (transparency     │
  │  log), which are commonly blocked on corporate VPNs. A browser may   │
  │  open for keyless OIDC authentication — complete it when it does.    │
  └────────────────────────────────────────────────────────────────────┘
EOF
printf '%s' "$RESET"
note "The predicate (incl. its baked-in attestedAt) is signed VERBATIM from the"
note "on-disk bundle, so the result is identical regardless of which host signs."
pause "Once you are OFF VPN, press Enter to sign + push"
run "$AICR" evidence publish "$OUT" --push "$PUSH_REF"

banner "Inspect the pointer after publish"
note "oci, digest, signer identity, and the Rekor log index are now populated."
printf '\n%s--- pointer.yaml (post-publish) ---%s\n' "$DIM" "$RESET"
cat "$OUT/pointer.yaml"
pause

# --- verify -------------------------------------------------------------------

banner "Verify from the pointer (maintainer path)"
note "The pointer is the only input: it carries bundle.oci + digest, so the verifier"
note "pulls the artifact BY DIGEST, then checks signature → DSSE predicate → manifest chain."
pause "Press Enter to verify from pointer.yaml"
run "$AICR" evidence verify "$OUT/pointer.yaml"

# --- done ---------------------------------------------------------------------

banner "Done"
printf '%sBundle published and verified.%s\n' "$GREEN" "$RESET"
note "OCI artifact:  $PUSH_REF  (ttl.sh default TTL: 72h)"
note "Bundle digest: $(awk '/digest:/ {print $2; exit}' "$OUT/pointer.yaml")"
note "Pointer:       $OUT/pointer.yaml"
note "Rekor entry is permanent even after the ttl.sh artifact expires:"
REKOR_INDEX="$(awk '/rekorLogIndex:/ {print $2; exit}' "$OUT/pointer.yaml" 2>/dev/null || true)"
if [ -n "${REKOR_INDEX:-}" ]; then
  note "  https://search.sigstore.dev/?logIndex=${REKOR_INDEX}"
fi

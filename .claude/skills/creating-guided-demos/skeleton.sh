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

# <name>.sh — guided walkthrough of <thing> (live demo or self-paced).  Frame → Tell → Show → Close.
#
# Copy this skeleton and fill in the four beats below. See the
# creating-guided-demos skill and demos/evidence-demo.sh for the full pattern.
#
# Usage:  ./demos/<name>.sh
#
# Config (env vars; required ones fail fast, the rest default):
#   BIN=/path/to/mycli     the binary the demo drives (default: mycli on PATH)
#   REQUIRED_INPUT=...     required — <what it is>
#   OPTIONAL_INPUT=foo     optional — <what it is> (default: foo)
#   DEMO_NO_PAUSE=1        unattended: skip the "Press Enter" prompts
#   NO_COLOR=1             disable ANSI color

set -euo pipefail

# --- configuration (one block; override only what differs) --------------------
BIN="${BIN:-mycli}"                 # the binary the demo drives (override: BIN=/path/to/mycli)
REQUIRED_INPUT="${REQUIRED_INPUT:-}"
OPTIONAL_INPUT="${OPTIONAL_INPUT:-foo}"
WORKDIR="${WORKDIR:-/tmp/demo}"   # scratch for the pre-staged artifact
OUT="$WORKDIR/out"                # the expensive-to-produce artifact
SKIP_SETUP="${SKIP_SETUP:-0}"     # reuse a pre-staged $OUT; skip the slow producer step

# --- presentation helpers -----------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; CYAN=$'\033[36m'; GREEN=$'\033[32m'
  YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'
else
  BOLD=''; DIM=''; CYAN=''; GREEN=''; YELLOW=''; RED=''; RESET=''
fi
STEP=0
banner() { STEP=$((STEP + 1)); printf '\n%s== STEP %s: %s ==%s\n' "$CYAN" "$STEP" "$1" "$RESET"; }
note()   { printf '%s%s%s\n' "$DIM" "$1" "$RESET"; }
# pause reads from the terminal directly so it works even when stdin is piped.
pause()  {
  [ "${DEMO_NO_PAUSE:-0}" = "1" ] && return 0
  printf '\n%s▶ %s%s ' "$YELLOW" "${1:-Press Enter to continue...}" "$RESET"
  if [ -r /dev/tty ]; then read -r _ </dev/tty; else read -r _ || true; fi
}
# run echoes the command, then runs it with output streaming straight to the
# terminal — never suppressed, never captured silently, never faked.
run() {
  printf '\n%s$ %s%s\n' "$GREEN" "$*" "$RESET"
  "$@"; local rc=$?
  printf '%s[exit %s]%s\n' "$DIM" "$rc" "$RESET"
  return "$rc"
}
# run_expect_fail: a NON-zero exit is the expected, successful outcome (e.g. a
# safety refusal you want to demo). The `|| rc=$?` keeps `set -euo pipefail`
# from aborting the demo on that intended failure.
run_expect_fail() {
  printf '\n%s$ %s%s\n' "$GREEN" "$*" "$RESET"
  local rc=0; "$@" || rc=$?
  if [ "$rc" -ne 0 ]; then printf '%s[exit %s — expected failure ✓]%s\n' "$GREEN" "$rc" "$RESET"
  else printf '%s[exit 0 — UNEXPECTED: was supposed to fail]%s\n' "$RED" "$RESET"; fi
}

# --- preflight: validate inputs needed in ALL modes, fail fast ----------------
# The binary is needed by every mode (setup AND the skip path), so check it here.
# Inputs only the producer step needs are validated inside the producer branch
# below — so a SKIP_SETUP=1 run doesn't demand inputs it won't use.
if ! command -v "$BIN" >/dev/null 2>&1 && [ ! -x "$BIN" ]; then
  printf '%sERROR: %q not found. Set BIN=/path/to/mycli.%s\n' "$RED" "$BIN" "$RESET" >&2
  exit 1
fi

# === 1. FRAME — why this matters (no commands) ================================
banner "Why this matters"
note "<one or two sentences framing the problem for the whole room>"
pause

# === 2. TELL — name the steps before running them =============================
banner "What we'll do"
note "1) <step one>   2) <step two>   3) <step three>"
note "Heads-up: <what's slow / needs network / has been pre-staged ahead of time>"
pause

# === 3. SHOW — run the real thing, stream real output =========================
# Pre-stage anything multi-minute ahead of time; SKIP_SETUP=1 reuses it so the
# live run is only the fast steps. Contract: guard the producer, assert the
# artifact exists (fail loudly), then run the fast/consumer steps either way.
if [ "$SKIP_SETUP" = "1" ]; then
  banner "Reuse pre-staged state (SKIP_SETUP=1)"
  [ -e "$OUT" ] || { printf '%sERROR: SKIP_SETUP=1 but %s missing — run once without it first.%s\n' "$RED" "$OUT" "$RESET" >&2; exit 1; }
  pause
else
  [ -n "$REQUIRED_INPUT" ] || {
    printf '%sERROR: REQUIRED_INPUT is required for the setup step.%s\n' "$RED" "$RESET" >&2; exit 1; }
  mkdir -p "$WORKDIR"
  banner "<slow setup step>"
  note "<what this produces — the multi-minute part>"
  pause "Press Enter to run <slow setup>"
  run echo "replace with the producer command (e.g. \"$BIN\" ...) that writes $OUT, using $REQUIRED_INPUT"
fi

banner "<live step — fast>"
note "<what this step proves>"
pause "Press Enter to run <live step>"
run echo "replace with the fast/consumer command (e.g. \"$BIN\" ...) that reads $OUT / $OPTIONAL_INPUT"
# A step whose failure IS the point (e.g. a safety refusal) uses run_expect_fail.

# === 4. CLOSE — recap + pointers ==============================================
banner "Done"
note "<one-line recap of what they just saw>"
note "Docs: <link/ADR>   ·   Related: <other demo>"

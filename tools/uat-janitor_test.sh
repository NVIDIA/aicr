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

# Unit harness for .github/scripts/uat-janitor.sh decision logic.
# Run directly: bash tools/uat-janitor_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: sources the janitor (its `main` guard means sourcing provisions
# nothing) and stubs `gh` on PATH, so no cloud CLI, credential, or network call
# is involved and no resource is ever touched. The assertions guard the
# safety-critical logic that decides whether a cloud deployment gets DESTROYED:
# the allowlist, the run-id requirement, the not-active liveness gate, the
# fail-closed handling of ambiguous GitHub API answers, and the age floors.
# Every ambiguous input must produce SKIP, never REAP.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
JANITOR="${REPO_ROOT}/.github/scripts/uat-janitor.sh"

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

# --- Stub `gh` on PATH --------------------------------------------------------
# GH_STUB drives the response for `gh api repos/*/actions/runs/<id>`:
#   ok:<status>:<updated_at>  200 with that run JSON
#   nofield                   200 whose body lacks status/updated_at
#   404                       run-level Not Found (the exact `HTTP 404` token)
#   404plain                  failure whose text merely CONTAINS 404, no HTTP 404
#   500                       any other API failure
# `gh api <url> [--jq ...]`. Branch on the URL so the two distinct calls
# classify() can make are stubbed independently: the single-run status lookup
# (keyed by GH_STUB) and the uat-daytime liveness list (keyed by DAYTIME_STUB).
cat >"${STUB_DIR}/gh" <<'STUB'
#!/usr/bin/env bash
url="${2:-}"
case "${url}" in
  *"/actions/workflows/"*"/runs"*)
    # uat_lifecycle_active() queries ?status=in_progress, so the list is already
    # filtered server-side. LIFECYCLE_STUB=active => one in-progress run; error =>
    # API failure; malformed/nullruns => 200 with a body that is not a well-formed
    # run list (all three must fail SAFE => "active"); default idle => empty array.
    # classify()'s reap assertions never set it, so they see idle.
    case "${LIFECYCLE_STUB:-idle}" in
      active)    printf '{"workflow_runs":[{"status":"in_progress"}]}\n' ;;
      error)     echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1 ;;
      malformed) printf '{"message":"partial response"}\n' ;;
      nullruns)  printf '{"workflow_runs":null}\n' ;;
      *)         printf '{"workflow_runs":[]}\n' ;;
    esac
    ;;
  *)
    case "${GH_STUB:-500}" in
      ok:*)
        rest="${GH_STUB#ok:}"
        printf '{"status":"%s","updated_at":"%s"}\n' "${rest%%:*}" "${rest#*:}"
        ;;
      nofield) printf '{"id":1}\n' ;;
      404) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
      404plain) echo "gh: gateway error code 404 upstream" >&2; exit 1 ;;
      *)   echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1 ;;
    esac
    ;;
esac
STUB
chmod +x "${STUB_DIR}/gh"

# The janitor uses GNU `date -u -d`; on a BSD-date host (macOS dev box) forward
# to gdate so the age assertions run everywhere CI does.
if ! date -u -d "2026-01-01T00:00:00Z" +%s >/dev/null 2>&1; then
    if command -v gdate >/dev/null 2>&1; then
        printf '#!/usr/bin/env bash\nexec gdate "$@"\n' >"${STUB_DIR}/date"
        chmod +x "${STUB_DIR}/date"
    else
        echo "SKIP: no GNU date (install coreutils for gdate)" >&2
        exit 0
    fi
fi
PATH="${STUB_DIR}:${PATH}"

# GH_STUB is read by the `gh` stub (a subprocess), REPO_REACHABLE by classify()
# from the sourced janitor — export both so later plain assignments carry over
# and shellcheck can see they are consumed.
export GH_STUB=500 REPO_REACHABLE=no
# Neutralize any host/CI export of the age floors so the SOURCED defaults are what
# we exercise (hermetic) AND the shipped default is actually asserted below — a
# regression flipping it would flip the assertion. Boundary cases set an explicit
# floor locally (a plain assignment; classify() reads the global at call time).
unset NIGHTLY_MIN_AGE_HOURS DAYTIME_MIN_AGE_HOURS
# Keyed independently of GH_STUB by the URL-aware `gh` stub above; idle so the
# daytime classify() reap assertions below (which don't set it) see no active run.
export LIFECYCLE_STUB=idle

# Source the janitor: the `main` guard keeps this inert. `gcp` only sets $CLOUD
# for log lines; no discovery or reaping is reachable from the pure helpers.
# shellcheck source=/dev/null
source "${JANITOR}" gcp
set +e   # the janitor's `set -e` would abort the harness on a failed assertion

ago() { date -u -d "-${1} hours" +%Y-%m-%dT%H:%M:%SZ; }

FAILED=0
check() { # check <label> <expected> <actual>
    if [[ "$2" == "$3" ]]; then
        echo "  ok   $1"
    else
        echo "  FAIL $1: expected '$2', got '$3'"
        FAILED=1
    fi
}

echo "shipped age-floor defaults:"
# Asserted from the sourced defaults (floors were unset above), so a regression in
# the shipped skip_delete retention policy is caught here, not silently pinned.
check "nightly default is 24h" "24" "${NIGHTLY_MIN_AGE_HOURS}"
check "daytime default is 24h" "24" "${DAYTIME_MIN_AGE_HOURS}"

echo "run_id_of:"
check "extracts trailing run id" "12345" "$(run_id_of aicr-uat-12345)"
check "extracts from daytime name" "999" "$(run_id_of aicr-uat-day-gh1-0-999)"
run_id_of aicr-uat-nightly >/dev/null 2>&1
check "rejects non-numeric tail" "1" "$?"
run_id_of aicr-uat >/dev/null 2>&1
check "rejects name with no id" "1" "$?"

echo "is_daytime:"
is_daytime aicr-uat-day-gh1-0-1; check "daytime name" "0" "$?"
is_daytime aicr-uat-1;           check "nightly name" "1" "$?"

echo "valid_dry_run:"
for v in true false; do
    valid_dry_run "$v"; check "accepts ${v}" "0" "$?"
done
# Anything else must be rejected outright: DRY_RUN is the last gate before the
# actuator, and only the exact string `true` holds it back, so a typo or a
# shell-truthy value must abort rather than resolve toward enforce.
for v in flase TRUE True 1 yes on "" " " "true "; do
    valid_dry_run "$v"; check "rejects '${v}'" "1" "$?"
done

echo "valid_age_floor:"
for v in 0 6 24 99999; do
    valid_age_floor "$v"; check "accepts '${v}'" "0" "$?"
done
# Empty, non-numeric, and negative are the obvious rejects; the last two guard the
# overflow class: >5 digits (and the 20-digit value that makes bash's `-lt` error
# with status 2) must be rejected up front, not read as REAP for a young run.
for v in "" " " -1 24h 1.5 100000 99999999999999999999; do
    valid_age_floor "$v"; check "rejects '${v}'" "1" "$?"
done

echo "age_hours_since:"
check "computes whole hours" "8" "$(age_hours_since "$(ago 8)")"
age_hours_since "not-a-timestamp" >/dev/null 2>&1
check "fails closed on garbage" "1" "$?"
age_hours_since "null" >/dev/null 2>&1
check "fails closed on literal null" "1" "$?"

echo "classify (allowlist + run id):"
GH_STUB="ok:completed:$(ago 99)"
# The allowlist is the outermost guard on a job that DESTROYS clusters, so both
# directions are asserted. Two schemas are accepted, anchored end to end:
#   aicr-uat-<run_id>                    nightly, all clouds
#   aicr-uat-day-<mid>-<run_id>          daytime, all clouds. <mid> is permissive
#                                        so it matches BOTH the current ADR-017
#                                        <slug>-<slot> form (gh1-0) AND the legacy
#                                        hyphenated <reservation> form (gcp-h100)
#                                        during the migration window.
# (GCP nightly is aicr-uat-<run_id> like the others — the GKE actuator now bounds
# the node-SA account_id, so the earlier aicr-<run_id> short form was dropped.)
check "accepts nightly"                  "REAP" "$(classify aicr-uat-31021150393)"
check "accepts daytime (reservation)"    "REAP" "$(classify aicr-uat-day-gcp-h100-31021150393)"
check "accepts daytime (adr-017 slug)"   "REAP" "$(classify aicr-uat-day-gh1-0-31021150393)"
check "accepts daytime multi-digit slot" "REAP" "$(classify aicr-uat-day-gh1-12-31021150393)"
# Still anchored: the aicr-uat-day- prefix and a trailing numeric run_id are
# required, so these are rejected — empty middle, no run id, wrong/absent prefix,
# or a non-numeric nightly tail. Persistent infra (aicr-testgrid-*) and the
# dropped GCP short form (aicr-<run_id>) match neither schema.
for n in aicr-uat-unrelated-7 aicr-uat- aicr-testgrid-vpc aicr-uat-day--7 \
         aicr-testgrid aicr-testgrid-7 aicr-prod-7 aicr- aicr \
         some-aicr-uat-123 aicr-uat-123-vpc aicr-uat-day-h100- \
         aicr-31021150393 ; do
    check "rejects '${n}'" "SKIP:not-allowlisted" "$(classify "$n")"
done
check "rejects missing run id" "SKIP:not-allowlisted" "$(classify aicr-uat-manual)"

echo "classify (liveness gate):"
for st in queued in_progress waiting pending requested; do
    GH_STUB="ok:${st}:$(ago 99)"
    check "skips active run (${st})" "SKIP:run-active(${st},run 7)" "$(classify aicr-uat-7)"
done
# Only `completed` may be reaped: a status GitHub adds later must fail closed,
# not fall through the case as "not active" and get destroyed on age alone.
for st in some_new_status neutral timed_out; do
    GH_STUB="ok:${st}:$(ago 99)"
    check "skips unsupported status (${st})" "SKIP:run-status-unsupported(${st},run 7)" "$(classify aicr-uat-7)"
done

echo "classify (fail closed on ambiguity):"
GH_STUB=500
check "skips on API error" "SKIP:gh-api-error(run 7)" "$(classify aicr-uat-7)"
GH_STUB=nofield
check "skips on incomplete run JSON" "SKIP:incomplete-run-json(run 7)" "$(classify aicr-uat-7)"
GH_STUB="ok:completed:not-a-timestamp"
check "skips on unparsable updated_at" "SKIP:bad-updated-at(not-a-timestamp)" "$(classify aicr-uat-7)"

echo "classify (404 requires a reachable repo):"
GH_STUB=404
REPO_REACHABLE=no
check "skips 404 when repo unproven" "SKIP:gh-api-error(run 7)" "$(classify aicr-uat-7)"
REPO_REACHABLE=yes
check "reaps 404 when repo proven" "REAP" "$(classify aicr-uat-7)"
# Narrowness guard: only the exact `HTTP 404` token counts as a definitive
# run-level 404. An error that merely CONTAINS "404" must stay a fail-closed skip
# EVEN with the repo proven reachable, so a future loosening of classify()'s grep
# (e.g. to a bare "404") can't silently widen the purged-run reap path.
GH_STUB=404plain
check "skips bare-404 text even when repo proven" "SKIP:gh-api-error(run 7)" "$(classify aicr-uat-7)"

echo "classify (age floors):"
REPO_REACHABLE=yes
# Boundary cases use an explicit nightly floor (6h) via a plain assignment —
# classify() reads the global at call time — so the arithmetic is exercised at a
# known threshold independent of the shipped 24h default asserted above.
NIGHTLY_MIN_AGE_HOURS=6
GH_STUB="ok:completed:$(ago 2)"
check "nightly under floor" "SKIP:too-young(2h < 6h)" "$(classify aicr-uat-7)"
GH_STUB="ok:completed:$(ago 8)"
check "nightly over floor" "REAP" "$(classify aicr-uat-7)"
GH_STUB="ok:completed:$(ago 8)"
check "daytime under floor" "SKIP:too-young(8h < 24h)" "$(classify aicr-uat-day-gcp-h100-7)"
GH_STUB="ok:completed:$(ago 30)"
check "daytime over floor" "REAP" "$(classify aicr-uat-day-gcp-h100-7)"

echo "uat_lifecycle_active (fail-safe liveness):"
# Only the uat-run.yaml workflow list drives this, so GH_STUB is irrelevant here.
# The three fail-safe cases (error/malformed/nullruns) must all read as "active".
LIFECYCLE_STUB=active;    uat_lifecycle_active; check "active when a lifecycle run is in flight" "0" "$?"
LIFECYCLE_STUB=idle;      uat_lifecycle_active; check "idle when none in flight"                 "1" "$?"
LIFECYCLE_STUB=error;     uat_lifecycle_active; check "fails safe (active) on api error"         "0" "$?"
LIFECYCLE_STUB=malformed; uat_lifecycle_active; check "fails safe (active) on missing field"     "0" "$?"
LIFECYCLE_STUB=nullruns;  uat_lifecycle_active; check "fails safe (active) on null workflow_runs" "0" "$?"

echo "classify (lifecycle liveness):"
# A daytime deployment old enough to reap must still DEFER while an evening
# daytime-DOWN (a uat-run.yaml run) is tearing down the same reservation — its live
# run id differs from the daytime-UP id in the name, so this is the only gate that
# can see it. Nightly is unaffected: its owning run IS what the run-status gate
# already checked.
REPO_REACHABLE=yes
GH_STUB="ok:completed:$(ago 30)"
LIFECYCLE_STUB=active
check "defers daytime reap while a lifecycle run is active" "SKIP:lifecycle-active(run 7)" "$(classify aicr-uat-day-gcp-h100-7)"
check "nightly ignores lifecycle liveness"                  "REAP"                          "$(classify aicr-uat-7)"
LIFECYCLE_STUB=idle
check "reaps daytime when no lifecycle run active"          "REAP"                          "$(classify aicr-uat-day-gcp-h100-7)"

if [[ "${FAILED}" -eq 0 ]]; then
    echo "PASS: uat-janitor decision logic"
else
    echo "FAIL: uat-janitor decision logic"
fi
exit "${FAILED}"

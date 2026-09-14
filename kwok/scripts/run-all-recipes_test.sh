#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Regression harness for run-all-recipes.sh's unmapped-profile handling.
# Proves the asymmetry required by #1997:
#   - Explicit invocation (positional recipe args, as every CI matrix
#     cell uses) must NOT report an unmapped recipe as passed; it must
#     specifically return rc=1 with the explicit-branch diagnostic so
#     the guard is genuinely covered (a downstream failure would also
#     be non-zero but wouldn't prove the guard fired).
#   - Implicit batch invocation (no args, get_recipes() drives the set,
#     as `make kwok-test-all` uses locally) may still SKIP an unmapped
#     recipe with rc=0 so a partly-populated profile tree doesn't turn
#     every dev-loop run red.
#
# Also proves that fatal selector failures (rc=1, e.g. duplicate profiles
# in the tree) propagate in BOTH modes — batch mode must not swallow
# tree-integrity faults, since a duplicate profile is exactly the kind of
# false-pass class this PR chain targets.
#
# Run directly: bash kwok/scripts/run-all-recipes_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

command -v yq >/dev/null 2>&1 || { echo "SKIP: yq not on PATH"; exit 0; }

# Source run-all-recipes.sh without letting main() run. The script's
# tail is the literal line `main "$@"`; strip it so we can source and
# then invoke individual functions with stubs in place.
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/run-all-recipes.sh"
# Keep the temp copy alongside the original so its ${SCRIPT_DIR} / lib
# resolution (which uses BASH_SOURCE) points at the real lib directory.
TMP_SOURCE=$(mktemp "${SCRIPT_DIR}/.run-all-recipes.test.XXXXXX.sh")
LOG_FILE=$(mktemp)
FIXTURE_ROOT=$(mktemp -d)
trap 'rm -f "${TMP_SOURCE}" "${LOG_FILE}"; rm -rf "${FIXTURE_ROOT}"' EXIT
grep -v '^main "\$@"$' "${SCRIPT_UNDER_TEST}" > "${TMP_SOURCE}"

# Verify the strip actually happened. The exact-anchored `grep -v` is a
# no-op if run-all-recipes.sh's tail line ever changes (trailing
# whitespace, `exec main`, a wrapping comment) — the pattern would stop
# matching, `main "$@"` would stay in the temp copy, and sourcing would
# execute the real main() with this harness's argv, driving into real
# kind/kubectl. Assert exactly one line disappeared and that the temp
# copy is free of any `main "$@"` invocation.
original_lines=$(wc -l < "${SCRIPT_UNDER_TEST}")
stripped_lines=$(wc -l < "${TMP_SOURCE}")
if (( original_lines - stripped_lines != 1 )); then
    echo "FAIL: expected exactly 1 line stripped from ${SCRIPT_UNDER_TEST}; got $((original_lines - stripped_lines))."
    echo "      The tail 'main \"\$@\"' invocation likely changed shape."
    echo "      Update the grep pattern in this harness or sourcing would run main() with the test argv."
    exit 1
fi
if grep -qE '^[[:space:]]*(exec[[:space:]]+)?main[[:space:]]+"\$@"[[:space:]]*$' "${TMP_SOURCE}"; then
    echo "FAIL: TMP_SOURCE still contains a 'main \"\$@\"' invocation after strip; sourcing would execute the real main()."
    exit 1
fi

# shellcheck disable=SC1090
source "${TMP_SOURCE}"

# Defense-in-depth: shadow the real main() with a no-op AFTER sourcing
# so an accidental invocation (from a future test that forgets to call
# a specific function) is inert rather than driving real cluster ops.
# shellcheck disable=SC2329
main() { :; }

# Stub cluster-touching helper AFTER sourcing so we win over the real
# definition. The unmapped tests exit run_recipe_test at the SKIP
# branch before cleanup_between_tests or apply-nodes.sh are reached;
# the dup-profile tests exit at the FATAL branch just as early. The
# stub is defense-in-depth against a future change that shifts either
# call above those checks.
# shellcheck disable=SC2329  # called indirectly via run_recipe_test
cleanup_between_tests() { :; }

# Pick a recipe known to be unmapped on the current tree. gb200-oke-training
# has service=oke, and no oke profile ships on main.
UNMAPPED_RECIPE="gb200-oke-training"
if [[ ! -f "${OVERLAYS_DIR}/${UNMAPPED_RECIPE}.yaml" ]]; then
    echo "FAIL: fixture recipe ${UNMAPPED_RECIPE} not found under ${OVERLAYS_DIR}"
    exit 1
fi

fails=0
ran=0
# check <name> <want_rc_op> <want_rc> <got_rc>
check() {
    local name="$1" want_rc_op="$2" want_rc="$3" got_rc="$4"
    ran=$((ran + 1))
    local ok=0
    case "${want_rc_op}" in
        eq) [[ "${got_rc}" == "${want_rc}" ]] && ok=1 ;;
        ne) [[ "${got_rc}" != "${want_rc}" ]] && ok=1 ;;
    esac
    if (( ok == 1 )); then
        echo "PASS: ${name}"
    else
        echo "FAIL: ${name} (want rc ${want_rc_op} ${want_rc}, got ${got_rc})"
        fails=$((fails + 1))
    fi
}

# check_log <name> <want_substring> <log_file>
# Asserts the log captured for a preceding run_recipe_test call contains
# the expected diagnostic; guards against a spurious pass where rc is
# right for the wrong reason (a downstream failure masquerading as the
# guard-triggered failure).
check_log() {
    local name="$1" want="$2" log_file="$3"
    ran=$((ran + 1))
    if grep -qF "${want}" "${log_file}"; then
        echo "PASS: ${name}"
    else
        echo "FAIL: ${name} (log did not contain '${want}')"
        echo "  ---- captured log ----"
        sed 's/^/    /' "${log_file}" || true
        echo "  ----------------------"
        fails=$((fails + 1))
    fi
}

# ── Unmapped-recipe tests (real profile tree) ─────────────────────────

# 1. Explicit invocation — the operator named this recipe on the command
# line, matching what every CI matrix cell does. An unmapped profile
# must fail with rc=1 SPECIFICALLY (a downstream failure like
# apply-nodes.sh hitting no cluster would also be non-zero without
# proving the explicit-branch guard fired) AND the captured log must
# carry the explicit-branch diagnostic.
# shellcheck disable=SC2034  # read by sourced run_recipe_test via is_explicit_recipe
EXPLICIT_RECIPES="${UNMAPPED_RECIPE}"
: > "${LOG_FILE}"
rc=0
run_recipe_test "${UNMAPPED_RECIPE}" >"${LOG_FILE}" 2>&1 || rc=$?
check "explicit-unmapped-recipe-fails-with-rc-1" eq 1 "${rc}"
check_log "explicit-unmapped-recipe-carries-explicit-diagnostic" "explicitly requested but has no KWOK profile" "${LOG_FILE}"

# 2. Implicit batch invocation — no positional args, recipe came from
# get_recipes(). Historical dev-loop behavior (make kwok-test-all) is
# preserved: unmapped recipes SKIP with rc=0 so a partly-populated
# profile tree doesn't turn every local run red.
# shellcheck disable=SC2034  # read by sourced run_recipe_test via is_explicit_recipe
EXPLICIT_RECIPES=""
: > "${LOG_FILE}"
rc=0
run_recipe_test "${UNMAPPED_RECIPE}" >"${LOG_FILE}" 2>&1 || rc=$?
check "implicit-batch-unmapped-recipe-is-still-skippable" eq 0 "${rc}"

# ── Fatal-selector tests (temp fixture with duplicate profiles) ───────
#
# A duplicate system profile makes select_profiles return rc=1 (not the
# skippable rc=2). run_recipe_test must propagate that in BOTH modes —
# batch mode explicitly cannot swallow rc=1, since a duplicate profile
# is the exact tree-integrity fault this PR chain refuses to hide.

# Build the fixture: a "dup" service with two system profiles. Compute
# name/dir outside the write block so shellcheck's SC2094 (read-and-write
# same file in a pipeline) doesn't flip on the inline $(basename ...).
write_profile() {
    local path="$1" provider="$2" nodeType="$3" accelerator="${4:-}"
    local name dir
    name=$(basename "${path}" .yaml)
    dir=$(dirname "${path}")
    mkdir -p "${dir}"
    {
        echo "apiVersion: aicr.run/v1beta1"
        echo "kind: KWOKNodeProfile"
        echo "metadata:"
        echo "  name: ${name}"
        echo "  labels:"
        echo "    provider: ${provider}"
        echo "    nodeType: ${nodeType}"
        [[ -n "${accelerator}" ]] && echo "    accelerator: ${accelerator}"
        echo "spec:"
        echo "  instanceType: fake"
    } > "${path}"
}
write_overlay() {
    local path="$1" service="$2" accelerator="$3"
    local name dir
    name=$(basename "${path}" .yaml)
    dir=$(dirname "${path}")
    mkdir -p "${dir}"
    cat > "${path}" <<EOF
apiVersion: aicr.run/v1beta1
kind: recipeMetadata
metadata:
  name: ${name}
spec:
  criteria:
    service: ${service}
    accelerator: ${accelerator}
EOF
}

write_profile "${FIXTURE_ROOT}/kwok/profiles/dup/system-a.yaml" dup system
write_profile "${FIXTURE_ROOT}/kwok/profiles/dup/system-b.yaml" dup system
write_profile "${FIXTURE_ROOT}/kwok/profiles/dup/p5-h100.yaml"  dup accelerated h100
write_overlay "${FIXTURE_ROOT}/overlays/dup-fixture.yaml"       dup h100

# Repoint the sourced globals at the fixture. Save originals in case a
# future test wants to switch back.
_ORIG_KWOK_DIR="${KWOK_DIR}"
_ORIG_OVERLAYS_DIR="${OVERLAYS_DIR}"
# shellcheck disable=SC2034  # read by sourced run_recipe_test
KWOK_DIR="${FIXTURE_ROOT}/kwok"
# shellcheck disable=SC2034  # read by sourced run_recipe_test
OVERLAYS_DIR="${FIXTURE_ROOT}/overlays"

# 3. Explicit invocation with a duplicate system profile — FATAL rc=1
# must propagate, and the operator-facing log must identify why (the
# ambiguous-system-profile diagnostic from select_profiles).
# shellcheck disable=SC2034
EXPLICIT_RECIPES="dup-fixture"
: > "${LOG_FILE}"
rc=0
run_recipe_test "dup-fixture" >"${LOG_FILE}" 2>&1 || rc=$?
check "explicit-dup-profile-fails-with-rc-1" eq 1 "${rc}"
check_log "explicit-dup-profile-log-identifies-ambiguous-system" "Multiple system profiles" "${LOG_FILE}"

# 4. Implicit batch invocation with a duplicate system profile — same
# outcome. Batch mode maps rc=2 (no-match) to skip but MUST propagate
# rc=1 (tree fault), otherwise a duplicate profile could quietly pass
# every batch CI run.
# shellcheck disable=SC2034
EXPLICIT_RECIPES=""
: > "${LOG_FILE}"
rc=0
run_recipe_test "dup-fixture" >"${LOG_FILE}" 2>&1 || rc=$?
check "implicit-batch-dup-profile-does-not-swallow-rc-1" eq 1 "${rc}"
check_log "implicit-batch-dup-profile-log-identifies-ambiguous-system" "Multiple system profiles" "${LOG_FILE}"

# Restore original globals for any downstream tests (currently none;
# harmless if the fixture-mutating pattern grows).
# shellcheck disable=SC2034
KWOK_DIR="${_ORIG_KWOK_DIR}"
# shellcheck disable=SC2034
OVERLAYS_DIR="${_ORIG_OVERLAYS_DIR}"

# ── Setup-failure CTRF record (#1806) ──────────────────────────────────
# The real main() is exercised end to end with stubbed kind/kubectl/helm on
# PATH so cluster and context setup fail the way they do in CI. Both paths
# must leave kwok-results.json with one kwok/setup/<deployer> entry of status
# "other" naming the failing stage, and must preserve the original exit code.
# The first case is the errexit trap: `ensure_cluster || rc=$?` would have let
# an internal `kind create cluster` failure return 0.
if command -v jq >/dev/null 2>&1; then
    STUB_BIN="${FIXTURE_ROOT}/stub-bin"
    mkdir -p "${STUB_BIN}"
    cat > "${STUB_BIN}/kind" <<'STUB'
#!/usr/bin/env bash
case "$1" in
    get) exit 0 ;;                       # no clusters -> create path
    create) echo "stub: kind create failed" >&2; exit 42 ;;
    *) exit 0 ;;
esac
STUB
    cat > "${STUB_BIN}/kubectl" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "config" && "$2" == "current-context" ]]; then echo "${STUB_CONTEXT:-kind-aicr-kwok-test}"; fi
exit 0
STUB
    cat > "${STUB_BIN}/helm" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
    chmod +x "${STUB_BIN}"/*

    SETUP_RESULTS="${FIXTURE_ROOT}/setup-results.json"
    : > "${LOG_FILE}"
    rc=0
    PATH="${STUB_BIN}:${PATH}" KWOK_RESULTS_FILE="${SETUP_RESULTS}" KWOK_CLUSTER=aicr-kwok-test \
        bash "${SCRIPT_UNDER_TEST}" --deployer helm gb200-eks-training >"${LOG_FILE}" 2>&1 || rc=$?
    check "setup-cluster-failure-preserves-exit-code" eq 42 "${rc}"
    ran=$((ran + 1))
    if jq -e '.results.summary.other == 1 and .results.summary.tests == 1
              and .results.tests[0].name == "kwok/setup/helm"
              and .results.tests[0].status == "other"
              and (.results.tests[0].message | test("cluster setup failed \\(rc=42\\)"))' \
              "${SETUP_RESULTS}" >/dev/null 2>&1; then
        echo "PASS: setup-cluster-failure-writes-other-record"
    else
        echo "FAIL: setup-cluster-failure-writes-other-record"; cat "${SETUP_RESULTS}" 2>/dev/null; fails=$((fails + 1))
    fi

    # Context guard: the cluster exists, but kubectl points somewhere else.
    cat > "${STUB_BIN}/kind" <<'STUB'
#!/usr/bin/env bash
case "$1" in
    get) echo "aicr-kwok-test" ;;
    *) exit 0 ;;
esac
STUB
    chmod +x "${STUB_BIN}/kind"
    : > "${LOG_FILE}"
    rc=0
    PATH="${STUB_BIN}:${PATH}" STUB_CONTEXT=kind-production KWOK_RESULTS_FILE="${SETUP_RESULTS}" KWOK_CLUSTER=aicr-kwok-test \
        bash "${SCRIPT_UNDER_TEST}" --deployer helm gb200-eks-training >"${LOG_FILE}" 2>&1 || rc=$?
    check "setup-context-guard-preserves-exit-code" eq 1 "${rc}"
    check_log "setup-context-guard-refuses-foreign-context" "is not a known KWOK Kind cluster" "${LOG_FILE}"
    ran=$((ran + 1))
    if jq -e '.results.summary.tests == 1 and .results.tests[0].status == "other"
              and (.results.tests[0].message | test("context setup failed \\(rc=1\\)"))' \
              "${SETUP_RESULTS}" >/dev/null 2>&1; then
        echo "PASS: setup-context-failure-writes-other-record"
    else
        echo "FAIL: setup-context-failure-writes-other-record"; cat "${SETUP_RESULTS}" 2>/dev/null; fails=$((fails + 1))
    fi
else
    echo "SKIP: jq not on PATH; setup-failure CTRF cases not run"
fi

# ── Interrupted cell and report-write failure (#1806) ──────────────────
# A TERM while a recipe is running must leave a valid report with the active
# cell recorded as "other" and exit 143 like an untrapped TERM would. A report
# path that cannot be written must not change the run's exit status.
if command -v jq >/dev/null 2>&1; then
    # A private copy of the kwok tree with stubbed apply-nodes.sh and a
    # blocking validate-scheduling.sh, so the real main() reaches the recipe
    # loop without a cluster. REPO_ROOT resolves to ${FIXTURE_ROOT}, so the
    # overlay and tools/ctrf are copied to the same relative places.
    KWOK_COPY="${FIXTURE_ROOT}/kwok"
    rm -rf "${KWOK_COPY}"
    cp -R "${SCRIPT_DIR}/.." "${KWOK_COPY}"
    mkdir -p "${FIXTURE_ROOT}/tools" "${FIXTURE_ROOT}/recipes/overlays"
    cp "${SCRIPT_DIR}/../../tools/ctrf" "${FIXTURE_ROOT}/tools/ctrf"
    cp "${SCRIPT_DIR}/../../recipes/overlays/gb200-eks-training.yaml" "${FIXTURE_ROOT}/recipes/overlays/"
    printf '#!/usr/bin/env bash\nexit 0\n' > "${KWOK_COPY}/scripts/apply-nodes.sh"
    cat > "${KWOK_COPY}/scripts/validate-scheduling.sh" <<'STUB'
#!/usr/bin/env bash
touch "${KWOK_STUB_STARTED:?}"
sleep 300
STUB
    chmod +x "${KWOK_COPY}/scripts/apply-nodes.sh" "${KWOK_COPY}/scripts/validate-scheduling.sh"
    cat > "${STUB_BIN}/kind" <<'STUB'
#!/usr/bin/env bash
case "$1" in
    get) echo "aicr-kwok-test" ;;
    *) exit 0 ;;
esac
STUB
    chmod +x "${STUB_BIN}/kind"

    INT_RESULTS="${FIXTURE_ROOT}/interrupt-results.json"
    STARTED="${FIXTURE_ROOT}/stub-started"
    rm -f "${STARTED}" "${INT_RESULTS}"
    : > "${LOG_FILE}"
    PATH="${STUB_BIN}:${PATH}" KWOK_STUB_STARTED="${STARTED}" KWOK_RESULTS_FILE="${INT_RESULTS}" KWOK_CLUSTER=aicr-kwok-test \
        bash "${KWOK_COPY}/scripts/run-all-recipes.sh" --deployer helm gb200-eks-training >"${LOG_FILE}" 2>&1 &
    runner_pid=$!
    for _ in $(seq 1 100); do [[ -f "${STARTED}" ]] && break; sleep 0.2; done
    if [[ ! -f "${STARTED}" ]]; then
        echo "FAIL: interrupted-cell: blocking stub never started"; fails=$((fails + 1)); kill "${runner_pid}" 2>/dev/null || true
    else
        kill -TERM "${runner_pid}"
        rc=0; wait "${runner_pid}" || rc=$?
        check "interrupted-cell-exits-143" eq 143 "${rc}"
        ran=$((ran + 1))
        if jq -e '.results.summary.tests == 1 and .results.summary.other == 1
                  and .results.tests[0].name == "kwok/gb200-eks-training/helm"
                  and (.results.tests[0].message | test("interrupted by SIGTERM"))' "${INT_RESULTS}" >/dev/null 2>&1; then
            echo "PASS: interrupted-cell-writes-other-record"
        else
            echo "FAIL: interrupted-cell-writes-other-record"; cat "${INT_RESULTS}" 2>/dev/null; fails=$((fails + 1))
        fi
    fi
    pkill -f "${KWOK_COPY}/scripts/validate-scheduling.sh" 2>/dev/null || true

    # Unwritable report path: /dev/null is a file, so mkdir -p of the parent
    # fails inside ctrf_write. The kind-create failure (exit 42) must still be
    # the run's exit status, and the failure must be logged, not fatal.
    cat > "${STUB_BIN}/kind" <<'STUB'
#!/usr/bin/env bash
case "$1" in
    get) exit 0 ;;
    create) echo "stub: kind create failed" >&2; exit 42 ;;
    *) exit 0 ;;
esac
STUB
    chmod +x "${STUB_BIN}/kind"
    : > "${LOG_FILE}"
    rc=0
    PATH="${STUB_BIN}:${PATH}" KWOK_RESULTS_FILE="/dev/null/kwok-results.json" KWOK_CLUSTER=aicr-kwok-test \
        bash "${SCRIPT_UNDER_TEST}" --deployer helm gb200-eks-training >"${LOG_FILE}" 2>&1 || rc=$?
    check "unwritable-report-preserves-exit-code" eq 42 "${rc}"
    check_log "unwritable-report-is-logged-not-fatal" "failed to write CTRF results" "${LOG_FILE}"
else
    echo "SKIP: jq not on PATH; interruption and write-failure cases not run"
fi

# ── Summary ────────────────────────────────────────────────────────────
# ${ran} is incremented inside check / check_log so the summary count is
# derived, not a hardcoded literal — a future contributor adding or
# removing an assertion doesn't have to remember to bump a string, and
# an early-exit or forgotten count-bump can't paint a partial run green.
if (( fails > 0 )); then
    echo "${fails} test(s) failed (${ran} attempted)"
    exit 1
fi
echo "All ${ran} tests passed"

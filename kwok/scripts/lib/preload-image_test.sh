#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Unit harness for lib/preload-image.sh (Kind image side-loading).
#
# The property under test is mostly about what preload_image must NOT do. It
# exists to remove a failure mode -- the kubelet pulling a public image inside a
# fixed 120s rollout budget -- so a bug that makes it fail the lane is strictly
# worse than not having it. Every case below therefore asserts rc=0, and the
# interesting assertions are on which stub commands ran.
#
# Run directly: bash kwok/scripts/lib/preload-image_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# install-infra.sh defines these before sourcing the subject; the harness
# supplies its own so log output does not pollute the assertions.
log_info()  { echo "INFO: $*"; }
log_warn()  { echo "WARN: $*"; }
log_debug() { echo "DEBUG: $*"; }

# Resolve the subject SCRIPT_DIR-relative — never a deployed copy.
# shellcheck source=preload-image.sh
source "${SCRIPT_DIR}/preload-image.sh"

fails=0
check() { # <name> <want_rc> <got_rc> [<must_contain> ...]
    local name="$1" want_rc="$2" got_rc="$3"; shift 3
    local ok=1 needle
    if [[ "${got_rc}" != "${want_rc}" ]]; then
        echo "FAIL: ${name} (want rc=${want_rc}, got rc=${got_rc})"
        fails=$((fails + 1))
        return
    fi
    for needle in "$@"; do
        if ! grep -q -- "${needle}" "${TRACE}"; then
            echo "FAIL: ${name} (trace missing '${needle}')"
            echo "      trace was: $(tr '\n' '|' < "${TRACE}")"
            ok=0
        fi
    done
    (( ok == 1 )) && echo "PASS: ${name}" || fails=$((fails + 1))
}

# check_bounded asserts the call finished within its CONFIGURED budget rather
# than some loose ceiling. A generous threshold would let a retry-backoff or
# extra-operation regression push the real ceiling well past the budget and
# still pass, which is the thing being tested.
#
# The margin covers process spawn and scheduling only.
BOUND_MARGIN_SECONDS=3
check_bounded() { # <name> <budget> <elapsed>
    local name="$1" budget="$2" elapsed="$3"
    local ceiling=$(( budget + BOUND_MARGIN_SECONDS ))
    if (( elapsed <= ceiling )); then
        echo "PASS: ${name} (${elapsed}s, budget ${budget}s)"
    else
        echo "FAIL: ${name} (took ${elapsed}s, budget ${budget}s + ${BOUND_MARGIN_SECONDS}s margin)"
        fails=$((fails + 1))
    fi
}

check_absent() { # <name> <must_not_contain>
    local name="$1" needle="$2"
    if grep -q -- "${needle}" "${TRACE}"; then
        echo "FAIL: ${name} (trace unexpectedly contains '${needle}')"
        echo "      trace was: $(tr '\n' '|' < "${TRACE}")"
        fails=$((fails + 1))
    else
        echo "PASS: ${name}"
    fi
}

STUB_DIR="$(mktemp -d)"
TRACE="${STUB_DIR}/trace"
trap 'rm -rf "${STUB_DIR}"' EXIT
PATH="${STUB_DIR}:${PATH}"

# Stub behavior is driven by these, reset before each case:
# Each must be EXPORTED: the stubs are separate processes, so a plain
# assignment would leave them at their defaults and quietly make a case pass
# over the wrong scenario.
#   STUB_INSPECT_RC   docker image inspect exit code (1 = not cached locally)
#   STUB_PULL_FAILS   number of leading `docker pull` attempts that fail
#   STUB_PULL_HANGS   non-empty makes every `docker pull` hang forever
#   STUB_INSPECT_HANGS non-empty makes every `docker image inspect` hang forever
#   STUB_PULL_SLOW_FAIL seconds a `docker pull` stalls after emitting its error
#   STUB_PULL_SLOW_SUCCESS seconds a `docker pull` stalls AFTER landing the image
#   STUB_PULL_SILENT_FAIL non-empty makes `docker pull` exit 1 with no stderr
#   STUB_PULL_BLANK_ERR non-empty makes a killed `docker pull` emit only a newline
#   STUB_INSPECT_SLOW seconds every `docker image inspect` stalls before answering
#   STUB_INSPECT_ALWAYS_MISS non-empty makes inspect report absent even after a pull
#   STUB_KIND_LOAD_RC `kind load` exit code
#   STUB_CLUSTERS     newline-separated `kind get clusters` output
write_stubs() {
    cat > "${STUB_DIR}/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "${TRACE}"
case "$1 $2" in
    "image inspect")
        # A wedged Docker Engine: the request is accepted and never answered.
        if [[ -n "${STUB_INSPECT_HANGS:-}" ]]; then sleep 3600; fi
        # A busy but RESPONSIVE Engine: slow, yet it does answer. Must not be
        # mistaken for a cache miss while the budget still has room.
        if [[ -n "${STUB_INSPECT_SLOW:-}" ]]; then sleep "${STUB_INSPECT_SLOW}"; fi
        # The image never becomes visible, even after a pull reports success:
        # a pull that exits 0 without the image landing (wrong ref, racing
        # prune). The loop still terminates because the pull breaks it.
        if [[ -n "${STUB_INSPECT_ALWAYS_MISS:-}" ]]; then exit 1; fi
        # Once a pull has succeeded the image is cached, so later inspects
        # must succeed too — otherwise the retry loop could not terminate.
        if [[ -f "${STUB_DIR}/pulled" ]]; then exit 0; fi
        exit "${STUB_INSPECT_RC:-1}"
        ;;
esac
if [[ "$1" == "pull" ]]; then
    n=$(( $(cat "${STUB_DIR}/pulls" 2>/dev/null || echo 0) + 1 ))
    echo "${n}" > "${STUB_DIR}/pulls"
    # A pull that never returns — the registry accepts the connection and then
    # goes quiet. Only `timeout` ends this.
    if [[ -n "${STUB_PULL_HANGS:-}" ]]; then sleep 3600; fi
    # A pull that reports its cause and then stalls until `timeout` kills it,
    # consuming the budget. stderr is written FIRST so the cause exists even
    # though the process never exits on its own.
    if [[ -n "${STUB_PULL_SLOW_FAIL:-}" ]]; then
        echo "toomanyrequests: Rate exceeded" >&2
        sleep "${STUB_PULL_SLOW_FAIL}"
        exit 1
    fi
    # A pull that lands the image and then overruns the budget. The layers are
    # on the host before `timeout` kills the client, so the image IS cached; any
    # verdict that says otherwise is reporting the clock, not the cache.
    if [[ -n "${STUB_PULL_SLOW_SUCCESS:-}" ]]; then
        touch "${STUB_DIR}/pulled"
        sleep "${STUB_PULL_SLOW_SUCCESS}"
        exit 0
    fi
    # A pull killed by `timeout` that managed to emit only a blank line first.
    # `tr` turns that newline into a space, so the captured cause is non-empty
    # whitespace -- which must not out-rank the exit code as a diagnosis.
    if [[ -n "${STUB_PULL_BLANK_ERR:-}" ]]; then
        printf '\n' >&2
        sleep 3600
    fi
    # A pull that fails WITHOUT writing to stderr. docker does this on its own,
    # so an empty captured cause must not be read as "timeout killed it".
    if [[ -n "${STUB_PULL_SILENT_FAIL:-}" ]]; then
        exit 1
    fi
    if (( n <= ${STUB_PULL_FAILS:-0} )); then
        # Real docker writes the cause here; the subject must forward it.
        echo "toomanyrequests: Rate exceeded" >&2
        exit 1
    fi
    touch "${STUB_DIR}/pulled"
    exit 0
fi
exit 0
EOF

    cat > "${STUB_DIR}/kind" <<'EOF'
#!/usr/bin/env bash
echo "kind $*" >> "${TRACE}"
if [[ "$1 $2" == "get clusters" ]]; then
    printf '%s\n' ${STUB_CLUSTERS:-aicr-kwok-test}
    exit 0
fi
if [[ "$1 $2" == "load docker-image" ]]; then
    exit "${STUB_KIND_LOAD_RC:-0}"
fi
exit 0
EOF
    chmod +x "${STUB_DIR}/docker" "${STUB_DIR}/kind"
}

reset() {
    : > "${TRACE}"
    rm -f "${STUB_DIR}/pulls" "${STUB_DIR}/pulled"
    unset STUB_INSPECT_RC STUB_PULL_FAILS STUB_KIND_LOAD_RC STUB_CLUSTERS STUB_PULL_HANGS
    unset STUB_INSPECT_HANGS STUB_PULL_SLOW_FAIL STUB_PULL_SLOW_SUCCESS
    unset STUB_PULL_SILENT_FAIL STUB_INSPECT_SLOW
    unset STUB_INSPECT_ALWAYS_MISS STUB_PULL_BLANK_ERR
    unset KWOK_PRELOAD_BUDGET_SECONDS
    unset KUBECTL_CONTEXT KWOK_CLUSTER
    export STUB_DIR TRACE
    write_stubs
}

IMG="public.ecr.aws/docker/library/registry:3.1.1"

# 1. The happy path: image is not cached, one pull, one side-load.
reset
preload_image "${IMG}" >/dev/null; rc=$?
check "pulls-then-loads" 0 "${rc}" "docker pull" "kind load docker-image ${IMG}"

# 2. Already cached on the runner -> no pull, still side-loaded. Lanes share a
#    runner, so the second lane must not re-pull.
reset
export STUB_INSPECT_RC=0
preload_image "${IMG}" >/dev/null; rc=$?
check "cached-image-still-loads" 0 "${rc}" "kind load docker-image ${IMG}"
check_absent "cached-image-skips-pull" "docker pull"

# 3. The case this whole change exists for: the first two pulls fail (the
#    transient upstream reset seen in CI) and the third succeeds.
reset
export STUB_PULL_FAILS=2
preload_image "${IMG}" >/dev/null; rc=$?
check "retries-transient-pull-failure" 0 "${rc}" "kind load docker-image ${IMG}"
if [[ "$(cat "${STUB_DIR}/pulls" 2>/dev/null || echo 0)" != "3" ]]; then
    echo "FAIL: retries-transient-pull-failure (want 3 pull attempts, got $(cat "${STUB_DIR}/pulls" 2>/dev/null || echo 0))"
    fails=$((fails + 1))
else
    echo "PASS: retries-until-the-pull-succeeds"
fi

# 3b. Retries are driven by the BUDGET, not a fixed attempt count. The schedule
#     this replaced gave up after 3 tries in ~17s while holding a 180s budget
#     (#2483), which is why a throttled pull never recovered. With a budget that
#     allows more attempts, more attempts must happen.
reset
# Three failures then success: the old fixed-3 schedule would give up before
# the fourth attempt and never load.
export STUB_PULL_FAILS=3
export KWOK_PRELOAD_BUDGET_SECONDS=40
preload_image "${IMG}" >/dev/null; rc=$?
attempts=$(cat "${STUB_DIR}/pulls" 2>/dev/null || echo 0)
check "budget-allows-more-than-three-attempts" 0 "${rc}" "kind load docker-image ${IMG}"
if (( attempts >= 4 )); then
    echo "PASS: retry-count-follows-the-budget (${attempts} attempts)"
else
    echo "FAIL: retry-count-follows-the-budget (only ${attempts} attempts; a fixed 3-attempt cap is back)"
    fails=$((fails + 1))
fi
unset KWOK_PRELOAD_BUDGET_SECONDS

# 4. Every pull fails -> rc still 0 and NO side-load attempted. The kubelet
#    fallback must remain, and a missing image must not be "loaded".
reset
export STUB_PULL_FAILS=99
# Small budget: exhaustion now means "spent the budget", so the default 180s
# would make this case dominate the suite's runtime.
export KWOK_PRELOAD_BUDGET_SECONDS=5
out=$(preload_image "${IMG}" 2>&1); rc=$?
check "pull-exhausted-does-not-fail-the-lane" 0 "${rc}"
check_absent "pull-exhausted-skips-load" "kind load"
# The failure REASON must reach the log, not merely the "last error:" label.
# Asserting the label alone passes even when the captured stderr is empty --
# which is exactly the discard-the-cause bug this guards against (#2483).
if [[ "${out}" == *"toomanyrequests"* ]]; then
    echo "PASS: pull-exhausted-reports-the-error"
else
    echo "FAIL: pull-exhausted-reports-the-error (docker's stderr was not forwarded: ${out})"
    fails=$((fails + 1))
fi
unset KWOK_PRELOAD_BUDGET_SECONDS

# 5. Side-load itself fails -> rc still 0. Same reason.
reset
export STUB_KIND_LOAD_RC=1
preload_image "${IMG}" >/dev/null; rc=$?
check "load-failure-does-not-fail-the-lane" 0 "${rc}" "kind load docker-image"

# 6. Cluster named by the context, not the default.
reset
export STUB_CLUSTERS="other-cluster"
export KUBECTL_CONTEXT="kind-other-cluster"
preload_image "${IMG}" >/dev/null; rc=$?
check "context-selects-the-cluster" 0 "${rc}" "kind load docker-image ${IMG} --name other-cluster"

# 7. A non-Kind context (a real cluster) -> no docker or kind work at all.
#    Preloading into a Kind node would be meaningless there.
reset
export KUBECTL_CONTEXT="arn:aws:eks:us-west-2:123456789012:cluster/prod"
preload_image "${IMG}" >/dev/null; rc=$?
check "non-kind-context-is-a-noop" 0 "${rc}"
check_absent "non-kind-context-skips-docker" "docker"

# 8. Context names a cluster that does not exist -> no pull, no load.
reset
export STUB_CLUSTERS="aicr-kwok-test"
export KUBECTL_CONTEXT="kind-missing-cluster"
preload_image "${IMG}" >/dev/null; rc=$?
check "unknown-cluster-is-a-noop" 0 "${rc}"
check_absent "unknown-cluster-skips-pull" "docker pull"

# 8b. A cluster whose name merely CONTAINS the target must not satisfy the
#     existence check. `kind get clusters` is line-oriented, so a substring
#     match would side-load into the wrong cluster's node.
reset
export STUB_CLUSTERS="aicr-kwok-test-two"
export KUBECTL_CONTEXT="kind-aicr-kwok-test"
preload_image "${IMG}" >/dev/null; rc=$?
check "substring-cluster-name-is-not-a-match" 0 "${rc}"
check_absent "substring-cluster-name-skips-load" "kind load"

# 9. KWOK_CLUSTER is honored when no context is pinned.
reset
export STUB_CLUSTERS="custom-kwok"
export KWOK_CLUSTER="custom-kwok"
preload_image "${IMG}" >/dev/null; rc=$?
check "kwok-cluster-env-selects-the-cluster" 0 "${rc}" "--name custom-kwok"

# 10. Neither docker nor kind on PATH (a dev box driving a remote cluster).
#     Must be a silent no-op rather than an error.
#
#     Removing the stubs is NOT enough: the host running this harness usually
#     HAS docker and kind, so command lookup falls through to the real binaries
#     and the case passes having exercised a live pull instead of the branch it
#     names. Point PATH at an empty directory so the lookup genuinely fails, and
#     assert the log line so the branch is proven to have run rather than
#     inferred from rc=0.
reset
mkdir -p "${STUB_DIR}/no-tools"
saved_path="${PATH}"
# shellcheck disable=SC2123  # Replacing PATH is the point: it is what makes
# `command -v docker` fail. Restored two lines down.
PATH="${STUB_DIR}/no-tools"
out=$(preload_image "${IMG}" 2>&1); rc=$?
PATH="${saved_path}"
check "missing-tooling-is-a-noop" 0 "${rc}"
if [[ "${out}" == *"unavailable"* ]]; then
    echo "PASS: missing-tooling-takes-the-unavailable-branch"
else
    echo "FAIL: missing-tooling-takes-the-unavailable-branch (got: ${out})"
    fails=$((fails + 1))
fi
check_absent "missing-tooling-runs-nothing" "docker"

# 11. A pull that never returns. This is the failure mode the whole change
#     exists to prevent, reintroduced in a new place: without a bound, three
#     stalled pulls plus a stalled side-load hold the lane for the entire
#     20-minute KWOK job, and the kubelet fallback never gets to run.
#
#     The budget is squeezed to 3s so the assertion is about the bound existing,
#     not about its production value.
reset
export STUB_PULL_HANGS=1
export KWOK_PRELOAD_BUDGET_SECONDS=3
started=$(date +%s)
preload_image "${IMG}" >/dev/null; rc=$?
elapsed=$(( $(date +%s) - started ))
check "hanging-pull-does-not-fail-the-lane" 0 "${rc}"
# Against the configured budget, not a loose ceiling. This also pins the
# backoff clamp: without it the first attempt's timeout would be followed by a
# full 5s sleep, overshooting the budget.
check_bounded "hanging-pull-is-bounded" "${KWOK_PRELOAD_BUDGET_SECONDS}" "${elapsed}"
check_absent "hanging-pull-skips-load" "kind load"
unset STUB_PULL_HANGS KWOK_PRELOAD_BUDGET_SECONDS

# 12. A wedged Docker Engine: `docker image inspect` never returns. It is the
#     first call in the retry loop, so an unbounded inspect stalls once per
#     attempt before the kubelet fallback can run — the same failure class as a
#     hanging pull, in the call that is easiest to assume is local and cheap.
#
#     The ceiling here is the budget PLUS one PRELOAD_PROBE_TIMEOUT, not the
#     budget alone: the final "is it cached" verdict is deliberately not drawn
#     from the deadline (a spent budget must not be reported as a cache miss),
#     so against a wedged daemon it costs its own bound on top. That is one
#     probe, not one per attempt — the in-loop check stays budget-bounded, which
#     is what keeps this from growing with the retry count.
reset
export STUB_INSPECT_HANGS=1
export KWOK_PRELOAD_BUDGET_SECONDS=3
started=$(date +%s)
preload_image "${IMG}" >/dev/null; rc=$?
elapsed=$(( $(date +%s) - started ))
check "hanging-inspect-does-not-fail-the-lane" 0 "${rc}"
check_bounded "hanging-inspect-is-bounded" \
    "$(( KWOK_PRELOAD_BUDGET_SECONDS + PRELOAD_PROBE_TIMEOUT ))" "${elapsed}"
check_absent "hanging-inspect-skips-load" "kind load"
unset STUB_INSPECT_HANGS KWOK_PRELOAD_BUDGET_SECONDS

# 13. A failed pull that consumes the LAST of the budget must still report its
#     cause. This is a different loop exit from case 4: there the budget check
#     fails before an attempt, here the attempt itself spends the budget and the
#     loop breaks out of the backoff clamp. An earlier version reported the
#     cause on only one of those paths, so this exact case lost the error.
reset
export STUB_PULL_SLOW_FAIL=30
export KWOK_PRELOAD_BUDGET_SECONDS=3
started=$(date +%s)
out=$(preload_image "${IMG}" 2>&1); rc=$?
elapsed=$(( $(date +%s) - started ))
check "budget-consumed-by-pull-does-not-fail-the-lane" 0 "${rc}"
check_bounded "budget-consumed-by-pull-is-bounded" "${KWOK_PRELOAD_BUDGET_SECONDS}" "${elapsed}"
if [[ "${out}" == *"toomanyrequests"* ]]; then
    echo "PASS: budget-consumed-by-pull-reports-the-error"
else
    echo "FAIL: budget-consumed-by-pull-reports-the-error (cause lost on this path: ${out})"
    fails=$((fails + 1))
fi
check_absent "budget-consumed-by-pull-skips-load" "kind load"
unset STUB_PULL_SLOW_FAIL KWOK_PRELOAD_BUDGET_SECONDS

# 14. The image is cached, but the pull that cached it spent the budget doing
#     it. The verdict must answer "is the image there", not "is the image there
#     AND is there time left" — otherwise the run is reported as a failed pull
#     when the pull in fact worked.
#
#     What this does NOT claim: that the image then reaches the Kind node. The
#     side-load has its own budget check and a genuinely exhausted budget still
#     ends in the kubelet pull, which is correct — there is no time left to
#     transfer it. #2502 is a reporting fix, so this asserts the report.
#
#     Drives preload_pull_retry directly for that reason: through preload_image
#     the return value is swallowed by the side-load budget check, so the
#     assertion would prove nothing about the verdict under test.
reset
export STUB_PULL_SLOW_SUCCESS=6
started=$(date +%s)
out=$(preload_pull_retry "${IMG}" "$(( $(date +%s) + 2 ))" 2>&1); rc=$?
elapsed=$(( $(date +%s) - started ))
check "cached-after-budget-spent-reports-success" 0 "${rc}"
# Same ceiling composition as hanging-inspect-is-bounded: the budget plus one
# probe floor. It happens to finish far inside that here because this case's
# stub inspect answers instantly, but the two assertions must not encode two
# different ceilings for the same function.
check_bounded "cached-after-budget-spent-is-bounded" \
    "$(( 2 + PRELOAD_PROBE_TIMEOUT ))" "${elapsed}"
if [[ "${out}" == *"is not cached"* ]]; then
    echo "FAIL: cached-after-budget-spent-does-not-misreport (claimed the cached image is missing: ${out})"
    fails=$((fails + 1))
else
    echo "PASS: cached-after-budget-spent-does-not-misreport"
fi
unset STUB_PULL_SLOW_SUCCESS

# 15. A pull killed by `timeout` writes nothing to stderr, so the captured cause
#     is empty. Selecting the message on that emptiness reports "no pull was
#     attempted" directly beneath the line announcing the attempt. The message
#     must follow the attempt counter, which knows what happened.
reset
export STUB_PULL_HANGS=1
out=$(preload_pull_retry "${IMG}" "$(( $(date +%s) + 3 ))" 2>&1); rc=$?
check "timeout-killed-pull-reports-failure" 1 "${rc}"
# Assert the message POSITIVELY -- the attempt count and the cause. Forbidding
# only the known-wrong string still passes if the message degrades into
# something else uninformative, which is the same vacuity this file exists to
# remove.
if [[ "${out}" == *"after 1 attempt(s)"* && "${out}" == *"killed by the budget timeout"* ]]; then
    echo "PASS: timeout-killed-pull-names-attempt-and-cause"
else
    echo "FAIL: timeout-killed-pull-names-attempt-and-cause (got: ${out})"
    fails=$((fails + 1))
fi
if [[ "${out}" == *"no pull was attempted"* ]]; then
    echo "FAIL: timeout-killed-pull-does-not-claim-no-attempt (contradicts its own attempt log: ${out})"
    fails=$((fails + 1))
fi
unset STUB_PULL_HANGS

# 16. A pull that fails on its own with a nonzero exit and NO stderr must not be
#     blamed on the budget. Empty stderr is ambiguous -- docker produces it too
#     -- so the timeout claim has to come from `timeout`'s own rc 124, not from
#     inferring a cause the code never observed.
reset
export STUB_PULL_SILENT_FAIL=1
out=$(preload_pull_retry "${IMG}" "$(( $(date +%s) + 4 ))" 2>&1); rc=$?
check "silent-pull-failure-reports-failure" 1 "${rc}"
if [[ "${out}" == *"killed by the budget timeout"* ]]; then
    echo "FAIL: silent-pull-failure-is-not-blamed-on-the-budget (got: ${out})"
    fails=$((fails + 1))
else
    echo "PASS: silent-pull-failure-is-not-blamed-on-the-budget"
fi
if [[ "${out}" == *"exited 1 without writing an error"* ]]; then
    echo "PASS: silent-pull-failure-reports-the-exit-code"
else
    echo "FAIL: silent-pull-failure-reports-the-exit-code (got: ${out})"
    fails=$((fails + 1))
fi
unset STUB_PULL_SILENT_FAIL

# 17. A busy but responsive Docker Engine, with plenty of budget left. The
#     probe must use the REMAINING budget when that is larger than its floor:
#     capping every probe at the floor would swap the budget-spent false miss
#     for a slow-daemon one, narrowing an allowance the old code did give.
#
#     CHARACTERISATION, not a regression test. It cannot fail against the
#     pre-change code, which bounded this probe by the remaining deadline and so
#     already allowed the slow inspect. It pins floor-not-cap against a FUTURE
#     change that turns the constant into a ceiling -- a mistake made once
#     already while writing this fix.
reset
export STUB_INSPECT_RC=0     # image IS present
#     6 is the cheapest value that still proves the point: it must exceed the
#     5s floor (or a capped-at-floor probe would survive and the case would
#     stop discriminating), and the image being present means preload_pull_retry
#     pays it twice -- once for the in-loop preload_have_image that breaks
#     immediately, once for the final preload_image_cached. So ~2*
#     STUB_INSPECT_SLOW is the floor on this case's wall-clock, and 6 is as low
#     as it goes without weakening the guard.
export STUB_INSPECT_SLOW=6
out=$(preload_pull_retry "${IMG}" "$(( $(date +%s) + 25 ))" 2>&1); rc=$?
check "slow-but-responsive-probe-is-a-hit" 0 "${rc}"
if [[ "${out}" == *"is not cached"* ]]; then
    echo "FAIL: slow-probe-with-budget-left-is-not-a-miss (probe was capped below the budget: ${out})"
    fails=$((fails + 1))
else
    echo "PASS: slow-probe-with-budget-left-is-not-a-miss"
fi
unset STUB_INSPECT_RC STUB_INSPECT_SLOW

# 18. A failed attempt superseded by a successful one must not be reported as
#     the cause when the final probe still misses. The carried cause has to be
#     cleared on the success break, or the log blames a pull that was retried
#     away -- the same misattribution as reading a timeout out of empty stderr,
#     one iteration further back.
reset
export STUB_PULL_FAILS=1          # attempt 1 fails, writing a real cause
export STUB_INSPECT_ALWAYS_MISS=1 # the image never becomes visible
out=$(preload_pull_retry "${IMG}" "$(( $(date +%s) + 15 ))" 2>&1); rc=$?
check "superseded-failure-reports-failure" 1 "${rc}"
if [[ "${out}" == *"toomanyrequests"* ]]; then
    echo "FAIL: superseded-failure-is-not-blamed (reported a cause a later attempt superseded: ${out})"
    fails=$((fails + 1))
else
    echo "PASS: superseded-failure-is-not-blamed"
fi
if [[ "${out}" == *"reported success but the image is not present"* ]]; then
    echo "PASS: pull-success-without-image-says-so"
else
    echo "FAIL: pull-success-without-image-says-so (got: ${out})"
    fails=$((fails + 1))
fi
unset STUB_PULL_FAILS STUB_INSPECT_ALWAYS_MISS

# 19. A killed pull that emitted only a blank line still has an empty CAUSE, but
#     a non-empty captured string. Testing that string for emptiness lets
#     whitespace out-rank the exit code and prints a blank "last error:".
reset
export STUB_PULL_BLANK_ERR=1
out=$(preload_pull_retry "${IMG}" "$(( $(date +%s) + 3 ))" 2>&1); rc=$?
check "blank-stderr-kill-reports-failure" 1 "${rc}"
if [[ "${out}" == *"killed by the budget timeout"* ]]; then
    echo "PASS: blank-stderr-kill-still-names-the-timeout"
else
    echo "FAIL: blank-stderr-kill-still-names-the-timeout (whitespace out-ranked the exit code: ${out})"
    fails=$((fails + 1))
fi
unset STUB_PULL_BLANK_ERR

if (( fails > 0 )); then
    echo "FAILED: ${fails} case(s)"
    exit 1
fi
echo "All preload-image cases passed"

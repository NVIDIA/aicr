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
    if (( n <= ${STUB_PULL_FAILS:-0} )); then exit 1; fi
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
    unset STUB_INSPECT_HANGS
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
    echo "PASS: retries-exactly-three-times"
fi

# 4. Every pull fails -> rc still 0 and NO side-load attempted. The kubelet
#    fallback must remain, and a missing image must not be "loaded".
reset
export STUB_PULL_FAILS=99
preload_image "${IMG}" >/dev/null; rc=$?
check "pull-exhausted-does-not-fail-the-lane" 0 "${rc}"
check_absent "pull-exhausted-skips-load" "kind load"

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
reset
export STUB_INSPECT_HANGS=1
export KWOK_PRELOAD_BUDGET_SECONDS=3
started=$(date +%s)
preload_image "${IMG}" >/dev/null; rc=$?
elapsed=$(( $(date +%s) - started ))
check "hanging-inspect-does-not-fail-the-lane" 0 "${rc}"
check_bounded "hanging-inspect-is-bounded" "${KWOK_PRELOAD_BUDGET_SECONDS}" "${elapsed}"
check_absent "hanging-inspect-skips-load" "kind load"
unset STUB_INSPECT_HANGS KWOK_PRELOAD_BUDGET_SECONDS

if (( fails > 0 )); then
    echo "FAILED: ${fails} case(s)"
    exit 1
fi
echo "All preload-image cases passed"

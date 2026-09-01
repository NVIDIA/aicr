#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Unit harness for lib/image-cache.sh (pull-once / load-many image cache).
#
# The property under test is the mirror image of preload-image_test.sh. There,
# every case asserts rc=0 because preload_image must never fail the lane. Here,
# image_cache_save is a GATE: the CI prime job runs it once so 127 matrix jobs
# never touch the upstream registry, and a save that reports success without
# producing a usable tarball would send all 127 back to the pull it exists to
# remove -- with the gate lit green. So the interesting assertions are that
# failures are LOUD, and that a partial tarball is never published.
#
# image_cache_load keeps the best-effort contract: a miss returns non-zero for
# the caller to shrug at, because the kubelet pull is still behind it.
#
# Run directly: bash kwok/scripts/lib/image-cache_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The subject expects its caller to supply these (install-infra.sh does); the
# harness supplies its own so log output does not pollute the assertions.
log_info()  { echo "INFO: $*"; }
log_warn()  { echo "WARN: $*"; }
log_debug() { echo "DEBUG: $*"; }

# Resolve the subject SCRIPT_DIR-relative — never a deployed copy.
# shellcheck source=image-cache.sh
source "${SCRIPT_DIR}/image-cache.sh"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

check_rc() { # <name> <want_rc> <got_rc>
    if [[ "$3" == "$2" ]]; then
        pass "$1"
    else
        fail "$1 (want rc=$2, got rc=$3)"
    fi
}

check_rc_nonzero() { # <name> <got_rc>
    if [[ "$2" != "0" ]]; then
        pass "$1"
    else
        fail "$1 (want a non-zero rc, got 0)"
    fi
}

check_trace() { # <name> <must_contain>
    if grep -q -- "$2" "${TRACE}"; then
        pass "$1"
    else
        fail "$1 (trace missing '$2'; trace was: $(tr '\n' '|' < "${TRACE}"))"
    fi
}

check_trace_absent() { # <name> <must_not_contain>
    if grep -q -- "$2" "${TRACE}"; then
        fail "$1 (trace unexpectedly contains '$2'; trace was: $(tr '\n' '|' < "${TRACE}"))"
    else
        pass "$1"
    fi
}

# Asserts the call finished within its CONFIGURED budget rather than some loose
# ceiling, for the same reason preload-image_test.sh does: a generous threshold
# would let a backoff regression push the real ceiling well past the budget and
# still pass. The margin covers process spawn and scheduling only.
BOUND_MARGIN_SECONDS=3
check_bounded() { # <name> <budget> <elapsed>
    if (( $3 <= $2 + BOUND_MARGIN_SECONDS )); then
        pass "$1 ($3s, budget $2s)"
    else
        fail "$1 (took $3s, budget $2s + ${BOUND_MARGIN_SECONDS}s margin)"
    fi
}

STUB_DIR="$(mktemp -d)"
CACHE_DIR="${STUB_DIR}/cache"
TRACE="${STUB_DIR}/trace"
trap 'rm -rf "${STUB_DIR}"' EXIT
PATH="${STUB_DIR}:${PATH}"

# Stub behavior is driven by these, reset before each case. Each must be
# EXPORTED: the stubs are separate processes, so a plain assignment would leave
# them at their defaults and quietly make a case pass over the wrong scenario.
#   STUB_INSPECT_RC   docker image inspect exit code (1 = not cached locally)
#   STUB_PULL_FAILS   number of leading `docker pull` attempts that fail
#   STUB_PULL_HANGS   non-empty makes every `docker pull` hang forever
#   STUB_SAVE_RC      `docker save` exit code
#   STUB_SAVE_PARTIAL non-empty writes bytes to the output path, then fails
#   STUB_SAVE_STALL   non-empty writes bytes to the output path, then stalls,
#                     leaving a save in flight for the caller to kill
#   STUB_LOAD_RC      `docker load` exit code
#   STUB_LOAD_CORRUPT non-empty makes `docker load` succeed without the image
#                     becoming present — a truncated or wrong-image tarball
#   STUB_LOAD_SLOW    seconds a `docker load` stalls AFTER landing the image,
#                     leaving only a sliver of the budget for the probe
#   STUB_INSPECT_SLOW seconds `docker image inspect` stalls before answering,
#                     applied only once the image is present (the post-load
#                     probe) so it cannot spend the budget before the load
write_stubs() {
    cat > "${STUB_DIR}/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "${TRACE}"
case "$1 $2" in
    "image inspect")
        # A busy Docker Engine answers, but not instantly. Applied only once the
        # image is present, which is precisely the post-load probe: slowing the
        # pre-load check too would spend the budget before the load and test a
        # different path. Under a sliver of remaining budget a deadline-bounded
        # probe kills this and calls a present image missing.
        if [[ -n "${STUB_INSPECT_SLOW:-}" && -f "${STUB_DIR}/have" ]]; then
            sleep "${STUB_INSPECT_SLOW}"
        fi
        # Once a pull or load has landed the image is present, so later
        # inspects must succeed too — otherwise no loop could terminate.
        if [[ -f "${STUB_DIR}/have" ]]; then exit 0; fi
        exit "${STUB_INSPECT_RC:-1}"
        ;;
esac
case "$1" in
    pull)
        n=$(( $(cat "${STUB_DIR}/pulls" 2>/dev/null || echo 0) + 1 ))
        echo "${n}" > "${STUB_DIR}/pulls"
        # A pull that never returns — the registry accepts the connection and
        # then goes quiet. Only `timeout` ends this.
        if [[ -n "${STUB_PULL_HANGS:-}" ]]; then sleep 3600; fi
        if (( n <= ${STUB_PULL_FAILS:-0} )); then
            # Real docker writes the cause here; the subject must forward it.
            echo "toomanyrequests: Rate exceeded" >&2
            exit 1
        fi
        touch "${STUB_DIR}/have"
        exit 0
        ;;
    save)
        # docker save -o <file> <image>
        out="$3"
        if [[ -n "${STUB_SAVE_STALL:-}" ]]; then
            # Bytes on disk, then still running: the state the process is in
            # when a runner cancels the job.
            printf 'partial' > "${out}"
            sleep 5
            exit 0
        fi
        if [[ -n "${STUB_SAVE_PARTIAL:-}" ]]; then
            # A save killed midway still leaves bytes on disk. The subject must
            # not publish them as a complete tarball.
            printf 'truncated' > "${out}"
            exit 1
        fi
        if [[ "${STUB_SAVE_RC:-0}" != "0" ]]; then
            echo "no such image" >&2
            exit "${STUB_SAVE_RC}"
        fi
        printf 'tarball-bytes' > "${out}"
        exit 0
        ;;
    load)
        if [[ "${STUB_LOAD_RC:-0}" != "0" ]]; then
            echo "invalid tar header" >&2
            exit "${STUB_LOAD_RC}"
        fi
        # A load that lands the image and then overruns the budget. The image is
        # on the host before the clock runs out, so the post-load verification
        # must answer about the image, not about the time left.
        if [[ -n "${STUB_LOAD_SLOW:-}" ]]; then
            touch "${STUB_DIR}/have"
            sleep "${STUB_LOAD_SLOW}"
            exit 0
        fi
        if [[ -z "${STUB_LOAD_CORRUPT:-}" ]]; then
            touch "${STUB_DIR}/have"
        fi
        exit 0
        ;;
esac
exit 0
EOF
    chmod +x "${STUB_DIR}/docker"
}

reset() {
    : > "${TRACE}"
    rm -rf "${CACHE_DIR}"
    mkdir -p "${CACHE_DIR}"
    rm -f "${STUB_DIR}/pulls" "${STUB_DIR}/have"
    unset STUB_INSPECT_RC STUB_PULL_FAILS STUB_PULL_HANGS
    unset STUB_SAVE_RC STUB_SAVE_PARTIAL STUB_SAVE_STALL STUB_LOAD_RC STUB_LOAD_CORRUPT
    unset STUB_LOAD_SLOW STUB_INSPECT_SLOW
    unset KWOK_IMAGE_CACHE_BUDGET_SECONDS
    export STUB_DIR TRACE
    write_stubs
}

IMG="public.ecr.aws/docker/library/registry:3.1.1"
GITEA="docker.gitea.com/gitea:1.27.0-rootless"

# ── image_cache_key ──────────────────────────────────────────────────────────

# 1. Deterministic. The prime job and the 127 matrix jobs each compute the key
#    independently from .settings.yaml; if the two sides could disagree, every
#    restore would miss and the cache would silently do nothing.
reset
k1=$(image_cache_key "${IMG}")
k2=$(image_cache_key "${IMG}")
if [[ "${k1}" == "${k2}" && -n "${k1}" ]]; then
    pass "key-is-deterministic"
else
    fail "key-is-deterministic (${k1} != ${k2})"
fi

# 2. A tag bump must change the key, or a renovate pin bump would restore the
#    OLD image and the lane would test the wrong version — worse than a miss.
if [[ "$(image_cache_key "${IMG}")" != "$(image_cache_key "public.ecr.aws/docker/library/registry:3.1.2")" ]]; then
    pass "key-changes-with-the-tag"
else
    fail "key-changes-with-the-tag (a pin bump would restore the stale image)"
fi

# 3. Same trailing name from a different registry must not collide. The key is
#    derived from the FULL ref for exactly this reason.
if [[ "$(image_cache_key "${GITEA}")" != "$(image_cache_key "ghcr.io/nvidia/gitea:1.27.0-rootless")" ]]; then
    pass "key-changes-with-the-registry"
else
    fail "key-changes-with-the-registry (two distinct images share a cache entry)"
fi

# 4. GitHub rejects a cache key containing a comma, and anything outside this
#    set is asking for quoting trouble in YAML. The key embeds an image ref
#    full of '/' and ':', so the sanitizing is load-bearing.
if [[ "${k1}" =~ ^[A-Za-z0-9._-]+$ ]]; then
    pass "key-is-safe-for-actions-cache"
else
    fail "key-is-safe-for-actions-cache (got '${k1}')"
fi

# 5. Human-readable: an operator reading the cache list should see which image
#    an entry is for without hashing anything themselves.
if [[ "${k1}" == *"registry"* && "$(image_cache_key "${GITEA}")" == *"gitea"* ]]; then
    pass "key-names-the-image"
else
    fail "key-names-the-image (got '${k1}')"
fi

# 6. Distinct images get distinct tarballs inside one cache dir, and the paths
#    stay under it.
reset
f1=$(image_cache_file "${CACHE_DIR}" "${IMG}")
f2=$(image_cache_file "${CACHE_DIR}" "${GITEA}")
if [[ "${f1}" != "${f2}" && "${f1}" == "${CACHE_DIR}/"* && "${f2}" == "${CACHE_DIR}/"* ]]; then
    pass "file-is-per-image-and-inside-the-dir"
else
    fail "file-is-per-image-and-inside-the-dir (${f1} / ${f2})"
fi

# ── image_cache_save ─────────────────────────────────────────────────────────

# 7. Cold cache: pull once, write the tarball.
reset
image_cache_save "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc "save-cold-succeeds" 0 "${rc}"
check_trace "save-cold-pulls" "docker pull"
check_trace "save-cold-saves" "docker save"
if [[ -s "$(image_cache_file "${CACHE_DIR}" "${IMG}")" ]]; then
    pass "save-cold-writes-the-tarball"
else
    fail "save-cold-writes-the-tarball (no tarball at $(image_cache_file "${CACHE_DIR}" "${IMG}"))"
fi

# 8. Warm cache (actions/cache restored the tarball): no registry contact at
#    all. This is the whole point of #2483 — a warm run must not pull.
reset
printf 'tarball-bytes' > "$(image_cache_file "${CACHE_DIR}" "${IMG}")"
image_cache_save "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc "save-warm-succeeds" 0 "${rc}"
check_trace_absent "save-warm-does-not-pull" "docker pull"
check_trace_absent "save-warm-does-not-resave" "docker save"

# 9. Every pull fails -> LOUD. This is the gate: reporting success here would
#    green-light the prime job and hand all 127 matrix jobs the pull back.
reset
export STUB_PULL_FAILS=99
export KWOK_IMAGE_CACHE_BUDGET_SECONDS=5
out=$(image_cache_save "${CACHE_DIR}" "${IMG}" 2>&1); rc=$?
check_rc_nonzero "save-pull-exhausted-fails" "${rc}"
if [[ "${out}" == *"toomanyrequests"* ]]; then
    pass "save-pull-exhausted-reports-the-error"
else
    fail "save-pull-exhausted-reports-the-error (docker's stderr was not forwarded: ${out})"
fi
if [[ -e "$(image_cache_file "${CACHE_DIR}" "${IMG}")" ]]; then
    fail "save-pull-exhausted-leaves-no-tarball (an empty tarball would be restored as a cache hit)"
else
    pass "save-pull-exhausted-leaves-no-tarball"
fi

# 10. `docker save` fails after writing bytes. The partial file must NOT be
#     published: actions/cache would upload it, every later run would take it
#     as a hit, and `docker load` would fail in all 127 jobs with the prime
#     job green. Publishing only after a complete save is what prevents that.
reset
export STUB_SAVE_PARTIAL=1
export KWOK_IMAGE_CACHE_BUDGET_SECONDS=10
image_cache_save "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "save-partial-fails" "${rc}"
if [[ -e "$(image_cache_file "${CACHE_DIR}" "${IMG}")" ]]; then
    fail "save-partial-publishes-nothing (a truncated tarball would be cached as good)"
else
    pass "save-partial-publishes-nothing"
fi
if compgen -G "${CACHE_DIR}/*.tmp" >/dev/null; then
    fail "save-partial-leaves-no-scratch (a .tmp file would be uploaded with the cache)"
else
    pass "save-partial-leaves-no-scratch"
fi

# 10b. The save is KILLED mid-write — a cancelled job, a runner reclaimed. No
#      cleanup runs, so the only thing standing between the partial bytes and
#      actions/cache is where they were written. Case 10 cannot prove this:
#      there `docker save` exits, so the function's own `rm -f` removes the
#      stump and a non-atomic implementation passes anyway. (Confirmed by
#      mutation: pointing the scratch path at the final path fails only here.)
reset
export STUB_SAVE_STALL=1
export KWOK_IMAGE_CACHE_BUDGET_SECONDS=60
bash "${SCRIPT_DIR}/image-cache.sh" save "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1 &
save_pid=$!
# Wait for the stub to write its bytes, so the kill lands during the save and
# not before it. Bounded so a stub that never writes cannot hang the suite.
for _ in $(seq 1 100); do
    compgen -G "${CACHE_DIR}/*" >/dev/null 2>&1 && break
    sleep 0.1
done
kill -9 "${save_pid}" 2>/dev/null
wait "${save_pid}" 2>/dev/null
if compgen -G "${CACHE_DIR}/*.tmp" >/dev/null 2>&1; then
    pass "save-killed-wrote-to-scratch"
else
    fail "save-killed-wrote-to-scratch (no in-flight save to observe — the case proved nothing)"
fi
if [[ -e "$(image_cache_file "${CACHE_DIR}" "${IMG}")" ]]; then
    fail "save-killed-publishes-nothing (partial bytes sit at the published path; actions/cache would upload them as a good tarball)"
else
    pass "save-killed-publishes-nothing"
fi
unset STUB_SAVE_STALL KWOK_IMAGE_CACHE_BUDGET_SECONDS

# 11. A hanging pull must not hold the prime job open. Same failure class the
#     preload budget exists for; the budget is squeezed so the assertion is
#     about the bound existing, not its production value.
reset
export STUB_PULL_HANGS=1
export KWOK_IMAGE_CACHE_BUDGET_SECONDS=3
started=$(date +%s)
image_cache_save "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
elapsed=$(( $(date +%s) - started ))
check_rc_nonzero "save-hanging-pull-fails" "${rc}"
check_bounded "save-hanging-pull-is-bounded" "${KWOK_IMAGE_CACHE_BUDGET_SECONDS}" "${elapsed}"

# 12. No docker (or no timeout) -> fail closed. preload_image treats missing
#     tooling as a silent no-op because it is best-effort; the prime job cannot,
#     or it would report a cache it never wrote.
reset
mkdir -p "${STUB_DIR}/no-tools"
saved_path="${PATH}"
# shellcheck disable=SC2123  # Replacing PATH is the point: it is what makes
# `command -v docker` fail. Restored two lines down.
PATH="${STUB_DIR}/no-tools"
image_cache_save "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
PATH="${saved_path}"
check_rc_nonzero "save-without-docker-fails" "${rc}"

# ── image_cache_load ─────────────────────────────────────────────────────────

# 13. Tarball present -> load it, no pull.
reset
printf 'tarball-bytes' > "$(image_cache_file "${CACHE_DIR}" "${IMG}")"
image_cache_load "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc "load-succeeds" 0 "${rc}"
check_trace "load-runs-docker-load" "docker load"
check_trace_absent "load-does-not-pull" "docker pull"

# 14. Cache miss -> non-zero, and nothing attempted. Non-fatal by contract: the
#     caller falls back to preload_image, which pulls.
reset
image_cache_load "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "load-miss-reports-miss" "${rc}"
check_trace_absent "load-miss-runs-nothing" "docker load"

# 15. `docker load` exits 0 but the image is still absent — a truncated or
#     wrong-image tarball. Trusting the exit code would report a preloaded
#     image that is not there, and the kubelet pull we were avoiding would run
#     anyway, now unexpectedly. Verify presence, not exit status.
reset
printf 'tarball-bytes' > "$(image_cache_file "${CACHE_DIR}" "${IMG}")"
export STUB_LOAD_CORRUPT=1
image_cache_load "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "load-verifies-the-image-is-present" "${rc}"

# 16. `docker load` itself fails -> non-zero, and no exception thrown at the
#     caller: the lane still has its fallback.
reset
printf 'tarball-bytes' > "$(image_cache_file "${CACHE_DIR}" "${IMG}")"
export STUB_LOAD_RC=1
image_cache_load "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "load-failure-reports-failure" "${rc}"

# 17. An image already present on the host needs no load at all — lanes share a
#     runner within a job.
reset
printf 'tarball-bytes' > "$(image_cache_file "${CACHE_DIR}" "${IMG}")"
export STUB_INSPECT_RC=0
image_cache_load "${CACHE_DIR}" "${IMG}" >/dev/null 2>&1; rc=$?
check_rc "load-already-present-succeeds" 0 "${rc}"
check_trace_absent "load-already-present-skips-docker-load" "docker load"

# 17b. The load succeeds but leaves only a sliver of the budget, and the Engine
#      is busy enough that the presence probe takes a moment. Bounding that
#      probe by the leftover deadline kills it and reports a freshly loaded
#      image as missing -- the caller then re-pulls an image it already has.
#      The probe answers "is it there", so it gets its own bound, not the
#      remainder of a budget that has already done its job.
reset
printf 'tarball-bytes' > "$(image_cache_file "${CACHE_DIR}" "${IMG}")"
#
#      Every boundary here needs a margin, because the checks before the load
#      can let a whole second tick and shrink the load's own timeout. With
#      load == that timeout the load is killed instead of completing, and the
#      case fails intermittently on the load-failed branch without ever
#      reaching the probe it exists to test.
#      Two inequalities have to hold at once, and they pull in opposite
#      directions:
#        leftover-after-load < STUB_INSPECT_SLOW   keeps this a REGRESSION test
#          -- a pre-fix deadline-bound probe must die. Raise the budget too far
#          and the old probe completes too, so the case passes against the buggy
#          code and silently stops guarding anything.
#        STUB_INSPECT_SLOW <= 5                    keeps the FIXED code passing
#          -- the floored probe must be able to finish.
#      Budget 4 buys the load a ~3s margin against its 1s sleep (was ~2s) while
#      leaving ~2-3s leftover, still under STUB_INSPECT_SLOW.
export STUB_LOAD_SLOW=1      # exits 0 with >=3s to spare inside its timeout
export STUB_INSPECT_SLOW=4   # exceeds any leftover budget, inside the 5s probe
export KWOK_IMAGE_CACHE_BUDGET_SECONDS=4
out=$(image_cache_load "${CACHE_DIR}" "${IMG}" 2>&1); rc=$?
check_rc "load-then-slow-probe-still-succeeds" 0 "${rc}"
if [[ "${out}" == *"still not present"* ]]; then
    fail "load-then-slow-probe-is-not-misreported (claimed a loaded image is missing: ${out})"
else
    pass "load-then-slow-probe-is-not-misreported"
fi
unset STUB_LOAD_SLOW STUB_INSPECT_SLOW KWOK_IMAGE_CACHE_BUDGET_SECONDS

# ── image_cache_settings ─────────────────────────────────────────────────────

# 18. Both pins resolve, and each key matches what image_cache_key produces for
#     that ref. The prime job and the matrix jobs both go through this, so a
#     disagreement here is a cache that misses every time while looking healthy.
reset
SETTINGS="${STUB_DIR}/settings.yaml"
cat > "${SETTINGS}" <<EOF
testing_tools:
  registry_image: '${IMG}'
  gitea_image: '${GITEA}'
EOF
out=$(image_cache_settings "${SETTINGS}" 2>&1); rc=$?
check_rc "settings-succeeds" 0 "${rc}"
want="registry_image=${IMG}
registry_key=$(image_cache_key "${IMG}")
gitea_image=${GITEA}
gitea_key=$(image_cache_key "${GITEA}")"
if [[ "${out}" == "${want}" ]]; then
    pass "settings-emits-images-and-matching-keys"
else
    fail "settings-emits-images-and-matching-keys (got: ${out})"
fi

# 19. A renamed or dropped pin must fail, not emit an empty ref. An empty ref
#     hashes fine, so the cache would key on nothing, "hit", and hold no image
#     — a green prime job protecting nothing.
reset
cat > "${SETTINGS}" <<EOF
testing_tools:
  registry_image: '${IMG}'
EOF
image_cache_settings "${SETTINGS}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "settings-missing-pin-fails" "${rc}"

cat > "${SETTINGS}" <<EOF
testing_tools:
  registry_image: ''
  gitea_image: '${GITEA}'
EOF
image_cache_settings "${SETTINGS}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "settings-empty-pin-fails" "${rc}"

image_cache_settings "${STUB_DIR}/does-not-exist.yaml" >/dev/null 2>&1; rc=$?
check_rc_nonzero "settings-missing-file-fails" "${rc}"

# 19b. A ref carrying whitespace would write a $GITHUB_OUTPUT line the runner
#      parses as something other than one key=value pair. Caught here, where
#      the message names the setting.
cat > "${SETTINGS}" <<EOF
testing_tools:
  registry_image: 'public.ecr.aws/docker/library/registry :3.1.1'
  gitea_image: '${GITEA}'
EOF
image_cache_settings "${SETTINGS}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "settings-whitespace-in-pin-fails" "${rc}"

# 20. Against the REAL .settings.yaml. This is the coupling that breaks
#     silently: rename testing_tools.registry_image and every case above still
#     passes on its fixture while CI caches nothing.
reset
REAL_SETTINGS="$(cd "${SCRIPT_DIR}/../../.." && pwd)/.settings.yaml"
if [[ -r "${REAL_SETTINGS}" ]]; then
    out=$(image_cache_settings "${REAL_SETTINGS}" 2>&1); rc=$?
    check_rc "settings-reads-the-real-settings-file" 0 "${rc}"
    if [[ "${out}" == *"registry_image=public.ecr.aws/"* && "${out}" == *"gitea_image=docker.gitea.com/"* ]]; then
        pass "settings-resolves-the-pinned-registries"
    else
        fail "settings-resolves-the-pinned-registries (got: ${out})"
    fi
else
    fail "settings-reads-the-real-settings-file (no .settings.yaml at ${REAL_SETTINGS})"
fi

# ── CLI surface ──────────────────────────────────────────────────────────────

# 18. The workflow calls this file as a command, not as a library. An unknown
#     verb or a missing argument must fail rather than silently no-op, or a
#     typo in the YAML would produce a green job that cached nothing.
reset
out=$(bash "${SCRIPT_DIR}/image-cache.sh" key "${IMG}" 2>&1); rc=$?
check_rc "cli-key-succeeds" 0 "${rc}"
if [[ "${out}" == "${k1}" ]]; then
    pass "cli-key-matches-the-library"
else
    fail "cli-key-matches-the-library (cli '${out}' != lib '${k1}')"
fi

bash "${SCRIPT_DIR}/image-cache.sh" bogus-verb >/dev/null 2>&1; rc=$?
check_rc_nonzero "cli-unknown-verb-fails" "${rc}"

bash "${SCRIPT_DIR}/image-cache.sh" save "${CACHE_DIR}" >/dev/null 2>&1; rc=$?
check_rc_nonzero "cli-missing-argument-fails" "${rc}"

if (( fails > 0 )); then
    echo "FAILED: ${fails} case(s)"
    exit 1
fi
echo "All image-cache cases passed"

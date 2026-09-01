#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# image-cache.sh - pull once, load many: a tarball cache for the public images
# the KWOK lanes need, so a run does not depend on an unauthenticated
# third-party registry pull at job time (#2483).
#
# The problem this solves is a per-IP registry throttle, not an outage. In the
# run that motivated it, 127 concurrent matrix jobs each pulled
# public.ecr.aws/docker/library/registry:3.1.1 at the same moment from the same
# runner egress IPs; 125 succeeded and 1 was shed, reddening main. Retrying
# harder cannot fix a quota shared across the matrix -- the fix is to stop
# making 127 requests. One job pulls and saves a tarball, actions/cache carries
# it, and every matrix job loads from disk.
#
# Used by:
#   .github/workflows/kwok-recipes.yaml  (prime-images job -> `save`)
#   .github/actions/kwok-test/action.yml (matrix jobs      -> `key` and `load`)
#
# Both sides derive the cache key from the same function here rather than from
# a value threaded through the reusable workflow: if the two could disagree,
# every restore would miss and the cache would silently do nothing while
# reporting success.
#
# Sourced by image-cache_test.sh (with stubbed docker); executed directly by
# the workflow. `save` and `load` differ deliberately in how loud they are:
#
#   save  is a GATE. The prime job runs it once so the matrix never touches the
#         registry, so a save that reports success without producing a usable
#         tarball is worse than no cache at all -- it sends all 127 jobs back to
#         the pull with the gate lit green. Every failure path returns non-zero.
#
#   load  is BEST EFFORT, like preload_image. A miss returns non-zero for the
#         caller to shrug at: preload_image still pulls, and the kubelet still
#         pulls behind that. A cache miss must never fail a lane.

IMAGE_CACHE_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# preload_remaining / preload_have_image / preload_image_cached /
# preload_pull_retry. The retry schedule is deliberately shared rather than
# reimplemented: it is the one tuned against the observed throttle (#2496), and
# a second copy here would drift from it.
# shellcheck source=preload-image.sh
source "${IMAGE_CACHE_SCRIPT_DIR}/preload-image.sh"

# Callers that source this file supply log_* (install-infra.sh does, and so
# does the test harness). A direct invocation from the workflow does not, so
# define only what is missing.
declare -F log_info  >/dev/null || log_info()  { echo "[INFO]  $*"; }
declare -F log_warn  >/dev/null || log_warn()  { echo "[WARN]  $*"; }
declare -F log_error >/dev/null || log_error() { echo "[ERROR] $*" >&2; }

# Wall-clock budget for one save or load, across every retry. Generous compared
# to preload_image's 180s because this runs ONCE per workflow run in a job of
# its own, rather than inside a 20-minute lane that still has a recipe to
# validate: spending ten minutes here to spare 127 jobs a throttled pull is a
# good trade, and the pull it is retrying is the one being rate-limited.
#
# Read at CALL time, not source time, so a caller (or a test) can change it
# between invocations.
IMAGE_CACHE_BUDGET_DEFAULT=600

# image_cache_sha256 hashes stdin. sha256sum is GNU/Linux (the CI runners);
# shasum is what macOS ships, and this library runs locally too.
image_cache_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | cut -d' ' -f1
    else
        shasum -a 256 | cut -d' ' -f1
    fi
}

# image_cache_key prints the actions/cache key for IMAGE.
#
# Shape: kwok-image-<name>-<16 hex>. The name is for the human reading the
# cache list; the hash is the actual identity and covers the FULL ref, so a
# tag bump misses (rather than restoring the stale image, which would test the
# wrong version) and two registries hosting the same name do not collide.
#
# The sanitizing is load-bearing: the ref is full of '/' and ':', and GitHub
# rejects a cache key containing a comma.
image_cache_key() {
    local image="$1" name hash
    name="${image%%:*}"   # drop the tag
    name="${name##*/}"    # keep the last path segment
    name="$(printf '%s' "${name}" | tr -c 'a-zA-Z0-9._-' '-' | tr -s '-')"
    hash="$(printf '%s' "${image}" | image_cache_sha256 | cut -c1-16)"
    printf 'kwok-image-%s-%s' "${name}" "${hash}"
}

# image_cache_file prints the tarball path for IMAGE inside DIR. The key is in
# the filename so a restored directory from a previous pin cannot be mistaken
# for a hit on the current one.
image_cache_file() {
    printf '%s/%s.tar' "$1" "$(image_cache_key "$2")"
}

# image_cache_tools_ready reports whether docker and timeout are both usable.
#
# `timeout` is required, not optional, for the same reason preload_image
# requires it: without it every docker call is unbounded and a stalled daemon
# holds the job for its entire budget.
#
# Uses only shell builtins, so it is safe to call before anything else -- the
# caller must not compute a deadline (which needs `date`) on a PATH where the
# tooling is missing.
image_cache_tools_ready() {
    command -v docker &>/dev/null && command -v timeout &>/dev/null
}

# image_cache_save DIR IMAGE ensures DIR holds a loadable tarball of IMAGE,
# pulling it first if the host does not have it. Returns non-zero on every
# failure -- see the gate/best-effort note at the top of this file.
image_cache_save() {
    local dir="$1" image="$2" file tmp budget deadline remaining

    if ! image_cache_tools_ready; then
        log_error "docker or timeout unavailable — cannot prime the image cache for ${image}"
        return 1
    fi

    file="$(image_cache_file "${dir}" "${image}")"
    if [[ -s "${file}" ]]; then
        log_info "Cache hit: ${image} already saved at ${file}"
        return 0
    fi

    if ! mkdir -p "${dir}"; then
        log_error "Could not create the image cache directory ${dir}"
        return 1
    fi

    budget="${KWOK_IMAGE_CACHE_BUDGET_SECONDS:-${IMAGE_CACHE_BUDGET_DEFAULT}}"
    deadline=$(( $(date +%s) + budget ))

    # preload_pull_retry owns the reporting of WHY a pull failed, so this path
    # only has to say what the failure means.
    if ! preload_pull_retry "${image}" "${deadline}"; then
        log_error "Could not pull ${image} within ${budget}s — the KWOK image cache cannot be primed"
        return 1
    fi

    if ! remaining=$(preload_remaining "${deadline}"); then
        log_error "Budget spent before ${image} could be saved"
        return 1
    fi

    # Save to scratch and publish with a rename.
    #
    # A `docker save` killed partway still leaves bytes on disk. Writing
    # straight to the final path would let actions/cache upload that stump,
    # every later run would take it as a hit, and `docker load` would then fail
    # in all 127 jobs -- with this job green, because it did pull the image.
    # Publishing only a complete save is what makes that impossible.
    tmp="${file}.tmp"
    rm -f "${tmp}"
    if ! timeout "${remaining}" docker save -o "${tmp}" "${image}"; then
        log_error "docker save failed for ${image}"
        rm -f "${tmp}"
        return 1
    fi
    if [[ ! -s "${tmp}" ]]; then
        log_error "docker save produced an empty tarball for ${image}"
        rm -f "${tmp}"
        return 1
    fi
    if ! mv "${tmp}" "${file}"; then
        log_error "Could not publish the tarball for ${image} at ${file}"
        rm -f "${tmp}"
        return 1
    fi

    log_info "Saved ${image} to ${file} ($(du -h "${file}" 2>/dev/null | cut -f1))"
    return 0
}

# image_cache_load DIR IMAGE makes IMAGE present in the host Docker cache from
# a previously saved tarball. Returns non-zero when it cannot -- best effort,
# see the note at the top of this file.
#
# A hit here is what removes the registry from the job's critical path:
# preload_image checks the host cache before every pull attempt, so an image
# loaded here means it never pulls at all.
image_cache_load() {
    local dir="$1" image="$2" file budget deadline remaining

    if ! image_cache_tools_ready; then
        log_warn "docker or timeout unavailable — leaving ${image} to the pull path"
        return 1
    fi

    budget="${KWOK_IMAGE_CACHE_BUDGET_SECONDS:-${IMAGE_CACHE_BUDGET_DEFAULT}}"
    deadline=$(( $(date +%s) + budget ))

    if preload_have_image "${image}" "${deadline}"; then
        log_info "${image} is already present on the host — nothing to load"
        return 0
    fi

    file="$(image_cache_file "${dir}" "${image}")"
    if [[ ! -s "${file}" ]]; then
        log_warn "No cached tarball for ${image} at ${file} — the pull path will handle it"
        return 1
    fi

    if ! remaining=$(preload_remaining "${deadline}"); then
        log_warn "Budget spent before ${image} could be loaded"
        return 1
    fi

    if ! timeout "${remaining}" docker load -i "${file}"; then
        log_warn "docker load failed for ${image} from ${file}"
        return 1
    fi

    # Verify the image is actually present rather than trusting the exit code.
    # A truncated or wrong-image tarball can load "successfully" and leave the
    # image absent; reporting that as a hit would tell the caller to skip a
    # pull it still needs.
    #
    # The probe is bounded by the larger of the remaining budget and
    # PRELOAD_PROBE_TIMEOUT, so a spent deadline cannot answer for it. Gating it
    # on the deadline alone reported a successful load that consumed the budget
    # as a failure. Note the bound is a floor, not a cap: while budget remains
    # this probe may wait for most of it, which is deliberate -- capping it
    # would make a slow-but-responsive daemon look like a cache miss instead.
    if ! preload_image_cached "${image}" "${deadline}"; then
        log_warn "${image} is still not present after loading ${file}"
        return 1
    fi

    log_info "Loaded ${image} from ${file}"
    return 0
}

# The public images the KWOK lanes preload. Must match what install-infra.sh
# actually installs -- it reads these same two settings.
IMAGE_CACHE_IMAGES=(registry gitea)

# image_cache_settings reads the pinned image refs out of SETTINGS_FILE and
# prints `<name>_image=` / `<name>_key=` lines, ready to append to
# $GITHUB_OUTPUT.
#
# Exists so the prime job and the 127 matrix jobs resolve images and keys
# through ONE tested code path instead of two copies of yq in two YAML files.
# .settings.yaml stays the single source of truth for the pins.
#
# Fails closed on a missing or empty pin: a renamed setting would otherwise
# yield an empty image ref, a key over the empty string, and a cache that
# reports success while holding nothing.
image_cache_settings() {
    local file="$1" name image

    if ! command -v yq &>/dev/null; then
        log_error "yq is required to read image pins from ${file}"
        return 1
    fi
    if [[ ! -r "${file}" ]]; then
        log_error "Cannot read ${file}"
        return 1
    fi

    for name in "${IMAGE_CACHE_IMAGES[@]}"; do
        image="$(yq eval ".testing_tools.${name}_image" "${file}")"
        if [[ -z "${image}" || "${image}" == "null" ]]; then
            log_error "testing_tools.${name}_image is missing from ${file}"
            return 1
        fi
        # These lines are appended to $GITHUB_OUTPUT, which is line-oriented
        # `name=value`. A ref carrying whitespace — a typo, a stray newline from
        # a folded YAML scalar — would write a line the runner parses as
        # something else entirely. Reject it here, where the message names the
        # setting, rather than downstream where it looks like a cache bug.
        if [[ "${image}" =~ [[:space:]] ]]; then
            log_error "testing_tools.${name}_image in ${file} contains whitespace: '${image}'"
            return 1
        fi
        printf '%s_image=%s\n' "${name}" "${image}"
        printf '%s_key=%s\n' "${name}" "$(image_cache_key "${image}")"
    done
}

image_cache_usage() {
    cat >&2 <<'EOF'
usage: image-cache.sh <verb> [args]

  key      <image>          print the actions/cache key for <image>
  file     <dir> <image>    print the tarball path for <image> inside <dir>
  save     <dir> <image>    ensure <dir> holds a loadable tarball of <image>
  load     <dir> <image>    load <image> into the host Docker cache from <dir>
  settings <settings-file>  print <name>_image= / <name>_key= for each pinned
                            KWOK image, for $GITHUB_OUTPUT
EOF
}

# An unknown verb or a missing argument must fail rather than silently no-op:
# a typo in the workflow YAML would otherwise produce a green job that cached
# nothing, which is the failure this whole library exists to make impossible.
image_cache_main() {
    case "${1:-}" in
        key)
            (( $# == 2 )) || { image_cache_usage; return 2; }
            image_cache_key "$2"
            ;;
        file)
            (( $# == 3 )) || { image_cache_usage; return 2; }
            image_cache_file "$2" "$3"
            ;;
        save)
            (( $# == 3 )) || { image_cache_usage; return 2; }
            image_cache_save "$2" "$3"
            ;;
        load)
            (( $# == 3 )) || { image_cache_usage; return 2; }
            image_cache_load "$2" "$3"
            ;;
        settings)
            (( $# == 2 )) || { image_cache_usage; return 2; }
            image_cache_settings "$2"
            ;;
        *)
            image_cache_usage
            return 2
            ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    set -uo pipefail
    image_cache_main "$@"
fi

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

# Hermetic tests for tools/bump version computation (#3289).
# Run directly: bash tools/bump_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Each case builds a throwaway repository whose `origin` is a local bare
# repository, so `tools/bump` runs end to end (clean-tree and pushed checks,
# tag creation, tag push) without network access. The assertion is on the tag
# that lands on origin.
#
# The behavior under test: major, minor, and patch count from the latest
# STABLE tag, so a leftover pre-release tag of the next version (v1.3.0-rc2)
# cannot shift the result (v1.4.0 instead of v1.3.0). rc counts from the latest
# tag of any kind, because it has to see the RC it is incrementing.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUMP="${SCRIPT_DIR}/bump"

WORK_DIR="$(mktemp -d)" || exit 1
trap 'rm -rf "${WORK_DIR}"' EXIT

# Keep the host's git configuration (signing, hooks, templates) out of the
# fixtures; they must behave the same on a laptop and on a CI runner.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME="bump-test"
export GIT_AUTHOR_EMAIL="bump-test@example.invalid"
export GIT_COMMITTER_NAME="bump-test"
export GIT_COMMITTER_EMAIL="bump-test@example.invalid"

fails=0
case_id=0
fail() {
    echo "FAIL: $*" >&2
    fails=$((fails + 1))
}

# Tag dates matter: when two tags point at the same commit, `git describe`
# prefers the annotated tag with the newer tagger date. Fixture tags therefore
# carry explicit, strictly increasing dates in the past, and tags created by
# tools/bump itself (dated "now") always sort after them, as they do in real
# use.
tag_clock=0

# new_repo creates a fixture and leaves the shell in its working clone.
# Layout: ${WORK_DIR}/<n>/origin.git (bare) and ${WORK_DIR}/<n>/clone.
new_repo() {
    case_id=$((case_id + 1))
    tag_clock=0
    local root="${WORK_DIR}/${case_id}"
    mkdir -p "${root}"
    git init -q --bare "${root}/origin.git"
    git -C "${root}/origin.git" symbolic-ref HEAD refs/heads/main
    git init -q "${root}/clone"
    cd "${root}/clone" || exit 1
    git symbolic-ref HEAD refs/heads/main
    git remote add origin "${root}/origin.git"
    commit "initial"
    git push -q -u origin main
}

# commit adds one commit on main and pushes it.
commit() {
    echo "$1" >>file.txt
    git add file.txt
    git commit -q -m "$1"
    git push -q origin main
}

# tag creates an annotated tag (what tools/bump creates) at HEAD or at "$2",
# and pushes it so origin has it as well.
tag() {
    local name="$1" sha="${2:-HEAD}"
    tag_clock=$((tag_clock + 1))
    GIT_COMMITTER_DATE="2020-01-01T00:00:$(printf '%02d' "${tag_clock}") +0000" \
        git tag -a "${name}" "${sha}" -m "Release ${name}"
    git push -q origin "${name}"
}

# local_tag creates an annotated tag that exists only in this clone.
local_tag() {
    local name="$1"
    tag_clock=$((tag_clock + 1))
    GIT_COMMITTER_DATE="2020-01-01T00:00:$(printf '%02d' "${tag_clock}") +0000" \
        git tag -a "${name}" -m "Release ${name}"
}

# origin_tags prints the tags on origin, sorted, comma-separated.
origin_tags() {
    git -C "$(git remote get-url origin)" tag -l | sort | paste -sd, -
}

# run_bump runs tools/bump from the current fixture. Output is captured in
# BUMP_OUT; the exit code is returned.
run_bump() {
    BUMP_OUT="$("${BUMP}" "$@" 2>&1)"
}

# expect_bump asserts that `tools/bump <args>` succeeds, pushes exactly
# <expected-tag> beyond the tags already on origin, and tags HEAD.
expect_bump() {
    local name="$1" expected="$2"
    shift 2
    local before after
    before="$(origin_tags)"
    if ! run_bump "$@"; then
        fail "${name}: tools/bump $* exited non-zero: ${BUMP_OUT}"
        return
    fi
    after="$(origin_tags)"
    local added
    added="$(comm -13 <(tr ',' '\n' <<<"${before}") <(tr ',' '\n' <<<"${after}") | paste -sd, -)"
    if [[ "${added}" != "${expected}" ]]; then
        fail "${name}: tools/bump $* pushed '${added}', want '${expected}'"
        return
    fi
    if [[ "$(git rev-list -n1 "${expected}")" != "$(git rev-parse HEAD)" ]]; then
        fail "${name}: ${expected} does not point at HEAD"
        return
    fi
    echo "ok: ${name} -> ${expected}"
}

# --- Leftover RC of the next version ------------------------------------------
# History: v1.2.3 (stable), then v1.3.0-rc1 and v1.3.0-rc2 cut from main.
# The RC tags are still present locally, which is exactly the state the
# maintainers used to have to clean up by hand.
leftover_rc_repo() {
    new_repo
    commit "feature a"
    tag v1.2.3
    commit "feature b"
    tag v1.3.0-rc1
    commit "feature c"
    tag v1.3.0-rc2
    commit "feature d"
}

leftover_rc_repo
expect_bump "minor ignores leftover rc" v1.3.0 minor

leftover_rc_repo
expect_bump "patch ignores leftover rc" v1.2.4 patch

# An RC of the next MAJOR is the case where major itself would be shifted
# (v2.0.0-rc1 -> v3.0.0) rather than coinciding with the stable-based answer.
new_repo
commit "feature a"
tag v1.2.3
commit "breaking change"
tag v2.0.0-rc1
commit "feature b"
expect_bump "major ignores leftover major rc" v2.0.0 major

leftover_rc_repo
expect_bump "rc increments the leftover rc" v1.3.0-rc3 rc

# The message names the base the version was computed from.
leftover_rc_repo
run_bump minor
if [[ "${BUMP_OUT}" != *"v1.2.3"*"v1.3.0"* ]]; then
    fail "minor message should read from v1.2.3 to v1.3.0, got: ${BUMP_OUT}"
else
    echo "ok: minor message names the stable base"
fi

# A leftover RC that exists only locally (never pushed) behaves the same.
new_repo
commit "feature a"
tag v1.2.3
commit "feature b"
local_tag v1.3.0-rc1
expect_bump "minor ignores a local-only rc" v1.3.0 minor

# --- No pre-release tags ------------------------------------------------------
new_repo
commit "feature a"
tag v1.2.3
commit "feature b"
expect_bump "minor from stable" v1.3.0 minor

new_repo
commit "feature a"
tag v1.2.3
commit "feature b"
expect_bump "patch from stable" v1.2.4 patch

new_repo
commit "feature a"
tag v1.2.3
commit "feature b"
expect_bump "rc from stable starts the next minor" v1.3.0-rc1 rc

# --- No tags at all -----------------------------------------------------------
new_repo
expect_bump "patch with no tags" v0.0.1 patch

# --- Promotion keeps working, and later bumps count from the new GA ----------
# bump-promote tags the RC's own commit, so work merged after the RC stays out
# of GA. Afterwards v1.3.0 and v1.3.0-rc2 sit on the same commit, and the RC
# tags are deliberately left in place.
RC2_SHA=""
rc_repo() {
    new_repo
    commit "feature a"
    tag v1.2.3
    commit "feature b"
    tag v1.3.0-rc1
    commit "feature c"
    RC2_SHA="$(git rev-parse HEAD)"
    tag v1.3.0-rc2
    commit "after the rc"
}

rc_repo
if ! run_bump promote v1.3.0-rc2; then
    fail "promote: tools/bump promote v1.3.0-rc2 exited non-zero: ${BUMP_OUT}"
elif [[ "$(git rev-list -n1 v1.3.0)" != "${RC2_SHA}" ]]; then
    fail "promote: v1.3.0 does not point at the rc2 commit"
elif ! git ls-remote --exit-code --tags origin refs/tags/v1.3.0 >/dev/null; then
    fail "promote: v1.3.0 was not pushed to origin (origin has $(origin_tags))"
else
    echo "ok: promote tags v1.3.0 on the rc2 commit"
fi

rc_repo
run_bump promote v1.3.0-rc2
expect_bump "patch after promote" v1.3.1 patch

rc_repo
run_bump promote v1.3.0-rc2
expect_bump "minor after promote" v1.4.0 minor

rc_repo
run_bump promote v1.3.0-rc2
expect_bump "rc after promote starts the next minor" v1.4.0-rc1 rc

# --- Argument validation is unchanged -----------------------------------------
new_repo
commit "feature a"
tag v1.2.3
if run_bump promote; then
    fail "promote without a tag should fail"
else
    echo "ok: promote without a tag fails"
fi
if run_bump promote v1.2.3; then
    fail "promote of a stable tag should fail"
else
    echo "ok: promote of a stable tag fails"
fi

# --- Non-version tags are ignored ---------------------------------------------
new_repo
commit "feature a"
tag v1.2.3
commit "feature b"
tag nightly   # not a version tag — must not be selected as the base
expect_bump "minor ignores a non-version tag" v1.3.0 minor

new_repo
commit "feature a"
tag v1.2.3
commit "feature b"
tag nightly
expect_bump "patch ignores a non-version tag" v1.2.4 patch

if ((fails > 0)); then
    echo "${fails} test(s) failed" >&2
    exit 1
fi
echo "All tools/bump tests passed"

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
#     cell uses) must NOT report an unmapped recipe as passed.
#   - Implicit batch invocation (no args, get_recipes() drives the set,
#     as `make kwok-test-all` uses locally) may still SKIP an unmapped
#     recipe with rc=0 so a partly-populated profile tree doesn't turn
#     every dev-loop run red.
#
# Run directly: bash kwok/scripts/run-all-recipes_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

command -v yq >/dev/null 2>&1 || { echo "SKIP: yq not on PATH"; exit 0; }

# Source run-all-recipes.sh without letting main() run. The script's
# tail is the literal line `main "$@"`; strip it so we can source and
# then invoke individual functions with stubs in place.
SCRIPT_UNDER_TEST="${SCRIPT_DIR}/run-all-recipes.sh"
# Keep the temp copy alongside the original so its ${SCRIPT_DIR} / lib
# resolution (which uses BASH_SOURCE) points at the real lib directory.
TMP_SOURCE=$(mktemp "${SCRIPT_DIR}/.run-all-recipes.test.XXXXXX.sh")
trap 'rm -f "${TMP_SOURCE}"' EXIT
grep -v '^main "\$@"$' "${SCRIPT_UNDER_TEST}" > "${TMP_SOURCE}"

# shellcheck disable=SC1090
source "${TMP_SOURCE}"

# Stub cluster-touching helper AFTER sourcing so we win over the real
# definition. Both test cases exit run_recipe_test at the SKIP branch
# before cleanup_between_tests or apply-nodes.sh are reached; the stub
# is defense-in-depth against a future change that shifts either call
# above the SKIP check.
# shellcheck disable=SC2329  # called indirectly via run_recipe_test
cleanup_between_tests() { :; }

# Point OVERLAYS_DIR at the real recipe tree so a known-unmapped recipe
# resolves through resolve_recipe_criteria + select_profiles the same way
# it would in production. Sourced run_recipe_test reads these globals.
# shellcheck disable=SC2034  # read by sourced run_recipe_test
OVERLAYS_DIR="${REPO_ROOT}/recipes/overlays"
# shellcheck disable=SC2034  # read by sourced run_recipe_test
KWOK_DIR="${REPO_ROOT}/kwok"
# shellcheck disable=SC2034  # read by sourced run_recipe_test
DEPLOYER="helm"

# Pick a recipe known to be unmapped on the current tree. gb200-oke-training
# has service=oke, and no oke profile ships on main.
UNMAPPED_RECIPE="gb200-oke-training"
if [[ ! -f "${OVERLAYS_DIR}/${UNMAPPED_RECIPE}.yaml" ]]; then
    echo "FAIL: fixture recipe ${UNMAPPED_RECIPE} not found under ${OVERLAYS_DIR}"
    exit 1
fi

fails=0
check() {
    local name="$1" want_rc_op="$2" want_rc="$3" got_rc="$4"
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

# 1. Explicit invocation — the operator named this recipe on the command
# line, matching what every CI matrix cell does. An unmapped profile
# must fail (rc != 0) rather than be reported as passed.
# shellcheck disable=SC2034  # read by sourced run_recipe_test via is_explicit_recipe
EXPLICIT_RECIPES="${UNMAPPED_RECIPE}"
rc=0
run_recipe_test "${UNMAPPED_RECIPE}" >/dev/null 2>&1 || rc=$?
check "explicit-unmapped-recipe-is-not-reported-as-passed" ne 0 "${rc}"

# 2. Implicit batch invocation — no positional args, recipe came from
# get_recipes(). Historical dev-loop behavior (make kwok-test-all) is
# preserved: unmapped recipes SKIP with rc=0 so a partly-populated
# profile tree doesn't turn every local run red.
# shellcheck disable=SC2034  # read by sourced run_recipe_test via is_explicit_recipe
EXPLICIT_RECIPES=""
rc=0
run_recipe_test "${UNMAPPED_RECIPE}" >/dev/null 2>&1 || rc=$?
check "implicit-batch-unmapped-recipe-is-still-skippable" eq 0 "${rc}"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All 2 tests passed"

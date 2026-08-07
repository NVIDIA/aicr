#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Unit harness for lib/profile-select.sh (label-driven KWOK profile selection).
# Run directly: bash kwok/scripts/lib/profile-select_test.sh
# Wired into CI by the kwok-recipes discover job.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=profile-select.sh
source "${SCRIPT_DIR}/profile-select.sh"

command -v yq >/dev/null 2>&1 || { echo "SKIP: yq not on PATH"; exit 0; }

# Invariant: the SKIP rc must stay distinct from the FATAL rc (1) and
# from success (0). Batch-mode CI relies on the split to swallow SKIP
# but propagate FATAL; a collapse would resurface the false-pass class
# this whole PR exists to eliminate.
if [[ "${PROFILE_SELECT_RC_NO_MATCH}" == "0" || "${PROFILE_SELECT_RC_NO_MATCH}" == "1" ]]; then
    echo "FAIL: PROFILE_SELECT_RC_NO_MATCH must not be 0 or 1 (got ${PROFILE_SELECT_RC_NO_MATCH})"
    exit 1
fi

fails=0
ran=0
# check <name> <want_rc> <want_stdout> <want_stderr_substring> <got_rc> <got_stdout> <got_stderr>
# want_stderr_substring: empty string means "do not check stderr".
check() {
    local name="$1" want_rc="$2" want_out="$3" want_err_sub="$4"
    local got_rc="$5" got_out="$6" got_err="$7"
    ran=$((ran + 1))
    local ok=1
    [[ "${got_rc}" == "${want_rc}" ]] || ok=0
    [[ "${got_out}" == "${want_out}" ]] || ok=0
    if [[ -n "${want_err_sub}" && "${got_err}" != *"${want_err_sub}"* ]]; then
        ok=0
    fi
    if (( ok == 1 )); then
        echo "PASS: ${name}"
    else
        echo "FAIL: ${name}"
        echo "  want: rc=${want_rc} out='${want_out}' err~='${want_err_sub}'"
        echo "  got : rc=${got_rc} out='${got_out}' err='${got_err}'"
        fails=$((fails + 1))
    fi
}

# Fixture root — a self-contained profiles/ tree the tests build up.
FIXTURE_ROOT=$(mktemp -d)
trap 'rm -rf "${FIXTURE_ROOT}"' EXIT

write_profile() {
    local path="$1" provider="$2" nodeType="$3" accelerator="${4:-}"
    local name dir
    name=$(basename "${path}" .yaml)
    dir=$(dirname "${path}")
    mkdir -p "${dir}"
    {
        echo "apiVersion: aicr.run/v1alpha2"
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

# Helper: run select_profiles and capture rc/stdout/stderr separately.
# Sets got_rc, got_out, got_err.
run_select() {
    local err_file
    err_file=$(mktemp)
    got_out=$(select_profiles "$@" 2>"${err_file}")
    got_rc=$?
    got_err=$(cat "${err_file}")
    rm -f "${err_file}"
}

# 1. Happy path: exactly one system + one gpu match.
write_profile "${FIXTURE_ROOT}/eks/system-m7i.yaml" eks system
write_profile "${FIXTURE_ROOT}/eks/p5-h100.yaml"   eks accelerated h100
run_select eks h100 "${FIXTURE_ROOT}"
check "happy-path-eks-h100" 0 "eks/system-m7i.yaml:eks/p5-h100.yaml" "" \
    "${got_rc}" "${got_out}" "${got_err}"

# 2. Happy path with multiple accelerators — still unique per accelerator.
write_profile "${FIXTURE_ROOT}/eks/p6-gb200.yaml" eks accelerated gb200
run_select eks gb200 "${FIXTURE_ROOT}"
check "happy-path-eks-gb200-with-siblings" 0 "eks/system-m7i.yaml:eks/p6-gb200.yaml" "" \
    "${got_rc}" "${got_out}" "${got_err}"

# Tests 3-5 and 8 assert the SKIP outcome (rc = PROFILE_SELECT_RC_NO_MATCH,
# currently 2). Tests 6-7 assert the FATAL outcome (rc = 1) for ambiguous
# matches. The distinction matters: batch-mode CI (run-all-recipes.sh)
# swallows the SKIP rc but must propagate the FATAL rc, otherwise a
# duplicate profile could silently pass CI green.

# 3. Unmapped service — dir does not exist. SKIP outcome.
run_select gke gb300 "${FIXTURE_ROOT}"
check "unmapped-service-fails-and-lists-known-services" "${PROFILE_SELECT_RC_NO_MATCH}" "" "Services with profiles on disk: eks" \
    "${got_rc}" "${got_out}" "${got_err}"

# 4. Unmapped accelerator — SKIP outcome.
run_select eks l40 "${FIXTURE_ROOT}"
check "unmapped-accelerator-fails-and-lists-available" "${PROFILE_SELECT_RC_NO_MATCH}" "" "Accelerators available for service='eks': gb200,h100" \
    "${got_rc}" "${got_out}" "${got_err}"

# 5. Missing system profile — SKIP outcome.
mkdir -p "${FIXTURE_ROOT}/gke"
write_profile "${FIXTURE_ROOT}/gke/a4x-gb300.yaml" gke accelerated gb300
run_select gke gb300 "${FIXTURE_ROOT}"
check "missing-system-profile-fails" "${PROFILE_SELECT_RC_NO_MATCH}" "" "No system profile for service='gke'" \
    "${got_rc}" "${got_out}" "${got_err}"

# 6. Ambiguous system — two nodeType=system profiles. FATAL outcome (rc=1);
# batch mode must NOT swallow this.
write_profile "${FIXTURE_ROOT}/gke/system-n2.yaml"  gke system
write_profile "${FIXTURE_ROOT}/gke/system-n2d.yaml" gke system
run_select gke gb300 "${FIXTURE_ROOT}"
check "ambiguous-system-profile-fails-fatal" 1 "" "Multiple system profiles for service='gke'" \
    "${got_rc}" "${got_out}" "${got_err}"

# 7. Ambiguous GPU (request matches the duplicated accelerator) — FATAL
#    outcome (rc=1). Use a fresh fixture subtree so previous tests don't
#    contaminate arity.
CLEAN_ROOT=$(mktemp -d)
write_profile "${CLEAN_ROOT}/eks/system-m7i.yaml" eks system
write_profile "${CLEAN_ROOT}/eks/p5-h100.yaml"    eks accelerated h100
write_profile "${CLEAN_ROOT}/eks/p5-h100-alt.yaml" eks accelerated h100
run_select eks h100 "${CLEAN_ROOT}"
check "ambiguous-gpu-profile-fails-fatal" 1 "" "accelerator='h100': eks/p5-h100-alt.yaml eks/p5-h100.yaml" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${CLEAN_ROOT}"

# 7b. Ambiguous GPU where the request does NOT match the duplicated
#    label. Two h100 profiles co-exist with a valid gb200 profile; the
#    request is for gb200. Historical (request-scoped) ambiguity check
#    would return rc=0 with the gb200 match, silently hiding the h100
#    tree fault. Tree-scoped check must surface it.
CLEAN_ROOT=$(mktemp -d)
write_profile "${CLEAN_ROOT}/eks/system-m7i.yaml"  eks system
write_profile "${CLEAN_ROOT}/eks/p5-h100.yaml"     eks accelerated h100
write_profile "${CLEAN_ROOT}/eks/p5-h100-alt.yaml" eks accelerated h100
write_profile "${CLEAN_ROOT}/eks/p6-gb200.yaml"    eks accelerated gb200
run_select eks gb200 "${CLEAN_ROOT}"
check "ambiguous-gpu-different-accelerator-still-fails-fatal" 1 "" "accelerator='h100': eks/p5-h100-alt.yaml eks/p5-h100.yaml" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${CLEAN_ROOT}"

# 8. Provider-label mismatch inside the directory — FATAL outcome (rc=1)
#    with a diagnostic naming the offending file. Silently ignoring the file
#    would degrade to an invisible rc=2 no-match (batch SKIP / CI drop) when
#    the mislabeled file is the sole profile for its role, hiding the tree
#    fault. See #1997 follow-up.
CLEAN_ROOT=$(mktemp -d)
write_profile "${CLEAN_ROOT}/gke/system-n2.yaml" gke system
# This file lives under gke/ but declares provider: eks — must be rejected.
write_profile "${CLEAN_ROOT}/gke/wrong-provider.yaml" eks accelerated gb300
run_select gke gb300 "${CLEAN_ROOT}"
check "mislabeled-provider-fails-fatal" 1 "" "gke/wrong-provider.yaml under gke/ declares metadata.labels.provider='eks'" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${CLEAN_ROOT}"

# 9. Argument validation — empty strings.
run_select "" "" ""
check "missing-args-fails" 1 "" "service, accelerator, and profiles_root are all required" \
    "${got_rc}" "${got_out}" "${got_err}"

# 10. Argument validation — no args at all. This runs under the script's
# set -u; the callee must surface its own diagnostic instead of blowing
# up with "unbound variable" on the local="$1" assignment.
run_select
check "no-args-fails-with-diagnostic" 1 "" "service, accelerator, and profiles_root are all required" \
    "${got_rc}" "${got_out}" "${got_err}"

# 11. Nonexistent profiles root.
run_select eks h100 "/nonexistent/${RANDOM}/profiles"
check "nonexistent-profiles-root-fails" 1 "" "Profiles root does not exist" \
    "${got_rc}" "${got_out}" "${got_err}"

# --- resolve_recipe_criteria ---

# Helper: write a recipe overlay with given criteria. Missing values are
# omitted from the YAML so we can exercise the null/default path.
write_overlay() {
    local path="$1" service="${2:-}" accelerator="${3:-}"
    local dir
    dir=$(dirname "${path}")
    mkdir -p "${dir}"
    {
        echo "apiVersion: aicr.run/v1alpha2"
        echo "kind: recipeMetadata"
        echo "metadata:"
        echo "  name: fixture"
        echo "spec:"
        echo "  criteria:"
        [[ -n "${service}" ]] && echo "    service: ${service}"
        [[ -n "${accelerator}" ]] && echo "    accelerator: ${accelerator}"
    } > "${path}"
}

# 12. Explicit criteria pass through verbatim.
OVERLAY_DIR=$(mktemp -d)
trap 'rm -rf "${FIXTURE_ROOT}" "${OVERLAY_DIR}"' EXIT
write_overlay "${OVERLAY_DIR}/explicit.yaml" gke gb200
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/explicit.yaml" 2>/dev/null)
got_rc=$?
check "resolve-explicit-criteria" 0 "gke gb200" "" "${got_rc}" "${got_out}" ""

# 13. Missing fields default to eks/h100 (matches apply-nodes.sh policy).
write_overlay "${OVERLAY_DIR}/missing.yaml"
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/missing.yaml" 2>/dev/null)
got_rc=$?
check "resolve-missing-defaults-to-eks-h100" 0 "eks h100" "" "${got_rc}" "${got_out}" ""

# 14. 'any' placeholder collapses to the same defaults.
write_overlay "${OVERLAY_DIR}/any.yaml" any any
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/any.yaml" 2>/dev/null)
got_rc=$?
check "resolve-any-collapses-to-defaults" 0 "eks h100" "" "${got_rc}" "${got_out}" ""

# 15. Explicit null service + empty-string accelerator — the mixed shape
# hits both normalization branches (!!null via bareword `null`, and !!str
# "" via a quoted empty string). Both must still collapse to defaults;
# write_overlay can't emit these forms so build the fixture directly.
cat > "${OVERLAY_DIR}/null-and-empty.yaml" <<'EOF'
apiVersion: aicr.run/v1alpha2
kind: recipeMetadata
metadata:
  name: fixture
spec:
  criteria:
    service: null
    accelerator: ""
EOF
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/null-and-empty.yaml" 2>/dev/null)
got_rc=$?
check "resolve-null-and-empty-collapse-to-defaults" 0 "eks h100" "" "${got_rc}" "${got_out}" ""

# 16. Nonexistent overlay is an error.
err_file=$(mktemp)
got_out=$(resolve_recipe_criteria "/nonexistent/${RANDOM}.yaml" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "resolve-missing-overlay-fails" 1 "" "Recipe overlay not found" \
    "${got_rc}" "${got_out}" "${got_err}"

# 17. No-args call — must surface the argument-validation diagnostic
# rather than triggering set -u "unbound variable" on $1.
err_file=$(mktemp)
got_out=$(resolve_recipe_criteria 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "resolve-no-args-fails-with-diagnostic" 1 "" "overlay_file is required" \
    "${got_rc}" "${got_out}" "${got_err}"

# 18. Boolean `false` for service — must NOT collapse to "eks" (yq's `//`
# alternative treated false as falsy; this is the false-pass class the
# whole PR exists to eliminate).
write_overlay "${OVERLAY_DIR}/bool-false.yaml" false h100
err_file=$(mktemp)
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/bool-false.yaml" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "resolve-boolean-false-service-rejected" 1 "" "invalid type !!bool" \
    "${got_rc}" "${got_out}" "${got_err}"

# 19. Boolean `true` for accelerator — also rejected. Guards against the
# other side of the falsy heuristic and confirms the type check, not the
# value, is what gates.
write_overlay "${OVERLAY_DIR}/bool-true.yaml" eks true
err_file=$(mktemp)
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/bool-true.yaml" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "resolve-boolean-true-accelerator-rejected" 1 "" "invalid type !!bool" \
    "${got_rc}" "${got_out}" "${got_err}"

# 20. Integer for service — rejected with the invalid-type diagnostic.
write_overlay "${OVERLAY_DIR}/int.yaml" 42 h100
err_file=$(mktemp)
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/int.yaml" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "resolve-integer-service-rejected" 1 "" "invalid type !!int" \
    "${got_rc}" "${got_out}" "${got_err}"

# 21. Malformed YAML — yq evaluation failure propagates as an error rather
# than silently defaulting to eks/h100.
cat > "${OVERLAY_DIR}/malformed.yaml" <<'EOF'
spec:
  criteria:
    service: eks
     accelerator: [unclosed
EOF
err_file=$(mktemp)
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/malformed.yaml" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "resolve-malformed-yaml-fails" 1 "" "malformed YAML" \
    "${got_rc}" "${got_out}" "${got_err}"

# 22. Malformed profile YAML inside the service directory — must fail
# fatally (rc=1) with a diagnostic identifying the bad file, rather
# than silently treating it as a no-match while a valid sibling gets
# selected. Placed under a fresh fixture root so the malformed file
# only affects this test.
BAD_ROOT=$(mktemp -d)
write_profile "${BAD_ROOT}/eks/system-m7i.yaml" eks system
write_profile "${BAD_ROOT}/eks/p5-h100.yaml"    eks accelerated h100
cat > "${BAD_ROOT}/eks/broken.yaml" <<'EOF'
metadata:
  name: broken
  labels:
    provider: eks
     nodeType: accelerated
EOF
run_select eks h100 "${BAD_ROOT}"
check "malformed-profile-fails-fatal" 1 "" "Malformed KWOK profile eks/broken.yaml" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${BAD_ROOT}"

# 23. Unknown nodeType (typo) — must fail fatally rather than being
# silently dropped by a missing case arm. Same coverage-lie class as
# provider-mismatch: a typo'd sole system profile would otherwise
# degrade to invisible rc=2 no-match. Fixture uses `sytem` (missing s)
# with no valid siblings for that role so a silent drop would fall out
# as rc=2 rather than rc=1 without the default arm.
CLEAN_ROOT=$(mktemp -d)
write_profile "${CLEAN_ROOT}/eks/system-typo.yaml" eks sytem
write_profile "${CLEAN_ROOT}/eks/p5-h100.yaml"     eks accelerated h100
run_select eks h100 "${CLEAN_ROOT}"
check "unknown-nodetype-fails-fatal" 1 "" "unrecognized metadata.labels.nodeType='sytem'" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${CLEAN_ROOT}"

# 24. Accelerated profile missing its accelerator label — must fail
# fatally. Previously the profile joined available_accels as "" and,
# if the requested accelerator was also empty (via an odd criteria
# combination), could yield a spurious match; even without that path,
# the empty label silently degrades GPU-role coverage.
CLEAN_ROOT=$(mktemp -d)
write_profile "${CLEAN_ROOT}/eks/system-m7i.yaml"    eks system
# Omit accelerator arg → write_profile skips the label entirely.
write_profile "${CLEAN_ROOT}/eks/p5-no-label.yaml"   eks accelerated
run_select eks h100 "${CLEAN_ROOT}"
check "accelerated-profile-missing-accelerator-label-fails-fatal" 1 "" \
    "eks/p5-no-label.yaml declares nodeType='accelerated' but is missing metadata.labels.accelerator" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${CLEAN_ROOT}"

# 25. Mislabeled system-role profile — complements test 8 (which covers
# a mislabeled accelerated profile). Provider-mismatch is fatal in
# both roles, so a system profile carrying the wrong provider label
# must also surface as rc=1 rather than degrading to rc=2 no-match.
CLEAN_ROOT=$(mktemp -d)
# system profile lives under gke/ but declares provider: eks
write_profile "${CLEAN_ROOT}/gke/wrong-system.yaml" eks system
write_profile "${CLEAN_ROOT}/gke/a3-h100.yaml"      gke accelerated h100
run_select gke h100 "${CLEAN_ROOT}"
check "mislabeled-provider-on-system-role-fails-fatal" 1 "" \
    "gke/wrong-system.yaml under gke/ declares metadata.labels.provider='eks'" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${CLEAN_ROOT}"

# 26. Mixed explicit-service + defaulted-accelerator criteria — the two
# fields normalize independently, so an overlay that names service but
# omits accelerator must return the explicit service verbatim with the
# accelerator defaulted to h100.
write_overlay "${OVERLAY_DIR}/mixed-service-explicit.yaml" gke
got_out=$(resolve_recipe_criteria "${OVERLAY_DIR}/mixed-service-explicit.yaml" 2>/dev/null)
got_rc=$?
check "resolve-explicit-service-defaulted-accelerator" 0 "gke h100" "" "${got_rc}" "${got_out}" ""

# --- read_criteria (CI classify wrapper) ---
#
# The CI classify step calls read_criteria to read overlay criteria
# with the "strict type, skip on missing/any" contract. Cover the two
# axes the workflow relies on: (1) command substitution captures ONLY
# the value on stdout, so annotations never leak into a shell
# variable; (2) failures propagate as rc=1 with a ::error:: annotation
# on stderr so the outer `|| exit 1` guard fires.

# 27. Happy path — a valid explicit string round-trips on stdout and
# emits nothing on stderr, so `v=$(read_criteria ...)` cannot capture
# any spurious content.
write_overlay "${OVERLAY_DIR}/rc-happy.yaml" gke gb200
err_file=$(mktemp)
got_out=$(read_criteria "${OVERLAY_DIR}/rc-happy.yaml" service 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "read_criteria-happy-path-captures-only-value" 0 "gke" "" \
    "${got_rc}" "${got_out}" "${got_err}"
# Assert stderr is genuinely empty (an empty want_err_sub in `check`
# means "do not check stderr"; here we specifically want it empty).
ran=$((ran + 1))
if [[ -n "${got_err}" ]]; then
    echo "FAIL: read_criteria-happy-path-stderr-empty (got: ${got_err})"
    fails=$((fails + 1))
else
    echo "PASS: read_criteria-happy-path-stderr-empty"
fi

# 28. Missing / defaulted criterion — empty stdout, rc=0. Proves the
# caller can distinguish "not set" (empty capture) from "set to a bad
# type" (rc=1) without extra plumbing.
write_overlay "${OVERLAY_DIR}/rc-missing.yaml"
got_out=$(read_criteria "${OVERLAY_DIR}/rc-missing.yaml" service 2>/dev/null)
got_rc=$?
check "read_criteria-missing-returns-empty-string" 0 "" "" \
    "${got_rc}" "${got_out}" ""

# 29. Non-string criterion (`service: false`) — must fail with rc=1,
# emit an ::error:: annotation on stderr, and leave stdout empty so
# `v=$(read_criteria ...)` doesn't paint an empty string over a real
# fault (`|| exit 1` is what surfaces it to the CI job).
write_overlay "${OVERLAY_DIR}/rc-bool.yaml" false h100
err_file=$(mktemp)
got_out=$(read_criteria "${OVERLAY_DIR}/rc-bool.yaml" service 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "read_criteria-non-string-fails-with-annotation" 1 "" \
    "::error file=${OVERLAY_DIR}/rc-bool.yaml::read_criteria_field failed for .spec.criteria.service" \
    "${got_rc}" "${got_out}" "${got_err}"

# --- profile_status (CI classify wrapper) ---
#
# profile_status folds a strict service pre-check + resolve_recipe_criteria
# + select_profiles into a single "is this recipe currently testable?"
# reply used by the classify step. Cover the outcomes it distinguishes:
# unique match, no-match (skippable), ambiguous (fatal), malformed
# criteria (fatal), placeholder criteria, and the generic Tier-1 regression.

# Shared fixtures for the success paths.
PS_ROOT=$(mktemp -d)
trap 'rm -rf "${FIXTURE_ROOT}" "${OVERLAY_DIR}" "${PS_ROOT}"' EXIT
write_profile "${PS_ROOT}/eks/system-m7i.yaml" eks system
write_profile "${PS_ROOT}/eks/p5-h100.yaml"    eks accelerated h100

# 30. Happy path — profile_status echoes "0" (success) to stdout and
# nothing to stderr. Captures behavior classify relies on when
# deciding a recipe belongs in the matrix.
write_overlay "${OVERLAY_DIR}/ps-happy.yaml" eks h100
err_file=$(mktemp)
got_out=$(profile_status "${OVERLAY_DIR}/ps-happy.yaml" "${PS_ROOT}" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "profile_status-happy-path-echoes-0" 0 "0" "" \
    "${got_rc}" "${got_out}" "${got_err}"

# 31. No-match — profile_status echoes PROFILE_SELECT_RC_NO_MATCH (2)
# to stdout and returns rc=0. This is the branch classify treats as
# "drop from matrix"; MUST NOT be conflated with the rc=1 fatal
# branch.
write_overlay "${OVERLAY_DIR}/ps-no-match.yaml" oke gb200
got_out=$(profile_status "${OVERLAY_DIR}/ps-no-match.yaml" "${PS_ROOT}" 2>/dev/null)
got_rc=$?
check "profile_status-no-match-echoes-skip-rc" 0 "${PROFILE_SELECT_RC_NO_MATCH}" "" \
    "${got_rc}" "${got_out}" ""

# 32. Ambiguous — a duplicate system profile makes select_profiles
# return rc=1. profile_status must propagate that as rc=1, emit an
# ::error:: annotation on stderr, and leave stdout empty so the
# caller's `|| exit 1` fires instead of parsing a bogus stdout value.
DUP_ROOT=$(mktemp -d)
write_profile "${DUP_ROOT}/eks/system-a.yaml" eks system
write_profile "${DUP_ROOT}/eks/system-b.yaml" eks system
write_profile "${DUP_ROOT}/eks/p5-h100.yaml"  eks accelerated h100
err_file=$(mktemp)
got_out=$(profile_status "${OVERLAY_DIR}/ps-happy.yaml" "${DUP_ROOT}" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "profile_status-ambiguous-fails-with-annotation-and-empty-stdout" 1 "" \
    "select_profiles failed (rc=1)" \
    "${got_rc}" "${got_out}" "${got_err}"
rm -rf "${DUP_ROOT}"

# 33. Non-string criterion — read_criteria rejects the type and
# profile_status must propagate rc=1 with an ::error:: annotation
# instead of proceeding to select_profiles with garbage input.
write_overlay "${OVERLAY_DIR}/ps-bool.yaml" false h100
err_file=$(mktemp)
got_out=$(profile_status "${OVERLAY_DIR}/ps-bool.yaml" "${PS_ROOT}" 2>"${err_file}")
got_rc=$?
got_err=$(cat "${err_file}"); rm -f "${err_file}"
check "profile_status-invalid-criteria-fails-with-annotation" 1 "" \
    "::error file=${OVERLAY_DIR}/ps-bool.yaml::read_criteria_field failed for .spec.criteria.service" \
    "${got_rc}" "${got_out}" "${got_err}"

# 34. Missing service — the overlay is a template rather than a
# concrete testable recipe. profile_status must echo
# PROFILE_SELECT_RC_NO_MATCH and NOT invoke select_profiles, which
# would otherwise match against the defaulted eks/h100 pair and
# dispatch meaningless coverage. Accelerator is left missing too, but
# the SERVICE emptiness is what triggers no-match (see test 36 for the
# service-set-accel-missing counter-case).
write_overlay "${OVERLAY_DIR}/ps-missing.yaml"
got_out=$(profile_status "${OVERLAY_DIR}/ps-missing.yaml" "${PS_ROOT}" 2>/dev/null)
got_rc=$?
check "profile_status-missing-service-treated-as-no-match" 0 "${PROFILE_SELECT_RC_NO_MATCH}" "" \
    "${got_rc}" "${got_out}" ""

# 35. Explicit "any" service — same treatment as missing. "any" means
# "author didn't target a specific service" and must not be dispatched.
write_overlay "${OVERLAY_DIR}/ps-any.yaml" any any
got_out=$(profile_status "${OVERLAY_DIR}/ps-any.yaml" "${PS_ROOT}" 2>/dev/null)
got_rc=$?
check "profile_status-any-service-treated-as-no-match" 0 "${PROFILE_SELECT_RC_NO_MATCH}" "" \
    "${got_rc}" "${got_out}" ""

# 36. Regression: generic Tier-1 overlay (service set, accelerator
# omitted) MUST remain testable via the h100 default. This is exactly
# the shape of recipes/overlays/eks-inference.yaml and every other
# generic overlay classify assigns to Tier 1; a change that dropped
# these silently would zero Tier-1 coverage in CI. profile_status must
# echo "0" (unique match found) and select_profiles must have been
# invoked with the defaulted (eks, h100) pair.
cat > "${OVERLAY_DIR}/ps-generic-tier1.yaml" <<'EOF'
apiVersion: aicr.run/v1alpha2
kind: recipeMetadata
metadata:
  name: ps-generic-tier1
spec:
  criteria:
    service: eks
    intent: inference
EOF
got_out=$(profile_status "${OVERLAY_DIR}/ps-generic-tier1.yaml" "${PS_ROOT}" 2>/dev/null)
got_rc=$?
check "profile_status-generic-tier1-defaults-accelerator-to-h100" 0 "0" "" \
    "${got_rc}" "${got_out}" ""

if (( fails > 0 )); then
    echo "${fails} test(s) failed (${ran} attempted)"
    exit 1
fi
echo "All ${ran} tests passed"

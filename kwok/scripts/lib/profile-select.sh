#!/usr/bin/env bash
# shellcheck shell=bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Fail-closed KWOK profile selection driven by profile-declared labels.
#
# Sourced by apply-nodes.sh. Selects the (system, gpu) profile pair for
# a given (service, accelerator) recipe criteria by scanning
# kwok/profiles/<service>/ and matching each candidate's
# metadata.labels. The selection is intrinsically validating: a profile
# whose labels disagree with the recipe simply does not match, so the
# fallback path that silently ran arm64 GB300 GKE lanes on amd64 H100
# nodes (#1985) cannot recur.
#
# Match rules (all evaluated against metadata.labels):
#   - provider   MUST equal <service>          (both roles)
#   - nodeType   == "system"                    (system role)
#   - nodeType   == "accelerated" AND
#     accelerator == <accelerator>              (gpu role)
#
# A unique match in each role is required; zero or multiple matches in
# either role is an error, and the diagnostic enumerates what was
# actually found on disk. Requires yq on PATH.
#
# Source guard: constants and functions only, no side effects at source
# time (same contract as lib/sync-budget.sh).

# Dedicated exit code for "no matching profile" — the batch-mode skip
# policy only applies to this outcome. Ambiguous matches, malformed
# args, and other selector failures return 1 so the caller can
# distinguish "recipe has no profile yet" (SKIP) from "something is
# wrong with the profile tree" (FAIL). See run-all-recipes.sh
# run_recipe_test.
readonly PROFILE_SELECT_RC_NO_MATCH=2

# resolve_recipe_criteria <overlay_file>
#
# Prints "<service> <accelerator>" to stdout after applying the same
# defaulting apply-nodes.sh uses: missing/null → eks/h100, and the
# "any" placeholder collapses to the same defaults so KWOK has a
# concrete pair to look up. Kept here so run-all-recipes.sh and
# apply-nodes.sh cannot drift on this policy. Requires yq.
resolve_recipe_criteria() {
    # Use :- default so a no-args call under caller `set -u` surfaces the
    # intended diagnostic instead of an "unbound variable" error.
    local overlay_file="${1:-}"
    if [[ -z "${overlay_file}" ]]; then
        echo "[ERROR] resolve_recipe_criteria: overlay_file is required" >&2
        return 1
    fi
    if [[ ! -f "${overlay_file}" ]]; then
        echo "[ERROR] Recipe overlay not found: ${overlay_file}" >&2
        return 1
    fi

    local svc accel
    svc=$(read_criteria_field "${overlay_file}" service eks) || return 1
    accel=$(read_criteria_field "${overlay_file}" accelerator h100) || return 1

    echo "${svc} ${accel}"
}

# read_criteria_field <overlay_file> <field> <default>
#
# Reads .spec.criteria.<field> and normalizes by yq tag:
#   - !!null / missing        -> <default>
#   - !!str "any" or ""       -> <default>
#   - !!str <other>           -> value verbatim
#   - any other tag (bool, int, ...) -> error with the invalid type
#
# Also propagates yq evaluation failures (malformed YAML) as errors
# instead of the alternative-operator (`//`) silently swallowing them
# into the default value. `false` in particular is falsy under `//`, so
# `service: false` used to collapse to "eks" without a warning.
#
# Callers that want to distinguish "field is placeholder/absent" from
# "field is set" (e.g. the CI classify step deciding dispatchability)
# pass an empty default: the tag validation still runs, so a mistyped
# value errors, but a missing/"any" value returns "" and can be
# short-circuited.
read_criteria_field() {
    local overlay_file="$1" field="$2" default="$3"
    local expr=".spec.criteria.${field}"

    local raw tag
    if ! raw=$(yq eval "${expr}" "${overlay_file}" 2>/dev/null); then
        echo "[ERROR] failed to read ${expr} from ${overlay_file} (malformed YAML?)" >&2
        return 1
    fi
    if ! tag=$(yq eval "${expr} | tag" "${overlay_file}" 2>/dev/null); then
        echo "[ERROR] failed to read tag of ${expr} from ${overlay_file}" >&2
        return 1
    fi

    case "${tag}" in
        '!!null')
            echo "${default}"
            ;;
        '!!str')
            if [[ -z "${raw}" || "${raw}" == "any" ]]; then
                echo "${default}"
            else
                echo "${raw}"
            fi
            ;;
        *)
            echo "[ERROR] ${expr} in ${overlay_file}: invalid type ${tag} (expected a string; got '${raw}')" >&2
            return 1
            ;;
    esac
}

# _read_profile_label <profile_path> <label_key> <profiles_root>
#
# Reads .metadata.labels.<label_key> from the profile and prints the
# value (or "" if the label is absent) on stdout. On yq evaluation
# failure — malformed YAML in the profile itself — prints a diagnostic
# identifying the file and returns 1 so select_profiles can fail
# closed instead of treating the broken file as a silent no-match
# (which would let a typo'd profile hide behind a valid sibling).
_read_profile_label() {
    local profile="$1" label="$2" profiles_root="$3"
    local value
    if ! value=$(yq eval ".metadata.labels.${label} // \"\"" "${profile}" 2>/dev/null); then
        echo "[ERROR] Malformed KWOK profile ${profile#"${profiles_root}"/}: yq failed reading .metadata.labels.${label}" >&2
        return 1
    fi
    echo "${value}"
}

# select_profiles <service> <accelerator> <profiles_root>
#
# On unique match prints "<system_relpath>:<gpu_relpath>" to stdout
# (paths relative to profiles_root). Otherwise prints a diagnostic to
# stderr and returns:
#   - PROFILE_SELECT_RC_NO_MATCH (2) when no profile exists for this
#     (service, accelerator) — safe for batch mode to skip.
#   - 1 for every other selector failure: missing/invalid args,
#     missing profiles_root, or ambiguous matches (>1 system or GPU
#     profile). Batch mode MUST NOT swallow these; a duplicate profile
#     is a real fault the tree must surface.
select_profiles() {
    # Use :- defaults so a no-args call under caller `set -u` surfaces
    # the intended diagnostic instead of an "unbound variable" error.
    local service="${1:-}"
    local accelerator="${2:-}"
    local profiles_root="${3:-}"

    if [[ -z "${service}" || -z "${accelerator}" || -z "${profiles_root}" ]]; then
        echo "[ERROR] select_profiles: service, accelerator, and profiles_root are all required" >&2
        return 1
    fi

    if [[ ! -d "${profiles_root}" ]]; then
        echo "[ERROR] Profiles root does not exist: ${profiles_root}" >&2
        return 1
    fi

    local service_dir="${profiles_root}/${service}"
    if [[ ! -d "${service_dir}" ]]; then
        echo "[ERROR] No KWOK profiles for service='${service}': ${service_dir} does not exist." >&2
        local available="" d
        for d in "${profiles_root}"/*/; do
            [[ -d "${d}" ]] || continue
            d="${d%/}"
            if [[ -z "${available}" ]]; then
                available="${d##*/}"
            else
                available="${available},${d##*/}"
            fi
        done
        if [[ -n "${available}" ]]; then
            echo "[ERROR] Services with profiles on disk: ${available}" >&2
        fi
        return "${PROFILE_SELECT_RC_NO_MATCH}"
    fi

    local system_matches=() gpu_matches=() available_accels=() available_accel_paths=()
    local profile
    while IFS= read -r -d '' profile; do
        local provider nodeType accel relpath
        # yq failures inside the loop propagate — a malformed profile is a
        # broken tree, not a silent no-match. Rc=1 (fatal), not the
        # skippable no-match code.
        provider=$(_read_profile_label "${profile}" provider "${profiles_root}") || return 1
        nodeType=$(_read_profile_label "${profile}" nodeType "${profiles_root}") || return 1
        relpath="${profile#"${profiles_root}"/}"
        # A profile whose provider label disagrees with its parent
        # directory is a tree-integrity fault, not a stray-file defense:
        # if the mislabeled file is the SOLE profile for a role, silently
        # continue'ing here degrades to an invisible rc=2 no-match
        # (batch SKIP / CI drop) instead of surfacing the fault. Fail
        # closed with a diagnostic naming the offending file and the
        # expected provider so the operator can either move the file to
        # kwok/profiles/<provider>/ or fix the label.
        if [[ "${provider}" != "${service}" ]]; then
            echo "[ERROR] KWOK profile ${relpath} under ${service}/ declares metadata.labels.provider='${provider}' — every profile in kwok/profiles/${service}/ must set provider=${service}. Move the file to kwok/profiles/${provider}/ or fix the label." >&2
            return 1
        fi
        case "${nodeType}" in
            system)
                system_matches+=("${relpath}")
                ;;
            accelerated)
                accel=$(_read_profile_label "${profile}" accelerator "${profiles_root}") || return 1
                # An accelerated profile without an accelerator label is
                # a tree fault: the loop would push "" into available_accels
                # and, if the requested accelerator was also empty, hit
                # a spurious match. Fail closed with a diagnostic naming
                # the offending file rather than let a mislabeled profile
                # silently degrade GPU-role coverage.
                if [[ -z "${accel}" ]]; then
                    echo "[ERROR] KWOK profile ${relpath} declares nodeType='accelerated' but is missing metadata.labels.accelerator — every accelerated profile must set an accelerator label so GPU-role matching works." >&2
                    return 1
                fi
                available_accels+=("${accel}")
                available_accel_paths+=("${relpath}")
                if [[ "${accel}" == "${accelerator}" ]]; then
                    gpu_matches+=("${relpath}")
                fi
                ;;
            *)
                # No case arm at all previously — a typo'd nodeType
                # ('sytem', 'accelerate') silently dropped the profile,
                # and if it was the sole profile for a role the fault
                # degraded to an invisible rc=2 no-match (batch SKIP /
                # CI drop). Same coverage-lie the PR targets, via a
                # different field. Same fatal treatment as the
                # provider-mismatch check above.
                echo "[ERROR] KWOK profile ${relpath} has unrecognized metadata.labels.nodeType='${nodeType}' (expected 'system' or 'accelerated')." >&2
                return 1
                ;;
        esac
    done < <(find "${service_dir}" -maxdepth 1 -name '*.yaml' -type f -print0)

    if (( ${#system_matches[@]} == 0 )); then
        echo "[ERROR] No system profile for service='${service}' (need metadata.labels.nodeType=system under ${service_dir})" >&2
        return "${PROFILE_SELECT_RC_NO_MATCH}"
    fi
    if (( ${#system_matches[@]} > 1 )); then
        echo "[ERROR] Multiple system profiles for service='${service}': ${system_matches[*]} — expected exactly one" >&2
        return 1
    fi

    # Tree-scoped duplicate-accelerator check. Runs BEFORE the request-scoped
    # gpu_matches count check below so a duplicate label is surfaced even
    # when the current request wouldn't match it — e.g. two `accelerator=h100`
    # profiles co-exist with a valid `gb200` profile and the request is
    # `gb200`. Without this pass the h100 tree fault stays invisible until
    # someone happens to request the duplicated accelerator.
    if (( ${#available_accels[@]} > 1 )); then
        local dup_labels dup_label i
        dup_labels=$(printf '%s\n' "${available_accels[@]}" | sort | uniq -d)
        if [[ -n "${dup_labels}" ]]; then
            echo "[ERROR] Multiple profiles under ${service_dir} declare the same metadata.labels.accelerator — every accelerator label must appear exactly once across the service directory (tree-integrity check, independent of the requested accelerator):" >&2
            while IFS= read -r dup_label; do
                local dup_files_arr=() dup_files
                for (( i = 0; i < ${#available_accels[@]}; i++ )); do
                    if [[ "${available_accels[i]}" == "${dup_label}" ]]; then
                        dup_files_arr+=("${available_accel_paths[i]}")
                    fi
                done
                # Sort so the diagnostic is deterministic regardless of
                # find(1)'s directory-order return.
                dup_files=$(printf '%s\n' "${dup_files_arr[@]}" | sort | paste -sd' ' -)
                echo "[ERROR]   accelerator='${dup_label}': ${dup_files}" >&2
            done <<< "${dup_labels}"
            return 1
        fi
    fi

    if (( ${#gpu_matches[@]} == 0 )); then
        echo "[ERROR] No GPU profile for service='${service}' accelerator='${accelerator}' under ${service_dir}" >&2
        if (( ${#available_accels[@]} > 0 )); then
            local uniq_accels
            uniq_accels=$(printf '%s\n' "${available_accels[@]}" | sort -u | paste -sd, -)
            echo "[ERROR] Accelerators available for service='${service}': ${uniq_accels}" >&2
        fi
        return "${PROFILE_SELECT_RC_NO_MATCH}"
    fi
    # No request-scoped ambiguity check needed here — the tree-scoped
    # duplicate-accelerator pass above already fails closed on any
    # duplication, including the specific requested-accelerator case.

    echo "${system_matches[0]}:${gpu_matches[0]}"
}

# read_criteria <overlay_file> <field>
#
# Thin wrapper over read_criteria_field with an empty default. Missing /
# !!null / "any" / "" all return "" so the caller can treat them as
# "not testable, skip"; any non-string type (e.g. `service: false`) is
# rejected with a GitHub-Actions ::error:: annotation on stderr and
# rc=1.
#
# Diagnostics go to stderr so command-substitution callers
# (`v=$(read_criteria ...)`) never capture annotation text into the
# value. Callers MUST guard with `|| return 1` (or `|| exit 1`): the
# inner rc only manifests as the substitution rc, so an unchecked
# call silently swallows the failure.
#
# Named for the CI classify step but usable anywhere a strict,
# skip-safe criterion read is needed.
read_criteria() {
    local overlay="${1:-}" field="${2:-}" value
    if [[ -z "${overlay}" || -z "${field}" ]]; then
        echo "[ERROR] read_criteria: overlay and field are required" >&2
        return 1
    fi
    if ! value=$(read_criteria_field "${overlay}" "${field}" "" 2>/dev/null); then
        echo "::error file=${overlay}::read_criteria_field failed for .spec.criteria.${field} — see stderr" >&2
        read_criteria_field "${overlay}" "${field}" "" >&2 || true
        return 1
    fi
    echo "${value}"
}

# profile_status <overlay_file> <profiles_root>
#
# "Is this recipe currently testable?" query for the CI classify step.
# Prints:
#   - "0" (success) on unique match,
#   - "${PROFILE_SELECT_RC_NO_MATCH}" (2) when the recipe is out of
#     scope for the profile tree — the caller can skip / DROP,
#   - nothing on stdout when a tree fault is detected (malformed
#     criteria, malformed profile, ambiguous match, provider mismatch,
#     etc.); an ::error:: annotation is emitted on stderr and the
#     function returns 1.
#
# Service is the testability signal — an overlay with no concrete
# service (missing / !!null / "" / "any") is a placeholder rather than
# a testable recipe. Without this pre-check, resolve_recipe_criteria
# would silently upgrade it to the "eks" default and classify would
# dispatch against coverage the author never targeted. read_criteria
# also tag-validates, so `service: false` fails closed here before the
# default can hide the type error.
#
# Accelerator, in contrast, has a legitimate direct-path default
# (h100): generic Tier-1 overlays (eks-inference, gke-training, ...)
# intentionally set `service` alone and expect the h100 default to
# apply. Delegate resolution (including that default) to
# resolve_recipe_criteria once the service pre-check passes so those
# overlays remain testable.
#
# `return 1` (not `exit 1`) so the function is safe to source and
# call directly from a test harness without terminating the script.
# Callers via $(...) get the same effect: the substitution rc is 1
# and the standard `|| exit 1` / `|| return 1` guard fires.
profile_status() {
    local overlay="${1:-}" profiles_root="${2:-}" svc_raw criteria svc accel rc=0
    if [[ -z "${overlay}" || -z "${profiles_root}" ]]; then
        echo "[ERROR] profile_status: overlay and profiles_root are required" >&2
        return 1
    fi
    svc_raw=$(read_criteria "${overlay}" service) || return 1
    if [[ -z "${svc_raw}" ]]; then
        echo "${PROFILE_SELECT_RC_NO_MATCH}"
        return 0
    fi
    if ! criteria=$(resolve_recipe_criteria "${overlay}" 2>/dev/null); then
        echo "::error file=${overlay}::resolve_recipe_criteria failed — see stderr" >&2
        resolve_recipe_criteria "${overlay}" >&2 || true
        return 1
    fi
    read -r svc accel <<< "${criteria}"
    select_profiles "${svc}" "${accel}" "${profiles_root}" >/dev/null 2>&1 || rc=$?
    if (( rc == 0 || rc == PROFILE_SELECT_RC_NO_MATCH )); then
        echo "${rc}"
        return 0
    fi
    echo "::error file=${overlay}::select_profiles failed (rc=${rc}) for service=${svc} accelerator=${accel} — see stderr" >&2
    select_profiles "${svc}" "${accel}" "${profiles_root}" >&2 || true
    return 1
}

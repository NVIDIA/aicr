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
    svc=$(_read_criteria_field "${overlay_file}" service eks) || return 1
    accel=$(_read_criteria_field "${overlay_file}" accelerator h100) || return 1

    echo "${svc} ${accel}"
}

# _read_criteria_field <overlay_file> <field> <default>
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
_read_criteria_field() {
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

    local system_matches=() gpu_matches=() available_accels=()
    local profile
    while IFS= read -r -d '' profile; do
        local provider nodeType accel relpath
        # yq failures inside the loop propagate — a malformed profile is a
        # broken tree, not a silent no-match. Rc=1 (fatal), not the
        # skippable no-match code.
        provider=$(_read_profile_label "${profile}" provider "${profiles_root}") || return 1
        nodeType=$(_read_profile_label "${profile}" nodeType "${profiles_root}") || return 1
        relpath="${profile#"${profiles_root}"/}"
        # Skip anything whose provider label disagrees with its directory —
        # protects against a mis-copied file living under the wrong service.
        if [[ "${provider}" != "${service}" ]]; then
            continue
        fi
        case "${nodeType}" in
            system)
                system_matches+=("${relpath}")
                ;;
            accelerated)
                accel=$(_read_profile_label "${profile}" accelerator "${profiles_root}") || return 1
                available_accels+=("${accel}")
                if [[ "${accel}" == "${accelerator}" ]]; then
                    gpu_matches+=("${relpath}")
                fi
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

    if (( ${#gpu_matches[@]} == 0 )); then
        echo "[ERROR] No GPU profile for service='${service}' accelerator='${accelerator}' under ${service_dir}" >&2
        if (( ${#available_accels[@]} > 0 )); then
            local uniq_accels
            uniq_accels=$(printf '%s\n' "${available_accels[@]}" | sort -u | paste -sd, -)
            echo "[ERROR] Accelerators available for service='${service}': ${uniq_accels}" >&2
        fi
        return "${PROFILE_SELECT_RC_NO_MATCH}"
    fi
    if (( ${#gpu_matches[@]} > 1 )); then
        echo "[ERROR] Multiple GPU profiles for service='${service}' accelerator='${accelerator}': ${gpu_matches[*]} — expected exactly one" >&2
        return 1
    fi

    echo "${system_matches[0]}:${gpu_matches[0]}"
}

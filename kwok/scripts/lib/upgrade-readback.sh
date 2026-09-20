#!/usr/bin/env bash
# shellcheck shell=bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# Comparison half of the inventory read-back (ADR-021 acceptance criterion
# 4, issue #2531). validate-scheduling.sh runs `aicr upgrade-check` twice
# against the same target, once with `--from cluster` and once with
# `--from <bundle>`, and hands both reports here.
#
# The bundle is the oracle. The recipe.yaml at its root lists exactly the
# releases the bundler emitted, at exactly the versions it pinned, so there
# is no golden to maintain and nothing to drift against: a per-component
# divergence is by construction a defect in the cluster read rather than in
# this comparison.
#
# It lives in its own file so the comparison is unit-testable against
# fixture reports, the negative cases especially, since the dangerous
# failure here is a check that passes on an ambiguous condition rather than
# one that is merely wrong.
#
# Source guard: constants and functions only, no side effects at source
# time (same contract as lib/cleanup.sh).

# What is compared: the per-component rows (which component, what kind of
# change, the `from` and `to` versions, and the verdict with the reason that
# produced it), plus the summary counts.
#
# What is deliberately not, because it differs by construction rather than
# by defect:
#   - the top-level `from`, the literal "cluster" on one side and a bundle
#     path on the other;
#   - `source`, which accounts for a cluster read and is absent from an
#     artifact comparison that contacted no cluster (compare_upgrade_readback
#     asserts on it separately, where its absence means something);
#   - `atRisk`, which a cluster read implies and an artifact comparison does
#     not perform, and whose counts are advisory and depend on whatever else
#     is on the cluster at the moment of the scan.
#
# `jq -S` plus the sort_by makes the rendering canonical, so a diff of two
# projections shows semantic differences only.
readonly READBACK_PROJECTION='{
  components: ([.components[]? | {component, change, from, to, verdict, reason, failsRun}]
               | sort_by(.component)),
  summary: .summary
}'

# readback_installed_components <bundle-recipe> <out-file>
#
# Writes the sorted component names the bundle installed to <out-file>.
# Returns 1 on a missing, unreadable, or component-less bundle recipe: the
# comparison downstream reads this list as "what both sides must have
# found", and an empty one would make every assertion vacuous.
readback_installed_components() {
    local bundle_recipe="${1:-}" out_file="${2:-}"
    if [[ -z "${bundle_recipe}" || -z "${out_file}" ]]; then
        echo "[ERROR] readback_installed_components: <bundle-recipe> and <out-file> are required" >&2
        return 1
    fi
    if [[ ! -f "${bundle_recipe}" ]]; then
        echo "[ERROR] Bundle recipe not found: ${bundle_recipe}" >&2
        echo "[ERROR] Every deployer branch of generate_bundle renders a bundle tree with a" >&2
        echo "[ERROR] recipe.yaml at its root; its absence means no readable artifact was produced." >&2
        return 1
    fi
    if ! yq eval -r '.componentRefs[].name' "${bundle_recipe}" | sort > "${out_file}"; then
        echo "[ERROR] Could not read .componentRefs[].name from ${bundle_recipe}" >&2
        return 1
    fi
    if [[ ! -s "${out_file}" ]]; then
        echo "[ERROR] ${bundle_recipe} declares no components, so neither read has anything to find." >&2
        return 1
    fi
}

# readback_assert_cluster_source <cluster-report> <deployer>
#
# Fails when the cluster report describes a read that recognized nothing.
#
# This is not redundant with the comparison below. A read that matched no
# release reports every component as "added", and so would an artifact read
# whose recipe.yaml went missing, which means two broken reads can agree
# their way to a passing diff. Naming the degenerate report as degenerate
# before comparing anything is what closes that.
readback_assert_cluster_source() {
    local cluster_json="${1:-}" deployer="${2:-unknown}"
    local matched stamped_unmatched

    matched=$(jq -r '.source.matched // -1' "${cluster_json}") || return 1
    if [[ "${matched}" == "-1" ]]; then
        echo "[ERROR] The --from cluster report carries no source block, so no cluster was read." >&2
        echo "[ERROR] Report: ${cluster_json}" >&2
        return 1
    fi
    if (( matched <= 0 )); then
        echo "[ERROR] The cluster read matched ${matched} components; it recognized nothing installed." >&2
        echo "[ERROR] Under deployer=${deployer} that means the release-name mapping in" >&2
        echo "[ERROR] pkg/inventory/read.go matchComponent does not match what this lane installed." >&2
        echo "[ERROR] Report: ${cluster_json}" >&2
        jq -r '.source' "${cluster_json}" >&2 || true
        return 1
    fi

    # Records AICR itself stamped that map to no component. Per the field's
    # own contract anything above zero means the mapping is broken, not that
    # the cluster is bare: AICR wrote those releases and no longer
    # recognizes their names.
    stamped_unmatched=$(jq -r '.source.helm.stampedUnmatched // 0' "${cluster_json}") || return 1
    if (( stamped_unmatched > 0 )); then
        echo "[ERROR] ${stamped_unmatched} Helm record(s) carry an AICR stamp and matched no component." >&2
        echo "[ERROR] Report: ${cluster_json}" >&2
        return 1
    fi
}

# readback_source_summary <cluster-report>
#
# Prints the cluster read's own accounting on stdout, one line per reader.
#
# This is the evidence a passing leg otherwise throws away. The read-back
# exists to validate the per-deployer release-name mapping against a real
# deployment, and a leg that prints only "PASSED" leaves no record that the
# mapping matched anything at all.
#
# Helm and Argo get a line each because they count different units: a
# release retaining ten revisions contributes ten storage records, an
# Application contributes one, so a total over both would be a number that
# means nothing. Argo's line says the stamp check does not apply rather than
# printing a zero, for the reason on pkg/upgrade.ReportSourceArgo: the
# generated Application carries no AICR stamp for the reader to look for.
#
# Always returns 0, printing nothing when the report, the block, or jq is
# unavailable. It runs on the path where the comparison already passed, so
# rendering evidence must not be able to change that verdict.
readback_source_summary() {
    local cluster_json="${1:-}"
    [[ -f "${cluster_json}" ]] || return 0
    # `select` yields nothing for an absent block, so jq exits 0 with no
    # output rather than non-zero: a missing field is silence, not a failure.
    jq -r '
        select(.source != null) | .source |
        "cluster read: matched \(.matched // 0) component(s)",
        ("  helm: \(.helm.records // 0) storage record(s), "
         + "\(.helm.unattributed // 0) unattributed, "
         + "\(.helm.unreadable // 0) unreadable, "
         + "\(.helm.uninstalled // 0) uninstalled, "
         + "\(.helm.stampedUnmatched // 0) stamped but unmatched"),
        ("  argo: \(.argo.applications // 0) application(s), "
         + "\(.argo.unattributed // 0) unattributed, "
         + "\(.argo.unreadable // 0) unreadable, no stamp to check")
    ' "${cluster_json}" 2>/dev/null || return 0
}

# readback_assert_none_added <report> <installed-file> <label>
#
# Fails when <report> calls a component the bundle installed "added". An
# added row for an installed component is a release that side did not see,
# whichever side it is, which is why both reports go through this and not
# only the cluster's.
readback_assert_none_added() {
    local report="${1:-}" installed_file="${2:-}" label="${3:-report}"
    local added_list missing

    added_list="$(dirname "${report}")/added-${label}.txt"
    jq -r '.components[]? | select(.change == "added") | .component' "${report}" \
        | sort > "${added_list}" || return 1

    missing=$(comm -12 "${installed_file}" "${added_list}")
    if [[ -n "${missing}" ]]; then
        echo "[ERROR] ==========================================" >&2
        echo "[ERROR] Inventory read-back FAILED (${label})" >&2
        echo "[ERROR] These components are installed by this bundle but read as new:" >&2
        while IFS= read -r component; do
            [[ -n "${component}" ]] && echo "[ERROR]   - ${component}" >&2
        done <<< "${missing}"
        echo "[ERROR] Report: ${report}" >&2
        echo "[ERROR] ==========================================" >&2
        return 1
    fi
}

# compare_upgrade_readback <artifact-report> <cluster-report> <installed-file> <out-dir> <deployer>
#
# Returns 0 when the two reports agree on every component, 1 otherwise,
# leaving the two projections and a unified diff in <out-dir>.
compare_upgrade_readback() {
    local artifact_json="${1:-}" cluster_json="${2:-}" installed_file="${3:-}"
    local out_dir="${4:-}" deployer="${5:-unknown}"

    if [[ -z "${artifact_json}" || -z "${cluster_json}" || -z "${installed_file}" || -z "${out_dir}" ]]; then
        echo "[ERROR] compare_upgrade_readback: <artifact> <cluster> <installed-file> <out-dir> are required" >&2
        return 1
    fi

    readback_assert_cluster_source "${cluster_json}" "${deployer}" || return 1
    readback_assert_none_added "${artifact_json}" "${installed_file}" "from-artifact" || return 1
    readback_assert_none_added "${cluster_json}" "${installed_file}" "from-cluster" || return 1

    local artifact_proj="${out_dir}/from-artifact.projection.json"
    local cluster_proj="${out_dir}/from-cluster.projection.json"
    if ! jq -S "${READBACK_PROJECTION}" "${artifact_json}" > "${artifact_proj}"; then
        echo "[ERROR] Could not project ${artifact_json}" >&2
        return 1
    fi
    if ! jq -S "${READBACK_PROJECTION}" "${cluster_json}" > "${cluster_proj}"; then
        echo "[ERROR] Could not project ${cluster_json}" >&2
        return 1
    fi

    local diff_file="${out_dir}/projection.diff"
    local diff_rc=0
    diff -u "${artifact_proj}" "${cluster_proj}" > "${diff_file}" || diff_rc=$?
    if (( diff_rc == 0 )); then
        return 0
    fi
    # diff exits 1 for "files differ" and 2+ for "diff itself failed"; the
    # two mean opposite things and only the first is a finding.
    if (( diff_rc != 1 )); then
        echo "[ERROR] diff failed (exit ${diff_rc}) comparing ${artifact_proj} and ${cluster_proj}" >&2
        return 1
    fi

    echo "[ERROR] ==========================================" >&2
    echo "[ERROR] Inventory read-back FAILED (aicr --deployer ${deployer})" >&2
    echo "[ERROR] ==========================================" >&2
    echo "[ERROR] The cluster-derived 'from' table disagrees with the bundle-derived one." >&2
    echo "[ERROR] The bundle is the oracle: it lists exactly the releases this lane installed," >&2
    echo "[ERROR] at exactly the versions it pinned. A divergence is a defect in the cluster" >&2
    echo "[ERROR] read, most likely the release-name mapping for this deployer" >&2
    echo "[ERROR] (pkg/inventory/read.go matchComponent) or the version the reader takes off a" >&2
    echo "[ERROR] record (pkg/inventory/read.go versionFor)." >&2
    echo "[ERROR]" >&2
    echo "[ERROR] --- diff ('-' is what the bundle says, '+' is what the cluster says) ---" >&2
    cat "${diff_file}" >&2
    echo "[ERROR] --- cluster read accounting ---" >&2
    jq -r '.source' "${cluster_json}" >&2 || true
    echo "[ERROR] Reports: ${cluster_json}, ${artifact_json}" >&2
    echo "[ERROR] ==========================================" >&2
    return 1
}

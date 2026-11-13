#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

export HELM_CALLS="${TMP_DIR}/helm-calls"
export HELM_RELEASES
mkdir -p "${TMP_DIR}/bin"

kube_system_names=$(yq -r \
    '.components[] | select(.helm.defaultNamespace == "kube-system") | .name' \
    "${REPO_ROOT}/recipes/registry.yaml")
[[ -n "${kube_system_names}" ]] || { echo "FAIL: registry has no kube-system components"; exit 1; }

kube_system_json=$(jq -Rn '[inputs | select(length > 0) | {name: ., namespace: "kube-system"}]' \
    <<<"${kube_system_names}")
HELM_RELEASES=$(jq -cn --argjson components "${kube_system_json}" '
    $components + [
        {name: "kwok-controller", namespace: "kube-system"},
        {name: "kwok-stage-fast", namespace: "kube-system"},
        {name: "cluster-owned", namespace: "kube-system"},
        {name: "argocd", namespace: "argocd"},
        {name: "recipe-release", namespace: "aicr-kwok-test"}
    ]
')
export HELM_RELEASES

cat >"${TMP_DIR}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "list" ]]; then
    printf '%s\n' "${HELM_RELEASES}"
    exit 0
fi
if [[ "$1" == "get" && "$2" == "values" ]]; then
    if [[ "${SRIOV_RELEASE:-}" == "$3" ]]; then
        printf '{"sriovNetworkOperator":{"enabled":true}}\n'
    else
        printf '{}\n'
    fi
    exit 0
fi
if [[ "$1" == "uninstall" ]]; then
    printf '%s|%s|%s\n' "$2" "$4" "$5" >>"${HELM_CALLS}"
    if [[ " $* " == *" --no-hooks "* ]]; then
        printf '%s\n' "$2" >>"${HELM_CALLS}.no-hooks"
    fi
    if [[ "${FAIL_RELEASE:-}" == "$2" ]]; then
        echo "mock uninstall failure for $2" >&2
        exit 1
    fi
    if [[ "${GONE_RELEASE:-}" == "$2" && " $* " != *" --ignore-not-found "* ]]; then
        echo "Error: uninstall: Release not loaded: $2: release: not found" >&2
        exit 1
    fi
    exit 0
fi
echo "unexpected helm command: $*" >&2
exit 2
EOF
chmod +x "${TMP_DIR}/bin/helm"
export PATH="${TMP_DIR}/bin:${PATH}"

# shellcheck source=cleanup.sh
source "${SCRIPT_DIR}/cleanup.sh"

if ! cleanup_recipe_helm_releases >/dev/null; then
    echo "FAIL: cleanup returned an error"
    exit 1
fi

while IFS= read -r name; do
    [[ -n "${name}" ]] || continue
    if ! grep -Fxq "${name}|kube-system|--wait" "${HELM_CALLS}"; then
        echo "FAIL: registry release ${name} was not uninstalled with --wait"
        exit 1
    fi
done <<<"${kube_system_names}"

grep -Fxq 'recipe-release|aicr-kwok-test|--wait=hookOnly' "${HELM_CALLS}" || {
    echo "FAIL: recipe release did not use --wait=hookOnly"
    exit 1
}
if grep -Eq '^(kwok-controller|kwok-stage-fast|cluster-owned|argocd)\|' "${HELM_CALLS}"; then
    echo "FAIL: cleanup selected a protected release"
    exit 1
fi
if [[ -s "${HELM_CALLS}.no-hooks" ]]; then
    echo "FAIL: cleanup skipped hooks for a release without the SR-IOV subchart"
    exit 1
fi

# A release enabling the sriov-network-operator subchart is uninstalled
# without hooks: its pre-delete Job cannot schedule on KWOK nodes.
: >"${HELM_CALLS}"
export SRIOV_RELEASE=recipe-release
if ! cleanup_recipe_helm_releases >/dev/null; then
    echo "FAIL: cleanup returned an error for the SR-IOV release"
    exit 1
fi
grep -sFxq 'recipe-release' "${HELM_CALLS}.no-hooks" || {
    echo "FAIL: SR-IOV release was not uninstalled with --no-hooks"
    exit 1
}
if grep -Fvx 'recipe-release' "${HELM_CALLS}.no-hooks" | grep -q .; then
    echo "FAIL: cleanup skipped hooks for a release without the SR-IOV subchart"
    exit 1
fi
unset SRIOV_RELEASE
rm -f "${HELM_CALLS}.no-hooks"

# nktp is uninstalled without hooks: its pre-delete Job takes no tolerations.
: >"${HELM_CALLS}"
base_releases=${HELM_RELEASES}
HELM_RELEASES=$(jq -c '. + [{name: "nktp", namespace: "nktp"}]' <<<"${base_releases}")
if ! cleanup_recipe_helm_releases >/dev/null; then
    echo "FAIL: cleanup returned an error for the nktp release"
    exit 1
fi
grep -sFxq 'nktp' "${HELM_CALLS}.no-hooks" || {
    echo "FAIL: nktp release was not uninstalled with --no-hooks"
    exit 1
}
if grep -Fvx 'nktp' "${HELM_CALLS}.no-hooks" | grep -q .; then
    echo "FAIL: cleanup skipped hooks for a release other than nktp"
    exit 1
fi
HELM_RELEASES=${base_releases}
rm -f "${HELM_CALLS}.no-hooks"

# A release another controller already uninstalled (Flux's prune cascade)
# counts as cleaned, not as a failure.
: >"${HELM_CALLS}"
export GONE_RELEASE=recipe-release
if ! cleanup_recipe_helm_releases >/dev/null 2>"${TMP_DIR}/stderr"; then
    echo "FAIL: cleanup failed on a release that was already gone"
    cat "${TMP_DIR}/stderr"
    exit 1
fi
unset GONE_RELEASE

: >"${HELM_CALLS}"
export FAIL_RELEASE=recipe-release
if cleanup_recipe_helm_releases >"${TMP_DIR}/stdout" 2>"${TMP_DIR}/stderr"; then
    echo "FAIL: cleanup succeeded after an uninstall error"
    exit 1
fi
grep -Fq 'mock uninstall failure for recipe-release' "${TMP_DIR}/stderr" || {
    echo "FAIL: Helm uninstall diagnostic was hidden"
    exit 1
}

echo "All cleanup release-selection tests passed"

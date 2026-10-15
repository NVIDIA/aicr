#!/usr/bin/env bash
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
if [[ "$1" == "uninstall" ]]; then
    printf '%s|%s|%s\n' "$2" "$4" "$5" >>"${HELM_CALLS}"
    if [[ "${FAIL_RELEASE:-}" == "$2" ]]; then
        echo "mock uninstall failure for $2" >&2
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

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

# validate-scheduling.sh - Validate bundle scheduling on KWOK cluster
#
# Usage:
#   ./validate-scheduling.sh [--keep-namespace] [--deployer <name>] <recipe-name>
#   ./validate-scheduling.sh h100-eks-ubuntu-training-kubeflow
#   ./validate-scheduling.sh --deployer argocd-oci eks-training
#
# Flags:
#   --keep-namespace    Preserve releases and namespaces after the run for
#                       inspection (skips cleanup).
#   --deployer <name>   Deployer to exercise. One of:
#                         helm             (default — original Helm path)
#                         argocd-oci       (aicr bundle --deployer argocd ->
#                                           OCI push -> kubectl apply
#                                           app-of-apps.yaml)
#                         argocd-helm-oci  (aicr bundle --deployer argocd-helm
#                                           -> OCI push -> helm install OCI
#                                           chart in argocd namespace)
#                         argocd-git       (aicr bundle --deployer argocd ->
#                                           filesystem output -> git push to
#                                           in-cluster Gitea -> kubectl apply
#                                           app-of-apps.yaml and let Argo CD
#                                           clone + reconcile from Git;
#                                           issue #963)
#                         flux-oci         (aicr bundle --deployer flux ->
#                                           OCI push -> apply OCIRepository +
#                                           Kustomization wrappers and let
#                                           Flux reconcile)
#                         flux-git         (aicr bundle --deployer flux ->
#                                           filesystem output -> git push to
#                                           in-cluster Gitea -> apply
#                                           GitRepository + Kustomization
#                                           wrappers and let Flux reconcile;
#                                           issue #963)
#                       For argocd-* values the in-cluster registry + Argo CD
#                       must already be installed; for flux-oci the registry
#                       + Flux 2 controllers must be installed; for flux-git
#                       and argocd-git additionally Gitea. All are managed by
#                       `kwok/scripts/install-infra.sh` keyed on the DEPLOYER
#                       env var.
#
# Environment variables (GitOps deployers only):
#   KWOK_REGISTRY_HOST_PORT  Host port the in-cluster registry is reachable on
#                            from the runner. MUST match the value used when
#                            running install-infra.sh / kind-config.yaml.
#                            Default: 5500 (avoids the macOS ControlCenter
#                            port-5000 collision; Linux CI runners have 5500
#                            free as well).
#   KWOK_ARGOCD_SYNC_TIMEOUT Seconds to wait for all Argo CD Applications to
#                            reach Synced + Healthy (or Progressing) before
#                            failing. Default: 480 (matches the chainsaw test's
#                            spec.timeouts.assert: 8m).
#   KWOK_ARGOCD_ROOT_GRACE   Seconds to wait for the root Argo CD Application
#                            (nvidia-stack for argocd-oci, aicr-stack for
#                            argocd-helm-oci) to appear in the argocd
#                            namespace before failing. A missing root after
#                            this grace window indicates `kubectl apply` /
#                            `helm install` silently produced no Application
#                            resource. Default: 30.
#   KWOK_FLUX_SYNC_TIMEOUT   Seconds to wait for the outer Kustomization +
#                            all HelmReleases to reach a terminal state
#                            before failing. Default: 500.
#   KWOK_FLUX_ROOT_GRACE     Seconds to wait for the outer Kustomization to
#                            appear in the cluster before failing. A missing
#                            Kustomization after the grace window indicates
#                            kubectl apply silently produced no resource
#                            (RBAC denial, CRD missing). Default: 30.
#   KWOK_GITEA_HOST_PORT     (flux-git / argocd-git) Host port the in-cluster
#                            Gitea is reachable on from the runner for
#                            `git push`. MUST match install-infra.sh /
#                            kind-config.yaml. Default: 3300.
#   KWOK_GITEA_USER          (flux-git / argocd-git) Gitea user to push as.
#                            Must match the admin user bootstrapped by
#                            install-infra.sh. Default: aicr.
#   KWOK_GITEA_PASSWORD      (flux-git / argocd-git) Password for KWOK_GITEA_USER.
#                            CI-only credential for the ephemeral in-cluster
#                            Gitea — not a secret. Default: aicr-kwok-ci.
#
# This script:
# 1. Generates a recipe from the cluster config
# 2. Generates a bundle from the recipe
# 3. Deploys the bundle to the KWOK cluster (Helm, Argo CD, or Flux per --deployer)
# 4. Verifies all pods reach Running state (KWOK auto-transitions them)
# 5. Reports success/failure
#
# Exit codes:
#    0  success
#    1  generic failure (bundle gen, deploy, pod scheduling, RBAC, CRD missing,
#       apiserver unreachable, etc.)
#   50  GitOps sync deadline hit (KWOK_ARGOCD_SYNC_TIMEOUT for argocd-*,
#       KWOK_FLUX_SYNC_TIMEOUT for flux-*). Distinct so run-all-recipes.sh
#       can apply the 3-strike rule (ADR-008 §"Error Handling and Failure
#       Modes") without grepping stderr. Only emitted for GitOps deployers;
#       the helm path never returns 50.

set -euo pipefail

# Distinct exit code for Argo CD sync timeout. Documented in the header above
# AND in kwok/scripts/run-all-recipes.sh so callers can branch on it.
readonly EXIT_ARGOCD_SYNC_TIMEOUT=50

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${KWOK_DIR}/.." && pwd)"

# Shared cleanup helpers (SYSTEM_NS_PATTERN constant, ensure_kwok_context
# safety guard). The same constants and functions are sourced by
# run-all-recipes.sh so the system-ns allowlist lives in one place.
# shellcheck source=lib/cleanup.sh
source "${SCRIPT_DIR}/lib/cleanup.sh"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_debug() { echo -e "${BLUE}[DEBUG]${NC} $*"; }

# Use consistent namespace/release names so Helm can upgrade existing resources
NAMESPACE="${KWOK_NAMESPACE:-aicr-kwok-test}"
RELEASE_NAME="${KWOK_RELEASE:-aicr-test}"
WORK_DIR=""
AICR_BIN=""
KEEP_NAMESPACE=false

# Deployer selection (issue #843). Set by --deployer flag in main().
# When DEPLOYER != "helm", generate_bundle pushes to OCI and deploy_bundle
# routes through Argo CD instead of `deploy.sh`. The helm path stays
# byte-identical to pre-#843 behavior.
DEPLOYER="helm"
# OCI ref populated in generate_bundle when DEPLOYER != "helm"; consumed by
# deploy_bundle (helm install path) and cleanup.
OCI_REF=""
# In-cluster pull URL for argocd-* deployers (no tag suffix). Populated in
# generate_bundle and consumed by deploy_bundle for the argocd-helm-oci lane,
# where the wrapper chart's `helm install` must be invoked with
# `--set repoURL=<in-cluster URL>` so the Argo CD Applications it renders
# point repo-server at the in-cluster registry Service DNS, not the
# runner-side localhost:5500 (which is unreachable from inside the cluster).
# The argocd-oci lane derives repoURL via `aicr bundle --repo` at bundle
# time, so it doesn't read this variable. Kept as a script-global instead
# of being re-derived in deploy_bundle so the runner→service-DNS rewrite
# rule lives in exactly one place.
OCI_IN_CLUSTER_REF=""
# Root Argo CD Application name emitted by the bundler. Set per-deployer in
# resolve_argocd_root_app():
#   - argocd-oci      -> "nvidia-stack" (pkg/bundler/deployer/argocd/
#                        templates/app-of-apps.yaml.tmpl)
#   - argocd-git      -> "nvidia-stack" (same --deployer argocd app-of-apps;
#                        only the source transport differs — Git vs OCI)
#   - argocd-helm-oci -> "aicr-stack"   (pkg/bundler/deployer/argocdhelm/
#                        argocdhelm.go: parentAppTemplate)
# Default empty when DEPLOYER=helm; cleanup guards against empty before
# issuing `kubectl delete application`.
ARGOCD_ROOT_APP=""

# Resolve ARGOCD_ROOT_APP from DEPLOYER. Must be called before any code path
# that consults ARGOCD_ROOT_APP (cleanup, wait_for_argocd_sync).
resolve_argocd_root_app() {
    case "$DEPLOYER" in
        argocd-oci|argocd-git) ARGOCD_ROOT_APP="nvidia-stack" ;;
        argocd-helm-oci)       ARGOCD_ROOT_APP="aicr-stack"   ;;
        *)                     ARGOCD_ROOT_APP=""             ;;
    esac
}

# Flux-specific globals (set in generate_bundle / deploy_bundle for flux-*).
# All live in the flux-system namespace (Flux's default control plane).
#
# FLUX_OCIREPOSITORY_NAME is fixed to "aicr-bundle" — the value the bundler
# embeds into local-chart HelmRelease sourceRefs when --deployer flux is
# paired with an OCI --output (see pkg/bundler/config/Config.bundleOCISourceName
# and pkg/cli/bundle.go). Changing the name in one place WITHOUT the other
# breaks chart resolution at reconcile time, so the constant is the contract.
#
# FLUX_GITREPOSITORY_NAME (flux-git) names the OUTER GitRepository this
# script applies at deploy time — the source the wrapper Kustomization
# pulls the bundle tree from. Named "aicr-bundle" for symmetry with the
# OCI lane. Unlike the OCI name there is NO bundler contract on it: with a
# filesystem --output the bundler bakes its OWN GitRepository source CR
# into the bundle (sources/gitrepo-<sanitized-url>.yaml, derived from
# --repo) and local-chart HelmReleases reference THAT object, not this
# one. Two GitRepository objects pointing at the same repo is accepted
# duplication — reproducing the Go sanitizeSourceName() truncation logic
# in bash to merge them would be fragile.
#
# FLUX_KUSTOMIZATION_NAME varies by recipe so back-to-back recipes on the
# same cluster don't collide (one Kustomization per recipe pointing at
# the same shared source over time).
#
# Default empty when DEPLOYER != flux-*; cleanup guards against empty.
FLUX_KUSTOMIZATION_NAME=""
FLUX_OCIREPOSITORY_NAME=""
FLUX_GITREPOSITORY_NAME=""

# Git URLs for the Git-source lanes flux-git / argocd-git (issue #963).
# Same dual-view pattern as OCI_REF / OCI_IN_CLUSTER_REF: the runner pushes
# via the Kind host-port mapping (localhost:3300), while the GitOps
# controller (Flux source-controller or Argo CD repo-server) clones via
# Service DNS from inside the cluster. Populated by compute_gitea_urls()
# in generate_bundle; consumed by deploy_bundle. Script-globals so the
# runner→service-DNS rewrite rule lives in exactly one place.
GIT_PUSH_URL=""
GIT_IN_CLUSTER_URL=""

# Resolve FLUX_* names from the recipe. Must be called before any code path
# that consults them (cleanup, deploy_bundle, wait_for_flux_sync).
resolve_flux_root_names() {
    local recipe="$1"
    FLUX_OCIREPOSITORY_NAME=""
    FLUX_GITREPOSITORY_NAME=""
    FLUX_KUSTOMIZATION_NAME=""
    case "$DEPLOYER" in
        flux-oci)
            # Fixed name — see comment above. Must match what
            # pkg/cli/bundle.go sets as BundleOCISourceName.
            FLUX_OCIREPOSITORY_NAME="aicr-bundle"
            FLUX_KUSTOMIZATION_NAME="aicr-${recipe}"
            ;;
        flux-git)
            FLUX_GITREPOSITORY_NAME="aicr-bundle"
            FLUX_KUSTOMIZATION_NAME="aicr-${recipe}"
            ;;
    esac
}

# Cleanup function
cleanup() {
    local exit_code=$?
    log_info "Cleaning up..."

    if [[ -n "$WORK_DIR" ]] && [[ -d "$WORK_DIR" ]]; then
        rm -rf "$WORK_DIR"
    fi

    if [[ "$KEEP_NAMESPACE" == "true" ]]; then
        log_info "Preserving releases for inspection"
        log_info "Clean up with: helm list -A (then uninstall each release)"
    else
        # Argo CD deployers: delete the root Application FIRST so prune
        # semantics tear down children before we touch the namespaces.
        # The Helm-OCI release (argocd-helm-oci) also needs to come down so
        # the next recipe gets a clean argocd namespace. argocd-git needs no
        # repo cleanup: Gitea state is emptyDir-ephemeral and force-pushed
        # each run, so deleting the root Application is sufficient.
        if [[ "$DEPLOYER" == argocd-* ]]; then
            if [[ -n "$ARGOCD_ROOT_APP" ]]; then
                log_info "Deleting Argo CD root Application ${ARGOCD_ROOT_APP} (prune cascade)..."
                timeout 120s kubectl delete application "$ARGOCD_ROOT_APP" -n argocd \
                    --ignore-not-found --wait=true 2>/dev/null || true
            fi
            if [[ "$DEPLOYER" == "argocd-helm-oci" ]]; then
                # Defensive guard: the install-infra.sh-managed Argo CD release
                # is named "argocd" in the same namespace. RELEASE_NAME-stack
                # must never collide with that — uninstalling the infra release
                # would tear down Argo CD itself and break subsequent recipes.
                local wrapper="${RELEASE_NAME}-stack"
                if [[ "$wrapper" == "argocd" ]]; then
                    log_error "Refusing to uninstall wrapper release named 'argocd'"
                    log_error "It collides with install-infra.sh's Argo CD release."
                    log_error "Override KWOK_RELEASE to something other than empty/'argocd'."
                else
                    log_info "Uninstalling argocd-helm-oci wrapper release ${wrapper}..."
                    log_info "Releases in argocd namespace before uninstall:"
                    helm list -n argocd 2>/dev/null || true
                    timeout 60s helm uninstall "$wrapper" -n argocd --wait 2>/dev/null || true
                fi
            fi
        fi

        # Flux deployers: delete the Kustomization FIRST so its prune
        # GC removes the per-component HelmReleases before namespace
        # cleanup races them. The source (OCIRepository / GitRepository)
        # then has nothing to feed and can be removed safely.
        if [[ "$DEPLOYER" == flux-* ]]; then
            if [[ -n "$FLUX_KUSTOMIZATION_NAME" ]]; then
                log_info "Deleting Flux Kustomization ${FLUX_KUSTOMIZATION_NAME} (prune cascade)..."
                timeout 120s kubectl delete kustomization "$FLUX_KUSTOMIZATION_NAME" \
                    -n flux-system --ignore-not-found --wait=true 2>/dev/null || true
            fi
            if [[ -n "$FLUX_OCIREPOSITORY_NAME" ]]; then
                log_info "Deleting Flux OCIRepository ${FLUX_OCIREPOSITORY_NAME}..."
                timeout 30s kubectl delete ocirepository "$FLUX_OCIREPOSITORY_NAME" \
                    -n flux-system --ignore-not-found --wait=true 2>/dev/null || true
            fi
            if [[ -n "$FLUX_GITREPOSITORY_NAME" ]]; then
                log_info "Deleting Flux GitRepository ${FLUX_GITREPOSITORY_NAME}..."
                timeout 30s kubectl delete gitrepository "$FLUX_GITREPOSITORY_NAME" \
                    -n flux-system --ignore-not-found --wait=true 2>/dev/null || true
                # The bundle also carried an INNER GitRepository source CR
                # (sources/gitrepo-<sanitized-url>.yaml). The Kustomization
                # prune above normally GCs it, but if prune raced the delete
                # it lingers and keeps polling Gitea. Best-effort sweep of
                # any leftovers in flux-system — safe because the flux-git
                # lane owns every GitRepository in that namespace.
                timeout 30s kubectl delete gitrepository --all \
                    -n flux-system --ignore-not-found --wait=true 2>/dev/null || true
            fi
            # Best-effort: clear any HelmReleases the bundle declared in
            # component namespaces. helm-controller's finalizer can hang
            # under KWOK; force-remove if needed before the namespace
            # sweep below force-finalizes the namespace.
            local stale_hrs
            stale_hrs=$(kubectl get helmrelease -A -o json 2>/dev/null \
                | jq -r '.items[] | "\(.metadata.namespace) \(.metadata.name)"' \
                || true)
            if [[ -n "$stale_hrs" ]]; then
                while IFS=' ' read -r ns name; do
                    if [[ -n "$ns" && "$ns" != "flux-system" ]]; then
                        log_info "Deleting HelmRelease ${name} in ${ns}..."
                        timeout 30s kubectl delete helmrelease "$name" -n "$ns" \
                            --ignore-not-found --wait=false 2>/dev/null || true
                        # Strip finalizers if helm-controller's reconciler is
                        # stuck. Merge patch (NOT --type=json remove): RFC 6902
                        # remove errors when the target path is absent, and the
                        # `|| true` would then silently swallow the exact case
                        # this guard exists for — a HelmRelease whose
                        # helm-controller crashed before attaching its
                        # finalizer. Merge patch with `null` is idempotent
                        # whether finalizers are present or absent.
                        kubectl patch helmrelease "$name" -n "$ns" \
                            --type=merge -p '{"metadata":{"finalizers":null}}' \
                            >/dev/null 2>&1 || true
                    fi
                done <<< "$stale_hrs"
            fi
        fi

        # Uninstall all Helm releases from component namespaces (excludes
        # argocd because we want the install-infra.sh-managed release to
        # survive recipe-to-recipe).
        local releases
        releases=$(helm list -A -o json 2>/dev/null \
            | jq -r '.[] | select(.namespace != "kube-system") | select(.namespace != "argocd") | "\(.name) \(.namespace)"' \
            || true)
        if [[ -n "$releases" ]]; then
            while IFS=' ' read -r name ns; do
                if [[ -n "$name" ]]; then
                    log_info "Uninstalling release $name from $ns..."
                    timeout 60s helm uninstall "$name" -n "$ns" --wait 2>/dev/null || true
                fi
            done <<< "$releases"
        fi
        # Clean up stale APIServices before namespace deletion to prevent hangs
        cleanup_stale_apiservices
        # Delete all non-system namespaces (dynamically covers any recipe).
        # `argocd`, `flux-system`, and `aicr-registry` are owned by
        # install-infra.sh and must survive between recipes for the GitOps
        # deployer matrix to work back-to-back. Listed unconditionally
        # (harmless when the corresponding controller isn't installed on
        # the current lane).
        local system_ns="${SYSTEM_NS_PATTERN}"  # see SYSTEM_NS_PATTERN in lib/cleanup.sh
        local test_namespaces
        test_namespaces=$(kubectl get ns -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -vE "^(${system_ns})$" || true)
        # Issue all delete requests asynchronously (--wait=false), then
        # force-finalize any namespace stuck in Terminating.
        #
        # Why force-finalize: KWOK fake controllers can drive Pods through
        # phases but cannot run real finalizer reconcilers (Argo CD's
        # resources-finalizer.argocd.argoproj.io, kubernetes.io/legacy-
        # finalizer, operator-injected finalizers). With --wait=true a
        # `kubectl delete ns` blocks until the namespace's finalizers are
        # cleared — which never happens in KWOK — and the previous
        # --timeout=120s loop spent the full 2 minutes per namespace,
        # exhausting the 15-minute job budget on cleanup *after* the
        # actual recipe validation had already PASSED. Observed: every
        # argocd-oci CI lane on commit c459b51f cancelled in post-test
        # cleanup with `##[error]The operation was canceled.`
        #
        # Force-finalize bypasses the controller protocol by PATCHing the
        # namespace's spec.finalizers to []. This is safe in the KWOK
        # ephemeral cluster: there are no real workloads to leak, and the
        # cluster is destroyed at job end regardless. Do NOT port this
        # pattern to non-KWOK / production cleanup.
        for ns in $test_namespaces; do
            log_info "Deleting namespace $ns..."
            kubectl delete ns "$ns" --ignore-not-found --wait=false 2>/dev/null || true
        done
        # Give graceful deletion a brief window first (real finalizers run
        # in well under a second in tests with no real workloads); then
        # force-finalize anything left over.
        sleep 2
        for ns in $test_namespaces; do
            if kubectl get ns "$ns" >/dev/null 2>&1; then
                log_debug "Force-finalizing stuck namespace: $ns"
                kubectl get ns "$ns" -o json 2>/dev/null \
                    | jq '.spec.finalizers = [] | .metadata.finalizers = []' \
                    | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null 2>&1 \
                    || true
            fi
        done
    fi

    exit $exit_code
}

# Find aicr binary (goreleaser puts it in platform-specific dirs)
find_aicr_binary() {
    # Check common locations in order of preference
    local candidates=(
        "${REPO_ROOT}/dist/aicr"
        "${REPO_ROOT}/dist/aicr_darwin_arm64_v8.0/aicr"
        "${REPO_ROOT}/dist/aicr_darwin_all/aicr"
        "${REPO_ROOT}/dist/aicr_linux_amd64_v1/aicr"
    )

    for candidate in "${candidates[@]}"; do
        if [[ -x "$candidate" ]]; then
            echo "$candidate"
            return 0
        fi
    done

    # Try glob pattern as fallback
    local found
    found=$(find "${REPO_ROOT}/dist" -name "aicr" -type f -perm /111 2>/dev/null | head -1)
    if [[ -n "$found" ]]; then
        echo "$found"
        return 0
    fi

    return 1
}

# Check dependencies
check_deps() {
    local missing=()
    local required=(kubectl helm yq jq)
    # flux-git pushes the filesystem bundle to the in-cluster Gitea.
    [[ "$DEPLOYER" == "flux-git" ]] && required+=(git)
    for cmd in "${required[@]}"; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done

    # Check for aicr binary
    AICR_BIN=$(find_aicr_binary) || {
        log_error "aicr binary not found in dist/"
        log_error "Run 'make build' first"
        exit 1
    }
    log_info "Using aicr binary: $AICR_BIN"

    if [[ ${#missing[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing[*]}"
        exit 1
    fi
}

# Force-remove finalizers from a stuck namespace
force_delete_namespace() {
    local ns="$1"
    log_warn "Force-removing finalizers from stuck namespace $ns"
    kubectl get ns "$ns" -o json 2>/dev/null | \
        jq '.spec.finalizers = []' | \
        kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null 2>&1 || true
}

# Clean up stale APIServices that can cause namespace deletion to hang
# prometheus-adapter creates these and they become stale when the adapter is deleted
cleanup_stale_apiservices() {
    local stale_apis=(
        "v1beta1.custom.metrics.k8s.io"
        "v1beta1.external.metrics.k8s.io"
    )

    for api in "${stale_apis[@]}"; do
        if kubectl get apiservice "$api" &>/dev/null; then
            # Check if the APIService is unavailable (stale)
            local available
            available=$(kubectl get apiservice "$api" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || echo "Unknown")
            if [[ "$available" != "True" ]]; then
                log_info "Removing stale APIService: $api"
                kubectl delete apiservice "$api" --ignore-not-found 2>/dev/null || true
            fi
        fi
    done
}

# Wait for a namespace to fully terminate
wait_for_namespace_gone() {
    local ns="$1"
    local max_wait="${2:-120}"
    local force_after="${3:-60}"  # Force-delete after this many seconds
    local waited=0
    local force_attempted=false

    while kubectl get ns "$ns" &>/dev/null; do
        if [[ $waited -ge $max_wait ]]; then
            log_warn "Timeout waiting for namespace $ns to terminate"
            return 1
        fi

        # After force_after seconds, try force-deleting if namespace is stuck
        if [[ $waited -ge $force_after ]] && [[ "$force_attempted" == "false" ]]; then
            local ns_status
            ns_status=$(kubectl get ns "$ns" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
            if [[ "$ns_status" == "Terminating" ]]; then
                # Check if namespace is empty (stuck on finalizer with no resources)
                local resource_count
                resource_count=$(kubectl api-resources --verbs=list --namespaced -o name 2>/dev/null | \
                    xargs -n 1 kubectl get -n "$ns" --ignore-not-found --no-headers 2>/dev/null | wc -l | tr -d ' ')
                if [[ "$resource_count" -eq 0 ]]; then
                    force_delete_namespace "$ns"
                    force_attempted=true
                fi
            fi
        fi

        log_info "Waiting for namespace $ns to terminate ($waited/${max_wait}s)..."
        sleep 5
        waited=$((waited + 5))
    done
    return 0
}

# Cleanup old test artifacts from previous runs
cleanup_old_tests() {
    log_info "Cleaning up old test artifacts..."

    # First, wait for any currently terminating namespaces to finish
    # This handles the case where a previous run's cleanup trap is still in progress
    if kubectl get ns "$NAMESPACE" &>/dev/null; then
        local ns_status
        ns_status=$(kubectl get ns "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        if [[ "$ns_status" == "Terminating" ]]; then
            log_info "Namespace $NAMESPACE is terminating from previous run, waiting..."
            wait_for_namespace_gone "$NAMESPACE" 120
        fi
    fi

    # Find and uninstall old Helm releases from component namespaces.
    # Skip the argocd namespace so the install-infra.sh-managed Argo CD
    # release survives recipe-to-recipe.
    local releases
    releases=$(helm list -A -o json 2>/dev/null \
        | jq -r '.[] | select(.namespace != "kube-system") | select(.namespace != "argocd") | "\(.namespace) \(.name)"' \
        || true)
    if [[ -n "$releases" ]]; then
        log_info "Uninstalling old releases..."
        echo "$releases" | while read -r ns release; do
            if [[ -n "$release" ]]; then
                log_info "  Uninstalling $release from $ns..."
                helm uninstall "$release" -n "$ns" --wait 2>/dev/null || true
            fi
        done
    fi

    # Clean up stale APIServices left by prometheus-adapter
    # These can cause namespace deletion to hang with "stale GroupVersion discovery" errors
    cleanup_stale_apiservices

    # Delete all non-system namespaces (dynamically covers any recipe).
    # `argocd` and `aicr-registry` are owned by install-infra.sh and must
    # survive between recipes.
    #
    # See the long comment in cleanup() for the rationale: --wait=true on
    # a KWOK cluster blocks the full --timeout=120s per namespace because
    # KWOK can't run real finalizers (Argo CD's resources-finalizer,
    # operator-injected finalizers, etc.). Issue async + force-finalize.
    local system_ns="${SYSTEM_NS_PATTERN}"  # see SYSTEM_NS_PATTERN in lib/cleanup.sh
    local test_namespaces
    test_namespaces=$(kubectl get ns -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -vE "^(${system_ns})$" || true)
    for ns in $test_namespaces; do
        log_info "Removing namespace $ns..."
        kubectl delete ns "$ns" --ignore-not-found --wait=false 2>/dev/null || true
    done
    sleep 2
    for ns in $test_namespaces; do
        if kubectl get ns "$ns" >/dev/null 2>&1; then
            log_debug "Force-finalizing stuck namespace: $ns"
            kubectl get ns "$ns" -o json 2>/dev/null \
                | jq '.spec.finalizers = [] | .metadata.finalizers = []' \
                | kubectl replace --raw "/api/v1/namespaces/${ns}/finalize" -f - >/dev/null 2>&1 \
                || true
        fi
    done

    # Also clean up legacy aicr-kwok-test namespaces (async + force-finalize).
    local old_namespaces
    old_namespaces=$(kubectl get ns -o name 2>/dev/null | grep "namespace/aicr-kwok-test" || true)
    if [[ -n "$old_namespaces" ]]; then
        log_info "Removing old test namespaces..."
        echo "$old_namespaces" | xargs kubectl delete --wait=false 2>/dev/null || true
        sleep 2
        # Force-finalize any of those that linger.
        while read -r ns_ref; do
            local ns_name="${ns_ref#namespace/}"
            if [[ -z "$ns_name" ]]; then continue; fi
            if kubectl get ns "$ns_name" >/dev/null 2>&1; then
                log_debug "Force-finalizing legacy namespace: $ns_name"
                kubectl get ns "$ns_name" -o json 2>/dev/null \
                    | jq '.spec.finalizers = [] | .metadata.finalizers = []' \
                    | kubectl replace --raw "/api/v1/namespaces/${ns_name}/finalize" -f - >/dev/null 2>&1 \
                    || true
            fi
        done <<< "$old_namespaces"
    fi

    log_info "Cleanup complete"
}

# Fixed defaults matching apply-nodes.sh
SYSTEM_NODE_COUNT=2
GPU_NODE_COUNT=4

# Verify KWOK nodes exist
verify_kwok_nodes() {
    local recipe="$1"
    local expected_total=$((SYSTEM_NODE_COUNT + GPU_NODE_COUNT))

    log_debug "Checking for KWOK nodes (expected: $expected_total)..."

    # Check if kubectl can connect to cluster
    if ! kubectl cluster-info &>/dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        log_error "Make sure a KWOK cluster is running: make kwok-cluster"
        exit 1
    fi

    local actual_count
    actual_count=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | wc -l | tr -d ' ')

    log_debug "Found $actual_count KWOK nodes"

    if [[ "$actual_count" -lt "$expected_total" ]]; then
        log_error "Expected $expected_total KWOK nodes, found $actual_count"
        log_error ""
        log_error "To fix this, run:"
        log_error "  make kwok-cluster              # Create cluster"
        log_error "  make kwok-nodes RECIPE=$recipe # Create nodes"
        log_error ""
        log_error "Or run the full e2e workflow:"
        log_error "  make kwok-e2e RECIPE=$recipe"
        exit 1
    fi

    log_info "Verified $actual_count KWOK nodes exist"
}

# compute_gitea_urls populates the dual-view Git URLs (GIT_IN_CLUSTER_URL,
# GIT_PUSH_URL) for the Git-source lanes (flux-git, argocd-git; issue #963).
# The IN-CLUSTER URL (Service DNS) is what gets baked into the bundle via
# --repo and what the GitOps controller clones from inside the cluster; the
# PUSH URL (localhost host-port + basic-auth) is what the runner pushes to.
# Shared so the runner→Service-DNS rewrite rule lives in exactly one place.
# The repo must be anonymously READABLE — install-infra.sh configures Gitea
# with DEFAULT_PUSH_CREATE_PRIVATE=false because neither the flux gitrepo
# source CR nor the argocd Application carries clone credentials.
compute_gitea_urls() {
    local recipe="$1"
    local gitea_host_port="${KWOK_GITEA_HOST_PORT:-3300}"
    local gitea_user="${KWOK_GITEA_USER:-aicr}"
    local gitea_password="${KWOK_GITEA_PASSWORD:-aicr-kwok-ci}"
    GIT_IN_CLUSTER_URL="http://gitea.aicr-registry.svc.cluster.local:3000/${gitea_user}/${recipe}.git"
    GIT_PUSH_URL="http://${gitea_user}:${gitea_password}@localhost:${gitea_host_port}/${gitea_user}/${recipe}.git"
}

# push_bundle_to_gitea pushes the rendered bundle tree at ${WORK_DIR}/bundle
# to Gitea on branch main. push-to-create makes the repo on first push;
# --force resets state on re-runs so the lane is idempotent (Gitea state is
# emptyDir-ephemeral anyway). Branch main is a hard requirement — both the
# flux GitRepository refs and the argocd targetRevision default to main, and
# there is no CLI flag to change it for filesystem output. compute_gitea_urls
# must have run first (GIT_PUSH_URL / GIT_IN_CLUSTER_URL set).
push_bundle_to_gitea() {
    local recipe="$1"
    local short_sha gitea_host_port gitea_user
    short_sha=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo "local")
    gitea_host_port="${KWOK_GITEA_HOST_PORT:-3300}"
    gitea_user="${KWOK_GITEA_USER:-aicr}"

    log_info "Pushing bundle to Gitea (localhost:${gitea_host_port}, repo ${gitea_user}/${recipe})..."
    local git_output
    if ! git_output=$(
        {
            cd "${WORK_DIR}/bundle" &&
            git init -q -b main &&
            git add -A &&
            git -c user.name=aicr-kwok -c user.email=aicr@example.invalid \
                commit -q -m "bundle ${recipe} ${short_sha}-${DEPLOYER}" &&
            git push -q --force "$GIT_PUSH_URL" main
        } 2>&1
    ); then
        log_error "git push to Gitea failed"
        log_error "$git_output"
        return 1
    fi
    log_info "Bundle pushed; GitOps controller will clone from ${GIT_IN_CLUSTER_URL} (branch main)"
}

# Generate recipe and bundle
generate_bundle() {
    local recipe="$1"

    log_info "Generating bundle for recipe: $recipe"

    # Read criteria from the recipe overlay file
    local recipe_overlay="${REPO_ROOT}/recipes/overlays/${recipe}.yaml"
    if [[ ! -f "$recipe_overlay" ]]; then
        log_error "Recipe overlay not found: $recipe_overlay"
        exit 1
    fi

    # Without --platform, *-slurm overlays resolve to their non-platform
    # parent and the bundle omits the slinky-slurm operator/cluster.
    # Scoped to slurm: kubeflow/dynamo are not yet validated under KWOK.
    local service accelerator intent os platform
    service=$(yq eval '.spec.criteria.service // ""' "$recipe_overlay")
    accelerator=$(yq eval '.spec.criteria.accelerator // ""' "$recipe_overlay")
    intent=$(yq eval '.spec.criteria.intent // ""' "$recipe_overlay")
    os=$(yq eval '.spec.criteria.os // ""' "$recipe_overlay")
    platform=$(yq eval '.spec.criteria.platform // ""' "$recipe_overlay")

    log_info "Criteria: service=$service accelerator=$accelerator intent=$intent os=$os platform=$platform"

    local recipe_args=()
    [[ -n "$service" ]] && recipe_args+=(--service "$service")
    [[ -n "$accelerator" ]] && recipe_args+=(--accelerator "$accelerator")
    [[ -n "$intent" ]] && recipe_args+=(--intent "$intent")
    [[ -n "$os" ]] && recipe_args+=(--os "$os")
    # Only forward --platform for platforms validated under KWOK. Other
    # platforms (kubeflow, dynamo, nim) historically resolve to their
    # non-platform parent here; preserve that behavior to avoid regressing
    # existing matrix lanes. Extend as additional platforms are validated.
    if [[ "$platform" == "slurm" ]]; then
        recipe_args+=(--platform "$platform")
    elif [[ -n "$platform" ]]; then
        log_info "platform=$platform not yet validated under KWOK — resolving without --platform"
    fi

    # Generate resolved recipe from criteria
    log_info "Generating resolved recipe..."
    log_debug "Running: $AICR_BIN recipe ${recipe_args[*]} --output ${WORK_DIR}/recipe.yaml"

    if ! "$AICR_BIN" recipe "${recipe_args[@]}" --output "${WORK_DIR}/recipe.yaml" 2>&1; then
        log_error "Recipe generation failed"
        return 1
    fi

    if [[ ! -f "${WORK_DIR}/recipe.yaml" ]]; then
        log_error "Recipe file not created: ${WORK_DIR}/recipe.yaml"
        return 1
    fi

    log_debug "Generated recipe:"
    head -20 "${WORK_DIR}/recipe.yaml"

    # Generate bundle with node scheduling flags for KWOK
    # Disable features not needed for scheduling validation:
    # - PrometheusRules and AlertManager (slow to create)
    # - Nodewright customization (creates CRs that depend on operator CRDs)
    # - slinky-slurm-operator webhook + cert-manager wiring: the operator's
    #   webhook validates Slurm CRs through a Service whose pod runs on a
    #   KWOK fake (Ready without container). Both certManager.enabled and
    #   webhook.enabled gate the cert-manager.io/Certificate submission
    #   plus the ValidatingWebhookConfiguration. Disabling them skips
    #   admission entirely; harmless under KWOK since no real Slurm CRs
    #   are reconciled.
    # - slinky-slurm controller persistence: the chart provisions a PVC
    #   via the cluster's default StorageClass. Kind's local-path provisioner
    #   binds with WaitForFirstConsumer, so the PVC is pinned to whichever
    #   node the pod schedules on — and KWOK fakes can't actually back a
    #   local-path volume, leaving the pod stuck Pending with NominatedNodeName
    #   set. Disabling persistence lets the controller pod bind.
    log_info "Generating bundle (deployer=${DEPLOYER})..."

    local bundle_output
    case "$DEPLOYER" in
        helm)
            # Original Helm path — DO NOT change this invocation. Byte-identical
            # to pre-#843 behavior is a non-regression requirement.
            if ! bundle_output=$("$AICR_BIN" bundle \
                --recipe "${WORK_DIR}/recipe.yaml" \
                --output "${WORK_DIR}/bundle" \
                --system-node-selector "aicr.nvidia.com/node-type=system" \
                --accelerated-node-selector "aicr.nvidia.com/node-type=accelerated" \
                --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
                --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --set "certmanager:startupapicheck.enabled=false" \
                --set "slinkyslurmoperator:webhook.enabled=false" \
                --set "slinkyslurmoperator:certManager.enabled=false" \
                --set "slurmcluster:controller.persistence.enabled=false" \
                --set "kubeprometheusstack:defaultRules.create=false" \
                --set "kubeprometheusstack:alertmanager.enabled=false" \
                --set "nodewright-customizations:enabled=false" \
                --set "dynamoplatform:etcd.persistence.enabled=false" \
                --set "dynamoplatform:nats.config.jetstream.fileStore.enabled=false" 2>&1); then
                log_error "Bundle generation failed"
                log_error "Bundle command output:"
                echo "$bundle_output"
                return 1
            fi

            if [[ ! -d "${WORK_DIR}/bundle" ]]; then
                log_error "Bundle directory not created: ${WORK_DIR}/bundle"
                return 1
            fi

            # KWOK clusters use emptyDir for Prometheus storage (no PVC /
            # StorageClass). Cloud overlays (EKS, AKS) set emptyDir: null +
            # volumeClaimTemplate, which the Prometheus CRD rejects in a
            # KWOK environment. Restore emptyDir and remove the PVC.
            #
            # This fix-up is intentionally helm-only: the argocd-* deployers
            # consume the bundle from OCI (the artifact was already pushed
            # above), so local-filesystem edits would be discarded by
            # Argo CD's repo-server when it re-pulls the artifact. See
            # SHOULD-FIX 2 of the #843 follow-up review.
            local prom_values
            prom_values=$(find "${WORK_DIR}/bundle" -mindepth 2 -maxdepth 2 \
                -path '*[0-9][0-9][0-9]-kube-prometheus-stack/values.yaml' \
                -type f 2>/dev/null | head -1)
            if [[ -n "$prom_values" && -f "$prom_values" ]] && \
                    yq eval '.prometheus.prometheusSpec.storageSpec.emptyDir' "$prom_values" 2>/dev/null | grep -q 'null'; then
                log_info "Fixing kube-prometheus-stack storageSpec for KWOK (emptyDir instead of PVC)"
                yq eval -i '
                    .prometheus.prometheusSpec.storageSpec.emptyDir = {"medium": "", "sizeLimit": "10Gi"} |
                    del(.prometheus.prometheusSpec.storageSpec.volumeClaimTemplate)
                ' "$prom_values"
            fi
            ;;
        argocd-oci|argocd-helm-oci)
            # Compute OCI ref. Tag includes the deployer so back-to-back runs
            # with different deployer values on the same recipe don't collide
            # in the registry.
            #
            # Tag-format constraint: helm v3's OCI client filters tags via
            # `semver.StrictNewVersion` (registry/client.go::Tags()) — a tag
            # like `fe346a05-argocd-helm-oci` is silently dropped and helm
            # reports `unable to locate any tags in provided repository`
            # even though the artifact is in the registry with correct
            # helm.chart.content.v1.tar+gzip mediaType (see #961 root
            # cause). Wrap the sha-and-deployer suffix as a semver pre-
            # release so `0.0.0-fe346a05-argocd-helm-oci` parses cleanly.
            # Argo CD's OCI Helm source does not have this filter, so the
            # other two lanes (helm, argocd-oci) keep the plain
            # sha-deployer tag.
            local short_sha
            short_sha=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo "local")
            local host_port="${KWOK_REGISTRY_HOST_PORT:-5500}"
            local tag="${short_sha}-${DEPLOYER}"
            if [[ "$DEPLOYER" == "argocd-helm-oci" ]]; then
                tag="0.0.0-${short_sha}-${DEPLOYER}"
            fi
            OCI_REF="oci://localhost:${host_port}/aicr/${recipe}:${tag}"

            # The OCI ref above is the runner's view (push side) — host port
            # exposed by Kind's extraPortMappings. Argo CD's repo-server runs
            # IN-CLUSTER and reaches the same registry via Service DNS, on the
            # Service's own port 5000 (not the runner's host port). The push
            # and pull URLs MUST differ; passing --repo to `aicr bundle`
            # overrides the default auto-derivation (which would otherwise
            # set repoURL = the runner-view URL and Argo CD would try to dial
            # localhost:5500 from inside its repo-server pod).
            # Script-global so deploy_bundle's argocd-helm-oci branch can
            # pass it through to `helm install --set repoURL=…` without
            # duplicating the runner→service-DNS rewrite rule.
            #
            # Per-lane assignment — the two deployers consume repoURL
            # differently:
            #
            #   - argocd-oci: the rendered `nvidia-stack` root application
            #     bakes the full artifact path into its `source.repoURL`.
            #     Pass the full "aicr/<recipe>" form so `--repo` and the
            #     baked URL both match the pushed artifact at
            #     oci://…/aicr/<recipe>:<tag>.
            #
            #   - argocd-helm-oci: the bundler's parent App template
            #     appends `/{{ .Chart.Name }}` to .Values.repoURL at helm
            #     render time (mirroring the path-based child template).
            #     Pass the PARENT NAMESPACE ONLY here so both halves of
            #     the rendered URL resolve to oci://…/aicr/<recipe>:<tag>.
            #     See pkg/bundler/deployer/argocdhelm/argocdhelm.go's
            #     `parentAppTemplate`.
            if [[ "$DEPLOYER" == "argocd-helm-oci" ]]; then
                OCI_IN_CLUSTER_REF="oci://registry.aicr-registry.svc.cluster.local:5000/aicr"
            else
                OCI_IN_CLUSTER_REF="oci://registry.aicr-registry.svc.cluster.local:5000/aicr/${recipe}"
            fi
            local in_cluster_repo="$OCI_IN_CLUSTER_REF"

            # Map our deployer-matrix name to aicr's --deployer value.
            local deployer_arg="argocd"
            [[ "$DEPLOYER" == "argocd-helm-oci" ]] && deployer_arg="argocd-helm"

            log_info "Bundling for ${deployer_arg}, pushing to ${OCI_REF}"
            if [[ "$DEPLOYER" == "argocd-helm-oci" ]]; then
                log_info "Argo CD will pull from ${in_cluster_repo}/${recipe}:${tag} (parent App template appends .Chart.Name at helm render time)"
            else
                log_info "Argo CD will pull from ${in_cluster_repo}:${tag}"
            fi
            # When --output is an oci:// reference, `aicr bundle` writes the
            # local bundle to ./bundle (relative to CWD) — there's no way to
            # redirect it to an absolute path. cd into WORK_DIR so the local
            # bundle lands at ${WORK_DIR}/bundle/ where downstream apply paths
            # expect it (kubectl apply -f .../app-of-apps.yaml for argocd-oci).
            if ! bundle_output=$(cd "${WORK_DIR}" && "$AICR_BIN" bundle \
                --recipe "${WORK_DIR}/recipe.yaml" \
                --deployer "$deployer_arg" \
                --output "$OCI_REF" \
                --repo "$in_cluster_repo" \
                --plain-http \
                --system-node-selector "aicr.nvidia.com/node-type=system" \
                --accelerated-node-selector "aicr.nvidia.com/node-type=accelerated" \
                --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
                --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --set "certmanager:startupapicheck.enabled=false" \
                --set "slinkyslurmoperator:webhook.enabled=false" \
                --set "slinkyslurmoperator:certManager.enabled=false" \
                --set "slurmcluster:controller.persistence.enabled=false" \
                --set "kubeprometheusstack:defaultRules.create=false" \
                --set "kubeprometheusstack:alertmanager.enabled=false" \
                --set "nodewright-customizations:enabled=false" \
                --set "dynamoplatform:etcd.persistence.enabled=false" \
                --set "dynamoplatform:nats.config.jetstream.fileStore.enabled=false" 2>&1); then
                log_error "Bundle generation failed"
                log_error "Bundle command output:"
                echo "$bundle_output"
                return 1
            fi

            # `--deployer argocd*` still renders the local bundle directory
            # (app-of-apps.yaml lives there for argocd-oci). For argocd-helm-oci
            # the bundle is also pushed as an OCI Helm chart we'll install via
            # `helm upgrade --install`.
            if [[ "$DEPLOYER" == "argocd-oci" ]] && [[ ! -f "${WORK_DIR}/bundle/app-of-apps.yaml" ]]; then
                log_error "Expected app-of-apps.yaml not found in bundle: ${WORK_DIR}/bundle"
                log_error "Bundle contents:"
                list_bundle_entries "${WORK_DIR}/bundle"
                return 1
            fi
            # NOTE: Prometheus emptyDir fix-up (see helm) branch above) is
            # intentionally NOT applied for argocd-* deployers because the
            # bundle is consumed from OCI, not the local filesystem. KWOK
            # clusters without a StorageClass may see Prometheus PVCs hang
            # Pending. Disable Prometheus persistence at bundle time via
            # `--set kubeprometheusstack:prometheus.prometheusSpec.storageSpec=null`
            # if running these recipes through argocd-*. See SHOULD-FIX 2 of
            # the #843 follow-up review and docs/plans/2026-05-18-kwok-
            # deployer-matrix.md (Unresolved Questions).
            ;;
        flux-oci)
            # Flux's source-controller does NOT filter OCI tags via semver
            # (it's an artifact pull, not a chart-version lookup), so the
            # plain sha-deployer tag is fine here — no `0.0.0-…` wrapping.
            local short_sha
            short_sha=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo "local")
            local host_port="${KWOK_REGISTRY_HOST_PORT:-5500}"
            OCI_REF="oci://localhost:${host_port}/aicr/${recipe}:${short_sha}-${DEPLOYER}"
            # In-cluster URL that the Flux OCIRepository will pull from.
            # The flux deployer does NOT bake repoURL into the bundle (the
            # bundle is a Kustomize tree of HelmRelease + HelmRepository
            # source CRs; the OUTER OCIRepository is what we apply at
            # deploy time, and its URL is set then). We still derive it
            # here so deploy_bundle can reuse the rewrite rule from
            # exactly one place.
            OCI_IN_CLUSTER_REF="oci://registry.aicr-registry.svc.cluster.local:5000/aicr/${recipe}"

            log_info "Bundling for flux, pushing to ${OCI_REF}"
            log_info "Flux will pull from ${OCI_IN_CLUSTER_REF}:${short_sha}-${DEPLOYER}"
            # cd into WORK_DIR for the same reason as the argocd-* branch
            # (relative ./bundle output path under `--output oci://...`).
            if ! bundle_output=$(cd "${WORK_DIR}" && "$AICR_BIN" bundle \
                --recipe "${WORK_DIR}/recipe.yaml" \
                --deployer flux \
                --output "$OCI_REF" \
                --plain-http \
                --system-node-selector "aicr.nvidia.com/node-type=system" \
                --accelerated-node-selector "aicr.nvidia.com/node-type=accelerated" \
                --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
                --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --set "certmanager:startupapicheck.enabled=false" \
                --set "slinkyslurmoperator:webhook.enabled=false" \
                --set "slinkyslurmoperator:certManager.enabled=false" \
                --set "slurmcluster:controller.persistence.enabled=false" \
                --set "kubeprometheusstack:defaultRules.create=false" \
                --set "kubeprometheusstack:alertmanager.enabled=false" \
                --set "nodewright-customizations:enabled=false" \
                --set "dynamoplatform:etcd.persistence.enabled=false" \
                --set "dynamoplatform:nats.config.jetstream.fileStore.enabled=false" 2>&1); then
                log_error "Bundle generation failed"
                log_error "Bundle command output:"
                echo "$bundle_output"
                return 1
            fi

            # Flux bundle root must contain kustomization.yaml — Flux's
            # Kustomization CR points its `path: ./` at it. Fail fast
            # otherwise (the OCIRepository pull would succeed but the
            # Kustomization apply would no-op silently).
            if [[ ! -f "${WORK_DIR}/bundle/kustomization.yaml" ]]; then
                log_error "Expected kustomization.yaml not found in bundle: ${WORK_DIR}/bundle"
                log_error "Bundle contents:"
                list_bundle_entries "${WORK_DIR}/bundle"
                return 1
            fi
            ;;
        flux-git)
            # Filesystem-bundle round-trip (issue #963): bundle to a local
            # directory, push the tree to the in-cluster Gitea, and let
            # Flux's source-controller clone it back. This exercises the
            # flux deployer's GIT shape — GitRepository source CRs in
            # sources/gitrepo-*.yaml — which the OCI lane never renders.
            #
            # --repo gets the IN-CLUSTER URL because it is baked into the
            # bundle's own GitRepository source CR, which source-controller
            # resolves from inside the cluster.
            compute_gitea_urls "$recipe"

            log_info "Bundling for flux (filesystem), repo URL ${GIT_IN_CLUSTER_URL}"
            # No --plain-http: that flag is OCI-only. No tag handling: the
            # bundler's GitRepository refs default to branch `main`, so the
            # push below MUST target main.
            if ! bundle_output=$("$AICR_BIN" bundle \
                --recipe "${WORK_DIR}/recipe.yaml" \
                --deployer flux \
                --output "${WORK_DIR}/bundle" \
                --repo "$GIT_IN_CLUSTER_URL" \
                --system-node-selector "aicr.nvidia.com/node-type=system" \
                --accelerated-node-selector "aicr.nvidia.com/node-type=accelerated" \
                --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
                --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --set "certmanager:startupapicheck.enabled=false" \
                --set "slinkyslurmoperator:webhook.enabled=false" \
                --set "slinkyslurmoperator:certManager.enabled=false" \
                --set "slurmcluster:controller.persistence.enabled=false" \
                --set "kubeprometheusstack:defaultRules.create=false" \
                --set "kubeprometheusstack:alertmanager.enabled=false" \
                --set "nodewright-customizations:enabled=false" \
                --set "dynamoplatform:etcd.persistence.enabled=false" \
                --set "dynamoplatform:nats.config.jetstream.fileStore.enabled=false" 2>&1); then
                log_error "Bundle generation failed"
                log_error "Bundle command output:"
                echo "$bundle_output"
                return 1
            fi

            # Same fail-fast as flux-oci: the wrapper Kustomization points
            # `path: ./` at the bundle root.
            if [[ ! -f "${WORK_DIR}/bundle/kustomization.yaml" ]]; then
                log_error "Expected kustomization.yaml not found in bundle: ${WORK_DIR}/bundle"
                log_error "Bundle contents:"
                list_bundle_entries "${WORK_DIR}/bundle"
                return 1
            fi
            # Git-mode proof: a filesystem --output must render at least one
            # GitRepository source CR. Its absence means the bundler silently
            # took the OCI path and the lane is no longer testing the Git
            # shape it exists for.
            if ! compgen -G "${WORK_DIR}/bundle/sources/gitrepo-*.yaml" >/dev/null; then
                log_error "No sources/gitrepo-*.yaml in bundle — flux git mode did not engage"
                log_error "Bundle contents:"
                list_bundle_entries "${WORK_DIR}/bundle"
                return 1
            fi

            # Push the bundle tree to Gitea (branch main, force-push for
            # idempotent re-runs). See push_bundle_to_gitea for details.
            if ! push_bundle_to_gitea "$recipe"; then
                return 1
            fi
            ;;
        argocd-git)
            # Git-source round-trip for the argocd deployer (issue #963):
            # bundle to a local directory with --repo set to the in-cluster
            # Git URL, push the tree to Gitea, and let Argo CD's repo-server
            # clone it back. This exercises the argocd deployer's GIT output
            # shape — an app-of-apps plus per-component application.yaml whose
            # spec.source.repoURL is the Git URL and targetRevision is `main`
            # (pkg/bundler/deployer/argocd/resolveRepoSettings) — which the
            # OCI lane never renders.
            compute_gitea_urls "$recipe"

            log_info "Bundling for argocd (filesystem), repo URL ${GIT_IN_CLUSTER_URL}"
            # No --plain-http (OCI-only) and no OCI tag: with a filesystem
            # --output the argocd deployer bakes --repo straight into every
            # Application's repoURL and defaults targetRevision to main, so
            # the push below MUST target main.
            if ! bundle_output=$("$AICR_BIN" bundle \
                --recipe "${WORK_DIR}/recipe.yaml" \
                --deployer argocd \
                --output "${WORK_DIR}/bundle" \
                --repo "$GIT_IN_CLUSTER_URL" \
                --system-node-selector "aicr.nvidia.com/node-type=system" \
                --accelerated-node-selector "aicr.nvidia.com/node-type=accelerated" \
                --system-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --accelerated-node-toleration "nvidia.com/gpu=present:NoSchedule" \
                --accelerated-node-toleration "kwok.x-k8s.io/node=fake:NoSchedule" \
                --set "certmanager:startupapicheck.enabled=false" \
                --set "slinkyslurmoperator:webhook.enabled=false" \
                --set "slinkyslurmoperator:certManager.enabled=false" \
                --set "slurmcluster:controller.persistence.enabled=false" \
                --set "kubeprometheusstack:defaultRules.create=false" \
                --set "kubeprometheusstack:alertmanager.enabled=false" \
                --set "nodewright-customizations:enabled=false" \
                --set "dynamoplatform:etcd.persistence.enabled=false" \
                --set "dynamoplatform:nats.config.jetstream.fileStore.enabled=false" 2>&1); then
                log_error "Bundle generation failed"
                log_error "Bundle command output:"
                echo "$bundle_output"
                return 1
            fi

            # The argocd deployer's root manifest is app-of-apps.yaml; the
            # deploy step applies it. Fail fast if it's missing.
            if [[ ! -f "${WORK_DIR}/bundle/app-of-apps.yaml" ]]; then
                log_error "Expected app-of-apps.yaml not found in bundle: ${WORK_DIR}/bundle"
                log_error "Bundle contents:"
                list_bundle_entries "${WORK_DIR}/bundle"
                return 1
            fi
            # Git-mode proof: the root Application's repoURL must be the Git
            # URL we passed via --repo. If it is an oci:// reference the
            # bundler silently took the OCI path and the lane is no longer
            # testing the Git shape it exists for. (The chainsaw gate asserts
            # this on the live resource too; this is the fail-fast at bundle
            # time before the push.) The repoURL is unquoted in app-of-apps
            # and quoted in per-component application.yaml, so match either.
            if ! grep -Eq "repoURL: '?${GIT_IN_CLUSTER_URL}'?[[:space:]]*$" \
                    "${WORK_DIR}/bundle/app-of-apps.yaml"; then
                log_error "app-of-apps.yaml repoURL is not the Git URL ${GIT_IN_CLUSTER_URL}"
                log_error "argocd git mode did not engage; app-of-apps.yaml:"
                cat "${WORK_DIR}/bundle/app-of-apps.yaml" || true
                return 1
            fi

            # Push the bundle tree to Gitea (branch main, force-push for
            # idempotent re-runs). See push_bundle_to_gitea for details.
            if ! push_bundle_to_gitea "$recipe"; then
                return 1
            fi
            ;;
        *)
            log_error "Unknown deployer: ${DEPLOYER}"
            return 1
            ;;
    esac

    log_info "Bundle generated at ${WORK_DIR}/bundle"

    log_debug "Bundle contents:"
    list_bundle_entries "${WORK_DIR}/bundle" | head -10
}

# Print top-level entries of a bundle directory, one per line, basename only.
# Portable replacement for `ls -1 <dir>` (SC2012) that does not depend on
# GNU find's `-printf` extension. Bash NULLGLOB is enabled locally so an
# empty directory prints nothing instead of the literal pattern.
#
# SIGPIPE handling: callers commonly pipe this to `head -N`. When the bundle
# has more than N entries, `head` closes stdin after reading N lines. The next
# `printf` in this subshell then writes to a closed FD; bash forwards SIGPIPE
# (exit 141), which under `set -euo pipefail` aborts the whole script even
# though listing succeeded. Ignoring SIGPIPE in the subshell turns the write
# into a plain EPIPE on `printf` (returns non-zero, but does not kill the
# shell); breaking out of the loop on that first failed write produces the
# same "listed up to N then stopped" behavior the caller intends, without
# leaking a 141 exit through the pipeline. Observed bite: OKE argocd-helm-oci
# CI cancellation on commit 263f4433 — bundle generated cleanly but `… | head
# -10` raced the producer past the 10th line and SIGPIPE killed the run.
list_bundle_entries() {
    local dir="$1"
    [[ -d "$dir" ]] || return 0
    (
        trap '' PIPE
        shopt -s nullglob dotglob
        local entry
        for entry in "$dir"/*; do
            printf '%s\n' "${entry##*/}" || break
        done
    )
}

# Like list_bundle_entries but excludes the bundle's generated deploy.sh.
# Used by the helm deploy path to log component folder names.
list_bundle_components() {
    local dir="$1"
    list_bundle_entries "$dir" | { grep -v '^deploy\.sh$' || true; }
}

# Deploy bundle to cluster.
#
# For DEPLOYER=helm: runs the bundle's generated deploy.sh (unchanged
# pre-#843 behavior).
#
# For DEPLOYER=argocd-oci: kubectl apply -f <bundle>/app-of-apps.yaml, then
# wait for every Argo CD Application in the argocd namespace to reach
# Synced + (Healthy|Progressing).
#
# For DEPLOYER=argocd-helm-oci: helm upgrade --install the OCI chart into
# the argocd namespace, then wait the same way.
deploy_bundle() {
    log_info "Deploying per-component bundle (deployer=${DEPLOYER})..."

    local bundle_dir="${WORK_DIR}/bundle"

    case "$DEPLOYER" in
        helm)
            if [[ ! -f "${bundle_dir}/deploy.sh" ]]; then
                log_error "deploy.sh not found in bundle directory: $bundle_dir"
                log_error "Bundle generation may have failed"
                return 1
            fi

            log_debug "Bundle directory: $bundle_dir"
            log_debug "Components in bundle:"
            list_bundle_components "$bundle_dir" | head -10

            # Run the generated deploy script without --wait since KWOK clusters
            # only validate scheduling, not pod readiness
            chmod +x "${bundle_dir}/deploy.sh"
            log_info "Running deploy.sh --no-wait..."

            local deploy_output
            if ! deploy_output=$("${bundle_dir}/deploy.sh" --no-wait 2>&1); then
                log_error "Deploy script failed"
                log_error "Last 50 lines of deploy output:"
                echo "$deploy_output" | tail -50
                return 1
            fi
            ;;
        argocd-oci|argocd-git)
            # Both lanes apply the same app-of-apps root manifest; they
            # differ only in the source the Applications point at (OCI
            # registry vs in-cluster Gitea), which is baked into the bundle
            # at generate time. The sync gate (wait_for_argocd_sync) routes
            # to the right chainsaw test per DEPLOYER.
            log_info "Applying app-of-apps manifest: ${bundle_dir}/app-of-apps.yaml"
            if ! kubectl apply -f "${bundle_dir}/app-of-apps.yaml"; then
                log_error "kubectl apply -f app-of-apps.yaml failed"
                return 1
            fi
            # Preserve wait_for_argocd_sync's exit code (50 == sync timeout)
            # so run-all-recipes.sh can apply the 3-strike rule.
            local sync_rc=0
            wait_for_argocd_sync || sync_rc=$?
            if (( sync_rc != 0 )); then
                return "$sync_rc"
            fi
            ;;
        argocd-helm-oci)
            if [[ -z "$OCI_REF" ]]; then
                log_error "OCI_REF unset for argocd-helm-oci path (bundle step skipped?)"
                return 1
            fi
            if [[ -z "$OCI_IN_CLUSTER_REF" ]]; then
                log_error "OCI_IN_CLUSTER_REF unset for argocd-helm-oci path (bundle step skipped?)"
                return 1
            fi
            # Helm OCI syntax: the chart reference must NOT include a ':<tag>'
            # suffix. The tag is passed via --version. OCI_REF is shaped as
            # oci://host/path:tag; split into ref (oci://host/path) + tag.
            local helm_ref="${OCI_REF%:*}"
            local helm_tag="${OCI_REF##*:}"
            # repoURL + targetRevision are REQUIRED by the argocdhelm wrapper
            # chart at install time (pkg/bundler/deployer/argocdhelm). The
            # chart templates path-based child Applications (e.g.
            # gpu-operator-post for manifest-only / mixed components) whose
            # spec.source.repoURL must resolve to a remote Argo CD's
            # repo-server can pull from. The wrapper enforces this with
            # `{{ required }}` on .Values.repoURL — without these flags
            # `helm install` fails template-render with
            # `repoURL is required: pass --set repoURL=<published bundle URL>`.
            #
            # repoURL is the IN-CLUSTER URL (Service DNS) because Argo CD's
            # repo-server runs inside the cluster; targetRevision is the
            # chart's OCI tag, which by convention also names the chart
            # version (loadAndRewriteChartYAML in pkg/oci/helm.go rewrites
            # Chart.yaml.version to the OCI tag at push time).
            log_info "helm upgrade --install ${RELEASE_NAME}-stack ${helm_ref} --version ${helm_tag} -n argocd --plain-http --set repoURL=${OCI_IN_CLUSTER_REF} --set targetRevision=${helm_tag}"
            # --plain-http: in-cluster registry is HTTP-only; helm defaults to HTTPS.
            if ! helm upgrade --install "${RELEASE_NAME}-stack" "$helm_ref" \
                    --version "$helm_tag" \
                    --namespace argocd \
                    --plain-http \
                    --set "repoURL=${OCI_IN_CLUSTER_REF}" \
                    --set "targetRevision=${helm_tag}"; then
                log_error "helm upgrade --install failed for ${OCI_REF}"
                return 1
            fi
            # Preserve wait_for_argocd_sync's exit code (50 == sync timeout)
            # so run-all-recipes.sh can apply the 3-strike rule.
            local sync_rc=0
            wait_for_argocd_sync || sync_rc=$?
            if (( sync_rc != 0 )); then
                return "$sync_rc"
            fi
            ;;
        flux-oci)
            if [[ -z "$OCI_IN_CLUSTER_REF" ]]; then
                log_error "OCI_IN_CLUSTER_REF unset for flux-oci path (bundle step skipped?)"
                return 1
            fi
            if [[ -z "$OCI_REF" ]]; then
                log_error "OCI_REF unset for flux-oci path (bundle step skipped?)"
                return 1
            fi
            # OCI_REF is "oci://host:port/path:tag"; the tag we need for the
            # OCIRepository's `ref.tag` is the rightmost ':<segment>'. Split.
            local oci_tag="${OCI_REF##*:}"

            # Render OCIRepository + Kustomization wrappers.
            #
            # OCIRepository:
            #   - spec.insecure: true — in-cluster registry is HTTP-only.
            #   - spec.layerSelector.mediaType — AICR's generic OCI push
            #     uses application/vnd.oci.image.layer.v1.tar+gzip (see
            #     pkg/oci/push.go::artifactType for the manifest's
            #     artifactType; source-controller doesn't filter on that
            #     field but DOES need a layerSelector for non-Flux-CLI
            #     artifacts per fluxcd/source-controller docs).
            #   - spec.layerSelector.operation: extract — unpack the
            #     layer into the source workspace.
            #
            # Kustomization:
            #   - sourceRef → the OCIRepository above
            #   - path: ./ — the bundle's root contains kustomization.yaml
            #   - prune: true — let Flux GC removed resources between runs
            #   - wait: false — KWOK can't drive Pod readiness for arbitrary
            #     workloads, so `wait: true` would block the Kustomization's
            #     Ready condition indefinitely. We assert terminal state via
            #     HelmRelease conditions below instead.
            #   - timeout: 5m — matches KWOK_FLUX_SYNC_TIMEOUT budget.
            log_info "Applying Flux OCIRepository ${FLUX_OCIREPOSITORY_NAME} (ref=${oci_tag})..."
            if ! kubectl apply -f - <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: ${FLUX_OCIREPOSITORY_NAME}
  namespace: flux-system
spec:
  interval: 1m
  insecure: true
  url: ${OCI_IN_CLUSTER_REF}
  ref:
    tag: ${oci_tag}
  layerSelector:
    mediaType: application/vnd.oci.image.layer.v1.tar+gzip
    operation: extract
EOF
            then
                log_error "kubectl apply OCIRepository failed"
                return 1
            fi

            log_info "Applying Flux Kustomization ${FLUX_KUSTOMIZATION_NAME}..."
            if ! kubectl apply -f - <<EOF
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: ${FLUX_KUSTOMIZATION_NAME}
  namespace: flux-system
spec:
  interval: 1m
  prune: true
  wait: false
  timeout: 5m
  sourceRef:
    kind: OCIRepository
    name: ${FLUX_OCIREPOSITORY_NAME}
  path: ./
EOF
            then
                log_error "kubectl apply Kustomization failed"
                return 1
            fi

            # Preserve wait_for_flux_sync's exit code (50 == sync timeout)
            # so run-all-recipes.sh can apply the 3-strike rule.
            local sync_rc=0
            wait_for_flux_sync || sync_rc=$?
            if (( sync_rc != 0 )); then
                return "$sync_rc"
            fi
            ;;
        flux-git)
            if [[ -z "$GIT_IN_CLUSTER_URL" ]]; then
                log_error "GIT_IN_CLUSTER_URL unset for flux-git path (bundle step skipped?)"
                return 1
            fi

            # Mirror of the flux-oci branch with a GitRepository source
            # instead of OCIRepository. The outer GitRepository here is the
            # wrapper source the Kustomization pulls the bundle tree from;
            # the bundle ALSO carries its own inner GitRepository CR
            # (sources/gitrepo-*.yaml) that local-chart HelmReleases
            # reference — both point at the same public Gitea repo.
            #
            # No spec.secretRef: the repo is public (anonymous clone), and
            # plain HTTP needs no insecure toggle for git sources (unlike
            # OCIRepository.spec.insecure).
            log_info "Applying Flux GitRepository ${FLUX_GITREPOSITORY_NAME} (url=${GIT_IN_CLUSTER_URL})..."
            if ! kubectl apply -f - <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: ${FLUX_GITREPOSITORY_NAME}
  namespace: flux-system
spec:
  interval: 1m
  url: ${GIT_IN_CLUSTER_URL}
  ref:
    branch: main
EOF
            then
                log_error "kubectl apply GitRepository failed"
                return 1
            fi

            # Kustomization identical to the flux-oci wrapper except for
            # sourceRef.kind — see that branch for the wait/prune/timeout
            # rationale.
            log_info "Applying Flux Kustomization ${FLUX_KUSTOMIZATION_NAME}..."
            if ! kubectl apply -f - <<EOF
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: ${FLUX_KUSTOMIZATION_NAME}
  namespace: flux-system
spec:
  interval: 1m
  prune: true
  wait: false
  timeout: 5m
  sourceRef:
    kind: GitRepository
    name: ${FLUX_GITREPOSITORY_NAME}
  path: ./
EOF
            then
                log_error "kubectl apply Kustomization failed"
                return 1
            fi

            # Preserve wait_for_flux_sync's exit code (50 == sync timeout)
            # so run-all-recipes.sh can apply the 3-strike rule.
            local sync_rc=0
            wait_for_flux_sync || sync_rc=$?
            if (( sync_rc != 0 )); then
                return "$sync_rc"
            fi
            ;;
        *)
            log_error "Unknown deployer: ${DEPLOYER}"
            return 1
            ;;
    esac

    # Brief wait for scheduler to place pods
    log_info "Waiting for pods to be scheduled..."
    sleep 5

    log_info "Bundle deployed successfully"
}

# Chainsaw-based sync gate for the argocd-* and flux-oci deployer lanes.
# Replaces the ~500-line bash + jq machinery (wait_for_argocd_root_app,
# wait_for_argocd_sync, dump_argocd_failures, and their flux-* peers) with
# thin shims that invoke the canonical chainsaw scenarios under
# tests/chainsaw/kwok/{argocd,flux}-sync/. See issue #962.
#
# The bash side still owns:
#
#   1. Per-deployer binding selection (ARGOCD_ROOT_APP shape, Flux CR
#      names) — set in the matrix-shape preamble at the top of this file.
#   2. Exit-code translation. Chainsaw collapses pass / fail to 0 / 1;
#      run-all-recipes.sh's 3-strike rule needs EXIT_ARGOCD_SYNC_TIMEOUT
#      (50) on the retryable path. The shim maps chainsaw exit 1 to 50
#      unconditionally — hard errors (CRD missing, RBAC denial) thus
#      consume strike budget instead of failing fast. Three consecutive
#      hard errors still bail the matrix job; the behavior difference
#      vs. the old bash gate is that they take three attempts instead
#      of one. Accept the tradeoff for the ~500 LOC reduction.
#   3. Failure-class logging. Chainsaw emits its own assertion report;
#      the shim adds a one-line log_info on PASS and log_error on FAIL
#      so the run-all-recipes.sh transcript still has a single grep-able
#      summary line per recipe.
#
# Chainsaw provides:
#
#   - Polling loop with deadline (spec.timeouts.assert overridden via
#     --assert-timeout from KWOK_*_SYNC_TIMEOUT).
#   - Per-resource assertion that ALL Applications / HelmReleases / etc.
#     match the terminal-pass predicate.
#   - catch: blocks that dump per-resource status + controller log tails
#     on assertion failure (parity with the prior dump_*_failures bash).
#
# Diagnostic-parity audit:
#
#   - dump_argocd_failures bash               → argocd-sync test's catch block
#   - dump_flux_failures bash                 → flux-sync test's per-step catch blocks
#   - StatefulSet/Deployment fallback for     → preserved verbatim in the
#     argocd-application-controller logs       argocd-sync catch script
#   - Flux source-watcher Deployment/         → preserved verbatim in the
#     StatefulSet fallback                     flux-sync ArtifactGenerator catch

wait_for_argocd_sync() {
    if [[ -z "$ARGOCD_ROOT_APP" ]]; then
        log_error "ARGOCD_ROOT_APP is empty (DEPLOYER=${DEPLOYER})"
        return 1
    fi

    local chainsaw_bin="${CHAINSAW_BIN:-chainsaw}"
    if ! command -v "$chainsaw_bin" &>/dev/null; then
        log_error "chainsaw not found on PATH: ${chainsaw_bin}"
        log_error "Install with: make tools-setup (chainsaw is pinned in .settings.yaml)"
        return 1
    fi

    # Per-deployer gate: argocd-oci / argocd-helm-oci use the source-agnostic
    # argocd-sync test; argocd-git uses the argocd-git-sync sibling, which
    # adds a leading step asserting the root Application's Git repoURL (the
    # Git-mode proof). The two tests' shared steps (root-App-present, 4-arm
    # terminal-pass) are byte-identical — see the sync-note headers in both
    # chainsaw-test.yaml files. argocd-git passes the extra repoURL binding.
    local test_dir extra_args=()
    case "$DEPLOYER" in
        argocd-git)
            test_dir="${REPO_ROOT}/tests/chainsaw/kwok/argocd-git-sync"
            if [[ -z "$GIT_IN_CLUSTER_URL" ]]; then
                log_error "GIT_IN_CLUSTER_URL is empty for argocd-git (bundle step skipped?)"
                return 1
            fi
            extra_args+=(--set "repoURL=${GIT_IN_CLUSTER_URL}")
            ;;
        *)
            test_dir="${REPO_ROOT}/tests/chainsaw/kwok/argocd-sync"
            ;;
    esac
    local sync_timeout="${KWOK_ARGOCD_SYNC_TIMEOUT:-480}s"

    log_info "Argo CD sync gate (chainsaw): rootApp=${ARGOCD_ROOT_APP} timeout=${sync_timeout}"

    # --assert-timeout overrides spec.timeouts.assert per CLI invocation,
    # letting the bash driver vary the deadline by env var without forking
    # the chainsaw test file per deployer. --no-color keeps the CI log
    # readable; the kwok-test composite action does not interpret ANSI.
    #
    # --error-timeout matters as much as --assert-timeout here: the
    # assert-all-applications-pass gate is an error-polarity op (poll
    # until NO Application matches the bad-state predicate), and
    # chainsaw's default error timeout is 10s — far below what a fleet
    # of Applications needs to converge. Without the override the gate
    # would flake on every multi-component recipe.
    #
    # --set is the inline override flag in chainsaw — it populates the
    # $values binding from the command line. --values is YAML-only
    # (file/stdin/heredoc); passing "key=value" to it silently leaves
    # the binding undefined, so the bindings: defaults take effect
    # regardless of the value supplied here. Use --set for inline
    # overrides.
    if "$chainsaw_bin" test "$test_dir" \
            --set "rootApp=${ARGOCD_ROOT_APP}" \
            "${extra_args[@]}" \
            --assert-timeout "$sync_timeout" \
            --error-timeout "$sync_timeout" \
            --no-color; then
        log_info "Argo CD sync PASS (chainsaw)"
        return 0
    fi

    log_error "Argo CD sync FAIL (chainsaw test in ${test_dir})"
    # Reuse EXIT_ARGOCD_SYNC_TIMEOUT so run-all-recipes.sh's 3-strike rule
    # treats every chainsaw-reported failure as retryable. Hard errors
    # (RBAC, CRD missing) will fail three times in a row and bail; the
    # behavior change vs. the prior bash gate is the 3x cost on hard
    # errors, which is acceptable for the LOC reduction.
    return "$EXIT_ARGOCD_SYNC_TIMEOUT"
}

wait_for_flux_sync() {
    if [[ -z "$FLUX_KUSTOMIZATION_NAME" ]]; then
        log_error "FLUX_KUSTOMIZATION_NAME is empty (DEPLOYER=${DEPLOYER})"
        return 1
    fi

    local chainsaw_bin="${CHAINSAW_BIN:-chainsaw}"
    if ! command -v "$chainsaw_bin" &>/dev/null; then
        log_error "chainsaw not found on PATH: ${chainsaw_bin}"
        log_error "Install with: make tools-setup (chainsaw is pinned in .settings.yaml)"
        return 1
    fi

    # Per-deployer gate: the two tests differ ONLY in the source-Ready
    # step (OCIRepository vs GitRepository); steps 2-4 (Kustomization,
    # HelmRelease terminal-pass predicate, ArtifactGenerator) are kept
    # byte-identical between them — see the sync-note header comments in
    # both chainsaw-test.yaml files and ADR-009.
    local test_dir source_args
    case "$DEPLOYER" in
        flux-oci)
            if [[ -z "$FLUX_OCIREPOSITORY_NAME" ]]; then
                log_error "FLUX_OCIREPOSITORY_NAME is empty (DEPLOYER=${DEPLOYER})"
                return 1
            fi
            test_dir="${REPO_ROOT}/tests/chainsaw/kwok/flux-sync"
            source_args=(--set "ociRepositoryName=${FLUX_OCIREPOSITORY_NAME}")
            log_info "Flux sync gate (chainsaw): ociRepository=${FLUX_OCIREPOSITORY_NAME} kustomization=${FLUX_KUSTOMIZATION_NAME}"
            ;;
        flux-git)
            if [[ -z "$FLUX_GITREPOSITORY_NAME" ]]; then
                log_error "FLUX_GITREPOSITORY_NAME is empty (DEPLOYER=${DEPLOYER})"
                return 1
            fi
            test_dir="${REPO_ROOT}/tests/chainsaw/kwok/flux-git-sync"
            source_args=(--set "gitRepositoryName=${FLUX_GITREPOSITORY_NAME}")
            log_info "Flux sync gate (chainsaw): gitRepository=${FLUX_GITREPOSITORY_NAME} kustomization=${FLUX_KUSTOMIZATION_NAME}"
            ;;
        *)
            log_error "wait_for_flux_sync called with non-flux DEPLOYER=${DEPLOYER}"
            return 1
            ;;
    esac

    local sync_timeout="${KWOK_FLUX_SYNC_TIMEOUT:-500}s"
    log_info "Flux sync timeout: ${sync_timeout}"

    # See the argocd shim's comment for the --set vs --values choice;
    # --values requires YAML, --set takes inline key=value pairs.
    #
    # --error-timeout matters as much as --assert-timeout here: the
    # all-HelmReleases and ArtifactGenerator gates are error-polarity
    # ops (poll until NO resource matches the bad-state predicate),
    # and chainsaw's default error timeout is 30s — far below what a
    # dependsOn chain needs to converge. Without the override the gate
    # would flake on every multi-component recipe.
    if "$chainsaw_bin" test "$test_dir" \
            "${source_args[@]}" \
            --set "kustomizationName=${FLUX_KUSTOMIZATION_NAME}" \
            --assert-timeout "$sync_timeout" \
            --error-timeout "$sync_timeout" \
            --no-color; then
        log_info "Flux sync PASS (chainsaw)"
        return 0
    fi

    log_error "Flux sync FAIL (chainsaw test in ${test_dir})"
    return "$EXIT_ARGOCD_SYNC_TIMEOUT"
}


# Verify pod scheduling
verify_pods() {
    log_info "Verifying pod scheduling..."

    # Get pod status across all component namespaces (per-component deployment)
    # Exclude system namespaces that aren't part of our bundle (control-plane
    # infra plus GitOps controller namespaces — argocd/flux-system/aicr-
    # registry are owned by install-infra.sh, not the bundle under test).
    local exclude_ns="kube-system|kube-node-lease|kube-public|local-path-storage|kwok-system|argocd|flux-system|aicr-registry"

    # Take a coherent snapshot of pod state and derive every count, the
    # pod distribution, and the failure diagnostic from it. The previous
    # implementation issued 5+ separate `kubectl get pods` calls over
    # several seconds; controllers (gpu-operator, dynamo, dra-driver, …)
    # reconciled new pods between calls, producing impossible math (e.g.
    # pending=0 + unscheduled=1) plus a "No resources found" diagnostic
    # when the transient pod had already been bound by the scheduler.
    # See #1090 for the failure signature.
    #
    # Poll-then-verify: after the Argo CD / Flux sync gate reports
    # Synced+Healthy, controllers continue creating pods that haven't
    # been bound by the scheduler yet. Without a settle window, every
    # snapshot taken in that gap reports the pre-bind transient as a
    # failure (observed on the eks-inference/argocd-oci and
    # oke-training/argocd-oci lanes — cert-manager and skyhook-operator
    # respectively, both with zero FailedScheduling events, meaning
    # the scheduler simply had not gotten to them yet). The loop below
    # re-snapshots up to KWOK_VERIFY_PODS_DEADLINE seconds at
    # KWOK_VERIFY_PODS_INTERVAL spacing and only declares failure if
    # unscheduled_pods is still positive at the deadline. Each snapshot
    # is internally coherent (single `kubectl get pods -o json`), so the
    # impossible-math signature from #1090 cannot reappear regardless
    # of how many iterations the loop runs.
    local deadline="${KWOK_VERIFY_PODS_DEADLINE:-60}"
    local interval="${KWOK_VERIFY_PODS_INTERVAL:-5}"

    # Validate the tunables before the loop relies on them. The settle
    # window is only bounded if `waited` advances every iteration:
    #   - interval=0 → `sleep 0` returns instantly AND `waited+=0` never
    #     grows, so an unschedulable pod (Pending, failed=0) spins the
    #     loop forever until the CI job timeout — defeating the deadline.
    #   - a non-integer value (e.g. "abc", "2.5") aborts opaquely under
    #     `set -euo pipefail` at `sleep`/`$(( ))` with no clear cause.
    #   - a leading-zero value (e.g. "08", "09") matches the digit regex
    #     but bash arithmetic reads it as octal ("08: value too great for
    #     base"), so the `waited >= deadline` test and `$(( ))` increment
    #     misbehave. Canonicalize to base 10 with `10#...` after the regex
    #     so the loop only ever sees clean decimal integers.
    # Reject non-digits up front, then normalize. deadline may be 0
    # (fail fast, no settle); interval must be >= 1.
    if ! [[ "$deadline" =~ ^[0-9]+$ ]]; then
        log_error "KWOK_VERIFY_PODS_DEADLINE must be a non-negative integer (got: '${deadline}')"
        return 1
    fi
    if ! [[ "$interval" =~ ^[0-9]+$ ]]; then
        log_error "KWOK_VERIFY_PODS_INTERVAL must be a positive integer (got: '${interval}')"
        return 1
    fi
    deadline=$((10#$deadline))
    interval=$((10#$interval))
    if (( interval < 1 )); then
        log_error "KWOK_VERIFY_PODS_INTERVAL must be a positive integer >= 1 (got: '${KWOK_VERIFY_PODS_INTERVAL:-}')"
        return 1
    fi

    local waited=0
    local attempt=0
    local snap
    local total_pods pending_pods failed_pods running_pods unscheduled_pods

    while :; do
        attempt=$((attempt + 1))

        # Fail closed on kubectl error: apiserver outage, auth/RBAC
        # failure, or a bad kube context must not be reported as a
        # scheduling pass. The pre-existing `2>/dev/null | wc -l` chain
        # did abort under `set -euo pipefail` (pipefail propagated
        # kubectl's non-zero status), but opaquely — stderr was
        # discarded, so the job log showed a bare non-zero exit with no
        # cause. Capture stderr and emit an explicit message + clean
        # `return 1` instead.
        local snap_err_file
        snap_err_file=$(mktemp)
        if ! snap=$(kubectl get pods --all-namespaces -o json 2>"$snap_err_file"); then
            log_error "Failed to list pods (apiserver unreachable, auth/RBAC failure, or bad kube context):"
            log_error "$(cat "$snap_err_file")"
            rm -f "$snap_err_file"
            return 1
        fi
        rm -f "$snap_err_file"

        # All counts derive from the same $snap in a SINGLE jq pass: the
        # namespace exclusion is applied once (`as $f`), then each count
        # is a sub-filter over $f, emitted as one tab-separated line and
        # split by `read`. One jq invocation per iteration (vs five)
        # reinforces the "all counts from one snapshot" invariant. The
        # exclusion filter uses the same pipe-delimited list passed via
        # --arg pat and split inside jq, so the bash regex pattern and
        # the jq namespace filter agree by construction. The fifth field,
        # "truly unscheduled", is Pending with no node assigned, excluding
        # Job-owned cleanup pods (Helm hooks may legitimately lack
        # tolerations).
        read -r total_pods running_pods pending_pods failed_pods unscheduled_pods < <(
            jq -r --arg pat "$exclude_ns" '
                [ .items[]
                  | select(.metadata.namespace as $ns | $pat | split("|") | map(. == $ns) | any | not)
                ] as $f
                | [ ($f | length),
                    ([$f[] | select(.status.phase == "Running")] | length),
                    ([$f[] | select(.status.phase == "Pending")] | length),
                    ([$f[] | select(.status.phase == "Failed")] | length),
                    ([$f[]
                      | select(.status.phase == "Pending")
                      | select((.spec.nodeName // "") == "")
                      | select((.metadata.ownerReferences // []) | map(.kind) | contains(["Job"]) | not)
                     ] | length)
                  ] | @tsv
            ' <<<"$snap"
        )

        # Exit conditions:
        #   - unscheduled == 0   → scheduler has caught up; fall through
        #   - failed > 0         → terminal state, no point waiting
        #   - waited >= deadline → bounded window exhausted, fail with
        #                          the final snapshot
        if [[ "$unscheduled_pods" -eq 0 ]] \
                || [[ "$failed_pods" -gt 0 ]] \
                || [[ "$waited" -ge "$deadline" ]]; then
            break
        fi

        log_info "Attempt ${attempt}: $total_pods total, $running_pods running, $pending_pods pending ($unscheduled_pods unscheduled), $failed_pods failed — waiting for scheduler (${waited}/${deadline}s)"
        sleep "$interval"
        waited=$((waited + interval))
    done

    log_info "Pod status: $total_pods total, $running_pods running, $pending_pods pending ($unscheduled_pods unscheduled), $failed_pods failed (attempts=${attempt}, waited=${waited}s)"

    # Pod distribution — derived from the SAME snapshot, not a fresh
    # `kubectl get pods -o wide` (which would race against the counts).
    log_info "Pod distribution:"
    jq -r --arg pat "$exclude_ns" '
        .items[]
        | select(.metadata.namespace as $ns | $pat | split("|") | map(. == $ns) | any | not)
        | (if (.spec.nodeName // "") == "" then "<none>" else .spec.nodeName end)
    ' <<<"$snap" | sort | uniq -c | while read -r count node; do
        echo "  $node: $count pods"
    done

    # Check for scheduling failures. Diagnostics MUST come from the same
    # snapshot the count came from; otherwise a transient Pending pod is
    # already bound by the time `kubectl get` re-runs, and the diagnostic
    # prints "No resources found" — which has misled multiple prior root-
    # cause analyses (see #1090).
    if [[ "$unscheduled_pods" -gt 0 ]]; then
        log_error "=========================================="
        log_error "Scheduling validation FAILED"
        log_error "=========================================="
        log_error "$unscheduled_pods pods could not be scheduled to nodes"
        log_error ""
        log_error "Unscheduled pods:"
        jq -r --arg pat "$exclude_ns" '
            .items[]
            | select(.metadata.namespace as $ns | $pat | split("|") | map(. == $ns) | any | not)
            | select(.status.phase == "Pending")
            | select((.spec.nodeName // "") == "")
            | select((.metadata.ownerReferences // []) | map(.kind) | contains(["Job"]) | not)
            | "  \(.metadata.namespace)/\(.metadata.name)  phase=\(.status.phase)  node=\(.spec.nodeName // "<none>")"
        ' <<<"$snap"
        log_error ""
        # FailedScheduling event dump filtered with the SAME namespace
        # exclusions as the count. The kind control-plane node carries an
        # intentional `nfd-excluded=true:NoSchedule` taint that kube-system
        # pods (coredns, local-path-provisioner) cannot tolerate; those
        # events are persistent harness noise — surfacing them unfiltered
        # misled at least two prior reviews of #1090's symptom signature.
        log_error "Events for unscheduled pods (excluding harness/system namespaces):"
        # `head -50` closes the pipe after 50 lines; if `kubectl get events`
        # is still writing it takes SIGPIPE (exit 141), which under
        # `set -euo pipefail` would abort the function before the trailing
        # diagnostics and `return 1`. Trailing `|| true` keeps the dump
        # best-effort (same pattern as list_bundle_entries' PIPE guard).
        kubectl get events --all-namespaces \
            --field-selector reason=FailedScheduling \
            --sort-by='.lastTimestamp' \
            --no-headers 2>/dev/null \
            | { grep -vE "^(${exclude_ns})\s" || true; } \
            | head -50 || true
        log_error ""
        log_error "Common causes:"
        log_error "  - Node selectors don't match available nodes"
        log_error "  - Tolerations missing for node taints"
        log_error "  - Insufficient resources on nodes"
        log_error "=========================================="
        return 1
    fi

    if [[ "$failed_pods" -gt 0 ]]; then
        log_error "=========================================="
        log_error "Scheduling validation FAILED"
        log_error "=========================================="
        log_error "$failed_pods pods are in Failed state"
        log_error ""
        log_error "Failed pods:"
        jq -r --arg pat "$exclude_ns" '
            .items[]
            | select(.metadata.namespace as $ns | $pat | split("|") | map(. == $ns) | any | not)
            | select(.status.phase == "Failed")
            | "  \(.metadata.namespace)/\(.metadata.name)  reason=\(.status.containerStatuses[0].state.terminated.reason // "Unknown")  message=\(.status.containerStatuses[0].state.terminated.message // "")"
        ' <<<"$snap"
        log_error "=========================================="
        return 1
    fi

    if [[ "$total_pods" -eq 0 ]]; then
        log_warn "No pods were created - bundle may be empty"
        return 0
    fi

    log_info "Scheduling validation PASSED: all $total_pods pods scheduled successfully"
    return 0
}

# Print usage to stderr and exit non-zero.
usage() {
    cat >&2 <<'EOF'
Usage: validate-scheduling.sh [--keep-namespace] [--deployer <name>] <recipe-name>

Flags:
  --keep-namespace        Preserve releases/namespaces after the run.
  --deployer <name>       One of: helm (default), argocd-oci, argocd-helm-oci,
                          argocd-git, flux-oci, flux-git.

Examples:
  validate-scheduling.sh h100-eks-ubuntu-training-kubeflow
  validate-scheduling.sh --keep-namespace eks-training
  validate-scheduling.sh --deployer argocd-oci eks-training
  validate-scheduling.sh --deployer argocd-helm-oci eks-training
  validate-scheduling.sh --deployer flux-oci eks-training
  validate-scheduling.sh --deployer flux-git eks-training
EOF
    exit 1
}

# Main
main() {
    local recipe=""

    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            --keep-namespace)
                KEEP_NAMESPACE=true
                shift
                ;;
            --deployer)
                if [[ $# -lt 2 ]]; then
                    log_error "--deployer requires a value"
                    usage
                fi
                DEPLOYER="$2"
                shift 2
                ;;
            --deployer=*)
                DEPLOYER="${1#--deployer=}"
                shift
                ;;
            -*)
                log_error "Unknown option: $1"
                usage
                ;;
            *)
                recipe="$1"
                shift
                ;;
        esac
    done

    if [[ -z "$recipe" ]]; then
        usage
    fi

    case "$DEPLOYER" in
        helm|argocd-oci|argocd-helm-oci|argocd-git|flux-oci|flux-git)
            ;;
        *)
            log_error "Invalid --deployer value: '${DEPLOYER}'"
            log_error "Must be one of: helm, argocd-oci, argocd-helm-oci, argocd-git, flux-oci, flux-git"
            exit 1
            ;;
    esac

    # Must run before cleanup trap so the trap sees the resolved root name.
    resolve_argocd_root_app
    resolve_flux_root_names "$recipe"

    # Safety: refuse to set up the cleanup trap (which force-finalizes
    # namespaces under KWOK) unless the current kubectl context is a
    # known KWOK Kind cluster with kwok-typed nodes. A misconfigured
    # KUBECONFIG must NOT silently strip finalizers from whatever
    # cluster the user happens to be authenticated to. apply-nodes.sh
    # has already populated the cluster by this point (the script is
    # invoked from run-all-recipes.sh::run_recipe_test after apply-
    # nodes.sh), so the node-label check is safe here.
    ensure_kwok_context

    # Set up cleanup trap
    trap cleanup EXIT

    log_info "=========================================="
    log_info "Starting validation for recipe: $recipe"
    log_info "Deployer: $DEPLOYER"
    log_info "=========================================="

    log_debug "Step 1: Checking dependencies..."
    check_deps

    log_debug "Step 2: Cleaning up old test artifacts..."
    cleanup_old_tests

    # Create temp work directory
    WORK_DIR=$(mktemp -d)
    log_info "Work directory: $WORK_DIR"
    log_info "Test namespace: $NAMESPACE"
    log_info "Helm release: $RELEASE_NAME"

    log_debug "Step 3: Verifying KWOK nodes..."
    verify_kwok_nodes "$recipe"

    log_debug "Step 4: Generating bundle..."
    generate_bundle "$recipe"

    # Exclude all real (non-KWOK) nodes from bundle DaemonSets. On KWOK
    # nodes stage-fast fakes pods into Running; on real Kind nodes,
    # containers actually start and CrashLoop because cloud services
    # (AWS IMDS, GCP metadata, etc.) are unavailable.
    #
    # Two exclusions are applied per node:
    #   1. eks.amazonaws.com/compute-type=hybrid  — the ebs-csi-driver
    #      DaemonSet uses a NotIn affinity that skips fargate/auto/hybrid
    #      nodes. Without this label the Kind node passes the NotIn check.
    #   2. nfd-excluded=true:NoSchedule taint — NFD worker DaemonSet
    #      respects this taint and will not schedule on tainted nodes.
    local cp_nodes
    cp_nodes=$(kubectl get nodes --selector='type!=kwok' -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    if [[ -z "$cp_nodes" ]]; then
        log_warn "No real (non-KWOK) nodes found to exclude — DaemonSet CrashLoops may occur"
    fi
    for cp_node in $cp_nodes; do
        log_info "Excluding real node ($cp_node) from bundle DaemonSets"
        kubectl label node "$cp_node" eks.amazonaws.com/compute-type=hybrid --overwrite
        kubectl taint nodes "$cp_node" nfd-excluded=true:NoSchedule --overwrite
    done

    log_debug "Step 5: Deploying bundle..."
    deploy_bundle

    log_debug "Step 6: Verifying pod scheduling..."
    verify_pods

    log_info "=========================================="
    log_info "✓ Validation PASSED for recipe: $recipe (deployer=$DEPLOYER)"
    log_info "=========================================="
}

main "$@"

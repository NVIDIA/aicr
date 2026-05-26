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
#                         flux-oci         (aicr bundle --deployer flux ->
#                                           OCI push -> apply OCIRepository +
#                                           Kustomization wrappers and let
#                                           Flux reconcile)
#                       For argocd-* values the in-cluster registry + Argo CD
#                       must already be installed; for flux-oci the registry
#                       + Flux 2 controllers must be installed. Both are
#                       managed by `kwok/scripts/install-infra.sh` keyed on
#                       the DEPLOYER env var.
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
#                            failing. Default: 300.
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
#       KWOK_FLUX_SYNC_TIMEOUT for flux-oci). Distinct so run-all-recipes.sh
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
#   - argocd-helm-oci -> "aicr-stack"   (pkg/bundler/deployer/argocdhelm/
#                        argocdhelm.go: parentAppTemplate)
# Default empty when DEPLOYER=helm; cleanup guards against empty before
# issuing `kubectl delete application`.
ARGOCD_ROOT_APP=""

# Resolve ARGOCD_ROOT_APP from DEPLOYER. Must be called before any code path
# that consults ARGOCD_ROOT_APP (cleanup, wait_for_argocd_sync).
resolve_argocd_root_app() {
    case "$DEPLOYER" in
        argocd-oci)      ARGOCD_ROOT_APP="nvidia-stack" ;;
        argocd-helm-oci) ARGOCD_ROOT_APP="aicr-stack"   ;;
        *)               ARGOCD_ROOT_APP=""             ;;
    esac
}

# Flux-specific globals (set in generate_bundle / deploy_bundle for flux-oci).
# Both live in the flux-system namespace (Flux's default control plane).
#
# FLUX_OCIREPOSITORY_NAME is fixed to "aicr-bundle" — the value the bundler
# embeds into local-chart HelmRelease sourceRefs when --deployer flux is
# paired with an OCI --output (see pkg/bundler/config/Config.bundleOCISourceName
# and pkg/cli/bundle.go). Changing the name in one place WITHOUT the other
# breaks chart resolution at reconcile time, so the constant is the contract.
#
# FLUX_KUSTOMIZATION_NAME varies by recipe so back-to-back recipes on the
# same cluster don't collide (one Kustomization per recipe pointing at
# the same shared OCIRepository over time).
#
# Default empty when DEPLOYER != flux-oci; cleanup guards against empty.
FLUX_KUSTOMIZATION_NAME=""
FLUX_OCIREPOSITORY_NAME=""

# Resolve FLUX_* names from the recipe. Must be called before any code path
# that consults them (cleanup, deploy_bundle, wait_for_flux_sync).
resolve_flux_root_names() {
    local recipe="$1"
    if [[ "$DEPLOYER" == "flux-oci" ]]; then
        # Fixed name — see comment above. Must match what
        # pkg/cli/bundle.go sets as BundleOCISourceName.
        FLUX_OCIREPOSITORY_NAME="aicr-bundle"
        FLUX_KUSTOMIZATION_NAME="aicr-${recipe}"
    else
        FLUX_OCIREPOSITORY_NAME=""
        FLUX_KUSTOMIZATION_NAME=""
    fi
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
        # the next recipe gets a clean argocd namespace.
        if [[ "$DEPLOYER" == "argocd-oci" || "$DEPLOYER" == "argocd-helm-oci" ]]; then
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

        # Flux deployer: delete the Kustomization FIRST so its prune
        # GC removes the per-component HelmReleases before namespace
        # cleanup races them. The OCIRepository then has nothing to feed
        # and can be removed safely.
        if [[ "$DEPLOYER" == "flux-oci" ]]; then
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
    for cmd in kubectl helm yq jq; do
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
            # Per PR #1032's contract change (and #1035's enforcement on the
            # parent App template), --set repoURL must carry the PARENT
            # NAMESPACE ONLY — without the per-recipe chart name. The
            # argocd-helm parent Application appends .Chart.Name via its
            # separate `source.chart` field; path-based child Applications
            # append /{{ .Chart.Name }} via their template, so both halves
            # resolve to the same artifact regardless of which Argo source
            # type the cluster picks. The pushed artifact lives at
            # oci://…/aicr/<recipe>:<tag>; the recipe segment is the chart
            # name, which Argo appends itself.
            OCI_IN_CLUSTER_REF="oci://registry.aicr-registry.svc.cluster.local:5000/aicr"
            local in_cluster_repo="$OCI_IN_CLUSTER_REF"

            # Map our deployer-matrix name to aicr's --deployer value.
            local deployer_arg="argocd"
            [[ "$DEPLOYER" == "argocd-helm-oci" ]] && deployer_arg="argocd-helm"

            log_info "Bundling for ${deployer_arg}, pushing to ${OCI_REF}"
            log_info "Argo CD will pull from ${in_cluster_repo}/${recipe}:${tag} (parent namespace + .Chart.Name appended by the parent App)"
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
        argocd-oci)
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

# Assert the deployer's root Argo CD Application exists within
# KWOK_ARGOCD_ROOT_GRACE seconds (default 30). Without this gate the script
# treats "kubectl apply / helm install silently produced no Application"
# (webhook reject, CRD scope mismatch, RBAC denial) as a slow controller
# and burns the full KWOK_ARGOCD_SYNC_TIMEOUT before failing with a
# misleading "deadline hit" message.
#
# On failure, dumps recent events in the argocd namespace and returns 1.
wait_for_argocd_root_app() {
    if [[ -z "$ARGOCD_ROOT_APP" ]]; then
        log_error "ARGOCD_ROOT_APP is empty (DEPLOYER=${DEPLOYER})"
        return 1
    fi

    local grace="${KWOK_ARGOCD_ROOT_GRACE:-30}"
    local start=$SECONDS

    log_info "Waiting for root Application ${ARGOCD_ROOT_APP} to appear (grace ${grace}s)..."

    while (( SECONDS - start < grace )); do
        if kubectl get application "$ARGOCD_ROOT_APP" -n argocd \
                >/dev/null 2>&1; then
            log_info "Root Application ${ARGOCD_ROOT_APP} present"
            return 0
        fi
        sleep 2
    done

    log_error "Root Application ${ARGOCD_ROOT_APP} not present after ${grace}s"
    log_error "kubectl apply / helm install for DEPLOYER=${DEPLOYER} produced no Application resource."
    log_error "--- argocd namespace events (last 20) ---"
    kubectl get events -n argocd --sort-by=.lastTimestamp 2>&1 | tail -20 || true
    return 1
}

# Wait until every Argo CD Application in the argocd namespace reaches a
# terminal pass state. The pass set is intentionally broader than just
# "Synced + Healthy" so KWOK simulation gaps and the real-world
# operator-mutation-after-sync pattern do not mask a healthy deployment:
#
#   1. Synced + Healthy ........................ canonical pass
#   2. Synced + Progressing .................... KWOK fake pods often sit in
#                                                Progressing because the
#                                                health controller wants a
#                                                deeper readiness signal
#                                                that stage-fast cannot
#                                                simulate (DaemonSet pods,
#                                                Prometheus StatefulSet
#                                                under the operator,
#                                                etc.); accepted per
#                                                ADR-008.
#   3. OutOfSync + Healthy + last op succeeded . Operator-mutation drift:
#                                                charts like gpu-operator
#                                                and nvidia-dra-driver-gpu
#                                                ship CRs that their own
#                                                controller updates post-
#                                                sync. Argo CD initiates
#                                                the sync, the operation
#                                                completes ("successfully
#                                                synced (all tasks run)"),
#                                                resources are Healthy,
#                                                then drift reappears as
#                                                the operator reconciles.
#                                                In production these are
#                                                handled with per-App
#                                                ignoreDifferences; for
#                                                KWOK we accept the
#                                                Healthy-post-success
#                                                state as a real pass and
#                                                rely on operationState
#                                                .phase=Succeeded to
#                                                distinguish it from a
#                                                still-broken OutOfSync.
#   4. Synced + Degraded + last op succeeded ... Chart applied but health
#                                                controller marks at
#                                                least one managed
#                                                resource Degraded —
#                                                typically a Pod whose
#                                                Ready signal KWOK cannot
#                                                produce (leader election
#                                                like kai-scheduler,
#                                                charts with strict probe
#                                                semantics) or a Deployment
#                                                that cannot reach
#                                                desired replicas under
#                                                stage-fast. The sync
#                                                operation succeeded — the
#                                                bundle was applied. The
#                                                health gradient is the
#                                                chart's runtime contract
#                                                + KWOK fidelity, not a
#                                                bundle defect. Same
#                                                operationState.phase=
#                                                Succeeded guard as #3
#                                                prevents masking a
#                                                still-broken sync.
#
# Returns 0 on success. On deadline hit OR on a `kubectl get applications`
# hard error (CRD missing, RBAC denied, apiserver unreachable), calls
# dump_argocd_failures and returns 1.
#
# Guards (issue #843 follow-up review):
#
#   MUST-FIX 1 — Asserts the deployer's root Application exists (call site
#   pre-checks via wait_for_argocd_root_app). Then derives the expected
#   set of child Applications from the root's status.resources so a
#   root-only "Synced + Healthy" state cannot masquerade as a true pass
#   (the chart wrapping in argocd-helm-oci can land just the root
#   Application without children if the OCI pull silently degrades).
#
#   MUST-FIX 2 — Distinguishes a `kubectl get applications` non-zero exit
#   (CRD missing, permission denied, apiserver unreachable) from a
#   well-formed empty list. A hard error fails the loop immediately
#   instead of spinning to the deadline.
#
# Deadline: KWOK_ARGOCD_SYNC_TIMEOUT env var (default 300s).
wait_for_argocd_sync() {
    if ! wait_for_argocd_root_app; then
        dump_argocd_failures
        return 1
    fi

    local deadline="${KWOK_ARGOCD_SYNC_TIMEOUT:-300}"
    local start=$SECONDS

    log_info "Waiting for Argo CD Applications to sync (deadline ${deadline}s)..."

    while (( SECONDS - start < deadline )); do
        local apps_json apps_rc total pending root_sync root_health
        # MUST-FIX 2: distinguish a hard error from an empty list. We
        # cannot use command substitution + `|| echo '{"items":[]}'`
        # because that swallows the exit code.
        #
        # Subtle bash semantics: a bare `apps_json=$(kubectl ...)`
        # assignment under `set -euo pipefail` (line 72) DOES trip
        # set -e when the substituted command exits non-zero — bash
        # exits before the next line's `apps_rc=$?` can capture the
        # status. Wrap in an `if cmd; then ok; else capture; fi`
        # block: command-substitution failures inside an `if`
        # condition are exempt from set -e (per the manual), so we
        # get the exit code into `apps_rc` safely.
        if apps_json=$(kubectl get applications -n argocd -o json 2>/dev/null); then
            apps_rc=0
        else
            apps_rc=$?
        fi
        if (( apps_rc != 0 )); then
            log_error "kubectl get applications -n argocd failed (rc=${apps_rc})"
            log_error "Likely cause: applications.argoproj.io CRD missing, RBAC denied, or apiserver unreachable."
            log_error "--- kubectl get applications stderr ---"
            kubectl get applications -n argocd 2>&1 | tail -20 || true
            dump_argocd_failures
            return 1
        fi

        total=$(echo "$apps_json" | jq '.items | length')

        # MUST-FIX 1: root must be Synced+Healthy AND its expected
        # children (status.resources[].name) must all be reconciled. We
        # use status.resources because it self-adjusts per recipe — no
        # static min-count env var to maintain.
        root_sync=$(echo "$apps_json" \
            | jq -r --arg n "$ARGOCD_ROOT_APP" \
                '.items[] | select(.metadata.name == $n) | .status.sync.status // "Unknown"')
        root_health=$(echo "$apps_json" \
            | jq -r --arg n "$ARGOCD_ROOT_APP" \
                '.items[] | select(.metadata.name == $n) | .status.health.status // "Unknown"')

        if [[ "$root_sync" != "Synced" || \
                ( "$root_health" != "Healthy" && "$root_health" != "Progressing" ) ]]; then
            log_debug "Root ${ARGOCD_ROOT_APP} not ready yet (sync=${root_sync} health=${root_health}; total=${total})..."
            sleep 5
            continue
        fi

        # Children expected (per the root's status.resources). For the
        # argocd-oci app-of-apps shape, status.resources lists the child
        # Applications by name. For argocd-helm-oci the wrapper App
        # references chart subresources — fall back to "any non-root
        # Application is a child" if status.resources is empty.
        local expected_children
        expected_children=$(echo "$apps_json" \
            | jq -r --arg n "$ARGOCD_ROOT_APP" '
                .items[]
                | select(.metadata.name == $n)
                | (.status.resources // [])
                | map(select(.kind == "Application"))
                | length')

        # Documented fallback: when status.resources does not list any
        # Application children but other Applications exist in the
        # namespace, treat the count of non-root Applications as the
        # observed-children count. The argocd-helm-oci wrapper's
        # status.resources can take a moment to populate while the
        # children it created are already reconciling; without this
        # fallback the loop sleeps to the deadline even when every
        # child is Synced+Healthy.
        if [[ "$expected_children" -eq 0 ]]; then
            local observed_children
            observed_children=$(echo "$apps_json" \
                | jq -r --arg n "$ARGOCD_ROOT_APP" \
                    '[.items[] | select(.metadata.name != $n)] | length')
            if [[ "$observed_children" -gt 0 ]]; then
                expected_children="$observed_children"
            fi
        fi

        if [[ "$expected_children" -eq 0 ]]; then
            # No children declared by the root yet. If we're the only
            # Application in the namespace and the controller has been
            # given a chance to reconcile, treat as root-only success
            # only when status.resources has resolved (controller has
            # observed the source). Otherwise keep waiting.
            local resources_populated
            resources_populated=$(echo "$apps_json" \
                | jq --arg n "$ARGOCD_ROOT_APP" '
                    .items[]
                    | select(.metadata.name == $n)
                    | (.status.resources // []) | length > 0')
            if [[ "$total" -eq 1 && "$resources_populated" == "true" ]]; then
                log_warn "Root Application ${ARGOCD_ROOT_APP} reconciled with zero child Applications"
                log_warn "Bundle may have been wrapped-only (no expansion). total=${total}"
                # Surfacing as success would mask the bug from MUST-FIX 1.
                # Fail closed: a child-less root is never a real PASS for
                # the deployer matrix (every recipe ships >= 1 component).
                dump_argocd_failures
                return 1
            fi
            log_debug "Root Application present but children not yet declared (total=${total})..."
            sleep 5
            continue
        fi

        # Pending = NOT one of the four terminal-pass states described
        # in the function docstring. Arms 3 & 4 are the load-bearing
        # ones for KWOK: they distinguish "bundle applied; chart-side
        # runtime gap" (pass) from "bundle never applied" (fail) via
        # operationState.phase=Succeeded.
        pending=$(echo "$apps_json" \
            | jq '[.items[]
                  | . as $app
                  | select(
                      # NOT (Synced + Healthy)
                      ($app.status.sync.status != "Synced"
                       or $app.status.health.status != "Healthy")
                      # AND NOT (Synced + Progressing)
                      and ($app.status.sync.status != "Synced"
                           or $app.status.health.status != "Progressing")
                      # AND NOT (OutOfSync + Healthy + last op Succeeded)
                      and ($app.status.sync.status != "OutOfSync"
                           or $app.status.health.status != "Healthy"
                           or ($app.status.operationState.phase // "") != "Succeeded")
                      # AND NOT (Synced + Degraded + last op Succeeded)
                      and ($app.status.sync.status != "Synced"
                           or $app.status.health.status != "Degraded"
                           or ($app.status.operationState.phase // "") != "Succeeded")
                    )]
                  | length')
        if [[ "$pending" == "0" ]]; then
            # MUST-FIX 1: log observed counts so a low total is auditable.
            log_info "Argo CD sync PASS: ${total} Applications reached terminal pass state (root=${ARGOCD_ROOT_APP}, expected_children=${expected_children})"
            return 0
        fi
        log_debug "Argo CD sync progress: ${pending}/${total} Applications still pending (expected_children=${expected_children})..."
        sleep 5
    done

    log_error "Argo CD sync deadline (${deadline}s) hit"
    dump_argocd_failures
    # Distinct rc so run-all-recipes.sh can apply the 3-strike rule without
    # parsing stderr. Other failure paths in this function still return 1.
    return "$EXIT_ARGOCD_SYNC_TIMEOUT"
}

# Dump diagnostics on Argo CD sync failure: per-Application status fields
# plus repo-server and application-controller log tails.
dump_argocd_failures() {
    log_error "--- Argo CD Applications (name / sync / health / conditions / op.message) ---"
    kubectl get applications -n argocd -o json 2>/dev/null \
        | jq -r '.items[] | {
            name: .metadata.name,
            sync: .status.sync.status,
            health: .status.health.status,
            conditions: .status.conditions,
            operation: .status.operationState.message
          }' \
        2>&1 || true
    log_error "--- argocd-repo-server (tail=200) ---"
    kubectl logs -n argocd deploy/argocd-repo-server --tail=200 2>&1 || true
    log_error "--- argocd-application-controller (tail=200) ---"
    # Argo CD chart 9.x ships application-controller as a StatefulSet. Try
    # StatefulSet first to avoid emitting a misleading "Deployment not
    # found" error into the diagnostic dump on the common case; fall back
    # to Deployment for older chart versions.
    kubectl logs -n argocd statefulset/argocd-application-controller --tail=200 2>&1 \
        || kubectl logs -n argocd deploy/argocd-application-controller --tail=200 2>&1 \
        || true
}

# Assert the outer Flux Kustomization exists within KWOK_FLUX_ROOT_GRACE
# seconds (default 30). Mirrors wait_for_argocd_root_app: a silently-missing
# Kustomization (RBAC denial, CRD scope mismatch, webhook reject) gets
# surfaced fast instead of burning the KWOK_FLUX_SYNC_TIMEOUT budget.
wait_for_flux_root_app() {
    if [[ -z "$FLUX_KUSTOMIZATION_NAME" ]]; then
        log_error "FLUX_KUSTOMIZATION_NAME is empty (DEPLOYER=${DEPLOYER})"
        return 1
    fi

    local grace="${KWOK_FLUX_ROOT_GRACE:-30}"
    local start=$SECONDS

    log_info "Waiting for Flux Kustomization ${FLUX_KUSTOMIZATION_NAME} to appear (grace ${grace}s)..."

    while (( SECONDS - start < grace )); do
        if kubectl get kustomization "$FLUX_KUSTOMIZATION_NAME" -n flux-system \
                >/dev/null 2>&1; then
            log_info "Kustomization ${FLUX_KUSTOMIZATION_NAME} present"
            return 0
        fi
        sleep 2
    done

    log_error "Kustomization ${FLUX_KUSTOMIZATION_NAME} not present after ${grace}s"
    log_error "kubectl apply for DEPLOYER=${DEPLOYER} produced no Kustomization resource."
    log_error "--- flux-system namespace events (last 20) ---"
    kubectl get events -n flux-system --sort-by=.lastTimestamp 2>&1 | tail -20 || true
    return 1
}

# Validate the flux-oci bundle was successfully applied and all HelmReleases
# reconciled. Local-chart components (manifest-only, pre-manifests, vendored
# wrappers) use Flux's ArtifactGenerator to extract sub-directories from the
# outer OCIRepository into ExternalArtifacts, referenced via HelmRelease
# spec.chartRef. This eliminates the former placeholder GitRepository URL
# and allows full reconciliation.
#
# What this lane validates:
#   1. Flux source-controller could PULL the AICR bundle artifact (the layer
#      mediaType + insecure HTTP wiring is correct).
#   2. Flux kustomize-controller could PARSE the bundle's root
#      kustomization.yaml and APPLY all manifests (no template-render bugs,
#      no missing files, no schema violations).
#   3. All HelmRelease CRs reach Ready=True — helm-controller could resolve
#      chart sources (ExternalArtifact for local charts, HelmRepository for
#      upstream charts) and reconcile each release.
#   4. When present, ArtifactGenerator CRs reach Ready=True — proves
#      source-watcher extracted local-chart sub-directories from the OCI
#      bundle into ExternalArtifacts that helm-controller can consume.
#
# Returns 0 on success, EXIT_ARGOCD_SYNC_TIMEOUT (50) on deadline (reused so
# run-all-recipes.sh's 3-strike rule fires symmetrically across argocd-* and
# flux-oci), and 1 on hard errors (CRD missing, RBAC denial, apiserver
# unreachable).
wait_for_flux_sync() {
    local deadline="${KWOK_FLUX_SYNC_TIMEOUT:-500}"
    local start=$SECONDS

    if ! wait_for_flux_root_app; then
        return 1
    fi

    log_info "Waiting for Flux Kustomization to apply the bundle (deadline ${deadline}s)..."

    local kust_json oci_json hr_json kust_ready oci_ready total
    local applied_rev
    local apps_rc

    while (( SECONDS - start < deadline )); do
        # 1. OCIRepository must reach Ready=True — proves source-controller
        # could PULL the bundle artifact (mediaType, insecure-HTTP, layer
        # selector all wired correctly). This is the artifact-shape
        # regression surface argocd-oci catches via repo-server; flux-oci
        # catches the same class of bug via source-controller.
        if oci_json=$(kubectl get ocirepository "$FLUX_OCIREPOSITORY_NAME" \
                -n flux-system -o json 2>/dev/null); then
            apps_rc=0
        else
            apps_rc=$?
        fi
        if (( apps_rc != 0 )); then
            log_error "kubectl get ocirepository failed (rc=${apps_rc})"
            log_error "Likely cause: OCIRepository CRD missing, RBAC denied, or apiserver unreachable."
            dump_flux_failures
            return 1
        fi
        oci_ready=$(echo "$oci_json" \
            | jq -r '.status.conditions[]? | select(.type == "Ready") | .status // "Unknown"')
        if [[ "$oci_ready" != "True" ]]; then
            log_debug "OCIRepository ${FLUX_OCIREPOSITORY_NAME} not Ready yet (status=${oci_ready:-Unknown})..."
            sleep 5
            continue
        fi

        # 2. Kustomization must reach Ready=True — proves kustomize-controller
        # could PARSE the bundle's root kustomization.yaml and APPLY all
        # manifests inside (no template-render bugs, no missing files, no
        # schema violations).
        if kust_json=$(kubectl get kustomization "$FLUX_KUSTOMIZATION_NAME" \
                -n flux-system -o json 2>/dev/null); then
            apps_rc=0
        else
            apps_rc=$?
        fi
        if (( apps_rc != 0 )); then
            log_error "kubectl get kustomization failed (rc=${apps_rc})"
            log_error "Likely cause: Kustomization CRD missing, RBAC denied, or apiserver unreachable."
            dump_flux_failures
            return 1
        fi
        kust_ready=$(echo "$kust_json" \
            | jq -r '.status.conditions[]? | select(.type == "Ready") | .status // "Unknown"')
        if [[ "$kust_ready" != "True" ]]; then
            log_debug "Kustomization ${FLUX_KUSTOMIZATION_NAME} not Ready yet (status=${kust_ready:-Unknown})..."
            sleep 5
            continue
        fi
        applied_rev=$(echo "$kust_json" \
            | jq -r '.status.lastAppliedRevision // ""')

        # 3. All HelmRelease CRs must reach Ready=True — proves
        # helm-controller could reconcile each release (chart sources
        # resolved via ExternalArtifact/HelmRepository, values applied,
        # install/upgrade succeeded).
        if hr_json=$(kubectl get helmrelease -A -o json 2>/dev/null); then
            apps_rc=0
        else
            apps_rc=$?
        fi
        if (( apps_rc != 0 )); then
            log_error "kubectl get helmrelease -A failed (rc=${apps_rc})"
            dump_flux_failures
            return 1
        fi
        total=$(echo "$hr_json" | jq '.items | length')
        if [[ "$total" -eq 0 ]]; then
            log_debug "Kustomization Ready but no HelmReleases yet — waiting for resources to appear..."
            sleep 5
            continue
        fi

        local hr_not_ready
        hr_not_ready=$(echo "$hr_json" | jq '[.items[] | select(
            (.status.conditions // []) | map(select(.type == "Ready" and .status == "True")) | length == 0
        )] | length')
        if [[ "$hr_not_ready" -gt 0 ]]; then
            log_debug "${hr_not_ready}/${total} HelmReleases not Ready yet — waiting..."
            sleep 5
            continue
        fi

        # 4. When ArtifactGenerator CRs are present (OCI mode with local-chart
        # components), verify they reach Ready=True — proves source-watcher
        # could extract sub-directories from the OCI bundle into
        # ExternalArtifacts. This is the regression surface for issue #964.
        local ag_json ag_total ag_not_ready ag_rc
        ag_json=$(kubectl get artifactgenerator -A -o json 2>&1) && ag_rc=0 || ag_rc=$?
        if (( ag_rc != 0 )); then
            log_error "kubectl get artifactgenerator failed (rc=${ag_rc}): ${ag_json}"
            return 1
        fi
        ag_total=$(echo "$ag_json" | jq '.items | length')
        if [[ "$ag_total" -gt 0 ]]; then
            ag_not_ready=$(echo "$ag_json" | jq '[.items[] | select(
                (.status.conditions // []) | map(select(.type == "Ready" and .status == "True")) | length == 0
            )] | length')
            if [[ "$ag_not_ready" -gt 0 ]]; then
                log_debug "${ag_not_ready}/${ag_total} ArtifactGenerators not Ready yet — waiting..."
                sleep 5
                continue
            fi
            log_info "All ${ag_total} ArtifactGenerators Ready (local-chart OCI extraction verified)"
        fi

        log_info "Flux bundle-apply PASS: OCIRepository pulled (rev=$(echo "$oci_json" | jq -r '.status.artifact.revision // "unknown"')), Kustomization applied (rev=${applied_rev}), ${total}/${total} HelmReleases Ready"
        return 0
    done

    log_error "Flux bundle-apply deadline (${deadline}s) hit"
    dump_flux_failures
    # Reuse EXIT_ARGOCD_SYNC_TIMEOUT so run-all-recipes.sh's 3-strike rule
    # treats GitOps sync deadlines symmetrically across argocd-* and flux-oci.
    return "$EXIT_ARGOCD_SYNC_TIMEOUT"
}

# Dump diagnostics on Flux sync failure: outer Kustomization status, all
# OCIRepository + HelmRelease statuses, plus controller log tails.
dump_flux_failures() {
    log_error "--- Flux Kustomization ${FLUX_KUSTOMIZATION_NAME} ---"
    kubectl get kustomization "$FLUX_KUSTOMIZATION_NAME" -n flux-system \
        -o json 2>/dev/null \
        | jq -r '{
            ready: ((.status.conditions // []) | map(select(.type == "Ready")) | .[0]),
            lastAppliedRevision: .status.lastAppliedRevision,
            inventory: ((.status.inventory.entries // []) | length)
          }' 2>&1 || true
    log_error "--- Flux OCIRepositories ---"
    kubectl get ocirepository -A -o json 2>/dev/null \
        | jq -r '.items[] | {
            namespace: .metadata.namespace,
            name: .metadata.name,
            ready: ((.status.conditions // []) | map(select(.type == "Ready")) | .[0]),
            artifact: .status.artifact.revision
          }' 2>&1 || true
    log_error "--- Flux HelmReleases (name / ready / released / stalled / hist[0]) ---"
    kubectl get helmrelease -A -o json 2>/dev/null \
        | jq -r '.items[] | {
            namespace: .metadata.namespace,
            name: .metadata.name,
            ready: ((.status.conditions // []) | map(select(.type == "Ready")) | .[0].status),
            released: ((.status.conditions // []) | map(select(.type == "Released")) | .[0].status),
            stalled: ((.status.conditions // []) | map(select(.type == "Stalled")) | .[0].status),
            history: (.status.history // [])[0]
          }' 2>&1 || true
    log_error "--- Flux ArtifactGenerators ---"
    kubectl get artifactgenerator -A -o json 2>/dev/null \
        | jq -r '.items[] | {
            namespace: .metadata.namespace,
            name: .metadata.name,
            ready: ((.status.conditions // []) | map(select(.type == "Ready")) | .[0].status)
          }' 2>&1 || true
    log_error "--- Flux ExternalArtifacts ---"
    kubectl get externalartifact -A -o json 2>/dev/null \
        | jq -r '.items[] | {
            namespace: .metadata.namespace,
            name: .metadata.name,
            ready: ((.status.conditions // []) | map(select(.type == "Ready")) | .[0].status),
            artifact: .status.artifact.revision
          }' 2>&1 || true
    log_error "--- source-controller (tail=200) ---"
    kubectl logs -n flux-system deploy/source-controller --tail=200 2>&1 || true
    log_error "--- kustomize-controller (tail=200) ---"
    kubectl logs -n flux-system deploy/kustomize-controller --tail=200 2>&1 || true
    log_error "--- helm-controller (tail=200) ---"
    kubectl logs -n flux-system deploy/helm-controller --tail=200 2>&1 || true
    log_error "--- source-watcher (tail=200) ---"
    kubectl logs -n flux-system deploy/source-watcher --tail=200 2>&1 || true
}

# Verify pod scheduling
verify_pods() {
    log_info "Verifying pod scheduling..."

    # Get pod status across all component namespaces (per-component deployment)
    # Exclude system namespaces that aren't part of our bundle (control-plane
    # infra plus GitOps controller namespaces — argocd/flux-system/aicr-
    # registry are owned by install-infra.sh, not the bundle under test).
    local ns_filter="--all-namespaces"
    local exclude_ns="kube-system|kube-node-lease|kube-public|local-path-storage|kwok-system|argocd|flux-system|aicr-registry"

    local total_pods pending_pods failed_pods running_pods unscheduled_pods
    total_pods=$(kubectl get pods ${ns_filter} --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')
    pending_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Pending --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')
    failed_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Failed --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')
    running_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Running --no-headers 2>/dev/null | { grep -vE "^(${exclude_ns})\s" || true; } | wc -l | tr -d ' ')

    # Count truly unscheduled pods (Pending with no node assigned)
    # Pods in ContainerCreating are Pending but scheduled - they have a node
    # Exclude cleanup/webhook Jobs - these are Helm hooks that may not have proper tolerations
    # Use awk to count lines, avoiding issues with empty output or newlines
    unscheduled_pods=$(kubectl get pods ${ns_filter} --field-selector=status.phase=Pending \
        -o json 2>/dev/null | \
        jq -r '.items[] | select(.metadata.namespace as $ns | "'${exclude_ns}'" | split("|") | map(. == $ns) | any | not) | select(.spec.nodeName == null or .spec.nodeName == "") | select(.metadata.ownerReferences == null or (.metadata.ownerReferences | map(.kind) | contains(["Job"]) | not)) | .metadata.name' | \
        awk 'NF {count++} END {print count+0}')

    log_info "Pod status: $total_pods total, $running_pods running, $pending_pods pending ($unscheduled_pods unscheduled), $failed_pods failed"

    # Show pod distribution across nodes.
    # grep -vE returns exit 1 when no lines match (e.g. all pods are in
    # excluded namespaces — happens on the flux-oci lane where every pod
    # the controller has placed so far lives in flux-system / kube-system).
    # Under `set -euo pipefail` that exit 1 propagates as the pipeline's
    # exit code and kills the script silently AFTER Pod-status logging,
    # masquerading as a pass-then-fail. Wrap with `{ ... || true; }` so
    # an empty result is a no-op, matching the total_pods / unscheduled_pods
    # extraction above.
    log_info "Pod distribution:"
    kubectl get pods --all-namespaces -o wide --no-headers 2>/dev/null | \
        { grep -vE "^(${exclude_ns})\s" || true; } | \
        awk '{print $8}' | sort | uniq -c | \
        while read -r count node; do
            echo "  $node: $count pods"
        done

    # Check for scheduling failures - only fail if pods are truly unscheduled (no node assigned)
    # Pods in ContainerCreating on real nodes are scheduled but waiting for container start
    if [[ "$unscheduled_pods" -gt 0 ]]; then
        log_error "=========================================="
        log_error "Scheduling validation FAILED"
        log_error "=========================================="
        log_error "$unscheduled_pods pods could not be scheduled to nodes"
        log_error ""
        log_error "Unscheduled pods:"
        kubectl get pods --all-namespaces --field-selector=status.phase=Pending -o wide | \
            awk 'NR==1 || $8=="<none>"'
        log_error ""
        log_error "Events for unscheduled pods:"
        kubectl get events --all-namespaces --field-selector reason=FailedScheduling --sort-by='.lastTimestamp'
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
        kubectl get pods --all-namespaces --field-selector=status.phase=Failed -o wide
        log_error ""
        log_error "Pod failure details:"
        kubectl get pods --all-namespaces --field-selector=status.phase=Failed -o json | \
            jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name): \(.status.containerStatuses[0].state.terminated.reason // "Unknown") - \(.status.containerStatuses[0].state.terminated.message // "")"'
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
  --deployer <name>       One of: helm (default), argocd-oci, argocd-helm-oci.

Examples:
  validate-scheduling.sh h100-eks-ubuntu-training-kubeflow
  validate-scheduling.sh --keep-namespace eks-training
  validate-scheduling.sh --deployer argocd-oci eks-training
  validate-scheduling.sh --deployer argocd-helm-oci eks-training
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
        helm|argocd-oci|argocd-helm-oci|flux-oci)
            ;;
        *)
            log_error "Invalid --deployer value: '${DEPLOYER}'"
            log_error "Must be one of: helm, argocd-oci, argocd-helm-oci, flux-oci"
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

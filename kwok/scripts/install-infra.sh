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

# install-infra.sh - Install in-cluster OCI registry + the controllers the
# selected DEPLOYER needs (Argo CD or Flux) for the KWOK deployer-matrix CI
# lane (issue #843).
#
# Idempotent: re-running is a no-op when state already matches. All versions
# are sourced from .settings.yaml (`testing_tools.registry_image`,
# `testing_tools.argocd_chart`, `testing_tools.flux_version`,
# `testing_tools.gitea_image`) — never hardcoded.
#
# Components installed (controller installs branch on $DEPLOYER):
#   1. ALWAYS: OCI registry Deployment (image pinned via .settings.yaml
#      `testing_tools.registry_image`) + NodePort Service in the
#      `aicr-registry` namespace. Service is exposed on nodePort 30500;
#      kwok/kind-config.yaml maps host port 5500 -> nodePort 30500 so
#      `aicr bundle --output oci://localhost:5500/...` works from the
#      runner. Host port 5500 is used (not the more conventional 5000)
#      because Apple ControlCenter holds 5000 on macOS by default; Linux
#      CI runners are unaffected.
#   2. DEPLOYER=argocd-*: Argo CD via Helm, chart pinned to .settings.yaml.
#      Server runs with `server.insecure=true` (no TLS termination needed
#      inside the cluster). RepoCreds template secret
#      `aicr-oci-repo-creds` with `insecureOCIForceHttp: "true"` so Argo
#      CD's repo-server pulls the plain-HTTP in-cluster registry over
#      HTTP. Phase 0 spike confirmed `repo-creds` shape does NOT propagate
#      this field for OCI repos — see docs/plans/2026-05-18-kwok-deployer-
#      matrix.md (F4).
#   3. DEPLOYER=flux-oci|flux-git: Flux 2 install manifest applied from the
#      pinned GitHub release; we only need source-controller
#      (OCIRepository/GitRepository), kustomize-controller (Kustomization
#      wrapper) and helm-controller (HelmRelease per AICR component).
#      notification-controller and image-reflector-controller are tolerated
#      but unused. Flux does NOT require a separate repo-creds-style secret
#      for the in-cluster registry: per-OCIRepository `spec.insecure: true`
#      toggles plain-HTTP at the source level.
#   4. DEPLOYER=flux-git|argocd-git: Gitea Deployment + NodePort Service in
#      the `aicr-registry` namespace (issue #963), mirroring the registry
#      pattern. Service is exposed on nodePort 30300; kwok/kind-config.yaml
#      maps host port 3300 -> nodePort 30300 so the runner can `git push`
#      the filesystem bundle. Push-to-create is enabled and created repos
#      are PUBLIC so the GitOps controller (Flux's source-controller or
#      Argo CD's repo-server) clones anonymously — the bundler's git
#      source carries no credentials. argocd-git installs BOTH Argo CD
#      (step 2) AND Gitea (this step).
#
# Usage:
#   DEPLOYER=helm|argocd-oci|argocd-helm-oci|argocd-git|flux-oci|flux-git ./install-infra.sh
#   (DEPLOYER defaults to "helm"; under helm only the registry is installed,
#    which is harmless if unused.)
#
# Environment variables:
#   KUBECTL_CONTEXT          (optional) - kubectl context to use; defaults to
#                                         the current context if unset.
#   KWOK_REGISTRY_HOST_PORT  (optional, default 5500) - Host port the script
#                                         probes when verifying the registry is
#                                         reachable. MUST match the
#                                         `extraPortMappings[*].hostPort` in
#                                         the Kind cluster config — this script
#                                         does not edit Kind config, so the
#                                         variable only adjusts the local
#                                         reachability check, not the actual
#                                         port mapping. The default matches
#                                         `kwok/kind-config.yaml`; override
#                                         only when running against a
#                                         non-standard cluster topology. The
#                                         in-cluster NodePort (30500) and the
#                                         Service containerPort (5000) are
#                                         hardcoded and not affected by this
#                                         variable.
#
# Environment variables:
#   DEPLOYER                 (optional, default "helm") - Selects which
#                                         controller(s) to install on top of
#                                         the registry. Recognized values:
#                                           helm             - registry only
#                                           argocd-oci       - registry + Argo CD
#                                           argocd-helm-oci  - registry + Argo CD
#                                           argocd-git       - registry + Argo CD + Gitea
#                                           flux-oci         - registry + Flux 2
#                                           flux-git         - registry + Flux 2 + Gitea
#                                         Unknown values fall back to the
#                                         registry-only path; the caller is
#                                         expected to validate first.
#   KWOK_GITEA_HOST_PORT     (optional, default 3300) - Host port the script
#                                         probes when verifying Gitea is
#                                         reachable. Same caveat as
#                                         KWOK_REGISTRY_HOST_PORT: must match
#                                         the Kind extraPortMappings hostPort.
#   KWOK_GITEA_USER          (optional, default "aicr") - Gitea admin user the
#                                         flux-git / argocd-git lanes push as.
#   KWOK_GITEA_PASSWORD      (optional, default "aicr-kwok-ci") - Password for
#                                         KWOK_GITEA_USER. CI-only credential
#                                         for the ephemeral in-cluster Gitea —
#                                         not a secret.
#
# Exit codes (distinct so callers can branch on failure mode):
#    0  success
#   10  `yq` missing or required .settings.yaml field absent
#   20  registry Deployment not Ready in 120s
#   21  registry not reachable on host port within 60s
#   30  Argo CD Helm install failed
#   31  `applications.argoproj.io` CRD not Established in 120s
#   40  Repository secret apply failed
#   60  Flux install manifest apply failed
#   61  Flux controller (source/kustomize/helm) not Ready in 180s
#   62  Flux CRDs (OCIRepository/Kustomization/HelmRelease) not Established in 60s
#   70  Gitea Deployment not Ready in 120s
#   71  Gitea not reachable on host port within 60s
#   72  Gitea admin user bootstrap failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${KWOK_DIR}/.." && pwd)"
SETTINGS_FILE="${REPO_ROOT}/.settings.yaml"

# Colors for output (match validate-scheduling.sh / install-karpenter-kwok.sh)
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_debug() { echo -e "${BLUE}[DEBUG]${NC} $*"; }

# Configuration
REGISTRY_NAMESPACE="aicr-registry"
REGISTRY_NAME="registry"
REGISTRY_PORT=5000
REGISTRY_NODEPORT=30500
REGISTRY_HOST_PORT="${KWOK_REGISTRY_HOST_PORT:-5500}"

ARGOCD_NAMESPACE="argocd"
ARGOCD_RELEASE="argocd"
ARGOCD_REPO_NAME="argo"
ARGOCD_REPO_URL="https://argoproj.github.io/argo-helm"
ARGOCD_HELM_TIMEOUT="5m"

# Gitea configuration (flux-git lane, issue #963). Lives in the registry
# namespace so the existing system-namespace cleanup allowlist covers it.
GITEA_NAME="gitea"
GITEA_PORT=3000
GITEA_NODEPORT=30300
GITEA_HOST_PORT="${KWOK_GITEA_HOST_PORT:-3300}"
GITEA_USER="${KWOK_GITEA_USER:-aicr}"
GITEA_PASSWORD="${KWOK_GITEA_PASSWORD:-aicr-kwok-ci}"

# Flux 2 configuration. install.yaml is fetched from the pinned GitHub
# release; only source/kustomize/helm controllers are required by the
# flux-oci lane, but the upstream manifest also installs notification-
# controller and image-reflector-controller — they're tolerated.
FLUX_NAMESPACE="flux-system"
FLUX_CONTROLLERS=(source-controller kustomize-controller helm-controller source-watcher)
FLUX_REQUIRED_CRDS=(
    ocirepositories.source.toolkit.fluxcd.io
    kustomizations.kustomize.toolkit.fluxcd.io
    helmreleases.helm.toolkit.fluxcd.io
    artifactgenerators.source.extensions.fluxcd.io
    externalartifacts.source.toolkit.fluxcd.io
)

# Selected deployer (env var). Branches controller install in main().
DEPLOYER="${DEPLOYER:-helm}"

# Wrapper that honors KUBECTL_CONTEXT when set; otherwise uses current context.
kc() {
    if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
        kubectl --context="${KUBECTL_CONTEXT}" "$@"
    else
        kubectl "$@"
    fi
}

hc() {
    if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
        helm --kube-context "${KUBECTL_CONTEXT}" "$@"
    else
        helm "$@"
    fi
}

# Read a required field from .settings.yaml via yq. Exit 10 if yq is missing
# or the field is empty / null / absent.
read_setting() {
    local field="$1"
    if ! command -v yq &>/dev/null; then
        log_error "yq is required but not installed"
        log_error "Missing field: ${field}"
        exit 10
    fi
    local value
    value=$(yq eval "${field} // \"\"" "${SETTINGS_FILE}" 2>/dev/null || echo "")
    if [[ -z "${value}" || "${value}" == "null" ]]; then
        log_error "Required field ${field} is missing or empty in ${SETTINGS_FILE}"
        exit 10
    fi
    echo "${value}"
}

# Diagnostic dump for registry failures.
dump_registry_diagnostics() {
    log_error "--- registry diagnostics ---"
    kc describe deployment -n "${REGISTRY_NAMESPACE}" "${REGISTRY_NAME}" 2>&1 || true
    log_error "--- registry pod logs (tail=200) ---"
    kc logs -n "${REGISTRY_NAMESPACE}" "deploy/${REGISTRY_NAME}" --tail=200 2>&1 || true
}

# Diagnostic dump for Gitea failures.
dump_gitea_diagnostics() {
    log_error "--- gitea diagnostics ---"
    kc describe deployment -n "${REGISTRY_NAMESPACE}" "${GITEA_NAME}" 2>&1 || true
    log_error "--- gitea pod logs (tail=200) ---"
    kc logs -n "${REGISTRY_NAMESPACE}" "deploy/${GITEA_NAME}" --tail=200 2>&1 || true
}

# Diagnostic dump for Argo CD failures.
dump_argocd_diagnostics() {
    log_error "--- argocd deployments ---"
    kc describe deployment -n "${ARGOCD_NAMESPACE}" 2>&1 || true
    log_error "--- argocd events (last 20) ---"
    kc get events -n "${ARGOCD_NAMESPACE}" --sort-by=.lastTimestamp 2>&1 | tail -20 || true
}

# Diagnostic dump for Flux controller failures.
dump_flux_diagnostics() {
    log_error "--- flux-system deployments ---"
    kc describe deployment -n "${FLUX_NAMESPACE}" 2>&1 || true
    log_error "--- flux-system pods ---"
    kc get pods -n "${FLUX_NAMESPACE}" -o wide 2>&1 || true
    log_error "--- flux-system events (last 20) ---"
    kc get events -n "${FLUX_NAMESPACE}" --sort-by=.lastTimestamp 2>&1 | tail -20 || true
    log_error "--- flux CRDs ---"
    kc get crd -l app.kubernetes.io/instance=flux-system 2>&1 || true
    log_error "--- source-watcher logs (last 50) ---"
    kc logs -n "${FLUX_NAMESPACE}" deploy/source-watcher --tail=50 2>&1 || true
}

# -------------------------------------------------------------------
# Step 1: Install in-cluster registry:2
# -------------------------------------------------------------------
install_registry() {
    local image="$1"

    log_info "Installing in-cluster OCI registry (${image}) in namespace ${REGISTRY_NAMESPACE}..."

    # Namespace — apply, not create, so re-runs upsert cleanly.
    kc apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${REGISTRY_NAMESPACE}
  labels:
    app.kubernetes.io/name: aicr-registry
    app.kubernetes.io/managed-by: install-infra.sh
EOF

    # Deployment + NodePort Service.
    kc apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${REGISTRY_NAME}
  namespace: ${REGISTRY_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${REGISTRY_NAME}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ${REGISTRY_NAME}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ${REGISTRY_NAME}
    spec:
      containers:
        - name: ${REGISTRY_NAME}
          image: ${image}
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: ${REGISTRY_PORT}
              protocol: TCP
          env:
            - name: REGISTRY_HTTP_ADDR
              value: "0.0.0.0:${REGISTRY_PORT}"
          readinessProbe:
            httpGet:
              path: /v2/
              port: http
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /v2/
              port: http
            initialDelaySeconds: 10
            periodSeconds: 10
---
apiVersion: v1
kind: Service
metadata:
  name: ${REGISTRY_NAME}
  namespace: ${REGISTRY_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${REGISTRY_NAME}
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: ${REGISTRY_NAME}
  ports:
    - name: http
      port: ${REGISTRY_PORT}
      targetPort: ${REGISTRY_PORT}
      nodePort: ${REGISTRY_NODEPORT}
      protocol: TCP
EOF

    log_info "Waiting for registry Deployment to become Ready (timeout 120s)..."
    if ! kc rollout status deployment/"${REGISTRY_NAME}" \
            -n "${REGISTRY_NAMESPACE}" --timeout=120s; then
        log_error "Registry Deployment did not become Ready within 120s"
        dump_registry_diagnostics
        exit 20
    fi

    # Probe the host port that Kind maps to NodePort 30500. KWOK_REGISTRY_HOST_PORT
    # only changes which port this probe waits on — it does NOT change Kind's
    # port mapping (that's hardcoded in kwok/kind-config.yaml). If you override
    # this variable, you must also have created the Kind cluster with a matching
    # `extraPortMappings[*].hostPort`, otherwise this probe will hang for 60s.
    log_info "Waiting for http://localhost:${REGISTRY_HOST_PORT}/v2/ to respond (timeout 60s)..."
    local waited=0
    local interval=2
    while (( waited < 60 )); do
        if curl -sf -o /dev/null --max-time 3 \
                "http://localhost:${REGISTRY_HOST_PORT}/v2/"; then
            log_info "Registry reachable at http://localhost:${REGISTRY_HOST_PORT}/v2/"
            return 0
        fi
        sleep "${interval}"
        waited=$(( waited + interval ))
    done

    log_error "Registry not reachable on host port ${REGISTRY_HOST_PORT} within 60s"
    dump_registry_diagnostics
    log_error "--- node port-mapping sanity ---"
    kc get nodes -o wide 2>&1 || true
    exit 21
}

# -------------------------------------------------------------------
# Step 2: Install Argo CD via Helm
# -------------------------------------------------------------------
install_argocd() {
    local chart_version="$1"

    log_info "Installing Argo CD (chart ${chart_version}) in namespace ${ARGOCD_NAMESPACE}..."

    if ! helm repo add "${ARGOCD_REPO_NAME}" "${ARGOCD_REPO_URL}" --force-update >/dev/null 2>&1; then
        log_warn "helm repo add returned non-zero; continuing (repo may already exist)"
    fi
    helm repo update >/dev/null

    # `server.insecure=true` disables TLS on the API server. Suitable for an
    # in-cluster CI install — not for production. The `\.` escape in the key
    # is required so Helm preserves the dot in `server.insecure`. Use
    # `--set-string` (not `--set`) so the value renders as the literal string
    # "true" in the `argocd-cmd-params-cm` ConfigMap — `configs.params` is a
    # stringly-typed map and the chart's `tpl` step drops bool-typed values,
    # which would leave the API server without `--insecure`.
    if ! hc upgrade --install "${ARGOCD_RELEASE}" "${ARGOCD_REPO_NAME}/argo-cd" \
            --namespace "${ARGOCD_NAMESPACE}" --create-namespace \
            --version "${chart_version}" \
            --set-string 'configs.params.server\.insecure=true' \
            --wait --timeout "${ARGOCD_HELM_TIMEOUT}"; then
        log_error "Argo CD Helm install failed"
        dump_argocd_diagnostics
        exit 30
    fi

    log_info "Waiting for applications.argoproj.io CRD to be Established (timeout 120s)..."
    if ! kc wait --for=condition=Established \
            crd/applications.argoproj.io --timeout=120s; then
        log_error "applications.argoproj.io CRD did not reach Established within 120s"
        kc describe crd applications.argoproj.io 2>&1 || true
        exit 31
    fi

    log_info "Argo CD ready"
}

# -------------------------------------------------------------------
# Step 3: Apply RepoCreds template for the in-cluster OCI registry.
#
# IMPORTANT: secret-type MUST be `repo-creds` (NOT `repository`).
# Repository-type secrets in Argo CD use EXACT URL match
# (util/db/repository_secrets.go::getRepositorySecret → git.SameURL).
# Different recipes produce different repoURLs (oci://…/aicr/<recipe>),
# so a single Repository secret cannot cover them all. RepoCreds-type
# secrets use longest-prefix match
# (util/db/repository_secrets.go::getRepositoryCredentialIndex →
# strings.HasPrefix), so one template at the …/aicr prefix covers
# every recipe pushed by validate-scheduling.sh. Argo CD enriches the
# synthetic Repository for each Application with this template's
# insecureOCIForceHttp=true, forcing plain HTTP for the in-cluster
# registry. The Phase 0 spike on chart 7.7.0 (= app v2.13) saw
# repo-creds + type=helm + enableOCI=true NOT propagate
# insecureOCIForceHttp; chart 9.5.x (= app v3.x) with type=oci does.
# -------------------------------------------------------------------
apply_repo_secret() {
    log_info "Applying Argo CD repo-creds template aicr-oci-repo-creds (type=oci, insecureOCIForceHttp=true)..."

    # Best-effort cleanup of the older repository-typed secret from
    # previous install-infra runs so an idempotent re-run converges
    # cleanly on the prefix-match shape.
    kc delete secret -n "${ARGOCD_NAMESPACE}" aicr-oci-repo --ignore-not-found >/dev/null 2>&1 || true

    if ! kc apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: aicr-oci-repo-creds
  namespace: ${ARGOCD_NAMESPACE}
  labels:
    argocd.argoproj.io/secret-type: repo-creds
stringData:
  type: oci
  url: oci://registry.${REGISTRY_NAMESPACE}.svc.cluster.local:${REGISTRY_PORT}/aicr
  insecureOCIForceHttp: "true"
EOF
    then
        log_error "Failed to apply repo-creds template aicr-oci-repo-creds"
        kc get secret -n "${ARGOCD_NAMESPACE}" aicr-oci-repo-creds -o yaml 2>&1 || true
        exit 40
    fi

    log_info "Repo-creds template applied (prefix-matches every oci://…/aicr/<recipe>)"
}

# -------------------------------------------------------------------
# Step 4 (flux-oci only): Install Flux 2 controllers via the pinned
# release's install.yaml.
#
# Why install.yaml and not the community Helm chart: install.yaml is the
# canonical, pre-rendered, version-locked manifest the Flux project ships
# for every release. No third-party chart drift, no `helm install` step
# (so no failure mode where Flux installs while helm-controller crashloops
# searching for its own CRDs). Apply, then wait for the three controllers
# the bundle pipeline actually uses to roll out.
#
# OCIRepository.spec.insecure handles plain-HTTP for the in-cluster
# registry, so there's no Flux equivalent of apply_repo_secret().
# -------------------------------------------------------------------
install_flux() {
    local version="$1"

    if ! command -v flux &>/dev/null; then
        log_error "flux CLI not found in PATH. Install it via: make tools-setup"
        exit 60
    fi

    log_info "Installing Flux ${version} with source-watcher via flux CLI..."
    # GitHub releases CDN occasionally returns transient 502s (same flake
    # class that motivated the kwok-stage-fast retry hardening in
    # run-all-recipes.sh). 5 attempts with doubling backoff (5+10+20+40s
    # cumulative sleep = 75s) covers the 30-60s typical CDN hiccup.
    local max_attempts=5
    local delay=5
    local attempt=1
    # Build the flux install command; include --context when KUBECTL_CONTEXT
    # is set so Flux targets the same kube context used by kc()/hc().
    local -a flux_cmd=(flux install --version="${version}" --components-extra=source-watcher)
    if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
        flux_cmd+=(--context "${KUBECTL_CONTEXT}")
    fi
    while true; do
        if "${flux_cmd[@]}"; then
            break
        fi
        if (( attempt >= max_attempts )); then
            log_error "flux install failed after ${attempt} attempt(s)"
            dump_flux_diagnostics
            exit 60
        fi
        log_warn "flux install failed (attempt ${attempt}/${max_attempts}); retrying in ${delay}s..."
        sleep "${delay}"
        attempt=$(( attempt + 1 ))
        delay=$(( delay * 2 ))
    done

    log_info "Waiting for Flux controllers to roll out (timeout 180s each)..."
    local ctrl
    for ctrl in "${FLUX_CONTROLLERS[@]}"; do
        if ! kc rollout status deployment/"${ctrl}" \
                -n "${FLUX_NAMESPACE}" --timeout=180s; then
            log_error "Flux controller ${ctrl} did not become Ready within 180s"
            dump_flux_diagnostics
            exit 61
        fi
    done

    # Enable ExternalArtifact feature gate on helm-controller so
    # HelmRelease.spec.chartRef can reference ExternalArtifact resources
    # produced by ArtifactGenerator CRs (issue #964).
    # Idempotent: skip the patch (and rollout wait) when the flag is already present.
    local hc_args
    hc_args=$(kc get deployment helm-controller -n "${FLUX_NAMESPACE}" \
        -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)
    if [[ "$hc_args" == *"--feature-gates=ExternalArtifact=true"* ]]; then
        log_info "ExternalArtifact feature gate already present on helm-controller — skipping patch"
    else
        log_info "Enabling ExternalArtifact feature gate on helm-controller..."
        if ! kc patch deployment helm-controller -n "${FLUX_NAMESPACE}" \
                --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--feature-gates=ExternalArtifact=true"}]'; then
            log_error "Failed to patch helm-controller with ExternalArtifact feature gate"
            dump_flux_diagnostics
            exit 61
        fi
        if ! kc rollout status deployment/helm-controller \
                -n "${FLUX_NAMESPACE}" --timeout=60s; then
            log_error "helm-controller did not become Ready after feature gate patch"
            dump_flux_diagnostics
            exit 61
        fi
    fi

    log_info "Waiting for Flux CRDs to be Established..."
    local crd
    for crd in "${FLUX_REQUIRED_CRDS[@]}"; do
        if ! kc wait --for=condition=Established \
                "crd/${crd}" --timeout=60s; then
            log_error "Flux CRD ${crd} did not reach Established within 60s"
            kc describe crd "${crd}" 2>&1 || true
            exit 62
        fi
    done

    log_info "Flux ready (source-watcher + ExternalArtifact enabled)"
}

# -------------------------------------------------------------------
# Step 5 (flux-git only): Install Gitea as the in-cluster Git server.
#
# Mirrors install_registry(): plain Deployment + NodePort Service in the
# registry namespace, image pinned via .settings.yaml. Configuration is
# entirely env-driven (GITEA__section__KEY), no app.ini templating:
#   - INSTALL_LOCK=true skips the interactive setup wizard.
#   - sqlite3 on an emptyDir — Gitea state is intentionally ephemeral;
#     validate-scheduling.sh force-pushes each bundle, so nothing needs
#     to survive a cluster rebuild.
#   - ENABLE_PUSH_CREATE_USER=true means the first `git push` creates the
#     repo — no per-recipe repo-creation API calls.
#   - DEFAULT_PUSH_CREATE_PRIVATE=false makes pushed repos PUBLIC so Flux
#     source-controller clones anonymously. The flux deployer's
#     GitRepository template has no secretRef, so anonymous read access
#     is a hard requirement of the lane, not a convenience.
# -------------------------------------------------------------------
install_gitea() {
    local image="$1"

    log_info "Installing in-cluster Gitea (${image}) in namespace ${REGISTRY_NAMESPACE}..."

    # Namespace may not exist yet if install order ever changes; apply is
    # idempotent either way.
    kc apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${REGISTRY_NAMESPACE}
  labels:
    app.kubernetes.io/name: aicr-registry
    app.kubernetes.io/managed-by: install-infra.sh
EOF

    kc apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${GITEA_NAME}
  namespace: ${REGISTRY_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${GITEA_NAME}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ${GITEA_NAME}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ${GITEA_NAME}
    spec:
      containers:
        - name: ${GITEA_NAME}
          image: ${image}
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: ${GITEA_PORT}
              protocol: TCP
          env:
            - name: GITEA__security__INSTALL_LOCK
              value: "true"
            - name: GITEA__database__DB_TYPE
              value: "sqlite3"
            - name: GITEA__server__HTTP_PORT
              value: "${GITEA_PORT}"
            - name: GITEA__server__ROOT_URL
              value: "http://${GITEA_NAME}.${REGISTRY_NAMESPACE}.svc.cluster.local:${GITEA_PORT}/"
            - name: GITEA__repository__ENABLE_PUSH_CREATE_USER
              value: "true"
            - name: GITEA__repository__DEFAULT_PUSH_CREATE_PRIVATE
              value: "false"
            - name: GITEA__service__DISABLE_REGISTRATION
              value: "true"
          volumeMounts:
            - name: data
              mountPath: /var/lib/gitea
            - name: config
              mountPath: /etc/gitea
          readinessProbe:
            httpGet:
              path: /api/healthz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /api/healthz
              port: http
            initialDelaySeconds: 15
            periodSeconds: 10
      volumes:
        - name: data
          emptyDir: {}
        - name: config
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ${GITEA_NAME}
  namespace: ${REGISTRY_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${GITEA_NAME}
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: ${GITEA_NAME}
  ports:
    - name: http
      port: ${GITEA_PORT}
      targetPort: ${GITEA_PORT}
      nodePort: ${GITEA_NODEPORT}
      protocol: TCP
EOF

    log_info "Waiting for Gitea Deployment to become Ready (timeout 120s)..."
    if ! kc rollout status deployment/"${GITEA_NAME}" \
            -n "${REGISTRY_NAMESPACE}" --timeout=120s; then
        log_error "Gitea Deployment did not become Ready within 120s"
        dump_gitea_diagnostics
        exit 70
    fi

    # Probe the host port that Kind maps to NodePort 30300. Same caveat as
    # the registry probe: KWOK_GITEA_HOST_PORT only changes what this probe
    # waits on, not Kind's actual port mapping (kwok/kind-config.yaml). A
    # 60s hang here usually means the cluster predates the 3300 mapping and
    # must be recreated.
    log_info "Waiting for http://localhost:${GITEA_HOST_PORT}/api/healthz to respond (timeout 60s)..."
    local waited=0
    local interval=2
    local reachable=0
    while (( waited < 60 )); do
        if curl -sf -o /dev/null --max-time 3 \
                "http://localhost:${GITEA_HOST_PORT}/api/healthz"; then
            log_info "Gitea reachable at http://localhost:${GITEA_HOST_PORT}/"
            reachable=1
            break
        fi
        sleep "${interval}"
        waited=$(( waited + interval ))
    done
    if (( ! reachable )); then
        log_error "Gitea not reachable on host port ${GITEA_HOST_PORT} within 60s"
        log_error "If this cluster predates the 3300 port mapping, recreate it:"
        log_error "  kind delete cluster --name <kwok-cluster-name>"
        dump_gitea_diagnostics
        log_error "--- node port-mapping sanity ---"
        kc get nodes -o wide 2>&1 || true
        exit 71
    fi

    # Bootstrap the admin user the lane pushes as. `gitea admin user create`
    # is not idempotent, so treat "already exists" as success for re-runs.
    log_info "Ensuring Gitea admin user '${GITEA_USER}' exists..."
    local create_out
    if create_out=$(kc exec -n "${REGISTRY_NAMESPACE}" "deploy/${GITEA_NAME}" -- \
            gitea admin user create \
                --admin \
                --username "${GITEA_USER}" \
                --password "${GITEA_PASSWORD}" \
                --email "aicr@example.invalid" \
                --must-change-password=false 2>&1); then
        log_info "Gitea admin user '${GITEA_USER}' created"
    elif grep -qi "already exists" <<<"${create_out}"; then
        log_info "Gitea admin user '${GITEA_USER}' already exists — skipping"
    else
        log_error "Gitea admin user bootstrap failed:"
        log_error "${create_out}"
        dump_gitea_diagnostics
        exit 72
    fi

    log_info "Gitea ready (push-to-create enabled, pushed repos are public)"
}

# -------------------------------------------------------------------
# Main
# -------------------------------------------------------------------
main() {
    log_info "=========================================="
    log_info "AICR KWOK lane infra install (issue #843)"
    log_info "=========================================="

    if [[ ! -f "${SETTINGS_FILE}" ]]; then
        log_error "Settings file not found: ${SETTINGS_FILE}"
        exit 10
    fi

    local registry_image
    registry_image=$(read_setting '.testing_tools.registry_image')

    log_info "registry_image:        ${registry_image}"
    log_info "DEPLOYER:              ${DEPLOYER}"
    log_info "registry host port:    ${REGISTRY_HOST_PORT}"
    if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
        log_info "kubectl context:       ${KUBECTL_CONTEXT}"
    else
        local current_ctx
        current_ctx=$(kubectl config current-context 2>/dev/null || echo "<none>")
        log_info "kubectl context:       ${current_ctx} (current)"
    fi

    log_debug "Step 1: install in-cluster registry"
    install_registry "${registry_image}"

    # Controller install branches on DEPLOYER. Unknown / "helm" values stop
    # after the registry — they don't need a GitOps controller. This avoids
    # the ~30-90s of unconditional Argo CD install the prior version paid
    # on the helm lane.
    case "${DEPLOYER}" in
        argocd-oci|argocd-helm-oci)
            local argocd_chart
            argocd_chart=$(read_setting '.testing_tools.argocd_chart')
            log_info "argocd_chart:          ${argocd_chart}"
            log_debug "Step 2 (argocd): install Argo CD"
            install_argocd "${argocd_chart}"
            log_debug "Step 3 (argocd): apply repository secret"
            apply_repo_secret
            ;;
        argocd-git)
            # Git-source round-trip (issue #963): same Argo CD install as
            # the OCI lanes, PLUS Gitea so the runner can `git push` the
            # filesystem bundle and Argo CD's repo-server clones it back.
            # The OCI repo-creds secret is harmless here (the Git source
            # carries no credentials and Argo CD clones the public repo
            # anonymously) — applied unconditionally to keep this branch a
            # superset of the OCI lane.
            local argocd_chart gitea_image
            argocd_chart=$(read_setting '.testing_tools.argocd_chart')
            gitea_image=$(read_setting '.testing_tools.gitea_image')
            log_info "argocd_chart:          ${argocd_chart}"
            log_info "gitea_image:           ${gitea_image}"
            log_info "gitea host port:       ${GITEA_HOST_PORT}"
            log_debug "Step 2 (argocd): install Argo CD"
            install_argocd "${argocd_chart}"
            log_debug "Step 3 (argocd): apply repository secret"
            apply_repo_secret
            log_debug "Step 4 (argocd-git): install Gitea"
            install_gitea "${gitea_image}"
            ;;
        flux-oci)
            local flux_version
            flux_version=$(read_setting '.testing_tools.flux_version')
            log_info "flux_version:          ${flux_version}"
            log_debug "Step 2 (flux): install Flux 2"
            install_flux "${flux_version}"
            ;;
        flux-git)
            local flux_version gitea_image
            flux_version=$(read_setting '.testing_tools.flux_version')
            gitea_image=$(read_setting '.testing_tools.gitea_image')
            log_info "flux_version:          ${flux_version}"
            log_info "gitea_image:           ${gitea_image}"
            log_info "gitea host port:       ${GITEA_HOST_PORT}"
            log_debug "Step 2 (flux): install Flux 2"
            install_flux "${flux_version}"
            log_debug "Step 3 (flux-git): install Gitea"
            install_gitea "${gitea_image}"
            ;;
        helm|"")
            log_info "DEPLOYER=${DEPLOYER:-helm}: skipping GitOps controller install"
            ;;
        *)
            log_warn "Unknown DEPLOYER=${DEPLOYER}; installing registry only"
            ;;
    esac

    log_info "=========================================="
    log_info "Infra install complete"
    log_info "=========================================="
}

main "$@"

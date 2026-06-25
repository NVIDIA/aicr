#!/bin/bash
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

# =============================================================================
# Private Sigstore E2E local runner
# =============================================================================
#
# PURPOSE:
# Stands up a self-hosted Sigstore stack (Fulcio CA + Rekor transparency log +
# CT log + Trillian) from the sigstore HELM charts inside a Kind cluster, then
# runs the bundle-attestation-private-sigstore chainsaw suite, which exercises:
#
#   aicr bundle --attest --fulcio-url ... --rekor-url ... --identity-token ...
#
# Mirrors the sigstore-scaffolding-e2e.yaml CI workflow.
#
# WHY HELM (NOT sigstore/scaffolding's setup-kind.sh):
# The scaffolding shell scripts deploy Knative Services exposed via MetalLB +
# Kourier + sslip.io, which are NOT reachable from a macOS (Docker Desktop /
# Colima) host because Kind runs inside a VM. The sigstore `scaffold` Helm chart
# instead deploys plain Deployment + ClusterIP Services, reachable identically
# on macOS and Linux through `kubectl port-forward` to localhost. No MetalLB, no
# Knative, no sslip.io, no /etc/hosts, no Colima --network-address, no sudo
# (beyond mkcert's one-time CA trust install).
#
# HTTPS: `aicr bundle` requires absolute https:// signing endpoints, so a small
# stdlib TLS-termination reverse proxy (tlsproxy/) fronts the plain-HTTP
# port-forwards with an mkcert certificate the host trust store accepts.
#
# OIDC: Fulcio is configured to trust the in-cluster Kubernetes ServiceAccount
# issuer; the identity token is minted with `kubectl create token`. See
# scaffold-values.yaml and oidc-discovery-rbac.yaml for the supporting config.
#
# ATTESTED BINARY:
# `aicr bundle --attest` refuses to run unless the binary carries an NVIDIA-CI
# attestation (a co-located <binary>-attestation.sigstore.json, verified against
# PUBLIC Sigstore). Provide one via AICR_BIN (e.g. the build-attested.yaml
# artifact) together with a matching AICR_IDENTITY_REGEXP. A plain goreleaser
# --snapshot build is NOT attested and will stop at the bundle step.
#
# PREREQUISITES: kind, kubectl, helm, mkcert, chainsaw, cosign, go, yq, docker
#   (goreleaser is additionally required only when AICR_BIN is unset, to build aicr)
#   cosign is used by the chainsaw assemble-private-trust-root step to build the
#   private trusted_root.json exercised by `aicr verify --trust-root` (#1153).
#
# USAGE:
#   AICR_BIN=/path/to/aicr \
#   AICR_IDENTITY_REGEXP='https://github.com/NVIDIA/aicr/\.github/workflows/build-attested\.yaml@.*' \
#     ./tests/chainsaw/signing/bundle-attestation-private-sigstore/run.sh
#
# ENVIRONMENT:
#   AICR_BIN                Path to a pre-built (attested) aicr binary
#   AICR_IDENTITY_REGEXP    --certificate-identity-regexp for the binary attestation
#                           (default: the build-attested.yaml identity)
#   SCAFFOLD_CHART_VERSION  sigstore/scaffold chart version (default: .settings.yaml)
#   KIND_NODE_IMAGE         Kind node image (default: .settings.yaml)
#   SIGSTORE_E2E_CLUSTER    Kind cluster name (default: aicr-sigstore-e2e)
#   KEEP_CLUSTER            "true" to leave the cluster running afterwards
#   FULCIO_TLS_PORT / REKOR_TLS_PORT   host https ports (default 8443/8444)
#   FULCIO_PF_PORT  / REKOR_PF_PORT    host port-forward ports (default 8080/8081)
# =============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../../.." && pwd)"
# shellcheck source=/dev/null
. "${ROOT}/tools/common"

# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------
CLUSTER_NAME="${SIGSTORE_E2E_CLUSTER:-aicr-sigstore-e2e}"
KCTX="kind-${CLUSTER_NAME}"
HELM_REPO_URL="https://sigstore.github.io/helm-charts"
# Pinned chart/node versions come from .settings.yaml (the CI source of truth),
# with hard-coded fallbacks if yq or the key is unavailable. Never use :latest.
SCAFFOLD_CHART_VERSION="${SCAFFOLD_CHART_VERSION:-$(yq '.testing_tools.scaffold_chart // "0.6.109"' "${ROOT}/.settings.yaml" 2>/dev/null || echo '0.6.109')}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-$(yq '.testing.kind_node_image // "kindest/node:v1.36.1"' "${ROOT}/.settings.yaml" 2>/dev/null || echo 'kindest/node:v1.36.1')}"

FULCIO_PF_PORT="${FULCIO_PF_PORT:-8080}"
REKOR_PF_PORT="${REKOR_PF_PORT:-8081}"
FULCIO_TLS_PORT="${FULCIO_TLS_PORT:-8443}"
REKOR_TLS_PORT="${REKOR_TLS_PORT:-8444}"

KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
AICR_BIN="${AICR_BIN:-}"
# Default to the build-attested.yaml identity (the documented local path). When
# pointing AICR_BIN at an on-tag binary instead, override this to match.
AICR_IDENTITY_REGEXP="${AICR_IDENTITY_REGEXP:-https://github.com/NVIDIA/aicr/\.github/workflows/build-attested\.yaml@.*}"

CERT_DIR="${TMPDIR:-/tmp}/aicr-sigstore-e2e-tls"
# Output path for the localhost TLS proxy (tlsproxy/). aicr requires https://
# signing endpoints, so run.sh `go build`s the committed proxy source to this
# temp binary at runtime; the compiled artifact is not checked in.
PROXY_BIN="${TMPDIR:-/tmp}/aicr-sigstore-e2e-tlsproxy"
# CTLog (CTFE) public key extracted from the in-cluster secret. The chainsaw
# `assemble-private-trust-root` step needs this to build a trusted_root.json
# (cosign trusted-root create --ctfe requires it, and sigstore-go's bundle
# verification fails the SCT check without it). It lives ONLY in a k8s secret
# (ctlog-system/ctlog-public-key), so run.sh (which has authenticated kubectl)
# extracts it here and hands the path to chainsaw, the same way it hands over
# FULCIO_URL/REKOR_URL/OIDC_TOKEN. The Fulcio chain and Rekor key, by contrast,
# are served over HTTP and the chainsaw step fetches those itself.
CTLOG_PUBKEY="${TMPDIR:-/tmp}/aicr-sigstore-e2e-ctfe.pub"
CLUSTER_CREATED=false
PROXY_PID=""
PF_PIDS=()

# -----------------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------------
cleanup() {
  local rc=$?
  msg "Cleaning up"
  if [ -n "${PROXY_PID}" ]; then
    kill "${PROXY_PID}" 2>/dev/null || true
  fi
  local pid
  for pid in "${PF_PIDS[@]:-}"; do
    if [ -n "${pid}" ]; then
      kill "${pid}" 2>/dev/null || true
    fi
  done
  rm -rf "${CERT_DIR}" "${PROXY_BIN}" "${CTLOG_PUBKEY}"
  if [ "${CLUSTER_CREATED}" = "true" ]; then
    if [ "${KEEP_CLUSTER}" = "true" ]; then
      log_warning "KEEP_CLUSTER=true: leaving cluster '${CLUSTER_NAME}' running (kind delete cluster --name ${CLUSTER_NAME})"
    else
      msg "Deleting Kind cluster '${CLUSTER_NAME}'"
      kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    fi
  fi
  # The mkcert CA stays installed in the host trust store (idempotent, reused
  # across runs). Remove it with `mkcert -uninstall` if desired.
  if [ "${rc}" -eq 0 ]; then
    msg "Private Sigstore E2E: PASSED"
  else
    log_error "Private Sigstore E2E: FAILED (exit ${rc})"
  fi
  exit "${rc}"
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------

# wait_rollout NAMESPACE DEPLOYMENT — block until the deployment is Available.
wait_rollout() {
  local ns="$1" deploy="$2"
  kubectl --context "${KCTX}" -n "${ns}" rollout status "deploy/${deploy}" --timeout=300s
}

# wait_url URL — poll an endpoint until it answers (max ~90s). On timeout it
# surfaces the last curl error to aid CI debugging.
wait_url() {
  local url="$1" _ last
  # Bound each probe (--connect-timeout/--max-time) so one stuck request cannot
  # hang the harness past the poll window.
  for _ in $(seq 1 45); do
    if curl --connect-timeout 2 --max-time 4 -fsS -o /dev/null "${url}" 2>/dev/null; then
      return 0
    fi
    sleep 2
  done
  last="$(curl --connect-timeout 2 --max-time 4 -sS -o /dev/null "${url}" 2>&1 || true)"
  err "Endpoint did not become reachable after ~90s: ${url} (${last})"
}

# -----------------------------------------------------------------------------
# Steps
# -----------------------------------------------------------------------------

resolve_binary() {
  if [ -n "${AICR_BIN}" ] && [ -x "${AICR_BIN}" ]; then
    msg "Using pre-built binary: ${AICR_BIN}"
    # aicr's FindBinaryAttestation convention is a co-located
    # "<binary>-attestation.sigstore.json", so this is basename-agnostic (the CI
    # workflow's equivalent check hard-codes the "aicr" basename goreleaser emits).
    if [ -f "${AICR_BIN}-attestation.sigstore.json" ]; then
      msg "Binary attestation found; --attest steps will run"
    else
      log_warning "No attestation next to ${AICR_BIN}; --attest steps will fail (provide a CI-attested binary)"
    fi
    export AICR_BIN
    return 0
  fi

  log_warning "AICR_BIN not set; building an unattested snapshot. 'aicr bundle --attest'"
  log_warning "requires an NVIDIA-CI-attested binary, so the bundle step will fail."
  log_warning "Provide AICR_BIN (e.g. the build-attested.yaml artifact) + AICR_IDENTITY_REGEXP."
  cd "${ROOT}"
  GOFLAGS=-mod=vendor goreleaser build --clean --single-target --snapshot --timeout 10m
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "${arch}" in
    x86_64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
  esac
  local pattern
  for pattern in \
    "${ROOT}/dist/aicr_${os}_${arch}/aicr" \
    "${ROOT}/dist/aicr_${os}_${arch}_v1/aicr" \
    "${ROOT}/dist/aicr_${os}_${arch}_v8.0/aicr"; do
    if [ -x "${pattern}" ]; then
      AICR_BIN="${pattern}"
      break
    fi
  done
  if [ -z "${AICR_BIN}" ] || [ ! -x "${AICR_BIN}" ]; then
    err "Built binary not found in dist/"
  fi
  export AICR_BIN
  msg "Using binary: ${AICR_BIN}"
}

create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
    msg "Reusing existing Kind cluster '${CLUSTER_NAME}'"
  else
    msg "Creating Kind cluster '${CLUSTER_NAME}' (${KIND_NODE_IMAGE})"
    kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --wait 120s
    CLUSTER_CREATED=true
  fi
}

deploy_stack() {
  msg "Deploying sigstore/scaffold ${SCAFFOLD_CHART_VERSION} (Fulcio + Rekor + CTLog + Trillian)"
  helm repo add sigstore "${HELM_REPO_URL}" >/dev/null 2>&1 || true
  helm repo update sigstore >/dev/null
  helm --kube-context "${KCTX}" upgrade --install scaffold sigstore/scaffold \
    --version "${SCAFFOLD_CHART_VERSION}" \
    -f "${DIR}/scaffold-values.yaml" \
    --timeout 12m

  # Grant anonymous access to the OIDC discovery endpoints Fulcio fetches, and
  # point Fulcio's TLS trust at the in-cluster CA so it can verify the apiserver
  # serving cert during OIDC discovery. Neither is expressible as a chart value.
  msg "Configuring Fulcio OIDC trust (anonymous discovery + apiserver CA)"
  kubectl --context "${KCTX}" apply -f "${DIR}/oidc-discovery-rbac.yaml"
  kubectl --context "${KCTX}" -n fulcio-system set env deploy/fulcio-server \
    SSL_CERT_FILE=/var/run/fulcio/ca.crt

  msg "Waiting for the Sigstore stack to become ready"
  wait_rollout trillian-system trillian-mysql
  wait_rollout trillian-system trillian-logserver
  wait_rollout trillian-system trillian-logsigner
  wait_rollout rekor-system rekor-server
  wait_rollout ctlog-system ctlog
  wait_rollout fulcio-system fulcio-server
}

# build_tls_proxy compiles the committed tlsproxy/ source to a temp binary. aicr
# requires https:// signing endpoints, but the local Fulcio/Rekor are reached
# over plain HTTP through port-forward; this proxy terminates TLS (with the
# mkcert cert) and forwards to them. The source is checked in (tlsproxy/); only
# the compiled binary is built here, so the cleanup trap's `kill` reaches the
# real process for graceful shutdown.
build_tls_proxy() {
  msg "Building localhost TLS proxy"
  (cd "${ROOT}" && go build -o "${PROXY_BIN}" \
    ./tests/chainsaw/signing/bundle-attestation-private-sigstore/tlsproxy/)
}

setup_tls_and_forwards() {
  msg "Issuing localhost TLS cert (mkcert) and starting port-forwards + TLS proxy"
  # mkcert -install adds the mkcert CA to the host trust store (Keychain on
  # macOS, ca-certificates on Linux) so aicr's Go TLS client trusts the proxy
  # cert while keeping the public roots needed to verify the binary attestation.
  mkcert -install
  mkdir -p "${CERT_DIR}"
  mkcert -cert-file "${CERT_DIR}/localhost.pem" -key-file "${CERT_DIR}/localhost-key.pem" \
    localhost 127.0.0.1 ::1

  kubectl --context "${KCTX}" -n fulcio-system port-forward svc/fulcio-server \
    "${FULCIO_PF_PORT}:80" >/dev/null 2>&1 &
  PF_PIDS+=("$!")
  kubectl --context "${KCTX}" -n rekor-system port-forward svc/rekor-server \
    "${REKOR_PF_PORT}:80" >/dev/null 2>&1 &
  PF_PIDS+=("$!")
  wait_url "http://localhost:${FULCIO_PF_PORT}/api/v1/rootCert"
  wait_url "http://localhost:${REKOR_PF_PORT}/api/v1/log/publicKey"

  "${PROXY_BIN}" \
    "${CERT_DIR}/localhost.pem" "${CERT_DIR}/localhost-key.pem" \
    "${FULCIO_TLS_PORT}=http://localhost:${FULCIO_PF_PORT}" \
    "${REKOR_TLS_PORT}=http://localhost:${REKOR_PF_PORT}" &
  PROXY_PID="$!"
  # 127.0.0.1 (not localhost): the proxy binds loopback IPv4 only, so this
  # avoids an IPv6 "localhost" first-attempt that would just fall back.
  wait_url "https://127.0.0.1:${FULCIO_TLS_PORT}/api/v1/rootCert"
  wait_url "https://127.0.0.1:${REKOR_TLS_PORT}/api/v1/log/publicKey"
}

# extract_ctlog_pubkey pulls the CTFE (CTLog) public key from the in-cluster
# secret and writes it as PEM to CTLOG_PUBKEY. The scaffold chart's
# ctlog-createcerts job generates this key into secret ctlog-public-key
# (namespace ctlog-system, data key "public"; see scaffold-values.yaml
# secrets.ctlog). The chainsaw assemble-private-trust-root step feeds it to
# `cosign trusted-root create --ctfe public-key=...`; without it, sigstore-go's
# SCT verification of the private Fulcio cert fails. This is the one piece of
# trust material not served over HTTP, so it is extracted here (where kubectl is
# authenticated against the kind context) rather than from the --no-cluster
# chainsaw step.
# base64_decode reads base64 from stdin and writes the decoded bytes to stdout,
# portable across GNU coreutils (base64 -d) and BSD/macOS (base64 -D). The
# documented local harness path runs on macOS, where -d may be unavailable; the
# probe selects the decode flag this platform's base64 actually accepts.
base64_decode() {
  if printf '' | base64 -d >/dev/null 2>&1; then
    base64 -d
  else
    base64 -D
  fi
}

extract_ctlog_pubkey() {
  msg "Extracting CTLog (CTFE) public key from ctlog-system/ctlog-public-key"
  kubectl --context "${KCTX}" -n ctlog-system get secret ctlog-public-key \
    -o jsonpath='{.data.public}' | base64_decode >"${CTLOG_PUBKEY}"
  # Fail closed: a non-empty PEM block is required before chainsaw runs.
  [ -s "${CTLOG_PUBKEY}" ] || err "CTLog public key extraction produced an empty file"
  grep -q "BEGIN PUBLIC KEY" "${CTLOG_PUBKEY}" || err "CTLog public key is not a PEM public key"
}

run_chainsaw() {
  msg "Running private-sigstore chainsaw tests"
  local token
  token="$(kubectl --context "${KCTX}" create token default --audience sigstore --duration=20m)"
  [ -n "${token}" ] || err "Failed to mint OIDC token"

  AICR_BIN="${AICR_BIN}" \
  FULCIO_URL="https://127.0.0.1:${FULCIO_TLS_PORT}" \
  REKOR_URL="https://127.0.0.1:${REKOR_TLS_PORT}" \
  OIDC_TOKEN="${token}" \
  AICR_IDENTITY_REGEXP="${AICR_IDENTITY_REGEXP}" \
  AICR_CTLOG_PUBKEY="${CTLOG_PUBKEY}" \
  FULCIO_PF_PORT="${FULCIO_PF_PORT}" \
  REKOR_PF_PORT="${REKOR_PF_PORT}" \
    chainsaw test \
      --no-cluster \
      --config "${ROOT}/tests/chainsaw/chainsaw-config.yaml" \
      --test-dir "${DIR}" \
      --selector 'requires=private-sigstore'
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------
msg "Starting Private Sigstore E2E Integration Tests"
# goreleaser is only needed to build the binary when AICR_BIN is unset; require
# it up front in that case so the run fails with a clear prereq error rather
# than a late "command not found" in resolve_binary.
# cosign is required by the chainsaw assemble-private-trust-root step, which
# runs `cosign trusted-root create` to build the private trusted_root.json from
# the Fulcio chain, Rekor key, and CTLog key (the #1153 trust-root proof).
if [ -z "${AICR_BIN}" ]; then
  has_tools kind kubectl helm mkcert chainsaw cosign go yq docker goreleaser
else
  has_tools kind kubectl helm mkcert chainsaw cosign go yq docker
fi

resolve_binary
build_tls_proxy
create_cluster
deploy_stack
setup_tls_and_forwards
extract_ctlog_pubkey
run_chainsaw

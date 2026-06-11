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
# KMS MiniStack E2E local runner
# =============================================================================
#
# PURPOSE:
# Starts a MiniStack container, provisions an ECDSA P-256 KMS key, builds the
# aicr binary, and runs the bundle-attestation-kms-ministack chainsaw suite
# against MiniStack. Mirrors the kms-ministack-e2e.yaml CI workflow.
#
# MiniStack (https://ministack.org) is an open-source, token-free AWS emulator.
# Its KMS supports the asymmetric ECDSA P-256 SIGN_VERIFY keys that cosign's
# awskms:// signer needs. We use it instead of LocalStack because recent
# LocalStack images require a paid license token to start.
#
# NOTE: The chainsaw "--attest" steps only pass with a binary that carries an
# NVIDIA-CI attestation; a local goreleaser --snapshot build does not. So with
# no attested binary this runner enters SMOKE MODE: it validates the MiniStack
# and KMS plumbing (container, key provisioning, PEM export, recipe + bundle)
# and exits 0 with a clear message, instead of running the full suite and
# failing. For the full sign/verify chain, run the kms-ministack-e2e.yaml
# workflow (which attests the binary), or point AICR_BIN at a CI-attested binary
# (plus a matching AICR_IDENTITY_REGEXP).
#
# PREREQUISITES:
# - docker
# - aws CLI (pip install awscli): talks to MiniStack via --endpoint-url
# - chainsaw (brew install kyverno/tap/chainsaw)
# - make, goreleaser (for building the binary)
# - openssl (for PEM export)
#
# USAGE:
#   ./tests/chainsaw/signing/bundle-attestation-kms-ministack/run.sh
#
# The script cleans up the MiniStack container on exit (success or failure).
# Pass DEBUG=true for verbose output.
#
# =============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../../.." && pwd)"
. "${ROOT}/tools/common"

# =============================================================================
# Prerequisites
# =============================================================================

has_tools docker aws chainsaw make goreleaser openssl mkcert

# =============================================================================
# Configuration
# =============================================================================

MINISTACK_CONTAINER_NAME="aicr-kms-e2e-ministack"
MINISTACK_PORT="${MINISTACK_PORT:-4566}"
# Image from .settings.yaml (testing_tools.ministack_image; the CI source of
# truth), with a pinned fallback if yq or the key is unavailable. Never :latest.
MINISTACK_IMAGE="${MINISTACK_IMAGE:-$(yq '.testing_tools.ministack_image // "ministackorg/ministack:1.3.61"' "${ROOT}/.settings.yaml" 2>/dev/null || echo 'ministackorg/ministack:1.3.61')}"
# HTTPS: sigstore's awskms client hardcodes https://, so MiniStack must serve TLS
# (USE_SSL=1) with a cert the Go AWS SDK trusts. See setup_tls().
MINISTACK_ENDPOINT="https://localhost:${MINISTACK_PORT}"
# Exported so the chainsaw suite (and any port override) reaches the same endpoint.
export MINISTACK_ENDPOINT

# mkcert-issued localhost cert for MiniStack TLS. The dir MUST live under $HOME so
# Colima shares it into its VM; a /tmp path is not visible inside the container.
MINISTACK_CERT_DIR="${MINISTACK_CERT_DIR:-${HOME}/.aicr-ministack-e2e-tls}"

# MiniStack accepts any credentials; these match the CI workflow.
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"

WORK_DIR="${TMPDIR:-/tmp}/kms-ministack-e2e-$$"

# Optional pre-built binary. Provide a CI-attested binary (e.g. the artifact from
# the build-attested.yaml workflow) plus a matching AICR_IDENTITY_REGEXP to run
# the full --attest chain locally; otherwise run.sh builds an unattested snapshot
# and the chainsaw --attest steps will stop at the first bundle command.
AICR_BIN="${AICR_BIN:-}"

# =============================================================================
# Cleanup
# =============================================================================

cleanup() {
  local rc=$?
  msg "Cleaning up"
  docker rm -f "${MINISTACK_CONTAINER_NAME}" &>/dev/null || true
  # WORK_DIR is an internal, per-PID path under TMPDIR, so a full rm is safe.
  rm -rf "${WORK_DIR}"
  # MINISTACK_CERT_DIR is user-overridable, so remove only the cert files this
  # script generated (never the whole directory with rm -rf) and drop the dir
  # only if our removal left it empty. A bad override must not delete unrelated
  # local data.
  rm -f "${MINISTACK_CERT_DIR}/cert.pem" "${MINISTACK_CERT_DIR}/key.pem"
  rmdir "${MINISTACK_CERT_DIR}" 2>/dev/null || true
  # Note: the mkcert CA stays installed in the system trust store (idempotent,
  # reusable across runs). Remove it with `mkcert -uninstall` if desired.
  if [ $rc -eq 0 ]; then
    msg "KMS MiniStack E2E: PASSED"
  else
    log_error "KMS MiniStack E2E: FAILED (exit ${rc})"
  fi
  exit $rc
}
trap cleanup EXIT

# =============================================================================
# TLS (mkcert)
# =============================================================================

setup_tls() {
  msg "Setting up TLS for MiniStack (mkcert)"

  # mkcert issues a localhost leaf cert and installs its CA into the system trust
  # store so the Go AWS SDK (Keychain on macOS / ca-certificates on Linux) trusts
  # MiniStack's TLS. `-install` is idempotent and may prompt for sudo the first time.
  mkcert -install

  mkdir -p "${MINISTACK_CERT_DIR}"
  mkcert -cert-file "${MINISTACK_CERT_DIR}/cert.pem" \
    -key-file "${MINISTACK_CERT_DIR}/key.pem" \
    localhost 127.0.0.1 ::1
  chmod 644 "${MINISTACK_CERT_DIR}/cert.pem" "${MINISTACK_CERT_DIR}/key.pem"

  # The Python aws CLI (botocore) ignores the system trust store and uses its own
  # bundle, so point it at the mkcert root CA for the provisioning/export calls.
  AWS_CA_BUNDLE="$(mkcert -CAROOT)/rootCA.pem"
  export AWS_CA_BUNDLE
  msg "TLS ready (cert: ${MINISTACK_CERT_DIR}, AWS_CA_BUNDLE: ${AWS_CA_BUNDLE})"
}

# =============================================================================
# MiniStack
# =============================================================================

start_ministack() {
  msg "Starting MiniStack (${MINISTACK_IMAGE})"

  # Remove any leftover container from a prior interrupted run
  docker rm -f "${MINISTACK_CONTAINER_NAME}" &>/dev/null || true

  # USE_SSL=1 makes MiniStack serve TLS (instead of plaintext HTTP) on the same
  # port, using the mounted mkcert cert. The cert dir is under $HOME (Colima-shared).
  docker run -d \
    --name "${MINISTACK_CONTAINER_NAME}" \
    -p "${MINISTACK_PORT}:4566" \
    -e USE_SSL=1 \
    -e MINISTACK_SSL_CERT=/certs/cert.pem \
    -e MINISTACK_SSL_KEY=/certs/key.pem \
    -v "${MINISTACK_CERT_DIR}:/certs:ro" \
    "${MINISTACK_IMAGE}" >/dev/null

  msg "Waiting for MiniStack KMS to become available"
  local retries=45
  until [ $retries -eq 0 ]; do
    # Fail the container check fast if it died (e.g. bad image tag) instead of
    # polling a dead endpoint for the full retry budget.
    if [ "$(docker inspect -f '{{.State.Running}}' "${MINISTACK_CONTAINER_NAME}" 2>/dev/null)" != "true" ]; then
      docker logs "${MINISTACK_CONTAINER_NAME}" 2>&1 | tail -15
      err "MiniStack container exited unexpectedly"
    fi

    # Probe KMS itself: the only readiness signal that matters here. No `timeout`
    # binary (absent on macOS); AWS_MAX_ATTEMPTS=1 + short CLI timeouts keep each
    # attempt fast when the endpoint is not yet listening.
    if AWS_MAX_ATTEMPTS=1 aws kms list-keys \
        --endpoint-url "${MINISTACK_ENDPOINT}" \
        --region "${AWS_DEFAULT_REGION}" \
        --cli-connect-timeout 3 \
        --cli-read-timeout 5 \
        --output text >/dev/null 2>&1; then
      msg "MiniStack KMS is ready"
      return 0
    fi
    retries=$((retries - 1))
    sleep 2
  done
  err "MiniStack did not become ready in time (check: docker logs ${MINISTACK_CONTAINER_NAME})"
}

# =============================================================================
# KMS key provisioning
# =============================================================================

provision_kms_key() {
  msg "Provisioning ECDSA P-256 signing key in MiniStack"

  KMS_KEY_ARN=$(aws kms create-key \
    --endpoint-url "${MINISTACK_ENDPOINT}" \
    --key-spec ECC_NIST_P256 \
    --key-usage SIGN_VERIFY \
    --description "aicr-ministack-e2e-key" \
    --region "${AWS_DEFAULT_REGION}" \
    --query 'KeyMetadata.Arn' \
    --output text)

  msg "Provisioned KMS key ARN: ${KMS_KEY_ARN}"

  # Derive awskms:// URI with embedded MiniStack host:port. The sigstore AWS KMS
  # client interprets the authority as a custom endpoint.
  local host
  host="${MINISTACK_ENDPOINT#http://}"
  host="${host#https://}"
  host="${host%/}"
  KMS_URI="awskms://${host}/${KMS_KEY_ARN}"
  msg "KMS URI: ${KMS_URI}"

  # Export public key for PEM-based verification step
  mkdir -p "${WORK_DIR}"
  aws kms get-public-key \
    --endpoint-url "${MINISTACK_ENDPOINT}" \
    --key-id "${KMS_KEY_ARN}" \
    --region "${AWS_DEFAULT_REGION}" \
    --query 'PublicKey' \
    --output text | base64 -d | \
    openssl pkey -pubin -inform DER -outform PEM \
    -out "${WORK_DIR}/signing-key.pem"

  msg "Public key exported to ${WORK_DIR}/signing-key.pem"

  export KMS_KEY_ARN KMS_URI
  export KMS_PEM_FILE="${WORK_DIR}/signing-key.pem"
}

# =============================================================================
# Binary build
# =============================================================================

build_binary() {
  # Honor a pre-built (ideally CI-attested) binary so the full --attest chain
  # can run locally. Pair with AICR_IDENTITY_REGEXP matching its attestation.
  if [ -n "${AICR_BIN}" ] && [ -x "${AICR_BIN}" ]; then
    msg "Using pre-built binary: ${AICR_BIN}"
    export AICR_BIN
    # Detect the binary attestation (FindBinaryAttestation convention:
    # <binary>-attestation.sigstore.json) so the --attest / verified-level steps
    # run only when the binary actually carries one (e.g. a build-attested artifact).
    if [ -f "${AICR_BIN}-attestation.sigstore.json" ]; then
      export AICR_ATTESTED=true
      msg "Binary attestation found; --attest steps will run"
    else
      export AICR_ATTESTED=false
      warn "No attestation next to ${AICR_BIN}; --attest steps will fail (provide a CI-attested binary)"
    fi
    return 0
  fi

  msg "Building aicr binary (unattested snapshot; --attest steps will not run)"
  cd "${ROOT}"

  if ! GOFLAGS=-mod=vendor goreleaser build --clean --single-target --snapshot --timeout 10m 2>&1; then
    err "Failed to build aicr binary"
  fi

  local os_name arch_name
  os_name=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch_name=$(uname -m)
  case "${arch_name}" in
    x86_64)          arch_name="amd64" ;;
    aarch64 | arm64) arch_name="arm64" ;;
  esac

  AICR_BIN=""
  for pattern in \
    "${ROOT}/dist/aicr_${os_name}_${arch_name}/aicr" \
    "${ROOT}/dist/aicr_${os_name}_${arch_name}_v1/aicr" \
    "${ROOT}/dist/aicr_${os_name}_${arch_name}_v8.0/aicr"; do
    if [ -x "${pattern}" ]; then
      AICR_BIN="${pattern}"
      break
    fi
  done

  if [ -z "${AICR_BIN}" ] || [ ! -x "${AICR_BIN}" ]; then
    err "Built binary not found in dist/"
  fi

  msg "Using binary: ${AICR_BIN}"
  export AICR_BIN
  # Snapshot builds are not attested; make that explicit so the --attest steps
  # are clearly skipped/failed rather than depending on an inherited env value.
  export AICR_ATTESTED=false
}

# =============================================================================
# Run chainsaw tests
# =============================================================================

run_chainsaw() {
  msg "Running KMS MiniStack chainsaw tests"
  cd "${ROOT}"
  chainsaw test \
    --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/signing/bundle-attestation-kms-ministack/ \
    --selector 'requires=ministack'
}

# smoke_test validates the MiniStack/KMS plumbing with non-attest commands and
# exits cleanly. Used when no CI-attested binary is available: the chainsaw
# suite's `bundle --attest` steps would otherwise fail (the bundler refuses to
# attest with an unattested binary), which reads as a real e2e failure rather
# than a missing prerequisite.
smoke_test() {
  msg "Smoke mode (no attested binary): validating MiniStack/KMS plumbing only"
  cd "${ROOT}"

  local smoke_work="${WORK_DIR}/smoke"
  mkdir -p "${smoke_work}"

  # 1. The binary runs and generates a recipe.
  "${AICR_BIN}" recipe --service eks --accelerator h100 --os ubuntu \
    --intent training -o "${smoke_work}/recipe.yaml"

  # 2. Bundling works end-to-end (without --attest, which needs an attested binary).
  "${AICR_BIN}" bundle -r "${smoke_work}/recipe.yaml" -o "${smoke_work}/bundle"

  # 3. The MiniStack KMS key was provisioned and its public key exported (PEM).
  test -s "${KMS_PEM_FILE}"
  msg "MiniStack KMS key: ${KMS_KEY_ARN}"
  msg "KMS URI:           ${KMS_URI}"

  msg "Smoke checks passed: MiniStack up, ECDSA P-256 key provisioned, PEM exported, recipe + bundle generated"
  msg "To run the full --attest sign/verify suite, set AICR_BIN to a CI-attested binary and AICR_IDENTITY_REGEXP (see README), or run the kms-ministack-e2e.yaml workflow."
}

# run_tests dispatches to the full chainsaw suite when an attested binary is
# present, or to the smoke checks otherwise.
run_tests() {
  if [ "${AICR_ATTESTED:-false}" != "true" ]; then
    smoke_test
    return
  fi
  run_chainsaw
}

# =============================================================================
# Main
# =============================================================================

msg "Starting KMS MiniStack E2E Integration Tests"

setup_tls
start_ministack
provision_kms_key
build_binary
run_tests

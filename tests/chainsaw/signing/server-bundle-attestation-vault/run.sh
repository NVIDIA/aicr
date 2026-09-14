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
# Server (aicrd) Vault/OpenBAO KMS bundle-attestation E2E local runner
# =============================================================================
#
# PURPOSE:
# Drives the aicrd HTTP SERVER (not the aicr CLI) end to end against a
# HashiCorp Vault-compatible Transit engine (OpenBAO, https://openbao.org).
# It starts OpenBAO in dev mode, enables the Transit secrets engine, provisions
# an ECDSA P-256 signing key `aicr`, starts aicrd in the background with the
# operator-configured KMS signing identity, and exercises the server-side
# bundle-attestation surface added for #1150:
#
#   POST /v1/bundle?attest=true   -> server signs the bundle as itself
#   aicr verify <bundle> --key hashivault://aicr --insecure-ignore-tlog
#
# The companion of tests/chainsaw/signing/bundle-attestation-vault/, which
# exercises the same hashivault:// path through the aicr CLI. This suite proves
# the server reproduces that behavior over HTTP with a non-interactive,
# operator-supplied signing identity (no human at a browser, no request-supplied
# identity material).
#
# Because the server is a long-running process, lifecycle management is the key
# difference from the CLI suite: the runner starts aicrd in the background, waits
# for /health, fails fast (dumping logs) if the process dies at startup, and the
# cleanup trap kills the server in addition to removing the OpenBAO container.
#
# STRUCTURE (why run.sh, not chainsaw-test.yaml): the server is a background
# process whose lifecycle (start -> poll /health -> curl -> verify -> kill) does
# not map cleanly onto chainsaw's per-step script model. A single well-structured
# run.sh keeps the process ownership, the fail-fast log dump, and the cleanup
# trap in one place, and is the primary artifact the CI workflow invokes. See the
# README for the rationale.
#
# MODES:
#   SMOKE MODE (no aicrd binary attestation present, e.g. a local snapshot):
#     The server-side --attest path requires the server to verify its OWN binary
#     attestation at startup, which a plain `goreleaser --snapshot` build does not
#     carry. So without an attested binary this runner validates the plumbing and
#     exits 0:
#       - OpenBAO up, Transit ECDSA P-256 key provisioned, PEM exported
#       - aicrd starts WITHOUT signing configured
#       - POST /v1/bundle?attest=true  -> 400 "not configured for attestation"
#       - POST /v1/bundle (unsigned)   -> 200, a zip archive
#
#   FULL MODE (attested aicrd binary present, e.g. built by server-kms-e2e.yaml):
#     Starts aicrd with AICR_SIGNING_KEY=hashivault://aicr, AICR_TLOG_UPLOAD=false,
#     and AICR_BINARY_ATTESTATION_FILE pointing at the sibling
#     aicrd-attestation.sigstore.json, then:
#       - POST /v1/bundle?attest=true  -> signed bundle zip
#       - aicr verify --key hashivault://aicr --insecure-ignore-tlog --format json
#         asserts bundleAttested:true and checksumsPassed:true
#       - asserts attestation/aicr-attestation.sigstore.json is in the bundle
#     For a full run, run the server-kms-e2e.yaml workflow (which attests the
#     aicrd binary with its own workflow identity), or point AICRD_BIN/AICR_BIN at
#     CI-attested binaries.
#
# PREREQUISITES:
# Always required (checked upfront):
# - docker  (runs the OpenBAO container)
# - curl    (talks to the Vault HTTP API and the aicrd server)
# - python3 (extracts the Transit PEM, parses verify JSON, unzips the bundle,
#            picks a free port)
# - yq      (reads the pinned OpenBAO image from .settings.yaml before startup)
# Checked lazily, only on the path that uses them:
# - goreleaser (builds unattested snapshot binaries when AICRD_BIN/AICR_BIN unset)
#
# USAGE:
#   ./tests/chainsaw/signing/server-bundle-attestation-vault/run.sh
#
# The script kills the aicrd server and removes the OpenBAO container on exit
# (success or failure). Pass DEBUG=true for verbose server logs. Override the
# image, port, token, or key with OPENBAO_IMAGE / OPENBAO_PORT / VAULT_TOKEN /
# VAULT_KMS_KEY; override the server port with PORT.
#
# =============================================================================

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DIR}/../../../.." && pwd)"
# shellcheck source=tools/common
. "${ROOT}/tools/common"

# =============================================================================
# Prerequisites
# =============================================================================

# Tools every path needs. goreleaser (build) is checked lazily in
# build_or_locate_binaries so the pre-built-binary and smoke paths do not
# require it.
has_tools docker curl python3 yq

# =============================================================================
# Configuration
# =============================================================================

OPENBAO_CONTAINER_NAME="aicr-server-kms-e2e-openbao"
OPENBAO_PORT="${OPENBAO_PORT:-8200}"
# Image from .settings.yaml (testing_tools.openbao_image) — the single source of
# truth, shared with CI via the load-versions action. Read it rather than
# duplicating a literal tag here (which could silently drift from the pin).
# Never :latest.
OPENBAO_IMAGE="${OPENBAO_IMAGE:-$(yq -r '.testing_tools.openbao_image' "${ROOT}/.settings.yaml" 2>/dev/null)}"
if [ -z "${OPENBAO_IMAGE}" ] || [ "${OPENBAO_IMAGE}" = "null" ]; then
  err "Could not read testing_tools.openbao_image from ${ROOT}/.settings.yaml"
fi

# Dev-mode root token. Dev mode auto-initializes and unseals, mounts Transit-able
# storage in memory, and serves plain HTTP — nothing here is a real secret.
VAULT_TOKEN="${VAULT_TOKEN:-root}"
VAULT_ADDR="http://127.0.0.1:${OPENBAO_PORT}"
# The sigstore hashivault provider (inside aicrd) reads VAULT_ADDR / VAULT_TOKEN
# (BAO_* also work). Exported so the provider in the server process reaches the
# same OpenBAO instance the runner provisioned.
export VAULT_ADDR VAULT_TOKEN
# Transit key name; the KMS URI is hashivault://<key>.
VAULT_KMS_KEY="${VAULT_KMS_KEY:-aicr}"
export VAULT_KMS_KEY

# A stray VAULT_NAMESPACE (e.g. inherited from an operator's shell or CI secret)
# makes the sigstore hashivault client target an Enterprise namespace path that
# dev-mode OpenBAO does not serve, breaking every Transit call with a confusing
# 404. Dev-mode OpenBAO has no namespaces, so clear it unconditionally.
unset VAULT_NAMESPACE || true

WORK_DIR="${TMPDIR:-/tmp}/server-kms-e2e-$$"
SERVER_LOG="${WORK_DIR}/aicrd.log"

# Optional pre-built binaries. Provide CI-attested binaries (e.g. from the
# server-kms-e2e.yaml workflow) to run the full sign/verify chain locally;
# otherwise the runner builds unattested snapshots and enters smoke mode.
# aicrd is the server under test; aicr is used for recipe generation and verify.
AICRD_BIN="${AICRD_BIN:-}"
AICR_BIN="${AICR_BIN:-}"

# Server listen port. Explicit PORT wins; otherwise pick a free ephemeral port so
# concurrent runs (and a stray local aicrd on 8080) do not collide.
SERVER_PORT="${PORT:-}"

# Populated by later stages.
AICR_ATTESTED="false"
BINARY_ATTESTATION_FILE=""
SERVER_PID=""

# =============================================================================
# Cleanup
# =============================================================================

stop_server() {
  if [ -n "${SERVER_PID}" ] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  SERVER_PID=""
}

cleanup() {
  local rc=$?
  msg "Cleaning up"
  stop_server
  docker rm -f "${OPENBAO_CONTAINER_NAME}" &>/dev/null || true
  # WORK_DIR is an internal, per-PID path under TMPDIR, so a full rm is safe.
  rm -rf "${WORK_DIR}"
  if [ $rc -eq 0 ]; then
    msg "Server KMS E2E: PASSED"
  else
    log_error "Server KMS E2E: FAILED (exit ${rc})"
  fi
  exit $rc
}
trap cleanup EXIT

# =============================================================================
# OpenBAO
# =============================================================================

start_openbao() {
  msg "Starting OpenBAO (${OPENBAO_IMAGE})"

  # Remove any leftover container from a prior interrupted run
  docker rm -f "${OPENBAO_CONTAINER_NAME}" &>/dev/null || true

  # Dev mode: single in-memory node, auto-unsealed, root token fixed to
  # ${VAULT_TOKEN}, HTTP listener on 0.0.0.0:8200. IPC_LOCK lets the server mlock
  # memory (Vault/OpenBAO warn without it; harmless to grant).
  docker run -d \
    --name "${OPENBAO_CONTAINER_NAME}" \
    --cap-add IPC_LOCK \
    -p "${OPENBAO_PORT}:8200" \
    "${OPENBAO_IMAGE}" \
    server -dev \
    -dev-root-token-id="${VAULT_TOKEN}" \
    -dev-listen-address=0.0.0.0:8200 >/dev/null

  msg "Waiting for OpenBAO to become available"
  local retries=45
  until [ $retries -eq 0 ]; do
    # Fail fast if the container died (e.g. bad image tag) instead of polling a
    # dead endpoint for the full retry budget.
    if [ "$(docker inspect -f '{{.State.Running}}' "${OPENBAO_CONTAINER_NAME}" 2>/dev/null)" != "true" ]; then
      docker logs "${OPENBAO_CONTAINER_NAME}" 2>&1 | tail -15
      err "OpenBAO container exited unexpectedly"
    fi

    # sys/health returns 200 only when initialized, unsealed, and active.
    if curl -sf --max-time 3 "${VAULT_ADDR}/v1/sys/health" >/dev/null 2>&1; then
      msg "OpenBAO is ready"
      return 0
    fi
    retries=$((retries - 1))
    sleep 2
  done
  err "OpenBAO did not become ready in time (check: docker logs ${OPENBAO_CONTAINER_NAME})"
}

# =============================================================================
# Transit provisioning
# =============================================================================

provision_transit_key() {
  msg "Enabling Transit secrets engine and provisioning ECDSA P-256 key '${VAULT_KMS_KEY}'"

  # Enable transit at the default mount path. Ignore "path is already in use"
  # (idempotent across reruns against a persistent server).
  curl -sf -H "X-Vault-Token: ${VAULT_TOKEN}" \
    -X POST -d '{"type":"transit"}' \
    "${VAULT_ADDR}/v1/sys/mounts/transit" >/dev/null 2>&1 || true

  # Create the signing key. Transit treats create-of-existing as a no-op.
  curl -sf -H "X-Vault-Token: ${VAULT_TOKEN}" \
    -X POST -d '{"type":"ecdsa-p256"}' \
    "${VAULT_ADDR}/v1/transit/keys/${VAULT_KMS_KEY}" >/dev/null

  KMS_URI="hashivault://${VAULT_KMS_KEY}"
  msg "KMS URI: ${KMS_URI}"

  # Export the public key (PEM) for the smoke-mode plumbing assertion. Transit
  # nests it under data.keys.<version>.public_key; pick the latest version.
  mkdir -p "${WORK_DIR}"
  curl -sf -H "X-Vault-Token: ${VAULT_TOKEN}" \
    "${VAULT_ADDR}/v1/transit/keys/${VAULT_KMS_KEY}" | \
    python3 -c "
import json, sys
keys = json.load(sys.stdin)['data']['keys']
latest = max(keys, key=lambda k: int(k))
sys.stdout.write(keys[latest]['public_key'])
" > "${WORK_DIR}/signing-key.pem"

  test -s "${WORK_DIR}/signing-key.pem"
  msg "Public key exported to ${WORK_DIR}/signing-key.pem"

  export KMS_URI
}

# =============================================================================
# Binary build / location
# =============================================================================

# find_dist_binary <name> <os> <arch> echoes the first matching goreleaser dist
# path for <name> (aicr | aicrd), covering the amd64 microarch suffixes.
find_dist_binary() {
  local name="$1" os="$2" arch="$3" pattern
  for pattern in \
    "${ROOT}/dist/${name}_${os}_${arch}/${name}" \
    "${ROOT}/dist/${name}_${os}_${arch}_v1/${name}" \
    "${ROOT}/dist/${name}_${os}_${arch}_v8.0/${name}"; do
    if [ -x "${pattern}" ]; then
      echo "${pattern}"
      return 0
    fi
  done
  return 1
}

build_or_locate_binaries() {
  local os_name arch_name
  os_name=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch_name=$(uname -m)
  case "${arch_name}" in
    x86_64)          arch_name="amd64" ;;
    aarch64 | arm64) arch_name="arm64" ;;
  esac

  # Validate explicitly-provided overrides up front (fail fast on a typo'd path
  # rather than silently falling through to a snapshot build).
  if [ -n "${AICRD_BIN}" ] && [ ! -x "${AICRD_BIN}" ]; then
    err "AICRD_BIN is set to '${AICRD_BIN}' but is not an executable file"
  fi
  if [ -n "${AICR_BIN}" ] && [ ! -x "${AICR_BIN}" ]; then
    err "AICR_BIN is set to '${AICR_BIN}' but is not an executable file"
  fi

  # Build unattested snapshots only when a binary is missing. aicrd is a
  # linux-only build target, so `--single-target` produces it only on a linux
  # host; elsewhere provide AICRD_BIN (e.g. `go build -o /tmp/aicrd ./cmd/aicrd`).
  if [ -z "${AICRD_BIN}" ] || [ -z "${AICR_BIN}" ]; then
    has_tools goreleaser
    msg "Building aicr + aicrd (unattested snapshot; smoke mode)"
    cd "${ROOT}"
    if ! goreleaser build --clean --single-target --snapshot --timeout 10m 2>&1; then
      err "Failed to build binaries with goreleaser"
    fi
    [ -n "${AICRD_BIN}" ] || AICRD_BIN="$(find_dist_binary aicrd "${os_name}" "${arch_name}" || true)"
    [ -n "${AICR_BIN}" ] || AICR_BIN="$(find_dist_binary aicr "${os_name}" "${arch_name}" || true)"
  fi

  if [ -z "${AICRD_BIN}" ] || [ ! -x "${AICRD_BIN}" ]; then
    err "aicrd server binary not found (build it, or set AICRD_BIN; note aicrd is a linux-only goreleaser target)"
  fi
  if [ -z "${AICR_BIN}" ] || [ ! -x "${AICR_BIN}" ]; then
    err "aicr CLI binary not found (needed for recipe generation + verify; build it, or set AICR_BIN)"
  fi

  msg "Server binary: ${AICRD_BIN}"
  msg "CLI binary:    ${AICR_BIN}"
  export AICRD_BIN AICR_BIN

  # Detect the aicrd binary attestation (FindBinaryAttestation convention:
  # <binary>-attestation.sigstore.json) to choose full vs smoke mode. The server
  # embeds this as tool provenance and verifies it against its own digest at
  # startup, so full mode only runs when the binary actually carries one.
  BINARY_ATTESTATION_FILE="$(dirname "${AICRD_BIN}")/aicrd-attestation.sigstore.json"
  if [ -f "${BINARY_ATTESTATION_FILE}" ]; then
    AICR_ATTESTED="true"
    msg "aicrd binary attestation found: ${BINARY_ATTESTATION_FILE}"
  else
    AICR_ATTESTED="false"
    BINARY_ATTESTATION_FILE=""
    msg "No aicrd binary attestation next to ${AICRD_BIN}; running smoke mode"
  fi
}

# =============================================================================
# Recipe body
# =============================================================================

# The server's POST /v1/bundle body is a resolved RecipeResult JSON. Generate it
# with the aicr CLI (the same shape /v1/recipe would return), matching the CLI
# suite's criteria (eks / h100 / ubuntu / training).
generate_recipe_body() {
  msg "Generating RecipeResult body (eks / h100 / ubuntu / training)"
  "${AICR_BIN}" recipe --service eks --accelerator h100 --os ubuntu \
    --intent training --format json -o "${WORK_DIR}/recipe.json"
  test -s "${WORK_DIR}/recipe.json"
}

# =============================================================================
# Server lifecycle
# =============================================================================

pick_server_port() {
  if [ -z "${SERVER_PORT}" ]; then
    SERVER_PORT="$(python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()')"
  fi
  msg "Server port: ${SERVER_PORT}"
}

# start_server starts aicrd in the background with the given extra environment
# (name=value pairs) and waits for /health. Fails fast (dumping logs) if the
# process exits during startup — e.g. a signing server that cannot verify its own
# binary attestation must not be polled for the full retry budget.
start_server() {
  local desc="$1"
  shift
  msg "Starting aicrd server (${desc}) on port ${SERVER_PORT}"

  local log_level="info"
  [ "${DEBUG:-false}" = "true" ] && log_level="debug"

  # PORT selects the listen port; AICR_LOG_LEVEL controls verbosity. Any extra
  # name=value args carry the signing identity for full mode.
  PORT="${SERVER_PORT}" AICR_LOG_LEVEL="${log_level}" \
    env "$@" "${AICRD_BIN}" >"${SERVER_LOG}" 2>&1 &
  SERVER_PID=$!

  local retries=30
  until [ ${retries} -eq 0 ]; do
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
      log_error "aicrd exited during startup; server log:"
      tail -30 "${SERVER_LOG}" >&2 || true
      SERVER_PID=""
      err "aicrd server failed to start (${desc})"
    fi
    if curl -sf --max-time 3 "http://127.0.0.1:${SERVER_PORT}/health" >/dev/null 2>&1; then
      msg "aicrd server is ready"
      return 0
    fi
    retries=$((retries - 1))
    sleep 1
  done
  log_error "aicrd did not become ready in time; server log:"
  tail -30 "${SERVER_LOG}" >&2 || true
  err "aicrd server /health never became ready (${desc})"
}

# =============================================================================
# Smoke test
# =============================================================================

# smoke_test starts aicrd WITHOUT a signing identity and validates the server
# plumbing: attest=true is rejected (400, not configured) and an unsigned bundle
# is still produced. Used when no attested aicrd binary is available.
smoke_test() {
  msg "Smoke mode (no attested aicrd binary): validating server plumbing only"

  # Start without any AICR_SIGNING_* env: the server comes up with signing
  # disabled. VAULT_ADDR/VAULT_TOKEN are harmless without AICR_SIGNING_KEY.
  start_server "signing disabled"

  local base="http://127.0.0.1:${SERVER_PORT}"

  # 1. attest=true must be rejected 400 "not configured" (signing disabled).
  local code
  code=$(curl -s -o "${WORK_DIR}/smoke-attest.json" -w '%{http_code}' \
    -X POST "${base}/v1/bundle?attest=true" \
    -H 'Content-Type: application/json' \
    --data-binary @"${WORK_DIR}/recipe.json")
  if [ "${code}" != "400" ]; then
    log_error "attest=true response body:"
    cat "${WORK_DIR}/smoke-attest.json" >&2 || true
    err "expected HTTP 400 for attest=true on an unconfigured server, got ${code}"
  fi
  if ! grep -qi "not configured" "${WORK_DIR}/smoke-attest.json"; then
    log_error "attest=true response body:"
    cat "${WORK_DIR}/smoke-attest.json" >&2 || true
    err "expected 'not configured' message in the attest=true rejection"
  fi
  msg "attest=true correctly rejected with 400 (server not configured for attestation)"

  # 2. An unsigned bundle POST returns a zip archive.
  code=$(curl -s -o "${WORK_DIR}/smoke-bundle.zip" -w '%{http_code}' \
    -X POST "${base}/v1/bundle" \
    -H 'Content-Type: application/json' \
    --data-binary @"${WORK_DIR}/recipe.json")
  if [ "${code}" != "200" ]; then
    log_error "unsigned bundle response body:"
    cat "${WORK_DIR}/smoke-bundle.zip" >&2 || true
    err "expected HTTP 200 for an unsigned bundle POST, got ${code}"
  fi
  # Confirm the response is a real zip carrying the standard bundle manifest.
  python3 -c "
import sys, zipfile
z = zipfile.ZipFile('${WORK_DIR}/smoke-bundle.zip')
names = z.namelist()
assert any(n.endswith('checksums.txt') for n in names), 'checksums.txt missing from bundle: %r' % names
print('unsigned bundle contains %d entries (checksums.txt present)' % len(names))
"
  msg "Unsigned bundle POST returned a valid zip"

  stop_server

  msg "Smoke checks passed: OpenBAO up, Transit ECDSA P-256 key provisioned, PEM exported, server up, attest=true rejected, unsigned bundle produced"
  msg "To run the full server sign/verify suite, provide an attested aicrd binary (AICRD_BIN + sibling aicrd-attestation.sigstore.json), or run the server-kms-e2e.yaml workflow."
}

# =============================================================================
# Full test
# =============================================================================

# full_test starts aicrd with the Vault KMS signing identity and drives the full
# server-side attestation + verify round-trip.
full_test() {
  msg "Full mode (attested aicrd binary): server sign + verify round-trip"

  # AICR_TLOG_UPLOAD=false: KMS (Mode A) signing with no Rekor upload, so verify
  # uses --insecure-ignore-tlog + --key (the offline/air-gapped path).
  # AICR_BINARY_ATTESTATION_FILE: the sibling attestation the server embeds as
  # tool provenance (and verifies against its own digest at startup).
  # AICR_BINARY_ATTESTATION_IDENTITY_REGEXP: the aicrd binary here is attested by
  # THIS e2e workflow (server-kms-e2e.yaml via generate-slsa-predicate), not the
  # release workflow (on-tag.yaml) the server pins by default, so retarget the
  # certificate-identity pattern to the e2e workflow. Still pinned to NVIDIA/aicr
  # (enforced by verifier.ValidateIdentityPattern). Overridable so a different
  # attesting workflow can supply its own identity.
  local identity_regexp
  identity_regexp="${AICR_BINARY_ATTESTATION_IDENTITY_REGEXP:-^https://github\.com/NVIDIA/aicr/\.github/workflows/server-kms-e2e\.yaml@.*}"
  start_server "hashivault KMS signing" \
    "AICR_SIGNING_KEY=hashivault://${VAULT_KMS_KEY}" \
    "AICR_TLOG_UPLOAD=false" \
    "AICR_BINARY_ATTESTATION_FILE=${BINARY_ATTESTATION_FILE}" \
    "AICR_BINARY_ATTESTATION_IDENTITY_REGEXP=${identity_regexp}" \
    "VAULT_ADDR=${VAULT_ADDR}" \
    "VAULT_TOKEN=${VAULT_TOKEN}"

  local base="http://127.0.0.1:${SERVER_PORT}"

  # 1. Request a signed bundle.
  local code
  code=$(curl -s -o "${WORK_DIR}/bundle.zip" -w '%{http_code}' \
    -X POST "${base}/v1/bundle?attest=true" \
    -H 'Content-Type: application/json' \
    --data-binary @"${WORK_DIR}/recipe.json")
  if [ "${code}" != "200" ]; then
    log_error "signed bundle response body:"
    cat "${WORK_DIR}/bundle.zip" >&2 || true
    err "expected HTTP 200 for POST /v1/bundle?attest=true, got ${code}"
  fi
  msg "Server returned a signed bundle zip"

  # 2. Unzip it.
  local unzipped="${WORK_DIR}/unzipped"
  rm -rf "${unzipped}"
  python3 -c "
import zipfile
zipfile.ZipFile('${WORK_DIR}/bundle.zip').extractall('${unzipped}')
"

  # 3. The bundle carries both the bundle attestation and the embedded binary
  # (tool provenance) attestation the server injected.
  test -f "${unzipped}/attestation/bundle-attestation.sigstore.json"
  test -f "${unzipped}/attestation/aicr-attestation.sigstore.json"
  msg "Bundle contains attestation/{bundle,aicr}-attestation.sigstore.json"

  # 4. Verify against the Transit key. --insecure-ignore-tlog because the server
  # signed with AICR_TLOG_UPLOAD=false (no Rekor entry to prove trusted time).
  # --certificate-identity-regexp pins the *binary* attestation identity: the
  # aicrd binary here was attested by this e2e workflow, not on-tag.yaml, so
  # verify must accept that identity (same override the server used to embed it)
  # to reach the "verified" trust level instead of "unknown".
  local out
  out=$("${AICR_BIN}" verify "${unzipped}" \
    --key "hashivault://${VAULT_KMS_KEY}" \
    --certificate-identity-regexp "${identity_regexp}" \
    --insecure-ignore-tlog \
    --format json)
  echo "${out}"
  echo "${out}" | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d.get('checksumsPassed') is True, 'checksumsPassed != true: %r' % d.get('checksumsPassed')
assert d.get('bundleAttested') is True, 'bundleAttested != true: %r' % d.get('bundleAttested')
print('verify: checksumsPassed=true, bundleAttested=true')
"

  stop_server

  msg "Full checks passed: server signed the bundle with hashivault://${VAULT_KMS_KEY} and it verified (checksumsPassed + bundleAttested)"
}

# =============================================================================
# Main
# =============================================================================

msg "Starting Server (aicrd) Vault (OpenBAO) KMS bundle-attestation E2E"

mkdir -p "${WORK_DIR}"
start_openbao
provision_transit_key
build_or_locate_binaries
generate_recipe_body
pick_server_port

if [ "${AICR_ATTESTED}" = "true" ]; then
  full_test
else
  smoke_test
fi

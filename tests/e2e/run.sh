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

set -euo pipefail

# =============================================================================
# E2E Tests for aicr with Tilt Cluster
# =============================================================================
#
# This script tests the full aicr workflow with a running Kubernetes cluster
# and the aicrd API server (via Tilt).
#
# Prerequisites:
#   - Tilt cluster running: make dev-env
#   - aicrd accessible at localhost:8080
#
# Usage:
#   ./tests/e2e/run.sh
#
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
DIM='\033[2m'
NC='\033[0m' # No Color

# Configuration
aicrd_URL="${aicrd_URL:-http://localhost:8080}"
OUTPUT_DIR="${OUTPUT_DIR:-$(mktemp -d)}"
AICR_BIN="${AICR_BIN:-}"
AICR_IMAGE="${AICR_IMAGE:-localhost:5001/aicr:local}"
AICR_VALIDATOR_IMAGE="${AICR_VALIDATOR_IMAGE:-localhost:5001/aicr-validator:local}"
SNAPSHOT_NAMESPACE="${SNAPSHOT_NAMESPACE:-default}"
SNAPSHOT_CM="${SNAPSHOT_CM:-aicr-e2e-snapshot}"
FAKE_GPU_ENABLED="${FAKE_GPU_ENABLED:-false}"
CREATED_FAKE_GPU_OPERATOR_DEPLOYMENT=false
CREATED_FAKE_CLUSTER_POLICY=false
CREATED_FAKE_CLUSTER_POLICY_CRD=false

# Seconds to wait for the concurrent runs' self-deleted Jobs to disappear from
# the API before the self-cleanup assertion gives up. Deletion is asynchronous,
# so the count settles shortly after the CLI returns rather than at that instant.
AGENT_JOB_SETTLE_TIMEOUT="${AGENT_JOB_SETTLE_TIMEOUT:-60}"

# Run IDs of the snapshot-agent runs this script launched, space separated.
# cleanup_e2e deletes only these Jobs. Every agent run carries the same
# app.kubernetes.io/{name,component} labels, so a label-only sweep would also
# delete -- and thereby terminate -- a concurrent snapshot run someone else
# started against the same cluster.
E2E_AGENT_RUN_IDS=""

# extract_run_id prints the agent run ID an `aicr snapshot` invocation logged
# ("runID=<id>" on its "snapshot agent run" line), read from the log file
# given as $1. Prints nothing when the run failed before generating one.
extract_run_id() {
  grep -o 'runID=[0-9a-f-]*' "$1" | head -1 | cut -d= -f2 || true
}

# remember_agent_run_id records a run ID in E2E_AGENT_RUN_IDS so cleanup_e2e
# can sweep that run's Job. An empty argument is ignored: a run that never
# reported an ID created no Job for cleanup to find.
remember_agent_run_id() {
  if [ -n "$1" ]; then
    E2E_AGENT_RUN_IDS="${E2E_AGENT_RUN_IDS} $1"
  fi
}

# Test counters
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

# =============================================================================
# Helpers
# =============================================================================

msg() {
  echo -e "${BLUE}[INFO]${NC} $1"
}

warn() {
  echo -e "${YELLOW}[WARN]${NC} $1"
}

err() {
  echo -e "${RED}[ERROR]${NC} $1"
  exit 1
}

pass() {
  local name=$1
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  PASSED_TESTS=$((PASSED_TESTS + 1))
  echo -e "${GREEN}[PASS]${NC} $name"
}

fail() {
  local name=$1
  local reason=${2:-""}
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  FAILED_TESTS=$((FAILED_TESTS + 1))
  if [ -n "$reason" ]; then
    echo -e "${RED}[FAIL]${NC} $name: $reason"
  else
    echo -e "${RED}[FAIL]${NC} $name"
  fi
}

skip() {
  local name=$1
  local reason=${2:-""}
  echo -e "${YELLOW}[SKIP]${NC} $name: $reason"
}

# run_stage invokes one suite stage and always returns 0, so a stage that
# reports failure by returning nonzero cannot trip this script's `set -e` and
# unwind main before cleanup_e2e runs. That unwind is the bug it guards: the
# stages run inside the fake-GPU block create Jobs, RBAC and fixtures on a
# cluster shared with other CI runs, and cleanup_e2e is the only thing that
# sweeps them -- a stage's own local cleanup removes only what that stage made.
#
# Swallowing the status does not make the suite more forgiving. Stages report
# their assertions through fail(), which has already counted them by the time
# one returns; when a stage returns nonzero WITHOUT having recorded a [FAIL] --
# an unexpected error rather than a failed assertion -- one is recorded here
# under the stage's own name. Either way FAILED_TESTS ends up nonzero,
# print_summary returns 1, and main exits nonzero.
run_stage() {
  local stage=$1
  local before=$FAILED_TESTS
  local rc=0
  "$stage" || rc=$?
  if [ "$rc" -ne 0 ] && [ "$FAILED_TESTS" -eq "$before" ]; then
    fail "$stage" "returned ${rc} without recording a failure"
  fi
  return 0
}

check_command() {
  if ! command -v "$1" &> /dev/null; then
    err "$1 is required but not installed"
  fi
}

# Show command being executed
run_cmd() {
  echo -e "${DIM}  \$ $*${NC}"
  "$@"
}

# Show detail/info line
detail() {
  echo -e "${CYAN}     → $1${NC}"
}

# =============================================================================
# Build
# =============================================================================

build_binaries() {
  msg "=========================================="
  msg "Building binaries"
  msg "=========================================="

  # Skip build if AICR_BIN is already set to a valid executable
  if [ -n "$AICR_BIN" ] && [ -x "$AICR_BIN" ]; then
    pass "build/aicr (pre-built)"
    msg "Using: ${AICR_BIN}"
    return 0
  fi

  cd "${ROOT_DIR}"

  # Build aicr directly with go build (simpler than goreleaser for e2e tests)
  local bin_dir="${ROOT_DIR}/dist/e2e"
  mkdir -p "${bin_dir}"

  if ! go build -o "${bin_dir}/aicr" ./cmd/aicr 2>&1; then
    err "Failed to build aicr"
  fi

  AICR_BIN="${bin_dir}/aicr"

  if [ ! -x "$AICR_BIN" ]; then
    err "aicr binary not found at ${AICR_BIN}"
  fi

  pass "build/aicr"
  msg "Using: ${AICR_BIN}"
}

# =============================================================================
# API Health Checks
# =============================================================================

check_api_health() {
  msg "=========================================="
  msg "Checking API health"
  msg "=========================================="

  # Health endpoint
  if curl -sf "${aicrd_URL}/health" > /dev/null 2>&1; then
    pass "api/health"
  else
    fail "api/health" "aicrd not responding at ${aicrd_URL}/health"
    warn "Is Tilt running? Try: make dev-env"
    return 1
  fi

  # Ready endpoint
  if curl -sf "${aicrd_URL}/ready" > /dev/null 2>&1; then
    pass "api/ready"
  else
    fail "api/ready" "aicrd not ready"
    return 1
  fi

  return 0
}

# =============================================================================
# CLI Recipe Tests (from e2e.md)
# =============================================================================

# =============================================================================
# API Recipe Tests (from e2e.md)
# =============================================================================

test_api_recipe() {
  msg "=========================================="
  msg "Testing API recipe endpoints"
  msg "=========================================="

  local recipe_dir="${OUTPUT_DIR}/api-recipes"
  mkdir -p "$recipe_dir"

  # Test 1: GET /v1/recipe with query params
  msg "--- Test: GET /v1/recipe ---"
  echo -e "${DIM}  \$ curl ${aicrd_URL}/v1/recipe?service=eks&accelerator=h100&intent=training${NC}"
  local get_recipe="${recipe_dir}/get.json"
  local http_code
  http_code=$(curl -s -w "%{http_code}" -o "$get_recipe" \
    "${aicrd_URL}/v1/recipe?service=eks&accelerator=h100&intent=training")

  if [ "$http_code" = "200" ] && [ -s "$get_recipe" ]; then
    detail "HTTP ${http_code} OK"
    pass "api/recipe/GET"
  else
    fail "api/recipe/GET" "HTTP $http_code"
  fi

  # Test 2: POST /v1/recipe with YAML body
  msg "--- Test: POST /v1/recipe ---"
  local post_recipe="${recipe_dir}/post.json"
  http_code=$(curl -s -w "%{http_code}" -o "$post_recipe" \
    -X POST "${aicrd_URL}/v1/recipe" \
    -H "Content-Type: application/x-yaml" \
    -d 'criteria:
  service: eks
  accelerator: h100
  intent: training')

  if [ "$http_code" = "200" ] && [ -s "$post_recipe" ]; then
    pass "api/recipe/POST"
  else
    fail "api/recipe/POST" "HTTP $http_code"
  fi
}

# =============================================================================
# CLI Bundle Tests (from e2e.md)
# =============================================================================

# =============================================================================
# API Bundle Tests (from e2e.md)
# =============================================================================

test_api_bundle() {
  msg "=========================================="
  msg "Testing API bundle endpoint"
  msg "=========================================="

  local bundle_dir="${OUTPUT_DIR}/api-bundles"
  mkdir -p "$bundle_dir"

  # Test: POST /v1/bundle (recipe -> bundle pipeline)
  msg "--- Test: POST /v1/bundle ---"
  echo -e "${DIM}  \$ curl -X POST ${aicrd_URL}/v1/bundle?deployer=helm -d <recipe>${NC}"

  # First get a recipe from API
  local recipe_json
  recipe_json=$(curl -s "${aicrd_URL}/v1/recipe?service=eks&accelerator=h100&intent=training")

  if [ -z "$recipe_json" ]; then
    fail "api/bundle/POST" "Could not get recipe from API"
    return 1
  fi

  # Then send to bundle endpoint
  local bundle_zip="${bundle_dir}/bundle.zip"
  local http_code
  http_code=$(curl -s -w "%{http_code}" -o "$bundle_zip" \
    -X POST "${aicrd_URL}/v1/bundle?deployer=helm" \
    -H "Content-Type: application/json" \
    -d "$recipe_json")

  if [ "$http_code" = "200" ] && [ -s "$bundle_zip" ]; then
    # Verify it's a valid zip
    if unzip -t "$bundle_zip" > /dev/null 2>&1; then
      pass "api/bundle/POST"

      # Extract and verify contents
      local extract_dir="${bundle_dir}/extracted"
      mkdir -p "$extract_dir"
      unzip -q "$bundle_zip" -d "$extract_dir"
      if [ -f "${extract_dir}/deploy.sh" ]; then
        pass "api/bundle/contents"
      else
        fail "api/bundle/contents" "deploy.sh not in bundle"
      fi
    else
      fail "api/bundle/POST" "Invalid zip file"
    fi
  else
    fail "api/bundle/POST" "HTTP $http_code"
  fi
}

# =============================================================================
# CLI Help Test
# =============================================================================

# =============================================================================
# Fake GPU Setup (for snapshot tests)
# =============================================================================

setup_fake_gpu() {
  msg "=========================================="
  msg "Setting up fake GPU environment"
  msg "=========================================="

  # Check if we can access the cluster
  if ! kubectl cluster-info > /dev/null 2>&1; then
    warn "Cannot access Kubernetes cluster, skipping fake GPU setup"
    return 1
  fi

  # Check if fake-gpu-operator is already running
  if kubectl get pods -n gpu-operator -l app.kubernetes.io/name=fake-gpu-operator > /dev/null 2>&1; then
    msg "fake-gpu-operator already running"
  fi

  # Inject fake nvidia-smi into Kind worker node
  local fake_smi="${ROOT_DIR}/tools/fake-nvidia-smi"
  if [ -f "$fake_smi" ]; then
    # Find Kind worker nodes
    local workers
    workers=$(docker ps --filter "name=aicr-worker" --format "{{.Names}}" 2>/dev/null || true)
    if [ -n "$workers" ]; then
      for worker in $workers; do
        msg "Injecting fake nvidia-smi into $worker"
        echo -e "${DIM}  \$ docker cp fake-nvidia-smi ${worker}:/usr/local/bin/nvidia-smi${NC}"
        docker cp "$fake_smi" "${worker}:/usr/local/bin/nvidia-smi"
        docker exec "$worker" chmod +x /usr/local/bin/nvidia-smi
        # Show what GPU is being simulated
        local gpu_info
        gpu_info=$(docker exec "$worker" nvidia-smi -L 2>/dev/null | head -1)
        detail "Simulated: ${gpu_info}"
      done
      # Show driver info
      local driver_info
      driver_info=$(docker exec "$worker" nvidia-smi --version 2>/dev/null | head -1)
      detail "Driver: ${driver_info}"
      pass "setup/fake-nvidia-smi"
      FAKE_GPU_ENABLED=true
    else
      warn "No Kind worker nodes found"
      return 1
    fi
  else
    warn "Fake nvidia-smi script not found at $fake_smi"
    return 1
  fi

  # Create namespace for snapshot tests (if it doesn't exist)
  kubectl create namespace "$SNAPSHOT_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

  # No RBAC is pre-provisioned here. Per ADR-020 the agent creates its own
  # run-scoped ServiceAccount, Role/RoleBinding and ClusterRole/ClusterRoleBinding
  # ("aicr-<run-id>" / "aicr-node-reader-<run-id>") and deletes exactly those at
  # the end of the run. The fixture this replaced pre-created a ServiceAccount
  # named "aicr" bound to an "aicr-e2e-reader" ClusterRole granting
  # nodes/pods/configmaps — the same access the agent now grants itself. Nothing
  # referenced that ServiceAccount once the agent stopped using a fixed name, and
  # its presence tripped the agent's adoption-drift warning on every run because
  # it collided with the default name prefix.

  return 0
}

# =============================================================================
# Snapshot Tests (from e2e.md)
# =============================================================================

test_snapshot() {
  msg "=========================================="
  msg "Testing snapshot collection"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "snapshot/agent" "Fake GPU not enabled"
    return 0
  fi

  # Clean up any existing snapshot
  kubectl delete cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" --ignore-not-found=true > /dev/null 2>&1

  # Test: Snapshot via agent deployment from the CI runner.
  # The snapshot command always deploys a Job to capture data on a cluster node.
  msg "--- Test: Snapshot via agent deployment ---"
  detail "Image: ${AICR_IMAGE}"
  detail "Output: cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}"

  echo -e "${DIM}  \$ aicr snapshot --image ${AICR_IMAGE} --namespace ${SNAPSHOT_NAMESPACE} -o cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}${NC}"
  local snapshot_log="${OUTPUT_DIR}/snapshot-agent.log"
  local snapshot_output
  snapshot_output=$("${AICR_BIN}" snapshot \
    --image "${AICR_IMAGE}" \
    --namespace "${SNAPSHOT_NAMESPACE}" \
    --output "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --timeout 120s \
    --privileged \
    --node-selector kubernetes.io/os=linux 2>&1) || true
  printf '%s\n' "$snapshot_output" > "$snapshot_log"
  remember_agent_run_id "$(extract_run_id "$snapshot_log")"

  if kubectl get cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" > /dev/null 2>&1; then
    pass "snapshot/agent"
  else
    echo "$snapshot_output"
    fail "snapshot/agent" "Snapshot ConfigMap not created"
    return 1
  fi

  # Verify snapshot contains GPU data
  msg "--- Test: Snapshot GPU data ---"
  local snapshot_data snapshot_read_stderr=""
  local snapshot_stderr_file="${OUTPUT_DIR}/snapshot-gpu-data.stderr"
  local snapshot_read_exit=0
  snapshot_data=$(kubectl get cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" \
    -o jsonpath='{.data.snapshot\.yaml}' 2>"$snapshot_stderr_file") || \
    snapshot_read_exit=$?

  if [ -s "$snapshot_stderr_file" ]; then
    snapshot_read_stderr=$(<"$snapshot_stderr_file")
  fi

  if [ "$snapshot_read_exit" -ne 0 ]; then
    fail "snapshot/gpu-data" \
      "Could not read snapshot.yaml from ConfigMap (exit ${snapshot_read_exit}): ${snapshot_read_stderr}"
    return 0
  fi

  if [ -z "$snapshot_data" ]; then
    # The ConfigMap exists (asserted above) but carries no snapshot payload:
    # that is a real defect, not an environment difference.
    fail "snapshot/gpu-data" "Snapshot ConfigMap has no snapshot.yaml payload"
    return 0
  fi

  # Keep malformed collector output inside the test accounting rather than
  # letting yq + pipefail terminate the entire suite before the summary.
  if ! printf '%s\n' "$snapshot_data" | yq eval '.' - > /dev/null; then
    fail "snapshot/gpu-data" "Snapshot ConfigMap contains malformed snapshot.yaml"
    return 0
  fi

  local gpu_measurement_count gpu_hardware_count gpu_hardware_complete_count
  gpu_measurement_count=$(printf '%s\n' "$snapshot_data" | \
    yq eval '[.measurements[]? | select(.type == "GPU")] | length' -)
  gpu_hardware_count=$(printf '%s\n' "$snapshot_data" | \
    yq eval '[.measurements[]? | select(.type == "GPU") | .subtypes[]? | select(.subtype == "hardware")] | length' -)
  gpu_hardware_complete_count=$(printf '%s\n' "$snapshot_data" | \
    yq eval '[.measurements[]? | select(.type == "GPU") | .subtypes[]? |
      select(.subtype == "hardware") |
      select((.data | has("gpu-present")) and
             (.data | has("gpu-count")) and
             (.data | has("driver-loaded")) and
             (.data | has("detection-source")))] | length' -)

  # Extract and display GPU info from the driver-free "hardware" subtype
  # (the SMI subtype was removed; model is the PCI-derived accelerator SKU).
  # Scope extraction to the GPU "hardware" subtype so a "model" key elsewhere in
  # the snapshot can't be misread as the GPU SKU.
  local gpu_name gpu_count driver_loaded
  gpu_name=$(printf '%s\n' "$snapshot_data" | yq eval '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.subtype == "hardware") | .data.model // "unknown"' - | head -1)
  gpu_count=$(printf '%s\n' "$snapshot_data" | yq eval '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.subtype == "hardware") | .data["gpu-count"] // 0' - | head -1)
  driver_loaded=$(printf '%s\n' "$snapshot_data" | yq eval '.measurements[] | select(.type == "GPU") | .subtypes[] | select(.subtype == "hardware") | .data["driver-loaded"] // "unknown"' - | head -1)

  if [ "$gpu_measurement_count" -eq 0 ]; then
    fail "snapshot/gpu-data" "Snapshot carries no GPU measurement"
  elif [ "$gpu_hardware_count" -eq 0 ]; then
    # The GPU collector deliberately degrades to an empty subtype list when
    # sysfs/PCI detection is unavailable. Preserve that environment-dependent
    # contract while keeping a missing GPU measurement itself as a hard failure.
    skip "snapshot/gpu-data" "GPU hardware detection unavailable in the snapshot agent"
  elif [ "$gpu_hardware_complete_count" -ne "$gpu_hardware_count" ]; then
    fail "snapshot/gpu-data" \
      "GPU hardware subtype is missing mandatory gpu-present, gpu-count, driver-loaded, or detection-source data"
  elif [ -n "$gpu_name" ] && [ "$gpu_name" != "unknown" ]; then
    detail "GPU SKU: ${gpu_name}"
    detail "Count: ${gpu_count}"
    detail "Driver loaded: ${driver_loaded}"
    pass "snapshot/gpu-data"
  else
    # skip() does not increment the pass count, so an environment that cannot
    # produce a SKU stops being reported as verified GPU coverage.
    skip "snapshot/gpu-data" "No GPU SKU in snapshot (fake-gpu-operator absent or SKU unrecognized)"
  fi
}

# =============================================================================
# Snapshot Run Isolation Tests (ADR-020, issue #2120)
# =============================================================================

# Objects snapshot_run_isolation_body creates that outlive its assertions.
# Published as globals so cleanup_snapshot_run_isolation can remove them on
# every exit path, including an early `return 1` from a failed assertion.
ISOLATION_DECOY=""
ISOLATION_RETAINED_ID=""

# cleanup_snapshot_run_isolation removes the decoy ClusterRole and the
# retained (--no-cleanup) run's objects. Safe to call when the body bailed
# before creating either: both globals are empty then.
cleanup_snapshot_run_isolation() {
  if [ -n "$ISOLATION_DECOY" ]; then
    kubectl delete clusterrole "$ISOLATION_DECOY" --ignore-not-found=true > /dev/null 2>&1 || true
  fi
  if [ -n "$ISOLATION_RETAINED_ID" ]; then
    local ns="$SNAPSHOT_NAMESPACE"
    kubectl delete job "aicr-${ISOLATION_RETAINED_ID}" -n "$ns" --ignore-not-found=true > /dev/null 2>&1 || true
    kubectl delete sa,role,rolebinding "aicr-${ISOLATION_RETAINED_ID}" -n "$ns" --ignore-not-found=true > /dev/null 2>&1 || true
    kubectl delete cm "aicr-agent-snapshot-${ISOLATION_RETAINED_ID}" -n "$ns" --ignore-not-found=true > /dev/null 2>&1 || true
    kubectl delete clusterrole,clusterrolebinding "aicr-node-reader-${ISOLATION_RETAINED_ID}" --ignore-not-found=true > /dev/null 2>&1 || true
  fi
  ISOLATION_DECOY=""
  ISOLATION_RETAINED_ID=""
}

# Verifies that concurrent snapshot runs own and delete only their own
# Kubernetes resources. These checks are cluster-backed on purpose: the
# unit tests in pkg/k8s/agent run against a fake clientset, which runs no
# Job controller (so pod ownerReferences must be hand-seeded there) and does
# not enforce metav1.Preconditions on delete. Only a real apiserver exercises
# both.
#
# Scope note on UID preconditions: the delete-with-stale-UID race itself is
# not reachable from outside the CLI process — the window between an object's
# creation and the deferred Cleanup is the Job wait, and swapping a live
# object's UID mid-run revokes the running agent's own credentials. That
# mechanism is covered by TestCleanupPassesUIDPrecondition and
# TestCleanupTreatsConflictAsSuccess in pkg/k8s/agent/deployer_test.go. What
# is verified here is the ownership contract those preconditions enforce and
# that IS externally observable: cleanup deletes only objects this run
# created, never one that merely matches its labels or name shape.
test_snapshot_run_isolation() {
  local rc=0
  snapshot_run_isolation_body || rc=$?
  # Housekeeping must run whether the body passed or bailed early. Every
  # `return 1` below used to skip it, leaving an aicr-labelled ClusterRole
  # and a full set of run-scoped objects behind for later tests -- and later
  # CI runs on the same cluster -- to trip over. The body's status is
  # preserved so a failed assertion still fails the suite.
  cleanup_snapshot_run_isolation
  return "$rc"
}

snapshot_run_isolation_body() {
  msg "=========================================="
  msg "Testing snapshot run isolation"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "snapshot/isolation" "Fake GPU not enabled"
    return 0
  fi

  local ns="$SNAPSHOT_NAMESPACE"
  local decoy="aicr-node-reader-e2edecoy"
  local rc=0

  # --- Decoy: labelled like a run-owned object, but created by nobody's run ---
  # A cleanup that swept by label or by name shape would collect this. A
  # cleanup scoped to its own created-set must leave it alone.
  msg "--- Test: cleanup is scoped to created objects, not to labels ---"
  kubectl delete clusterrole "$decoy" --ignore-not-found=true > /dev/null 2>&1 || true
  kubectl create clusterrole "$decoy" --verb=get --resource=nodes > /dev/null 2>&1 || true
  kubectl label clusterrole "$decoy" \
    app.kubernetes.io/name=aicr \
    app.kubernetes.io/managed-by=aicr \
    app.kubernetes.io/component=snapshot-agent \
    aicr.run/run-id=e2edecoy --overwrite > /dev/null 2>&1 || true
  ISOLATION_DECOY="$decoy"
  local decoy_uid_before
  decoy_uid_before=$(kubectl get clusterrole "$decoy" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")

  # --- Retained run: --no-cleanup, so its objects must outlive later runs ---
  msg "--- Test: a retained run's resources survive concurrent runs ---"
  local retained_log="${OUTPUT_DIR}/isolation-retained.log"
  "${AICR_BIN}" snapshot \
    --image "${AICR_IMAGE}" \
    --namespace "${ns}" \
    --no-cleanup \
    --output "${OUTPUT_DIR}/isolation-retained.yaml" \
    --timeout 180s \
    --privileged \
    --node-selector kubernetes.io/os=linux > "$retained_log" 2>&1 || rc=$?

  if [ "$rc" -ne 0 ]; then
    cat "$retained_log"
    fail "snapshot/isolation/retained-run" "retained snapshot run failed"
    return 1
  fi

  # Take the run ID from THIS run's own output. A label query returning
  # `.items[0]` would happily hand back an aborted run's Job, or a concurrent
  # snapshot someone else started -- and everything below, including the
  # housekeeping delete, is keyed on this value.
  local retained_id
  retained_id=$(extract_run_id "$retained_log")
  if [ -z "$retained_id" ]; then
    cat "$retained_log"
    fail "snapshot/isolation/run-id-label" "retained run printed no runID"
    return 1
  fi
  ISOLATION_RETAINED_ID="$retained_id"
  remember_agent_run_id "$retained_id"

  # That Job must carry the run ID as a label, and the stable
  # component label consumers select on across runs.
  local retained_job_labels
  retained_job_labels=$(kubectl get job -n "$ns" "aicr-${retained_id}" \
    -o jsonpath='{.metadata.labels.aicr\.run/run-id}/{.metadata.labels.app\.kubernetes\.io/component}' \
    2>/dev/null || echo "")
  if [ "$retained_job_labels" != "${retained_id}/snapshot-agent" ]; then
    fail "snapshot/isolation/run-id-label" \
      "Job aicr-${retained_id} labels run-id/component = '${retained_job_labels}', want '${retained_id}/snapshot-agent'"
    return 1
  fi
  detail "retained run ID: ${retained_id}"
  pass "snapshot/isolation/run-id-label"

  # Every run-owned object must carry the run ID in its name.
  local missing=""
  kubectl get job     -n "$ns" "aicr-${retained_id}"                      > /dev/null 2>&1 || missing="${missing} job"
  kubectl get sa      -n "$ns" "aicr-${retained_id}"                      > /dev/null 2>&1 || missing="${missing} sa"
  kubectl get role    -n "$ns" "aicr-${retained_id}"                      > /dev/null 2>&1 || missing="${missing} role"
  kubectl get rolebinding -n "$ns" "aicr-${retained_id}"                  > /dev/null 2>&1 || missing="${missing} rolebinding"
  kubectl get cm      -n "$ns" "aicr-agent-snapshot-${retained_id}"       > /dev/null 2>&1 || missing="${missing} staging-cm"
  kubectl get clusterrole        "aicr-node-reader-${retained_id}"        > /dev/null 2>&1 || missing="${missing} clusterrole"
  kubectl get clusterrolebinding "aicr-node-reader-${retained_id}"        > /dev/null 2>&1 || missing="${missing} clusterrolebinding"
  if [ -n "$missing" ]; then
    fail "snapshot/isolation/run-scoped-names" "not found under run-scoped names:${missing}"
    return 1
  fi
  pass "snapshot/isolation/run-scoped-names"

  # The agent's staging ConfigMap must not reuse the validator's name shape.
  # aicr validate hands one run ID to both subsystems in one namespace, so a
  # shared aicr-snapshot- prefix would give two owners one object.
  if kubectl get cm -n "$ns" "aicr-snapshot-${retained_id}" > /dev/null 2>&1; then
    fail "snapshot/isolation/staging-cm-name" "found aicr-snapshot-${retained_id}; collides with the validator's data ConfigMap"
    return 1
  fi
  pass "snapshot/isolation/staging-cm-name"

  # The pod must be authorized by its controlling ownerReference, not by a
  # label — pod labels are writable by anything that can update pods.
  msg "--- Test: pod is owned by its Job (controlling ownerReference) ---"
  local pod job_uid owner_uid
  pod=$(kubectl get pods -n "$ns" -l "aicr.run/run-id=${retained_id}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  job_uid=$(kubectl get job -n "$ns" "aicr-${retained_id}" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  owner_uid=$(kubectl get pod -n "$ns" "$pod" \
    -o jsonpath='{.metadata.ownerReferences[?(@.controller==true)].uid}' 2>/dev/null || echo "")
  if [ -n "$pod" ] && [ -n "$job_uid" ] && [ "$job_uid" = "$owner_uid" ]; then
    detail "pod ${pod} controlled by Job UID ${job_uid}"
    pass "snapshot/isolation/pod-owned-by-job"
  else
    fail "snapshot/isolation/pod-owned-by-job" "pod=${pod} jobUID=${job_uid} ownerUID=${owner_uid}"
    return 1
  fi

  # --- Two concurrent runs, with the retained run's objects still present ---
  msg "--- Test: two concurrent runs are independent ---"
  local log_a="${OUTPUT_DIR}/isolation-a.log"
  local log_b="${OUTPUT_DIR}/isolation-b.log"
  local rc_a=0 rc_b=0 pid_a pid_b

  "${AICR_BIN}" snapshot --image "${AICR_IMAGE}" --namespace "${ns}" \
    --output "${OUTPUT_DIR}/isolation-a.yaml" --timeout 180s --privileged \
    --node-selector kubernetes.io/os=linux > "$log_a" 2>&1 &
  pid_a=$!
  "${AICR_BIN}" snapshot --image "${AICR_IMAGE}" --namespace "${ns}" \
    --output "${OUTPUT_DIR}/isolation-b.yaml" --timeout 180s --privileged \
    --node-selector kubernetes.io/os=linux > "$log_b" 2>&1 &
  pid_b=$!

  wait "$pid_a" || rc_a=$?
  wait "$pid_b" || rc_b=$?

  if [ "$rc_a" -ne 0 ] || [ "$rc_b" -ne 0 ]; then
    echo "--- concurrent run A ---"; tail -30 "$log_a"
    echo "--- concurrent run B ---"; tail -30 "$log_b"
    fail "snapshot/isolation/concurrent-runs" "run A rc=${rc_a}, run B rc=${rc_b}"
    return 1
  fi

  local id_a id_b
  id_a=$(extract_run_id "$log_a")
  id_b=$(extract_run_id "$log_b")
  if [ -z "$id_a" ] || [ -z "$id_b" ] || [ "$id_a" = "$id_b" ]; then
    fail "snapshot/isolation/concurrent-runs" "expected two distinct run IDs, got '${id_a}' and '${id_b}'"
    return 1
  fi
  remember_agent_run_id "$id_a"
  remember_agent_run_id "$id_b"
  detail "run A: ${id_a}"
  detail "run B: ${id_b}"
  if [ ! -s "${OUTPUT_DIR}/isolation-a.yaml" ] || [ ! -s "${OUTPUT_DIR}/isolation-b.yaml" ]; then
    fail "snapshot/isolation/concurrent-runs" "a concurrent run produced an empty snapshot"
    return 1
  fi
  pass "snapshot/isolation/concurrent-runs"

  # The retained run's objects must be untouched by both concurrent runs.
  # Before ADR-020 this is exactly what broke: a second run's ensureJob
  # deleted the same-named Job and its cleanup deleted the shared RBAC.
  local destroyed=""
  kubectl get job     -n "$ns" "aicr-${retained_id}"                > /dev/null 2>&1 || destroyed="${destroyed} job"
  kubectl get sa      -n "$ns" "aicr-${retained_id}"                > /dev/null 2>&1 || destroyed="${destroyed} sa"
  kubectl get role    -n "$ns" "aicr-${retained_id}"                > /dev/null 2>&1 || destroyed="${destroyed} role"
  kubectl get rolebinding -n "$ns" "aicr-${retained_id}"            > /dev/null 2>&1 || destroyed="${destroyed} rolebinding"
  kubectl get clusterrole        "aicr-node-reader-${retained_id}"  > /dev/null 2>&1 || destroyed="${destroyed} clusterrole"
  kubectl get clusterrolebinding "aicr-node-reader-${retained_id}"  > /dev/null 2>&1 || destroyed="${destroyed} clusterrolebinding"
  if [ -n "$destroyed" ]; then
    fail "snapshot/isolation/retained-run-survives" "concurrent runs destroyed:${destroyed}"
    return 1
  fi
  pass "snapshot/isolation/retained-run-survives"

  # Each concurrent run must have removed its own resources: exactly the
  # retained Job survives.
  #
  # Poll rather than sample once. Kubernetes deletion is asynchronous -- a Job
  # whose delete the CLI already issued and acked stays listable while its
  # pods terminate and its finalizers clear, so a single read can legitimately
  # still see it and report "found 2" for a cleanup that worked. Only the
  # timing is tolerant; the assertion below is still exact.
  local leftover_jobs=""
  local agent_job_selector="app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent"
  local deadline=$((SECONDS + AGENT_JOB_SETTLE_TIMEOUT))
  while :; do
    leftover_jobs=$(kubectl get jobs -n "$ns" -l "$agent_job_selector" -o name 2>/dev/null | wc -l | tr -d ' ')
    if [ "$leftover_jobs" = "1" ]; then
      break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      kubectl get jobs -n "$ns" -l "$agent_job_selector" -o name || true
      fail "snapshot/isolation/self-cleanup" \
        "expected only the retained Job to remain after ${AGENT_JOB_SETTLE_TIMEOUT}s, found ${leftover_jobs}"
      return 1
    fi
    sleep 2
  done
  pass "snapshot/isolation/self-cleanup"

  # The decoy must have survived every run above.
  local decoy_uid_after
  decoy_uid_after=$(kubectl get clusterrole "$decoy" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
  if [ -n "$decoy_uid_before" ] && [ "$decoy_uid_before" = "$decoy_uid_after" ]; then
    pass "snapshot/isolation/cleanup-scoped-to-created"
  else
    fail "snapshot/isolation/cleanup-scoped-to-created" \
      "aicr-labelled ClusterRole it never created was deleted or replaced (before=${decoy_uid_before} after=${decoy_uid_after})"
    return 1
  fi

  # Housekeeping (the decoy and the retained run's objects) is the wrapper's
  # job -- see cleanup_snapshot_run_isolation -- so it also happens on the
  # `return 1` paths above.
}

# =============================================================================
# Recipe from Snapshot Tests (from e2e.md)
# =============================================================================

test_recipe_from_snapshot() {
  msg "=========================================="
  msg "Testing recipe from snapshot"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "recipe/from-snapshot" "Fake GPU not enabled"
    return 0
  fi

  local recipe_dir="${OUTPUT_DIR}/snapshot-recipes"
  mkdir -p "$recipe_dir"

  # Test: Recipe from ConfigMap snapshot
  msg "--- Test: Recipe from snapshot (cm://...) ---"
  local snapshot_recipe="${recipe_dir}/from-snapshot.yaml"
  # NOTE: --intent inference, not training. The GPU-less Kind cluster cannot
  # state an accelerator, and only kind-inference covers intent without one —
  # training on kind exists only accelerator-qualified (h100-kind-training),
  # so --intent training is rejected by the criteria-coverage post-condition
  # (issue #1542) as an uncovered dimension.
  echo -e "${DIM}  \$ aicr recipe --snapshot cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM} --intent inference -o from-snapshot.yaml${NC}"
  if "${AICR_BIN}" recipe \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --intent inference \
    --output "$snapshot_recipe" 2>&1; then
    if [ -f "$snapshot_recipe" ] && grep -q "kind: RecipeResult" "$snapshot_recipe"; then
      # Show detected criteria
      local service accelerator
      service=$(grep "^  service:" "$snapshot_recipe" 2>/dev/null | head -1 | awk '{print $2}')
      accelerator=$(grep "^  accelerator:" "$snapshot_recipe" 2>/dev/null | head -1 | awk '{print $2}')
      detail "Detected: service=${service:-auto}, accelerator=${accelerator:-auto}"
      pass "recipe/from-snapshot"
    else
      fail "recipe/from-snapshot" "Recipe file invalid"
    fi
  else
    fail "recipe/from-snapshot" "Command failed"
  fi

  # Test: View recipe constraints
  msg "--- Test: Recipe constraints ---"
  if [ -f "$snapshot_recipe" ]; then
    if ! yq eval '.' "$snapshot_recipe" > /dev/null; then
      fail "recipe/constraints" "Generated recipe is not valid YAML"
    elif yq eval -e '.constraints | (type == "!!seq" and length > 0)' \
      "$snapshot_recipe" > /dev/null 2>&1; then
      pass "recipe/constraints"
    else
      # The resolved recipe above is valid RecipeResult, so a missing
      # constraints block is a resolution defect, not an environment difference.
      fail "recipe/constraints" "Generated recipe carries no top-level constraints"
    fi
  else
    skip "recipe/constraints" "No recipe file"
  fi
}

# =============================================================================
# Validate Tests (from e2e.md)
# =============================================================================

test_validate() {
  msg "=========================================="
  msg "Testing recipe validation (multi-phase)"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/recipe" "Fake GPU not enabled"
    skip "validate/multi-phase" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-multiphase"
  mkdir -p "$validate_dir"

  # Generate a recipe for testing.
  # NOTE: --intent inference for coverage on the GPU-less Kind cluster —
  # see the recipe-from-snapshot test above for the full rationale.
  local recipe_file="${validate_dir}/recipe.yaml"
  "${AICR_BIN}" recipe \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --intent inference \
    --output "$recipe_file" 2>&1 || true

  if [ ! -f "$recipe_file" ]; then
    skip "validate/recipe" "Could not generate recipe"
    skip "validate/multi-phase" "Could not generate recipe"
    return 0
  fi

  # Test 1: Deployment phase
  msg "--- Test: Validate with --phase deployment ---"
  echo -e "${DIM}  \$ aicr validate --phase deployment${NC}"
  local deployment_result="${validate_dir}/validation-deployment.json"
  local deployment_output
  deployment_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$deployment_result" 2>&1) || true

  # Check the output file is valid CTRF (phase may have 0 tests if recipe has no deployment checks)
  if [ -f "$deployment_result" ] && jq -e '.reportFormat == "CTRF"' "$deployment_result" > /dev/null 2>&1; then
    detail "Deployment phase: PASS"
    pass "validate/phase-deployment"
  else
    fail "validate/phase-deployment" "Invalid or missing CTRF output"
  fi

  # Test 2: Performance phase
  msg "--- Test: Validate with --phase performance ---"
  echo -e "${DIM}  \$ aicr validate --phase performance${NC}"
  local performance_result="${validate_dir}/validation-performance.json"
  local performance_output
  performance_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase performance \
    --output "$performance_result" 2>&1) || true

  # Check the output file is valid CTRF (phase may have 0 tests if recipe has no performance checks)
  if [ -f "$performance_result" ] && jq -e '.reportFormat == "CTRF"' "$performance_result" > /dev/null 2>&1; then
    detail "Performance phase: PASS"
    pass "validate/phase-performance"
  else
    fail "validate/phase-performance" "Invalid or missing CTRF output"
  fi

  # Test 3: All phases (also covers basic validate/recipe)
  msg "--- Test: Validate with --phase all ---"
  echo -e "${DIM}  \$ aicr validate --phase all${NC}"
  local all_result="${validate_dir}/validation-all.json"
  local all_output
  all_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase all \
    --output "$all_result" 2>&1) || true

  # Check that the summary has tests >= 0
  if [ -f "$all_result" ] && jq -e '.results.summary.tests >= 0' "$all_result" > /dev/null 2>&1; then
    detail "All phases: PASS"
    pass "validate/recipe"
    pass "validate/phase-all"
  else
    fail "validate/recipe" "Expected summary with tests >= 0 in output"
    fail "validate/phase-all" "Expected summary with tests >= 0 in output"
  fi

  # Test 4: Verify phase result structure
  if [ -f "$all_result" ]; then
    msg "--- Test: Verify CTRF result structure ---"
    echo -e "${DIM}  \$ jq .reportFormat validation-all.json${NC}"

    if [ -f "$all_result" ] && jq -e '.reportFormat == "CTRF"' "$all_result" > /dev/null 2>&1; then
      detail "CTRF result structure: PASS"
      pass "validate/result-structure"
    else
      fail "validate/result-structure" "reportFormat field not found in CTRF result"
    fi
  fi
}

# =============================================================================
# External Data Tests (--data flag)
# =============================================================================


# =============================================================================
# Deployment Phase Constraint Tests
# =============================================================================

setup_fake_gpu_operator_fixture() {
  CREATED_FAKE_GPU_OPERATOR_DEPLOYMENT=false
  CREATED_FAKE_CLUSTER_POLICY=false
  CREATED_FAKE_CLUSTER_POLICY_CRD=false

  kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f - 2>&1 || true
  kubectl delete jobs,pods -n gpu-operator -l app=aicr-validator --ignore-not-found 2>&1 || true

  if kubectl get deployment gpu-operator -n gpu-operator >/dev/null 2>&1; then
    warn "Skipping fake GPU operator fixture: deployment/gpu-operator already exists in namespace gpu-operator"
    return 1
  fi
  if kubectl get clusterpolicy cluster-policy >/dev/null 2>&1; then
    warn "Skipping fake GPU operator fixture: ClusterPolicy cluster-policy already exists"
    return 1
  fi
  if kubectl get crd clusterpolicies.nvidia.com >/dev/null 2>&1; then
    warn "Skipping fake GPU operator fixture: CRD clusterpolicies.nvidia.com already exists"
    return 1
  fi

  # Deliberate: the CRD below does NOT declare `subresources: { status: {} }`.
  # We want kubectl apply on the ClusterPolicy CR that follows to persist the
  # literal .status.state: ready field, so the validator's
  # verifyClusterPolicyReady check sees the fixture as "ready". If a status
  # subresource were declared, kubectl apply would silently drop .status
  # updates (status is then only writable via PATCH on the /status
  # subresource), and every deployment-phase test that depends on this
  # fixture would start failing. If you need to add a status subresource
  # later, also update the fixture to write .status via `kubectl patch
  # --subresource=status`.
  if ! cat <<YAML | kubectl apply -f - 2>&1; then
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterpolicies.nvidia.com
spec:
  group: nvidia.com
  scope: Cluster
  names:
    plural: clusterpolicies
    singular: clusterpolicy
    kind: ClusterPolicy
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        x-kubernetes-preserve-unknown-fields: true
YAML
    return 1
  fi
  CREATED_FAKE_CLUSTER_POLICY_CRD=true

  if ! kubectl wait --for=condition=Established crd/clusterpolicies.nvidia.com --timeout=60s 2>&1; then
    cleanup_fake_gpu_operator_fixture
    return 1
  fi

  if ! cat <<YAML | kubectl apply -f - 2>&1; then
apiVersion: nvidia.com/v1
kind: ClusterPolicy
metadata:
  name: cluster-policy
status:
  state: ready
YAML
    cleanup_fake_gpu_operator_fixture
    return 1
  fi
  CREATED_FAKE_CLUSTER_POLICY=true

  if ! cat <<YAML | kubectl apply -f - 2>&1; then
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
  labels:
    app.kubernetes.io/name: gpu-operator
    app.kubernetes.io/version: v24.6.0
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gpu-operator
  template:
    metadata:
      labels:
        app: gpu-operator
    spec:
      containers:
      - name: gpu-operator
        image: nvcr.io/nvidia/gpu-operator:v24.6.0
        imagePullPolicy: IfNotPresent
YAML
    cleanup_fake_gpu_operator_fixture
    return 1
  fi
  CREATED_FAKE_GPU_OPERATOR_DEPLOYMENT=true

  if ! kubectl wait --for=condition=available deployment/gpu-operator -n gpu-operator --timeout=60s 2>&1; then
    cleanup_fake_gpu_operator_fixture
    return 1
  fi
}

cleanup_fake_gpu_operator_fixture() {
  if [ "${CREATED_FAKE_GPU_OPERATOR_DEPLOYMENT}" = "true" ]; then
    kubectl delete deployment gpu-operator -n gpu-operator --ignore-not-found 2>&1 || true
    CREATED_FAKE_GPU_OPERATOR_DEPLOYMENT=false
  fi
  if [ "${CREATED_FAKE_CLUSTER_POLICY}" = "true" ]; then
    kubectl delete clusterpolicy cluster-policy --ignore-not-found 2>&1 || true
    CREATED_FAKE_CLUSTER_POLICY=false
  fi
  if [ "${CREATED_FAKE_CLUSTER_POLICY_CRD}" = "true" ]; then
    kubectl delete crd clusterpolicies.nvidia.com --ignore-not-found 2>&1 || true
    CREATED_FAKE_CLUSTER_POLICY_CRD=false
  fi
}

test_validate_deployment_checks() {
  msg "=========================================="
  msg "Testing deployment checks (constraints, expected-resources, chainsaw)"
  msg "=========================================="

  # Create validation namespace for tests
  kubectl create namespace aicr-validation 2>&1 || true

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/deployment-constraints" "Fake GPU not enabled"
    skip "validate/expected-resources" "Fake GPU not enabled"
    skip "validate/chainsaw-healthcheck" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-deployment-checks"
  mkdir -p "$validate_dir"

  # -----------------------------------------------------------------------
  # Shared setup: Create fake GPU operator readiness fixture ONCE
  # -----------------------------------------------------------------------
  msg "--- Setup: Create fake GPU operator readiness fixture ---"

  if setup_fake_gpu_operator_fixture; then
    detail "Created fake GPU operator deployment and ready ClusterPolicy fixture"
  else
    skip "validate/deployment-constraint-pass" "Could not create GPU operator deployment"
    skip "validate/deployment-constraint-fail" "Could not create GPU operator deployment"
    skip "validate/expected-resources-fail" "Could not create GPU operator deployment"
    skip "validate/expected-resources-manual-pass" "Could not create GPU operator deployment"
    skip "validate/expected-resources-manual-merge" "Could not create GPU operator deployment"
    skip "validate/chainsaw-healthcheck-pass" "Could not create GPU operator deployment"
    skip "validate/chainsaw-healthcheck-fail" "Could not create GPU operator deployment"
    return 0
  fi

  # -----------------------------------------------------------------------
  # Constraint tests
  # -----------------------------------------------------------------------
  msg "=========================================="
  msg "Deployment constraint tests"
  msg "=========================================="

  # Test: Validate with passing constraint
  local recipe_file="${validate_dir}/recipe-with-constraints.yaml"
  cat > "$recipe_file" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
validation:
  deployment:
    checks:
      - gpu-operator-version
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"
RECIPE

  msg "--- Test: Deployment constraint (should pass) ---"
  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe.yaml${NC}"
  local deployment_result="${validate_dir}/validation-deployment-pass.json"
  local deployment_output
  deployment_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$deployment_result" 2>&1) || true

  detail "Captured validation output:"
  echo "$deployment_output" | sed 's/^/    /'

  if [ -f "$deployment_result" ]; then
    detail "Validation output file created: $deployment_result"
  else
    detail "Validation output file NOT created: $deployment_result"
  fi

  if [ -f "$deployment_result" ] && \
     grep -q "gpu-operator-version" "$deployment_result"; then
    if grep -A1 '"gpu-operator-version"' "$deployment_result" | grep -q '"status": "passed"'; then
      detail "GPU operator version constraint: PASS (v24.6.0 >= v24.6.0)"
      pass "validate/deployment-constraint-pass"
    else
      detail "Constraint found but status unclear. Showing report:"
      grep -A5 "gpu-operator-version" "$deployment_result" | sed 's/^/    /' || true
      fail "validate/deployment-constraint-pass" "Constraint status unclear"
    fi
  else
    fail "validate/deployment-constraint-pass" "Constraint not evaluated (not found in output)"
  fi

  # Test: Validate with failing constraint
  msg "--- Test: Deployment constraint (should fail) ---"
  local recipe_file_fail="${validate_dir}/recipe-with-failing-constraint.yaml"
  cat > "$recipe_file_fail" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
validation:
  deployment:
    checks:
      - gpu-operator-version
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v25.0.0"
RECIPE

  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe.yaml${NC}"
  local deployment_fail_result="${validate_dir}/validation-deployment-fail.json"
  local deployment_fail_output
  deployment_fail_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file_fail" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$deployment_fail_result" 2>&1) || true

  # Negative test: the named constraint MUST report the intended version
  # mismatch. A generic validator failure (context/RBAC/crash) is not proof that
  # the comparison itself rejected v24.6.0.
  if [ -f "$deployment_fail_result" ] && \
     jq -e 'any(.results.tests[]?; .name == "gpu-operator-version")' \
       "$deployment_fail_result" > /dev/null 2>&1; then
    if jq -e 'any(.results.tests[]?;
                  .name == "gpu-operator-version" and
                  .status == "failed" and
                  ((.message // "") |
                    contains("GPU Operator version v24.6.0 does not satisfy constraint \">= v25.0.0\"")))' \
         "$deployment_fail_result" > /dev/null 2>&1; then
      detail "GPU operator version constraint: FAIL (v24.6.0 < v25.0.0) - as expected"
      pass "validate/deployment-constraint-fail"
    else
      local deployment_fail_actual
      deployment_fail_actual=$(jq -c \
        'first(.results.tests[]? | select(.name == "gpu-operator-version")) | {status, message}' \
        "$deployment_fail_result" 2>/dev/null || echo "unavailable")
      fail "validate/deployment-constraint-fail" \
        "Expected the v24.6.0 versus >= v25.0.0 mismatch; got ${deployment_fail_actual}"
    fi
  else
    fail "validate/deployment-constraint-fail" \
      "Constraint not evaluated (gpu-operator-version absent from CTRF output)"
  fi

  # -----------------------------------------------------------------------
  # Expected-resources tests
  # -----------------------------------------------------------------------
  msg "=========================================="
  msg "Expected-resources tests"
  msg "=========================================="

  # Test: Validate expected-resources with failing check (resource missing)
  msg "--- Test: Expected resources check (should fail - missing resource) ---"
  local recipe_er_fail="${validate_dir}/recipe-expected-resources-fail.yaml"
  cat > "$recipe_er_fail" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  # Enabled GPU advertiser: allocation-policy resolution (#1327) rejects
  # advertiser-less recipes (row 6) before any validator runs; the ref
  # carries no expectedResources so it is inert for these checks.
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
  - name: nonexistent-component
    type: Helm
    source: https://charts.example.com
    chart: nonexistent-component
    version: v1.0.0
    namespace: gpu-operator
    expectedResources:
      - kind: Deployment
        name: nonexistent-deployment
        namespace: gpu-operator
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe-fail.yaml${NC}"
  local result_er_fail="${validate_dir}/result-er-fail.json"
  local result_er_fail_output
  result_er_fail_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_er_fail" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --image "${AICR_VALIDATOR_IMAGE}" \
    --output "$result_er_fail" 2>&1) || true

  # Bind name, status, and the missing-resource diagnosis to the SAME CTRF test
  # object. A generic expected-resources infrastructure failure is not proof
  # that nonexistent-deployment was actually checked.
  if [ -f "$result_er_fail" ] && \
     jq -e 'any(.results.tests[]?; .name == "expected-resources")' \
       "$result_er_fail" > /dev/null 2>&1; then
    if jq -e 'any(.results.tests[]?;
                  .name == "expected-resources" and
                  .status == "failed" and
                  ((.message // "") |
                    contains("nonexistent-deployment") and contains("not found")))' \
         "$result_er_fail" > /dev/null 2>&1; then
      detail "Expected-resources check: FAIL (nonexistent-deployment not found) - as expected"
      pass "validate/expected-resources-fail"
    else
      local expected_resources_fail_actual
      expected_resources_fail_actual=$(jq -c \
        'first(.results.tests[]? | select(.name == "expected-resources")) | {status, message}' \
        "$result_er_fail" 2>/dev/null || echo "unavailable")
      fail "validate/expected-resources-fail" \
        "Expected nonexistent-deployment not found; got ${expected_resources_fail_actual}"
    fi
  else
    fail "validate/expected-resources-fail" "expected-resources not found in output"
  fi

  # Manual expectedResources with a real helm-installed workload
  if ! command -v helm &> /dev/null; then
    skip "validate/expected-resources-manual-pass" "helm CLI not available"
    skip "validate/expected-resources-manual-merge" "helm CLI not available"
  elif [ "${AICR_E2E_FULL:-false}" != "true" ]; then
    skip "validate/expected-resources-manual-pass" "Set AICR_E2E_FULL=true for helm tests"
    skip "validate/expected-resources-manual-merge" "Set AICR_E2E_FULL=true for helm tests"
  else
    local nginx_ns="aicr-e2e-nginx"
    local nginx_release="nginx-test"
    local helm_install_ok=false

    # Setup: Install Bitnami nginx
    msg "--- Setup: Installing Bitnami nginx chart ---"
    kubectl create namespace "$nginx_ns" --dry-run=client -o yaml | kubectl apply -f - 2>&1 || true
    echo -e "${DIM}  \$ helm install $nginx_release nginx --repo https://charts.bitnami.com/bitnami -n $nginx_ns${NC}"
    if helm install "$nginx_release" nginx \
        --repo https://charts.bitnami.com/bitnami \
        --namespace "$nginx_ns" \
        --set replicaCount=1 \
        --set service.type=ClusterIP \
        --set "resources.requests.cpu=50m" \
        --set "resources.requests.memory=64Mi" \
        --wait --timeout 120s 2>&1; then
      detail "Installed $nginx_release in $nginx_ns"
      helm_install_ok=true
    else
      detail "helm install failed (network or chart issue)"
    fi

    if [ "$helm_install_ok" = true ]; then
      # Derive the installed chart version so the recipes below describe what
      # is actually deployed (the install is unpinned; a fabricated pin would
      # lie). Coherence requires SOME version on a chart-referencing ref.
      # Fail closed: the pipeline is guarded against set -e/pipefail so a
      # helm/jq failure reaches the fail branch (and the nginx cleanup below)
      # instead of killing the run, and only an exact nginx-<version> result
      # is accepted — an empty or null answer must not become a fabricated pin.
      local nginx_chart nginx_chart_ver=""
      nginx_chart=$(helm list -n "$nginx_ns" -o json 2>/dev/null \
        | jq -r --arg r "$nginx_release" '[.[] | select(.name == $r) | .chart][0] // empty' \
        2>/dev/null) || nginx_chart=""
      case "$nginx_chart" in
        nginx-?*) nginx_chart_ver="${nginx_chart#nginx-}" ;;
      esac

      if [ -z "$nginx_chart_ver" ]; then
        fail "validate/expected-resources-manual-pass" "could not determine installed nginx chart version (helm list chart: '${nginx_chart}')"
        fail "validate/expected-resources-manual-merge" "could not determine installed nginx chart version (helm list chart: '${nginx_chart}')"
      else

      # Test: Manual expectedResources pointing to real Deployment (should pass)
      msg "--- Test: Manual expectedResources matching deployed workload ---"
      local recipe_manual="${validate_dir}/recipe-manual-pass.yaml"
      cat > "$recipe_manual" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  # Enabled GPU advertiser: allocation-policy resolution (#1327) rejects
  # advertiser-less recipes (row 6) before any validator runs; the ref
  # carries no expectedResources so it is inert for these checks.
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
  - name: ${nginx_release}
    type: Helm
    source: https://charts.bitnami.com/bitnami
    chart: nginx
    version: "${nginx_chart_ver}"
    namespace: ${nginx_ns}
    expectedResources:
      - kind: Deployment
        name: ${nginx_release}
        namespace: ${nginx_ns}
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

      echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe-manual-pass.yaml${NC}"
      local result_manual="${validate_dir}/result-manual-pass.json"
      local result_manual_output
      result_manual_output=$("${AICR_BIN}" validate \
        --recipe "$recipe_manual" \
        --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
        --phase deployment \
        --image "${AICR_VALIDATOR_IMAGE}" \
        --output "$result_manual" 2>&1) || true

      detail "Captured validation output:"
      echo "$result_manual_output" | sed 's/^/    /'

      if [ -f "$result_manual" ] && grep -q '"expected-resources"' "$result_manual"; then
        if grep -A1 '"expected-resources"' "$result_manual" | grep -q '"status": "passed"'; then
          detail "Expected-resources check passed for deployed nginx"
          pass "validate/expected-resources-manual-pass"
        else
          fail "validate/expected-resources-manual-pass" "Check did not pass for deployed resource"
        fi
      else
        fail "validate/expected-resources-manual-pass" "expected-resources not found in output"
      fi

      # Test: Merge — one real resource + one fake resource
      msg "--- Test: Manual expectedResources merge (real + fake) ---"
      local recipe_merge="${validate_dir}/recipe-manual-merge.yaml"
      cat > "$recipe_merge" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  # Enabled GPU advertiser: allocation-policy resolution (#1327) rejects
  # advertiser-less recipes (row 6) before any validator runs; the ref
  # carries no expectedResources so it is inert for these checks.
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
  - name: ${nginx_release}
    type: Helm
    source: https://charts.bitnami.com/bitnami
    chart: nginx
    version: "${nginx_chart_ver}"
    namespace: ${nginx_ns}
    expectedResources:
      - kind: Deployment
        name: ${nginx_release}
        namespace: ${nginx_ns}
      - kind: Deployment
        name: nonexistent-deploy
        namespace: ${nginx_ns}
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

      echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe-manual-merge.yaml${NC}"
      local result_merge="${validate_dir}/result-manual-merge.json"
      local result_merge_output
      result_merge_output=$("${AICR_BIN}" validate \
        --recipe "$recipe_merge" \
        --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
        --phase deployment \
        --image "${AICR_VALIDATOR_IMAGE}" \
        --output "$result_merge" 2>&1) || true

      detail "Captured validation output:"
      echo "$result_merge_output" | sed 's/^/    /'

      if [ -f "$result_merge" ] && grep -q '"expected-resources"' "$result_merge"; then
        if grep -A1 '"expected-resources"' "$result_merge" | grep -q '"status": "failed"'; then
          detail "Expected-resources check correctly failed for missing resource in merge"
          pass "validate/expected-resources-manual-merge"
        else
          fail "validate/expected-resources-manual-merge" "Check should have failed for nonexistent-deploy but passed"
        fi
      else
        fail "validate/expected-resources-manual-merge" "expected-resources not found in output"
      fi
      fi
    else
      skip "validate/expected-resources-manual-pass" "helm install failed"
      skip "validate/expected-resources-manual-merge" "helm install failed"
    fi

    # Cleanup nginx chart
    msg "--- Cleanup: Removing nginx chart ---"
    helm uninstall "$nginx_release" -n "$nginx_ns" 2>&1 || true
    kubectl delete namespace "$nginx_ns" 2>&1 || true
  fi

  # -----------------------------------------------------------------------
  # Chainsaw health check tests
  # -----------------------------------------------------------------------
  msg "=========================================="
  msg "Chainsaw health check tests"
  msg "=========================================="

  # Re-check deployment is available (may have been affected by prior tests)
  if ! kubectl wait --for=condition=available deployment/gpu-operator -n gpu-operator --timeout=60s 2>&1; then
    skip "validate/chainsaw-healthcheck-pass" "GPU operator deployment not available"
    skip "validate/chainsaw-healthcheck-fail" "GPU operator deployment not available"
    cleanup_fake_gpu_operator_fixture
    return 0
  fi

  # Clean up leftover validator pods before chainsaw tests
  kubectl delete jobs,pods -n gpu-operator -l app=aicr-validator --ignore-not-found 2>&1 || true

  local recipe_chainsaw="${validate_dir}/recipe-chainsaw.yaml"
  cat > "$recipe_chainsaw" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

  # Test: Chainsaw health check should pass using embedded registry
  msg "--- Test: Chainsaw health check via embedded registry (should pass) ---"

  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe.yaml${NC}"
  local result_chainsaw_pass="${validate_dir}/result-chainsaw-pass.json"
  local result_chainsaw_output
  local validate_exit=0
  result_chainsaw_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_chainsaw" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --namespace aicr-validation \
    --image "${AICR_VALIDATOR_IMAGE}" \
    --output "$result_chainsaw_pass" 2>&1) || validate_exit=$?

  detail "Captured validation output:"
  echo "$result_chainsaw_output" | sed 's/^/    /'

  if [ -f "$result_chainsaw_pass" ] && \
     grep -q '"expected-resources"' "$result_chainsaw_pass"; then
    if grep -A1 '"expected-resources"' "$result_chainsaw_pass" | grep -q '"status": "passed"'; then
      detail "Chainsaw health check: PASS (gpu-operator deployment found via embedded assert)"
      pass "validate/chainsaw-healthcheck-pass"
    elif grep -q '"summary"' "$result_chainsaw_pass" && grep -q '"status": "passed"' "$result_chainsaw_pass"; then
      detail "Chainsaw health check: PASS (from summary status)"
      pass "validate/chainsaw-healthcheck-pass"
    else
      detail "Check found but status unclear. Showing check section:"
      grep -A5 '"expected-resources"' "$result_chainsaw_pass" | sed 's/^/    /' || true
      fail "validate/chainsaw-healthcheck-pass" "Check did not pass"
    fi
  else
    fail "validate/chainsaw-healthcheck-pass" "expected-resources not found in output"
  fi

  # Test: Expected-resources should fail when recipe declares a nonexistent resource
  msg "--- Test: Expected-resources check (should fail - nonexistent resource) ---"
  local recipe_chainsaw_fail="${validate_dir}/recipe-chainsaw-fail.yaml"
  cat > "$recipe_chainsaw_fail" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
    expectedResources:
      - kind: Deployment
        name: nonexistent-gpu-operator
        namespace: gpu-operator
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe-fail.yaml (should fail)${NC}"
  local result_chainsaw_fail="${validate_dir}/result-chainsaw-fail.json"
  local result_chainsaw_fail_output
  local validate_fail_exit=0
  result_chainsaw_fail_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_chainsaw_fail" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --namespace aicr-validation \
    --output "$result_chainsaw_fail" 2>&1) || validate_fail_exit=$?

  detail "Captured validation output:"
  echo "$result_chainsaw_fail_output" | sed 's/^/    /'

  if [ -f "$result_chainsaw_fail" ] && \
     grep -q '"expected-resources"' "$result_chainsaw_fail"; then
    if grep -A1 '"expected-resources"' "$result_chainsaw_fail" | grep -q '"status": "failed"'; then
      detail "Expected-resources check: FAIL (nonexistent resource not found) - as expected"
      pass "validate/chainsaw-healthcheck-fail"
    elif grep -q '"summary"' "$result_chainsaw_fail" && grep -q '"status": "failed"' "$result_chainsaw_fail"; then
      detail "Expected-resources check: FAIL (from summary status) - as expected"
      pass "validate/chainsaw-healthcheck-fail"
    else
      fail "validate/chainsaw-healthcheck-fail" "Check did not fail for nonexistent resource"
    fi
  else
    fail "validate/chainsaw-healthcheck-fail" "expected-resources not found in output"
  fi

  # -----------------------------------------------------------------------
  # Single cleanup for ALL deployment check tests
  # -----------------------------------------------------------------------
  msg "--- Cleanup: Removing fake GPU operator readiness fixture ---"
  cleanup_fake_gpu_operator_fixture
}

test_validate_job_deployment() {
  msg "=========================================="
  msg "Testing validation Job deployment"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/job-deployment" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-jobs"
  mkdir -p "$validate_dir"

  msg "--- Setup: Create fake GPU operator readiness fixture ---"
  if setup_fake_gpu_operator_fixture; then
    detail "Created fake GPU operator deployment and ready ClusterPolicy fixture"
  else
    skip "validate/job-rbac-serviceaccount" "Could not create GPU operator readiness fixture"
    skip "validate/job-rbac-role" "Could not create GPU operator readiness fixture"
    skip "validate/job-creation" "Could not create GPU operator readiness fixture"
    skip "validate/job-success" "Could not create GPU operator readiness fixture"
    skip "validate/command-success" "Could not create GPU operator readiness fixture"
    skip "validate/job-custom-namespace" "Could not create GPU operator readiness fixture"
    skip "validate/job-cleanup" "Could not create GPU operator readiness fixture"
    skip "validate/job-result-format" "Could not create GPU operator readiness fixture"
    return 0
  fi

  # Create a recipe with explicit deployment checks so Jobs are created.
  local recipe_file="${validate_dir}/recipe.yaml"
  cat > "$recipe_file" <<RECIPE
kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    type: Helm
    source: https://helm.ngc.nvidia.com/nvidia
    chart: gpu-operator
    version: v24.6.0
    namespace: gpu-operator
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

  # Test 1: Validation with default namespace
  msg "--- Test: Validation Job in default namespace ---"
  echo -e "${DIM}  \$ aicr validate --recipe recipe.yaml --snapshot cm://... --phase deployment${NC}"

  # Start from an empty namespace so stable labels cannot match resources from
  # an aborted prior invocation. The bounded delete also finishes any
  # best-effort --wait=false teardown still in progress from the previous run.
  local default_validation_namespace="aicr-validation"
  local default_namespace_setup_exit=0
  kubectl delete namespace "$default_validation_namespace" \
    --ignore-not-found --wait=true --timeout=60s 2>&1 || \
    default_namespace_setup_exit=$?
  if [ "$default_namespace_setup_exit" -eq 0 ]; then
    kubectl create namespace "$default_validation_namespace" 2>&1 || \
      default_namespace_setup_exit=$?
  fi

  if [ "$default_namespace_setup_exit" -ne 0 ]; then
    fail "validate/job-rbac-serviceaccount" \
      "Could not reset ${default_validation_namespace} (exit ${default_namespace_setup_exit})"
    skip "validate/job-rbac-role" "Default validation namespace reset failed"
    skip "validate/job-creation" "Default validation namespace reset failed"
    skip "validate/job-success" "Default validation namespace reset failed"
    skip "validate/command-success" "Default validation namespace reset failed"
    skip "validate/job-custom-namespace" "Default validation namespace reset failed"
    skip "validate/job-cleanup" "Default validation namespace reset failed"
    skip "validate/job-result-format" "Default validation namespace reset failed"
    cleanup_fake_gpu_operator_fixture
    return 0
  fi

  # Run validation (this should create Jobs)
  local validation_result="${validate_dir}/validation-default-ns.json"
  local validation_exit=0
  "${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$validation_result" \
    --no-cleanup 2>&1 || validation_exit=$?

  # Check if RBAC resources were created. Locate the run-specific ServiceAccount
  # by its stable label, then use its generated name for the exact CRB lookup.
  local rbac_label_selector="app.kubernetes.io/name=aicr-validator"
  local validation_job_label_selector="app.kubernetes.io/name=aicr"
  local sa_name=""
  local sa_query_exit=0
  sa_name=$(kubectl get sa -n "$default_validation_namespace" \
    -l "${rbac_label_selector}" -o jsonpath='{.items[*].metadata.name}' \
    2>/dev/null) || sa_query_exit=$?
  if [ "$sa_query_exit" -eq 0 ] && [ -n "${sa_name}" ]; then
    detail "ServiceAccount created: ${sa_name}"
    pass "validate/job-rbac-serviceaccount"
  elif [ "$sa_query_exit" -ne 0 ]; then
    fail "validate/job-rbac-serviceaccount" \
      "Could not list validator ServiceAccounts (exit ${sa_query_exit})"
  else
    fail "validate/job-rbac-serviceaccount" "ServiceAccount not found after --no-cleanup"
  fi

  # The per-run ServiceAccount and ClusterRoleBinding have the same generated
  # name. Querying that exact name prevents an orphaned CRB from a prior run
  # satisfying this assertion through the stable label.
  local crb_name="$sa_name"
  local role_ref=""
  local crb_query_exit=0
  if [ -n "$crb_name" ]; then
    role_ref=$(kubectl get clusterrolebinding "$crb_name" \
      -o jsonpath='{.roleRef.name}' 2>/dev/null) || crb_query_exit=$?
  fi
  if [ "$crb_query_exit" -eq 0 ] && [ -n "$crb_name" ] && [ -n "$role_ref" ]; then
    detail "ClusterRoleBinding created: ${crb_name} → ${role_ref}"
    pass "validate/job-rbac-role"
  elif [ "$crb_query_exit" -ne 0 ]; then
    fail "validate/job-rbac-role" \
      "Could not read ClusterRoleBinding ${crb_name} (exit ${crb_query_exit})"
  else
    fail "validate/job-rbac-role" "ClusterRoleBinding not found after --no-cleanup"
  fi

  # Identify the Job owned by this run's retained ServiceAccount. The later
  # cleanup-enabled invocation must not delete this --no-cleanup Job while it
  # removes only the Job it creates itself.
  local retained_job_name=""
  local retained_job_query_exit=0
  if [ -n "$sa_name" ]; then
    retained_job_name=$(kubectl get jobs -n aicr-validation \
      -l "$validation_job_label_selector" -o json 2>/dev/null | \
      jq -r --arg service_account "$sa_name" \
        '[.items[]? |
          select(.spec.template.spec.serviceAccountName == $service_account) |
          .metadata.name][0] // empty') || retained_job_query_exit=$?
  fi

  # Check if jobs were created (they may not exist if recipe has no checks)
  local job_count
  job_count=$(kubectl get jobs -n aicr-validation --no-headers 2>/dev/null | grep -c "aicr-" || echo "0")

  if [ "$job_count" -gt 0 ]; then
    detail "Validation jobs created: $job_count"
    pass "validate/job-creation"

    # Check job success status (not just completion)
    # Job status shows "1/1" for completion but we need to check .status.succeeded
    local succeeded_jobs
    succeeded_jobs=$(kubectl get jobs -n aicr-validation -o jsonpath='{range .items[?(@.status.succeeded==1)]}{.metadata.name}{"\n"}{end}' 2>/dev/null | wc -l)

    if [ "$succeeded_jobs" -eq "$job_count" ]; then
      detail "All jobs succeeded: $succeeded_jobs/$job_count"
      pass "validate/job-success"
    else
      local failed_jobs
      failed_jobs=$(kubectl get jobs -n aicr-validation -o jsonpath='{range .items[?(@.status.failed>=1)]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
      if [ -n "$failed_jobs" ]; then
        warn "Some jobs failed:"
        echo "$failed_jobs" | while read -r job_name; do
          warn "  - $job_name"
          # Show logs for failed job
          local pod_name
          pod_name=$(kubectl get pods -n aicr-validation -l "batch.kubernetes.io/job-name=$job_name" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
          if [ -n "$pod_name" ]; then
            detail "Last 10 lines of logs:"
            kubectl logs -n aicr-validation "$pod_name" --tail=10 2>&1 | sed 's/^/    /' || true
          fi
        done
      fi
      fail "validate/job-success" "Expected $job_count succeeded jobs, got $succeeded_jobs"
    fi

    # Check validation command exit code
    if [ "$validation_exit" -eq 0 ]; then
      detail "Validation command succeeded (exit code: 0)"
      pass "validate/command-success"
    else
      fail "validate/command-success" "Validation command failed with exit code: $validation_exit"
    fi
  else
    # If no jobs were created but we expected them, that's a failure
    fail "validate/job-creation" "No validation jobs created"
  fi

  # Test 2: Validation with custom namespace
  local custom_validation_namespace="custom-validation"
  msg "--- Test: Validation Job in custom namespace ---"
  echo -e "${DIM}  \$ aicr validate --namespace ${custom_validation_namespace}${NC}"

  # Start from an empty namespace so resources left by an aborted prior run
  # cannot satisfy this run's assertions.
  local custom_namespace_setup_exit=0
  kubectl delete namespace "$custom_validation_namespace" \
    --ignore-not-found --wait=true --timeout=60s 2>&1 || \
    custom_namespace_setup_exit=$?
  if [ "$custom_namespace_setup_exit" -eq 0 ]; then
    kubectl create namespace "$custom_validation_namespace" 2>&1 || \
      custom_namespace_setup_exit=$?
  fi

  # --no-cleanup so the per-run RBAC outlives the call. With cleanup enabled
  # (the default) deferClusterCleanup -> CleanupRBAC deletes the ServiceAccount
  # before it can be observed, so asserting its presence afterwards could never
  # hold. Cleanup is covered separately by validate/job-cleanup below, and the
  # teardown at the end of this function removes the namespace and the labelled
  # ClusterRoleBinding this leaves behind.
  local validation_custom="${validate_dir}/validation-custom-ns.json"
  local custom_validation_exit=0
  if [ "$custom_namespace_setup_exit" -eq 0 ]; then
    "${AICR_BIN}" validate \
      --recipe "$recipe_file" \
      --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
      --phase deployment \
      --namespace "$custom_validation_namespace" \
      --output "$validation_custom" \
      --no-cleanup \
      2>&1 || custom_validation_exit=$?
  fi

  # --no-cleanup preserves both resources. Since the namespace was empty before
  # this invocation, a passing report plus a labelled SA and Job proves the
  # current run deployed and completed in --namespace.
  local custom_sa_name custom_sa_query_exit=0
  local custom_job_name custom_job_query_exit=0
  if [ "$custom_namespace_setup_exit" -eq 0 ]; then
    custom_sa_name=$(kubectl get sa -n "$custom_validation_namespace" \
      -l "$rbac_label_selector" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) || \
      custom_sa_query_exit=$?
    custom_job_name=$(kubectl get jobs -n "$custom_validation_namespace" \
      -l "$validation_job_label_selector" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) || \
      custom_job_query_exit=$?
  fi

  if [ "$custom_namespace_setup_exit" -ne 0 ]; then
    fail "validate/job-custom-namespace" \
      "Could not reset ${custom_validation_namespace} (exit ${custom_namespace_setup_exit})"
  elif [ "$custom_validation_exit" -ne 0 ]; then
    fail "validate/job-custom-namespace" \
      "Validation command failed in ${custom_validation_namespace} (exit ${custom_validation_exit})"
  elif [ ! -f "$validation_custom" ] || \
       ! jq -e 'any(.results.tests[]?;
                    .name == "expected-resources" and .status == "passed")' \
            "$validation_custom" > /dev/null 2>&1; then
    fail "validate/job-custom-namespace" \
      "No passing expected-resources result from ${custom_validation_namespace}"
  elif [ "$custom_sa_query_exit" -ne 0 ]; then
    fail "validate/job-custom-namespace" \
      "Could not list validator ServiceAccounts in ${custom_validation_namespace} (exit ${custom_sa_query_exit})"
  elif [ -z "$custom_sa_name" ]; then
    fail "validate/job-custom-namespace" \
      "No ServiceAccount labelled ${rbac_label_selector} in ${custom_validation_namespace}"
  elif [ "$custom_job_query_exit" -ne 0 ]; then
    fail "validate/job-custom-namespace" \
      "Could not list validator Jobs in ${custom_validation_namespace} (exit ${custom_job_query_exit})"
  elif [ -z "$custom_job_name" ]; then
    fail "validate/job-custom-namespace" \
      "No Job labelled ${validation_job_label_selector} in ${custom_validation_namespace}"
  else
    detail "ServiceAccount created in ${custom_validation_namespace}: ${custom_sa_name}"
    detail "Validator Job created in ${custom_validation_namespace}: ${custom_job_name}"
    pass "validate/job-custom-namespace"
  fi

  # Test 3: Job cleanup (verify cleanup from default namespace run with --no-cleanup)
  msg "--- Test: Validation Job cleanup ---"
  echo -e "${DIM}  \$ aicr validate${NC}"

  # Record the Job name set before the run. Names are compared rather than
  # counted so a Job leaked by this run cannot be masked by an unrelated Job
  # disappearing over the same window.
  local jobs_before_file="${validate_dir}/jobs-before.txt"
  local jobs_after_file="${validate_dir}/jobs-after.txt"
  local jobs_before_exit=0
  kubectl get jobs -n aicr-validation \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
    > "$jobs_before_file" 2>/dev/null || jobs_before_exit=$?

  # Run validation with cleanup enabled. Keep check failures in CTRF so the CLI
  # exit code is reserved for orchestration/serialization/cleanup errors; a
  # passing expected-resources result below proves the validator Job ran.
  local validation_cleanup="${validate_dir}/validation-cleanup.json"
  local cleanup_exit=0
  "${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$validation_cleanup" \
    --fail-on-error=false \
    2>&1 || cleanup_exit=$?

  # Retry only while Jobs introduced by this cleanup-enabled run remain. The
  # retained --no-cleanup Job is part of the before-set, so it never drives the
  # wait. A healthy synchronous cleanup completes on the first inventory.
  local jobs_after_exit=0
  local leaked_jobs=""
  local job_diff_exit=0
  local cleanup_inventory_attempt=0
  local cleanup_inventory_attempts=30
  while [ "$cleanup_inventory_attempt" -lt "$cleanup_inventory_attempts" ]; do
    jobs_after_exit=0
    kubectl get jobs -n aicr-validation \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
      > "$jobs_after_file" 2>/dev/null || jobs_after_exit=$?

    leaked_jobs=""
    job_diff_exit=0
    if [ "$jobs_before_exit" -eq 0 ] && [ "$jobs_after_exit" -eq 0 ]; then
      leaked_jobs=$(grep -Fvx -f "$jobs_before_file" "$jobs_after_file") || \
        job_diff_exit=$?
      # grep returns 1 when every post-run Job was already present before the
      # run. That is the expected no-leak result, not a comparison failure.
      if [ "$job_diff_exit" -eq 1 ]; then
        job_diff_exit=0
      fi
      leaked_jobs=${leaked_jobs//$'\n'/ }
    fi

    if [ "$jobs_before_exit" -ne 0 ] || [ "$jobs_after_exit" -ne 0 ] || \
       [ "$job_diff_exit" -ne 0 ] || [ -z "$leaked_jobs" ]; then
      break
    fi

    cleanup_inventory_attempt=$((cleanup_inventory_attempt + 1))
    if [ "$cleanup_inventory_attempt" -lt "$cleanup_inventory_attempts" ]; then
      sleep 1
    fi
  done

  local retained_job_check_output=""
  local retained_job_check_exit=0
  if [ "$retained_job_query_exit" -eq 0 ] && [ -n "$retained_job_name" ]; then
    retained_job_check_output=$(kubectl get job "$retained_job_name" \
      -n aicr-validation -o name 2>&1) || retained_job_check_exit=$?
  fi

  if [ "$jobs_before_exit" -ne 0 ]; then
    fail "validate/job-cleanup" \
      "Could not inventory Jobs before cleanup (exit ${jobs_before_exit})"
  elif [ "$jobs_after_exit" -ne 0 ]; then
    fail "validate/job-cleanup" \
      "Could not inventory Jobs after cleanup (exit ${jobs_after_exit})"
  elif [ "$job_diff_exit" -ne 0 ]; then
    fail "validate/job-cleanup" \
      "Could not compare Job inventories (exit ${job_diff_exit})"
  elif [ "$retained_job_query_exit" -ne 0 ]; then
    fail "validate/job-cleanup" \
      "Could not identify the Job retained by --no-cleanup (exit ${retained_job_query_exit})"
  elif [ -z "$retained_job_name" ]; then
    fail "validate/job-cleanup" \
      "No Job was associated with the ServiceAccount retained by --no-cleanup"
  elif [ "$retained_job_check_exit" -ne 0 ]; then
    fail "validate/job-cleanup" \
      "Could not verify retained Job ${retained_job_name} after cleanup (exit ${retained_job_check_exit}): ${retained_job_check_output}"
  elif [ -n "$leaked_jobs" ]; then
    # Report leaks before command/report failures so the cleanup defect is not
    # hidden by an unrelated validator outcome.
    fail "validate/job-cleanup" \
      "Validator Jobs not cleaned up after bounded retry: ${leaked_jobs}"
  elif [ "$cleanup_exit" -ne 0 ]; then
    fail "validate/job-cleanup" \
      "Validation command failed (exit ${cleanup_exit}); cleanup could not be verified"
  elif [ ! -f "$validation_cleanup" ] || \
       ! jq -e 'any(.results.tests[]?;
                    .name == "expected-resources" and .status == "passed")' \
            "$validation_cleanup" > /dev/null 2>&1; then
    fail "validate/job-cleanup" \
      "Cleanup run produced no passing expected-resources result; cleanup could not be verified"
  else
    detail "Retained --no-cleanup Job still present: ${retained_job_name}"
    detail "Jobs cleaned up successfully (no new Jobs remain in aicr-validation)"
    pass "validate/job-cleanup"
  fi

  # Test 4: Validation result format
  msg "--- Test: Validation result format ---"
  if [ -f "$validation_result" ]; then
    # Assert the CTRF envelope the same way validate/result-structure does.
    # The previous condition OR-ed the identical grep with itself, so the
    # second operand could never change the outcome.
    if jq -e '.reportFormat == "CTRF"' "$validation_result" > /dev/null 2>&1; then
      detail "Validation result has correct structure"
      pass "validate/job-result-format"
    else
      fail "validate/job-result-format" "reportFormat is not \"CTRF\" in ${validation_result}"
    fi
  else
    fail "validate/job-result-format" "Validation result file not created at ${validation_result}"
  fi

  # Cleanup test namespaces, then any per-run validator ClusterRoleBindings.
  # CRBs are cluster-scoped and survive namespace deletion, and the `--no-cleanup`
  # run earlier in this test intentionally leaves its CRB behind. Label-based
  # bulk delete also cleans up any stale CRBs from prior failed runs so they
  # cannot be picked up by the label selector in the next invocation. Namespace
  # teardown is best-effort and non-blocking so stuck finalizers cannot suppress
  # the test summary.
  kubectl delete namespace aicr-validation \
    --ignore-not-found --wait=false 2>&1 || true
  kubectl delete namespace "$custom_validation_namespace" \
    --ignore-not-found --wait=false 2>&1 || true
  kubectl delete clusterrolebinding -l "$rbac_label_selector" 2>&1 || true
  cleanup_fake_gpu_operator_fixture
}


# =============================================================================
# API Metrics Tests
# =============================================================================

test_api_metrics() {
  msg "=========================================="
  msg "Testing API metrics endpoint"
  msg "=========================================="

  # Test: GET /metrics (Prometheus format)
  msg "--- Test: GET /metrics ---"
  echo -e "${DIM}  \$ curl ${aicrd_URL}/metrics${NC}"

  local metrics_output="${OUTPUT_DIR}/metrics.txt"
  local http_code
  http_code=$(curl -s -w "%{http_code}" -o "$metrics_output" "${aicrd_URL}/metrics")

  if [ "$http_code" = "200" ] && [ -s "$metrics_output" ]; then
    # Verify it's Prometheus format (should contain # HELP or # TYPE)
    if grep -q "# HELP\|# TYPE" "$metrics_output" 2>/dev/null; then
      # Show some metric names
      local metric_count
      metric_count=$(grep -c "^# HELP" "$metrics_output" 2>/dev/null || echo "0")
      detail "HTTP ${http_code} OK - Prometheus format (${metric_count} metrics)"

      # Check for expected aicr metrics
      if grep -q "http_requests_total\|recipe_built_duration" "$metrics_output" 2>/dev/null; then
        detail "aicr-specific metrics present"
      fi
      pass "api/metrics"
    else
      fail "api/metrics" "Response not in Prometheus format"
    fi
  else
    fail "api/metrics" "HTTP $http_code"
  fi
}

# =============================================================================
# Output Format Tests (--format json/table)
# =============================================================================

# =============================================================================
# OCI Bundle Tests (from e2e.md)
# =============================================================================

test_oci_bundle() {
  msg "=========================================="
  msg "Testing OCI bundle"
  msg "=========================================="

  # Check if we have a local registry
  if ! curl -sf http://localhost:5001/v2/ > /dev/null 2>&1; then
    skip "bundle/oci" "Local registry not available"
    return 0
  fi

  local oci_dir="${OUTPUT_DIR}/oci-bundle"
  mkdir -p "$oci_dir"

  # Generate a recipe first
  local recipe_file="${oci_dir}/recipe.yaml"
  "${AICR_BIN}" recipe \
    --service eks \
    --accelerator h100 \
    --intent training \
    --output "$recipe_file" 2>&1 || true

  if [ ! -f "$recipe_file" ]; then
    skip "bundle/oci" "Could not generate recipe"
    return 0
  fi

  # Test: Bundle as OCI image
  # Note: This may fail with local HTTP registries due to HTTPS enforcement in ORAS
  msg "--- Test: Bundle as OCI image ---"
  local digest_file="${oci_dir}/.digest"
  local bundle_output
  bundle_output=$("${AICR_BIN}" bundle \
    --recipe "$recipe_file" \
    --output "oci://localhost:5001/aicr-e2e-bundle" \
    --deployer helm \
    --insecure-tls \
    --image-refs "$digest_file" 2>&1) || true

  if [ -f "$digest_file" ]; then
    pass "bundle/oci-push"
    msg "Bundle pushed: $(cat "$digest_file")"
  elif echo "$bundle_output" | grep -q "http: server gave HTTP response to HTTPS client"; then
    # Known issue with local insecure registries
    warn "OCI push failed due to HTTP/HTTPS mismatch (expected with local registry)"
    skip "bundle/oci-push" "Local registry requires HTTPS client config"
  elif curl -sf http://localhost:5001/v2/aicr-e2e-bundle/tags/list 2>/dev/null | grep -q "dev\|latest"; then
    pass "bundle/oci-push"
  else
    fail "bundle/oci-push" "Command failed"
  fi
}

# =============================================================================
# Cleanup
# =============================================================================

cleanup_e2e() {
  msg "=========================================="
  msg "Cleaning up e2e resources"
  msg "=========================================="

  # Clean up snapshot resources. The Job name is run-scoped ("aicr-<run-id>"),
  # so select by label -- but scoped to the run IDs THIS script launched.
  # A sweep of every Job matching the agent's labels would also delete, and
  # so terminate, a concurrent snapshot run started against the same cluster
  # by someone else.
  local run_id
  for run_id in $E2E_AGENT_RUN_IDS; do
    kubectl delete job \
      -l "app.kubernetes.io/name=aicr,app.kubernetes.io/component=snapshot-agent,aicr.run/run-id=${run_id}" \
      -n "$SNAPSHOT_NAMESPACE" --ignore-not-found=true > /dev/null 2>&1 || true
  done
  kubectl delete cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" --ignore-not-found=true > /dev/null 2>&1 || true

  msg "Cleanup complete"
}

# =============================================================================
# Summary
# =============================================================================

print_summary() {
  echo ""
  msg "=========================================="
  msg "Test Summary"
  msg "=========================================="
  echo "Total:  ${TOTAL_TESTS}"
  echo -e "Passed: ${GREEN}${PASSED_TESTS}${NC}"
  echo -e "Failed: ${RED}${FAILED_TESTS}${NC}"
  echo ""
  msg "Output: ${OUTPUT_DIR}"

  if [ "$FAILED_TESTS" -gt 0 ]; then
    return 1
  fi
  return 0
}

# =============================================================================
# Main
# =============================================================================

main() {
  msg "AICR E2E Tests"
  msg "Output directory: ${OUTPUT_DIR}"
  msg "API URL: ${aicrd_URL}"
  echo ""

  # Check required tools
  check_command curl
  check_command make

  # Build binaries
  build_binaries

  # Check API is available
  if ! check_api_health; then
    warn "API not available, skipping API tests"
    API_AVAILABLE=false
  else
    API_AVAILABLE=true
  fi

  # Run API tests (if available)
  # NOTE: Pure CLI tests (recipe, bundle, help, output formats, external data,
  # deploy agent flags) are covered by chainsaw CLI tests in the CLI E2E job.
  # This script focuses on cluster-dependent tests only.
  if [ "$API_AVAILABLE" = true ]; then
    test_api_recipe
    test_api_bundle
    test_api_metrics
  fi

  # Setup fake GPU environment and run snapshot tests
  if setup_fake_gpu; then
    test_snapshot
    # Guarded: this stage returns its assertion status, which `set -e` would
    # otherwise take as the script's exit status -- skipping every stage below
    # AND cleanup_e2e, and stranding this run's resources on a shared cluster.
    run_stage test_snapshot_run_isolation
    test_recipe_from_snapshot
    test_validate
    test_validate_deployment_checks
    test_validate_job_deployment
    test_oci_bundle
    cleanup_e2e
  else
    warn "Skipping snapshot/validate/OCI tests (fake GPU setup failed)"
  fi

  # Print summary and exit
  if print_summary; then
    msg "All tests passed!"
    exit 0
  else
    err "Some tests failed"
  fi
}

# Run main only when executed directly; sourcing exposes the pure helpers
# (pass, fail, run_stage, print_summary) without contacting a cluster.
# tools/e2e-run_test.sh drives them via `make test` (test-shell target).
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi

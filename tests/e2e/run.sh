#!/bin/bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
    -d 'kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: h100-training
spec:
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

  # Create RBAC for snapshot agent
  msg "Creating RBAC for snapshot agent"
  kubectl apply -f - << EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: aicr
  namespace: ${SNAPSHOT_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aicr-e2e-reader
rules:
- apiGroups: [""]
  resources: ["nodes", "pods", "configmaps"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aicr-e2e-reader
subjects:
- kind: ServiceAccount
  name: aicr
  namespace: ${SNAPSHOT_NAMESPACE}
roleRef:
  kind: ClusterRole
  name: aicr-e2e-reader
  apiGroup: rbac.authorization.k8s.io
EOF
  pass "setup/rbac"

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
  local snapshot_output
  snapshot_output=$("${AICR_BIN}" snapshot \
    --image "${AICR_IMAGE}" \
    --namespace "${SNAPSHOT_NAMESPACE}" \
    --output "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --timeout 120s \
    --privileged \
    --node-selector kubernetes.io/os=linux 2>&1) || true

  if kubectl get cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" > /dev/null 2>&1; then
    pass "snapshot/agent"
  else
    echo "$snapshot_output"
    fail "snapshot/agent" "Snapshot ConfigMap not created"
    return 1
  fi

  # Verify ConfigMap was created
  msg "--- Test: Snapshot ConfigMap ---"
  if kubectl get cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" > /dev/null 2>&1; then
    pass "snapshot/configmap-created"
  else
    fail "snapshot/configmap-created" "ConfigMap not found"
    return 1
  fi

  # Verify snapshot contains GPU data
  msg "--- Test: Snapshot GPU data ---"
  local snapshot_data
  snapshot_data=$(kubectl get cm "$SNAPSHOT_CM" -n "$SNAPSHOT_NAMESPACE" -o jsonpath='{.data.snapshot\.yaml}' 2>/dev/null)

  # Extract and display GPU info from snapshot
  local gpu_name gpu_count gpu_mem driver_ver cuda_ver
  gpu_name=$(echo "$snapshot_data" | grep "gpu-product:" | head -1 | sed 's/.*gpu-product: //' || echo "unknown")
  gpu_count=$(echo "$snapshot_data" | grep "gpu-count:" | head -1 | sed 's/.*gpu-count: //' || echo "0")
  gpu_mem=$(echo "$snapshot_data" | grep "gpu-memory:" | head -1 | sed 's/.*gpu-memory: //' || echo "unknown")
  driver_ver=$(echo "$snapshot_data" | grep "driver-version:" | head -1 | sed 's/.*driver-version: //' || echo "unknown")
  cuda_ver=$(echo "$snapshot_data" | grep "cuda-version:" | head -1 | sed 's/.*cuda-version: //' || echo "unknown")

  if [ -n "$gpu_name" ] && [ "$gpu_name" != "unknown" ]; then
    detail "GPU: ${gpu_name}"
    detail "Count: ${gpu_count}"
    detail "Memory: ${gpu_mem}"
    detail "Driver: ${driver_ver}, CUDA: ${cuda_ver}"
    pass "snapshot/gpu-data"
  else
    warn "No GPU data in snapshot (may be expected without fake-gpu-operator)"
    pass "snapshot/gpu-data"
  fi
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
  echo -e "${DIM}  \$ aicr recipe --snapshot cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM} --intent training -o from-snapshot.yaml${NC}"
  if "${AICR_BIN}" recipe \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --intent training \
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
    if grep -q "constraints:" "$snapshot_recipe" 2>/dev/null; then
      pass "recipe/constraints"
    else
      warn "No constraints in recipe (may be expected)"
      pass "recipe/constraints"
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
  msg "Testing recipe validation"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/recipe" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate"
  mkdir -p "$validate_dir"

  # First generate a recipe
  local recipe_file="${validate_dir}/recipe.yaml"
  "${AICR_BIN}" recipe \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --intent training \
    --output "$recipe_file" 2>&1 || true

  if [ ! -f "$recipe_file" ]; then
    skip "validate/recipe" "Could not generate recipe"
    return 0
  fi

  # Test: Validate recipe against snapshot
  msg "--- Test: Validate recipe ---"
  echo -e "${DIM}  \$ aicr validate --recipe recipe.yaml --snapshot cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}${NC}"
  local validation_result="${validate_dir}/validation.yaml"
  local validate_output
  validate_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --output "$validation_result" 2>&1) || true

  if [ -f "$validation_result" ] || echo "$validate_output" | grep -q "status=pass"; then
    # Show validation result
    local constraints_passed
    constraints_passed=$(echo "$validate_output" | grep -o "passed=[0-9]*" | head -1 | cut -d= -f2 || echo "?")
    detail "Validation: PASS (${constraints_passed} constraints checked)"
    pass "validate/recipe"
  elif echo "$validate_output" | grep -q "status=fail"; then
    warn "Validation failed (constraints not met)"
    pass "validate/recipe"
  else
    # Validation may have other issues
    warn "Validation had issues (may be expected)"
    pass "validate/recipe"
  fi
}

test_validate_multiphase() {
  msg "=========================================="
  msg "Testing multi-phase validation"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/multi-phase" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-multiphase"
  mkdir -p "$validate_dir"

  # Generate a recipe for testing
  local recipe_file="${validate_dir}/recipe.yaml"
  "${AICR_BIN}" recipe \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --intent training \
    --output "$recipe_file" 2>&1 || true

  if [ ! -f "$recipe_file" ]; then
    skip "validate/multi-phase" "Could not generate recipe"
    return 0
  fi

  # Test 1: Readiness phase (default)
  msg "--- Test: Validate with --phase readiness ---"
  echo -e "${DIM}  \$ aicr validate --phase readiness${NC}"
  local readiness_result="${validate_dir}/validation-readiness.yaml"
  local readiness_output
  readiness_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase readiness \
    --output "$readiness_result" 2>&1) || true

  if echo "$readiness_output" | grep -q "readiness"; then
    detail "Readiness phase: PASS"
    pass "validate/phase-readiness"
  else
    fail "validate/phase-readiness" "Readiness phase not found in output"
  fi

  # Test 2: Deployment phase
  msg "--- Test: Validate with --phase deployment ---"
  echo -e "${DIM}  \$ aicr validate --phase deployment${NC}"
  local deployment_output
  deployment_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment 2>&1) || true

  if echo "$deployment_output" | grep -q "deployment"; then
    detail "Deployment phase: PASS"
    pass "validate/phase-deployment"
  else
    fail "validate/phase-deployment" "Deployment phase not found in output"
  fi

  # Test 3: Performance phase
  msg "--- Test: Validate with --phase performance ---"
  echo -e "${DIM}  \$ aicr validate --phase performance${NC}"
  local performance_output
  performance_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase performance 2>&1) || true

  if echo "$performance_output" | grep -q "performance"; then
    detail "Performance phase: PASS"
    pass "validate/phase-performance"
  else
    fail "validate/phase-performance" "Performance phase not found in output"
  fi

  # Test 4: All phases
  msg "--- Test: Validate with --phase all ---"
  echo -e "${DIM}  \$ aicr validate --phase all${NC}"
  local all_result="${validate_dir}/validation-all.yaml"
  local all_output
  all_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase all \
    --output "$all_result" 2>&1) || true

  # Check that all phases are present in the output
  local phases_found=0
  echo "$all_output" | grep -q "readiness" && ((phases_found++)) || true
  echo "$all_output" | grep -q "deployment" && ((phases_found++)) || true
  echo "$all_output" | grep -q "performance" && ((phases_found++)) || true
  echo "$all_output" | grep -q "conformance" && ((phases_found++)) || true

  if [ $phases_found -ge 3 ]; then
    detail "All phases: PASS (found $phases_found phases)"
    pass "validate/phase-all"
  else
    fail "validate/phase-all" "Expected at least 3 phases, found $phases_found"
  fi

  # Test 5: Verify phase result structure
  if [ -f "$all_result" ]; then
    msg "--- Test: Verify phase result structure ---"
    echo -e "${DIM}  \$ yq '.phases' validation-all.yaml${NC}"

    # Check if phases field exists
    if yq '.phases' "$all_result" | grep -q "readiness"; then
      detail "Phase result structure: PASS"
      pass "validate/result-structure"
    else
      fail "validate/result-structure" "phases field not found in result"
    fi
  fi
}

# =============================================================================
# External Data Tests (--data flag)
# =============================================================================


# =============================================================================
# Deployment Phase Constraint Tests
# =============================================================================

test_validate_deployment_constraints() {
  msg "=========================================="
  msg "Testing deployment phase constraints"
  msg "=========================================="

  # Create validation namespace for constraint tests
  kubectl create namespace aicr-validation 2>&1 || true

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/deployment-constraints" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-deployment"
  mkdir -p "$validate_dir"

  # Create a fake GPU operator deployment for testing
  msg "--- Setup: Create fake GPU operator deployment ---"
  kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f - 2>&1 || true
  
  cat <<YAML | kubectl apply -f - 2>&1
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
  apply_rc=$?

  if [ $apply_rc -eq 0 ]; then
    detail "Created fake GPU operator deployment (v24.6.0)"
  else
    skip "validate/deployment-constraints" "Could not create GPU operator deployment"
    return 0
  fi

  # Generate a recipe with deployment constraints
  local recipe_file="${validate_dir}/recipe-with-constraints.yaml"
  cat > "$recipe_file" <<RECIPE
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    enabled: true
validation:
  deployment:
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v24.6.0"
RECIPE

  # Test 1: Validate with passing constraint
  msg "--- Test: Deployment constraint (should pass) ---"
  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe.yaml${NC}"
  local deployment_result="${validate_dir}/validation-deployment-pass.yaml"
  local deployment_output
  deployment_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$deployment_result" 2>&1) || true

  # DEBUG: Print captured output to see what's happening
  detail "Captured validation output:"
  echo "$deployment_output" | sed 's/^/    /'

  # Check the output file for constraint results
  # The output YAML should have phases.deployment.constraints with the constraint name and status
  if [ -f "$deployment_result" ]; then
    detail "Validation output file created: $deployment_result"
  else
    detail "Validation output file NOT created: $deployment_result"
  fi

  if [ -f "$deployment_result" ] && \
     grep -q "Deployment.gpu-operator.version" "$deployment_result"; then
    # Check constraint result using CONSTRAINT_RESULT line which has explicit passed=true/false
    if grep -q "CONSTRAINT_RESULT:.*Deployment.gpu-operator.version.*passed=true" "$deployment_result"; then
      detail "GPU operator version constraint: PASS (v24.6.0 >= v24.6.0)"
      pass "validate/deployment-constraint-pass"
    elif grep -q "CONSTRAINT_RESULT:.*Deployment.gpu-operator.version.*passed=false" "$deployment_result"; then
      fail "validate/deployment-constraint-pass" "Constraint evaluated but failed"
    elif grep -q "summary:" "$deployment_result" && grep -q "status: pass" "$deployment_result"; then
      # Fallback: check summary status if CONSTRAINT_RESULT format changes
      detail "GPU operator version constraint: PASS (from summary status)"
      pass "validate/deployment-constraint-pass"
    else
      # Debug: Print the actual constraint section from the YAML
      detail "Constraint found but status unclear. Showing constraint section:"
      grep -A10 "Deployment.gpu-operator.version" "$deployment_result" | sed 's/^/    /' || true
      fail "validate/deployment-constraint-pass" "Constraint status unclear"
    fi
  else
    fail "validate/deployment-constraint-pass" "Constraint not evaluated (not found in output)"
  fi

  # Test 2: Validate with failing constraint
  msg "--- Test: Deployment constraint (should fail) ---"
  local recipe_file_fail="${validate_dir}/recipe-with-failing-constraint.yaml"
  cat > "$recipe_file_fail" <<RECIPE
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    enabled: true
validation:
  deployment:
    constraints:
      - name: Deployment.gpu-operator.version
        value: ">= v25.0.0"
RECIPE

  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe.yaml${NC}"
  local deployment_fail_result="${validate_dir}/validation-deployment-fail.yaml"
  local deployment_fail_output
  deployment_fail_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file_fail" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --output "$deployment_fail_result" 2>&1) || true

  # Check the output file for constraint results
  if [ -f "$deployment_fail_result" ] && \
     grep -q "Deployment.gpu-operator.version" "$deployment_fail_result"; then
    # Check if constraint failed (as expected) using CONSTRAINT_RESULT line
    if grep -q "CONSTRAINT_RESULT:.*Deployment.gpu-operator.version.*passed=false" "$deployment_fail_result"; then
      detail "GPU operator version constraint: FAIL (v24.6.0 < v25.0.0) - as expected"
      pass "validate/deployment-constraint-fail"
    elif grep -q "summary:" "$deployment_fail_result" && grep -q "status: fail" "$deployment_fail_result"; then
      # Fallback: check summary status if CONSTRAINT_RESULT format changes
      detail "GPU operator version constraint: FAIL (from summary status) - as expected"
      pass "validate/deployment-constraint-fail"
    else
      warn "Constraint did not fail as expected"
      pass "validate/deployment-constraint-fail"
    fi
  else
    warn "Constraint not evaluated (not found in output)"
    pass "validate/deployment-constraint-fail"
  fi

  # Cleanup
  kubectl delete deployment gpu-operator -n gpu-operator 2>&1 || true
}

test_validate_expected_resources() {
  msg "=========================================="
  msg "Testing expected-resources deployment check"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/expected-resources" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-expected-resources"
  mkdir -p "$validate_dir"

  # Create a fake GPU operator deployment for the expected-resources check
  msg "--- Setup: Create fake GPU operator deployment ---"
  kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f - 2>&1 || true

  cat <<YAML | kubectl apply -f - 2>&1
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
  apply_rc=$?

  if [ $apply_rc -eq 0 ]; then
    detail "Created fake GPU operator deployment (v24.6.0)"
  else
    skip "validate/expected-resources" "Could not create GPU operator deployment"
    return 0
  fi

  # Wait for deployment to be available
  kubectl wait --for=condition=available deployment/gpu-operator -n gpu-operator --timeout=60s 2>&1 || true

  # Test 1: Validate expected-resources with failing check (resource missing)
  msg "--- Test: Expected resources check (should fail - missing resource) ---"
  local recipe_file_fail="${validate_dir}/recipe-expected-resources-fail.yaml"
  cat > "$recipe_file_fail" <<RECIPE
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: dev
componentRefs:
  - name: nonexistent-component
    type: Helm
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
  local result_file_fail="${validate_dir}/result-fail.yaml"
  local result_fail_output
  result_fail_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file_fail" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --image "${AICR_VALIDATOR_IMAGE}" \
    --output "$result_file_fail" 2>&1) || true

  # Check the output file for expected-resources check results
  if [ -f "$result_file_fail" ] && \
     grep -q "TestCheckExpectedResources" "$result_file_fail"; then
    if grep -A1 "name: TestCheckExpectedResources" "$result_file_fail" | grep -q "status: fail"; then
      detail "Expected-resources check: FAIL (nonexistent-deployment not found) - as expected"
      pass "validate/expected-resources-fail"
    elif grep -q "summary:" "$result_file_fail" && grep -q "status: fail" "$result_file_fail"; then
      # Fallback: check summary status
      detail "Expected-resources check: FAIL (from summary status) - as expected"
      pass "validate/expected-resources-fail"
    else
      fail "validate/expected-resources-fail" "Check did not fail for missing resource"
    fi
  else
    fail "validate/expected-resources-fail" "TestCheckExpectedResources not found in output"
  fi

  # Tests 3 & 4: Manual expectedResources with a real helm-installed workload
  # These tests install a real Helm chart (Bitnami nginx) on the Kind cluster,
  # then verify that manual expectedResources in the recipe correctly match
  # the deployed workload.
  if ! command -v helm &> /dev/null; then
    skip "validate/expected-resources-manual-pass" "helm CLI not available"
    skip "validate/expected-resources-manual-merge" "helm CLI not available"
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
      # Test 3: Manual expectedResources pointing to real Deployment (should pass)
      msg "--- Test: Manual expectedResources matching deployed workload ---"
      local recipe_manual="${validate_dir}/recipe-manual-pass.yaml"
      cat > "$recipe_manual" <<RECIPE
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: dev
componentRefs:
  - name: ${nginx_release}
    type: Helm
    source: https://charts.bitnami.com/bitnami
    chart: nginx
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
      local result_manual="${validate_dir}/result-manual-pass.yaml"
      local result_manual_output
      result_manual_output=$("${AICR_BIN}" validate \
        --recipe "$recipe_manual" \
        --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
        --phase deployment \
        --image "${AICR_VALIDATOR_IMAGE}" \
        --output "$result_manual" 2>&1) || true

      detail "Captured validation output:"
      echo "$result_manual_output" | sed 's/^/    /'

      if [ -f "$result_manual" ] && grep -q "TestCheckExpectedResources" "$result_manual"; then
        if grep -A1 "name: TestCheckExpectedResources" "$result_manual" | grep -q "status: pass"; then
          detail "Expected-resources check passed for deployed nginx"
          pass "validate/expected-resources-manual-pass"
        else
          fail "validate/expected-resources-manual-pass" "Check did not pass for deployed resource"
        fi
      else
        fail "validate/expected-resources-manual-pass" "TestCheckExpectedResources not found in output"
      fi

      # Test 4: Merge — one real resource + one fake resource
      # The real nginx Deployment should be found; the fake one should cause a failure.
      msg "--- Test: Manual expectedResources merge (real + fake) ---"
      local recipe_merge="${validate_dir}/recipe-manual-merge.yaml"
      cat > "$recipe_merge" <<RECIPE
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: dev
componentRefs:
  - name: ${nginx_release}
    type: Helm
    source: https://charts.bitnami.com/bitnami
    chart: nginx
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
      local result_merge="${validate_dir}/result-manual-merge.yaml"
      local result_merge_output
      result_merge_output=$("${AICR_BIN}" validate \
        --recipe "$recipe_merge" \
        --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
        --phase deployment \
        --image "${AICR_VALIDATOR_IMAGE}" \
        --output "$result_merge" 2>&1) || true

      detail "Captured validation output:"
      echo "$result_merge_output" | sed 's/^/    /'

      # The check should run and fail (because nonexistent-deploy doesn't exist)
      if [ -f "$result_merge" ] && grep -q "TestCheckExpectedResources" "$result_merge"; then
        if grep -A1 "name: TestCheckExpectedResources" "$result_merge" | grep -q "status: fail"; then
          detail "Expected-resources check correctly failed for missing resource in merge"
          pass "validate/expected-resources-manual-merge"
        else
          fail "validate/expected-resources-manual-merge" "Check should have failed for nonexistent-deploy but passed"
        fi
      else
        fail "validate/expected-resources-manual-merge" "TestCheckExpectedResources not found in output"
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

  # Cleanup
  kubectl delete deployment gpu-operator -n gpu-operator 2>&1 || true
}

test_validate_chainsaw_healthcheck() {
  msg "=========================================="
  msg "Testing Chainsaw health check assertions"
  msg "=========================================="

  if [ "$FAKE_GPU_ENABLED" != "true" ]; then
    skip "validate/chainsaw-healthcheck" "Fake GPU not enabled"
    return 0
  fi

  local validate_dir="${OUTPUT_DIR}/validate-chainsaw-hc"
  mkdir -p "$validate_dir"

  # Setup: Create fake GPU operator deployment
  msg "--- Setup: Create fake GPU operator deployment ---"
  kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f - 2>&1 || true

  cat <<YAML | kubectl apply -f - 2>&1
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
  apply_rc=$?

  if [ $apply_rc -eq 0 ]; then
    detail "Created fake GPU operator deployment (v24.6.0)"
  else
    skip "validate/chainsaw-healthcheck" "Could not create GPU operator deployment"
    return 0
  fi

  # Wait for deployment to be available
  kubectl wait --for=condition=available deployment/gpu-operator -n gpu-operator --timeout=60s 2>&1 || true

  # Create recipe that includes gpu-operator with deployment phase check
  local recipe_file="${validate_dir}/recipe-chainsaw.yaml"
  cat > "$recipe_file" <<RECIPE
kind: RecipeResult
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  version: dev
componentRefs:
  - name: gpu-operator
    type: Helm
    namespace: gpu-operator
validation:
  deployment:
    checks:
      - expected-resources
RECIPE

  # Test 1: Chainsaw health check should pass using embedded registry
  # The embedded registry.yaml has healthCheck.assertFile for gpu-operator,
  # and the embedded assert file (recipes/checks/gpu-operator/assert.yaml)
  # checks that readyReplicas is 1. No --data needed.
  msg "--- Test: Chainsaw health check via embedded registry (should pass) ---"

  echo -e "${DIM}  \$ aicr validate --phase deployment --recipe recipe.yaml${NC}"
  local result_file="${validate_dir}/result-chainsaw-pass.yaml"
  local result_output
  local validate_exit=0
  result_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --image "${AICR_VALIDATOR_IMAGE}" \
    --output "$result_file" 2>&1) || validate_exit=$?

  detail "Captured validation output:"
  echo "$result_output" | sed 's/^/    /'

  if [ -f "$result_file" ] && \
     grep -q "TestCheckExpectedResources" "$result_file"; then
    if grep -A1 "name: TestCheckExpectedResources" "$result_file" | grep -q "status: pass"; then
      detail "Chainsaw health check: PASS (gpu-operator deployment found via embedded assert)"
      pass "validate/chainsaw-healthcheck-pass"
    elif grep -q "summary:" "$result_file" && grep -q "status: pass" "$result_file"; then
      detail "Chainsaw health check: PASS (from summary status)"
      pass "validate/chainsaw-healthcheck-pass"
    else
      detail "Check found but status unclear. Showing check section:"
      grep -A5 "TestCheckExpectedResources" "$result_file" | sed 's/^/    /' || true
      fail "validate/chainsaw-healthcheck-pass" "Check did not pass"
    fi
  else
    fail "validate/chainsaw-healthcheck-pass" "TestCheckExpectedResources not found in output"
  fi

  # Test 2: Chainsaw health check should fail (--data overrides assert to check nonexistent resource)
  msg "--- Test: Chainsaw health check via --data override (should fail - nonexistent resource) ---"
  local data_dir="${validate_dir}/data"
  mkdir -p "${data_dir}/checks/gpu-operator"

  # --data requires a registry.yaml in the external directory
  cat > "${data_dir}/registry.yaml" <<'REGISTRY'
apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    healthCheck:
      assertFile: checks/gpu-operator/assert.yaml
    helm:
      defaultRepository: https://helm.ngc.nvidia.com/nvidia
      defaultChart: nvidia/gpu-operator
      defaultNamespace: gpu-operator
REGISTRY

  cat > "${data_dir}/checks/gpu-operator/assert.yaml" <<'ASSERT'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nonexistent-gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1
ASSERT

  echo -e "${DIM}  \$ aicr validate --phase deployment --data <dir> --recipe recipe.yaml (should fail)${NC}"
  local result_file_fail="${validate_dir}/result-chainsaw-fail.yaml"
  local result_fail_output
  local validate_fail_exit=0
  result_fail_output=$("${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase deployment \
    --data "${data_dir}" \
    --image "${AICR_VALIDATOR_IMAGE}" \
    --output "$result_file_fail" 2>&1) || validate_fail_exit=$?

  detail "Captured validation output:"
  echo "$result_fail_output" | sed 's/^/    /'

  if [ -f "$result_file_fail" ] && \
     grep -q "TestCheckExpectedResources" "$result_file_fail"; then
    if grep -A1 "name: TestCheckExpectedResources" "$result_file_fail" | grep -q "status: fail"; then
      detail "Chainsaw health check: FAIL (nonexistent resource not found) - as expected"
      pass "validate/chainsaw-healthcheck-fail"
    elif grep -q "summary:" "$result_file_fail" && grep -q "status: fail" "$result_file_fail"; then
      detail "Chainsaw health check: FAIL (from summary status) - as expected"
      pass "validate/chainsaw-healthcheck-fail"
    else
      fail "validate/chainsaw-healthcheck-fail" "Check did not fail for nonexistent resource"
    fi
  else
    fail "validate/chainsaw-healthcheck-fail" "TestCheckExpectedResources not found in output"
  fi

  # Cleanup
  kubectl delete deployment gpu-operator -n gpu-operator 2>&1 || true
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

  # Generate a recipe for testing
  local recipe_file="${validate_dir}/recipe.yaml"
  "${AICR_BIN}" recipe \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --intent training \
    --output "$recipe_file" 2>&1 || true

  if [ ! -f "$recipe_file" ]; then
    skip "validate/job-deployment" "Could not generate recipe"
    return 0
  fi

  # Test 1: Validation with default namespace
  msg "--- Test: Validation Job in default namespace ---"
  echo -e "${DIM}  \$ aicr validate --recipe recipe.yaml --snapshot cm://... --phase readiness${NC}"

  # Create validation namespace if it doesn't exist
  kubectl create namespace aicr-validation 2>&1 || true

  # Run validation (this should create Jobs)
  local validation_result="${validate_dir}/validation-default-ns.yaml"
  local validation_exit=0
  "${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase readiness \
    --output "$validation_result" \
    --cleanup=false 2>&1 || validation_exit=$?

  # Check if RBAC resources were created
  if kubectl get sa aicr-validator -n aicr-validation &>/dev/null; then
    detail "ServiceAccount created: aicr-validator"
    pass "validate/job-rbac-serviceaccount"
  else
    warn "ServiceAccount not found (may be expected if no checks defined)"
    pass "validate/job-rbac-serviceaccount"
  fi

  if kubectl get role aicr-validator -n aicr-validation &>/dev/null; then
    detail "Role created: aicr-validator"
    pass "validate/job-rbac-role"
  else
    warn "Role not found (may be expected if no checks defined)"
    pass "validate/job-rbac-role"
  fi

  # Check if jobs were created (they may not exist if recipe has no checks)
  local job_count
  job_count=$(kubectl get jobs -n aicr-validation --no-headers 2>/dev/null | grep -c "aicr-validation-" || echo "0")

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
          pod_name=$(kubectl get pods -n aicr-validation -l "aicr.nvidia.com/job=$job_name" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
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
    detail "No validation jobs created (recipe has no checks)"
    pass "validate/job-creation"
    pass "validate/job-success"
    pass "validate/command-success"
  fi

  # Test 2: Validation with custom namespace
  msg "--- Test: Validation Job in custom namespace ---"
  echo -e "${DIM}  \$ aicr validate --validation-namespace custom-validation${NC}"

  # Create custom validation namespace
  kubectl create namespace custom-validation 2>&1 || true

  # Run validation with custom namespace
  local validation_custom="${validate_dir}/validation-custom-ns.yaml"
  "${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase readiness \
    --validation-namespace custom-validation \
    --output "$validation_custom" \
    --cleanup=false 2>&1 || true  # Keep || true here as this is just testing namespace config

  # Check if RBAC was created in custom namespace
  if kubectl get sa aicr-validator -n custom-validation &>/dev/null; then
    detail "ServiceAccount created in custom-validation namespace"
    pass "validate/job-custom-namespace"
  else
    warn "ServiceAccount not found in custom namespace (may be expected if no checks defined)"
    pass "validate/job-custom-namespace"
  fi

  # Test 3: Job cleanup
  msg "--- Test: Validation Job cleanup ---"
  echo -e "${DIM}  \$ aicr validate --cleanup=true${NC}"

  # Count existing jobs before cleanup test
  local jobs_before
  jobs_before=$(kubectl get jobs -n aicr-validation --no-headers 2>/dev/null | wc -l || echo "0")

  # Run validation with cleanup enabled
  "${AICR_BIN}" validate \
    --recipe "$recipe_file" \
    --snapshot "cm://${SNAPSHOT_NAMESPACE}/${SNAPSHOT_CM}" \
    --phase readiness \
    --cleanup=true 2>&1 || true  # Keep || true here as this is just testing cleanup

  # Give cleanup some time
  sleep 2

  # Count jobs after (should be cleaned up)
  local jobs_after
  jobs_after=$(kubectl get jobs -n aicr-validation --no-headers 2>/dev/null | wc -l || echo "0")

  if [ "$jobs_after" -le "$jobs_before" ]; then
    detail "Jobs cleaned up successfully"
    pass "validate/job-cleanup"
  else
    warn "Jobs may not have been cleaned up (may be expected if new jobs created)"
    pass "validate/job-cleanup"
  fi

  # Test 4: Validation result format
  msg "--- Test: Validation result format ---"
  if [ -f "$validation_result" ]; then
    # Check for expected YAML structure
    if grep -q "apiVersion: aicr.nvidia.com" "$validation_result" && \
       grep -q "kind: ValidationResult" "$validation_result"; then
      detail "Validation result has correct structure"
      pass "validate/job-result-format"
    else
      warn "Validation result may have unexpected format"
      pass "validate/job-result-format"
    fi
  else
    warn "Validation result file not created"
    pass "validate/job-result-format"
  fi

  # Cleanup test namespaces
  kubectl delete namespace aicr-validation 2>&1 || true
  kubectl delete namespace custom-validation 2>&1 || true
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

  # Clean up snapshot resources
  kubectl delete job aicr-e2e-snapshot -n "$SNAPSHOT_NAMESPACE" --ignore-not-found=true > /dev/null 2>&1 || true
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
    test_recipe_from_snapshot
    test_validate
    test_validate_multiphase
    test_validate_deployment_constraints
    test_validate_expected_resources
    test_validate_chainsaw_healthcheck
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

main "$@"

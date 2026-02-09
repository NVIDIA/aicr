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
# Local E2E Test Runner - Mimics CI Workflow
# =============================================================================
#
# This script replicates the exact CI E2E test workflow locally.
# It follows the same steps as .github/actions/e2e/action.yml
#
# Usage:
#   ./scripts/run-e2e-local.sh [--skip-cleanup] [--collect-artifacts]
#
# Options:
#   --skip-cleanup       Don't delete cluster after tests
#   --collect-artifacts  Collect debug artifacts even on success
#   --help              Show this help message
#
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# Configuration
SKIP_CLEANUP=false
COLLECT_ARTIFACTS=false
ARTIFACTS_DIR="${ROOT_DIR}/e2e-artifacts"

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --skip-cleanup)
      SKIP_CLEANUP=true
      shift
      ;;
    --collect-artifacts)
      COLLECT_ARTIFACTS=true
      shift
      ;;
    --help)
      grep "^#" "$0" | grep -v "#!/bin/bash" | sed 's/^# //' | sed 's/^#//'
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
  esac
done

# Helpers
msg() {
  echo -e "${BLUE}[INFO]${NC} $1"
}

warn() {
  echo -e "${YELLOW}[WARN]${NC} $1"
}

err() {
  echo -e "${RED}[ERROR]${NC} $1"
}

success() {
  echo -e "${GREEN}[SUCCESS]${NC} $1"
}

step() {
  echo ""
  echo -e "${CYAN}===================================================${NC}"
  echo -e "${CYAN}Step: $1${NC}"
  echo -e "${CYAN}===================================================${NC}"
}

check_tool() {
  if ! command -v "$1" &> /dev/null; then
    err "$1 is not installed"
    err "Run: make tools-setup"
    exit 1
  fi
}

# Cleanup function
cleanup() {
  local exit_code=$?

  if [ $exit_code -ne 0 ] || [ "$COLLECT_ARTIFACTS" = true ]; then
    step "Collecting debug artifacts"
    collect_artifacts
  fi

  if [ "$SKIP_CLEANUP" = false ]; then
    step "Cleanup"
    msg "Deleting cluster and cleaning up resources..."
    make cluster-delete || true
    docker system prune -f || true
    success "Cleanup complete"
  else
    warn "Skipping cleanup (--skip-cleanup flag set)"
    msg "Cluster remains running. Clean up with: make cluster-delete"
  fi

  if [ $exit_code -eq 0 ]; then
    success "E2E tests completed successfully!"
  else
    err "E2E tests failed with exit code: $exit_code"
    if [ -d "$ARTIFACTS_DIR" ]; then
      msg "Debug artifacts collected in: $ARTIFACTS_DIR"
    fi
  fi

  exit $exit_code
}

collect_artifacts() {
  mkdir -p "$ARTIFACTS_DIR"

  msg "Collecting Kubernetes resources..."
  kubectl get all --all-namespaces > "$ARTIFACTS_DIR/all-resources.txt" 2>&1 || true

  msg "Collecting events..."
  kubectl get events --all-namespaces --sort-by='.lastTimestamp' > "$ARTIFACTS_DIR/events.txt" 2>&1 || true

  msg "Collecting eidosd logs..."
  kubectl logs -n eidos -l app.kubernetes.io/name=eidosd --tail=500 > "$ARTIFACTS_DIR/eidosd-logs.txt" 2>&1 || true

  msg "Collecting docker images..."
  docker images > "$ARTIFACTS_DIR/docker-images.txt" 2>&1 || true

  msg "Exporting Kind logs..."
  mkdir -p "$ARTIFACTS_DIR/kind-logs"
  kind export logs "$ARTIFACTS_DIR/kind-logs" --name eidos 2>&1 || true

  success "Artifacts collected in: $ARTIFACTS_DIR"
}

# Trap errors and cleanup
trap cleanup EXIT

cd "$ROOT_DIR"

# =============================================================================
# CI Step 1: Check Prerequisites
# =============================================================================

step "Checking prerequisites"

check_tool go
check_tool docker
check_tool kubectl
check_tool kind
check_tool ctlptl
check_tool tilt
check_tool ko
check_tool curl

success "All required tools are installed"

# =============================================================================
# CI Step 2: Prep System for Kind Cluster
# =============================================================================

step "Preparing system for Kind cluster"

msg "Configuring network settings for Kind..."
# Try to configure sysctl settings (may require sudo password)
# These are optional - Kind will work without them, but they improve performance
if sudo -n true 2>/dev/null; then
  # Passwordless sudo available
  sudo sysctl -w net.ipv4.ip_forward=1 2>/dev/null || warn "Could not set net.ipv4.ip_forward (not critical)"
  sudo sysctl -w fs.inotify.max_user_watches=524288 2>/dev/null || warn "Could not set fs.inotify.max_user_watches (not critical)"
  sudo sysctl -w fs.inotify.max_user_instances=1024 2>/dev/null || warn "Could not set fs.inotify.max_user_instances (not critical)"
  success "System configured"
else
  warn "Skipping sysctl configuration (passwordless sudo not available)"
  warn "This is OK - Kind will work fine without these settings"
  msg "To enable these settings, run: sudo -v before running this script"
fi

# =============================================================================
# CI Step 3: Create Kind Cluster
# =============================================================================

step "Creating Kind cluster with local registry"

make cluster-create

success "Cluster created"

# =============================================================================
# CI Step 4: Run Tilt CI
# =============================================================================

step "Starting Tilt in CI mode"

msg "This will start Tilt without UI and wait for resources to be ready..."
make tilt-ci &
TILT_PID=$!

# Wait for Tilt to start
sleep 10

success "Tilt started (PID: $TILT_PID)"

# =============================================================================
# CI Step 5: Build and Push Eidos Image
# =============================================================================

step "Building and pushing snapshot agent image"

msg "Building eidos image for snapshot agent (Ko-built)..."
KO_DOCKER_REPO=localhost:5001/eidos ko build --bare --tags=local ./cmd/eidos

msg "Verifying image is available..."
if curl -sf http://localhost:5001/v2/eidos/tags/list | grep -q "local"; then
  success "Snapshot agent image available: localhost:5001/eidos:local"
else
  err "Failed to verify snapshot agent image"
  exit 1
fi

# =============================================================================
# CI Step 6: Build and Push Validator Image
# =============================================================================

step "Building and pushing validator image"

msg "Building validator image with Go toolchain..."
docker build -f Dockerfile.validator -t localhost:5001/eidos-validator:local .
docker push localhost:5001/eidos-validator:local

msg "Verifying image is available..."
if curl -sf http://localhost:5001/v2/eidos-validator/tags/list | grep -q "local"; then
  success "Validator image available: localhost:5001/eidos-validator:local"
else
  err "Failed to verify validator image"
  exit 1
fi

# =============================================================================
# CI Step 7: Set Up Fake GPU Environment
# =============================================================================

step "Setting up fake GPU environment"

msg "Creating gpu-operator namespace..."
kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f -

msg "Injecting fake nvidia-smi into Kind worker nodes..."
for node in $(docker ps --filter "name=-worker" --format "{{.Names}}"); do
  msg "  Processing node: $node"
  docker cp tools/fake-nvidia-smi "${node}:/usr/local/bin/nvidia-smi"
  docker exec "$node" chmod +x /usr/local/bin/nvidia-smi

  # Verify it works
  msg "  Verifying nvidia-smi..."
  docker exec "$node" nvidia-smi --version
done

success "Fake GPU environment configured"

# =============================================================================
# CI Step 8: Set Up Port Forwarding
# =============================================================================

step "Setting up port forwarding to eidosd"

msg "Waiting for eidosd service to be ready..."
kubectl wait --for=condition=available --timeout=120s deployment/eidosd -n eidos || true

msg "Starting port forward to eidosd..."
kubectl port-forward -n eidos svc/eidosd 8080:8080 &
PORT_FORWARD_PID=$!

sleep 5

msg "Checking eidosd health endpoint..."
if curl -sf http://localhost:8080/health > /dev/null 2>&1; then
  success "eidosd is healthy (PID: $PORT_FORWARD_PID)"
else
  err "eidosd health check failed"
  kubectl logs -n eidos -l app.kubernetes.io/name=eidosd --tail=50
  exit 1
fi

# =============================================================================
# CI Step 9: Run E2E Tests
# =============================================================================

step "Running E2E tests"

export EIDOS_IMAGE=localhost:5001/eidos:local
export EIDOS_VALIDATOR_IMAGE=localhost:5001/eidos-validator:local
export FAKE_GPU_ENABLED=true

msg "Environment:"
msg "  EIDOS_IMAGE=$EIDOS_IMAGE"
msg "  EIDOS_VALIDATOR_IMAGE=$EIDOS_VALIDATOR_IMAGE"
msg "  FAKE_GPU_ENABLED=$FAKE_GPU_ENABLED"
echo ""

./tests/e2e/run.sh

success "E2E tests passed!"

# Cleanup happens via trap

#!/usr/bin/env bash
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

# Create KWOK nodes by inferring configuration from recipe overlays
# Usage: ./apply-nodes.sh <recipe-name>
#
# Reads the recipe overlay to determine:
#   - criteria.service → cloud provider (eks, gke)
#   - criteria.accelerator → GPU type (h100, gb200)
#
# Fixed defaults: 2 system nodes, 4 GPU nodes

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KWOK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${KWOK_DIR}/.." && pwd)"
TEMPLATE="${KWOK_DIR}/templates/nodes/node.yaml.tmpl"
OVERLAYS_DIR="${REPO_ROOT}/pkg/recipe/data/overlays"

# Fixed defaults
SYSTEM_NODE_COUNT=2
GPU_NODE_COUNT=4
DEFAULT_K8S_VERSION="v1.33.5"
DEFAULT_REGION="us-east-1"
DEFAULT_ZONES=("us-east-1a" "us-east-1b")

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

check_deps() {
    for cmd in yq kubectl; do
        command -v "$cmd" &>/dev/null || { log_error "Missing: $cmd"; exit 1; }
    done
}

# Map service + accelerator to profile paths
get_profiles() {
    local service="$1"
    local accelerator="$2"

    # System profile: {service}/system-*.yaml
    local system_profile=""
    case "$service" in
        eks) system_profile="eks/system-m7i.yaml" ;;
        gke) system_profile="eks/system-m7i.yaml" ;;  # Fallback to EKS for now
        *)   system_profile="eks/system-m7i.yaml" ;;  # Default
    esac

    # GPU profile: based on accelerator
    local gpu_profile=""
    case "$service" in
        eks)
            case "$accelerator" in
                gb200) gpu_profile="eks/p6-gb200.yaml" ;;
                *)     gpu_profile="eks/p5-h100.yaml" ;;  # h100 or default
            esac
            ;;
        gke)
            # GKE uses same profiles for now
            case "$accelerator" in
                gb200) gpu_profile="eks/p6-gb200.yaml" ;;
                *)     gpu_profile="eks/p5-h100.yaml" ;;
            esac
            ;;
        *)
            gpu_profile="eks/p5-h100.yaml"  # Default to H100
            ;;
    esac

    echo "${system_profile}:${gpu_profile}"
}

# Generate node YAML from template
generate_node() {
    local node_name="$1" node_type="$2" instance_type="$3" region="$4" zone="$5"
    local k8s_version="$6" cpu="$7" memory="$8" storage="$9" arch="${10}" os_image="${11}"
    local gpu_product="${12:-}" gpu_count="${13:-}" gpu_memory="${14:-}"
    local gpu_driver="${15:-}" gpu_driver_major="${16:-}" gpu_driver_minor="${17:-}"

    local content gpu_labels="" gpu_annotations="" gpu_capacity="" gpu_allocatable="" extra_labels=""
    local max_pods="110"

    # System nodes get control-plane label for operator controllers
    if [[ "$node_type" == "system" ]]; then
        extra_labels="    node-role.kubernetes.io/control-plane: \"\""
    fi

    if [[ -n "$gpu_product" ]]; then
        max_pods="250"
        gpu_labels="    nvidia.com/gpu.present: \"true\"
    nvidia.com/gpu.product: ${gpu_product}
    nvidia.com/gpu.count: \"${gpu_count}\"
    nvidia.com/gpu.memory: \"${gpu_memory}\"
    nvidia.com/cuda.driver.major: \"${gpu_driver_major}\"
    nvidia.com/cuda.driver.minor: \"${gpu_driver_minor}\""
        gpu_annotations="    nvidia.com/gpu.driver.version: \"${gpu_driver}\""
        gpu_capacity="    nvidia.com/gpu: \"${gpu_count}\""
        gpu_allocatable="    nvidia.com/gpu: \"${gpu_count}\""
    fi

    content=$(cat "$TEMPLATE")
    content="${content//\$\{NODE_NAME\}/$node_name}"
    content="${content//\$\{NODE_TYPE\}/$node_type}"
    content="${content//\$\{INSTANCE_TYPE\}/$instance_type}"
    content="${content//\$\{REGION\}/$region}"
    content="${content//\$\{ZONE\}/$zone}"
    content="${content//\$\{K8S_VERSION\}/$k8s_version}"
    content="${content//\$\{CPU\}/$cpu}"
    content="${content//\$\{MEMORY\}/$memory}"
    content="${content//\$\{STORAGE\}/$storage}"
    content="${content//\$\{ARCH\}/$arch}"
    content="${content//\$\{OS_IMAGE\}/$os_image}"
    content="${content//\$\{MAX_PODS\}/$max_pods}"
    content="${content//\$\{EXTRA_LABELS\}/$extra_labels}"
    content="${content//\$\{GPU_LABELS\}/$gpu_labels}"
    content="${content//\$\{GPU_ANNOTATIONS\}/$gpu_annotations}"
    content="${content//\$\{GPU_CAPACITY\}/$gpu_capacity}"
    content="${content//\$\{GPU_ALLOCATABLE\}/$gpu_allocatable}"

    echo "$content"
}

create_nodes() {
    local recipe="$1"
    local overlay_file="${OVERLAYS_DIR}/${recipe}.yaml"

    [[ -f "$overlay_file" ]] || { log_error "Recipe overlay not found: $overlay_file"; exit 1; }

    # Extract criteria from overlay
    local service accelerator
    service=$(yq eval '.spec.criteria.service // "eks"' "$overlay_file")
    accelerator=$(yq eval '.spec.criteria.accelerator // "h100"' "$overlay_file")

    # Handle 'any' or empty values
    [[ "$service" == "any" || "$service" == "null" ]] && service="eks"
    [[ "$accelerator" == "any" || "$accelerator" == "null" ]] && accelerator="h100"

    log_info "Recipe: $recipe (service=$service, accelerator=$accelerator)"

    # Get profile paths
    local profiles system_profile gpu_profile
    profiles=$(get_profiles "$service" "$accelerator")
    system_profile="${profiles%%:*}"
    gpu_profile="${profiles##*:}"

    local sys_profile_path="${KWOK_DIR}/profiles/${system_profile}"
    local gpu_profile_path="${KWOK_DIR}/profiles/${gpu_profile}"

    [[ -f "$sys_profile_path" ]] || { log_error "System profile not found: $sys_profile_path"; exit 1; }
    [[ -f "$gpu_profile_path" ]] || { log_error "GPU profile not found: $gpu_profile_path"; exit 1; }

    log_info "Profiles: system=$system_profile, gpu=$gpu_profile"

    local temp_dir
    temp_dir=$(mktemp -d)
    trap "rm -rf \"$temp_dir\"" EXIT

    # System nodes
    local sys_instance sys_arch sys_os sys_cpu sys_mem sys_storage
    sys_instance=$(yq eval '.spec.instanceType' "$sys_profile_path")
    sys_arch=$(yq eval '.spec.arch' "$sys_profile_path")
    sys_os=$(yq eval '.spec.osImage' "$sys_profile_path")
    sys_cpu=$(yq eval '.spec.resources.cpu' "$sys_profile_path")
    sys_mem=$(yq eval '.spec.resources.memory' "$sys_profile_path")
    sys_storage=$(yq eval '.spec.resources.storage' "$sys_profile_path")

    log_info "Creating $SYSTEM_NODE_COUNT system nodes ($sys_instance)"
    for ((i = 0; i < SYSTEM_NODE_COUNT; i++)); do
        local zone node_name="system-${i}"
        zone="${DEFAULT_ZONES[$((i % ${#DEFAULT_ZONES[@]}))]}"
        generate_node "$node_name" "system" "$sys_instance" "$DEFAULT_REGION" "$zone" \
            "$DEFAULT_K8S_VERSION" "$sys_cpu" "$sys_mem" "$sys_storage" "$sys_arch" "$sys_os" \
            > "${temp_dir}/${node_name}.yaml"
        log_info "  $node_name ($zone)"
    done

    # GPU nodes
    local gpu_instance gpu_arch gpu_os gpu_cpu gpu_mem gpu_storage
    local gpu_product gpu_count gpu_memory gpu_driver gpu_major gpu_minor
    gpu_instance=$(yq eval '.spec.instanceType' "$gpu_profile_path")
    gpu_arch=$(yq eval '.spec.arch' "$gpu_profile_path")
    gpu_os=$(yq eval '.spec.osImage' "$gpu_profile_path")
    gpu_cpu=$(yq eval '.spec.resources.cpu' "$gpu_profile_path")
    gpu_mem=$(yq eval '.spec.resources.memory' "$gpu_profile_path")
    gpu_storage=$(yq eval '.spec.resources.storage' "$gpu_profile_path")
    gpu_product=$(yq eval '.spec.gpu.product' "$gpu_profile_path")
    gpu_count=$(yq eval '.spec.gpu.count' "$gpu_profile_path")
    gpu_memory=$(yq eval '.spec.gpu.memory' "$gpu_profile_path")
    gpu_driver=$(yq eval '.spec.gpu.driver' "$gpu_profile_path")
    gpu_major=$(yq eval '.spec.gpu.driverMajor' "$gpu_profile_path")
    gpu_minor=$(yq eval '.spec.gpu.driverMinor' "$gpu_profile_path")

    log_info "Creating $GPU_NODE_COUNT GPU nodes ($gpu_instance, ${gpu_count}x ${gpu_product})"
    for ((i = 0; i < GPU_NODE_COUNT; i++)); do
        local zone node_name="gpu-${i}"
        zone="${DEFAULT_ZONES[$((i % ${#DEFAULT_ZONES[@]}))]}"
        generate_node "$node_name" "accelerated" "$gpu_instance" "$DEFAULT_REGION" "$zone" \
            "$DEFAULT_K8S_VERSION" "$gpu_cpu" "$gpu_mem" "$gpu_storage" "$gpu_arch" "$gpu_os" \
            "$gpu_product" "$gpu_count" "$gpu_memory" "$gpu_driver" "$gpu_major" "$gpu_minor" \
            > "${temp_dir}/${node_name}.yaml"
        log_info "  $node_name ($zone, ${gpu_count}x GPU)"
    done

    log_info "Applying nodes to cluster..."
    kubectl apply -f "${temp_dir}/"
    kubectl wait --for=condition=Ready nodes -l type=kwok --timeout=60s

    local total_gpus=$((GPU_NODE_COUNT * gpu_count))
    log_info "Created $((SYSTEM_NODE_COUNT + GPU_NODE_COUNT)) KWOK nodes ($total_gpus GPUs)"
}

main() {
    [[ $# -lt 1 ]] && { echo "Usage: $0 <recipe-name>"; exit 1; }
    check_deps
    create_nodes "$1"
}

main "$@"

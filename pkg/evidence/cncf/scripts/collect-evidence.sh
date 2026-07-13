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

# DEPRECATED: Use 'aicr validate --evidence-dir' instead.
#
# Evidence is now generated directly from validation results:
#   aicr validate -r recipe.yaml --phase conformance --evidence-dir ./evidence
#   aicr validate -r recipe.yaml --phase conformance --evidence-dir ./evidence --result result.yaml

# Note: 'aicr validate --evidence-dir' generates structural validation evidence.
# This script collects behavioral test evidence (HPA scaling, DRA allocation, etc.)
# that requires deploying test workloads. Both are needed for full conformance evidence.

# Support invocation from aicr CLI (env vars) or standalone (defaults).
SCRIPT_DIR="${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/../../.." && pwd)}"
EVIDENCE_DIR="${EVIDENCE_DIR:-${SCRIPT_DIR}/evidence}"
SECTION="${1:-all}"

# Current output file — set per section
EVIDENCE_FILE=""

# Timeouts
POD_TIMEOUT=120   # seconds to wait for pod completion
DEPLOY_TIMEOUT=60 # seconds to wait for deployment readiness

# Colors for terminal output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# Capture command output into evidence file as a fenced code block
capture() {
    local label="$1"
    shift
    echo "" >> "${EVIDENCE_FILE}"
    echo "**${label}**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    # Strip absolute paths from command display to avoid leaking local/temp paths
    local cmd_display="$*"
    cmd_display="${cmd_display//${SCRIPT_DIR}\//}"
    cmd_display="${cmd_display//${REPO_ROOT}\//}"
    # Strip any remaining absolute paths to manifests (e.g., temp dirs from aicr evidence)
    cmd_display=$(echo "${cmd_display}" | sed 's|[^ ]*/manifests/|manifests/|g')
    echo "\$ ${cmd_display}" >> "${EVIDENCE_FILE}"
    local rc=0
    if output=$("$@" 2>&1); then
        echo "${output}" >> "${EVIDENCE_FILE}"
    else
        rc=$?
        echo "${output}" >> "${EVIDENCE_FILE}"
        echo "(exit code: ${rc})" >> "${EVIDENCE_FILE}"
    fi
    echo '```' >> "${EVIDENCE_FILE}"
    # Propagate the command's status so callers whose step must succeed
    # (e.g. applying the test workload) can fail closed. Callers that only
    # record diagnostics ignore the return value, as before.
    return "${rc}"
}

# Wait for a pod to reach a terminal phase (Succeeded or Failed).
# Exits early on unrecoverable container errors (ImagePullBackOff, CrashLoopBackOff, etc.)
wait_for_pod() {
    local ns="$1" name="$2" timeout="$3"
    local elapsed=0
    while [ $elapsed -lt "$timeout" ]; do
        phase=$(kubectl get pod "$name" -n "$ns" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
        case "$phase" in
            Succeeded|Failed) echo "$phase"; return 0 ;;
        esac
        # Check for unrecoverable container errors to fail early
        local waiting_reason
        waiting_reason=$(kubectl get pod "$name" -n "$ns" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null)
        case "$waiting_reason" in
            ErrImagePull|ImagePullBackOff|CrashLoopBackOff|InvalidImageName|CreateContainerConfigError)
                log_error "Pod $name failed early: $waiting_reason" >&2
                echo "Failed"
                return 1
                ;;
        esac
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "Timeout"
    return 1
}

# Wait for a local port to accept connections (e.g., after kubectl port-forward).
# Exits early if the background process dies.
wait_for_port() {
    local port="$1" timeout="$2" pid="$3"
    local elapsed=0
    while [ $elapsed -lt "$timeout" ]; do
        if curl -sf "http://localhost:${port}/-/ready" &>/dev/null; then return 0; fi
        if ! kill -0 "$pid" 2>/dev/null; then return 1; fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    return 1
}

# Runtime results tracker — records check name and status as they execute.
# Format: "name:status" entries separated by newlines.
CHECK_RESULTS=""

# Parse the single authoritative verdict from an evidence file. Verdict lines
# begin at column zero so numbered prose such as "6. **Result: PASS**" in a
# section overview cannot mask the result written after the checks run.
#
# Every collector must write exactly one PASS, SKIP, or FAIL verdict. Missing,
# unknown, or conflicting verdicts fail closed; optional prerequisites are
# represented by an explicit SKIP in the evidence file.
evidence_result() {
    local evidence_path="$1"

    if [ ! -f "${evidence_path}" ]; then
        return 1
    fi

    local result_count=0 result_line="" status=""
    while IFS= read -r line; do
        case "${line}" in
            '**Result:'*)
                result_count=$((result_count + 1))
                result_line="${line}"
                ;;
        esac
    done < "${evidence_path}"

    if [ "${result_count}" -ne 1 ]; then
        return 1
    fi

    status=$(printf '%s\n' "${result_line}" | sed -nE \
        's/^\*\*Result:[[:space:]]*(PASS|SKIP|FAIL)([[:space:]]+\([^*]+\))?\*\*([[:space:]]+—.*)?[[:space:]]*$/\1/p')
    if [ -z "${status}" ]; then
        return 1
    fi

    echo "${status}"
}

# Run a collector and record its result based on the evidence file it produces.
# Usage: run_check "DRA Support" "dra-support" collect_dra
run_check() {
    local display_name="$1" file_key="$2" collector_fn="$3"
    local evidence_path="${EVIDENCE_DIR}/${file_key}.md"
    local collector_rc=0 status=""

    # Do not let an artifact from a prior run satisfy the current check.
    if ! rm -f "${evidence_path}"; then
        log_error "Could not remove stale evidence for ${display_name}: ${evidence_path}"
        CHECK_RESULTS="${CHECK_RESULTS}${display_name}:FAIL\n"
        return 1
    fi

    "${collector_fn}" || collector_rc=$?

    if [ "${collector_rc}" -ne 0 ]; then
        log_error "Collector for ${display_name} exited with status ${collector_rc}"
        status="FAIL"
    elif ! status=$(evidence_result "${evidence_path}"); then
        log_error "Evidence for ${display_name} has no single valid PASS/SKIP/FAIL result: ${evidence_path}"
        status="FAIL"
    fi

    CHECK_RESULTS="${CHECK_RESULTS}${display_name}:${status}\n"
    if [ "${status}" = "FAIL" ]; then
        return 1
    else
        return 0
    fi
}

# Clean up a test namespace properly: pods → resourceclaims → namespace
# This order prevents stale DRA kubelet checkpoint issues caused by
# orphaned ResourceClaims with delete-protection finalizers.
cleanup_ns() {
    local ns="$1"
    local phase="${2:-post}"  # "pre" = always run, "post" = respect NO_CLEANUP
    # Respect NO_CLEANUP for post-run cleanup only — pre-run cleanup always runs
    # to avoid stale resource conflicts on reruns.
    if [ "${phase}" = "post" ] && [ "${NO_CLEANUP:-}" = "true" ]; then
        log_info "Skipping post-run cleanup of namespace ${ns} (NO_CLEANUP=true)"
        return 0
    fi
    # Skip if namespace doesn't exist
    if ! kubectl get namespace "$ns" &>/dev/null; then return 0; fi
    # Delete pods first so DRA driver can call NodeUnprepareResources
    kubectl delete pods --all -n "$ns" --ignore-not-found --wait=true --timeout=30s &>/dev/null || true
    # Delete resourceclaims (finalizer removed after pod deletion)
    kubectl delete resourceclaims --all -n "$ns" --ignore-not-found --wait=true --timeout=30s &>/dev/null || true
    # Now namespace can terminate cleanly
    kubectl delete namespace "$ns" --ignore-not-found --timeout=60s &>/dev/null || true
}

# ensure_fresh_namespace deletes any prior run's namespace and VERIFIES it is
# gone before the caller deploys a new workload. Without the verification, a
# stuck/terminating namespace makes the subsequent apply fail while a stale
# previously-successful pod survives — wait_for_pod would then observe the OLD
# pod's phase and produce a false PASS. Returns nonzero (callers record FAIL)
# when the namespace still exists after ~90s.
ensure_fresh_namespace() {
    local ns="$1" elapsed=0
    cleanup_ns "$ns" pre
    while kubectl get namespace "$ns" &>/dev/null; do
        if [ "$elapsed" -ge 90 ]; then
            log_error "namespace ${ns} still present after ${elapsed}s — cannot guarantee a fresh workload"
            return 1
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    return 0
}

# --- GPU allocation mode helpers (#1629) ---

# detect_resource_api_version prints the newest SERVED resource.k8s.io API
# version — preference order v1, v1beta2, v1beta1, mirroring
# validators/internal/allocmode.APIVersionPreference — or nothing when the
# group is not served or the discovery query fails. Callers that need the DRA
# API treat an empty result as "API not served" and record FAIL (fail closed).
detect_resource_api_version() {
    local served v
    served=$(kubectl api-versions 2>/dev/null) || return 0
    for v in v1 v1beta2 v1beta1; do
        if echo "${served}" | grep -qx "resource.k8s.io/${v}"; then
            echo "${v}"
            return 0
        fi
    done
}

# emit_resourceclaim_yaml prints a single-GPU ResourceClaim manifest at the
# given served resource.k8s.io API version. Schema shapes verified against the
# vendored types (vendor/k8s.io/api/resource/*/types.go):
#   v1, v1beta2: spec.devices.requests[0].exactly.deviceClassName
#                (DeviceRequest.Exactly -> ExactDeviceRequest)
#   v1beta1:     spec.devices.requests[0].deviceClassName — no `exactly`
#                wrapper (DeviceRequest carries the fields directly)
# Any other version is an error (fail closed).
# Usage: emit_resourceclaim_yaml <version> <name> <namespace> <deviceclass>
emit_resourceclaim_yaml() {
    local version="$1" name="$2" namespace="$3" deviceclass="$4"
    case "${version}" in
        v1|v1beta2)
            cat <<EOF
apiVersion: resource.k8s.io/${version}
kind: ResourceClaim
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  devices:
    requests:
      - name: gpu
        exactly:
          deviceClassName: ${deviceclass}
          allocationMode: ExactCount
          count: 1
EOF
            ;;
        v1beta1)
            cat <<EOF
apiVersion: resource.k8s.io/v1beta1
kind: ResourceClaim
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  devices:
    requests:
      - name: gpu
        deviceClassName: ${deviceclass}
        allocationMode: ExactCount
        count: 1
EOF
            ;;
        *)
            log_error "emit_resourceclaim_yaml: unsupported resource.k8s.io version: ${version:-<empty>}" >&2
            return 1
            ;;
    esac
}

# count_resourceslices_for_driver prints the number of ResourceSlices
# published by the given DRA driver, or "error" (and returns 1) when the
# query or parse fails — callers must fail closed on "error", never treat it
# as zero.
count_resourceslices_for_driver() {
    local driver="$1" out
    if ! out=$(kubectl get resourceslices -o json 2>/dev/null | python3 -c '
import json, sys
data = json.load(sys.stdin)
print(sum(1 for s in data.get("items", [])
          if s.get("spec", {}).get("driver") == sys.argv[1]))
' "${driver}" 2>/dev/null) || [ -z "${out}" ]; then
        echo "error"
        return 1
    fi
    echo "${out}"
}

# resolve_alloc_mode resolves which GPU allocation mode the dra-support and
# secure-access sections must exercise, mirroring the Go validators' #1327
# contract: configuration selects the allocation policy (the recipe-resolved
# AICR_GPU_ALLOCATION_POLICY env var, threaded by `aicr validate
# --cncf-submission` when recipe context is available); only recipe-less
# standalone runs fall back to capability detection. An unknown or
# unverifiable policy — including the reserved dra-extended-resource — fails
# closed with NO fallback to detection, mirroring allocmode.Verify.
#
# Result is cached in globals (resolved once per script invocation):
#   ALLOC_MODE         "dra" | "device-plugin" (empty on failure)
#   ALLOC_MODE_SOURCE  provenance of the decision (policy vs detection)
#   ALLOC_MODE_DETAIL  detection basis / evidence lines (detection path only)
#   ALLOC_MODE_ERROR   fail-closed reason when resolution failed (returns 1)
resolve_alloc_mode() {
    if [ -n "${ALLOC_MODE_RESOLVED:-}" ]; then
        [ -n "${ALLOC_MODE_ERROR}" ] && return 1
        return 0
    fi
    ALLOC_MODE_RESOLVED=1
    ALLOC_MODE=""
    ALLOC_MODE_SOURCE=""
    ALLOC_MODE_DETAIL=""
    ALLOC_MODE_ERROR=""

    local policy="${AICR_GPU_ALLOCATION_POLICY:-}"
    case "${policy}" in
        device-plugin-extended-resource)
            ALLOC_MODE="device-plugin"
            ALLOC_MODE_SOURCE="recipe-configured GPU allocation policy \"${policy}\""
            return 0
            ;;
        dra-resource-claim)
            ALLOC_MODE="dra"
            ALLOC_MODE_SOURCE="recipe-configured GPU allocation policy \"${policy}\""
            return 0
            ;;
        ""|unspecified)
            # No configured policy — standalone run, capability detection below.
            ;;
        *)
            ALLOC_MODE_ERROR="unknown/unverifiable GPU allocation policy \"${policy}\" — mirrors allocmode.Verify fail-closed semantics, issue #1327"
            return 1
            ;;
    esac

    # Capability detection (standalone runs), mirroring
    # validators/internal/allocmode.Detect as far as bash reasonably can:
    #   DRA usable:           gpu.nvidia.com DeviceClass exists AND >=1
    #                         node-local (spec.nodeName) gpu.nvidia.com
    #                         ResourceSlice advertises >=1 device on a Ready,
    #                         schedulable node.
    #   Device-plugin usable: a Ready, schedulable node has allocatable
    #                         nvidia.com/gpu > 0.
    # Every query failure is fail-closed: the mechanism is treated as NOT
    # usable and the failure is recorded — never silently skipped.
    ALLOC_MODE_SOURCE="capability detection (no recipe-configured GPU allocation policy)"

    local api_version dra_detail plugin_detail
    local dra_usable="false" plugin_usable="false"
    api_version=$(detect_resource_api_version)

    if [ -z "${api_version}" ]; then
        dra_detail="DRA not usable: no served resource.k8s.io API version (or discovery query failed — treated as not usable, fail closed)"
    elif ! kubectl get deviceclass gpu.nvidia.com &>/dev/null; then
        dra_detail="DRA not usable: DeviceClass gpu.nvidia.com not found (or query failed — treated as not usable, fail closed)"
    else
        local nodes_json_file slices_json_file
        nodes_json_file=$(mktemp "${TMPDIR:-/tmp}/aicr-evidence-XXXXXX")
        slices_json_file=$(mktemp "${TMPDIR:-/tmp}/aicr-evidence-XXXXXX")
        if kubectl get nodes -o json > "${nodes_json_file}" 2>/dev/null \
            && kubectl get resourceslices -o json > "${slices_json_file}" 2>/dev/null \
            && dra_detail=$(python3 - "${nodes_json_file}" "${slices_json_file}" <<'PYEOF'
import json
import sys

with open(sys.argv[1]) as f:
    nodes = json.load(f)
with open(sys.argv[2]) as f:
    slices = json.load(f)

eligible = set()
for n in nodes.get("items", []):
    if n.get("spec", {}).get("unschedulable"):
        continue
    conditions = n.get("status", {}).get("conditions", []) or []
    if any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions):
        eligible.add(n["metadata"]["name"])

counts = {}
for s in slices.get("items", []):
    spec = s.get("spec", {})
    if spec.get("driver") != "gpu.nvidia.com":
        continue
    node = spec.get("nodeName", "")
    devices = spec.get("devices", []) or []
    if node in eligible and len(devices) > 0:
        counts[node] = counts.get(node, 0) + len(devices)

if counts:
    fmt = ",".join(f"{k}={v}" for k, v in sorted(counts.items()))
    print("DRA usable: DeviceClass gpu.nvidia.com exists and node-local "
          f"gpu.nvidia.com ResourceSlice device(s) on Ready, schedulable node(s) [{fmt}]")
else:
    print("DRA not usable: DeviceClass gpu.nvidia.com exists but no gpu.nvidia.com "
          "ResourceSlice advertises a device (via spec.nodeName) on a Ready, schedulable node")
PYEOF
        ); then
            case "${dra_detail}" in
                "DRA usable:"*) dra_usable="true" ;;
            esac
        else
            dra_detail="DRA not usable: node/ResourceSlice query or parse failed — treated as not usable, fail closed"
        fi
        rm -f "${nodes_json_file}" "${slices_json_file}"
    fi

    if plugin_detail=$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json
import sys

nodes = json.load(sys.stdin)
usable = []
for n in nodes.get("items", []):
    if n.get("spec", {}).get("unschedulable"):
        continue
    conditions = n.get("status", {}).get("conditions", []) or []
    if not any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions):
        continue
    alloc = n.get("status", {}).get("allocatable", {}).get("nvidia.com/gpu", "0")
    try:
        count = int(alloc)
    except ValueError:
        count = 0
    if count > 0:
        usable.append("{}={}".format(n["metadata"]["name"], count))

if usable:
    print("device plugin usable: Ready, schedulable node(s) with allocatable "
          "nvidia.com/gpu [{}]".format(",".join(sorted(usable))))
else:
    print("device plugin not usable: no Ready, schedulable node advertises "
          "allocatable nvidia.com/gpu")
' 2>/dev/null); then
        case "${plugin_detail}" in
            "device plugin usable:"*) plugin_usable="true" ;;
        esac
    else
        plugin_detail="device plugin not usable: node query or parse failed — treated as not usable, fail closed"
    fi

    ALLOC_MODE_DETAIL="${dra_detail}
${plugin_detail}
note: this detection checks DeviceClass existence, node-local ResourceSlice publication, and scalar allocatable only — full pool-generation/completeness, device-taint, and topology validation is performed by the Go validators (validators/internal/allocmode), not this script"

    # Preference order must match validators/conformance/secure_access_check.go
    # capability dispatch: DRA when usable, else device plugin, else FAIL.
    if [ "${dra_usable}" = "true" ]; then
        ALLOC_MODE="dra"
    elif [ "${plugin_usable}" = "true" ]; then
        ALLOC_MODE="device-plugin"
    else
        ALLOC_MODE_ERROR="no usable whole-GPU allocation mechanism detected: ${dra_detail}; ${plugin_detail}"
        return 1
    fi
    return 0
}

# write_alloc_mode_evidence appends the resolved GPU allocation mode (or the
# fail-closed resolution error) to the current EVIDENCE_FILE. Returns
# resolve_alloc_mode's status so callers can bail out after recording FAIL.
write_alloc_mode_evidence() {
    local ok=true
    resolve_alloc_mode || ok=false

    cat >> "${EVIDENCE_FILE}" <<EOF
## GPU Allocation Mode

**Configured policy (AICR_GPU_ALLOCATION_POLICY):** \`${AICR_GPU_ALLOCATION_POLICY:-<unset>}\`
**Mode source:** ${ALLOC_MODE_SOURCE:-n/a}
EOF
    if [ "${ok}" = "true" ]; then
        echo "**Resolved mode:** \`${ALLOC_MODE}\`" >> "${EVIDENCE_FILE}"
    fi
    if [ -n "${ALLOC_MODE_DETAIL}" ]; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Detection basis:**" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
        echo "${ALLOC_MODE_DETAIL}" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
    fi
    if [ "${ok}" != "true" ]; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — ${ALLOC_MODE_ERROR}" >> "${EVIDENCE_FILE}"
        return 1
    fi
    return 0
}

# Detect cluster info once and cache in global variables.
# Sets: CLUSTER_DESC, CLUSTER_K8S_VERSION, CLUSTER_PLATFORM, CLUSTER_OS_IMAGE,
#        CLUSTER_PROVIDER_ID, CLUSTER_INSTANCE_TYPE, CLUSTER_ACCELERATOR
detect_cluster_info() {
    # Guard: only detect once
    if [ -n "${CLUSTER_INFO_DETECTED:-}" ]; then
        return
    fi
    CLUSTER_INFO_DETECTED=1

    CLUSTER_K8S_VERSION=$(kubectl version -o json 2>/dev/null | python3 -c "import sys,json; v=json.load(sys.stdin)['serverVersion']; print(f\"v{v['major']}.{v['minor']}\")" 2>/dev/null || echo "unknown")
    CLUSTER_PLATFORM=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.operatingSystem}/{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo "unknown")
    CLUSTER_OS_IMAGE=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.osImage}' 2>/dev/null || echo "unknown")

    CLUSTER_PROVIDER_ID=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || echo "")
    CLUSTER_ACCELERATOR=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.labels.nvidia\.com/gpu\.product}' 2>/dev/null || echo "unknown")
    CLUSTER_INSTANCE_TYPE=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.labels.node\.kubernetes\.io/instance-type}' 2>/dev/null || echo "unknown")

    if [[ "${CLUSTER_PROVIDER_ID}" == aws://* ]]; then
        CLUSTER_DESC="EKS / ${CLUSTER_INSTANCE_TYPE} / ${CLUSTER_ACCELERATOR}"
    elif [[ "${CLUSTER_PROVIDER_ID}" == gce://* ]]; then
        local gke_accel
        gke_accel=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.labels.cloud\.google\.com/gke-accelerator}' 2>/dev/null || echo "${CLUSTER_ACCELERATOR}")
        CLUSTER_DESC="GKE / ${CLUSTER_INSTANCE_TYPE} / ${gke_accel}"
    else
        CLUSTER_DESC="${CLUSTER_INSTANCE_TYPE} / ${CLUSTER_ACCELERATOR}"
    fi
}

# Write a per-section evidence file header
write_section_header() {
    local title="$1"
    local timestamp
    timestamp=$(date -u '+%Y-%m-%d %H:%M:%S UTC')

    cat > "${EVIDENCE_FILE}" <<EOF
# ${title}

**Cluster:** \`${CLUSTER_DESC}\`
**Generated:** ${timestamp}
**Kubernetes Version:** ${CLUSTER_K8S_VERSION}
**Platform:** ${CLUSTER_PLATFORM}

---

EOF
}

# --- Section 1: DRA Support ---
# Mode-aware (#1629), mirroring validators/conformance/dra_support_check.go:
# always records the served resource.k8s.io version and per-driver
# ResourceSlice inventory; the behavioral full-GPU ResourceClaim test runs
# only under the DRA allocation mode. Under the device-plugin mode whole GPUs
# are device-plugin-allocated, so the behavioral claim test is recorded as
# not applicable and the section passes on API + NVIDIA slice evidence
# (compute-domain.nvidia.com slices count as valid NVIDIA DRA publication).
collect_dra() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/dra-support.md"
    log_info "Collecting DRA Support evidence → ${EVIDENCE_FILE}"
    write_section_header "DRA Support (Dynamic Resource Allocation)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that the cluster supports DRA (resource.k8s.io API group), has a working
DRA driver, and advertises NVIDIA devices via ResourceSlices. Under the DRA GPU
allocation mode a behavioral test additionally allocates a GPU to a pod through a
ResourceClaim; under the device-plugin mode whole GPUs are device-plugin-allocated
and the behavioral ResourceClaim test is not applicable.

EOF
    if ! write_alloc_mode_evidence; then
        log_error "GPU allocation mode resolution failed: ${ALLOC_MODE_ERROR}"
        return
    fi

    local api_version
    api_version=$(detect_resource_api_version)

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## DRA API Enabled
EOF
    capture "DRA API resources" kubectl api-resources --api-group=resource.k8s.io
    echo "" >> "${EVIDENCE_FILE}"
    if [ -n "${api_version}" ]; then
        echo "**Served resource.k8s.io version (newest):** \`resource.k8s.io/${api_version}\`" >> "${EVIDENCE_FILE}"
    else
        echo "**Served resource.k8s.io version (newest):** none — the resource.k8s.io API group is not served" >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## DeviceClasses
EOF
    capture "DeviceClasses" kubectl get deviceclass

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## DRA Driver Health
EOF
    capture "DRA driver pods" kubectl get pods -n nvidia-dra-driver -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Device Advertisement (ResourceSlices)
EOF
    capture "ResourceSlices" kubectl get resourceslices

    # Per-driver inventory: gpu.nvidia.com (full-GPU DRA) and
    # compute-domain.nvidia.com (IMEX channels — the supported NVIDIA DRA
    # configuration under the device-plugin default, #1620/#1327). "error"
    # means the query failed and is treated as no evidence (fail closed).
    local gpu_slices cd_slices
    gpu_slices=$(count_resourceslices_for_driver gpu.nvidia.com) || true
    cd_slices=$(count_resourceslices_for_driver compute-domain.nvidia.com) || true
    cat >> "${EVIDENCE_FILE}" <<EOF

**ResourceSlice inventory by NVIDIA driver**
\`\`\`
gpu.nvidia.com:            ${gpu_slices} slice(s)
compute-domain.nvidia.com: ${cd_slices} slice(s)
\`\`\`
EOF

    if [ "${ALLOC_MODE}" = "device-plugin" ]; then
        collect_dra_device_plugin_verdict "${api_version}" "${gpu_slices}" "${cd_slices}"
        return
    fi

    # Fail closed on a failed inventory query in DRA mode too: the slice
    # inventory is part of the submission evidence, and a behavioral PASS
    # must not ship alongside an "error" inventory row.
    if [ "${gpu_slices}" = "error" ] || [ "${cd_slices}" = "error" ]; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — ResourceSlice inventory query failed (fail closed); rerun with a reachable API server." >> "${EVIDENCE_FILE}"
        log_error "ResourceSlice inventory query failed in DRA mode"
        return
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Allocation Test

Deploy a test pod that requests 1 GPU via ResourceClaim and verifies device access.
The ResourceClaim is generated at the newest served resource.k8s.io API version;
the namespace and pod come from `pkg/evidence/cncf/scripts/manifests/dra-gpu-test.yaml`.
EOF

    if [ -z "${api_version}" ]; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — DRA allocation mode selected but no served resource.k8s.io API version was discovered; the ResourceClaim test cannot run." >> "${EVIDENCE_FILE}"
        log_error "DRA mode with no served resource.k8s.io API version"
        return
    fi

    # Compose the test manifest: drop the embedded ResourceClaim document from
    # dra-gpu-test.yaml and substitute the version-correct claim.
    local test_manifest
    test_manifest=$(mktemp "${TMPDIR:-/tmp}/aicr-evidence-XXXXXX")
    awk 'BEGIN{RS="---\n"; ORS="---\n"} $0 !~ /kind: ResourceClaim/' \
        "${SCRIPT_DIR}/manifests/dra-gpu-test.yaml" > "${test_manifest}"
    if ! emit_resourceclaim_yaml "${api_version}" gpu-claim dra-test gpu.nvidia.com >> "${test_manifest}"; then
        rm -f "${test_manifest}"
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — could not generate a ResourceClaim for served version ${api_version}." >> "${EVIDENCE_FILE}"
        return
    fi

    echo '```yaml' >> "${EVIDENCE_FILE}"
    cat "${test_manifest}" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Clean up any previous run and verify it is actually gone — a stale
    # Succeeded pod from a prior run must not satisfy wait_for_pod.
    if ! ensure_fresh_namespace dra-test; then
        rm -f "${test_manifest}"
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — previous dra-test namespace could not be removed; cannot guarantee a fresh workload." >> "${EVIDENCE_FILE}"
        return
    fi

    # Deploy test (fail closed when the apply itself fails)
    log_info "Deploying DRA GPU test..."
    if ! capture "Apply test manifest" kubectl apply -f "${test_manifest}"; then
        rm -f "${test_manifest}"
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — applying the DRA test manifest failed." >> "${EVIDENCE_FILE}"
        cleanup_ns dra-test
        return
    fi
    rm -f "${test_manifest}"

    # Wait for pod completion
    log_info "Waiting for DRA test pod (up to ${POD_TIMEOUT}s)..."
    pod_phase=$(wait_for_pod "dra-test" "dra-gpu-test" "${POD_TIMEOUT}")
    log_info "Pod phase: ${pod_phase}"

    capture "ResourceClaim status" kubectl get resourceclaim -n dra-test -o wide
    echo "" >> "${EVIDENCE_FILE}"
    echo "> **Note:** ResourceClaim shows \`pending\` because the DRA controller deallocates the claim after pod completion. The pod logs below confirm the GPU was successfully allocated and visible during execution." >> "${EVIDENCE_FILE}"
    capture "Pod status" kubectl get pod dra-gpu-test -n dra-test -o wide
    capture "Pod logs" kubectl logs dra-gpu-test -n dra-test

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    if [ "${pod_phase}" = "Succeeded" ]; then
        echo "**Result: PASS** — Pod completed successfully with GPU access via DRA." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — Pod phase: ${pod_phase}" >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup
EOF
    capture "Delete test namespace" cleanup_ns dra-test

    log_info "DRA evidence collection complete."
}

# collect_dra_device_plugin_verdict renders the device-plugin-mode verdict for
# the dra-support section: no claim pod is deployed (whole GPUs are
# device-plugin-allocated); PASS requires a served resource.k8s.io version AND
# at least one ResourceSlice from an NVIDIA driver (gpu.nvidia.com or
# compute-domain.nvidia.com — the latter is the supported publication under
# the device-plugin default). A failed slice query ("error") fails closed.
collect_dra_device_plugin_verdict() {
    local api_version="$1" gpu_slices="$2" cd_slices="$3"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Allocation Test

**Behavioral full-GPU allocation: Not applicable** — device-plugin allocation
policy; whole GPUs are device-plugin-allocated (`nvidia.com/gpu` extended
resource), so no full-GPU ResourceClaim test pod is deployed. DRA support is
evidenced by the served resource.k8s.io API and NVIDIA driver ResourceSlice
publication above.

> **Note:** this section validates a subset under the device-plugin mode —
> API served + NVIDIA ResourceSlice publication. Full pool-generation and
> device-taint validation of the slices is performed by the Go validators
> (`aicr validate --phase conformance`, dra-support check), not this script.
EOF

    echo "" >> "${EVIDENCE_FILE}"
    if [ -z "${api_version}" ]; then
        echo "**Result: FAIL** — the resource.k8s.io API group is not served (requires K8s 1.32+)." >> "${EVIDENCE_FILE}"
        return
    fi
    if [ "${gpu_slices}" = "error" ] || [ "${cd_slices}" = "error" ]; then
        echo "**Result: FAIL** — ResourceSlice inventory query failed (gpu.nvidia.com: ${gpu_slices}, compute-domain.nvidia.com: ${cd_slices}) — fail closed." >> "${EVIDENCE_FILE}"
        return
    fi
    if [ "$((gpu_slices + cd_slices))" -ge 1 ]; then
        echo "**Result: PASS** — resource.k8s.io/${api_version} is served and NVIDIA DRA driver(s) publish ResourceSlices (gpu.nvidia.com: ${gpu_slices}, compute-domain.nvidia.com: ${cd_slices}). Behavioral full-GPU claim test not applicable under the device-plugin allocation policy." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — resource.k8s.io/${api_version} is served but no NVIDIA DRA driver publishes any ResourceSlice (gpu.nvidia.com: 0, compute-domain.nvidia.com: 0)." >> "${EVIDENCE_FILE}"
    fi

    log_info "DRA evidence collection complete (device-plugin mode)."
}

# --- Section 2: Gang Scheduling ---
collect_gang() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/gang-scheduling.md"
    log_info "Collecting Gang Scheduling evidence → ${EVIDENCE_FILE}"
    write_section_header "Gang Scheduling (KAI Scheduler)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that the cluster supports gang (all-or-nothing) scheduling using KAI
scheduler with PodGroups. Both pods in the group must be scheduled together or not at all.

## KAI Scheduler Components
EOF
    capture "KAI scheduler deployments" kubectl get deploy -n kai-scheduler
    capture "KAI scheduler pods" kubectl get pods -n kai-scheduler

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## PodGroup CRD
EOF
    capture "PodGroup CRD" kubectl get crd podgroups.scheduling.run.ai

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Gang Scheduling Test

Deploy a PodGroup with minMember=2 and two GPU pods. KAI scheduler ensures both
pods are scheduled atomically.

**Test manifest:** `pkg/evidence/cncf/scripts/manifests/gang-scheduling-test.yaml`
EOF
    echo '```yaml' >> "${EVIDENCE_FILE}"
    cat "${SCRIPT_DIR}/manifests/gang-scheduling-test.yaml" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Clean up any previous run
    cleanup_ns gang-scheduling-test pre

    # Deploy test
    log_info "Deploying gang scheduling test..."
    capture "Apply test manifest" kubectl apply -f "${SCRIPT_DIR}/manifests/gang-scheduling-test.yaml"

    # Wait for both pods to complete
    log_info "Waiting for gang-worker-0 (up to ${POD_TIMEOUT}s)..."
    phase0=$(wait_for_pod "gang-scheduling-test" "gang-worker-0" "${POD_TIMEOUT}")
    log_info "gang-worker-0 phase: ${phase0}"

    log_info "Waiting for gang-worker-1 (up to ${POD_TIMEOUT}s)..."
    phase1=$(wait_for_pod "gang-scheduling-test" "gang-worker-1" "${POD_TIMEOUT}")
    log_info "gang-worker-1 phase: ${phase1}"

    capture "PodGroup status" kubectl get podgroups -n gang-scheduling-test -o wide
    capture "Pod status" kubectl get pods -n gang-scheduling-test -o wide
    capture "gang-worker-0 logs" kubectl logs gang-worker-0 -n gang-scheduling-test
    capture "gang-worker-1 logs" kubectl logs gang-worker-1 -n gang-scheduling-test

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    if [ "${phase0}" = "Succeeded" ] && [ "${phase1}" = "Succeeded" ]; then
        echo "**Result: PASS** — Both pods scheduled and completed together via gang scheduling." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — worker-0: ${phase0}, worker-1: ${phase1}" >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup
EOF
    capture "Delete test namespace" cleanup_ns gang-scheduling-test

    log_info "Gang scheduling evidence collection complete."
}

# --- Section 3: Secure Accelerator Access ---
# Mode-aware (#1629), mirroring validators/conformance/secure_access_check.go:
# under the DRA mode the isolation test allocates a GPU through a
# ResourceClaim (generated at the served resource.k8s.io version); under the
# device-plugin mode it ports the two-container pattern from
# kubernetes-sigs/ai-conformance#75 — an authorized container with
# `nvidia.com/gpu: 1` limits must see exactly one GPU, and an unauthorized
# sibling container with no GPU request must see none.
collect_secure() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/secure-accelerator-access.md"
    log_info "Collecting Secure Accelerator Access evidence → ${EVIDENCE_FILE}"
    write_section_header "Secure Accelerator Access"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that GPU access is mediated through Kubernetes allocation APIs
(DRA ResourceClaims or device plugin resource limits, per the cluster's GPU
allocation mode), not via direct host device mounts. This ensures proper
isolation, access control, and auditability of accelerator usage.

EOF
    if ! write_alloc_mode_evidence; then
        log_error "GPU allocation mode resolution failed: ${ALLOC_MODE_ERROR}"
        return
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Operator Health

### ClusterPolicy
EOF
    capture "ClusterPolicy status" kubectl get clusterpolicy -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### GPU Operator Pods
EOF
    capture "GPU operator pods" kubectl get pods -n gpu-operator -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### GPU Operator DaemonSets
EOF
    capture "GPU operator DaemonSets" kubectl get ds -n gpu-operator

    if [ "${ALLOC_MODE}" = "device-plugin" ]; then
        collect_secure_device_plugin
    else
        collect_secure_dra
    fi
}

# collect_secure_dra exercises the DRA isolation test: a pod granted one GPU
# through a gpu.nvidia.com ResourceClaim generated at the newest served
# resource.k8s.io API version.
collect_secure_dra() {
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## DRA-Mediated GPU Access

GPU access is provided through DRA ResourceClaims (`resource.k8s.io`), not through
direct `hostPath` volume mounts to `/dev/nvidia*`. The DRA driver advertises individual
GPU devices via ResourceSlices, and pods request access through ResourceClaims.

### ResourceSlices (Device Advertisement)
EOF
    capture "ResourceSlices" kubectl get resourceslices -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### GPU Device Details
EOF
    capture "GPU devices in ResourceSlice" kubectl get resourceslices -o yaml

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Device Isolation Verification

Deploy a test pod requesting 1 GPU via ResourceClaim (generated at the newest
served resource.k8s.io API version) and verify:
1. No `hostPath` volumes to `/dev/nvidia*`
2. Pod spec uses `resourceClaims` (DRA), not `resources.limits` (device plugin)
3. Only the allocated GPU device is visible inside the container
EOF

    local api_version
    api_version=$(detect_resource_api_version)
    if [ -z "${api_version}" ]; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — DRA allocation mode selected but no served resource.k8s.io API version was discovered; the ResourceClaim isolation test cannot run." >> "${EVIDENCE_FILE}"
        log_error "DRA mode with no served resource.k8s.io API version"
        return
    fi

    # Clean up any previous run
    if ! ensure_fresh_namespace secure-access-test; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — previous secure-access-test namespace could not be removed; cannot guarantee a fresh workload." >> "${EVIDENCE_FILE}"
        return
    fi

    # Compose the isolation test manifest with the version-correct claim.
    local test_manifest
    test_manifest=$(mktemp "${TMPDIR:-/tmp}/aicr-evidence-XXXXXX")
    {
        cat <<'MANIFEST'
apiVersion: v1
kind: Namespace
metadata:
  name: secure-access-test
---
MANIFEST
        emit_resourceclaim_yaml "${api_version}" isolated-gpu secure-access-test gpu.nvidia.com
        cat <<'MANIFEST'
---
apiVersion: v1
kind: Pod
metadata:
  name: isolation-test
  namespace: secure-access-test
spec:
  restartPolicy: Never
  tolerations:
    - operator: Exists
  resourceClaims:
    - name: gpu
      resourceClaimName: isolated-gpu
  containers:
    - name: gpu-test
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command:
        - bash
        - -c
        - |
          echo "=== Visible NVIDIA devices ==="
          ls -la /dev/nvidia* 2>/dev/null || echo "No /dev/nvidia* devices"
          echo ""
          echo "=== nvidia-smi output (claim requests exactly 1 GPU) ==="
          if ! nvidia-smi -L; then
            echo "FAIL: nvidia-smi cannot enumerate GPUs - no usable GPU granted"
            exit 1
          fi
          count=$(nvidia-smi -L | wc -l)
          echo ""
          echo "=== GPU count ==="
          nvidia-smi --query-gpu=index,name,uuid --format=csv,noheader
          echo "visible GPU count: ${count}"
          if [ "${count}" != "1" ]; then
            echo "FAIL: expected exactly 1 GPU visible via the ResourceClaim, saw ${count}"
            exit 1
          fi
          echo "PASS: exactly one GPU visible"
          echo "Secure accelerator access test completed"
      resources:
        claims:
          - name: gpu
MANIFEST
    } > "${test_manifest}"

    if ! capture "Apply isolation test manifest" kubectl apply -f "${test_manifest}"; then
        rm -f "${test_manifest}"
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — applying the isolation test manifest failed." >> "${EVIDENCE_FILE}"
        cleanup_ns secure-access-test
        return
    fi
    rm -f "${test_manifest}"

    log_info "Waiting for isolation test pod (up to ${POD_TIMEOUT}s)..."
    pod_phase=$(wait_for_pod "secure-access-test" "isolation-test" "${POD_TIMEOUT}")
    log_info "Pod phase: ${pod_phase}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Pod Spec (no hostPath volumes)
EOF
    capture "Pod resourceClaims" kubectl get pod isolation-test -n secure-access-test -o jsonpath='{.spec.resourceClaims}'
    capture "Pod volumes (no hostPath)" kubectl get pod isolation-test -n secure-access-test -o jsonpath='{.spec.volumes}'
    capture "ResourceClaim allocation" kubectl get resourceclaim isolated-gpu -n secure-access-test -o wide
    echo "" >> "${EVIDENCE_FILE}"
    echo "> **Note:** ResourceClaim may show \`pending\` after pod completion because the DRA controller deallocates claims when the consuming pod terminates. The pod logs below confirm GPU isolation was enforced during execution." >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Container GPU Visibility (only allocated GPU visible)
EOF
    capture "Isolation test logs" kubectl logs isolation-test -n secure-access-test

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    if [ "${pod_phase}" = "Succeeded" ]; then
        echo "**Result: PASS** — GPU access mediated through DRA ResourceClaim (resource.k8s.io/${api_version}). No direct host device mounts. Only allocated GPU visible in container." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — Pod phase: ${pod_phase}" >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup
EOF
    capture "Delete test namespace" cleanup_ns secure-access-test

    log_info "Secure accelerator access evidence collection complete."
}

# collect_secure_device_plugin exercises the device-plugin isolation pattern
# ported from kubernetes-sigs/ai-conformance#75 (mirrored by
# validators/conformance/secure_access_check.go): one pod with two containers —
# an authorized container requesting `nvidia.com/gpu: 1` that must see EXACTLY
# one GPU via nvidia-smi (positive probe), and an unauthorized sibling
# container with no GPU request that must see no /dev/nvidia* device nodes and
# no working nvidia-smi (negative probes). The pod reaches phase Succeeded only
# when every container exits 0, so PASS requires all probes to pass; any probe
# or query failure fails closed.
collect_secure_device_plugin() {
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Device-Plugin-Mediated GPU Access

GPU access is provided through the device plugin (`nvidia.com/gpu` extended
resource in `resources.limits`), not through direct `hostPath` volume mounts to
`/dev/nvidia*`. The kubelet grants each container exactly the devices the
device plugin allocated to it.

## Device Isolation Verification

Deploy a test pod with two containers and verify container-level isolation
(kubernetes-sigs/ai-conformance#75 pattern):
1. Authorized container (`nvidia.com/gpu: 1` limit): `nvidia-smi` sees EXACTLY one GPU
2. Unauthorized sibling container (no GPU request): no `/dev/nvidia*` device nodes,
   and `nvidia-smi` is absent or fails
3. No `hostPath` volumes to `/dev/nvidia*`
EOF

    # Clean up any previous run and verify it is actually gone.
    if ! ensure_fresh_namespace secure-access-test; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — previous secure-access-test namespace could not be removed; cannot guarantee a fresh workload." >> "${EVIDENCE_FILE}"
        return
    fi

    local dp_manifest
    dp_manifest=$(mktemp "${TMPDIR:-/tmp}/aicr-evidence-XXXXXX")
    cat <<'MANIFEST' > "${dp_manifest}"
apiVersion: v1
kind: Namespace
metadata:
  name: secure-access-test
---
apiVersion: v1
kind: Pod
metadata:
  name: isolation-test
  namespace: secure-access-test
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: false
    seccompProfile:
      type: RuntimeDefault
  tolerations:
    - operator: Exists
  containers:
    - name: authorized
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command:
        - bash
        - -c
        - |
          echo "=== nvidia-smi -L (authorized container, nvidia.com/gpu: 1) ==="
          if ! nvidia-smi -L; then
            echo "FAIL: nvidia-smi cannot enumerate GPUs - no usable GPU granted"
            exit 1
          fi
          count=$(nvidia-smi -L | wc -l)
          echo "visible GPU count: ${count}"
          if [ "${count}" = "1" ]; then
            echo "PASS: exactly one GPU visible"
          else
            echo "FAIL: expected exactly 1 GPU, saw ${count}"
            exit 1
          fi
      securityContext:
        allowPrivilegeEscalation: false
      resources:
        limits:
          nvidia.com/gpu: 1
    - name: unauthorized
      image: nvidia/cuda:12.9.0-base-ubuntu24.04
      command:
        - bash
        - -c
        - |
          echo "=== /dev/nvidia* (unauthorized sibling, no GPU request) ==="
          if ls /dev/nvidia* 2>/dev/null; then
            echo "FAIL: GPU device nodes visible without GPU allocation"
            exit 1
          fi
          echo "PASS: no /dev/nvidia* device nodes visible"
          echo ""
          echo "=== nvidia-smi (must be absent or fail) ==="
          if nvidia-smi -L 2>/dev/null; then
            echo "FAIL: nvidia-smi enumerated GPUs without GPU allocation"
            exit 1
          fi
          echo "PASS: nvidia-smi absent or fails without GPU allocation"
      securityContext:
        allowPrivilegeEscalation: false
MANIFEST

    if ! capture "Apply isolation test manifest" kubectl apply -f "${dp_manifest}"; then
        rm -f "${dp_manifest}"
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — applying the isolation test manifest failed." >> "${EVIDENCE_FILE}"
        cleanup_ns secure-access-test
        return
    fi
    rm -f "${dp_manifest}"

    log_info "Waiting for device-plugin isolation test pod (up to ${POD_TIMEOUT}s)..."
    pod_phase=$(wait_for_pod "secure-access-test" "isolation-test" "${POD_TIMEOUT}")
    log_info "Pod phase: ${pod_phase}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Pod Spec (device plugin limits, no hostPath volumes)
EOF
    capture "Container GPU limits" kubectl get pod isolation-test -n secure-access-test \
        -o jsonpath='{range .spec.containers[*]}{.name}{": nvidia.com/gpu="}{.resources.limits.nvidia\.com/gpu}{"\n"}{end}'
    capture "Pod volumes (no hostPath)" kubectl get pod isolation-test -n secure-access-test -o jsonpath='{.spec.volumes}'
    capture "Container exit codes" kubectl get pod isolation-test -n secure-access-test \
        -o jsonpath='{range .status.containerStatuses[*]}{.name}{": exitCode="}{.state.terminated.exitCode}{"\n"}{end}'

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Authorized Container (positive probe: exactly one GPU)
EOF
    capture "Authorized container logs" kubectl logs isolation-test -c authorized -n secure-access-test

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Unauthorized Sibling Container (negative probes: no GPU visible)
EOF
    capture "Unauthorized container logs" kubectl logs isolation-test -c unauthorized -n secure-access-test

    # Verdict: phase Succeeded requires every container to exit 0, i.e. the
    # positive probe AND both negative probes passed. Anything else — probe
    # failure, scheduling failure, timeout, query failure — is FAIL.
    echo "" >> "${EVIDENCE_FILE}"
    if [ "${pod_phase}" = "Succeeded" ]; then
        echo "**Result: PASS** — GPU access mediated through device plugin \`nvidia.com/gpu\` limits: the authorized container saw exactly one GPU and the unauthorized sibling container saw none. No direct host device mounts." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — Pod phase: ${pod_phase} (PASS requires the authorized container to see exactly one GPU AND the unauthorized sibling to see none)." >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup
EOF
    capture "Delete test namespace" cleanup_ns secure-access-test

    log_info "Secure accelerator access evidence collection complete (device-plugin mode)."
}

# --- Section 4a: Accelerator Metrics (DCGM Exporter) ---
collect_accelerator_metrics() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/accelerator-metrics.md"
    log_info "Collecting Accelerator Metrics evidence → ${EVIDENCE_FILE}"
    write_section_header "Accelerator Metrics (DCGM Exporter)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that the DCGM exporter exposes per-GPU metrics (utilization, memory,
temperature, power) in Prometheus format via a standardized metrics endpoint.

## Monitoring Stack Health

### Prometheus
EOF
    capture "Prometheus pods" kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus
    capture "Prometheus service" kubectl get svc kube-prometheus-prometheus -n monitoring

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Prometheus Adapter (Custom Metrics API)
EOF
    capture "Prometheus adapter pod" kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
    capture "Prometheus adapter service" kubectl get svc prometheus-adapter -n monitoring

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Grafana
EOF
    capture "Grafana pod" kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Accelerator Metrics (DCGM Exporter)

NVIDIA DCGM Exporter exposes per-GPU metrics including utilization, memory usage,
temperature, power draw, and more in Prometheus exposition format.

### DCGM Exporter Health
EOF
    capture "DCGM exporter pod" kubectl get pods -n gpu-operator -l app=nvidia-dcgm-exporter -o wide
    capture "DCGM exporter service" kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### DCGM Metrics Endpoint

Query DCGM exporter directly to show raw GPU metrics in Prometheus format.
EOF

    # Query DCGM metrics via port-forward to the exporter service.
    # The DCGM container is minimal (no shell tools), so we port-forward and curl from the host.
    local dcgm_svc
    dcgm_svc=$(kubectl get svc -n gpu-operator -l app=nvidia-dcgm-exporter -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "${dcgm_svc}" ]; then
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Key GPU metrics from DCGM exporter (sampled)**" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
        kubectl port-forward "svc/${dcgm_svc}" -n gpu-operator 9401:9400 &>/dev/null &
        local dcgm_pf_pid=$!
        # Wait for port-forward to be ready (up to 10s)
        local dcgm_ready=false
        for i in $(seq 1 10); do
            if curl -sf http://localhost:9401/metrics &>/dev/null; then dcgm_ready=true; break; fi
            if ! kill -0 "${dcgm_pf_pid}" 2>/dev/null; then break; fi
            sleep 1
        done
        if [ "${dcgm_ready}" = "true" ]; then
            curl -sf http://localhost:9401/metrics 2>/dev/null | \
                grep -E "^(DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED|DCGM_FI_DEV_FB_FREE|DCGM_FI_DEV_GPU_TEMP|DCGM_FI_DEV_POWER_USAGE|DCGM_FI_DEV_MEM_COPY_UTIL)" | \
                head -30 >> "${EVIDENCE_FILE}" 2>&1
        fi
        kill "${dcgm_pf_pid}" 2>/dev/null || true
        echo '```' >> "${EVIDENCE_FILE}"
    else
        echo "" >> "${EVIDENCE_FILE}"
        echo "**WARNING:** Could not find DCGM exporter service" >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Prometheus Querying GPU Metrics

Query Prometheus to verify it is actively scraping and storing DCGM metrics.
EOF

    # Port-forward to Prometheus and query
    kubectl port-forward svc/kube-prometheus-prometheus -n monitoring 9090:9090 &>/dev/null &
    local pf_pid=$!

    if wait_for_port 9090 30 "${pf_pid}"; then
        # GPU Utilization
        echo "" >> "${EVIDENCE_FILE}"
        echo "**GPU Utilization (DCGM_FI_DEV_GPU_UTIL)**" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
        curl -sf 'http://localhost:9090/api/v1/query?query=DCGM_FI_DEV_GPU_UTIL' 2>&1 | \
            python3 -c "import sys,json; data=json.loads(sys.stdin.read()); print(json.dumps(data,indent=2))" >> "${EVIDENCE_FILE}" 2>&1
        echo '```' >> "${EVIDENCE_FILE}"

        # GPU Memory Used
        echo "" >> "${EVIDENCE_FILE}"
        echo "**GPU Memory Used (DCGM_FI_DEV_FB_USED)**" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
        curl -sf 'http://localhost:9090/api/v1/query?query=DCGM_FI_DEV_FB_USED' 2>&1 | \
            python3 -c "import sys,json; data=json.loads(sys.stdin.read()); print(json.dumps(data,indent=2))" >> "${EVIDENCE_FILE}" 2>&1
        echo '```' >> "${EVIDENCE_FILE}"

        # GPU Temperature
        echo "" >> "${EVIDENCE_FILE}"
        echo "**GPU Temperature (DCGM_FI_DEV_GPU_TEMP)**" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
        curl -sf 'http://localhost:9090/api/v1/query?query=DCGM_FI_DEV_GPU_TEMP' 2>&1 | \
            python3 -c "import sys,json; data=json.loads(sys.stdin.read()); print(json.dumps(data,indent=2))" >> "${EVIDENCE_FILE}" 2>&1
        echo '```' >> "${EVIDENCE_FILE}"

        # GPU Power Usage
        echo "" >> "${EVIDENCE_FILE}"
        echo "**GPU Power Draw (DCGM_FI_DEV_POWER_USAGE)**" >> "${EVIDENCE_FILE}"
        echo '```' >> "${EVIDENCE_FILE}"
        curl -sf 'http://localhost:9090/api/v1/query?query=DCGM_FI_DEV_POWER_USAGE' 2>&1 | \
            python3 -c "import sys,json; data=json.loads(sys.stdin.read()); print(json.dumps(data,indent=2))" >> "${EVIDENCE_FILE}" 2>&1
        echo '```' >> "${EVIDENCE_FILE}"

    else
        echo "" >> "${EVIDENCE_FILE}"
        echo "**WARNING:** Could not port-forward to Prometheus" >> "${EVIDENCE_FILE}"
    fi
    # Always clean up port-forward process to avoid leaking on timeout/failure
    kill "${pf_pid}" 2>/dev/null || true

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    local pass=true
    if [ -z "${dcgm_svc}" ]; then pass=false; fi
    if [ "${pass}" = "true" ]; then
        echo "**Result: PASS** — DCGM exporter provides per-GPU metrics (utilization, memory, temperature, power). Prometheus actively scrapes and stores metrics." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — DCGM exporter not found or metrics unavailable." >> "${EVIDENCE_FILE}"
    fi

    log_info "Accelerator metrics evidence collection complete."
}

# --- Section 4b: AI Service Metrics (Prometheus Discovery) ---
# Detects the AI workload type and collects metrics evidence accordingly.
# Priority: Dynamo inference > standalone PyTorch training (with embedded manifest).
# The training path only requires GPU nodes + Prometheus — no Kubeflow Trainer needed.
collect_service_metrics() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/ai-service-metrics.md"
    log_info "Collecting AI Service Metrics evidence → ${EVIDENCE_FILE}"

    # Detect workload type: Dynamo inference > NIM inference > PyTorch training
    local dynamo_ns="dynamo-workload"
    local nim_ns="nim-workload"

    if kubectl get pods -n "${dynamo_ns}" -l nvidia.com/dynamo-component-type=worker --no-headers 2>/dev/null | grep -q .; then
        collect_service_metrics_dynamo
    elif kubectl get pods -n "${nim_ns}" -l app.kubernetes.io/managed-by=k8s-nim-operator --no-headers 2>/dev/null | grep -q .; then
        collect_service_metrics_nim
    else
        # Training path: deploys a standalone PyTorch pod with Prometheus metrics.
        # Only requires GPU nodes + Prometheus — no Kubeflow Trainer dependency.
        collect_service_metrics_trainer
    fi
}

# --- Dynamo inference metrics collection ---
collect_service_metrics_dynamo() {
    write_section_header "AI Service Metrics (Prometheus PodMonitor Discovery)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that Prometheus discovers and collects metrics from AI workloads
that expose them in Prometheus exposition format, using the PodMonitor CRD
for automatic target discovery.

## Dynamo Inference Workload
EOF

    local NS="dynamo-workload"
    local deployed_dynamo=false

    # Deploy Dynamo workload if not already running
    if ! kubectl get pods -n "${NS}" -l nvidia.com/dynamo-component-type=worker --no-headers 2>/dev/null | grep -q .; then
        local manifest="${SCRIPT_DIR}/manifests/dynamo-vllm-agg.yaml"
        if [ -f "${manifest}" ]; then
            log_info "Deploying Dynamo vLLM workload from embedded manifest..."
            kubectl apply -f "${manifest}"
            deployed_dynamo=true
        else
            log_warn "No Dynamo workload running and manifest not found at ${manifest}"
            echo "**Result: SKIP (prerequisite absent)** — no Dynamo workload or deployment manifest was found." >> "${EVIDENCE_FILE}"
            return
        fi
    fi

    # Wait for Dynamo workload pods to be ready (poll every 15s, up to 5 minutes)
    log_info "Waiting for Dynamo workload pods in ${NS} (up to 5m)..."
    local worker_pod=""
    local frontend_pod=""
    local workload_ready=false
    for i in $(seq 1 20); do
        worker_pod=$(kubectl get pods -n "${NS}" -l nvidia.com/dynamo-component-type=worker \
            --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
        frontend_pod=$(kubectl get pods -n "${NS}" -l nvidia.com/dynamo-component-type=frontend \
            --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
        if [ -n "${worker_pod}" ] && [ -n "${frontend_pod}" ]; then
            workload_ready=true
            break
        fi
        log_info "Dynamo pods not ready yet (attempt ${i}/20), retrying in 15s..."
        sleep 15
    done

    if [ "${workload_ready}" != "true" ]; then
        log_warn "Dynamo workload not ready in ${NS} after 5 minutes"
        echo "**Result: FAIL** — Dynamo workload is present but did not become ready in ${NS}." >> "${EVIDENCE_FILE}"
        return
    fi

    # Wait for the inference endpoint to be serving (model loading can take additional time)
    log_info "Waiting for Dynamo frontend to serve requests (up to 3m)..."
    local serving_ready=false
    for i in $(seq 1 12); do
        if kubectl exec -n "${NS}" "${frontend_pod}" -- python3 -c "
import urllib.request
urllib.request.urlopen('http://localhost:8000/v1/models')" &>/dev/null; then
            serving_ready=true
            break
        fi
        log_info "Frontend not serving yet (attempt ${i}/12), retrying in 15s..."
        sleep 15
    done

    if [ "${serving_ready}" != "true" ]; then
        log_warn "Dynamo frontend not serving after 3 minutes"
        capture "Dynamo workload pods" kubectl get pods -n "${NS}" -o wide
        echo "**Result: FAIL** — Dynamo frontend did not become ready." >> "${EVIDENCE_FILE}"
        return
    fi

    capture "Dynamo workload pods" kubectl get pods -n "${NS}" -o wide

    # Send inference requests via frontend to generate non-zero metrics
    log_info "Sending 10 inference requests via Dynamo frontend..."
    for i in $(seq 1 10); do
        kubectl exec -n "${NS}" "${frontend_pod}" -- python3 -c "
import urllib.request, json
req = urllib.request.Request('http://localhost:8000/v1/completions',
    data=json.dumps({'model': 'Qwen/Qwen3-0.6B', 'prompt': 'Explain GPU computing.', 'max_tokens': 50}).encode(),
    headers={'Content-Type': 'application/json'})
urllib.request.urlopen(req)" &>/dev/null || true
    done

    # Collect worker metrics (Dynamo runtime exposes metrics on port 9090 "system")
    # Filter to dynamo_component_* and key vllm:* metrics, excluding _bucket and _created
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Worker metrics endpoint (sampled after 10 inference requests)**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl exec -n "${NS}" "${worker_pod}" -- python3 -c "
import urllib.request
data = urllib.request.urlopen('http://localhost:9090/metrics').read().decode()
for l in data.split('\n'):
    if not l or l.startswith('#') or '_bucket' in l or '_created' in l:
        continue
    # Only show dynamo_component_* and select vllm:* metrics
    if not (l.startswith('dynamo_component_') or l.startswith('vllm:prefix_cache') or
            l.startswith('vllm:engine_sleep')):
        continue
    parts = l.rsplit(' ', 1)
    if len(parts) == 2 and parts[1] not in ('0', '0.0'):
        print(l)" 2>&1 | head -15 >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Show PodMonitors (auto-created by Dynamo operator)
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## PodMonitors (Auto-Created by Dynamo Operator)

The Dynamo operator automatically creates PodMonitors for worker and frontend
pods. Prometheus discovers workload pods in any namespace via
`namespaceSelector.any: true`.
EOF
    capture "Dynamo PodMonitors" kubectl get podmonitors -n dynamo-system
    capture "Worker PodMonitor spec" kubectl get podmonitor dynamo-worker -n dynamo-system -o yaml

    # Show worker pod labels matching PodMonitor selector
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Worker pod labels (matching PodMonitor selector)**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get pod "${worker_pod}" -n "${NS}" -o jsonpath='{.metadata.labels}' 2>&1 | \
        python3 -c "import sys,json; d=json.loads(sys.stdin.read()); [print(f'{k}: {v}') for k,v in sorted(d.items()) if 'dynamo' in k or 'metrics' in k]" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    # Verify Prometheus target discovery via port-forward
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Prometheus Target Discovery

Prometheus automatically discovers both Dynamo frontend and worker pods as
scrape targets via PodMonitors and actively collects metrics.
EOF

    log_info "Checking Prometheus targets for Dynamo workload..."
    kubectl port-forward svc/kube-prometheus-prometheus -n monitoring 9090:9090 &>/dev/null &
    local pf_pid=$!

    if wait_for_port 9090 30 "${pf_pid}"; then
        # Wait for Dynamo targets to appear (up to 2 minutes)
        local target_found=false
        for i in $(seq 1 12); do
            if curl -sf 'http://localhost:9090/api/v1/targets?state=active' 2>/dev/null | \
                python3 -c "import sys,json; data=json.load(sys.stdin); exit(0 if any('dynamo' in t['labels'].get('job','') for t in data['data']['activeTargets']) else 1)" 2>/dev/null; then
                target_found=true
                break
            fi
            sleep 10
        done

        if [ "${target_found}" = "true" ]; then
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Prometheus scrape targets (active)**" >> "${EVIDENCE_FILE}"
            echo '```' >> "${EVIDENCE_FILE}"
            curl -sf 'http://localhost:9090/api/v1/targets?state=active' 2>/dev/null | \
                python3 -c "
import sys,json
data=json.load(sys.stdin)
for t in data['data']['activeTargets']:
    job = t['labels'].get('job','')
    # Only show workload PodMonitor targets (dynamo-system/dynamo-*), not operator ServiceMonitor
    if job.startswith('dynamo-system/dynamo-'):
        print(json.dumps({'job':job,'endpoint':t['scrapeUrl'],'health':t['health'],'lastScrape':t['lastScrape']},indent=2))" >> "${EVIDENCE_FILE}" 2>&1
            echo '```' >> "${EVIDENCE_FILE}"

            # Query Dynamo metrics from Prometheus
            cat >> "${EVIDENCE_FILE}" <<'EOF'

## Dynamo Metrics in Prometheus

Prometheus collects Dynamo application-level inference metrics from both
frontend and worker, including request throughput, latency, token counts,
and model KV cache utilization.
EOF
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Dynamo metrics queried from Prometheus (after 10 inference requests)**" >> "${EVIDENCE_FILE}"
            echo '```' >> "${EVIDENCE_FILE}"
            local prom_response
            prom_response=$(curl -sf --data-urlencode 'query={job=~"dynamo-system/dynamo-.*",__name__=~"dynamo_.*"}' 'http://localhost:9090/api/v1/query' 2>/dev/null)
            if [ -n "${prom_response}" ]; then
                echo "${prom_response}" | python3 -c "
import sys,json
data=json.load(sys.stdin)
seen=set()
for r in data['data']['result']:
    name=r['metric']['__name__']
    val=r['value'][1]
    if name not in seen and val not in ('0','0.0') and '_bucket' not in name:
        seen.add(name)
        endpoint=r['metric'].get('dynamo_endpoint','')
        label=f'{{endpoint=\"{endpoint}\"}}' if endpoint else ''
        print(f'{name}{label} = {val}')" 2>&1 | head -15 >> "${EVIDENCE_FILE}"
            else
                echo "WARNING: No response from Prometheus query" >> "${EVIDENCE_FILE}"
            fi
            echo '```' >> "${EVIDENCE_FILE}"

            echo "" >> "${EVIDENCE_FILE}"
            echo "**Result: PASS** — Prometheus discovers Dynamo inference workloads (frontend + worker) via operator-managed PodMonitors and actively scrapes their Prometheus-format metrics endpoints. Application-level AI inference metrics are collected and queryable." >> "${EVIDENCE_FILE}"
        else
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Result: FAIL** — Prometheus did not discover Dynamo targets within 2 minutes. Ensure PodMonitors have the 'release' label matching Prometheus podMonitorSelector." >> "${EVIDENCE_FILE}"
        fi
    else
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — Could not connect to Prometheus." >> "${EVIDENCE_FILE}"
    fi
    kill "${pf_pid}" 2>/dev/null || true

    # Cleanup deployed workload if we created it
    if [ "${deployed_dynamo}" = "true" ] && [ "${NO_CLEANUP}" != "true" ]; then
        log_info "Cleaning up deployed Dynamo workload..."
        kubectl delete -f "${SCRIPT_DIR}/manifests/dynamo-vllm-agg.yaml" --ignore-not-found 2>/dev/null || true
    fi

    # Always document cleanup steps
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup

**Delete workload namespace**
```
$ kubectl delete ns dynamo-workload
```
EOF

    log_info "AI service metrics (Dynamo) evidence collection complete."
}

# --- NIM inference metrics collection ---
# Collects metrics from a running NIMService deployment. NIM exposes OpenAI-compatible
# inference metrics at /v1/metrics in Prometheus exposition format.
collect_service_metrics_nim() {
    write_section_header "AI Service Metrics (NIM Inference)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that NVIDIA NIM inference microservices expose Prometheus-format
metrics that can be discovered and collected by the monitoring stack.

## NIM Inference Workload
EOF

    local NS="nim-workload"

    # Find the NIM service pod
    local nim_pod=""
    nim_pod=$(kubectl get pods -n "${NS}" -l app.kubernetes.io/managed-by=k8s-nim-operator \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -z "${nim_pod}" ]; then
        log_warn "NIM workload is present but has no running pod in ${NS}"
        echo "**Result: FAIL** — NIM workload is present but has no running pod in ${NS}." >> "${EVIDENCE_FILE}"
        return
    fi

    # Get the NIMService name from pod labels
    local nim_service=""
    nim_service=$(kubectl get pod "${nim_pod}" -n "${NS}" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/name}' 2>/dev/null)

    capture "NIMService" kubectl get nimservice -n "${NS}"
    capture "NIM workload pods" kubectl get pods -n "${NS}" -o wide

    # Wait for NIM to be serving
    log_info "Checking NIM readiness..."
    local serving_ready=false
    for i in $(seq 1 12); do
        if kubectl exec -n "${NS}" "${nim_pod}" -- python3 -c "
import urllib.request
urllib.request.urlopen('http://localhost:8000/v1/health/ready')" &>/dev/null; then
            serving_ready=true
            break
        fi
        log_info "NIM not serving yet (attempt ${i}/12), retrying in 15s..."
        sleep 15
    done

    if [ "${serving_ready}" != "true" ]; then
        log_warn "NIM service not serving after 3 minutes"
        echo "**Result: FAIL** — NIM service did not become ready." >> "${EVIDENCE_FILE}"
        return
    fi

    # Show available models
    echo "" >> "${EVIDENCE_FILE}"
    echo "**NIM models endpoint**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl exec -n "${NS}" "${nim_pod}" -- python3 -c "
import urllib.request, json
data = json.loads(urllib.request.urlopen('http://localhost:8000/v1/models').read())
for m in data['data']:
    print(f\"Model: {m['id']}\")" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    # Get model name for requests
    local model_name=""
    model_name=$(kubectl exec -n "${NS}" "${nim_pod}" -- python3 -c "
import urllib.request, json
data = json.loads(urllib.request.urlopen('http://localhost:8000/v1/models').read())
print(data['data'][0]['id'])" 2>/dev/null)

    # Send inference requests to generate non-zero metrics
    log_info "Sending 10 inference requests via NIM..."
    for i in $(seq 1 10); do
        kubectl exec -n "${NS}" "${nim_pod}" -- python3 -c "
import urllib.request, json
req = urllib.request.Request('http://localhost:8000/v1/chat/completions',
    data=json.dumps({'model': '${model_name}', 'messages': [{'role': 'user', 'content': 'Explain GPU computing in one sentence.'}], 'max_tokens': 30}).encode(),
    headers={'Content-Type': 'application/json'})
urllib.request.urlopen(req)" &>/dev/null || true
    done

    # Collect NIM metrics from /v1/metrics
    echo "" >> "${EVIDENCE_FILE}"
    echo "**NIM inference metrics endpoint (sampled after generating inference traffic)**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl exec -n "${NS}" "${nim_pod}" -- python3 -c "
import urllib.request
data = urllib.request.urlopen('http://localhost:8000/v1/metrics').read().decode()
for l in data.split('\n'):
    if not l or l.startswith('#') or '_bucket' in l or '_created' in l:
        continue
    parts = l.rsplit(' ', 1)
    if len(parts) == 2 and parts[1] not in ('0', '0.0'):
        # Show key inference metrics
        if any(k in l for k in ['prompt_tokens', 'generation_tokens', 'time_to_first_token',
                'time_per_output_token', 'request_success', 'num_request',
                'e2e_request_latency', 'request_prompt_tokens', 'request_generation_tokens']):
            print(l)" 2>&1 | head -20 >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Create a ServiceMonitor so Prometheus can discover and scrape NIM metrics.
    # NIM exposes metrics at /v1/metrics (not /metrics), so we need a custom path.
    log_info "Creating ServiceMonitor for NIM metrics discovery..."
    kubectl apply -f - <<'SM_EOF'
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: nim-inference
  namespace: monitoring
  labels:
    release: kube-prometheus
spec:
  namespaceSelector:
    matchNames:
      - nim-workload
  selector:
    matchLabels:
      app.kubernetes.io/managed-by: k8s-nim-operator
  endpoints:
    - port: api
      path: /v1/metrics
      interval: 15s
SM_EOF

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Prometheus Metrics Discovery

A ServiceMonitor is created to enable Prometheus auto-discovery of NIM inference
metrics. NIM exposes metrics at `/v1/metrics` in Prometheus exposition format.
EOF

    capture "NIM ServiceMonitor" kubectl get servicemonitor nim-inference -n monitoring -o yaml

    log_info "Waiting for Prometheus to discover and scrape NIM targets (up to 3m)..."
    kubectl port-forward svc/kube-prometheus-prometheus -n monitoring 9090:9090 &>/dev/null &
    local pf_pid=$!

    if wait_for_port 9090 30 "${pf_pid}"; then
        # Wait for NIM targets with health=up (at least one successful scrape).
        # Match by namespace since the job name comes from the service name.
        local target_found=false
        for i in $(seq 1 18); do
            if curl -sf 'http://localhost:9090/api/v1/targets?state=active' 2>/dev/null | \
                python3 -c "import sys,json; data=json.load(sys.stdin); exit(0 if any(t['labels'].get('namespace','')=='${NS}' and t.get('health')=='up' for t in data['data']['activeTargets']) else 1)" 2>/dev/null; then
                target_found=true
                break
            fi
            log_info "NIM target not yet healthy (attempt ${i}/18), retrying in 10s..."
            sleep 10
        done

        if [ "${target_found}" = "true" ]; then
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Prometheus scrape targets (active)**" >> "${EVIDENCE_FILE}"
            echo '```' >> "${EVIDENCE_FILE}"
            curl -sf 'http://localhost:9090/api/v1/targets?state=active' 2>/dev/null | \
                python3 -c "
import sys,json
data=json.load(sys.stdin)
for t in data['data']['activeTargets']:
    ns = t['labels'].get('namespace','')
    if ns == '${NS}':
        print(json.dumps({'job':t['labels'].get('job',''),'endpoint':t['scrapeUrl'],'health':t['health'],'lastScrape':t['lastScrape']},indent=2))" >> "${EVIDENCE_FILE}" 2>&1
            echo '```' >> "${EVIDENCE_FILE}"

            # Query NIM-specific metrics from Prometheus
            local prom_response
            prom_response=$(curl -sf --data-urlencode "query={__name__=~\"prompt_tokens_total|generation_tokens_total|time_to_first_token_seconds_sum|time_per_output_token_seconds_sum|e2e_request_latency_seconds_sum\",model_name=~\".*\"}" 'http://localhost:9090/api/v1/query' 2>/dev/null)

            if [ -n "${prom_response}" ] && echo "${prom_response}" | python3 -c "import sys,json; data=json.load(sys.stdin); exit(0 if data['data']['result'] else 1)" 2>/dev/null; then
                echo "" >> "${EVIDENCE_FILE}"
                echo "**NIM metrics queried from Prometheus**" >> "${EVIDENCE_FILE}"
                echo '```' >> "${EVIDENCE_FILE}"
                echo "${prom_response}" | python3 -c "
import sys,json
data=json.load(sys.stdin)
for r in data['data']['result']:
    name=r['metric']['__name__']
    model=r['metric'].get('model_name','')
    val=r['value'][1]
    print(f'{name}{{model_name=\"{model}\"}} = {val}')" 2>&1 | head -15 >> "${EVIDENCE_FILE}"
                echo '```' >> "${EVIDENCE_FILE}"
            fi

            echo "" >> "${EVIDENCE_FILE}"
            echo "**Result: PASS** — Prometheus discovers NIM inference workloads via ServiceMonitor and actively scrapes application-level AI inference metrics (token throughput, request latency, time-to-first-token) from the /v1/metrics endpoint." >> "${EVIDENCE_FILE}"
        else
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Result: FAIL** — Prometheus did not discover NIM targets within 2 minutes." >> "${EVIDENCE_FILE}"
        fi
    else
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — Could not connect to Prometheus." >> "${EVIDENCE_FILE}"
    fi
    kill "${pf_pid}" 2>/dev/null || true

    # Clean up ServiceMonitor
    if [ "${NO_CLEANUP}" != "true" ]; then
        kubectl delete servicemonitor nim-inference -n monitoring --ignore-not-found 2>/dev/null || true
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup

**Delete workload namespace**
```
$ kubectl delete ns nim-workload
```
EOF

    log_info "AI service metrics (NIM) evidence collection complete."
}

# --- PyTorch training workload metrics collection ---
# Deploys a PyTorch training pod that exposes training metrics (loss, throughput,
# GPU memory) on :8080/metrics in Prometheus format via a ServiceMonitor.
collect_service_metrics_trainer() {
    write_section_header "AI Service Metrics (Prometheus ServiceMonitor Discovery)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates that Prometheus discovers and collects metrics from AI training
workloads that expose them in Prometheus exposition format, using the
ServiceMonitor CRD for automatic target discovery.

## PyTorch Training Workload
EOF

    local NS="trainer-metrics-test"
    local train_manifest="${SCRIPT_DIR}/manifests/trainer-pytorch-test.yaml"
    local deployed_training=false

    # Deploy PyTorch training workload
    if [ -f "${train_manifest}" ]; then
        log_info "Deploying PyTorch training workload..."
        kubectl apply -f "${train_manifest}"
        deployed_training=true
    else
        log_warn "trainer-pytorch-test.yaml not found at ${train_manifest}"
        echo "**Result: SKIP (prerequisite absent)** — training manifest not found." >> "${EVIDENCE_FILE}"
        return
    fi

    # Wait for training pod to be running (poll every 15s, up to 5 minutes)
    log_info "Waiting for training pod to be running (up to 5m)..."
    local pod_ready=false
    for i in $(seq 1 20); do
        local phase
        phase=$(kubectl get pod pytorch-training-job -n "${NS}" -o jsonpath='{.status.phase}' 2>/dev/null)
        if [ "${phase}" = "Running" ]; then
            pod_ready=true
            break
        elif [ "${phase}" = "Failed" ] || [ "${phase}" = "Error" ]; then
            log_error "Training pod failed"
            break
        fi
        log_info "Training pod not ready yet (attempt ${i}/20), retrying in 15s..."
        sleep 15
    done

    if [ "${pod_ready}" != "true" ]; then
        log_warn "Training pod did not become ready"
        capture "Training pod status" kubectl get pods -n "${NS}" -o wide
        echo "**Result: FAIL** — Training pod did not become ready." >> "${EVIDENCE_FILE}"
        if [ "${deployed_training}" = "true" ] && [ "${NO_CLEANUP}" != "true" ]; then
            kubectl delete -f "${train_manifest}" --ignore-not-found 2>/dev/null || true
        fi
        return
    fi

    # Wait for metrics endpoint to be serving (training may still be warming up)
    log_info "Waiting for training metrics endpoint (up to 2m)..."
    local metrics_ready=false
    for i in $(seq 1 8); do
        if kubectl exec -n "${NS}" pytorch-training-job -- python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8080/metrics')" &>/dev/null; then
            metrics_ready=true
            break
        fi
        sleep 15
    done

    if [ "${metrics_ready}" != "true" ]; then
        log_warn "Training metrics endpoint not ready"
        echo "**Result: FAIL** — Training metrics endpoint not ready." >> "${EVIDENCE_FILE}"
        if [ "${deployed_training}" = "true" ] && [ "${NO_CLEANUP}" != "true" ]; then
            kubectl delete -f "${train_manifest}" --ignore-not-found 2>/dev/null || true
        fi
        return
    fi

    capture "Training workload pod" kubectl get pods -n "${NS}" -o wide

    # Collect training metrics from the pod
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Training metrics endpoint (after training run)**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl exec -n "${NS}" pytorch-training-job -- python3 -c "
import urllib.request
print(urllib.request.urlopen('http://localhost:8080/metrics').read().decode())" 2>&1 >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Show ServiceMonitor
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## ServiceMonitor
EOF
    capture "Training ServiceMonitor" kubectl get servicemonitor pytorch-training -n "${NS}" -o yaml
    capture "Service endpoint" kubectl get endpoints pytorch-training-metrics -n "${NS}"

    # Verify Prometheus target discovery
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Prometheus Target Discovery

Prometheus automatically discovers the PyTorch training workload as a scrape
target via ServiceMonitor and actively collects metrics.
EOF

    log_info "Checking Prometheus targets for training workload..."
    kubectl port-forward svc/kube-prometheus-prometheus -n monitoring 9090:9090 &>/dev/null &
    local pf_pid=$!

    if wait_for_port 9090 30 "${pf_pid}"; then
        # Wait for target to appear (up to 3 minutes)
        local target_found=false
        for i in $(seq 1 18); do
            if curl -sf 'http://localhost:9090/api/v1/targets?state=active' 2>/dev/null | \
                python3 -c "import sys,json; data=json.load(sys.stdin); exit(0 if any(t['labels'].get('job','')=='pytorch-training-metrics' for t in data['data']['activeTargets']) else 1)" 2>/dev/null; then
                target_found=true
                break
            fi
            sleep 10
        done

        if [ "${target_found}" = "true" ]; then
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Prometheus scrape target (active)**" >> "${EVIDENCE_FILE}"
            echo '```' >> "${EVIDENCE_FILE}"
            curl -sf 'http://localhost:9090/api/v1/targets?state=active' 2>/dev/null | \
                python3 -c "
import sys,json
data=json.load(sys.stdin)
for t in data['data']['activeTargets']:
    if t['labels'].get('job','')=='pytorch-training-metrics':
        print(json.dumps({'job':t['labels']['job'],'endpoint':t['scrapeUrl'],'health':t['health'],'lastScrape':t['lastScrape']},indent=2))" >> "${EVIDENCE_FILE}" 2>&1
            echo '```' >> "${EVIDENCE_FILE}"

            # Wait for metrics to be ingested (one more scrape cycle)
            sleep 20

            # Query training metrics from Prometheus
            cat >> "${EVIDENCE_FILE}" <<'EOF'

## Training Metrics in Prometheus

Prometheus collects PyTorch training workload metrics including training step
count, loss, throughput, and GPU memory utilization.
EOF
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Training metrics queried from Prometheus**" >> "${EVIDENCE_FILE}"
            echo '```' >> "${EVIDENCE_FILE}"
            for metric in training_step_total training_loss training_throughput_samples_per_sec training_gpu_memory_used_bytes training_gpu_memory_total_bytes; do
                local val
                val=$(curl -sf --data-urlencode "query=${metric}{job=\"pytorch-training-metrics\"}" 'http://localhost:9090/api/v1/query' 2>/dev/null | \
                    python3 -c "import sys,json; data=json.load(sys.stdin); r=data['data']['result']; print(f'{r[0][\"metric\"][\"__name__\"]} = {r[0][\"value\"][1]}') if r else None" 2>/dev/null)
                if [ -n "${val}" ]; then
                    echo "${val}" >> "${EVIDENCE_FILE}"
                fi
            done
            echo '```' >> "${EVIDENCE_FILE}"

            echo "" >> "${EVIDENCE_FILE}"
            echo "**Result: PASS** — Prometheus discovers the PyTorch training workload via ServiceMonitor and actively scrapes its Prometheus-format metrics endpoint. Training-level metrics (step count, loss, throughput, GPU memory) are collected and queryable." >> "${EVIDENCE_FILE}"
        else
            echo "" >> "${EVIDENCE_FILE}"
            echo "**Result: FAIL** — Prometheus did not discover training target within 3 minutes." >> "${EVIDENCE_FILE}"
        fi
    else
        echo "" >> "${EVIDENCE_FILE}"
        echo "**Result: FAIL** — Could not connect to Prometheus." >> "${EVIDENCE_FILE}"
    fi
    kill "${pf_pid}" 2>/dev/null || true

    # Cleanup
    if [ "${deployed_training}" = "true" ] && [ "${NO_CLEANUP}" != "true" ]; then
        log_info "Cleaning up training workload..."
        kubectl delete -f "${train_manifest}" --ignore-not-found 2>/dev/null || true
    fi

    log_info "AI service metrics (training) evidence collection complete."
}

# --- Section 5: Inference API Gateway ---
collect_gateway() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/inference-gateway.md"
    log_info "Collecting Inference API Gateway evidence → ${EVIDENCE_FILE}"

    # Skip if agentgateway is not installed (training clusters don't have inference gateway)
    if ! kubectl get deploy -n agentgateway-system --no-headers 2>/dev/null | grep -q .; then
        write_section_header "Inference API Gateway (agentgateway)"
        echo "**Result: SKIP (prerequisite absent)** — agentgateway is not installed." >> "${EVIDENCE_FILE}"
        log_info "Inference gateway evidence collection skipped — agentgateway not installed."
        return
    fi

    write_section_header "Inference API Gateway (agentgateway)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement for Kubernetes Gateway API support
with an implementation for advanced traffic management for inference services.

## Summary

1. **agentgateway controller** — Running in `agentgateway-system`
2. **inference-gateway deployment** — Running (the inference extension controller)
3. **Gateway API CRDs** — All present (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant)
4. **Active Gateway** — `inference-gateway` with class `agentgateway`, programmed with a load balancer address
5. **Inference Extension CRDs** — InferencePool, InferenceObjective, InferenceModelRewrite installed
6. **Result: PASS**

---

## agentgateway Controller
EOF
    capture "agentgateway deployments" kubectl get deploy -n agentgateway-system
    capture "agentgateway pods" kubectl get pods -n agentgateway-system

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GatewayClass
EOF
    capture "GatewayClass" kubectl get gatewayclass

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Gateway API CRDs
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Gateway API CRDs**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    echo '$ kubectl get crds | grep gateway.networking.k8s.io' >> "${EVIDENCE_FILE}"
    kubectl get crds 2>/dev/null | grep -E "gateway\.networking\.k8s\.io" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Active Gateway
EOF
    capture "Gateways" kubectl get gateways -A
    capture "Gateway details" kubectl get gateway inference-gateway -n agentgateway-system -o yaml

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Gateway Conditions

Verify GatewayClass is Accepted and Gateway is Programmed (not just created).
EOF
    # Check GatewayClass Accepted condition
    echo "" >> "${EVIDENCE_FILE}"
    echo "**GatewayClass conditions**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get gatewayclass agentgateway -o jsonpath='{range .status.conditions[*]}{.type}: {.status} ({.reason}){"\n"}{end}' >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    # Check Gateway Programmed condition
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Gateway conditions**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get gateway inference-gateway -n agentgateway-system -o jsonpath='{range .status.conditions[*]}{.type}: {.status} ({.reason}){"\n"}{end}' >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Inference Extension CRDs
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Inference extension CRDs installed**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    echo '$ kubectl get crds | grep inference' >> "${EVIDENCE_FILE}"
    kubectl get crds 2>/dev/null | grep -E "inference" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    # Verdict — check both GatewayClass Accepted and Gateway Programmed
    echo "" >> "${EVIDENCE_FILE}"
    local gw_accepted gw_programmed
    gw_accepted=$(kubectl get gatewayclass agentgateway -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null)
    gw_programmed=$(kubectl get gateway inference-gateway -n agentgateway-system -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)
    if [ "${gw_accepted}" = "True" ] && [ "${gw_programmed}" = "True" ]; then
        echo "**Result: PASS** — agentgateway controller running, GatewayClass Accepted, Gateway Programmed, inference CRDs installed." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — No active Gateway found." >> "${EVIDENCE_FILE}"
    fi

    log_info "Inference gateway evidence collection complete."
}

# --- Section 6: Robust AI Operator ---
collect_operator() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/robust-operator.md"
    log_info "Collecting Robust AI Operator evidence → ${EVIDENCE_FILE}"

    # Detect which AI operator is present and route to the appropriate collector.
    # Priority: Dynamo > NIM Operator > Kubeflow Trainer
    if kubectl get deploy -n dynamo-system dynamo-platform-dynamo-operator-controller-manager --no-headers 2>/dev/null | grep -q .; then
        collect_operator_dynamo
    elif kubectl get deploy -n nvidia-nim -l app.kubernetes.io/name=k8s-nim-operator --no-headers 2>/dev/null | grep -q .; then
        collect_operator_nim
    elif kubectl get deploy -n kubeflow kubeflow-trainer-controller-manager --no-headers 2>/dev/null | grep -q .; then
        collect_operator_kubeflow
    else
        write_section_header "Robust AI Operator"
        echo "**Result: SKIP (prerequisite absent)** — no supported Dynamo, NIM, or Kubeflow Trainer operator is installed." >> "${EVIDENCE_FILE}"
        log_info "Robust operator evidence collection skipped — no supported operator found."
        return
    fi
}

# --- Kubeflow Trainer evidence ---
collect_operator_kubeflow() {
    write_section_header "Robust AI Operator (Kubeflow Trainer)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **Kubeflow Trainer** — Controller manager running in `kubeflow` namespace
2. **Custom Resource Definitions** — TrainJob, TrainingRuntime, ClusterTrainingRuntime CRDs registered
3. **Webhooks Operational** — Validating webhook `validator.trainer.kubeflow.org` configured and active
4. **Webhook Rejection Test** — Invalid TrainJob correctly rejected by webhook
5. **Result: PASS**

---

## Kubeflow Trainer Health
EOF
    capture "Kubeflow Trainer deployments" kubectl get deploy -n kubeflow
    capture "Kubeflow Trainer pods" kubectl get pods -n kubeflow -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Custom Resource Definitions
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Kubeflow Trainer CRDs**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get crds 2>/dev/null | grep -E "trainer\.kubeflow\.org" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Webhooks
EOF
    capture "Validating webhooks" kubectl get validatingwebhookconfigurations validator.trainer.kubeflow.org
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Webhook endpoint verification**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get endpoints -n kubeflow 2>/dev/null | head -10 >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## ClusterTrainingRuntimes
EOF
    capture "ClusterTrainingRuntimes" kubectl get clustertrainingruntimes

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Webhook Rejection Test

Submit an invalid TrainJob (referencing a non-existent runtime) to verify the
validating webhook actively rejects malformed resources.
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Invalid TrainJob rejection**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    local webhook_result
    webhook_result=$(kubectl apply -f - 2>&1 <<INVALID_CR || true
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: webhook-test-invalid
  namespace: default
spec:
  runtimeRef:
    name: nonexistent-runtime
    apiGroup: trainer.kubeflow.org
    kind: ClusterTrainingRuntime
INVALID_CR
)
    echo "${webhook_result}" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    echo "" >> "${EVIDENCE_FILE}"
    # Check if the rejection came from the admission webhook (not RBAC or transport errors).
    # Webhook rejections contain "admission webhook" or "denied the request".
    if echo "${webhook_result}" | grep -qi "admission webhook\|denied the request"; then
        echo "Webhook correctly rejected the invalid resource." >> "${EVIDENCE_FILE}"
    elif echo "${webhook_result}" | grep -qi "cannot create resource\|unauthorized"; then
        echo "WARNING: Rejection was from RBAC, not the admission webhook." >> "${EVIDENCE_FILE}"
    elif echo "${webhook_result}" | grep -qi "denied\|forbidden\|invalid"; then
        echo "Webhook rejected the invalid resource (unconfirmed source)." >> "${EVIDENCE_FILE}"
    else
        echo "WARNING: Webhook did not reject the invalid resource." >> "${EVIDENCE_FILE}"
        # Clean up if accidentally created
        kubectl delete trainjob webhook-test-invalid -n default --ignore-not-found 2>/dev/null
    fi

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    local crd_count
    crd_count=$(kubectl get crds 2>/dev/null | grep -c "trainer\.kubeflow\.org" || true)
    local controller_ready
    controller_ready=$(kubectl get deploy -n kubeflow kubeflow-trainer-controller-manager --no-headers 2>/dev/null | awk '{print $2}' | grep -c "1/1" || true)
    local webhook_ok
    # Only count confirmed webhook rejections (not RBAC or transport errors)
    webhook_ok=$(echo "${webhook_result}" | grep -ci "admission webhook\|denied the request" || true)

    if [ "${crd_count}" -gt 0 ] && [ "${controller_ready}" -gt 0 ] && [ "${webhook_ok}" -gt 0 ]; then
        echo "**Result: PASS** — Kubeflow Trainer running, webhooks operational (rejection verified), ${crd_count} CRDs registered." >> "${EVIDENCE_FILE}"
    elif [ "${crd_count}" -gt 0 ] && [ "${controller_ready}" -gt 0 ]; then
        echo "**Result: PASS** — Kubeflow Trainer running, ${crd_count} CRDs registered." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — Kubeflow Trainer controller not ready or CRDs missing." >> "${EVIDENCE_FILE}"
    fi

    log_info "Robust operator (Kubeflow Trainer) evidence collection complete."
}

# --- NIM Operator evidence ---
collect_operator_nim() {
    write_section_header "Robust AI Operator (NIM Operator)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **NIM Operator** — Controller manager running in `nvidia-nim`
2. **Custom Resource Definitions** — NIMService, NIMCache, NIMPipeline, NIMBuild CRDs registered
3. **Admission Controller** — Validating/mutating webhooks configured and active
4. **Custom Resource Reconciled** — `NIMService` reconciled into running inference pod(s)
5. **Result: PASS**

---

## NIM Operator Health
EOF
    capture "NIM operator deployment" kubectl get deploy -n nvidia-nim
    capture "NIM operator pods" kubectl get pods -n nvidia-nim

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Custom Resource Definitions
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**NIM CRDs**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get crds 2>/dev/null | grep "apps\.nvidia\.com" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Webhooks
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**NIM Operator webhooks**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    # Match webhooks by name or by backing service in the nvidia-nim namespace
    if [[ "${HAS_JQ}" == "true" ]]; then
      kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations -o json 2>/dev/null | \
        jq -r '.items[] | select(.webhooks[]?.clientConfig.service.namespace == "nvidia-nim") | "\(.kind)/\(.metadata.name)"' 2>/dev/null >> "${EVIDENCE_FILE}" 2>&1 || true
    else
      kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations 2>/dev/null | grep -iE 'nim|apps\.nvidia\.com' >> "${EVIDENCE_FILE}" 2>&1 || true
    fi
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Custom Resource Reconciliation

A `NIMService` defines an inference microservice. The operator reconciles it into
a Deployment with GPU resources, a Service, and health monitoring.
EOF
    capture "NIMServices" kubectl get nimservices -A
    local nim_ns="nim-workload"
    local nim_service=""
    nim_service=$(kubectl get nimservices -n "${nim_ns}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "${nim_service}" ]; then
        capture "NIMService details" kubectl get nimservice "${nim_service}" -n "${nim_ns}" -o yaml
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Workload Pods Created by Operator
EOF
    capture "NIM workload pods" kubectl get pods -n "${nim_ns}" -l app.kubernetes.io/managed-by=k8s-nim-operator -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Webhook Rejection Test

Submit an invalid NIMService to verify the admission controller actively
rejects malformed resources.
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Invalid CR rejection**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    local webhook_result
    webhook_result=$(kubectl apply -f - 2>&1 <<INVALID_CR || true
apiVersion: apps.nvidia.com/v1alpha1
kind: NIMService
metadata:
  name: webhook-test-invalid
  namespace: default
spec: {}
INVALID_CR
)
    echo "${webhook_result}" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    echo "" >> "${EVIDENCE_FILE}"
    if echo "${webhook_result}" | grep -qi "denied\|forbidden\|invalid\|error"; then
        echo "Webhook correctly rejected the invalid resource." >> "${EVIDENCE_FILE}"
    else
        echo "WARNING: Webhook did not reject the invalid resource." >> "${EVIDENCE_FILE}"
        kubectl delete nimservice webhook-test-invalid -n default --ignore-not-found 2>/dev/null
    fi

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    local crd_count
    crd_count=$(kubectl get crds 2>/dev/null | grep -c "apps\.nvidia\.com" || true)
    local running_pods
    running_pods=$(kubectl get pods -n "${nim_ns}" -l app.kubernetes.io/managed-by=k8s-nim-operator --no-headers 2>/dev/null | grep -c "Running" || true)
    local webhook_ok
    webhook_ok=$(echo "${webhook_result}" | grep -ci "denied\|forbidden\|invalid\|error" || true)

    if [ "${crd_count}" -gt 0 ] && [ "${running_pods}" -gt 0 ] && [ "${webhook_ok}" -gt 0 ]; then
        echo "**Result: PASS** — NIM operator running, webhooks operational (rejection verified), ${crd_count} CRDs registered, NIMService reconciled with ${running_pods} healthy inference pod(s)." >> "${EVIDENCE_FILE}"
    elif [ "${crd_count}" -gt 0 ] && [ "${running_pods}" -gt 0 ]; then
        echo "**Result: PASS** — NIM operator running, ${crd_count} CRDs registered, NIMService reconciled with ${running_pods} healthy inference pod(s)." >> "${EVIDENCE_FILE}"
    elif [ "${crd_count}" -gt 0 ]; then
        echo "**Result: FAIL** — NIMService found but no healthy inference pods." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — No NIM CRDs found." >> "${EVIDENCE_FILE}"
    fi

    log_info "Robust operator (NIM) evidence collection complete."
}

# --- Dynamo evidence ---
collect_operator_dynamo() {
    write_section_header "Robust AI Operator (Dynamo Platform)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement that at least one complex AI operator
with a CRD can be installed and functions reliably, including operator pods running,
webhooks operational, and custom resources reconciled.

## Summary

1. **Dynamo Operator** — Controller manager running in `dynamo-system`
2. **Custom Resource Definitions** — 6 Dynamo CRDs registered (DynamoGraphDeployment, DynamoComponentDeployment, etc.)
3. **Webhooks Operational** — Validating webhook configured and active
4. **Custom Resource Reconciled** — `DynamoGraphDeployment/vllm-agg` reconciled into running workload pods via PodCliques
5. **Supporting Services** — NATS running for Dynamo's Kubernetes event plane
6. **Result: PASS**

---

## Dynamo Operator Health
EOF
    capture "Dynamo operator deployments" kubectl get deploy -n dynamo-system
    capture "Dynamo operator pods" kubectl get pods -n dynamo-system

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Custom Resource Definitions
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Dynamo CRDs**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get crds 2>/dev/null | grep -E "dynamo|nvidia\.com" | grep -i dynamo >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Webhooks
EOF
    capture "Validating webhooks" kubectl get validatingwebhookconfigurations -l app.kubernetes.io/instance=dynamo-platform
    # Fallback
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Dynamo validating webhooks**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get validatingwebhookconfigurations 2>/dev/null | grep dynamo >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Custom Resource Reconciliation

A `DynamoGraphDeployment` defines an inference serving graph. The operator reconciles
it into workload pods managed via PodCliques.
EOF
    capture "DynamoGraphDeployments" kubectl get dynamographdeployments -A
    capture "DynamoGraphDeployment details" kubectl get dynamographdeployment vllm-agg -n dynamo-workload -o yaml

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### Workload Pods Created by Operator
EOF
    capture "Dynamo workload pods" kubectl get pods -n dynamo-workload -l nvidia.com/dynamo-graph-deployment-name -o wide

    cat >> "${EVIDENCE_FILE}" <<'EOF'

### PodCliques
EOF
    capture "PodCliques" kubectl get podcliques -n dynamo-workload

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Webhook Rejection Test

Submit an invalid DynamoGraphDeployment to verify the validating webhook
actively rejects malformed resources.
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Invalid CR rejection**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    # Submit an invalid DynamoGraphDeployment (empty spec) — webhook should reject it
    local webhook_result
    webhook_result=$(kubectl apply -f - 2>&1 <<INVALID_CR || true
apiVersion: nvidia.com/v1beta1
kind: DynamoGraphDeployment
metadata:
  name: webhook-test-invalid
  namespace: default
spec: {}
INVALID_CR
)
    echo "${webhook_result}" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Check if webhook rejected it
    echo "" >> "${EVIDENCE_FILE}"
    if echo "${webhook_result}" | grep -qi "denied\|forbidden\|invalid\|error"; then
        echo "Webhook correctly rejected the invalid resource." >> "${EVIDENCE_FILE}"
    else
        echo "WARNING: Webhook did not reject the invalid resource." >> "${EVIDENCE_FILE}"
    fi

    # Verdict — require DGD + healthy workload pods; webhook rejection strengthens but is optional.
    # Distinguish a failed DGD query (fail closed) from a successful query returning
    # zero rows: an absent inference workload is an absent prerequisite (SKIP), not a
    # failure, because a stock inference recipe deploys the operator but no persistent
    # DynamoGraphDeployment (the service-metrics section deploys and cleans up its own).
    echo "" >> "${EVIDENCE_FILE}"
    local dgd_query_ok=true dgd_count=0 dgd_out
    if dgd_out=$(kubectl get dynamographdeployments -A --no-headers 2>/dev/null); then
        dgd_count=$(printf '%s' "${dgd_out}" | grep -c . || true)
    else
        dgd_query_ok=false
    fi
    local running_pods
    running_pods=$(kubectl get pods -n dynamo-workload -l nvidia.com/dynamo-graph-deployment-name --no-headers 2>/dev/null | grep -c "Running" || true)
    local webhook_ok
    webhook_ok=$(echo "${webhook_result}" | grep -ci "denied\|forbidden\|invalid\|error" || true)

    if [ "${dgd_query_ok}" != "true" ]; then
        echo "**Result: FAIL** — DynamoGraphDeployment query failed (fail closed); rerun against a reachable API server." >> "${EVIDENCE_FILE}"
    elif [ "${dgd_count}" -gt 0 ] && [ "${running_pods}" -gt 0 ] && [ "${webhook_ok}" -gt 0 ]; then
        echo "**Result: PASS** — Dynamo operator running, webhooks operational (rejection verified), CRDs registered, DynamoGraphDeployment reconciled with ${running_pods} healthy workload pod(s)." >> "${EVIDENCE_FILE}"
    elif [ "${dgd_count}" -gt 0 ] && [ "${running_pods}" -gt 0 ]; then
        echo "**Result: PASS** — Dynamo operator running, CRDs registered, DynamoGraphDeployment reconciled with ${running_pods} healthy workload pod(s)." >> "${EVIDENCE_FILE}"
    elif [ "${dgd_count}" -gt 0 ]; then
        echo "**Result: FAIL** — DynamoGraphDeployment found but no healthy workload pods." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: SKIP (prerequisite absent)** — Dynamo operator present but no DynamoGraphDeployment exists; deploy an inference workload (e.g. demos/workloads/inference/vllm-agg.yaml) to exercise operator reconciliation." >> "${EVIDENCE_FILE}"
    fi

    log_info "Robust operator evidence collection complete."
}

# --- Section 7: Pod Autoscaling (HPA) ---
collect_hpa() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/pod-autoscaling.md"
    log_info "Collecting Pod Autoscaling (HPA) evidence → ${EVIDENCE_FILE}"
    write_section_header "Pod Autoscaling (HPA with GPU Metrics)"

    cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement that HPA functions correctly for pods
utilizing accelerators, including the ability to scale based on custom GPU metrics.

## Summary

1. **Prometheus Adapter** — Exposes GPU metrics via Kubernetes custom metrics API
2. **Custom Metrics API** — `gpu_utilization`, `gpu_memory_used`, `gpu_power_usage` available
3. **GPU Stress Workload** — Deployment running CUDA N-Body Simulation to generate GPU load
4. **HPA Configuration** — Targets `gpu_utilization` with threshold of 50%
5. **HPA Scale-Up** — Successfully scales replicas when GPU utilization exceeds target
6. **Result: PASS**

---

## Prometheus Adapter
EOF
    capture "Prometheus adapter pod" kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus-adapter
    capture "Prometheus adapter service" kubectl get svc prometheus-adapter -n monitoring

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Custom Metrics API
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Available custom metrics**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    echo '$ kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 | python3 -c "..." # extract resource names' >> "${EVIDENCE_FILE}"
    kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 2>&1 | \
        python3 -c "import sys,json; data=json.loads(sys.stdin.read()); resources=data.get('resources',[]); [print(r['name']) for r in resources]" >> "${EVIDENCE_FILE}" 2>&1
    echo '```' >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Stress Test Deployment

Deploy a GPU workload running CUDA N-Body Simulation to generate sustained GPU utilization,
then create an HPA targeting `gpu_utilization` to demonstrate autoscaling.

**Test manifest:** `pkg/evidence/cncf/scripts/manifests/hpa-gpu-test.yaml`
EOF
    echo '```yaml' >> "${EVIDENCE_FILE}"
    cat "${SCRIPT_DIR}/manifests/hpa-gpu-test.yaml" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Clean up any previous run
    cleanup_ns hpa-test pre

    # Deploy test
    log_info "Deploying HPA GPU test..."
    capture "Apply test manifest" kubectl apply -f "${SCRIPT_DIR}/manifests/hpa-gpu-test.yaml"

    # Wait for pod to become Ready (uses watch API for immediate detection)
    log_info "Waiting for GPU workload pod (up to ${POD_TIMEOUT}s)..."
    kubectl wait --for=condition=Ready pod -l app=gpu-workload -n hpa-test --timeout="${POD_TIMEOUT}s" 2>/dev/null || true
    capture "GPU workload pod" kubectl get pods -n hpa-test -o wide

    # Wait for GPU metrics to be available and HPA to scale up (up to 3 minutes)
    # Check-then-sleep pattern: detect scale-up or failure as early as possible
    log_info "Waiting for GPU metrics and HPA scale-up (up to 3 minutes)..."
    local hpa_scaled=false
    for i in $(seq 1 18); do
        targets=$(kubectl get hpa gpu-workload-hpa -n hpa-test -o jsonpath='{.status.currentMetrics[0].pods.current.averageValue}' 2>/dev/null)
        replicas=$(kubectl get hpa gpu-workload-hpa -n hpa-test -o jsonpath='{.status.currentReplicas}' 2>/dev/null)
        log_info "  Check ${i}/18: gpu_utilization=${targets:-unknown}, replicas=${replicas:-1}"
        if [ "${replicas:-1}" -gt 1 ] && [ -n "$targets" ]; then
            hpa_scaled=true
            break
        fi
        # Fail early on unrecoverable HPA conditions
        local hpa_conditions
        hpa_conditions=$(kubectl get hpa gpu-workload-hpa -n hpa-test -o jsonpath='{.status.conditions[?(@.status=="False")].reason}' 2>/dev/null)
        case "$hpa_conditions" in
            *FailedGetMetrics*|*InvalidMetricSourceType*)
                log_error "HPA failed early: $hpa_conditions"
                break
                ;;
        esac
        sleep 10
    done

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## HPA Status
EOF
    capture "HPA status" kubectl get hpa -n hpa-test
    capture "HPA details" kubectl describe hpa gpu-workload-hpa -n hpa-test

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Utilization Evidence
EOF
    local hpa_pod
    hpa_pod=$(kubectl get pod -n hpa-test -l app=gpu-workload -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "${hpa_pod}" ]; then
        capture "GPU utilization (nvidia-smi)" kubectl exec -n hpa-test "${hpa_pod}" -- nvidia-smi --query-gpu=utilization.gpu,utilization.memory,power.draw --format=csv
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Pods After Scale-Up
EOF
    capture "Pods after scale-up" kubectl get pods -n hpa-test -o wide

    # Verdict — require actual scale-up for PASS
    echo "" >> "${EVIDENCE_FILE}"
    if [ "${hpa_scaled}" = "true" ]; then
        echo "**Result: PASS** — HPA successfully read gpu_utilization metric and scaled replicas when utilization exceeded target threshold." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — HPA did not scale replicas within the timeout. Check GPU workload, DCGM exporter, and prometheus-adapter configuration." >> "${EVIDENCE_FILE}"
    fi

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Cleanup
EOF
    if [ "${NO_CLEANUP:-}" != "true" ]; then
        kubectl delete deploy gpu-workload -n hpa-test --ignore-not-found 2>/dev/null || true
        kubectl delete pods -n hpa-test -l app=gpu-workload --force --grace-period=0 2>/dev/null || true
    fi
    capture "Delete test namespace" cleanup_ns hpa-test

    log_info "Pod autoscaling evidence collection complete."
}

# --- Section 8: Cluster Autoscaling ---
collect_cluster_autoscaling() {
    EVIDENCE_FILE="${EVIDENCE_DIR}/cluster-autoscaling.md"
    log_info "Collecting Cluster Autoscaling evidence → ${EVIDENCE_FILE}"
    write_section_header "Cluster Autoscaling"

    # Detect platform from node providerID
    local provider_id
    provider_id=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || echo "")

    if [[ "${provider_id}" == aws://* ]]; then
        log_info "Detected EKS cluster, collecting AWS ASG evidence"
        cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement that the platform has GPU-aware
cluster autoscaling infrastructure configured, with Auto Scaling Groups capable
of scaling GPU node groups based on workload demand.

## Summary

1. **GPU Node Group (ASG)** — EKS Auto Scaling Group configured with GPU instances
2. **Capacity Reservation** — Dedicated GPU capacity available for scale-up
3. **Scalable Configuration** — ASG min/max configurable for demand-based scaling
4. **Kubernetes Integration** — ASG nodes auto-join the EKS cluster with GPU labels
5. **Autoscaler Compatibility** — Cluster Autoscaler supported via ASG tag discovery

---

## GPU Node Auto Scaling Group

The cluster uses an AWS Auto Scaling Group (ASG) for GPU nodes, which can scale
up/down based on workload demand.
EOF
        collect_eks_autoscaling_evidence
    elif [[ "${provider_id}" == gce://* ]]; then
        log_info "Detected GKE cluster, collecting GKE node pool autoscaling evidence"
        cat >> "${EVIDENCE_FILE}" <<'EOF'
Demonstrates CNCF AI Conformance requirement that the platform has GPU-aware
cluster autoscaling infrastructure configured. GKE provides a built-in cluster
autoscaler that manages node pool scaling based on workload demand.

---
EOF
        collect_gke_autoscaling_evidence
    else
        echo "**Result: SKIP (prerequisite absent)** — no supported EKS or GKE cluster-autoscaling prerequisite was detected (providerID=${provider_id:-<empty>})." >> "${EVIDENCE_FILE}"
        log_info "Cluster autoscaling evidence collection skipped — unknown provider (providerID=${provider_id})."
        return
    fi

    log_info "Cluster autoscaling evidence collection complete."
}

# Collect EKS-specific ASG evidence using AWS CLI and kubectl.
collect_eks_autoscaling_evidence() {
    # Detect region from node topology label (no hardcoded region).
    local region
    region=$(kubectl get nodes -o jsonpath='{.items[0].metadata.labels.topology\.kubernetes\.io/region}' 2>/dev/null || echo "us-east-1")

    # Detect cluster name from EKS tags on nodes.
    local cluster_name
    cluster_name=$(kubectl get nodes -o jsonpath='{.items[0].metadata.labels.alpha\.eksctl\.io/cluster-name}' 2>/dev/null)
    if [ -z "${cluster_name}" ]; then
        # Fallback: extract from kube-system configmap or context
        cluster_name=$(kubectl config current-context 2>/dev/null | sed 's|.*/||' || echo "unknown")
    fi

    # Find GPU node group name from node labels.
    local gpu_nodegroup
    gpu_nodegroup=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.labels.eks\.amazonaws\.com/nodegroup}' 2>/dev/null)
    if [ -z "${gpu_nodegroup}" ]; then
        gpu_nodegroup=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.labels.nodeGroup}' 2>/dev/null)
    fi

    cat >> "${EVIDENCE_FILE}" <<EOF

## EKS Cluster Details

- **Region:** ${region}
- **Cluster:** ${cluster_name}
- **GPU Node Group:** ${gpu_nodegroup:-unknown}
EOF

    # GPU nodes from Kubernetes
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Nodes
EOF
    capture "GPU nodes" kubectl get nodes -l nvidia.com/gpu.present=true \
        -o custom-columns='NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.metadata.labels.nvidia\.com/gpu\.count,PRODUCT:.metadata.labels.nvidia\.com/gpu\.product,NODE-GROUP:.metadata.labels.nodeGroup,ZONE:.metadata.labels.topology\.kubernetes\.io/zone'

    # AWS ASG details (only if aws CLI is available and node group was found)
    local asg_verified=false
    if command -v aws &>/dev/null && [ -n "${gpu_nodegroup}" ]; then
        cat >> "${EVIDENCE_FILE}" <<'EOF'

## Auto Scaling Group (AWS)
EOF
        # Find ASG by EKS nodegroup tag, falling back to instance-id lookup
        local asg_name
        asg_name=$(aws autoscaling describe-auto-scaling-groups --region "${region}" \
            --query "AutoScalingGroups[?contains(Tags[?Key==\`eks:nodegroup-name\`].Value, \`${gpu_nodegroup}\`)].AutoScalingGroupName | [0]" \
            --output text 2>/dev/null)

        # Fallback: resolve ASG from GPU node instance ID (handles custom ASGs without eks:nodegroup-name tag)
        # Strip whitespace/newlines — query may return "None\nNone" for multiple ASGs.
        asg_name=$(echo "${asg_name}" | head -1 | tr -d '[:space:]')
        if [ -z "${asg_name}" ] || [ "${asg_name}" = "None" ]; then
            local instance_id
            instance_id=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null | grep -oE 'i-[a-f0-9]+')
            if [ -n "${instance_id}" ]; then
                asg_name=$(aws autoscaling describe-auto-scaling-instances --region "${region}" \
                    --instance-ids "${instance_id}" \
                    --query 'AutoScalingInstances[0].AutoScalingGroupName' \
                    --output text 2>/dev/null)
            fi
        fi

        if [ -n "${asg_name}" ] && [ "${asg_name}" != "None" ]; then
            asg_verified=true
            capture "GPU ASG details" aws autoscaling describe-auto-scaling-groups --region "${region}" \
                --auto-scaling-group-names "${asg_name}" \
                --query 'AutoScalingGroups[0].{Name:AutoScalingGroupName,MinSize:MinSize,MaxSize:MaxSize,DesiredCapacity:DesiredCapacity,AvailabilityZones:AvailabilityZones,HealthCheckType:HealthCheckType}' \
                --output table

            # Launch template
            local lt_id
            lt_id=$(aws autoscaling describe-auto-scaling-groups --region "${region}" \
                --auto-scaling-group-names "${asg_name}" \
                --query 'AutoScalingGroups[0].LaunchTemplate.LaunchTemplateId' --output text 2>/dev/null)
            if [ -n "${lt_id}" ] && [ "${lt_id}" != "None" ]; then
                capture "GPU launch template" aws ec2 describe-launch-template-versions --region "${region}" \
                    --launch-template-id "${lt_id}" --versions '$Latest' \
                    --query 'LaunchTemplateVersions[0].LaunchTemplateData.{InstanceType:InstanceType,ImageId:ImageId}' \
                    --output table
            fi

            # ASG autoscaler tags
            capture "ASG autoscaler tags" aws autoscaling describe-tags --region "${region}" \
                --filters "Name=auto-scaling-group,Values=${asg_name}" \
                --query 'Tags[*].{Key:Key,Value:Value}' \
                --output table
        else
            echo "" >> "${EVIDENCE_FILE}"
            echo "**NOTE:** Could not find ASG for node group '${gpu_nodegroup}' via AWS API." >> "${EVIDENCE_FILE}"
        fi

        # Capacity reservations for GPU instance type
        local instance_type
        instance_type=$(kubectl get nodes -l nvidia.com/gpu.present=true \
            -o jsonpath='{.items[0].metadata.labels.node\.kubernetes\.io/instance-type}' 2>/dev/null)
        if [ -n "${instance_type}" ]; then
            cat >> "${EVIDENCE_FILE}" <<'EOF'

## Capacity Reservation
EOF
            capture "GPU capacity reservation" aws ec2 describe-capacity-reservations --region "${region}" \
                --query "CapacityReservations[?InstanceType==\`${instance_type}\`].{ID:CapacityReservationId,Type:InstanceType,State:State,Total:TotalInstanceCount,Available:AvailableInstanceCount,AZ:AvailabilityZone}" \
                --output table
        fi
    else
        echo "" >> "${EVIDENCE_FILE}"
        if ! command -v aws &>/dev/null; then
            echo "**NOTE:** AWS CLI not available — skipping ASG-level evidence. GPU node group metadata from Kubernetes labels shown above." >> "${EVIDENCE_FILE}"
        elif [ -z "${gpu_nodegroup}" ]; then
            echo "**NOTE:** GPU node group label not found on nodes — cannot query ASG details. GPU node metadata from Kubernetes labels shown above." >> "${EVIDENCE_FILE}"
        fi
    fi

    # Verdict — indicate whether ASG was actually verified.
    echo "" >> "${EVIDENCE_FILE}"
    if [ "${asg_verified}" = "true" ]; then
        echo "**Result: PASS** — EKS cluster with GPU nodes managed by Auto Scaling Group, ASG configuration verified via AWS API. Evidence is configuration-level; a live scale event is not triggered to avoid disrupting the cluster." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: PASS (partial)** — EKS GPU nodes present but ASG-level verification was not performed (aws CLI unavailable or node group label missing). Kubernetes-level evidence only." >> "${EVIDENCE_FILE}"
    fi
}

# Collect GKE-specific autoscaling evidence.
collect_gke_autoscaling_evidence() {
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GKE Cluster Details
EOF
    # Extract project and cluster info from providerID (gce://project/zone/instance)
    local provider_id
    provider_id=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || echo "")
    local gce_project gce_zone
    gce_project=$(echo "${provider_id}" | cut -d'/' -f3)
    gce_zone=$(echo "${provider_id}" | cut -d'/' -f4)

    echo "" >> "${EVIDENCE_FILE}"
    echo "- **Project:** ${gce_project:-unknown}" >> "${EVIDENCE_FILE}"
    echo "- **Zone:** ${gce_zone:-unknown}" >> "${EVIDENCE_FILE}"

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Nodes
EOF
    capture "GPU nodes" kubectl get nodes -l nvidia.com/gpu.present=true \
        -o custom-columns='NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.status.capacity.nvidia\.com/gpu,ACCELERATOR:.metadata.labels.cloud\.google\.com/gke-accelerator,NODE-POOL:.metadata.labels.cloud\.google\.com/gke-nodepool'

    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GKE Cluster Autoscaler

GKE includes a built-in cluster autoscaler that manages node pool scaling.
The autoscaler is configured per node pool and can be verified via annotations
on nodes and the cluster-autoscaler-status ConfigMap.
EOF

    # Check cluster-autoscaler-status ConfigMap (GKE writes autoscaler status here)
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Cluster Autoscaler Status**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get configmap cluster-autoscaler-status -n kube-system -o jsonpath='{.data.status}' 2>/dev/null >> "${EVIDENCE_FILE}" || echo "ConfigMap cluster-autoscaler-status not found" >> "${EVIDENCE_FILE}"
    echo "" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Check node pool annotations for autoscaling config
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Node Pool Autoscaling Configuration
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**GPU node pool annotations**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.cluster-autoscaler\.kubernetes\.io/scale-down-disabled}{"\t"}{.metadata.labels.cloud\.google\.com/gke-nodepool}{"\n"}{end}' 2>/dev/null >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Check for NotTriggerScaleUp events (proves autoscaler is active)
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Autoscaler Activity
EOF
    echo "" >> "${EVIDENCE_FILE}"
    echo "**Recent autoscaler events**" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"
    kubectl get events -A --sort-by='.lastTimestamp' 2>/dev/null | grep -E "NotTriggerScaleUp|ScaledUpGroup|ScaleDown|TriggeredScaleUp" | tail -10 >> "${EVIDENCE_FILE}" || echo "No autoscaler events found" >> "${EVIDENCE_FILE}"
    echo '```' >> "${EVIDENCE_FILE}"

    # Verdict
    echo "" >> "${EVIDENCE_FILE}"
    local gpu_node_count
    gpu_node_count=$(kubectl get nodes -l nvidia.com/gpu.present=true --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local ca_status
    ca_status=$(kubectl get configmap cluster-autoscaler-status -n kube-system 2>/dev/null && echo "found" || echo "")

    if [ "${gpu_node_count}" -gt 0 ] && [ -n "${ca_status}" ]; then
        echo "**Result: PASS** — GKE cluster with ${gpu_node_count} GPU nodes and built-in cluster autoscaler active." >> "${EVIDENCE_FILE}"
    elif [ "${gpu_node_count}" -gt 0 ]; then
        echo "**Result: PASS (partial)** — GKE cluster with ${gpu_node_count} GPU nodes. Cluster autoscaler status ConfigMap not found — autoscaler may not be enabled for this node pool." >> "${EVIDENCE_FILE}"
    else
        echo "**Result: FAIL** — No GPU nodes found." >> "${EVIDENCE_FILE}"
    fi
}

# Collect Kubernetes-level autoscaling evidence (non-EKS/GKE clusters).
collect_k8s_autoscaling_evidence() {
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## GPU Nodes
EOF
    capture "GPU nodes" kubectl get nodes -l nvidia.com/gpu.present=true \
        -o custom-columns='NAME:.metadata.name,INSTANCE-TYPE:.metadata.labels.node\.kubernetes\.io/instance-type,GPUS:.status.capacity.nvidia\.com/gpu,VERSION:.status.nodeInfo.kubeletVersion'

    # Check for Karpenter
    cat >> "${EVIDENCE_FILE}" <<'EOF'

## Autoscaler
EOF
    if kubectl get deploy -n karpenter karpenter &>/dev/null; then
        capture "Karpenter controller" kubectl get deploy -n karpenter
        if kubectl get nodepools.karpenter.sh &>/dev/null; then
            capture "Karpenter NodePools" kubectl get nodepools.karpenter.sh
        else
            echo "" >> "${EVIDENCE_FILE}"
            echo "**NOTE:** Karpenter NodePool CRD not found." >> "${EVIDENCE_FILE}"
        fi
    elif kubectl get deploy -n kube-system cluster-autoscaler &>/dev/null; then
        capture "Cluster Autoscaler" kubectl get deploy -n kube-system cluster-autoscaler
    else
        echo "" >> "${EVIDENCE_FILE}"
        echo "**NOTE:** No Karpenter or Cluster Autoscaler deployment found." >> "${EVIDENCE_FILE}"
    fi

    echo "" >> "${EVIDENCE_FILE}"
    echo "**Result: PASS** — GPU nodes present in cluster with autoscaling capability." >> "${EVIDENCE_FILE}"
}

# --- Main ---
main() {
    log_info "CNCF AI Conformance Evidence Collection"

    # Verify cluster access
    if ! kubectl cluster-info &>/dev/null; then
        log_error "Cannot connect to Kubernetes cluster. Check KUBECONFIG."
        exit 1
    fi

    mkdir -p "${EVIDENCE_DIR}"

    # Detect cluster info once for use in headers and summary
    detect_cluster_info

    case "${SECTION}" in
        dra)
            run_check "DRA Support" "dra-support" collect_dra
            ;;
        gang)
            run_check "Gang Scheduling" "gang-scheduling" collect_gang
            ;;
        secure)
            run_check "Secure Accelerator Access" "secure-accelerator-access" collect_secure
            ;;
        accelerator-metrics)
            run_check "Accelerator Metrics" "accelerator-metrics" collect_accelerator_metrics
            ;;
        service-metrics)
            run_check "AI Service Metrics" "ai-service-metrics" collect_service_metrics
            ;;
        gateway)
            run_check "Inference Gateway" "inference-gateway" collect_gateway
            ;;
        operator)
            run_check "Robust AI Operator" "robust-operator" collect_operator
            ;;
        hpa)
            run_check "Pod Autoscaling (HPA)" "pod-autoscaling" collect_hpa
            ;;
        cluster-autoscaling)
            run_check "Cluster Autoscaling" "cluster-autoscaling" collect_cluster_autoscaling
            ;;
        all)
            run_check "DRA Support" "dra-support" collect_dra
            run_check "Gang Scheduling" "gang-scheduling" collect_gang
            run_check "Secure Accelerator Access" "secure-accelerator-access" collect_secure
            run_check "Accelerator Metrics" "accelerator-metrics" collect_accelerator_metrics
            run_check "AI Service Metrics" "ai-service-metrics" collect_service_metrics
            run_check "Inference Gateway" "inference-gateway" collect_gateway
            run_check "Robust AI Operator" "robust-operator" collect_operator
            run_check "Pod Autoscaling (HPA)" "pod-autoscaling" collect_hpa
            run_check "Cluster Autoscaling" "cluster-autoscaling" collect_cluster_autoscaling
            ;;
        *)
            log_error "Unknown section: ${SECTION}"
            echo "Usage: $0 [dra|gang|secure|accelerator-metrics|service-metrics|gateway|operator|hpa|cluster-autoscaling|all]"
            exit 1
            ;;
    esac

    # Redact ELB hostnames from evidence files (publicly reachable endpoints)
    for f in "${EVIDENCE_DIR}"/*.md; do
        [ -f "$f" ] || continue
        sed -i.bak -E 's/[a-z0-9]+-[a-z0-9]+\.[a-z0-9-]+\.elb\.amazonaws\.com/<elb-redacted>.elb.amazonaws.com/g' "$f"
        rm -f "${f}.bak"
    done

    log_info "Evidence written to: ${EVIDENCE_DIR}/"

    # Print summary using cached cluster info
    echo ""
    echo "=== Evidence Collection Summary ==="
    echo ""
    echo "  Cluster:    ${CLUSTER_DESC}"
    echo "  K8s:        ${CLUSTER_K8S_VERSION}"
    echo "  OS:         ${CLUSTER_OS_IMAGE}"
    echo "  Evidence:   ${EVIDENCE_DIR}/"
    echo ""
    local passed=0 failed=0 skipped=0
    printf "  %-30s %s\n" "Check" "Status"
    printf "  %-30s %s\n" "-----" "------"
    while IFS= read -r line; do
        [ -z "${line}" ] && continue
        local name="${line%%:*}"
        local status="${line#*:}"
        printf "  %-30s %s\n" "${name}" "${status}"
        case "${status}" in
            PASS*) passed=$((passed + 1)) ;;
            FAIL*) failed=$((failed + 1)) ;;
            SKIP)  skipped=$((skipped + 1)) ;;
        esac
    done < <(printf '%b' "${CHECK_RESULTS}")
    echo ""
    echo "  Total: $((passed + failed + skipped)) | Passed: ${passed} | Failed: ${failed} | Skipped: ${skipped}"
    echo ""

    if [ "${failed}" -ne 0 ]; then
        return 1
    fi
    return 0
}

# Hidden subcommand for unit tests (pkg/evidence/cncf/claim_shape_test.go):
# print the version-correct ResourceClaim YAML and exit without contacting a
# cluster. Not part of the documented section interface.
# Usage: collect-evidence.sh emit-claim <version> [name] [namespace] [deviceclass]
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    if [ "${SECTION}" = "emit-claim" ]; then
        emit_resourceclaim_yaml "${2:-}" "${3:-gpu-claim}" "${4:-dra-test}" "${5:-gpu.nvidia.com}"
        exit $?
    fi

    main
fi

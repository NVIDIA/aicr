#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

validate_duration_input() {
  local input_name="$1"
  local input_value="$2"

  if ! [[ "${input_value}" =~ ^[0-9]+[smh]$ ]]; then
    echo "::error::${input_name} must be a duration like 300s, 10m, or 1h; got '${input_value}'"
    exit 1
  fi
}

validate_generated_control_plane_config() {
  if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" ]]; then
    for patch_file in "${patch_dir}"/*.yaml; do
      if ! grep -Fxq 'apiVersion: v1' "${patch_file}" ||
        ! grep -Fxq 'kind: Pod' "${patch_file}" ||
        ! grep -Eq '^[[:space:]]+resources:$' "${patch_file}"; then
        echo "::error::rendered static pod patch ${patch_file} is missing expected top-level YAML"
        sed 's/^/  /' "${patch_file}" || true
        exit 1
      fi
    done

    if ! grep -Eq '^[[:space:]]*extraMounts:$' "${config_template}" ||
      ! grep -Fq 'directory: /patches' "${config_template}"; then
      echo "::error::rendered kind config is missing control-plane patch mounts"
      sed 's/^/  /' "${config_template}" || true
      exit 1
    fi
  fi

  if [[ "${CONTROL_PLANE_LEADER_ELECTION_TUNING}" == "true" ]]; then
    for expected in \
      'apiVersion: kubeadm.k8s.io/v1beta3' \
      'apiVersion: kubeadm.k8s.io/v1beta4' \
      "leader-elect-lease-duration: \"${LEADER_ELECTION_LEASE_DURATION}\"" \
      "leader-elect-renew-deadline: \"${LEADER_ELECTION_RENEW_DEADLINE}\"" \
      "leader-elect-retry-period: \"${LEADER_ELECTION_RETRY_PERIOD}\"" \
      "value: \"${LEADER_ELECTION_LEASE_DURATION}\"" \
      "value: \"${LEADER_ELECTION_RENEW_DEADLINE}\"" \
      "value: \"${LEADER_ELECTION_RETRY_PERIOD}\""; do
      if ! grep -Fq "${expected}" "${config_template}"; then
        echo "::error::rendered kind config is missing expected leader election setting: ${expected}"
        sed 's/^/  /' "${config_template}" || true
        exit 1
      fi
    done
  fi
}

validate_duration_input cluster_create_timeout "${CLUSTER_CREATE_TIMEOUT}"
validate_duration_input leader_election_lease_duration "${LEADER_ELECTION_LEASE_DURATION}"
validate_duration_input leader_election_renew_deadline "${LEADER_ELECTION_RENEW_DEADLINE}"
validate_duration_input leader_election_retry_period "${LEADER_ELECTION_RETRY_PERIOD}"

CREATE_ARGS=(--name="${KIND_CLUSTER_NAME}")
if [[ -n "${KIND_NODE_IMAGE}" ]]; then
  echo "Using kind node image: ${KIND_NODE_IMAGE}"
  CREATE_ARGS+=(--image="${KIND_NODE_IMAGE}")
fi

case "${CONTROL_PLANE_RESOURCE_PATCHES}" in
  true) ;;
  ""|false) CONTROL_PLANE_RESOURCE_PATCHES=false ;;
  *)
    echo "::error::control_plane_resource_patches must be true or false, got '${CONTROL_PLANE_RESOURCE_PATCHES}'"
    exit 1
    ;;
esac

case "${CONTROL_PLANE_LEADER_ELECTION_TUNING}" in
  true) ;;
  ""|false) CONTROL_PLANE_LEADER_ELECTION_TUNING=false ;;
  *)
    echo "::error::control_plane_leader_election_tuning must be true or false, got '${CONTROL_PLANE_LEADER_ELECTION_TUNING}'"
    exit 1
    ;;
esac

if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" || "${CONTROL_PLANE_LEADER_ELECTION_TUNING}" == "true" ]]; then
  patch_dir="$(mktemp -d)"
  config_template="$(mktemp)"
  cleanup_generated_config() {
    [[ -n "${patch_dir:-}" ]] && rm -rf "${patch_dir}"
    [[ -n "${config_template:-}" ]] && rm -f "${config_template}"
  }
  trap cleanup_generated_config EXIT

  # Keep YAML heredocs at column 0; indentation is literal content.
  if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" ]]; then
  cat > "${patch_dir}/kube-apiserver+strategic.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
  namespace: kube-system
spec:
  containers:
  - name: kube-apiserver
    resources:
      requests:
        cpu: ${API_SERVER_CPU_REQUEST}
        memory: ${API_SERVER_MEMORY_REQUEST}
EOF

  cat > "${patch_dir}/kube-controller-manager+strategic.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: kube-controller-manager
  namespace: kube-system
spec:
  containers:
  - name: kube-controller-manager
    resources:
      requests:
        cpu: ${CONTROLLER_MANAGER_CPU_REQUEST}
        memory: ${CONTROLLER_MANAGER_MEMORY_REQUEST}
EOF

  cat > "${patch_dir}/kube-scheduler+strategic.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: kube-scheduler
  namespace: kube-system
spec:
  containers:
  - name: kube-scheduler
    resources:
      requests:
        cpu: ${SCHEDULER_CPU_REQUEST}
        memory: ${SCHEDULER_MEMORY_REQUEST}
EOF

  cat > "${patch_dir}/etcd+strategic.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: etcd
  namespace: kube-system
spec:
  containers:
  - name: etcd
    resources:
      requests:
        cpu: ${ETCD_CPU_REQUEST}
        memory: ${ETCD_MEMORY_REQUEST}
EOF
  fi

  cat > "${config_template}" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
{{- if hasKey $ "name" }}
name: {{ $.name }}
{{- end }}
nodes:
- role: control-plane
  {{- if hasKey $ "image" }}
  image: {{ $.image }}
  {{- end }}
EOF
  if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" ]]; then
  cat >> "${config_template}" <<EOF
  extraMounts:
  - hostPath: ${patch_dir}
    containerPath: /patches
EOF
  fi
  cat >> "${config_template}" <<'EOF'
  kubeadmConfigPatches:
EOF
  if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" ]]; then
  cat >> "${config_template}" <<'EOF'
  - |
    kind: InitConfiguration
    patches:
      directory: /patches
EOF
  fi
  if [[ "${CONTROL_PLANE_LEADER_ELECTION_TUNING}" == "true" ]]; then
  # kind v0.31 renders kubeadm v1beta3. Keep a v1beta4 patch too so
  # this remains valid when a future kind image switches API versions.
  cat >> "${config_template}" <<EOF
  - |
    kind: ClusterConfiguration
    apiVersion: kubeadm.k8s.io/v1beta3
    controllerManager:
      extraArgs:
        leader-elect-lease-duration: "${LEADER_ELECTION_LEASE_DURATION}"
        leader-elect-renew-deadline: "${LEADER_ELECTION_RENEW_DEADLINE}"
        leader-elect-retry-period: "${LEADER_ELECTION_RETRY_PERIOD}"
    scheduler:
      extraArgs:
        leader-elect-lease-duration: "${LEADER_ELECTION_LEASE_DURATION}"
        leader-elect-renew-deadline: "${LEADER_ELECTION_RENEW_DEADLINE}"
        leader-elect-retry-period: "${LEADER_ELECTION_RETRY_PERIOD}"
  - |
    kind: ClusterConfiguration
    apiVersion: kubeadm.k8s.io/v1beta4
    controllerManager:
      extraArgs:
      - name: leader-elect-lease-duration
        value: "${LEADER_ELECTION_LEASE_DURATION}"
      - name: leader-elect-renew-deadline
        value: "${LEADER_ELECTION_RENEW_DEADLINE}"
      - name: leader-elect-retry-period
        value: "${LEADER_ELECTION_RETRY_PERIOD}"
    scheduler:
      extraArgs:
      - name: leader-elect-lease-duration
        value: "${LEADER_ELECTION_LEASE_DURATION}"
      - name: leader-elect-renew-deadline
        value: "${LEADER_ELECTION_RENEW_DEADLINE}"
      - name: leader-elect-retry-period
        value: "${LEADER_ELECTION_RETRY_PERIOD}"
EOF
  fi
  cat >> "${config_template}" <<'EOF'
{{- range $.workers }}
- role: worker
  {{- if hasKey $ "image" }}
  image: {{ $.image }}
  {{- end }}

  {{- if hasKey . "devices" }}
  {{- $devices := .devices }}
  {{- if not (kindIs "slice" $devices) }}
    {{- $devices = list .devices }}
  {{- end }}
  extraMounts:
    # We inject all NVIDIA GPUs using the nvidia-container-runtime.
    # This requires `accept-nvidia-visible-devices-as-volume-mounts = true` be set
    # in `/etc/nvidia-container-runtime/config.toml`
    {{- range $d := $devices }}
    - hostPath: /dev/null
      containerPath: /var/run/nvidia-container-devices/{{ $d }}
    {{- end }}
  {{- end }}
{{- end }}
EOF
  if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" ]]; then
    echo "Applying control-plane static pod resource patches from ${patch_dir}:"
    for patch_file in "${patch_dir}"/*.yaml; do
      echo "--- ${patch_file}"
      sed 's/^/  /' "${patch_file}"
    done
  fi
  if [[ "${CONTROL_PLANE_LEADER_ELECTION_TUNING}" == "true" ]]; then
    echo "Increasing kube-controller-manager and kube-scheduler leader election timeouts for slow CI control planes:"
    echo "  lease-duration=${LEADER_ELECTION_LEASE_DURATION}"
    echo "  renew-deadline=${LEADER_ELECTION_RENEW_DEADLINE}"
    echo "  retry-period=${LEADER_ELECTION_RETRY_PERIOD}"
  fi
  validate_generated_control_plane_config
  CREATE_ARGS+=(--config-template="${config_template}")
fi

set +e
timeout "${CLUSTER_CREATE_TIMEOUT}" nvkind cluster create "${CREATE_ARGS[@]}"
create_status=$?
set -e
case "${create_status}" in
  0) ;;
  124)
    echo "::warning::nvkind cluster create timed out after ${CLUSTER_CREATE_TIMEOUT}; continuing only if post-create checks pass"
    ;;
  *)
    echo "::error::nvkind cluster create failed with status ${create_status}"
    exit "${create_status}"
    ;;
esac

kubectl --context="kind-${KIND_CLUSTER_NAME}" wait --for=condition=Ready nodes --all --timeout=300s
kubectl --context="kind-${KIND_CLUSTER_NAME}" cluster-info
kubectl --context="kind-${KIND_CLUSTER_NAME}" get nodes -o wide
kubectl --context="kind-${KIND_CLUSTER_NAME}" describe nodes | \
  grep -E "^(Name:|Capacity:|Allocatable:|Allocated resources:|  cpu|  memory|  nvidia.com/gpu)" || true

echo "=== Kind node container resources ==="
docker ps --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" \
  --format '{{.Names}}' | sort | while read -r node_container; do
    [[ -z "${node_container}" ]] && continue
    docker inspect "${node_container}" \
      --format '{{.Name}} NanoCpus={{.HostConfig.NanoCpus}} CpuShares={{.HostConfig.CpuShares}} Memory={{.HostConfig.Memory}} MemoryReservation={{.HostConfig.MemoryReservation}}'
  done

echo "=== Control-plane resource requests/limits ==="
kubectl --context="kind-${KIND_CLUSTER_NAME}" -n kube-system \
  get pods -l tier=control-plane -o json | jq -r '
    .items[] as $pod |
    $pod.metadata.name,
    ($pod.spec.containers[] |
      "  " + .name +
      " requests=" + ((.resources.requests // {}) | tostring) +
      " limits=" + ((.resources.limits // {}) | tostring))
  ' || true

normalize_cpu_request() {
  local cpu="$1"

  if [[ "${cpu}" =~ ^([0-9]+)000m$ ]]; then
    echo "${BASH_REMATCH[1]}"
    return
  fi
  echo "${cpu}"
}

control_plane_request() {
  local component="$1"
  local resource="$2"

  kubectl --context="kind-${KIND_CLUSTER_NAME}" -n kube-system \
    get pod -l "component=${component}" \
    -o "jsonpath={.items[0].spec.containers[0].resources.requests.${resource}}"
}

assert_control_plane_request() {
  local component="$1"
  local resource="$2"
  local expected="$3"
  local actual

  actual="$(control_plane_request "${component}" "${resource}")"
  if [[ "${resource}" == "cpu" ]]; then
    expected="$(normalize_cpu_request "${expected}")"
    actual="$(normalize_cpu_request "${actual}")"
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    echo "::error::${component} ${resource} request is '${actual}', expected '${expected}'"
    exit 1
  fi
  echo "${component} ${resource} request verified: ${actual}"
}

control_plane_command_args() {
  local component="$1"

  kubectl --context="kind-${KIND_CLUSTER_NAME}" -n kube-system \
    get pod -l "component=${component}" \
    -o json | jq -r '.items[0].spec.containers[0] | ((.command // []) + (.args // []))[]?'
}

static_pod_manifest_contains_arg() {
  local component="$1"
  local expected="$2"
  local node="${KIND_CLUSTER_NAME}-control-plane"

  docker exec "${node}" grep -Fq -- "- ${expected}" "/etc/kubernetes/manifests/${component}.yaml"
}

running_static_pod_container_contains_arg() {
  local component="$1"
  local expected="$2"
  local node="${KIND_CLUSTER_NAME}-control-plane"
  local container_ids
  local container_id

  if ! container_ids="$(docker exec "${node}" crictl ps --name "${component}" -q 2>/dev/null)"; then
    return 1
  fi
  [[ -z "${container_ids}" ]] && return 1

  for container_id in ${container_ids}; do
    if docker exec "${node}" crictl inspect "${container_id}" 2>/dev/null | jq -e --arg expected "${expected}" '
      ([.info.runtimeSpec.process.args[]?, .status.info.runtimeSpec.process.args[]?] | index($expected)) != null
    ' >/dev/null; then
      return 0
    fi
  done
  return 1
}

dump_running_static_pod_container_args() {
  local component="$1"
  local node="${KIND_CLUSTER_NAME}-control-plane"
  local container_ids
  local container_id

  echo "Running ${component} CRI container args:"
  container_ids="$(docker exec "${node}" crictl ps --name "${component}" -q 2>/dev/null || true)"
  if [[ -z "${container_ids}" ]]; then
    echo "(no running ${component} CRI containers found)"
    return
  fi
  for container_id in ${container_ids}; do
    echo "--- ${container_id} ---"
    docker exec "${node}" crictl inspect "${container_id}" 2>/dev/null | jq -r '
      [.info.runtimeSpec.process.args[]?, .status.info.runtimeSpec.process.args[]?][]?
    ' || true
  done
}

dump_static_pod_manifest() {
  local component="$1"
  local node="${KIND_CLUSTER_NAME}-control-plane"

  echo "Static pod manifest /etc/kubernetes/manifests/${component}.yaml:"
  docker exec "${node}" sed -n '1,220p' "/etc/kubernetes/manifests/${component}.yaml" || true
}

assert_control_plane_arg() {
  local component="$1"
  local expected="$2"
  local command_args

  command_args="$(control_plane_command_args "${component}")"
  if ! grep -Fxq -- "${expected}" <<< "${command_args}"; then
    if running_static_pod_container_contains_arg "${component}" "${expected}"; then
      echo "${component} running CRI container args verified: ${expected} (live mirror pod omitted it)"
      return
    fi
    if static_pod_manifest_contains_arg "${component}" "${expected}"; then
      echo "::warning::${component} live mirror pod and running CRI container args did not show ${expected}; static pod manifest is patched"
      return
    fi
    echo "::error::${component} live pod command/args does not contain ${expected}"
    echo "Observed live command/args:"
    echo "${command_args}"
    dump_running_static_pod_container_args "${component}"
    dump_static_pod_manifest "${component}"
    exit 1
  fi
  echo "${component} command/args verified: ${expected}"
}

if [[ "${CONTROL_PLANE_RESOURCE_PATCHES}" == "true" ]]; then
  echo "Verifying control-plane resource patches..."
  assert_control_plane_request kube-apiserver cpu "${API_SERVER_CPU_REQUEST}"
  assert_control_plane_request kube-apiserver memory "${API_SERVER_MEMORY_REQUEST}"
  assert_control_plane_request kube-controller-manager cpu "${CONTROLLER_MANAGER_CPU_REQUEST}"
  assert_control_plane_request kube-controller-manager memory "${CONTROLLER_MANAGER_MEMORY_REQUEST}"
  assert_control_plane_request kube-scheduler cpu "${SCHEDULER_CPU_REQUEST}"
  assert_control_plane_request kube-scheduler memory "${SCHEDULER_MEMORY_REQUEST}"
  assert_control_plane_request etcd cpu "${ETCD_CPU_REQUEST}"
  assert_control_plane_request etcd memory "${ETCD_MEMORY_REQUEST}"
fi

if [[ "${CONTROL_PLANE_LEADER_ELECTION_TUNING}" == "true" ]]; then
  echo "Verifying control-plane leader election timeout patches..."
  for component in kube-controller-manager kube-scheduler; do
    assert_control_plane_arg "${component}" "--leader-elect-lease-duration=${LEADER_ELECTION_LEASE_DURATION}"
    assert_control_plane_arg "${component}" "--leader-elect-renew-deadline=${LEADER_ELECTION_RENEW_DEADLINE}"
    assert_control_plane_arg "${component}" "--leader-elect-retry-period=${LEADER_ELECTION_RETRY_PERIOD}"
  done
fi

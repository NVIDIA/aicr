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

# Diagnostic artifact collection intentionally omits -e so one broken cluster
# call does not prevent later artifacts from being collected.
set -uo pipefail
rm -rf /tmp/debug-artifacts /tmp/kind-logs
mkdir -p /tmp/debug-artifacts
mkdir -p /tmp/kind-logs
CONTROL_PLANE_COMPONENTS="kube-apiserver kube-controller-manager kube-scheduler etcd"
MAX_KIND_NODE_ARTIFACT_SECONDS="${MAX_KIND_NODE_ARTIFACT_SECONDS:-600}"
COLLECT_NODE_RUNTIME_ARTIFACTS="${COLLECT_NODE_RUNTIME_ARTIFACTS:-false}"
if ! [[ "${MAX_KIND_NODE_ARTIFACT_SECONDS}" =~ ^[0-9]+$ ]]; then
  echo "::warning::MAX_KIND_NODE_ARTIFACT_SECONDS must be an integer; got '${MAX_KIND_NODE_ARTIFACT_SECONDS}', defaulting to 600" >&2
  MAX_KIND_NODE_ARTIFACT_SECONDS=600
fi
command_timeout() {
  local limit="$1"
  shift
  timeout "${limit}" "$@"
}
kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" "$@"
}
docker_timeout() {
  local limit="$1"
  shift
  timeout "${limit}" docker "$@"
}

{
  date -u || true
  hostname || true
  uptime || true
  nproc || true
  free -h || true
  df -h / || true
  df -ih / || true
} > /tmp/debug-artifacts/runner-baseline.txt 2>&1 || true
docker_timeout 30s version > /tmp/debug-artifacts/docker-version.txt 2>&1 || true
docker_timeout 30s info > /tmp/debug-artifacts/docker-info.txt 2>&1 || true
command_timeout 30s nvidia-smi -L > /tmp/debug-artifacts/host-gpus.txt 2>&1 || true
command_timeout 30s nvidia-smi >> /tmp/debug-artifacts/host-gpus.txt 2>&1 || true
command_timeout 30s kind get clusters > /tmp/debug-artifacts/kind-clusters.txt 2>&1 || true
docker_timeout 30s ps -a --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" \
  > /tmp/debug-artifacts/kind-node-containers.txt 2>&1 || true

kubectl_kind get all --all-namespaces > /tmp/debug-artifacts/all-resources.txt || true
kubectl_kind get events --all-namespaces --sort-by='.lastTimestamp' > /tmp/debug-artifacts/events.txt || true
kubectl_kind get --raw='/livez?verbose' > /tmp/debug-artifacts/apiserver-livez.txt 2>&1 || true
kubectl_kind get --raw='/readyz?verbose' > /tmp/debug-artifacts/apiserver-readyz.txt 2>&1 || true
kubectl_kind -n kube-system get pods -l tier=control-plane -o wide \
  > /tmp/debug-artifacts/control-plane-pods.txt 2>&1 || true
kubectl_kind -n kube-system get events --sort-by='.lastTimestamp' \
  > /tmp/debug-artifacts/kube-system-events.txt 2>&1 || true
for component in ${CONTROL_PLANE_COMPONENTS}; do
  kubectl_kind -n kube-system describe pod -l "component=${component}" \
    > "/tmp/debug-artifacts/${component}-describe.txt" 2>&1 || true
  kubectl_kind -n kube-system logs -l "component=${component}" --all-containers --tail=300 \
    > "/tmp/debug-artifacts/${component}-logs.txt" 2>&1 || true
  kubectl_kind -n kube-system logs -l "component=${component}" --all-containers --previous --tail=300 \
    > "/tmp/debug-artifacts/${component}-previous-logs.txt" 2>&1 || true
  kubectl_kind -n kube-system get lease "${component}" -o yaml \
    > "/tmp/debug-artifacts/${component}-lease.yaml" 2>&1 || true
done
kubectl_kind -n gpu-operator get pods -o wide > /tmp/debug-artifacts/gpu-operator-pods.txt || true
kubectl_kind -n gpu-operator logs -l app=nvidia-device-plugin-daemonset --tail=100 > /tmp/debug-artifacts/device-plugin-logs.txt || true
kubectl_kind -n gpu-operator logs -l app.kubernetes.io/component=gpu-operator --tail=100 > /tmp/debug-artifacts/gpu-operator-logs.txt || true
kubectl_kind -n monitoring get deployment,statefulset,daemonset,pods -o wide \
  > /tmp/debug-artifacts/monitoring-workloads.txt 2>&1 || true
kubectl_kind -n monitoring describe deployment kube-prometheus-operator \
  > /tmp/debug-artifacts/kube-prometheus-operator-deployment-describe.txt 2>&1 || true
kubectl_kind -n monitoring logs deployment/kube-prometheus-operator --all-containers --tail=300 \
  > /tmp/debug-artifacts/kube-prometheus-operator-logs.txt 2>&1 || true
kubectl_kind -n monitoring logs deployment/kube-prometheus-operator --all-containers --previous --tail=300 \
  > /tmp/debug-artifacts/kube-prometheus-operator-previous-logs.txt 2>&1 || true
kubectl_kind -n monitoring get events --sort-by='.lastTimestamp' \
  > /tmp/debug-artifacts/monitoring-events.txt 2>&1 || true
{
  kubectl_kind -n monitoring get pods -o name 2>/dev/null \
    | grep '^pod/kube-prometheus-operator-' \
    | while read -r pod; do
        echo "=== ${pod} ==="
        kubectl_kind -n monitoring describe "${pod}" 2>&1 || true
      done
} > /tmp/debug-artifacts/kube-prometheus-operator-pods-describe.txt 2>&1 || true
kubectl_kind get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded > /tmp/debug-artifacts/non-running-pods.txt || true
tar_inputs=()
[[ -f recipe.yaml ]] && tar_inputs+=(recipe.yaml)
[[ -d bundle ]] && tar_inputs+=(bundle)
if [[ "${#tar_inputs[@]}" -gt 0 ]]; then
  echo "Archiving runtime bundle inputs: ${tar_inputs[*]}"
  tar -czf /tmp/debug-artifacts/aicr-runtime-bundle.tar.gz "${tar_inputs[@]}" || true
else
  echo "No recipe.yaml or bundle directory found; skipping runtime bundle archive"
fi

case "${COLLECT_NODE_RUNTIME_ARTIFACTS}" in
  true)
    artifact_loop_start="$(date +%s)"
    docker_timeout 30s ps --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" \
      --format '{{.Names}}' | sort | while read -r node_container; do
        [[ -z "${node_container}" ]] && continue
        artifact_loop_elapsed=$(($(date +%s) - artifact_loop_start))
        if (( artifact_loop_elapsed > MAX_KIND_NODE_ARTIFACT_SECONDS )); then
          echo "Kind node artifact collection exceeded ${MAX_KIND_NODE_ARTIFACT_SECONDS}s; stopping after partial collection."
          break
        fi
        node_file="${node_container//[^A-Za-z0-9_.-]/_}"
        docker_timeout 30s inspect "${node_container}" \
          > "/tmp/debug-artifacts/${node_file}-docker-inspect.json" 2>&1 || true
        docker_timeout 30s exec "${node_container}" journalctl -u kubelet \
          --since "90 minutes ago" --no-pager \
          > "/tmp/debug-artifacts/${node_file}-kubelet-journal.txt" 2>&1 || true
        docker_timeout 30s exec "${node_container}" journalctl -u containerd \
          --since "90 minutes ago" --no-pager \
          > "/tmp/debug-artifacts/${node_file}-containerd-journal.txt" 2>&1 || true
        docker_timeout 30s exec "${node_container}" crictl ps -a \
          > "/tmp/debug-artifacts/${node_file}-crictl-ps-a.txt" 2>&1 || true
        docker_timeout 30s exec "${node_container}" crictl pods \
          > "/tmp/debug-artifacts/${node_file}-crictl-pods.txt" 2>&1 || true
        docker_timeout 30s exec "${node_container}" crictl stats \
          > "/tmp/debug-artifacts/${node_file}-crictl-stats.txt" 2>&1 || true
        docker_timeout 30s exec "${node_container}" sh -c '
          date
          uptime || true
          free -h || true
          df -h / /var/lib/containerd /var/lib/kubelet 2>/dev/null || df -h
          echo "--- top cpu/memory processes ---"
          ps -eo pid,ppid,stat,etime,%cpu,%mem,comm,args --sort=-%cpu | head -40 || true
        ' > "/tmp/debug-artifacts/${node_file}-node-pressure.txt" 2>&1 || true
        # shellcheck disable=SC2016 # Expanded inside the kind node shell.
        docker_timeout 120s exec "${node_container}" sh -c '
          for component in kube-apiserver kube-controller-manager kube-scheduler etcd; do
            echo "=== ${component} static pod manifest ==="
            sed -n "1,220p" "/etc/kubernetes/manifests/${component}.yaml" 2>/dev/null || true
            echo "=== ${component} CRI containers ==="
            crictl ps -a --name "${component}" || true
            count=0
            for container_id in $(crictl ps -a --name "${component}" -q 2>/dev/null); do
              count=$((count + 1))
              if [ "${count}" -gt 8 ]; then
                echo "Skipping remaining ${component} CRI containers after first 8 entries."
                break
              fi
              echo "=== crictl inspect ${component} ${container_id} ==="
              crictl inspect "${container_id}" || true
              echo "=== crictl logs ${component} ${container_id} ==="
              crictl logs --tail=300 "${container_id}" || true
            done
          done
        ' > "/tmp/debug-artifacts/${node_file}-control-plane-cri.txt" 2>&1 || true
      done || true
    ;;
  ""|false)
    echo "Skipped kind node runtime artifacts. Set collect_node_runtime_artifacts=true to collect journalctl, crictl, and kind export logs." \
      > /tmp/debug-artifacts/node-runtime-artifacts-skipped.txt
    echo "Skipped kind log export. Set collect_node_runtime_artifacts=true to export kind logs." \
      > /tmp/kind-logs/kind-logs-skipped.txt
    ;;
  *)
    echo "Unknown COLLECT_NODE_RUNTIME_ARTIFACTS=${COLLECT_NODE_RUNTIME_ARTIFACTS}; skipping kind node runtime artifacts." \
      > /tmp/debug-artifacts/node-runtime-artifacts-skipped.txt
    echo "Unknown COLLECT_NODE_RUNTIME_ARTIFACTS=${COLLECT_NODE_RUNTIME_ARTIFACTS}; skipping kind log export." \
      > /tmp/kind-logs/kind-logs-skipped.txt
    ;;
esac

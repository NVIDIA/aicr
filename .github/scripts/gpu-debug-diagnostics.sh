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

# Diagnostic script: intentionally omits -e so each mode can keep collecting
# partial failure data. Keep -u and pipefail to catch script bugs and pipeline
# failures while individual kubectl_kind calls tolerate cluster errors.
set -uo pipefail

mode="${GPU_TEST_DIAGNOSTIC_MODE:-smoke}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME must be set}"

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

docker_timeout() {
  local limit="$1"
  shift
  timeout "${limit}" docker "$@"
}

command_timeout() {
  local limit="$1"
  shift
  timeout "${limit}" "$@"
}

print_setup_diagnostics() {
  echo "=== Runner baseline ==="
  date -u || true
  hostname || true
  uptime || true
  cat /proc/loadavg || true
  nproc || true
  free -h || true
  df -h / || true
  df -ih / || true
  echo "=== Docker health ==="
  docker_timeout 30s info >/dev/null 2>&1 && docker_timeout 30s version || true
  echo "=== Host GPUs ==="
  command_timeout 30s nvidia-smi -L || true
  command_timeout 30s nvidia-smi || true
  echo "=== Kind clusters ==="
  command_timeout 30s kind get clusters || true
  echo "=== Kind node containers ==="
  docker_timeout 30s ps -a --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" || true
  echo "=== Kind node container resources ==="
  docker_timeout 30s ps --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" \
    --format '{{.Names}}' | sort | while read -r node_container; do
      [[ -z "${node_container}" ]] && continue
      docker_timeout 30s inspect "${node_container}" \
        --format '{{.Name}} State={{.State.Status}} NanoCpus={{.HostConfig.NanoCpus}} CpuShares={{.HostConfig.CpuShares}} Memory={{.HostConfig.Memory}} MemoryReservation={{.HostConfig.MemoryReservation}}' || true
    done || true
  print_kind_node_pressure
}

print_kind_node_pressure() {
  local node_container

  echo "=== Kind node pressure snapshots ==="
  docker_timeout 30s ps --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" \
    --format '{{.Names}}' | sort | while read -r node_container; do
      [[ -z "${node_container}" ]] && continue
      echo "--- ${node_container} docker stats ---"
      docker_timeout 30s stats --no-stream \
        --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}' \
        "${node_container}" || true
      echo "--- ${node_container} node pressure ---"
      docker_timeout 30s exec "${node_container}" sh -c '
        date
        hostname || true
        uptime || true
        cat /proc/loadavg || true
        nproc || true
        free -h || true
        df -h / /var/lib/containerd /var/lib/kubelet 2>/dev/null || df -h
        echo "--- top cpu/memory processes ---"
        ps -eo pid,ppid,stat,etime,%cpu,%mem,comm,args --sort=-%cpu | head -25 || true
      ' || true
    done || true
}

print_workload_images() {
  local ns="$1"
  kubectl_kind -n "${ns}" get deployment,daemonset,statefulset -o json 2>/dev/null \
    | jq -r '
      .items[] |
      [
        .kind,
        .metadata.namespace + "/" + .metadata.name,
        (([.spec.template.spec.containers[]?.image] +
          [.spec.template.spec.initContainers[]?.image]) | unique | join(","))
      ] | @tsv
    ' || true
}

print_workload_inventory() {
  local ns
  echo "=== Workload image inventory ==="
  for ns in "$@"; do
    echo "--- ${ns} ---"
    print_workload_images "${ns}"
  done
}

print_component_status_summary() {
  echo "=== Component workload status ==="
  kubectl_kind get deployments,statefulsets,daemonsets,pods -A -o wide 2>/dev/null || true
  echo "=== Component rollout conditions ==="
  kubectl_kind get deployments,statefulsets,daemonsets -A \
    -o custom-columns='KIND:.kind,NAMESPACE:.metadata.namespace,NAME:.metadata.name,READY:.status.readyReplicas,AVAILABLE:.status.availableReplicas,DESIRED:.status.replicas,UPDATED:.status.updatedReplicas,AGE:.metadata.creationTimestamp' \
    2>/dev/null || true
  echo "=== Non-ready pods ==="
  kubectl_kind get pods -A \
    --field-selector=status.phase!=Running,status.phase!=Succeeded \
    -o wide 2>/dev/null || true
}

print_kube_prometheus_operator_diagnostics() {
  echo "=== Monitoring workloads ==="
  kubectl_kind -n monitoring get deployment,statefulset,daemonset,pods -o wide 2>/dev/null || true
  echo "=== kube-prometheus-operator deployment ==="
  kubectl_kind -n monitoring get deployment kube-prometheus-operator -o wide 2>/dev/null || true
  echo "=== kube-prometheus-operator deployment describe ==="
  kubectl_kind -n monitoring describe deployment kube-prometheus-operator 2>/dev/null || true
  echo "=== kube-prometheus-operator pod describe ==="
  kubectl_kind -n monitoring get pods -o name 2>/dev/null \
    | grep '^pod/kube-prometheus-operator-' \
    | while read -r pod; do
        echo "--- ${pod} ---"
        kubectl_kind -n monitoring describe "${pod}" 2>/dev/null || true
      done || true
  echo "=== kube-prometheus-operator logs ==="
  kubectl_kind -n monitoring logs deployment/kube-prometheus-operator --all-containers --tail=200 2>/dev/null || true
  echo "=== kube-prometheus-operator previous logs ==="
  kubectl_kind -n monitoring logs deployment/kube-prometheus-operator --all-containers --previous --tail=200 2>/dev/null || true
  echo "=== Recent events (monitoring) ==="
  kubectl_kind -n monitoring get events --sort-by='.lastTimestamp' 2>/dev/null | tail -80 || true
}

print_kai_diagnostics() {
  echo "=== KAI scheduler pods ==="
  kubectl_kind -n kai-scheduler get pods -o wide 2>/dev/null || true
  echo "=== KAI admission deployment ==="
  kubectl_kind -n kai-scheduler get deployment admission -o wide 2>/dev/null || true
  echo "=== KAI admission deployment describe ==="
  kubectl_kind -n kai-scheduler describe deployment admission 2>/dev/null || true
  echo "=== KAI admission pod describe ==="
  kubectl_kind -n kai-scheduler get pods -o name 2>/dev/null \
    | grep '^pod/admission-' \
    | while read -r pod; do
        kubectl_kind -n kai-scheduler describe "${pod}" 2>/dev/null || true
      done || true
  echo "=== KAI admission logs ==="
  kubectl_kind -n kai-scheduler logs deployment/admission --all-containers --tail=200 2>/dev/null || true
  echo "=== KAI scheduler logs ==="
  kubectl_kind -n kai-scheduler logs deployment/kai-scheduler-default --tail=100 2>/dev/null || true
  echo "=== KAI scheduler queues ==="
  kubectl_kind get queues -A 2>/dev/null || true
  echo "=== KAI scheduler podgroups ==="
  kubectl_kind get podgroups -A 2>/dev/null || true
  echo "=== Recent events (kai-scheduler) ==="
  kubectl_kind -n kai-scheduler get events --sort-by='.lastTimestamp' 2>/dev/null | tail -50 || true
}

print_custom_metrics() {
  local metric
  local ns
  local namespaces=("$@")

  echo "=== Custom metrics API ==="
  for metric in gpu_utilization gpu_memory_used gpu_power_usage; do
    for ns in "${namespaces[@]}"; do
      echo "--- ${ns}/${metric} ---"
      kubectl_kind get --raw "/apis/custom.metrics.k8s.io/v1beta1/namespaces/${ns}/pods/*/${metric}" 2>/dev/null \
        | jq . || true
    done
  done
}

print_metrics_pipeline_diagnostics() {
  echo "=== prometheus-adapter pods ==="
  kubectl_kind -n monitoring get pods -l app.kubernetes.io/name=prometheus-adapter -o wide 2>/dev/null || true
  echo "=== DCGM Exporter pods ==="
  kubectl_kind -n gpu-operator get pods -l app=nvidia-dcgm-exporter -o wide 2>/dev/null || true
  echo "=== Monitoring pods ==="
  kubectl_kind -n monitoring get pods -o wide 2>/dev/null || true
  echo "=== DRA ResourceSlices ==="
  kubectl_kind get resourceslices -o wide 2>/dev/null || true
  echo "=== Node status ==="
  kubectl_kind get nodes -o wide 2>/dev/null || true
}

print_common_gpu_diagnostics() {
  echo "=== ClusterPolicy status ==="
  kubectl_kind get clusterpolicy -o yaml 2>/dev/null || true
  echo "=== GPU Operator pods ==="
  kubectl_kind -n gpu-operator get pods -o wide 2>/dev/null || true
  echo "=== Non-running pods (all namespaces) ==="
  kubectl_kind get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded 2>/dev/null || true
  echo "=== Recent events (gpu-operator) ==="
  kubectl_kind -n gpu-operator get events --sort-by='.lastTimestamp' 2>/dev/null | tail -30 || true
}

print_h100_common_diagnostics() {
  local metric_namespaces=("$@")
  local common_namespaces=(
    cert-manager
    gpu-operator
    monitoring
    skyhook
    nvsentinel
    nvidia-dra-driver
    nvidia-network-operator
    kai-scheduler
  )

  print_setup_diagnostics
  print_component_status_summary
  print_workload_inventory "${common_namespaces[@]}" "${metric_namespaces[@]}"
  print_common_gpu_diagnostics
  print_kube_prometheus_operator_diagnostics
  print_kai_diagnostics
  print_custom_metrics gpu-operator "${metric_namespaces[@]}"
  print_metrics_pipeline_diagnostics
  echo "=== Node resources ==="
  kubectl_kind describe nodes 2>/dev/null | grep -A 20 "Allocated resources" || true
}

print_kubeflow_diagnostics() {
  echo "=== Kubeflow Trainer deployment ==="
  kubectl_kind -n kubeflow get deployment kubeflow-trainer-controller-manager -o wide 2>/dev/null || true
  echo "=== Kubeflow pods ==="
  kubectl_kind -n kubeflow get pods -o wide 2>/dev/null || true
  echo "=== Kubeflow validating webhooks ==="
  kubectl_kind get validatingwebhookconfigurations validator.trainer.kubeflow.org -o yaml 2>/dev/null || true
  echo "=== Kubeflow Trainer CRD ==="
  kubectl_kind get crd trainjobs.trainer.kubeflow.org -o yaml 2>/dev/null || true
}

print_dynamo_diagnostics() {
  echo "=== Dynamo pods ==="
  kubectl_kind -n dynamo-system get pods -o wide 2>/dev/null || true
  echo "=== Dynamo operator logs ==="
  kubectl_kind -n dynamo-system logs deployment/dynamo-platform-dynamo-operator-controller-manager --tail=100 -c manager 2>/dev/null || true
  echo "=== Recent events (dynamo-system) ==="
  kubectl_kind -n dynamo-system get events --sort-by='.lastTimestamp' 2>/dev/null | tail -30 || true
}

print_kgateway_diagnostics() {
  echo "=== kgateway pods ==="
  kubectl_kind -n kgateway-system get pods -o wide 2>/dev/null || true
  echo "=== GatewayClass status ==="
  kubectl_kind get gatewayclass -o yaml 2>/dev/null || true
  echo "=== Gateway status ==="
  kubectl_kind get gateways -A -o yaml 2>/dev/null || true
}

case "${mode}" in
  smoke)
    print_setup_diagnostics
    print_common_gpu_diagnostics
    echo "=== Node status ==="
    kubectl_kind get nodes -o wide 2>/dev/null || true
    ;;
  training)
    print_h100_common_diagnostics kubeflow
    print_kubeflow_diagnostics
    ;;
  inference)
    print_h100_common_diagnostics dynamo-system kgateway-system
    print_dynamo_diagnostics
    print_kgateway_diagnostics
    ;;
  *)
    echo "::error::unknown GPU_TEST_DIAGNOSTIC_MODE: ${mode}"
    exit 1
    ;;
esac

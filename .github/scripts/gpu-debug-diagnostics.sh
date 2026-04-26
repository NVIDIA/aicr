#!/usr/bin/env bash
set -o pipefail

mode="${GPU_TEST_DIAGNOSTIC_MODE:-smoke}"

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" "$@"
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

print_grafana_diagnostics() {
  echo "=== Grafana deployment ==="
  kubectl_kind -n monitoring get deployment grafana -o wide 2>/dev/null || true
  echo "=== Grafana pods ==="
  kubectl_kind -n monitoring get pods -l app.kubernetes.io/name=grafana -o wide 2>/dev/null || true
  echo "=== Grafana deployment describe ==="
  kubectl_kind -n monitoring describe deployment grafana 2>/dev/null || true
  echo "=== Grafana pod describe ==="
  kubectl_kind -n monitoring describe pods -l app.kubernetes.io/name=grafana 2>/dev/null || true
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

case "${mode}" in
  smoke)
    print_common_gpu_diagnostics
    echo "=== Node status ==="
    kubectl_kind get nodes -o wide 2>/dev/null || true
    ;;
  training)
    print_workload_inventory cert-manager gpu-operator monitoring skyhook nvsentinel nvidia-dra-driver \
      nvidia-network-operator kai-scheduler kubeflow
    print_grafana_diagnostics
    print_kai_diagnostics
    echo "=== Kubeflow Trainer deployment ==="
    kubectl_kind -n kubeflow get deployment kubeflow-trainer-controller-manager -o wide 2>/dev/null || true
    echo "=== Kubeflow pods ==="
    kubectl_kind -n kubeflow get pods -o wide 2>/dev/null || true
    echo "=== Kubeflow validating webhooks ==="
    kubectl_kind get validatingwebhookconfigurations validator.trainer.kubeflow.org -o yaml 2>/dev/null || true
    echo "=== Kubeflow Trainer CRD ==="
    kubectl_kind get crd trainjobs.trainer.kubeflow.org -o yaml 2>/dev/null || true
    echo "=== Non-running pods (all namespaces) ==="
    kubectl_kind get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded 2>/dev/null || true
    echo "=== GPU Operator pods ==="
    kubectl_kind -n gpu-operator get pods -o wide 2>/dev/null || true
    echo "=== Node resources ==="
    kubectl_kind describe nodes 2>/dev/null | grep -A 20 "Allocated resources" || true
    ;;
  inference)
    print_workload_inventory cert-manager gpu-operator monitoring skyhook nvsentinel nvidia-dra-driver \
      nvidia-network-operator kai-scheduler dynamo-system kgateway-system
    print_common_gpu_diagnostics
    echo "=== Dynamo pods ==="
    kubectl_kind -n dynamo-system get pods -o wide 2>/dev/null || true
    echo "=== Dynamo operator logs ==="
    kubectl_kind -n dynamo-system logs deployment/dynamo-operator-controller-manager --tail=100 -c manager 2>/dev/null || true
    echo "=== Recent events (dynamo-system) ==="
    kubectl_kind -n dynamo-system get events --sort-by='.lastTimestamp' 2>/dev/null | tail -30 || true
    print_kai_diagnostics
    echo "=== Custom metrics API ==="
    for metric in gpu_utilization gpu_memory_used gpu_power_usage; do
      echo "--- ${metric} ---"
      for ns in gpu-operator dynamo-system; do
        kubectl_kind get --raw "/apis/custom.metrics.k8s.io/v1beta1/namespaces/${ns}/pods/*/${metric}" 2>/dev/null | jq . || true
      done
    done
    print_grafana_diagnostics
    echo "=== prometheus-adapter pods ==="
    kubectl_kind -n monitoring get pods -l app.kubernetes.io/name=prometheus-adapter -o wide 2>/dev/null || true
    echo "=== kgateway pods ==="
    kubectl_kind -n kgateway-system get pods -o wide 2>/dev/null || true
    echo "=== GatewayClass status ==="
    kubectl_kind get gatewayclass -o yaml 2>/dev/null || true
    echo "=== Gateway status ==="
    kubectl_kind get gateways -A -o yaml 2>/dev/null || true
    echo "=== DCGM Exporter pods ==="
    kubectl_kind -n gpu-operator get pods -l app=nvidia-dcgm-exporter -o wide 2>/dev/null || true
    echo "=== Monitoring pods ==="
    kubectl_kind -n monitoring get pods -o wide 2>/dev/null || true
    echo "=== DRA ResourceSlices ==="
    kubectl_kind get resourceslices -o wide 2>/dev/null || true
    echo "=== Node status ==="
    kubectl_kind get nodes -o wide 2>/dev/null || true
    ;;
  *)
    echo "::error::unknown GPU_TEST_DIAGNOSTIC_MODE: ${mode}"
    exit 1
    ;;
esac

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

set -euo pipefail

validate_duration_input() {
  local input_name="$1"
  local input_value="$2"

  if ! [[ "${input_value}" =~ ^[0-9]+[smh]$ ]]; then
    echo "::error::${input_name} must be a duration like 60s, 2m, or 1h; got '${input_value}'"
    exit 1
  fi
}

duration_seconds() {
  local input_value="$1"
  local number="${input_value%[smh]}"
  local unit="${input_value: -1}"
  local amount

  amount=$((10#${number}))

  case "${unit}" in
    s) echo "${amount}" ;;
    m) echo $((amount * 60)) ;;
    h) echo $((amount * 3600)) ;;
    *)
      echo "::error::unsupported duration unit in '${input_value}'" >&2
      exit 1
      ;;
  esac
}

MAX_RESTARTS="${MAX_RESTARTS:-}"
MAX_RESTARTS="${MAX_RESTARTS#"${MAX_RESTARTS%%[![:space:]]*}"}"
MAX_RESTARTS="${MAX_RESTARTS%"${MAX_RESTARTS##*[![:space:]]}"}"
MAX_RESTARTS_LIMIT=""
if [[ -n "${MAX_RESTARTS}" ]]; then
  if ! [[ "${MAX_RESTARTS}" =~ ^[0-9]+$ ]]; then
    echo "::error::max_restarts must be a non-negative integer, got '${MAX_RESTARTS}'"
    exit 1
  fi
  MAX_RESTARTS_LIMIT="$((10#${MAX_RESTARTS}))"
fi

WAIT_TIMEOUT="${WAIT_TIMEOUT#"${WAIT_TIMEOUT%%[![:space:]]*}"}"
WAIT_TIMEOUT="${WAIT_TIMEOUT%"${WAIT_TIMEOUT##*[![:space:]]}"}"
validate_duration_input wait_timeout "${WAIT_TIMEOUT}"

STABILITY_WINDOW="${STABILITY_WINDOW#"${STABILITY_WINDOW%%[![:space:]]*}"}"
STABILITY_WINDOW="${STABILITY_WINDOW%"${STABILITY_WINDOW##*[![:space:]]}"}"
if [[ -z "${STABILITY_WINDOW}" ]]; then
  STABILITY_WINDOW="0s"
fi
validate_duration_input stability_window "${STABILITY_WINDOW}"
if [[ "${STABILITY_WINDOW}" =~ ^0+[smh]$ ]]; then
  STABILITY_WINDOW="0s"
fi
STABILITY_WINDOW_SECONDS="$(duration_seconds "${STABILITY_WINDOW}")"
if [[ -n "${MAX_RESTARTS_LIMIT}" ]] && [[ "${STABILITY_WINDOW}" != "0s" ]] && (( MAX_RESTARTS_LIMIT != 1 )); then
  echo "::warning::max_restarts is diagnostic context when stability_window is non-zero; new restarts during the stability window remain the hard failure gate"
fi

STABILITY_PROBE_INTERVAL="${STABILITY_PROBE_INTERVAL:-10s}"
STABILITY_PROBE_INTERVAL="${STABILITY_PROBE_INTERVAL#"${STABILITY_PROBE_INTERVAL%%[![:space:]]*}"}"
STABILITY_PROBE_INTERVAL="${STABILITY_PROBE_INTERVAL%"${STABILITY_PROBE_INTERVAL##*[![:space:]]}"}"
validate_duration_input stability_probe_interval "${STABILITY_PROBE_INTERVAL}"
STABILITY_PROBE_INTERVAL_SECONDS="$(duration_seconds "${STABILITY_PROBE_INTERVAL}")"
if (( STABILITY_PROBE_INTERVAL_SECONDS <= 0 )); then
  echo "::error::stability_probe_interval must be greater than 0, got '${STABILITY_PROBE_INTERVAL}'"
  exit 1
fi
STABILITY_PROBE_FAILURE_THRESHOLD="${STABILITY_PROBE_FAILURE_THRESHOLD:-2}"
STABILITY_PROBE_FAILURE_THRESHOLD="${STABILITY_PROBE_FAILURE_THRESHOLD#"${STABILITY_PROBE_FAILURE_THRESHOLD%%[![:space:]]*}"}"
STABILITY_PROBE_FAILURE_THRESHOLD="${STABILITY_PROBE_FAILURE_THRESHOLD%"${STABILITY_PROBE_FAILURE_THRESHOLD##*[![:space:]]}"}"
if ! [[ "${STABILITY_PROBE_FAILURE_THRESHOLD}" =~ ^[0-9]+$ ]]; then
  echo "::error::stability_probe_failure_threshold must be a positive integer, got '${STABILITY_PROBE_FAILURE_THRESHOLD}'"
  exit 1
fi
if (( STABILITY_PROBE_FAILURE_THRESHOLD <= 0 )); then
  echo "::error::stability_probe_failure_threshold must be greater than 0, got '${STABILITY_PROBE_FAILURE_THRESHOLD}'"
  exit 1
fi

LEASE_COMPONENTS="${LEASE_COMPONENTS:-kube-controller-manager kube-scheduler}"
LEASE_COMPONENTS="${LEASE_COMPONENTS#"${LEASE_COMPONENTS%%[![:space:]]*}"}"
LEASE_COMPONENTS="${LEASE_COMPONENTS%"${LEASE_COMPONENTS##*[![:space:]]}"}"

LEASE_STALE_TIMEOUT="${LEASE_STALE_TIMEOUT:-120s}"
LEASE_STALE_TIMEOUT="${LEASE_STALE_TIMEOUT#"${LEASE_STALE_TIMEOUT%%[![:space:]]*}"}"
LEASE_STALE_TIMEOUT="${LEASE_STALE_TIMEOUT%"${LEASE_STALE_TIMEOUT##*[![:space:]]}"}"
validate_duration_input lease_stale_timeout "${LEASE_STALE_TIMEOUT}"
LEASE_STALE_TIMEOUT_SECONDS="$(duration_seconds "${LEASE_STALE_TIMEOUT}")"
if (( LEASE_STALE_TIMEOUT_SECONDS <= 0 )); then
  echo "::error::lease_stale_timeout must be greater than 0, got '${LEASE_STALE_TIMEOUT}'"
  exit 1
fi

RUNTIME_DIAGNOSTICS="${RUNTIME_DIAGNOSTICS:-false}"
RUNTIME_DIAGNOSTICS="${RUNTIME_DIAGNOSTICS#"${RUNTIME_DIAGNOSTICS%%[![:space:]]*}"}"
RUNTIME_DIAGNOSTICS="${RUNTIME_DIAGNOSTICS%"${RUNTIME_DIAGNOSTICS##*[![:space:]]}"}"
case "${RUNTIME_DIAGNOSTICS}" in
  true|false) ;;
  *)
    echo "::error::runtime_diagnostics must be true or false, got '${RUNTIME_DIAGNOSTICS}'"
    exit 1
    ;;
esac

kubectl_kind() {
  timeout 30s kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" "$@"
}

docker_timeout() {
  timeout 30s docker "$@"
}

RESTART_COUNT_ATTEMPTS=3
RESTART_COUNT_RETRY_SLEEP_SECONDS=2
declare -A INITIAL_RESTARTS=()

kubectl_kind get --raw='/readyz' || true

wait_ready() {
  local component="$1"
  local selector="component=${component}"

  if ! timeout "${WAIT_TIMEOUT}" kubectl --request-timeout=10s --context="kind-${KIND_CLUSTER_NAME}" -n "${NAMESPACE}" \
    wait --for=condition=Ready pod -l "${selector}" --timeout="${WAIT_TIMEOUT}"; then
    return 1
  fi
}

restart_total() {
  local component="$1"
  local selector="component=${component}"
  local restart_counts
  local restart_count
  local total=0
  local attempt

  for ((attempt = 1; attempt <= RESTART_COUNT_ATTEMPTS; attempt++)); do
    if restart_counts=$(kubectl_kind -n "${NAMESPACE}" get pod -l "${selector}" \
      -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}'); then
      if [[ -n "${restart_counts}" ]]; then
        break
      fi
      echo "::warning::no container statuses found for ${component} pods (attempt ${attempt}/${RESTART_COUNT_ATTEMPTS})" >&2
    else
      echo "::warning::failed to read restart counts for ${component} pods (attempt ${attempt}/${RESTART_COUNT_ATTEMPTS})" >&2
    fi

    if (( attempt < RESTART_COUNT_ATTEMPTS )); then
      sleep "${RESTART_COUNT_RETRY_SLEEP_SECONDS}"
    fi
  done

  if [[ -z "${restart_counts}" ]]; then
    echo "::error::no container statuses found for ${component} pods after ${RESTART_COUNT_ATTEMPTS} attempts" >&2
    dump_component_diagnostics "${component}" >&2
    exit 1
  fi

  while IFS= read -r restart_count; do
    [[ -z "${restart_count}" ]] && continue
    total=$((total + restart_count))
  done <<< "${restart_counts}"
  echo "${total}"
}

report_restart_baseline() {
  local component="$1"
  local restart_count="$2"

  if (( restart_count > 0 )); then
    if [[ "${STABILITY_WINDOW}" == "0s" ]] && [[ -n "${MAX_RESTARTS_LIMIT}" ]]; then
      echo "::warning::${component} has historical restartCount=${restart_count}; max_restarts=${MAX_RESTARTS_LIMIT} will be enforced because stability_window=0s"
    else
      echo "::warning::${component} has historical restartCount=${restart_count}; checking current readiness and stability window only"
    fi
    return
  fi
  echo "${component} restartCount=${restart_count}"
}

dump_control_plane_summary() {
  echo "=== Control-plane pod restart summary ==="
  kubectl_kind -n "${NAMESPACE}" get pods -l tier=control-plane -o wide || true
  kubectl_kind -n "${NAMESPACE}" get pods -l tier=control-plane \
    -o jsonpath='{range .items[*]}{.metadata.name}{" restartCount="}{range .status.containerStatuses[*]}{.restartCount}{" "}{end}{"\n"}{end}' || true
}

require_readyz() {
  local reason="$1"

  if ! kubectl_kind get --raw='/readyz'; then
    echo "::error::kube-apiserver /readyz failed ${reason}"
    dump_all_control_plane_runtime_diagnostics
    exit 1
  fi
}

probe_control_plane_api() {
  local reason="$1"
  local component
  local lease_summary

  if ! kubectl_kind get --raw='/readyz' >/dev/null; then
    echo "::error::kube-apiserver /readyz probe failed ${reason}"
    return 1
  fi

  for component in ${LEASE_COMPONENTS}; do
    if ! lease_summary=$(kubectl_kind -n "${NAMESPACE}" get lease "${component}" \
      -o jsonpath='{.metadata.name}{" holder="}{.spec.holderIdentity}{" renewTime="}{.spec.renewTime}{"\n"}' 2>/dev/null); then
      echo "::error::failed to read leader election lease ${component} ${reason}"
      return 1
    fi
    echo "${lease_summary}"
  done
}

lease_renew_epoch() {
  local renew_time="$1"

  date -u -d "${renew_time}" +%s 2>/dev/null
}

verify_leader_lease_freshness() {
  local component
  local now_epoch
  local renew_time
  local renew_epoch
  local lease_age

  [[ -z "${LEASE_COMPONENTS}" ]] && return

  now_epoch="$(date -u +%s)"
  echo "Checking leader election lease freshness (max age ${LEASE_STALE_TIMEOUT})..."
  for component in ${LEASE_COMPONENTS}; do
    if ! renew_time=$(kubectl_kind -n "${NAMESPACE}" get lease "${component}" -o jsonpath='{.spec.renewTime}' 2>/dev/null); then
      echo "::error::failed to read leader election lease ${component}"
      dump_all_control_plane_runtime_diagnostics
      exit 1
    fi
    if [[ -z "${renew_time}" ]]; then
      echo "::error::leader election lease ${component} has empty spec.renewTime"
      dump_all_control_plane_runtime_diagnostics
      exit 1
    fi
    if ! renew_epoch="$(lease_renew_epoch "${renew_time}")"; then
      echo "::error::failed to parse leader election lease ${component} renewTime '${renew_time}'"
      dump_all_control_plane_runtime_diagnostics
      exit 1
    fi
    lease_age=$((now_epoch - renew_epoch))
    if (( lease_age < 0 )); then
      lease_age=0
    fi
    echo "${component} lease renewTime=${renew_time} age=${lease_age}s"
    if (( lease_age > LEASE_STALE_TIMEOUT_SECONDS )); then
      echo "::error::leader election lease ${component} is stale: age=${lease_age}s exceeds ${LEASE_STALE_TIMEOUT}"
      dump_all_control_plane_runtime_diagnostics
      exit 1
    fi
  done
}

observe_stability_window() {
  local label="$1"
  local elapsed=0
  local probe=0
  local sleep_seconds
  local consecutive_failures=0
  local total_failures=0

  echo "Observing control-plane stability for ${STABILITY_WINDOW} (${label}); probing every ${STABILITY_PROBE_INTERVAL}, failing after ${STABILITY_PROBE_FAILURE_THRESHOLD} consecutive probe failure(s)..."
  while (( elapsed < STABILITY_WINDOW_SECONDS )); do
    sleep_seconds="${STABILITY_PROBE_INTERVAL_SECONDS}"
    if (( elapsed + sleep_seconds > STABILITY_WINDOW_SECONDS )); then
      sleep_seconds=$((STABILITY_WINDOW_SECONDS - elapsed))
    fi
    if (( sleep_seconds > 0 )); then
      sleep "${sleep_seconds}"
      elapsed=$((elapsed + sleep_seconds))
    fi

    probe=$((probe + 1))
    echo "=== Control-plane stability probe ${probe} (${elapsed}/${STABILITY_WINDOW_SECONDS}s, ${label}) ==="
    if probe_control_plane_api "during ${label} stability probe ${probe}"; then
      consecutive_failures=0
      continue
    fi

    total_failures=$((total_failures + 1))
    consecutive_failures=$((consecutive_failures + 1))
    echo "::warning::control-plane stability probe ${probe} failed (${consecutive_failures} consecutive, ${total_failures} total)"
    if (( consecutive_failures >= STABILITY_PROBE_FAILURE_THRESHOLD )); then
      echo "::error::control-plane had ${consecutive_failures} consecutive failed stability probes during ${label}"
      dump_all_control_plane_runtime_diagnostics
      exit 1
    fi
  done

  if (( total_failures > 0 )); then
    echo "::warning::control-plane had ${total_failures} transient failed stability probe(s) during ${label}; final health checks must still pass"
  fi
  verify_leader_lease_freshness
}

dump_api_server_health() {
  local endpoint

  for endpoint in '/livez?verbose' '/readyz?verbose' '/healthz'; do
    echo "=== kube-apiserver ${endpoint} ==="
    kubectl_kind get --raw="${endpoint}" || true
  done
}

dump_kind_node_runtime_summary() {
  local node="${KIND_CLUSTER_NAME}-control-plane"

  if ! docker_timeout inspect "${node}" >/dev/null 2>&1; then
    echo "::warning::cannot collect node runtime summary: kind node container ${node} not found"
    return
  fi

  echo "=== ${node} docker stats ==="
  docker_timeout stats --no-stream \
    --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}' \
    "${node}" || true

  echo "=== ${node} docker inspect state ==="
  docker_timeout inspect \
    --format 'status={{.State.Status}} running={{.State.Running}} oomKilled={{.State.OOMKilled}} pid={{.State.Pid}} started={{.State.StartedAt}} finished={{.State.FinishedAt}}' \
    "${node}" || true

  echo "=== ${node} node pressure snapshot ==="
  docker_timeout exec "${node}" sh -c '
    date
    uptime || true
    free -h || true
    df -h / /var/lib/containerd /var/lib/kubelet 2>/dev/null || df -h
    echo "--- top cpu/memory processes ---"
    ps -eo pid,ppid,stat,etime,%cpu,%mem,comm,args --sort=-%cpu | head -40 || true
  ' || true

  echo "=== ${node} CRI pod/container summary ==="
  docker_timeout exec "${node}" crictl pods || true
  docker_timeout exec "${node}" crictl ps -a || true
  docker_timeout exec "${node}" crictl stats || true
}

dump_static_pod_runtime_diagnostics() {
  local component="$1"
  local node="${KIND_CLUSTER_NAME}-control-plane"
  local container_ids
  local container_id
  local count=0

  if ! docker_timeout inspect "${node}" >/dev/null 2>&1; then
    echo "::warning::cannot collect ${component} runtime diagnostics: kind node container ${node} not found"
    return
  fi

  echo "=== ${node} ${component} static pod manifest ==="
  docker_timeout exec "${node}" sh -c "sed -n '1,220p' /etc/kubernetes/manifests/${component}.yaml" || true

  echo "=== ${node} ${component} CRI containers ==="
  docker_timeout exec "${node}" crictl ps -a --name "${component}" || true

  container_ids=$(docker_timeout exec "${node}" crictl ps -a --name "${component}" -q 2>/dev/null || true)
  for container_id in ${container_ids}; do
    count=$((count + 1))
    if (( count > 8 )); then
      echo "Skipping remaining ${component} CRI containers after first 8 entries."
      break
    fi

    echo "=== ${node} crictl inspect ${component} ${container_id} ==="
    docker_timeout exec "${node}" crictl inspect "${container_id}" || true
    echo "=== ${node} crictl logs ${component} ${container_id} ==="
    docker_timeout exec "${node}" crictl logs --tail=200 "${container_id}" || true
  done

  echo "=== ${node} kubelet journal (${component}) ==="
  docker_timeout exec "${node}" journalctl -u kubelet --since '45 minutes ago' --no-pager 2>/dev/null \
    | grep -Ei "${component}|static pod|mirror pod|probe|liveness|readiness|startup|back-off|backoff|container|failed|error|oom|killed" \
    | tail -200 || true

  echo "=== ${node} containerd journal (${component}) ==="
  docker_timeout exec "${node}" journalctl -u containerd --since '45 minutes ago' --no-pager 2>/dev/null \
    | grep -Ei "${component}|container|task|shim|deadline|failed|error|oom|killed" \
    | tail -200 || true
}

dump_all_control_plane_runtime_diagnostics() {
  local component

  dump_control_plane_summary
  dump_api_server_health
  if [[ "${RUNTIME_DIAGNOSTICS}" != "true" ]]; then
    echo "Skipping kind node runtime diagnostics. Set runtime_diagnostics=true to collect docker stats, crictl, and journalctl on failure."
    return
  fi
  dump_kind_node_runtime_summary
  for component in ${COMPONENTS}; do
    dump_static_pod_runtime_diagnostics "${component}"
    kubectl_kind -n "${NAMESPACE}" get lease "${component}" -o yaml 2>/dev/null || true
  done
}

dump_component_diagnostics() {
  local component="$1"
  local selector="component=${component}"
  local pods
  local pod

  dump_control_plane_summary
  kubectl_kind -n "${NAMESPACE}" get pod -l "${selector}" -o wide || true
  kubectl_kind -n "${NAMESPACE}" describe pod -l "${selector}" || true
  kubectl_kind -n "${NAMESPACE}" get events --sort-by='.lastTimestamp' 2>/dev/null | tail -30 || true

  pods=$(kubectl_kind -n "${NAMESPACE}" get pod -l "${selector}" -o name 2>/dev/null || true)
  while IFS= read -r pod; do
    [[ -z "${pod}" ]] && continue
    echo "=== ${pod} logs ==="
    kubectl_kind -n "${NAMESPACE}" logs "${pod}" --all-containers --tail=100 2>/dev/null || true
    echo "=== ${pod} previous logs ==="
    kubectl_kind -n "${NAMESPACE}" logs "${pod}" --all-containers --previous --tail=100 2>/dev/null || true
  done <<< "${pods}"

  dump_all_control_plane_runtime_diagnostics
  kubectl_kind -n "${NAMESPACE}" get lease "${component}" -o yaml 2>/dev/null || true
}

check_component() {
  local component="$1"
  local selector="component=${component}"
  local pods
  local initial_restarts

  if ! pods=$(kubectl_kind -n "${NAMESPACE}" get pod -l "${selector}" -o name); then
    echo "::error::failed to list ${component} pods in ${NAMESPACE} with selector ${selector}"
    kubectl_kind -n "${NAMESPACE}" get pods -o wide || true
    exit 1
  fi
  if [[ -z "${pods}" ]]; then
    echo "::error::no ${component} pods found in ${NAMESPACE} with selector ${selector}"
    kubectl_kind -n "${NAMESPACE}" get pods -o wide || true
    exit 1
  fi

  if ! wait_ready "${component}"; then
    echo "::error::${component} pods did not become Ready within ${WAIT_TIMEOUT}"
    dump_component_diagnostics "${component}"
    kubectl_kind get --raw='/readyz' || true
    exit 1
  fi
  initial_restarts=$(restart_total "${component}")
  report_restart_baseline "${component}" "${initial_restarts}"
  INITIAL_RESTARTS["${component}"]="${initial_restarts}"
}

verify_stability_window() {
  local component
  local initial_restarts
  local final_restarts

  if [[ "${STABILITY_WINDOW}" == "0s" ]]; then
    if [[ -n "${MAX_RESTARTS_LIMIT}" ]]; then
      for component in ${COMPONENTS}; do
        final_restarts="${INITIAL_RESTARTS[${component}]:-0}"
        if (( final_restarts > MAX_RESTARTS_LIMIT )); then
          echo "::error::${component} restartCount=${final_restarts} exceeds max_restarts=${MAX_RESTARTS_LIMIT}"
          dump_component_diagnostics "${component}"
          exit 1
        fi
      done
    fi
    verify_leader_lease_freshness
    return
  fi

  observe_stability_window "primary"
  for component in ${COMPONENTS}; do
    initial_restarts="${INITIAL_RESTARTS[${component}]:-}"
    if [[ -z "${initial_restarts}" ]]; then
      echo "::error::missing initial restart count for ${component}"
      exit 1
    fi
    if ! wait_ready "${component}"; then
      echo "::error::${component} pods became unready during ${STABILITY_WINDOW}"
      dump_component_diagnostics "${component}"
      kubectl_kind get --raw='/readyz' || true
      exit 1
    fi
    final_restarts=$(restart_total "${component}")
    if (( final_restarts > initial_restarts )); then
      echo "::error::${component} restartCount increased from ${initial_restarts} to ${final_restarts} during ${STABILITY_WINDOW}"
      dump_component_diagnostics "${component}"
      kubectl_kind get --raw='/readyz' || true
      exit 1
    fi
    INITIAL_RESTARTS["${component}"]="${final_restarts}"
  done
}

for component in ${COMPONENTS}; do
  check_component "${component}"
done
verify_stability_window
require_readyz "after stability window"

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
#
# shellcheck shell=bash
#
# #2625: prove the NVSentinel health pipeline with fault injection. Adopts
# NVSentinel upstream's tests/uat/tests.sh ladder: write a synthetic XID 79
# ("GPU has fallen off the bus") line to a real node's /dev/kmsg and assert,
# in order:
#   Stage 1 (detection) -- SysLogsXIDError appears on the node. Always run.
#   Stage 2 (quarantine) -- the node is cordoned. Gated on
#                           FAULT_INJECT_REMEDIATION_ENABLED; SKIPPED (not
#                           failed) otherwise.
#   Stage 3 (recovery)  -- the node's boot ID changes (a reboot occurred).
#                           Gated on FAULT_INJECT_REMEDIATION_ENABLED AND
#                           FAULT_INJECT_JANITOR_ENABLED; SKIPPED otherwise.
#
# No shipped recipe enables fault quarantine or remediation today (#2609),
# so both gates default to false and Stages 2/3 are SKIPPED on every lane as
# shipped -- this is the "skipped otherwise rather than failing" Success
# Criterion on #2625, implemented now so no further wiring is needed once a
# recipe opts in. CONFIRM BEFORE FLIPPING EITHER GATE ON A REAL LANE: there
# is currently no recipe-level field this repo can read to detect "fault
# quarantine enabled" (that lands with #1014/#2609) -- the two env vars
# below are an explicit, manually-set stand-in until that field exists, at
# which point they should be derived from the resolved recipe instead of set
# by hand.
#
# Sourced by tests/uat/lib/phases.sh, which calls phase_sxid_fault_inject()
# from its phase dispatcher (uat_main) and runs it as the `sxidfault` phase,
# AFTER phase_verify (see the function's own comment below for why).
#
# Results are recorded as CTRF (sxidfault-result.json, mirroring phase_prep's
# snapshot-result.json pattern, #1806) so they show up in the UAT report
# alongside the other CUJ lanes, win or lose.
#
# CONFIRM BEFORE ENABLING IN A NIGHTLY LANE: this function was written
# against the issue's description of upstream's tests/uat/tests.sh, which is
# not vendored in this repo. Before this runs unattended, diff the
# circuit-breaker ConfigMap name/namespace/field (FAULT_INJECT_* below) and
# the synthetic kmsg line format against the actual upstream script.

# Set by _sxid_fault_inject_impl once it knows them, so the outer wrapper can
# clean up the debug pod on every exit path (including a `return` deep inside
# the impl) without threading the values back up as return values.
_SXID_POD_NAME=""
_SXID_NS=""

phase_sxid_fault_inject() {
  ctrf_init "aicr-uat"
  local start_ms
  start_ms="$(ctrf_now_ms)"

  local rc=0
  _sxid_fault_inject_impl || rc=$?

  if [[ -n "${_SXID_POD_NAME}" ]]; then
    kubectl delete pod "${_SXID_POD_NAME}" -n "${_SXID_NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi

  local status message
  if (( rc == 0 )); then
    status=passed
    message=""
  else
    status=failed
    message="phase_sxid_fault_inject exited ${rc}; see the job log above for the failing stage"
  fi
  ctrf_add sxidfault "${status}" "$(ctrf_elapsed_ms "${start_ms}")" "${message}"
  if ! ctrf_write sxidfault-result.json; then
    echo "::warning::failed to write sxidfault-result.json; the sxidfault outcome is unaffected" >&2
  fi

  return "${rc}"
}

_sxid_fault_inject_impl() {
  local node_selector="${FAULT_INJECT_NODE_SELECTOR:-nvidia.com/gpu.present=true}"
  local nvs_ns="${FAULT_INJECT_NVSENTINEL_NAMESPACE:-nvsentinel}"
  local condition="${FAULT_INJECT_CONDITION:-SysLogsXIDError}"
  local breaker_cm="${FAULT_INJECT_CIRCUIT_BREAKER_CONFIGMAP:-fault-quarantine-circuit-breaker}"
  local breaker_field="${FAULT_INJECT_CIRCUIT_BREAKER_FIELD:-state}"
  local poll_wait_seconds="${FAULT_INJECT_MONITOR_POLL_SECONDS:-60}"
  local detect_timeout_seconds="${FAULT_INJECT_DETECT_TIMEOUT_SECONDS:-300}"
  local remediation_enabled="${FAULT_INJECT_REMEDIATION_ENABLED:-false}"
  local janitor_enabled="${FAULT_INJECT_JANITOR_ENABLED:-false}"
  local quarantine_timeout_seconds="${FAULT_INJECT_QUARANTINE_TIMEOUT_SECONDS:-180}"
  local recovery_timeout_seconds="${FAULT_INJECT_RECOVERY_TIMEOUT_SECONDS:-900}"
  _SXID_NS="${nvs_ns}"
  _SXID_POD_NAME="sxid-fault-inject-${RUN_ID}"

  echo "::group::Fault-injection circuit-breaker precheck"
  local breaker_json="" breaker_rc=0
  breaker_json="$(kubectl get configmap "${breaker_cm}" -n "${nvs_ns}" -o json 2>/tmp/breaker-err.log)" || breaker_rc=$?
  if (( breaker_rc != 0 )); then
    if grep -qi "notfound" /tmp/breaker-err.log; then
      echo "circuit-breaker ConfigMap ${breaker_cm} not found in ${nvs_ns} -- expected with fault quarantine disabled (no shipped recipe enables it yet); continuing"
    else
      cat /tmp/breaker-err.log >&2
      echo "::error::could not read circuit-breaker ConfigMap ${breaker_cm} in ${nvs_ns} (see above); failing closed rather than assuming the breaker is clear" >&2
      rm -f /tmp/breaker-err.log
      ctrf_add sxidfault-detect failed 0 "circuit-breaker precheck failed closed"
      return 1
    fi
  else
    local breaker_state
    breaker_state="$(jq -r --arg f "${breaker_field}" '.data[$f] // "UNKNOWN"' <<<"${breaker_json}")"
    echo "circuit-breaker ${breaker_cm}.${breaker_field}=${breaker_state}"
    if [[ "${breaker_state}" == "TRIPPED" ]]; then
      echo "::error::fault-quarantine circuit breaker is TRIPPED; refusing to inject another fault" >&2
      rm -f /tmp/breaker-err.log
      ctrf_add sxidfault-detect failed 0 "circuit breaker TRIPPED"
      return 1
    fi
  fi
  rm -f /tmp/breaker-err.log
  echo "::endgroup::"

  echo "::group::Select a GPU node"
  local candidates
  candidates="$(kubectl get nodes -l "${node_selector}" -o json | jq -r '
    .items[]
    | select(.spec.unschedulable != true)
    | select((.status.conditions[]? | select(.type=="Ready") | .status) == "True")
    | .metadata.name')"
  if [[ -z "${candidates}" ]]; then
    echo "::error::no Ready, uncordoned, GPU-labeled (${node_selector}) node found; cannot run fault injection" >&2
    ctrf_add sxidfault-detect failed 0 "no eligible GPU node"
    return 1
  fi

  local node pod_ready=""
  for node in ${candidates}; do
    pod_ready="$(kubectl get pods -n "${nvs_ns}" --field-selector "spec.nodeName=${node}" -o json | jq -r '
      [.items[]
        | select(.metadata.ownerReferences[]?.name // "" | startswith("syslog-health-monitor"))
        | (.status.conditions[]? | select(.type=="Ready") | .status) == "True"]
      | any')"
    if [[ "${pod_ready}" == "true" ]]; then
      break
    fi
    node=""
  done
  if [[ -z "${node}" ]]; then
    echo "::error::no candidate node has a Ready syslog-health-monitor pod; cannot run fault injection" >&2
    ctrf_add sxidfault-detect failed 0 "no node with a Ready syslog-health-monitor pod"
    return 1
  fi
  echo "selected node: ${node}"

  local pre_state
  pre_state="$(kubectl get node "${node}" -o jsonpath="{.status.conditions[?(@.type==\"${condition}\")].status}")"
  if [[ "${pre_state}" == "True" ]]; then
    echo "::error::${node} already carries ${condition}=True before injection; a pre-existing condition cannot produce a trustworthy pass" >&2
    ctrf_add sxidfault-detect failed 0 "${node} already carried ${condition}=True before injection"
    return 1
  fi

  local pre_boot_id
  pre_boot_id="$(kubectl get node "${node}" -o jsonpath='{.status.nodeInfo.bootID}')"
  echo "::endgroup::"

  echo "::group::Wait for syslog-health-monitor's first poll"
  sleep "${poll_wait_seconds}"
  echo "::endgroup::"

  local runtime_class="nvidia"
  if kubectl get runtimeclass nvidia-container-runtime >/dev/null 2>&1; then
    runtime_class="nvidia-container-runtime"
  fi

  echo "::group::Inject synthetic XID 79"
  cat <<MANIFEST | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${_SXID_POD_NAME}
  namespace: ${nvs_ns}
  labels:
    app.kubernetes.io/name: sxid-fault-inject
    app.kubernetes.io/part-of: aicr-uat
spec:
  nodeName: ${node}
  runtimeClassName: ${runtime_class}
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: inject
      image: busybox:1.36
      securityContext:
        privileged: true
      command: ["sleep", "300"]
      volumeMounts:
        - name: host-root
          mountPath: /host
  volumes:
    - name: host-root
      hostPath:
        path: /
MANIFEST

  if ! kubectl wait pod "${_SXID_POD_NAME}" -n "${nvs_ns}" --for=condition=Ready --timeout=120s; then
    echo "::error::fault-injection debug pod did not become Ready" >&2
    kubectl describe pod "${_SXID_POD_NAME}" -n "${nvs_ns}" || true
    ctrf_add sxidfault-detect failed 0 "debug pod never became Ready"
    return 1
  fi

  local kmsg_line="<3>NVRM: Xid (PCI:0000:00:00): 79, pid=1, Channel ID 00000000 intr 00000000 (GPU has fallen off the bus)."
  if ! kubectl exec "${_SXID_POD_NAME}" -n "${nvs_ns}" -- chroot /host sh -c "echo '${kmsg_line}' > /dev/kmsg"; then
    echo "::error::failed to write synthetic XID to ${node}'s /dev/kmsg" >&2
    ctrf_add sxidfault-detect failed 0 "could not write to ${node}'s /dev/kmsg"
    return 1
  fi
  echo "wrote synthetic XID 79 to ${node}:/dev/kmsg"
  echo "::endgroup::"

  echo "::group::Stage 1 -- wait for ${condition}=True"
  local waited=0
  local state="${pre_state}"
  while (( waited < detect_timeout_seconds )); do
    state="$(kubectl get node "${node}" -o jsonpath="{.status.conditions[?(@.type==\"${condition}\")].status}")"
    if [[ "${state}" == "True" ]]; then
      break
    fi
    sleep 10
    waited=$(( waited + 10 ))
  done
  echo "::endgroup::"

  if [[ "${state}" != "True" ]]; then
    echo "::error::${condition} did not appear on ${node} within ${detect_timeout_seconds}s of injection -- a missing signal is a FAIL, not a pass" >&2
    ctrf_add sxidfault-detect failed "$(( waited * 1000 ))" "${condition} never reached True"
    return 1
  fi
  echo "Stage 1 (detection) OK: ${node} carries ${condition}=True ${waited}s after injection"
  ctrf_add sxidfault-detect passed "$(( waited * 1000 ))" ""

  if [[ "${remediation_enabled}" != "true" ]]; then
    echo "Stage 2 (quarantine) SKIPPED: fault quarantine is not enabled on this recipe (#2609 has not landed; FAULT_INJECT_REMEDIATION_ENABLED is unset/false)"
    ctrf_add sxidfault-quarantine skipped 0 "fault quarantine not enabled"
  else
    echo "::group::Stage 2 -- wait for ${node} to be cordoned"
    local qwaited=0 cordoned=false
    while (( qwaited < quarantine_timeout_seconds )); do
      if [[ "$(kubectl get node "${node}" -o jsonpath='{.spec.unschedulable}')" == "true" ]]; then
        cordoned=true
        break
      fi
      sleep 10
      qwaited=$(( qwaited + 10 ))
    done
    echo "::endgroup::"
    if [[ "${cordoned}" != "true" ]]; then
      echo "::error::${node} was not cordoned within ${quarantine_timeout_seconds}s of ${condition}=True, with remediation enabled -- a missing quarantine is a FAIL" >&2
      ctrf_add sxidfault-quarantine failed "$(( qwaited * 1000 ))" "${node} was never cordoned"
      return 1
    fi
    echo "Stage 2 (quarantine) OK: ${node} cordoned ${qwaited}s after detection"
    ctrf_add sxidfault-quarantine passed "$(( qwaited * 1000 ))" ""
  fi

  if [[ "${remediation_enabled}" != "true" || "${janitor_enabled}" != "true" ]]; then
    echo "Stage 3 (recovery) SKIPPED: janitor-driven reboot is not enabled on this recipe (FAULT_INJECT_REMEDIATION_ENABLED and/or FAULT_INJECT_JANITOR_ENABLED unset/false)"
    ctrf_add sxidfault-recovery skipped 0 "janitor-driven recovery not enabled"
  else
    echo "::group::Stage 3 -- wait for ${node}'s boot ID to change (reboot)"
    local rwaited=0 rebooted=false post_boot_id=""
    while (( rwaited < recovery_timeout_seconds )); do
      post_boot_id="$(kubectl get node "${node}" -o jsonpath='{.status.nodeInfo.bootID}' 2>/dev/null || true)"
      if [[ -n "${post_boot_id}" && "${post_boot_id}" != "${pre_boot_id}" ]]; then
        rebooted=true
        break
      fi
      sleep 15
      rwaited=$(( rwaited + 15 ))
    done
    echo "::endgroup::"
    if [[ "${rebooted}" != "true" ]]; then
      echo "::error::${node}'s boot ID did not change within ${recovery_timeout_seconds}s, with remediation+janitor enabled -- a missing reboot is a FAIL" >&2
      ctrf_add sxidfault-recovery failed "$(( rwaited * 1000 ))" "${node} never rebooted (boot ID unchanged)"
      return 1
    fi
    echo "Stage 3 (recovery) OK: ${node} rebooted ${rwaited}s after quarantine (boot ID ${pre_boot_id} -> ${post_boot_id})"
    ctrf_add sxidfault-recovery passed "$(( rwaited * 1000 ))" ""
  fi

  return 0
}
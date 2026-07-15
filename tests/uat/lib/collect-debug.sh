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
# Cloud-agnostic cluster debug-bundle collector, sourced by the per-cloud UAT
# `run` scripts (tests/uat/{aws,gcp}/run) and invoked as the `debug` phase from
# the workflows on failure — AFTER the failing phase, BEFORE teardown, while the
# cluster is still alive.
#
# The pre-existing `Upload failure debug` artifact only captured files already on
# the runner's disk (report.json, recipe, snapshot); it carried ZERO live cluster
# state, so a deployment-phase failure (e.g. a skyhook/nodewright tuning reboot
# re-opening status=in_progress, or a check Job's pod evicted by that reboot) had
# to be reconstructed from raw CI step logs. This collector snapshots the cluster
# state that actually explains those failures: node conditions + taints, the
# operator CRs (Skyhook status history the readiness gate keys off of), cluster
# events (reboots, evictions), and operator/check-Job pod logs (incl. --previous
# to survive a restart across a reboot).
#
# Everything here is BEST-EFFORT: every kubectl call is guarded with `|| true`,
# bounded by a per-command timeout (CLUSTER_DEBUG_CMD_TIMEOUT) so one hung call
# cannot starve later sections, and the whole collector is wrapped so it never
# fails the run or delays teardown (the caller also bounds it with a workflow
# `timeout-minutes`).
#
# Privacy: no Kubernetes Secret objects are fetched, and `kubectl describe pods`
# (which renders literal, non-Secret env values) is limited to the operator/check
# namespaces in CLUSTER_DEBUG_LOG_NAMESPACES — every other namespace gets only
# get/events. This is NOT a guarantee the bundle is credential-free: describe on
# the allowlisted namespaces and pod logs (incl. --previous) can still contain
# app-emitted credentials or PII. The bundle uploads as an Actions artifact on a
# PUBLIC repo (downloadable per the repo's artifact access) — treat it as
# sensitive. Redaction is deliberately not attempted (fragile); the namespace
# allowlist is the primary mitigation.

# Directory the bundle is written into (relative to $PWD, matching serve-logs/
# and train-logs/); the workflow adds `cluster-debug/**` to the upload artifact.
CLUSTER_DEBUG_DIR="${CLUSTER_DEBUG_DIR:-cluster-debug}"

# Readiness-gate time-series log. phase_install appends each gate attempt's
# `validate --phase deployment` output (timestamped) here, so the bundle carries
# the actual status.status progression across the tuning-settling window — the
# complete→in_progress flips AS THEY HAPPEN — rather than only a single snapshot
# taken minutes later at teardown. This is the highest-value signal for the
# skyhook re-tuning race: a teardown snapshot can miss the flip (status may have
# re-converged), but the gate series records every sample.
CLUSTER_DEBUG_GATE_LOG="${CLUSTER_DEBUG_GATE_LOG:-${CLUSTER_DEBUG_DIR}/readiness-gate.log}"

# Per-pod log tail. Bounds bundle size on a busy cluster; 2000 lines is enough to
# see a crash/reboot loop without shipping gigabytes.
CLUSTER_DEBUG_LOG_TAIL="${CLUSTER_DEBUG_LOG_TAIL:-2000}"

# Namespaces whose pod logs are collected in full (operators + the check Jobs
# that fail). Other namespaces still get get/describe/events, just not every log —
# keeps the bundle focused on the GPU/tuning stack that drives deployment-phase
# failures. Space-separated; overridable.
CLUSTER_DEBUG_LOG_NAMESPACES="${CLUSTER_DEBUG_LOG_NAMESPACES:-skyhook gpu-operator nvidia-dra-driver nvsentinel node-feature-discovery kai-scheduler cert-manager monitoring}"

# Cluster-scoped custom resources most relevant to a deployment-phase failure.
# Skyhook is first: its status.status is the non-monotonic signal the readiness
# gate keys off of, and its full YAML carries the per-package/per-node status
# history that explains a re-tuning race. Best-effort — a kind absent on this
# cloud/recipe just no-ops.
CLUSTER_DEBUG_CLUSTER_RESOURCES="${CLUSTER_DEBUG_CLUSTER_RESOURCES:-skyhooks.skyhook.nvidia.com clusterpolicies.nvidia.com nodefeatures.nfd.k8s-sigs.io resourceslices.resource.k8s.io deviceclasses.resource.k8s.io}"

# Per-command wall-clock bound. A single slow/unreachable kubectl call (stale
# creds, an apiserver hiccup, describe over many nodes) must not consume the whole
# step-level timeout and starve later, higher-value sections (per-namespace pod
# logs). 30s is generous for a single Get/describe.
CLUSTER_DEBUG_CMD_TIMEOUT="${CLUSTER_DEBUG_CMD_TIMEOUT:-30}"

# _cd_bounded runs an EXTERNAL command under CLUSTER_DEBUG_CMD_TIMEOUT when the
# coreutils `timeout` is present (Linux CI runners), else runs it directly so the
# collector stays portable to macOS dev boxes that lack `timeout`. Only wrap
# external commands (kubectl, bash -c) with this — `timeout` cannot invoke a shell
# function, so self-bounding functions call _cd_bounded on their own kubectl
# instead of being passed to it.
_cd_bounded() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "${CLUSTER_DEBUG_CMD_TIMEOUT}" "$@"
  else
    "$@"
  fi
}

# _cd_node_reboot_fingerprint prints a one-line-per-node reboot fingerprint:
# bootID (changes across a reboot), kernel version, and the Ready condition's
# lastTransitionTime (a recent transition ⇒ the kubelet just came back). A skyhook
# tuning package with interrupt:reboot shows up here as a fresh Ready transition /
# changed bootID, which is the durable proof that a reboot — not a genuine resource
# gap — re-opened status.status. Never fails.
_cd_node_reboot_fingerprint() {
  _cd_bounded kubectl get nodes -o json 2>/dev/null | jq -r '.items[]
    | "\(.metadata.name)  bootID=\(.status.nodeInfo.bootID)  kernel=\(.status.nodeInfo.kernelVersion)  Ready.lastTransition=\([.status.conditions[]|select(.type=="Ready")|.lastTransitionTime][0])  taints=\(.spec.taints // [])"' 2>/dev/null || true
}

# capture_skyhook_snapshot dumps a focused, FAST skyhook/node snapshot the moment
# a failure is detected in the hot path (called inline from phase_conformance on a
# validate failure), seconds after the chainsaw assert gives up — while
# status.status is most likely STILL in_progress. Contrast collect_cluster_debug,
# which runs at the teardown-adjacent failure step minutes later, by when the CR
# may have re-converged to complete and hidden the flip. Best-effort; never fails
# the run. $1 is a short label distinguishing multiple captures.
capture_skyhook_snapshot() {
  local label="${1:-snapshot}"
  command -v kubectl >/dev/null 2>&1 || return 0
  mkdir -p "${CLUSTER_DEBUG_DIR}"
  local out="${CLUSTER_DEBUG_DIR}/skyhook-at-failure-${label}.txt"
  echo "::group::Capture skyhook snapshot at failure (${label})"
  {
    echo "##### skyhook snapshot (${label}) @ $(date -u +%Y-%m-%dT%H:%M:%SZ) #####"
    echo "# Captured inline seconds after the failure — status.status here is the"
    echo "# state at (or nearest to) the moment the check failed, not a re-converged"
    echo "# teardown-time reading."
    echo
    echo "----- Skyhook CRs (full YAML: per-package/per-node status) -----"
    _cd_bounded kubectl get skyhooks.skyhook.nvidia.com -A -o yaml 2>&1 || true
    echo
    echo "----- node reboot fingerprint (bootID / kernel / Ready transition / taints) -----"
    _cd_node_reboot_fingerprint
    echo
    echo "----- skyhook namespace pods (tuning package pods) -----"
    _cd_bounded kubectl get pods -n skyhook -o wide 2>&1 || true
    echo "----- skyhook namespace events (by time) -----"
    _cd_bounded kubectl get events -n skyhook --sort-by=.lastTimestamp 2>&1 || true
  } | tee "${out}"
  echo "::endgroup::"
  return 0
}

# _cd_section runs a labeled command group, teeing to both the step log (folded
# in an Actions ::group::) and a file in the bundle. The command is bounded by
# CLUSTER_DEBUG_CMD_TIMEOUT (via _cd_bounded), so a single hung call can't starve
# later sections. Callers must pass an EXTERNAL command (kubectl / bash -c), never
# a bare shell function — `timeout` cannot invoke a function; route function-based
# sections directly (see the node-reboot-fingerprint section). Never fails.
_cd_section() {
  local label="$1" outfile="$2"; shift 2
  {
    echo "##### ${label} #####"
    echo "\$ $*"
    _cd_bounded "$@" 2>&1 || true
    echo
  } | tee -a "${CLUSTER_DEBUG_DIR}/${outfile}"
}

# collect_cluster_debug snapshots live cluster state into CLUSTER_DEBUG_DIR.
# Best-effort and self-bounding: safe to call from an `if: failure()` step.
collect_cluster_debug() {
  mkdir -p "${CLUSTER_DEBUG_DIR}"
  echo "::group::Collect cluster debug bundle -> ${CLUSTER_DEBUG_DIR}/"

  # --- Self-describing manifest: what failed, on what, when ------------------
  # So the bundle is diagnosable standalone, without cross-referencing CI logs.
  {
    echo "# UAT cluster debug bundle"
    echo "generatedAt: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "runId: ${RUN_ID:-unknown}"
    echo "config: ${config:-unknown}"
    if [[ -f recipe.yaml ]]; then
      # The emitted recipe carries no metadata.name; its identity is the leaf
      # (last) applied overlay. Fall back to metadata.name then "unknown".
      echo "recipe: $(yq -r '.metadata.appliedOverlays[-1] // .metadata.name // "unknown"' recipe.yaml 2>/dev/null || echo unknown)"
      echo "criteria: $(yq -o=json -I=0 '.criteria // {}' recipe.yaml 2>/dev/null || echo '{}')"
    fi
    # Surface the failing check(s) straight from report.json so a reader starts
    # at the smoking gun rather than grepping. `other` counts as failing (gates
    # treat it so); include its message (e.g. "pod for job not found").
    if [[ -f report.json ]]; then
      echo "failingChecks:"
      jq -r '.results.tests[]? | select(.status=="failed" or .status=="other")
             | "  - name: \(.name)\n    status: \(.status)\n    message: \(.message // "none")"' \
        report.json 2>/dev/null || echo "  (report.json unparseable)"
    fi
    # Point the reader at the two highest-value time-series artifacts if present:
    # the readiness-gate status.status progression (written during phase_install)
    # and any inline skyhook-at-failure snapshot(s).
    [[ -f "${CLUSTER_DEBUG_GATE_LOG}" ]] && echo "readinessGateSeries: $(basename "${CLUSTER_DEBUG_GATE_LOG}")"
    local snap
    for snap in "${CLUSTER_DEBUG_DIR}"/skyhook-at-failure-*.txt; do
      [[ -f "${snap}" ]] && echo "skyhookAtFailure: $(basename "${snap}")"
    done
  } | tee "${CLUSTER_DEBUG_DIR}/MANIFEST.yaml"

  if ! command -v kubectl >/dev/null 2>&1; then
    echo "kubectl not on PATH; skipping live cluster collection" \
      | tee -a "${CLUSTER_DEBUG_DIR}/MANIFEST.yaml"
    echo "::endgroup::"
    return 0
  fi

  # --- Tier 1: cluster-wide state -------------------------------------------
  # Nodes carry the reboot/taint evidence central to tuning-race failures.
  _cd_section "nodes (wide)" nodes.txt kubectl get nodes -o wide
  _cd_section "nodes (yaml)" nodes.yaml kubectl get nodes -o yaml
  _cd_section "nodes (describe)" nodes-describe.txt kubectl describe nodes
  # Reboot fingerprint + taints: bootID (changes across a reboot), kernel, the
  # Ready condition's lastTransitionTime, and taints. The skyhook.nvidia.com
  # NoSchedule taint applied during tuning and removed on completion — plus a
  # fresh Ready transition / changed bootID — is the durable "a reboot re-opened
  # tuning" signal that distinguishes an in-flight reboot from a genuine resource
  # gap. Routed directly (not via _cd_section) because it is a shell function,
  # which `timeout` cannot invoke; the function bounds its own kubectl internally.
  {
    echo "##### node reboot fingerprint + taints #####"
    _cd_node_reboot_fingerprint
    echo
  } | tee -a "${CLUSTER_DEBUG_DIR}/node-reboot-fingerprint.txt"
  # Events across all namespaces: node Reboot/NotReady, pod evictions (the run-2
  # "pod for job not found" cause), FailedScheduling, etc.
  _cd_section "events (all namespaces, by time)" events.txt \
    kubectl get events -A --sort-by=.lastTimestamp
  _cd_section "pods (all namespaces, wide)" pods.txt kubectl get pods -A -o wide
  _cd_section "pods not Running/Completed" pods-notready.txt \
    bash -c "kubectl get pods -A 2>/dev/null | grep -Ev '\\s+Running\\s+|\\s+Completed\\s+' || true"

  # --- Tier 1: operator custom resources (full YAML) ------------------------
  # Skyhook first — the status history that the readiness gate's status.status
  # check reads. Full YAML captures per-package/per-node state + timestamps.
  for res in ${CLUSTER_DEBUG_CLUSTER_RESOURCES}; do
    _cd_section "CR ${res} (yaml)" "cr-${res%%.*}.yaml" \
      kubectl get "${res}" -A -o yaml
  done
  # Platform workload CRs (Dynamo / Kubeflow) if the CRD exists.
  for res in dynamographdeployments.nvidia.com trainjobs.trainer.kubeflow.org; do
    _cd_bounded kubectl get crd "${res}" >/dev/null 2>&1 || continue
    _cd_section "CR ${res} (yaml)" "cr-${res%%.*}.yaml" \
      kubectl get "${res}" -A -o yaml
  done

  # --- Tier 2: per-namespace describe/events + operator/check-Job logs -------
  local ns
  for ns in $(_cd_bounded kubectl get ns -o name 2>/dev/null | sed 's|namespace/||'); do
    local nsfile="ns-${ns}.txt"
    _cd_section "[${ns}] pods (wide)" "${nsfile}" \
      kubectl get pods -n "${ns}" -o wide
    _cd_section "[${ns}] jobs" "${nsfile}" \
      kubectl get jobs -n "${ns}" -o wide
    _cd_section "[${ns}] events (by time)" "${nsfile}" \
      kubectl get events -n "${ns}" --sort-by=.lastTimestamp

    # `describe pods` and full logs are limited to the operator/check namespaces
    # in the allowlist. `describe` renders literal (non-Secret) env values in its
    # Environment section; scoping it to trusted operator namespaces — rather than
    # every namespace — bounds credential/PII exposure in the public-repo artifact
    # (a UAT recipe override or third-party chart with a literal-env secret would
    # otherwise leak straight in). The get/events above still cover every namespace.
    case " ${CLUSTER_DEBUG_LOG_NAMESPACES} " in
      *" ${ns} "*) ;;
      *) continue ;;
    esac
    _cd_section "[${ns}] describe pods" "${nsfile}" \
      kubectl describe pods -n "${ns}"
    local logfile="logs-${ns}.txt" p
    for p in $(_cd_bounded kubectl get pods -n "${ns}" -o name 2>/dev/null); do
      {
        echo "===== ${ns}/${p} (current) ====="
        _cd_bounded kubectl logs -n "${ns}" "${p#pod/}" --all-containers \
          --tail="${CLUSTER_DEBUG_LOG_TAIL}" 2>&1 || true
        # --previous survives a container restart across a reboot — the log that
        # actually explains a tuning-reboot crash is usually the previous one.
        echo "===== ${ns}/${p} (previous, if any) ====="
        _cd_bounded kubectl logs -n "${ns}" "${p#pod/}" --all-containers --previous \
          --tail="${CLUSTER_DEBUG_LOG_TAIL}" 2>&1 || true
      } >> "${CLUSTER_DEBUG_DIR}/${logfile}"
    done
  done

  echo "cluster debug bundle written to ${CLUSTER_DEBUG_DIR}/ ($(du -sh "${CLUSTER_DEBUG_DIR}" 2>/dev/null | cut -f1))"
  echo "::endgroup::"
  return 0
}

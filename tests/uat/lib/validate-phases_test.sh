#!/bin/bash
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

# Guards VALIDATE_PHASES, the knob that decides which `aicr validate` phases a
# lane runs and, with them, whether the post-install readiness gate runs.
#
# TWO FAILURES ARE WORTH DIFFERENT AMOUNTS HERE, and the cases below are sized
# accordingly.
#
#   Dropping the gate on a lane that needs it is the expensive one: the cloud
#   lanes' readiness gate is what makes them wait out nodewright reboots and
#   gpu-operator convergence, and without it they validate a half-converged
#   cluster. So the DEFAULT is asserted first, and asserted to be "all".
#
#   Restoring it on the simulated lane is the cheap-but-wasteful one: that lane
#   cannot pass the deployment phase (it deploys a subset of its recipe), so the
#   gate would poll for a DRA kubelet plugin that is never installed until
#   READINESS_TIMEOUT_SECONDS. Sixty minutes to reach a known answer.
#
# The knob and the gate are asserted to move TOGETHER. Two independent switches
# would let a tree exist where the conformance run skips the deployment phase
# while the gate still blocks on it, which is the worst of both.
#
# THE PREDICATE IS NOT THE KNOB. Asserting validate_runs_deployment_phase in
# isolation says nothing about the two places that consult it, and both survive
# being disconnected: `if true` in phase_install silently restores the readiness
# gate, and a hardcoded `--phase all` in phase_conformance silently restores the
# phase set. Each call site therefore gets a case that drives the phase function
# itself, with the boundary commands (the deployer, the gate, kubectl, the aicr
# binary) replaced and nothing else.
#
# Hermetic: sources phases.sh, which defines constants and functions only, reads
# run-sim as text, and drives the two phase functions against fixture files under
# a run-scoped scratch directory. No cluster, no aicr binary.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_SIM="${SCRIPT_DIR}/../kind/run-sim"

# Run-scoped, not a fixed basename: a fixed one lets an earlier run's leftovers
# be read as this run's fixtures. mktemp is deliberately avoided -- it is denied
# in some sandboxes, and this suite has to run in all of them.
SCRATCH="${TMPDIR:-/tmp}/aicr-validate-phases-test-$$"
rm -rf "${SCRATCH}"
mkdir -p "${SCRATCH}"
trap 'rm -rf "${SCRATCH}"' EXIT

# The one field phase_install reads from the lane config.
CONFIG="${SCRATCH}/config.yaml"
cat >"${CONFIG}" <<'YAML'
spec:
  bundle:
    deployment:
      deployer: helmfile
YAML

fail=0
check() {
    local desc="$1" want="$2" got="$3"
    if [[ "$want" != "$got" ]]; then
        echo "FAIL: ${desc}: want '${want}', got '${got}'" >&2
        fail=1
    else
        echo "ok: ${desc}"
    fi
}

# phases_with <value>
#
# Sources phases.sh in a SUBSHELL with VALIDATE_PHASES preset (empty means
# unset, exercising the default), and prints "<effective>|<gate>". Sourcing in a
# subshell keeps each case from inheriting the previous one's value, which is
# exactly the leak that would make every case agree with the first.
phases_with() {
    local preset="${1:-}"
    (
        if [[ -n "${preset}" ]]; then
            export VALIDATE_PHASES="${preset}"
        else
            unset VALIDATE_PHASES
        fi
        # shellcheck source=./phases.sh
        source "${SCRIPT_DIR}/phases.sh" >/dev/null 2>&1
        if validate_runs_deployment_phase; then
            printf '%s|gate-on' "${VALIDATE_PHASES}"
        else
            printf '%s|gate-off' "${VALIDATE_PHASES}"
        fi
    )
}

# --- the default is what the cloud lanes depend on -------------------------
check "unset defaults to all, with the gate on" "all|gate-on" "$(phases_with)"
check "an explicit all keeps the gate on" "all|gate-on" "$(phases_with all)"
# A lane that names the deployment phase outright still gets the gate: the gate
# IS that phase in a retry loop, so the two cannot disagree.
check "naming deployment keeps the gate on" "deployment|gate-on" "$(phases_with deployment)"
check "deployment among several keeps the gate on" "deployment,conformance|gate-on" \
    "$(phases_with deployment,conformance)"

# --- narrowing away from deployment takes the gate with it ------------------
check "conformance alone turns the gate off" "conformance|gate-off" "$(phases_with conformance)"
check "performance alone turns the gate off" "performance|gate-off" "$(phases_with performance)"

# --- CALL SITE 1: phase_install and the readiness gate ----------------------
#
# Drives phase_install with the deployer bodies, the gate and kubectl replaced,
# and reports whether the gate ran. The gate is what makes a lane wait out
# operator convergence, so "did it run" is the behaviour, not "is the predicate
# true".
install_gate_with() {
    local preset="${1:-}"
    (
        if [[ -n "${preset}" ]]; then
            export VALIDATE_PHASES="${preset}"
        else
            unset VALIDATE_PHASES
        fi
        # shellcheck source=./phases.sh
        source "${SCRIPT_DIR}/phases.sh" >/dev/null 2>&1
        export config="${CONFIG}"
        install_helmfile() { :; }
        install_argocd()   { echo "argocd-install-ran"; }
        install_readiness_gate() { echo "readiness-gate-ran"; }
        kubectl() { :; }
        phase_install 2>&1
    ) | grep -c 'readiness-gate-ran' | tr -d ' '
}

check "the default runs the readiness gate" "1" "$(install_gate_with)"
check "an explicit all runs the readiness gate" "1" "$(install_gate_with all)"
check "naming deployment runs the readiness gate" "1" "$(install_gate_with deployment)"
# The discriminating one: this is the lane that cannot pass the phase, and a
# gate that still runs here polls for a DRA kubelet plugin that is never
# installed until READINESS_TIMEOUT_SECONDS.
check "conformance alone does not run the readiness gate" "0" "$(install_gate_with conformance)"
# A skip nobody can see in the log is indistinguishable from a bug in the
# script, so the skip announces itself and names the cost.
check "the skipped gate says the lane does not assert deployment readiness" "1" \
    "$(
        (
            export VALIDATE_PHASES="conformance"
            # shellcheck source=./phases.sh
            source "${SCRIPT_DIR}/phases.sh" >/dev/null 2>&1
            export config="${CONFIG}"
            install_helmfile() { :; }
            install_readiness_gate() { :; }
            kubectl() { :; }
            phase_install 2>&1
        ) | grep -c 'DOES NOT ASSERT DEPLOYMENT' | tr -d ' '
    )"

# --- CALL SITE 2: phase_conformance and the --phase argument ----------------
#
# Drives phase_conformance and reports the --phase value the aicr binary was
# actually invoked with. AICR_BIN is set to the name of a shell function, so the
# invocation `"${AICR_BIN}" validate ...` resolves to it and the arguments are
# recorded without writing an executable.
#
# Reading the recorded argument, and not the `::group::Validate (phases: ...)`
# banner printed two lines above it, is the point: the banner interpolates
# VALIDATE_PHASES directly, so it keeps telling the truth even when the
# invocation below it does not.
conformance_phase_arg() {
    local preset="${1:-}"
    local work="${SCRATCH}/conf-${preset:-default}"
    rm -rf "${work}"
    mkdir -p "${work}"
    (
        cd "${work}" || exit 1
        if [[ -n "${preset}" ]]; then
            export VALIDATE_PHASES="${preset}"
        else
            unset VALIDATE_PHASES
        fi
        # shellcheck source=./phases.sh
        source "${SCRIPT_DIR}/phases.sh" >/dev/null 2>&1
        export config="${CONFIG}"
        printf 'criteria:\n  platform: slurm\n' >recipe.yaml
        inject_push_target() { :; }
        assert_gpu_census()  { return 0; }
        kubectl() { return 0; }
        export AICR_BIN="recorded_aicr"
        recorded_aicr() {
            printf '%s\n' "$*" >"${work}/aicr-args"
            mkdir -p ./evidence
            printf 'pointer\n' >./evidence/pointer.yaml
        }
        phase_conformance
    ) >/dev/null 2>&1
    if [[ ! -f "${work}/aicr-args" ]]; then
        echo "<aicr never invoked>"
        return
    fi
    sed -n 's/.*--phase \([^ ]*\).*/\1/p' "${work}/aicr-args"
}

check "the default validates all phases" "all" "$(conformance_phase_arg)"
check "an explicit all validates all phases" "all" "$(conformance_phase_arg all)"
# The discriminating one: a hardcoded --phase all here would put the deployment
# phase back into the run this lane narrowed it out of, and the run would stall
# on the DRA poll rather than reach a verdict.
check "conformance alone validates only conformance" "conformance" \
    "$(conformance_phase_arg conformance)"
check "a multi-phase value is passed through verbatim" "deployment,conformance" \
    "$(conformance_phase_arg deployment,conformance)"

# --- the simulated lane sets it, and the cloud lanes do not -----------------
#
# Read from run-sim as text rather than by running it: running it would need a
# cluster. The assertion is that the value is SET THERE, because a lane that
# relied on the workflow to export it would behave differently under a local
# invocation of the same shim.
check "run-sim narrows the simulated lane to conformance" "1" \
    "$(grep -c '^VALIDATE_PHASES="conformance"$' "${RUN_SIM}" | tr -d ' ')"
check "run-sim exports it, so phases.sh sees it" "1" \
    "$(grep -c '^export .*VALIDATE_PHASES' "${RUN_SIM}" | tr -d ' ')"
# The nvkind sibling must NOT carry it: that lane deploys the whole recipe and
# needs the gate. If this ever fires, the value was copied rather than reasoned.
check "the nvkind lane does not set it" "0" \
    "$(grep -c 'VALIDATE_PHASES' "${SCRIPT_DIR}/../kind/run" | tr -d ' ')"

# --- the skip is documented where a reader meets it -------------------------
#
# The point of this one is that "the gate is off" and "here is why, and what it
# costs" ship together. A skip with no explanation gets restored by the next
# person, who then spends an afternoon rediscovering the 8-minute DRA poll.
check "run-sim says the lane does not assert deployment readiness" "1" \
    "$(grep -c 'DOES NOT ASSERT' "${RUN_SIM}" | tr -d ' ')"
check "run-sim names the divergence from the cloud lanes" "1" \
    "$(grep -ci 'DIVERGES from the cloud lanes' "${RUN_SIM}" | tr -d ' ')"

if [[ "${fail}" -ne 0 ]]; then
    echo "validate-phases_test.sh: FAILED" >&2
    exit 1
fi
echo "validate-phases_test.sh: all checks passed"

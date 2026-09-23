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

# Guards both gates on the `platform` dispatch axis:
#
#   A. .github/workflows/uat-run.yaml, `resolve` job, "Resolve reservation row".
#      Answers "can this LANE serve a platform at all". Only run-kind forwards
#      the input; uat-aws/gcp/azure.yaml declare no platform input and build
#      TEST_CONFIG from intent alone, so a platform dispatched at a cloud
#      reservation used to be dropped at the job boundary and that cell ran its
#      default per-intent config to completion and reported SUCCESS for a recipe
#      nobody asked for. Nothing on that path failed, because the default config
#      exists, so the pipelines' own "test config not found" check never fired.
#
#   B. .github/workflows/uat-kind.yaml, `uat-kind` job, "Validate inputs".
#      Answers "is this a value the kind lane admits", which today is the empty
#      one and nothing else: that lane's cluster is single-node nvkind and the
#      only platform in the tree, slurm, needs four schedulable nodes. Admitting
#      it would spend a slot on a leased GPU runner before failing on three
#      Pending slurmd pods. The cell runs on uat-kind-sim.yaml instead.
#
# The two are deliberately separate: A is per-lane, B is per-value. Asserting
# both here keeps them from being merged into one duplicated list that drifts.
#
# Why it reads the workflows instead of holding a copy: each subject is
# EXTRACTED with yq at run time and executed verbatim, so this file cannot drift
# from the steps it protects. A transcribed copy would keep passing after
# someone edited the real one.
#
# Hermetic: `tools/uat-broker` is a local Go program that gate A's step builds
# from source itself, and `infra/uat/reservations.yaml` and the test configs are
# in-repo. No network, no cluster, no prebuilt binary.
#
# It never skips. A missing yq, a missing go, or a step that cannot be extracted
# is a FAILURE, not a skip: a suite that switches itself off when a dependency
# is absent reports green while checking nothing.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subjects from SCRIPT_DIR, not from the caller's cwd, so the files
# under test are the ones in THIS worktree however the suite is invoked.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
RUN_WORKFLOW="${REPO_ROOT}/.github/workflows/uat-run.yaml"
KIND_WORKFLOW="${REPO_ROOT}/.github/workflows/uat-kind.yaml"

fail=0
check() {
    local desc="$1" want="$2" got="$3"
    if [[ "${want}" != "${got}" ]]; then
        echo "FAIL: ${desc}: want '${want}', got '${got}'" >&2
        fail=1
    else
        echo "ok: ${desc}"
    fi
}

# Hard preconditions. Each aborts the suite rather than skipping a case.
abort() { echo "FAIL: $*" >&2; exit 1; }
command -v yq >/dev/null 2>&1 || abort "yq is not on PATH; it is required to extract the steps under test"
command -v go >/dev/null 2>&1 || abort "go is not on PATH; the extracted resolve step builds tools/uat-broker from source"
[[ -f "${RUN_WORKFLOW}" ]] || abort "workflow not found: ${RUN_WORKFLOW}"
[[ -f "${KIND_WORKFLOW}" ]] || abort "workflow not found: ${KIND_WORKFLOW}"

WORK="$(mktemp -d)" || abort "could not create a temporary directory"
trap 'rm -rf "${WORK}"' EXIT

# extract_step <workflow> <yq job selector> <step name> <dest> <marker>
#
# yq prints the literal "null" (exit 0) when the selection matches nothing, so
# an empty or null extraction has to be rejected explicitly: a suite run against
# an empty subject would report every case as it pleased. The marker confirms
# the step that came back is the one intended, so a rename cannot silently hand
# the suite a different step.
extract_step() {
    local wf="$1" job="$2" name="$3" dest="$4" marker="$5"
    yq -r "${job}.steps[] | select(.name == \"${name}\") | .run" "${wf}" \
       > "${dest}" 2>"${WORK}/yq.err" \
       || abort "yq failed on ${wf}: $(cat "${WORK}/yq.err")"
    if [[ ! -s "${dest}" ]] || [[ "$(cat "${dest}")" == "null" ]]; then
        abort "could not extract the '${name}' step from ${wf} (renamed or removed?)"
    fi
    grep -q -- "${marker}" "${dest}" \
        || abort "the extracted '${name}' step from ${wf} lacks '${marker}'; wrong step extracted"
    echo "ok: extracted '${name}' from $(basename "${wf}") ($(wc -l < "${dest}" | tr -d ' ') lines)"
}

RESOLVE_STEP="${WORK}/resolve-step.sh"
KIND_STEP="${WORK}/kind-validate-step.sh"
extract_step "${RUN_WORKFLOW}" '.jobs.resolve' 'Resolve reservation row' \
             "${RESOLVE_STEP}" 'uat-broker reservations'
extract_step "${KIND_WORKFLOW}" '.jobs["uat-kind"]' 'Validate inputs' \
             "${KIND_STEP}" 'unsupported platform'

# ---------------------------------------------------------------------------
# Gate A: uat-run.yaml, can this lane serve a platform at all
# ---------------------------------------------------------------------------

# run_resolve <reservation> <platform> -> rc, publishing into ${WORK}/row.txt.
# cwd is the repo root because the step resolves ./tools and ./bin relative to
# it, exactly as it does on a runner.
run_resolve() {
    : > "${WORK}/row.txt"
    ( cd "${REPO_ROOT}" \
      && RESERVATION="$1" SLOT=0 PLATFORM="$2" GITHUB_OUTPUT="${WORK}/row.txt" \
         bash "${RESOLVE_STEP}" ) > "${WORK}/step.log" 2>&1
}
lane_verdict() {
    if run_resolve "$1" "$2"; then echo accept; else echo reject; fi
}

# Preconditions on the registry rows the cases below name. Without these, a
# renamed or deleted reservation would surface as a puzzling accept/reject
# mismatch instead of naming the missing row.
for pair in "aws-h100:aws" "gcp-h100:gcp" "azure-h100:azure" "kind-h100:kind"; do
    res="${pair%%:*}"; want_cloud="${pair##*:}"
    run_resolve "${res}" ""
    check "reservation ${res} resolves and publishes cloud=${want_cloud}" \
        "cloud=${want_cloud}" "$(grep '^cloud=' "${WORK}/row.txt")"
done

# REJECT: a non-empty platform at a lane that cannot serve it. These are the
# false-green dispatches gate A exists to stop.
check "aws lane rejects platform=slurm"   "reject" "$(lane_verdict aws-h100 slurm)"
check "gcp lane rejects platform=slurm"   "reject" "$(lane_verdict gcp-h100 slurm)"
check "azure lane rejects platform=slurm" "reject" "$(lane_verdict azure-h100 slurm)"

# Gate A is an ALLOWLIST of lanes, not a denylist of platform values. These two
# discriminate the shapes: a denylist naming slurm would let both through.
check "aws lane rejects platform=kubeflow, not just slurm" "reject" "$(lane_verdict aws-h100 kubeflow)"
check "aws lane rejects a platform that exists nowhere"    "reject" "$(lane_verdict aws-h100 nosuch)"

# ACCEPT: every path that must keep working. The four empty-platform cases are
# the backward-compatibility guarantee for every cell that predates this input.
check "aws lane accepts an empty platform"   "accept" "$(lane_verdict aws-h100 '')"
check "gcp lane accepts an empty platform"   "accept" "$(lane_verdict gcp-h100 '')"
check "azure lane accepts an empty platform" "accept" "$(lane_verdict azure-h100 '')"
check "kind lane accepts an empty platform"  "accept" "$(lane_verdict kind-h100 '')"
check "kind lane accepts platform=slurm"     "accept" "$(lane_verdict kind-h100 slurm)"

# Deliberate division of responsibility: gate A answers "can this LANE serve a
# platform at all", gate B answers "is this a value the kind lane has a config
# for". Asserting the accept here pins that split.
check "kind lane accepts an unknown platform, value validation is gate B's job" \
    "accept" "$(lane_verdict kind-h100 nosuch)"

# What actually stops a cloud lane from starting is that no `cloud` output is
# published on the rejected path, NOT that the step exits non-zero: run-aws,
# run-gcp and run-azure carry `always()`, which runs a job whose `needs` failed.
# With no output, their `needs.resolve.outputs.cloud == '<cloud>'` compares the
# empty string against a literal and cannot match. Asserting the absence keeps
# a future edit from moving the publish back above the gate.
run_resolve aws-h100 slurm
check "a rejected dispatch publishes NO row at all" "0" \
    "$(wc -l < "${WORK}/row.txt" | tr -d ' ')"
check "a rejected dispatch publishes no cloud output" "0" \
    "$(grep -c '^cloud=' "${WORK}/row.txt")"

# The rejection has to name the platform and the lane, or an operator reading a
# red run has to go source-diving to learn what was wrong with the dispatch.
check "the rejection names the platform" "1" \
    "$(grep -c "platform='slurm'" "${WORK}/step.log")"
check "the rejection names the resolved lane" "1" \
    "$(grep -c "'aws' lane" "${WORK}/step.log")"

# ---------------------------------------------------------------------------
# Gate B: uat-kind.yaml, is this a value the kind lane has a config for
# ---------------------------------------------------------------------------

# Every case below passes an EXISTING TEST_CONFIG, so a rejection can only come
# from the platform allowlist and never from the file-existence check that
# follows it. The one case that probes that check supplies a missing path on
# purpose.
KIND_CONFIG="tests/uat/kind/tests/h100-training-config.yaml"
[[ -f "${REPO_ROOT}/${KIND_CONFIG}" ]] || abort "fixture missing: ${KIND_CONFIG}"

run_kind() { # <platform> [test-config]
    ( cd "${REPO_ROOT}" \
      && INTENT=training PLATFORM="$1" LIFECYCLE=nightly \
         TEST_CONFIG="${2:-${KIND_CONFIG}}" KIND_CLUSTER_NAME=uat-kind-training \
         bash "${KIND_STEP}" ) > "${WORK}/kind.log" 2>&1
}
value_verdict() {
    if run_kind "$@"; then echo accept; else echo reject; fi
}

# The empty value is the backward-compatibility guarantee for every cell that
# predates this input, and it is now the ONLY admitted value.
check "kind allowlist accepts an empty platform" "accept" "$(value_verdict '')"
check "kind allowlist rejects slurm"             "reject" "$(value_verdict slurm)"

# The gate is an allowlist of the values this lane admits, not a denylist that
# names slurm. Written the other way round -- reject slurm, accept the rest --
# it would pass the case above and let all three of these through to a
# TEST_CONFIG path that does not exist.
check "kind allowlist rejects kubeflow" "reject" "$(value_verdict kubeflow)"
check "kind allowlist rejects dynamo"   "reject" "$(value_verdict dynamo)"
check "kind allowlist rejects a platform that exists nowhere" "reject" "$(value_verdict nosuch)"

# Comparison stays verbatim, with no normalization added on top. A rewrite that
# trimmed or stripped whitespace before comparing would map a whitespace-only
# dispatch onto the accepted empty value, and the lane would silently run its
# default config for a request that asked for something else.
check "kind allowlist rejects Slurm"                    "reject" "$(value_verdict Slurm)"
check "kind allowlist rejects a leading-space platform" "reject" "$(value_verdict ' slurm')"
check "kind allowlist rejects a whitespace-only platform" "reject" "$(value_verdict ' ')"

# An operator reading a red run must learn what was wrong AND where the cell
# they wanted actually runs, or the rejection just moves the source-diving.
run_kind Slurm
check "the kind rejection names the offending value" "1" \
    "$(grep -c "unsupported platform: 'Slurm'" "${WORK}/kind.log")"
check "the kind rejection points at the lane that serves the cell" "1" \
    "$(grep -c 'uat-kind-sim.yaml' "${WORK}/kind.log")"

# The file-existence check behind the allowlist must stay fail-closed: it is
# what catches an admitted platform whose config was never added.
check "kind lane rejects an admitted platform whose config is missing" \
    "reject" "$(value_verdict '' 'tests/uat/kind/tests/does-not-exist.yaml')"

if [[ "${fail}" -ne 0 ]]; then
    echo "FAILED" >&2
    exit 1
fi
echo "PASSED"

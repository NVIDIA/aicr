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

# Guards the "Clear an orphaned flow-logs log group" step of
# .github/workflows/uat-aws.yaml. A re-run attempt reuses the run ID, so the
# VPC flow-logs log group name repeats, and a flow-log delivery still in flight
# when the previous attempt was destroyed recreates the group behind it. The
# step deletes that group only when no flow log uses it, matched by name or by
# destination ARN, and leaves a group still in use to Terraform. A failed or
# unreadable lookup stops the run without deleting anything.
#
# The step is EXTRACTED with yq and executed verbatim against a stub `aws`, so
# this file cannot drift from the step it protects. Hermetic: no AWS access.
# A missing yq or jq, or a step that cannot be extracted, is a FAILURE, not a
# skip.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
WORKFLOW="${REPO_ROOT}/.github/workflows/uat-aws.yaml"
STEP_NAME="Clear an orphaned flow-logs log group"

abort() { echo "FAIL: $*" >&2; exit 1; }
command -v yq >/dev/null 2>&1 || abort "yq is not on PATH; it is required to extract the step under test"
command -v jq >/dev/null 2>&1 || abort "jq is not on PATH; the step under test filters AWS output with it"
[[ -f "${WORKFLOW}" ]] || abort "workflow not found: ${WORKFLOW}"

WORK="$(mktemp -d)" || abort "could not create a temporary directory"
trap 'rm -rf "${WORK}"' EXIT

STEP="${WORK}/step.sh"
yq -r ".jobs[\"uat-aws\"].steps[] | select(.name == \"${STEP_NAME}\") | .run" "${WORKFLOW}" \
    > "${STEP}" 2>"${WORK}/yq.err" || abort "yq failed on ${WORKFLOW}: $(cat "${WORK}/yq.err")"
if [[ ! -s "${STEP}" ]] || [[ "$(cat "${STEP}")" == "null" ]]; then
    abort "could not extract the '${STEP_NAME}' step from ${WORKFLOW} (renamed or removed?)"
fi
grep -q -- 'delete-log-group' "${STEP}" \
    || abort "the extracted '${STEP_NAME}' step lacks 'delete-log-group'; wrong step extracted"

# The step must run right before Bringup, under the same condition, or a
# re-run still reaches Bringup with the orphan in place.
steps_json="$(yq -o=json '.jobs["uat-aws"].steps' "${WORKFLOW}")" || abort "yq could not read the job's steps"
next_name="$(jq -r --arg n "${STEP_NAME}" '. as $s | ($s | map(.name) | index($n)) as $i | $s[$i + 1].name' <<<"${steps_json}")"
[[ "${next_name}" == "Bringup Infra" ]] \
    || abort "the step after '${STEP_NAME}' is '${next_name}', want 'Bringup Infra'"
same_if="$(jq -r --arg n "${STEP_NAME}" '(map(select(.name == $n))[0].if) == (map(select(.name == "Bringup Infra"))[0].if)' <<<"${steps_json}")"
[[ "${same_if}" == "true" ]] || abort "'${STEP_NAME}' and 'Bringup Infra' run under different conditions"
echo "ok: the step runs right before Bringup Infra, under the same condition"

mkdir -p "${WORK}/bin"
cat > "${WORK}/bin/aws" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "${AWS_CALLS}"
case "$1 $2" in
    "logs describe-log-groups")
        if [[ "${STUB_DESCRIBE_FAIL:-}" == 1 ]]; then
            echo "An error occurred (AccessDeniedException)" >&2
            exit 254
        fi
        printf '%s\n' "${STUB_LOG_GROUPS}" ;;
    "ec2 describe-flow-logs")
        if [[ "${STUB_FLOW_LOGS_FAIL:-}" == 1 ]]; then
            echo "An error occurred (UnauthorizedOperation)" >&2
            exit 254
        fi
        printf '%s\n' "${STUB_FLOW_LOGS}" ;;
    "logs delete-log-group") exit "${STUB_DELETE_RC:-0}" ;;
    *) echo "unexpected aws call: $*" >&2; exit 99 ;;
esac
STUB
chmod +x "${WORK}/bin/aws"

GROUP="/aws/vpc/aicr-uat-42-flow-logs"
GROUP_JSON="{\"logGroups\":[{\"logGroupName\":\"${GROUP}\"}]}"
ARN="arn:aws:logs:us-east-1:111111111111:log-group"
NO_FLOW_LOGS='{"FlowLogs":[]}'
OTHER_FLOW_LOGS="{\"FlowLogs\":[{\"FlowLogId\":\"fl-other\",\"LogGroupName\":\"/aws/vpc/aicr-uat-7-flow-logs\",\"LogDestination\":\"${ARN}:/aws/vpc/aicr-uat-7-flow-logs\"},{\"FlowLogId\":\"fl-old\",\"LogGroupName\":\"${GROUP}-old\",\"LogDestination\":\"${ARN}:${GROUP}-old\"}]}"

fail=0
# run_case <name> <want rc: 0|nonzero> <want delete: yes|no> <log groups json> <flow logs json> [env...]
run_case() {
    local name="$1" want_rc="$2" want_delete="$3" groups="$4" flows="$5"
    shift 5
    : > "${WORK}/calls"
    local rc=0
    env PATH="${WORK}/bin:${PATH}" AWS_CALLS="${WORK}/calls" \
        DEPLOYMENT_ID=aicr-uat-42 AWS_REGION=us-east-1 \
        STUB_LOG_GROUPS="${groups}" STUB_FLOW_LOGS="${flows}" "$@" \
        bash "${STEP}" > "${WORK}/out" 2>&1 || rc=$?
    local deleted=no
    grep -q "^logs delete-log-group .*--log-group-name ${GROUP}\$" "${WORK}/calls" && deleted=yes
    local ok=1
    if [[ "${want_rc}" == 0 && "${rc}" -ne 0 ]] || [[ "${want_rc}" != 0 && "${rc}" -eq 0 ]]; then ok=0; fi
    [[ "${deleted}" == "${want_delete}" ]] || ok=0
    if [[ "${ok}" == 1 ]]; then
        echo "ok: ${name}"
    else
        echo "FAIL: ${name}: rc=${rc} (want ${want_rc}), deleted=${deleted} (want ${want_delete})" >&2
        sed 's/^/    /' "${WORK}/out" "${WORK}/calls" >&2
        fail=1
    fi
}

run_case "no log group: nothing to do" 0 no '{"logGroups":[]}' "${NO_FLOW_LOGS}"
run_case "a longer name that shares the prefix is left alone" 0 no \
    "{\"logGroups\":[{\"logGroupName\":\"${GROUP}-old\"}]}" "${NO_FLOW_LOGS}"
run_case "an orphaned group is deleted" 0 yes "${GROUP_JSON}" "${NO_FLOW_LOGS}"
run_case "flow logs of other groups do not keep it" 0 yes "${GROUP_JSON}" "${OTHER_FLOW_LOGS}"
run_case "a group a flow log names is left to Terraform" 0 no "${GROUP_JSON}" \
    "{\"FlowLogs\":[{\"FlowLogId\":\"fl-0123\",\"LogGroupName\":\"${GROUP}\"}]}"
run_case "a group a flow log's destination ARN names is left to Terraform" 0 no "${GROUP_JSON}" \
    "{\"FlowLogs\":[{\"FlowLogId\":\"fl-0123\",\"LogDestination\":\"${ARN}:${GROUP}\"}]}"
run_case "a destination ARN with a :* suffix also keeps it" 0 no "${GROUP_JSON}" \
    "{\"FlowLogs\":[{\"FlowLogId\":\"fl-0123\",\"LogDestination\":\"${ARN}:${GROUP}:*\"}]}"
run_case "a failed log-group lookup stops the run" nonzero no \
    '{"logGroups":[]}' "${NO_FLOW_LOGS}" STUB_DESCRIBE_FAIL=1
run_case "a failed flow-log lookup stops the run" nonzero no \
    "${GROUP_JSON}" "${NO_FLOW_LOGS}" STUB_FLOW_LOGS_FAIL=1
run_case "a flow-log response it cannot read stops the run" nonzero no \
    "${GROUP_JSON}" '{}'
run_case "a failed delete stops the run" nonzero yes "${GROUP_JSON}" "${NO_FLOW_LOGS}" STUB_DELETE_RC=254

if [[ "${fail}" -ne 0 ]]; then
    exit 1
fi
echo "PASS: the flow-logs pre-flight deletes only an orphaned log group, and never on a failed lookup"

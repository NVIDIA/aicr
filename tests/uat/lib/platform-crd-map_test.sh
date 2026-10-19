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

# Guards the platform-to-workload-CRD map used by phase_conformance.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the subject SCRIPT_DIR-relative, never via $HOME/.claude or a
# deployed copy, so this exercises the file in THIS worktree.
# shellcheck source=./platform-crd-map.sh
source "${SCRIPT_DIR}/platform-crd-map.sh"

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

check "dynamo maps to its CRD" \
    "dynamographdeployments.nvidia.com" "$(platform_workload_crd dynamo)"
check "kubeflow maps to its CRD" \
    "trainjobs.trainer.kubeflow.org" "$(platform_workload_crd kubeflow)"
check "slurm maps to the Slinky NodeSet CRD" \
    "nodesets.slinky.slurm.net" "$(platform_workload_crd slurm)"

# An unknown platform must return non-zero AND empty stdout. Asserting only
# the empty string would pass even if the function returned 0, which is the
# bug that let slurm skip silently.
out="$(platform_workload_crd bogus)"; rc=$?
check "unknown platform returns empty stdout" "" "${out}"
check "unknown platform returns rc=1" "1" "${rc}"

# The enumeration and the lookup must stay in step: collect_cluster_debug
# sweeps every known platform through platform_workload_platforms and resolves
# each name back through platform_workload_crd. A name the lookup cannot
# resolve silently drops that platform's CRs from the debug bundle.
check "enumeration lists the known platforms in map order" \
    "dynamo kubeflow slurm" \
    "$(platform_workload_platforms | tr '\n' ' ' | sed 's/ $//')"

resolved=""
for p in $(platform_workload_platforms); do
    crd="$(platform_workload_crd "${p}")" || crd="<unresolved:${p}>"
    resolved="${resolved}${resolved:+ }${crd}"
done
check "every enumerated platform resolves through the lookup" \
    "dynamographdeployments.nvidia.com trainjobs.trainer.kubeflow.org nodesets.slinky.slurm.net" \
    "${resolved}"

exit "${fail}"

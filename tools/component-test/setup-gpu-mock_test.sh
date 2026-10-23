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

# Unit harness for the driver version setup-gpu-mock.sh asks nvml-mock for.
# Run directly: bash tools/component-test/setup-gpu-mock_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: stubs `kubectl` and `helm` on PATH and drives the Helm path, so no
# cluster or registry is touched. The chart reports its profile's driver unless
# driverVersion is set, so a DRIVER_VERSION that never reaches helm, or a
# logged default that differs from the profile, runs a driver-gated check
# against a driver nobody logged.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="${SCRIPT_DIR}/setup-gpu-mock.sh"

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT
HLOG="${STUB_DIR}/helm.log"
export HLOG

# kubectl: no DaemonSet yet (so the script installs), one mock pod, and a
# staged driver tree for the verifier. helm: one argument per line, so an
# assertion matches a whole argument rather than a substring of the command.
cat >"${STUB_DIR}/kubectl" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "get" && "$2" == "daemonset" ]]; then exit 1; fi
if [[ "$1" == "get" && "$2" == "pods" ]]; then echo "nvml-mock-stub"; exit 0; fi
exit 0
STUB

cat >"${STUB_DIR}/helm" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" >>"${HLOG}"
exit 0
STUB

chmod +x "${STUB_DIR}/kubectl" "${STUB_DIR}/helm"

OUT=""
RC=0
HELM_ARGS=""
# run <VAR=value...>: run the script with only the given overrides, capturing
# its output in $OUT, its exit code in $RC and helm's argv in $HELM_ARGS.
run() {
    : >"${HLOG}"
    OUT="$(env -u DRIVER_VERSION -u GPU_PROFILE -u GPU_COUNT \
        PATH="${STUB_DIR}:${PATH}" "$@" bash "${SUBJECT}" 2>&1)"
    RC=$?
    HELM_ARGS="$(cat "${HLOG}")"
}

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1: $2"; fails=$((fails + 1)); }

check_rc() { # <name> <want_rc>
    if [[ "${RC}" == "$2" ]]; then pass "$1"; else fail "$1" "want rc=$2 got rc=${RC}"; fi
}
check_out() { # <name> <needle>
    if [[ "${OUT}" == *"$2"* ]]; then pass "$1"; else fail "$1" "output lacks: $2"; fi
}
check_helm_arg() { # <name> <argument>
    if grep -qxF -- "$2" <<<"${HELM_ARGS}"; then pass "$1"; else fail "$1" "helm was not passed: $2"; fi
}
check_no_helm_arg() { # <name> <prefix>
    if grep -qF -- "$2" <<<"${HELM_ARGS}"; then
        fail "$1" "helm was passed: $(grep -F -- "$2" <<<"${HELM_ARGS}")"
    else
        pass "$1"
    fi
}

# 1. A requested driver reaches the chart, which otherwise reports the
#    profile's (h100 is 550.163.01 in the pinned chart).
run GPU_PROFILE=h100 DRIVER_VERSION=570.158.01
check_rc "override-install-succeeds" 0
check_helm_arg "override-forwarded" "driverVersion=570.158.01"
check_out "override-logged" "driver=570.158.01"

# 2. Unset means the chart's profile default: nothing forwarded, and the log
#    names the driver the pinned chart's profile actually reports (case 3).
run GPU_PROFILE=h100
check_rc "default-install-succeeds" 0
check_no_helm_arg "default-not-forwarded" "driverVersion="

# 3. The logged default follows each profile of the pinned chart
#    (profiles/<name>.yaml system.driver_version at 0.4.0-rc2).
for pair in a100=550.163.01 b200=560.35.03 gb200=580.65.06 gb300=570.124.06 \
    h100=550.163.01 l40s=550.163.01 t4=550.163.01; do
    profile="${pair%%=*}"
    driver="${pair#*=}"
    run GPU_PROFILE="${profile}"
    check_out "default-${profile}-logged" "driver=${driver}"
done

if [[ "${fails}" -gt 0 ]]; then
    echo "${fails} check(s) failed"
    exit 1
fi
echo "all checks passed"

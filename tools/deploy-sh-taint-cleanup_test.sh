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

# Unit harness for the stale nodewright-taint cleanup in the generated
# deploy.sh (pkg/bundler/deployer/helm/templates/deploy.sh.tmpl).
# Run directly: bash tools/deploy-sh-taint-cleanup_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: extracts remove_stale_nodewright_taints from the committed golden
# render and drives it with a stubbed `kubectl` on PATH, so no cluster is
# required. The assertions guard the destructive path — which `kubectl taint
# <node> <key>-` calls are issued — against the failure modes raised on
# NVIDIA/aicr#2597: a read error must not strip a live gate, and a configured
# key must match exactly (no word-splitting, no prefix match).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GOLDEN="${SCRIPT_DIR}/../pkg/bundler/deployer/helm/testdata/nodewright_present/deploy.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# --- Extract the function under test -----------------------------------------
sed -n '/^function remove_stale_nodewright_taints()/,/^}/p' "${GOLDEN}" >"${WORK}/fn.sh"
if [[ ! -s "${WORK}/fn.sh" ]]; then
    echo "FAIL: remove_stale_nodewright_taints not found in ${GOLDEN}"
    exit 1
fi

# --- Stub kubectl on PATH -----------------------------------------------------
# Behaviour is driven by env vars so each case sets its own cluster shape:
#   STUB_DEPLOY_RC / STUB_DEPLOY_OUT  -> `kubectl get deploy` exit code / stdout
#     (stdout is "<name> <availableReplicas>" per Deployment; the count is
#     empty when availableReplicas is omitted, i.e. zero available)
#   STUB_NODES_RC  / STUB_NODES_OUT   -> `kubectl get nodes`  exit code / stdout
# Every `kubectl taint` call is appended to $KLOG.
cat >"${WORK}/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
    "get deploy") printf '%s' "${STUB_DEPLOY_OUT:-}"; exit "${STUB_DEPLOY_RC:-0}" ;;
    "get nodes")  printf '%s' "${STUB_NODES_OUT:-}";  exit "${STUB_NODES_RC:-0}" ;;
    "taint node") printf '%s\n' "$*" >>"${KLOG}"; exit 0 ;;
esac
exit 0
STUB
chmod +x "${WORK}/kubectl"
export PATH="${WORK}:${PATH}"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

# run <values-file>: source the function with a _warn_line shim, call it, and
# capture output in $OUT and the taint-call log in $TAINTS.
OUT=""
TAINTS=""
run() {
    export KLOG="${WORK}/klog"
    : >"${KLOG}"
    OUT="$(bash -c '
        set -euo pipefail
        _warn_line() { printf "WARN %s\n" "$*"; }
        source "$1"
        remove_stale_nodewright_taints skyhook "$2"
    ' _ "${WORK}/fn.sh" "$1" 2>&1)"
    RC=$?
    TAINTS="$(cat "${KLOG}")"
}

check_rc0()        { if [[ "${RC}" == "0" ]]; then pass "$1"; else fail "$1" "want rc=0 got rc=${RC}: ${OUT}"; fi; }
check_out()        { if [[ "${OUT}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected output to contain: $2 (got: ${OUT})"; fi; }
check_no_taints()  { if [[ -z "${TAINTS}" ]]; then pass "$1"; else fail "$1" "expected no kubectl taint calls, got: ${TAINTS}"; fi; }
check_taint()      { if [[ "${TAINTS}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected taint call: $2 (got: ${TAINTS})"; fi; }
check_no_taint()   { if [[ "${TAINTS}" != *"$2"* ]]; then pass "$1"; else fail "$1" "unexpected taint call: $2"; fi; }
check_taint_count() { local n; n=$(grep -c . <<<"${TAINTS}" || true); if [[ "${n}" == "$2" ]]; then pass "$1"; else fail "$1" "want $2 taint calls got ${n}: ${TAINTS}"; fi; }

NODES_LEGACY_TAINTED=$'gpu-0 skyhook.nvidia.com node.kubernetes.io/unschedulable\ngpu-1 nodewright.nvidia.com\ncpu-0 dedicated\n'
NO_VALUES="${WORK}/no-such-values.yaml"

# 1. A failed `kubectl get deploy` (transient API / RBAC error) must skip the
#    cleanup entirely — never read as "operator not running".
STUB_DEPLOY_RC=1 STUB_DEPLOY_OUT='Unable to connect to the server: EOF' \
STUB_NODES_OUT="${NODES_LEGACY_TAINTED}" run "${NO_VALUES}"
check_rc0       "deploy-read-error-rc0"
check_out       "deploy-read-error-warns" "skipping stale-taint cleanup"
check_no_taints "deploy-read-error-no-taint-calls"

# 2. A running operator (available replicas > 0) owns the taints: no cleanup.
STUB_DEPLOY_OUT=$'skyhook-operator-controller-manager 1\n' STUB_NODES_OUT="${NODES_LEGACY_TAINTED}" run "${NO_VALUES}"
check_rc0       "operator-running-rc0"
check_no_taints "operator-running-no-taint-calls"

# 2a. Several matching Deployments where only one is available: an active
#     operator owns the gate regardless of row order or of a zero / omitted
#     count on the other rows.
STUB_DEPLOY_OUT=$'nodewright-controller-manager 0\nskyhook-operator-controller-manager 1\n' \
STUB_NODES_OUT="${NODES_LEGACY_TAINTED}" run "${NO_VALUES}"
check_rc0       "mixed-zero-then-active-rc0"
check_no_taints "mixed-zero-then-active-no-taint-calls"
STUB_DEPLOY_OUT=$'skyhook-operator-controller-manager 1\nnodewright-controller-manager \n' \
STUB_NODES_OUT="${NODES_LEGACY_TAINTED}" run "${NO_VALUES}"
check_rc0       "mixed-active-then-omitted-rc0"
check_no_taints "mixed-active-then-omitted-no-taint-calls"

# 2b. Several matching Deployments, none available: stale taints are cleaned
#     with the existing-operator key set (configured + legacy only).
STUB_DEPLOY_OUT=$'nodewright-controller-manager 0\nskyhook-operator-controller-manager \n' \
STUB_NODES_OUT="${NODES_LEGACY_TAINTED}" run "${NO_VALUES}"
check_taint       "mixed-none-active-legacy-removed" "taint node gpu-0 skyhook.nvidia.com-"
check_taint       "mixed-none-active-default-removed" "taint node gpu-1 nodewright.nvidia.com-"
check_taint_count "mixed-none-active-two-calls" 2

# 3. A failed `kubectl get nodes` must also skip, not treat "no output" as clean.
STUB_DEPLOY_OUT='' STUB_NODES_RC=1 STUB_NODES_OUT='forbidden' run "${NO_VALUES}"
check_out       "nodes-read-error-warns" "skipping stale-taint cleanup"
check_no_taints "nodes-read-error-no-taint-calls"

# 4. Fresh deploy (no Deployment -> empty output), no configured taint: both
#    operator default keys are cleaned, each exactly once, unrelated taints kept.
STUB_DEPLOY_OUT='' STUB_NODES_OUT="${NODES_LEGACY_TAINTED}" run "${NO_VALUES}"
check_rc0         "defaults-rc0"
check_taint       "defaults-legacy-key-removed" "taint node gpu-0 skyhook.nvidia.com-"
check_taint       "defaults-new-key-removed"    "taint node gpu-1 nodewright.nvidia.com-"
check_no_taint    "defaults-unrelated-kept"     "cpu-0"
check_taint_count "defaults-exactly-two-calls"  2

# 5. Scaled-to-zero operator (0 available) with a configured custom key: the
#    custom key and the legacy key are cleaned; the now-unconfigured
#    nodewright.nvidia.com default is not; a prefix-similar key is not matched.
cat >"${WORK}/values.yaml" <<'EOF'
controllerManager:
  manager:
    env:
      runtimeRequiredTaint: "custom.io/gate=true:NoSchedule"
EOF
STUB_DEPLOY_OUT=$'skyhook-operator-controller-manager 0\n' \
STUB_NODES_OUT=$'gpu-0 custom.io/gate\ngpu-1 custom.io/gate2\ngpu-2 skyhook.nvidia.com\ngpu-3 nodewright.nvidia.com\n' \
run "${WORK}/values.yaml"
check_taint       "custom-key-removed"           "taint node gpu-0 custom.io/gate-"
check_no_taint    "custom-prefix-not-matched"    "gpu-1"
check_taint       "custom-legacy-still-removed"  "taint node gpu-2 skyhook.nvidia.com-"
check_no_taint    "custom-default-not-removed"   "gpu-3"
check_taint_count "custom-exactly-two-calls"     2

# 5a. Same, but the existing Deployment omits availableReplicas entirely (the
#     field is omitempty, so zero available prints nothing). This is an existing
#     operator, not a fresh deploy: the unconfigured default key must be kept.
STUB_DEPLOY_OUT=$'skyhook-operator-controller-manager \n' \
STUB_NODES_OUT=$'gpu-0 custom.io/gate\ngpu-2 skyhook.nvidia.com\ngpu-3 nodewright.nvidia.com\n' \
run "${WORK}/values.yaml"
check_taint       "omitted-count-custom-removed"       "taint node gpu-0 custom.io/gate-"
check_taint       "omitted-count-legacy-removed"       "taint node gpu-2 skyhook.nvidia.com-"
check_no_taint    "omitted-count-default-not-removed"  "gpu-3"
check_taint_count "omitted-count-exactly-two-calls"    2

# 5b. Fresh deploy (no Deployment) with a configured custom key: the previous
#     install may have tainted with either default key, so both defaults are
#     cleaned alongside the custom key — the incoming operator would never
#     remove a stale nodewright.nvidia.com taint itself.
STUB_DEPLOY_OUT='' \
STUB_NODES_OUT=$'gpu-0 custom.io/gate\ngpu-1 custom.io/gate2\ngpu-2 skyhook.nvidia.com\ngpu-3 nodewright.nvidia.com\n' \
run "${WORK}/values.yaml"
check_taint       "fresh-custom-key-removed"      "taint node gpu-0 custom.io/gate-"
check_no_taint    "fresh-custom-prefix-not-matched" "gpu-1"
check_taint       "fresh-custom-legacy-removed"   "taint node gpu-2 skyhook.nvidia.com-"
check_taint       "fresh-custom-default-removed"  "taint node gpu-3 nodewright.nvidia.com-"
check_taint_count "fresh-custom-exactly-three-calls" 3

# 6. A configured key that contains whitespace (rejected by ParseTaint, but
#    guarded here too) must stay one token: nodes carrying `foo` or `bar`
#    taints are not selected.
cat >"${WORK}/values-bad.yaml" <<'EOF'
controllerManager:
  manager:
    env:
      runtimeRequiredTaint: "foo bar=true:NoSchedule"
EOF
STUB_DEPLOY_OUT='' STUB_NODES_OUT=$'gpu-0 foo\ngpu-1 bar\ngpu-2 skyhook.nvidia.com\n' run "${WORK}/values-bad.yaml"
check_no_taint    "malformed-key-no-foo"   "gpu-0"
check_no_taint    "malformed-key-no-bar"   "gpu-1"
check_taint       "malformed-key-legacy"   "taint node gpu-2 skyhook.nvidia.com-"
check_taint_count "malformed-key-one-call" 1

echo
if [[ "${fails}" -ne 0 ]]; then
    echo "${fails} check(s) failed"
    exit 1
fi
echo "all checks passed"

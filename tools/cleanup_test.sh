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

# Unit harness for tools/cleanup exclusion + backstop behavior.
# Run directly: bash tools/cleanup_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: stubs `kubectl` and `helm` on PATH and drives the script under
# --dry-run, so no cluster is required and no resource is ever touched. The
# assertions guard the safety-critical logic (which installs get fenced out)
# that protects out-of-band installs from data loss. See #1672.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLEANUP="${SCRIPT_DIR}/cleanup"

# --- Stub kubectl/helm/sleep on PATH ------------------------------------------
# All three log their args to $KLOG/$HLOG when set, so the live (--yes) path can
# assert which resources actually received destructive calls.
# kubectl: context name for detection; `get crd` returns an excluded and a
# non-excluded CRD; `get ns` reports existence + a Terminating phase so the
# phase-4 finalizer rescue engages; `api-resources`/`get configmaps` feed that
# rescue one patchable object. helm: `ls` (phase 1) returns two releases — one
# out-of-band, one AICR-owned; `list` (driver probe) returns nothing so the
# cluster is treated as non-driver-managed. sleep: instant (skips phase-4 wait).
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

cat >"${STUB_DIR}/kubectl" <<'STUB'
#!/usr/bin/env bash
[[ -n "${KLOG:-}" ]] && printf '%s\n' "$*" >>"${KLOG}"
if [[ "$1" == "config" && "$2" == "current-context" ]]; then echo "stub-ctx"; exit 0; fi
if [[ "$1" == "api-resources" ]]; then echo "configmaps"; exit 0; fi
if [[ "$1" == "get" ]]; then
    case "$2" in
        crd)
            printf '%s\n' \
                "customresourcedefinition.apiextensions.k8s.io/clusterpolicies.nvidia.com" \
                "customresourcedefinition.apiextensions.k8s.io/nodes.skyhook.nvidia.com"
            ;;
        ns|namespace)
            for a in "$@"; do [[ "$a" == *status.phase* ]] && { echo "Terminating"; exit 0; }; done
            ;;
        configmaps) echo "configmap/stub-cm" ;;
    esac
    exit 0
fi
exit 0
STUB

cat >"${STUB_DIR}/helm" <<'STUB'
#!/usr/bin/env bash
[[ -n "${HLOG:-}" ]] && printf '%s\n' "$*" >>"${HLOG}"
if [[ "$1" == "ls" ]]; then
    printf 'NAME\tNAMESPACE\tREVISION\n'
    printf 'nodewright\tskyhook\t1\n'
    printf 'gpu-operator\tgpu-operator\t1\n'
fi
exit 0
STUB

cat >"${STUB_DIR}/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB

chmod +x "${STUB_DIR}/kubectl" "${STUB_DIR}/helm" "${STUB_DIR}/sleep"
export PATH="${STUB_DIR}:${PATH}"

# run <args...>: capture combined stdout+stderr into $OUT and exit code into $RC.
OUT=""
RC=0
run() {
    OUT="$("${CLEANUP}" "$@" 2>&1)"
    RC=$?
}

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1 — $2"; fails=$((fails + 1)); }

check_contains() { # <name> <needle>
    if [[ "${OUT}" == *"$2"* ]]; then pass "$1"; else fail "$1" "expected to contain: $2"; fi
}
check_absent() { # <name> <needle>
    if [[ "${OUT}" != *"$2"* ]]; then pass "$1"; else fail "$1" "expected NOT to contain: $2"; fi
}
check_rc() { # <name> <want_rc>
    if [[ "${RC}" == "$2" ]]; then pass "$1"; else fail "$1" "want rc=$2 got rc=${RC}"; fi
}
check_rc_nonzero() { # <name>
    if [[ "${RC}" != "0" ]]; then pass "$1"; else fail "$1" "want nonzero rc, got 0"; fi
}

# 1. Repeatable + comma-separated --exclude-ns parse into the exclusion set.
run --dry-run --exclude-ns a,b --exclude-ns c --exclude-crd x
check_contains "csv-and-repeat-ns-parsed" "Excluding namespaces (Helm + namespace deletion): a b c"

# 2. Phase 1: an excluded namespace's Helm release is skipped; a non-excluded
#    AICR release is still uninstalled.
run --dry-run --exclude-ns skyhook --exclude-crd skyhook.nvidia.com
check_contains "phase1-excluded-release-skipped" "skip (excluded ns): helm release nodewright -n skyhook"
check_contains "phase1-nonexcluded-release-uninstalled" "helm uninstall gpu-operator -n gpu-operator"

# 3. Phase 4: excluded namespace is preserved; the check-created backstop
#    namespace is still deleted.
check_contains "phase4-excluded-ns-skipped" "skip (excluded ns): skyhook"
check_absent   "phase4-excluded-ns-not-deleted" "kubectl delete ns skyhook "
check_contains "phase4-check-backstop-deleted" "kubectl delete ns gang-scheduling-test"

# 4. Phase 3: excluded CRD match is echoed under dry-run.
check_contains "phase3-excluded-crd-echoed" "excluding CRDs matching: skyhook.nvidia.com"

# 5. Asymmetric invocation warns (ns without crd, and crd without ns).
run --dry-run --exclude-ns skyhook
check_contains "warn-ns-without-crd" "given without --exclude-crd"
run --dry-run --exclude-crd skyhook.nvidia.com
check_contains "warn-crd-without-ns" "given without --exclude-ns"

# 6. Symmetric invocation does NOT warn.
run --dry-run --exclude-ns skyhook --exclude-crd skyhook.nvidia.com
check_absent "no-warn-when-paired" "given without"

# 7. Missing / flag-shaped argument fails closed.
run --dry-run --exclude-ns
check_rc_nonzero "missing-exclude-ns-arg-fails"
run --dry-run --exclude-ns --yes
check_rc_nonzero "flag-shaped-exclude-ns-arg-fails"

# 8. No-exclusion path runs cleanly under set -u (empty arrays).
run --dry-run
check_rc "no-flags-dry-run-succeeds" 0
check_contains "no-flags-backstop-present" "kubectl delete ns gang-scheduling-test"

# 9. Whitespace-padded CSV tokens are trimmed (both are protected).
run --dry-run --exclude-ns 'skyhook, gpu-operator'
check_contains "csv-trims-whitespace" "Excluding namespaces (Helm + namespace deletion): skyhook gpu-operator"

# 10. Live (--yes) path: exercise the real destructive branches with logging
#     stubs and assert excluded resources NEVER receive delete/patch calls
#     (dry-run cannot reach is_excluded_crd or the finalizer-rescue skip).
has()    { if [[ "$2" == *"$3"* ]]; then pass "$1"; else fail "$1" "expected to contain: $3"; fi; }
has_not() { if [[ "$2" != *"$3"* ]]; then pass "$1"; else fail "$1" "expected NOT to contain: $3"; fi; }

KLOG="$(mktemp)"; HLOG="$(mktemp)"
KLOG="${KLOG}" HLOG="${HLOG}" "${CLEANUP}" --yes \
    --exclude-ns skyhook --exclude-crd skyhook.nvidia.com >/dev/null 2>&1
klog="$(cat "${KLOG}")"; hlog="$(cat "${HLOG}")"
rm -f "${KLOG}" "${HLOG}"

# CRD phase: non-excluded CRD deleted; the excluded group is never touched even
# though the broad nvidia.com pattern matches it.
has     "live-crd-nonexcluded-deleted" "${klog}" "delete customresourcedefinition.apiextensions.k8s.io/clusterpolicies.nvidia.com"
has_not "live-nothing-skyhook-in-kubectl" "${klog}" "skyhook"
# Namespace phase: backstop deleted; excluded namespace neither deleted nor
# finalizer-patched (the has_not above already covers skyhook end-to-end).
has     "live-backstop-ns-deleted" "${klog}" "delete ns gang-scheduling-test"
# Finalizer rescue actually engaged on a non-excluded Terminating namespace.
has     "live-finalizer-rescue-ran" "${klog}" "patch configmap/stub-cm -n gang-scheduling-test"
# Helm phase: AICR-owned release uninstalled; out-of-band release left alone.
has     "live-helm-nonexcluded-uninstalled" "${hlog}" "uninstall gpu-operator"
has_not "live-helm-excluded-untouched" "${hlog}" "nodewright"

if (( fails > 0 )); then
    echo "${fails} test(s) failed"
    exit 1
fi
echo "All cleanup exclusion tests passed"

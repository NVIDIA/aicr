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

# Tests for the envtest check in the Makefile's test-go target. The envtest
# suites skip when KUBEBUILDER_ASSETS is empty, so a `make test` that ran them
# without the binaries would pass while testing none of them: without
# setup-envtest, or when it returns no path, test-go must stop with the
# install hint before `go test` runs. `go` and `setup-envtest` are stubs, so
# no test runs and nothing is downloaded.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v yq >/dev/null 2>&1; then
  if [[ -n "${CI:-}" ]]; then
    echo "FAIL: yq is not installed; the test-go envtest check cannot be verified in CI" >&2
    exit 1
  fi
  echo "SKIP: yq is not installed; run 'make tools-setup'"
  exit 0
fi
version="$(yq -r '.testing_tools.setup_envtest' "${REPO_ROOT}/.settings.yaml")"
if [[ -z "${version}" || "${version}" == "null" ]]; then
  echo "FAIL: .settings.yaml pins no testing_tools.setup_envtest" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

cat >"${WORK}/go" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  list) echo "aicr.run/pkg/stub" ;;
  test) printf '%s' "${KUBEBUILDER_ASSETS-unset}" >"${GO_TEST_MARKER}"; exit 1 ;;
esac
STUB
chmod +x "${WORK}/go"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# run_test_go <setup-envtest stub body>: runs `make test-go` with the stub,
# leaving its stderr in ${WORK}/stderr and its exit status in ${rc}.
run_test_go() {
  printf '#!/usr/bin/env bash\n%s\n' "$1" >"${WORK}/setup-envtest"
  chmod +x "${WORK}/setup-envtest"
  rm -f "${WORK}/ran"
  rc=0
  env -u MAKEFLAGS -u MAKELEVEL -u MFLAGS \
    PATH="${WORK}:${PATH}" GO_TEST_MARKER="${WORK}/ran" \
    make --no-print-directory -C "${REPO_ROOT}" test-go >/dev/null 2>"${WORK}/stderr" || rc=$?
}

hint="go install sigs.k8s.io/controller-runtime/tools/setup-envtest@${version}"
for case in "absent:echo 'setup-envtest: command not found' >&2; exit 127" \
            "no path:exit 0"; do
  name="${case%%:*}"
  run_test_go "${case#*:}"
  if [[ "${rc}" -eq 0 ]]; then
    fail "${name}: make test-go exited 0 without envtest binaries"
  fi
  if [[ -e "${WORK}/ran" ]]; then
    fail "${name}: make test-go ran go test without envtest binaries"
  fi
  for want in "${hint}" "setup-envtest use -p path"; do
    if ! grep -qF -- "${want}" "${WORK}/stderr"; then
      fail "${name}: the hint does not say '${want}'; stderr: $(cat "${WORK}/stderr")"
    fi
  done
done

run_test_go "echo /stub/envtest/assets"
if [[ "$(cat "${WORK}/ran" 2>/dev/null)" != "/stub/envtest/assets" ]]; then
  fail "with a path from setup-envtest, go test ran with KUBEBUILDER_ASSETS='$(cat "${WORK}/ran" 2>/dev/null || echo 'go test never ran')'"
fi

if [[ "${failures}" -gt 0 ]]; then
  exit 1
fi
echo "PASS: make test-go stops without envtest binaries and passes their path to go test"

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

# Unit tests for tools/check-settings-checksums.
#
# The checker itself needs the network -- it re-derives each digest from the
# upstream release. These tests do not: they drive it through AICR_REPO_ROOT
# against a fixture tree whose refreshers are stubs, so `make test-shell` stays
# hermetic. What is under test is the checker's decision logic (which pins it
# reads, what it treats as stale, what it refuses to pass), not any digest.
#
# The coverage case is the load-bearing one. A checksum-pinned tool that is
# absent from the checker's SPECS list is silently ungated -- exactly the state
# helm_diff was effectively in before this gate existed -- and that failure is
# invisible in every other test, because the checker still exits 0.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="${SCRIPT_DIR}/check-settings-checksums"

if [[ ! -x "${CHECKER}" ]]; then
  echo "FAIL: ${CHECKER} is missing or not executable" >&2
  exit 1
fi

TOOLS=(
  update-addlicense-checksums
  update-chainsaw-checksums
  update-helm-diff-checksums
  update-helmfile-checksums
  update-oasdiff-checksums
  update-oras-checksums
  update-setup-envtest-checksums
)
KEYS=(addlicense chainsaw helm_diff helmfile oasdiff oras setup_envtest)

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}
ok() { echo "  ok   $*"; }

# ---------------------------------------------------------------------------
# Every real refresh script is gated
# ---------------------------------------------------------------------------
#
# Compares the checker's SPECS list against what is actually on disk, so adding
# tools/update-<new>-checksums without adding it to SPECS fails here rather than
# leaving that pin unverified forever.

declared=$(grep -oE '"update-[a-z-]+-checksums\|' "${CHECKER}" | sed 's/^"//; s/|$//' | sort)
on_disk=$(find "${SCRIPT_DIR}" -maxdepth 1 -name 'update-*-checksums' -exec basename {} \; | sort)

if [[ "${declared}" != "${on_disk}" ]]; then
  fail "checker SPECS does not match tools/update-*-checksums on disk"
  diff <(printf '%s\n' "${declared}") <(printf '%s\n' "${on_disk}") \
    | sed 's/^/    /' >&2 || true
else
  ok "SPECS covers every tools/update-*-checksums script ($(printf '%s\n' "${on_disk}" | wc -l | tr -d ' ') of them)"
fi

# ---------------------------------------------------------------------------
# Fixture helpers
# ---------------------------------------------------------------------------

# Builds a fixture repo whose refreshers are all no-ops (the idempotent case
# every real refresh script documents), then applies optional overrides.
make_fixture() {
  local root
  root=$(mktemp -d)
  mkdir -p "${root}/tools"

  {
    echo "testing_tools:"
    local i
    for i in "${!KEYS[@]}"; do
      echo "  ${KEYS[$i]}: 'v1.0.0'"
      echo "  ${KEYS[$i]}_checksums:"
      echo "    linux_amd64: '$(printf '%064d' "$i")'"
    done
  } > "${root}/.settings.yaml"

  local t
  for t in "${TOOLS[@]}"; do
    printf '#!/usr/bin/env bash\nexit 0\n' > "${root}/tools/${t}"
    chmod +x "${root}/tools/${t}"
  done

  printf '%s' "${root}"
}

run_checker() {
  local root="$1"
  AICR_REPO_ROOT="${root}" "${CHECKER}" >"${root}/out.log" 2>&1
}

# ---------------------------------------------------------------------------
# Clean tree passes
# ---------------------------------------------------------------------------

root=$(make_fixture)
if run_checker "${root}"; then
  if grep -q "All ${#TOOLS[@]} checksum pin(s) match" "${root}/out.log"; then
    ok "idempotent refreshers report every pin as current"
  else
    fail "clean tree passed but did not report all ${#TOOLS[@]} pins"
    sed 's/^/    /' "${root}/out.log" >&2
  fi
else
  fail "clean tree should pass"
  sed 's/^/    /' "${root}/out.log" >&2
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------
# A digest that no longer matches its pinned version is caught
# ---------------------------------------------------------------------------
#
# This is #2801: the version pin moved, the refresher would now produce a
# different digest, and the committed one was left behind.

root=$(make_fixture)
cat > "${root}/tools/update-helm-diff-checksums" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
sed -E -i'' -e "s/^(    linux_amd64: )'[0-9a-f]{64}'/\1'$(printf 'a%.0s' {1..64})'/" .settings.yaml
STUB
chmod +x "${root}/tools/update-helm-diff-checksums"

if run_checker "${root}"; then
  fail "a digest that does not match its pinned version must not pass"
  sed 's/^/    /' "${root}/out.log" >&2
else
  if grep -q "do not match their pinned version" "${root}/out.log" \
     && grep -q "helm_diff" "${root}/out.log"; then
    ok "stale digest fails the gate and names the offending pin"
  else
    fail "stale digest failed for the wrong reason"
    sed 's/^/    /' "${root}/out.log" >&2
  fi
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------
# A refresher that cannot run fails closed
# ---------------------------------------------------------------------------
#
# The mode that produced #2801: the hook ran and failed. Passing here would
# report "verified" for a pin nothing checked.

root=$(make_fixture)
printf '#!/usr/bin/env bash\necho "boom" >&2\nexit 1\n' > "${root}/tools/update-oras-checksums"
chmod +x "${root}/tools/update-oras-checksums"

if run_checker "${root}"; then
  fail "a refresher that exits non-zero must not be reported as verified"
  sed 's/^/    /' "${root}/out.log" >&2
else
  if grep -q "could not be verified" "${root}/out.log" && grep -q "oras" "${root}/out.log"; then
    ok "failed refresher fails closed instead of passing unverified"
  else
    fail "failed refresher failed for the wrong reason"
    sed 's/^/    /' "${root}/out.log" >&2
  fi
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------
# A missing version pin fails closed
# ---------------------------------------------------------------------------

root=$(make_fixture)
sed -E -i'' -e "/^  oasdiff: /d" "${root}/.settings.yaml"

if run_checker "${root}"; then
  fail "a missing version pin must not pass"
  sed 's/^/    /' "${root}/out.log" >&2
else
  if grep -q "could not be verified" "${root}/out.log" && grep -q "oasdiff" "${root}/out.log"; then
    ok "missing version pin fails closed"
  else
    fail "missing version pin failed for the wrong reason"
    sed 's/^/    /' "${root}/out.log" >&2
  fi
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------
# The checked-out tree is never modified
# ---------------------------------------------------------------------------
#
# Refreshers rewrite .settings.yaml in their working directory, so the checker
# has to sandbox them to a copy. If that slips, a read-only check silently edits
# the tree it is checking.

root=$(make_fixture)
cat > "${root}/tools/update-chainsaw-checksums" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "rewritten" >> .settings.yaml
STUB
chmod +x "${root}/tools/update-chainsaw-checksums"
before=$(shasum -a 256 < "${root}/.settings.yaml")
run_checker "${root}" || true
after=$(shasum -a 256 < "${root}/.settings.yaml")

if [[ "${before}" == "${after}" ]]; then
  ok "a rewriting refresher cannot modify the tree being checked"
else
  fail ".settings.yaml was modified by the check"
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
  echo "FAIL: ${failures} check-settings-checksums test(s) failed" >&2
  exit 1
fi

echo "PASS: check-settings-checksums decision logic"

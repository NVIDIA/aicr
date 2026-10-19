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
#
# It is a real git repo with one commit, because the checker reads the previous
# version pins from HEAD^ to attribute drift. Tests that need a baseline commit
# the current .settings.yaml first (see commit_fixture_baseline).
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

  git -C "${root}" init -q 2>/dev/null
  git -C "${root}" config user.email test@example.com
  git -C "${root}" config user.name test
  git -C "${root}" config commit.gpgsign false

  printf '%s' "${root}"
}

# Gives the fixture a HEAD^ to compare against: an initial commit, then an empty
# one so HEAD^ resolves to a tree carrying the current pins.
commit_fixture_baseline() {
  local root="$1"
  git -C "${root}" add -A
  git -C "${root}" commit -qm baseline
  git -C "${root}" commit -q --allow-empty -m head
}

run_checker() {
  local root="$1"
  AICR_REPO_ROOT="${root}" "${CHECKER}" >"${root}/out.log" 2>&1
}

# ---------------------------------------------------------------------------
# Clean tree passes
# ---------------------------------------------------------------------------

root=$(make_fixture)
commit_fixture_baseline "${root}"
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
# A BUMPED version whose digests were left behind is caught, and is offered the
# mechanical fix
# ---------------------------------------------------------------------------
#
# This is #2801: the version pin moved in this diff, the refresher would now
# produce a different digest, and the committed one was left behind. Refreshing
# is just finishing the bump, so the remedy names the exact command.

drift_stub() {
  cat <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
sed -E -i'' -e "s/^(    linux_amd64: )'[0-9a-f]{64}'/\1'$(printf 'a%.0s' {1..64})'/" .settings.yaml
STUB
}

root=$(make_fixture)
commit_fixture_baseline "${root}"
drift_stub > "${root}/tools/update-helm-diff-checksums"
chmod +x "${root}/tools/update-helm-diff-checksums"
# The bump this PR is making: helm_diff moves, its digests do not.
sed -E -i'' -e "s/^(  helm_diff: )'v1\.0\.0'/\1'v2.0.0'/" "${root}/.settings.yaml"

if run_checker "${root}"; then
  fail "a bumped version with unrefreshed digests must not pass"
  sed 's/^/    /' "${root}/out.log" >&2
else
  if grep -q "do not match their bumped version" "${root}/out.log" \
     && grep -q "tools/update-helm-diff-checksums v2.0.0" "${root}/out.log"; then
    ok "bumped-but-unrefreshed pin fails and names the exact refresh command"
  else
    fail "bumped-but-unrefreshed pin failed for the wrong reason"
    sed 's/^/    /' "${root}/out.log" >&2
  fi
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------
# Drift under an UNCHANGED version is reported as upstream tampering, not as a
# stale pin, and is NOT offered the refresh command
# ---------------------------------------------------------------------------
#
# .settings.yaml's own rationale for committing digests is that "an attacker who
# replaces both the archive and its checksum still fails against a value
# committed and reviewed at this SHA". Telling someone to re-run the refresher
# here would overwrite that reviewed value with whatever upstream now serves,
# which is exactly the laundering path the pin exists to block. The two causes
# must not share a remedy.

root=$(make_fixture)
commit_fixture_baseline "${root}"
drift_stub > "${root}/tools/update-helm-diff-checksums"
chmod +x "${root}/tools/update-helm-diff-checksums"
# No version bump this time — upstream simply serves different bytes.

if run_checker "${root}"; then
  fail "upstream drift under an unchanged pin must not pass"
  sed 's/^/    /' "${root}/out.log" >&2
else
  if grep -q "drifted WITHOUT a version bump" "${root}/out.log" \
     && grep -q "Do NOT run the refresh script" "${root}/out.log" \
     && ! grep -q "fix with" "${root}/out.log"; then
    ok "unchanged-pin drift is reported as tampering, with no refresh remedy"
  else
    fail "unchanged-pin drift was not distinguished from a stale bump"
    sed 's/^/    /' "${root}/out.log" >&2
  fi
fi
rm -rf "${root}"

# ---------------------------------------------------------------------------
# An unresolvable baseline is treated as the cautious case
# ---------------------------------------------------------------------------
#
# Without a baseline the cause of drift cannot be attributed. Guessing "stale
# bump" would hand out the refresh command in exactly the situation where it
# might launder a tampered release, so the unknown case fails closed as drift.

root=$(make_fixture)   # deliberately NOT committed: HEAD^ does not resolve
drift_stub > "${root}/tools/update-helm-diff-checksums"
chmod +x "${root}/tools/update-helm-diff-checksums"

if run_checker "${root}"; then
  fail "drift with no resolvable baseline must not pass"
  sed 's/^/    /' "${root}/out.log" >&2
else
  if grep -q "drifted WITHOUT a version bump" "${root}/out.log" \
     && grep -q "could not be read" "${root}/out.log"; then
    ok "unresolvable baseline falls back to the cautious verdict and says so"
  else
    fail "unresolvable baseline did not fall back to the cautious verdict"
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
commit_fixture_baseline "${root}"
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
commit_fixture_baseline "${root}"
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
commit_fixture_baseline "${root}"
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

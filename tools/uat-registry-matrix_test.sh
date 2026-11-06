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

# Unit harness for .github/scripts/uat-registry-matrix.sh.
# Run directly: bash tools/uat-registry-matrix_test.sh
# Wired into CI via `make test` (test-shell target).
#
# Hermetic: builds tools/uat-broker from this checkout into a temp dir, writes
# fixture registries and cluster-configs there, and runs the script from that
# fixture root. Every cloud CLI and docker is shadowed by a failing PATH stub.
#
# It never skips. A missing go, yq, or jq FAILS the suite: one that switches
# itself off when a dependency is absent reports green while checking nothing.
#
# Every failure case also asserts the ::error:: line names its cause, so a
# fixture the broker happens to reject cannot pass a case for the wrong reason.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${REPO_ROOT}/.github/scripts/uat-registry-matrix.sh"

abort() { echo "FAIL: $*" >&2; exit 1; }
for tool in go yq jq; do
    command -v "${tool}" >/dev/null 2>&1 || abort "${tool} is not on PATH; this suite needs it and never skips"
done

WORK="$(mktemp -d)" || abort "could not create a temporary directory"
trap 'rm -rf "${WORK}"' EXIT

( cd "${REPO_ROOT}" && go build -o "${WORK}/uat-broker" ./tools/uat-broker ) >"${WORK}/build.log" 2>&1 \
    || abort "go build ./tools/uat-broker failed: $(cat "${WORK}/build.log")"

STUBS="${WORK}/stubs"
mkdir -p "${STUBS}"
for cli in aws az gcloud terraform docker kubectl; do
    printf '#!/usr/bin/env bash\necho "%s $*" >>"%s/cloud-trace"\nexit 1\n' "${cli}" "${WORK}" >"${STUBS}/${cli}"
    chmod +x "${STUBS}/${cli}"
done

# run_matrix <dir> <args...>: runs the script from <dir> with UT_BROKER and
# UT_PATH; stdout lands in ${WORK}/out, stderr in ${WORK}/err.
UT_BROKER="${WORK}/uat-broker"
UT_PATH="${STUBS}:${PATH}"
run_matrix() {
    local dir="$1"; shift
    ( cd "${dir}" && PATH="${UT_PATH}" UAT_BROKER="${UT_BROKER}" "${SCRIPT}" "$@" ) \
        >"${WORK}/out" 2>"${WORK}/err"
}

PASSED=0 FAILED=0
pass() { echo "  ok   $1"; PASSED=$((PASSED + 1)); }
fail() { echo "  FAIL $1: $2"; FAILED=$((FAILED + 1)); }

# expect_json <label> <want> <dir> <args...>: exit 0 and stdout exactly <want>.
expect_json() {
    local label="$1" want="$2"; shift 2
    run_matrix "$@"; local rc=$?
    local got; got="$(cat "${WORK}/out")"
    if [[ "${rc}" -eq 0 && "${got}" == "${want}" ]]; then
        pass "${label}"
    else
        fail "${label}" "rc=${rc}, want '${want}', got '${got}', stderr: $(tr '\n' ' ' <"${WORK}/err")"
    fi
}

# expect_fail <label> <needle> <dir> <args...>: non-zero exit, empty stdout, and
# a ::error:: line containing <needle>.
expect_fail() {
    local label="$1" needle="$2"; shift 2
    run_matrix "$@"; local rc=$?
    if [[ "${rc}" -ne 0 && ! -s "${WORK}/out" ]] &&
       grep '^::error::' "${WORK}/err" | grep -qF -- "${needle}"; then
        pass "${label}"
    else
        fail "${label}" "rc=${rc}, stdout '$(cat "${WORK}/out")', want ::error:: naming '${needle}', stderr: $(tr '\n' ' ' <"${WORK}/err")"
    fi
}

# --- fixtures ----------------------------------------------------------------
# row <name> <slug> <cloud> <cluster-config-path> [daytime-intent]: one registry
# row carrying every field the broker requires.
row() {
    printf '  - name: %s\n    slug: %s\n    cloud: %s\n    accelerator: h100\n    gpu-count: 8\n    cluster-config-path: %s\n    test-config-dir: tests\n' \
        "$1" "$2" "$3" "$4"
    [[ -z "${5:-}" ]] || printf '    daytime-intent: %s\n' "$5"
}
# registry <file> <row-output...>: a registry document from row() output.
registry() { local f="$1"; shift; { echo "reservations:"; printf '%s\n' "$@"; } >"${f}"; }
# cfg <path> <tenancy-yaml> <location-yaml> [state] [stateLocation]: a
# cluster-config. Values are written verbatim, so callers quote strings and can
# pass a bare null.
cfg() {
    mkdir -p "$(dirname "$1")"
    {
        echo "deployment:"
        echo "  tenancy: $2"
        echo "  location: $3"
        [[ -z "${4:-}" ]] || echo "  state: $4"
        [[ -z "${5:-}" ]] || echo "  stateLocation: $5"
    } >"$1"
}

FX="${WORK}/fx"
mkdir -p "${FX}"
cd "${FX}" || abort "cannot enter ${FX}"
cfg c/a.yaml '"111111111111"' us-east-1 tenancy
cfg c/a2.yaml '"111111111111"' us-east-2 tenancy
cfg c/b.yaml '"222222222222"' us-west-2 tenancy
# A plausible AWS pair, so a non-aws row that were not ignored would add a leg.
cfg c/other.yaml '"999999999999"' eu-west-1 tenancy
cfg c/p.yaml '"333333333333"' us-east-2 tenancy us-east-1
cfg c/q.yaml '"333333333333"' us-east-2 tenancy us-east-1
cfg c/q-noloc.yaml '"333333333333"' us-east-2 tenancy
cfg c/q-local.yaml '"333333333333"' us-east-2 local us-east-1

echo "janitor-aws:"
registry share.yaml "$(row a1 aa aws c/a.yaml)" "$(row a2 ab aws c/a.yaml)"
expect_json "rows sharing a config collapse to one leg" \
    '[{"tenancy":"111111111111","location":"us-east-1","config":"c/a.yaml"}]' \
    "${FX}" janitor-aws share.yaml

registry regions.yaml "$(row a1 aa aws c/a.yaml)" "$(row a2 ab aws c/a2.yaml)"
expect_json "one tenancy in two regions gives two legs" \
    '[{"tenancy":"111111111111","location":"us-east-1","config":"c/a.yaml"},{"tenancy":"111111111111","location":"us-east-2","config":"c/a2.yaml"}]' \
    "${FX}" janitor-aws regions.yaml

registry ignore.yaml "$(row g1 ga gcp c/other.yaml)" "$(row a1 aa aws c/a.yaml)" \
    "$(row z1 za azure c/other.yaml)" "$(row k1 ka kind c/other.yaml)"
expect_json "gcp, azure and kind rows are ignored" \
    '[{"tenancy":"111111111111","location":"us-east-1","config":"c/a.yaml"}]' \
    "${FX}" janitor-aws ignore.yaml

# Registry order is deliberately unsorted on both keys: 222 first, and within
# 111 the us-east-2 row before us-east-1.
registry mixed.yaml "$(row b1 ba aws c/b.yaml)" "$(row g1 ga gcp c/other.yaml)" \
    "$(row a3 ac aws c/a2.yaml)" "$(row a1 aa aws c/a.yaml)" "$(row a2 ab aws c/a.yaml)" \
    "$(row z1 za azure c/other.yaml)" "$(row k1 ka kind c/other.yaml)"
expect_json "JSON is exact, compact, and sorted by tenancy then location" \
    '[{"tenancy":"111111111111","location":"us-east-1","config":"c/a.yaml"},{"tenancy":"111111111111","location":"us-east-2","config":"c/a2.yaml"},{"tenancy":"222222222222","location":"us-west-2","config":"c/b.yaml"}]' \
    "${FX}" janitor-aws mixed.yaml

# q precedes p in the registry, so "first in registry order" and "first
# alphabetically" disagree.
registry samebackend.yaml "$(row q1 qa aws c/q.yaml)" "$(row p1 pa aws c/p.yaml)"
expect_json "two configs with equal state/stateLocation give one leg, first path" \
    '[{"tenancy":"333333333333","location":"us-east-2","config":"c/q.yaml"}]' \
    "${FX}" janitor-aws samebackend.yaml

# stateLocation absent on one side reads as empty, which differs.
registry diffloc.yaml "$(row p1 pa aws c/p.yaml)" "$(row q1 qa aws c/q-noloc.yaml)"
expect_fail "unequal stateLocation in one pair" "c/q-noloc.yaml" "${FX}" janitor-aws diffloc.yaml
registry diffstate.yaml "$(row p1 pa aws c/p.yaml)" "$(row q1 qa aws c/q-local.yaml)"
expect_fail "unequal state in one pair" "c/q-local.yaml" "${FX}" janitor-aws diffstate.yaml

registry missing.yaml "$(row a1 aa aws c/a.yaml)" "$(row a2 ab aws c/absent.yaml)"
expect_fail "missing config file" "c/absent.yaml" "${FX}" janitor-aws missing.yaml
# The absolute path names a file that exists, so only the rule rejects it.
registry absolute.yaml "$(row a1 aa aws "${FX}/c/a.yaml")"
expect_fail "absolute config path" "${FX}/c/a.yaml" "${FX}" janitor-aws absolute.yaml
registry dotdot.yaml "$(row a1 aa aws c/../c/a.yaml)"
expect_fail "config path with a .. segment" "c/../c/a.yaml" "${FX}" janitor-aws dotdot.yaml

for t in '""' null '"12345678901"' '"1234567890123"' '"12345678901a"' abcdefghijkl; do
    cfg c/bad-tenancy.yaml "${t}" us-east-1 tenancy
    registry badtenancy.yaml "$(row a1 aa aws c/bad-tenancy.yaml)"
    expect_fail "tenancy ${t} is rejected" ".deployment.tenancy" "${FX}" janitor-aws badtenancy.yaml
done
for l in '""' null '"us east-1"' '"us-east-1;touch"' "'\$(id)'"; do
    cfg c/bad-location.yaml '"111111111111"' "${l}" tenancy
    registry badlocation.yaml "$(row a1 aa aws c/bad-location.yaml)"
    expect_fail "location ${l} is rejected" ".deployment.location" "${FX}" janitor-aws badlocation.yaml
done

registry noaws.yaml "$(row g1 ga gcp c/other.yaml)" "$(row k1 ka kind c/other.yaml)"
expect_fail "no aws rows" "no aws reservation" "${FX}" janitor-aws noaws.yaml

registry unknownkey.yaml "$(row a1 aa aws c/a.yaml)
    bogus-key: x"
expect_fail "registry with an unknown key" "uat-broker" "${FX}" janitor-aws unknownkey.yaml
registry dupname.yaml "$(row a1 aa aws c/a.yaml)" "$(row a1 ab aws c/a.yaml)"
expect_fail "registry with a duplicate name" "uat-broker" "${FX}" janitor-aws dupname.yaml

echo "arguments and prerequisites:"
expect_fail "unknown mode" "unknown mode" "${FX}" janitor-gcp mixed.yaml
expect_fail "missing args" "usage" "${FX}"
expect_fail "missing registry arg" "usage" "${FX}" janitor-aws
UT_BROKER="${WORK}/no-such-broker"
expect_fail "UAT_BROKER names a missing file" "${WORK}/no-such-broker" "${FX}" janitor-aws mixed.yaml
UT_BROKER="${WORK}/uat-broker"

# uat-janitor.yaml relies on the ./bin/uat-broker default rather than setting
# UAT_BROKER, so pin it: unset finds the binary there, and fails without it.
mkdir -p "${FX}/bin"
cp "${WORK}/uat-broker" "${FX}/bin/uat-broker"
got="$(cd "${FX}" && env -u UAT_BROKER PATH="${UT_PATH}" "${SCRIPT}" janitor-aws share.yaml 2>"${WORK}/err")"
if [[ "${got}" == '[{"tenancy":"111111111111","location":"us-east-1","config":"c/a.yaml"}]' ]]; then
    pass "UAT_BROKER unset defaults to ./bin/uat-broker"
else
    fail "UAT_BROKER unset defaults to ./bin/uat-broker" "got '${got}', stderr: $(tr '\n' ' ' <"${WORK}/err")"
fi
rm -rf "${FX:?}/bin"
UT_BROKER=""
expect_fail "UAT_BROKER empty and no ./bin/uat-broker" "./bin/uat-broker" "${FX}" janitor-aws share.yaml
UT_BROKER="${WORK}/uat-broker"

# jq absent: PATH holds only bash and yq.
NOJQ="${WORK}/nojq"
mkdir -p "${NOJQ}"
ln -s "$(command -v bash)" "${NOJQ}/bash"
ln -s "$(command -v yq)" "${NOJQ}/yq"
UT_PATH="${NOJQ}"
expect_fail "jq absent from PATH" "jq" "${FX}" daytime-down mixed.yaml
UT_PATH="${STUBS}:${PATH}"

# yq absent, broker and jq reachable: PATH holds only bash and jq.
NOYQ="${WORK}/noyq"
mkdir -p "${NOYQ}"
ln -s "$(command -v bash)" "${NOYQ}/bash"
ln -s "$(command -v jq)" "${NOYQ}/jq"
UT_PATH="${NOYQ}"
expect_fail "yq absent from PATH" "yq" "${FX}" janitor-aws mixed.yaml
UT_PATH="${STUBS}:${PATH}"

# stub_broker <file> <--name output>: --list prints a1; --name prints the given
# lines, or exits 3 when they are the word FAIL.
stub_broker() {
    local body
    if [[ "$2" == FAIL ]]; then
        body='echo "stub: lookup failed" >&2; exit 3'
    else
        body="printf '%s\\n' $2"
    fi
    printf '#!/usr/bin/env bash\ncase " $* " in *" --list "*) echo a1 ;; *) %s ;; esac\n' "${body}" >"$1"
    chmod +x "$1"
}
stub_broker "${WORK}/broker-name-fails" FAIL
stub_broker "${WORK}/broker-no-cloud" "slug=aa cluster-config-path=c/a.yaml daytime-intent="
stub_broker "${WORK}/broker-bogus-intent" "slug=aa cloud=aws cluster-config-path=c/a.yaml daytime-intent=bogus"
stub_broker "${WORK}/broker-no-intent" "slug=aa cloud=aws cluster-config-path=c/a.yaml"

UT_BROKER="${WORK}/broker-name-fails"
expect_fail "broker exits non-zero on --name" "reservation a1" "${FX}" janitor-aws mixed.yaml
UT_BROKER="${WORK}/broker-no-cloud"
expect_fail "broker --name output without cloud=" "cloud=" "${FX}" janitor-aws mixed.yaml
UT_BROKER="${WORK}/uat-broker"

echo "daytime-down:"
# One daytime-intent per cloud is all the broker admits. The kind row sits
# mid-registry and carries an intent, so its absence is the mode's rule.
registry daytime.yaml "$(row a1 aa aws c/a.yaml)" "$(row k1 ka kind c/other.yaml training)" \
    "$(row a2 ab aws c/b.yaml inference)" "$(row g1 ga gcp c/other.yaml)" \
    "$(row z1 za azure c/other.yaml inference)"
expect_json "every non-kind row in registry order, intent or training" \
    '[{"reservation":"a1","intent":"training"},{"reservation":"a2","intent":"inference"},{"reservation":"g1","intent":"training"},{"reservation":"z1","intent":"inference"}]' \
    "${FX}" daytime-down daytime.yaml

registry kindonly.yaml "$(row k1 ka kind c/other.yaml)"
expect_fail "no non-kind row" "no non-kind reservation" "${FX}" daytime-down kindonly.yaml
expect_fail "registry the broker rejects" "uat-broker" "${FX}" daytime-down unknownkey.yaml
UT_BROKER="${WORK}/broker-bogus-intent"
expect_fail "broker prints daytime-intent=bogus" "bogus" "${FX}" daytime-down mixed.yaml
UT_BROKER="${WORK}/broker-no-intent"
expect_fail "broker prints no daytime-intent= line" "daytime-intent=" "${FX}" daytime-down mixed.yaml
UT_BROKER="${WORK}/uat-broker"

echo "smoke (committed registry, repo root):"
# Pins no rows, so onboarding a reservation does not break it.
smoke() { # smoke <mode> <jq key array>
    run_matrix "${REPO_ROOT}" "$1" infra/uat/reservations.yaml; local rc=$?
    if [[ "${rc}" -eq 0 ]] &&
       jq -e --argjson k "$2" 'type == "array" and length > 0 and all(.[]; keys == $k)' "${WORK}/out" >/dev/null 2>&1 &&
       [[ "$(wc -l <"${WORK}/out" | tr -d ' ')" == 1 ]]; then
        pass "$1 on the committed registry"
    else
        fail "$1 on the committed registry" "rc=${rc}, stdout '$(cat "${WORK}/out")', stderr: $(tr '\n' ' ' <"${WORK}/err")"
    fi
}
smoke janitor-aws '["config","location","tenancy"]'
smoke daytime-down '["intent","reservation"]'

# Harness guard, not a case: nothing reached a cloud CLI or docker stub.
if [[ -s "${WORK}/cloud-trace" ]]; then
    fail "no cloud CLI or docker invoked" "$(tr '\n' ';' <"${WORK}/cloud-trace")"
fi

echo "${PASSED} passed, ${FAILED} failed"
if [[ "${FAILED}" -eq 0 ]]; then
    echo "PASS: uat-registry-matrix"
else
    echo "FAIL: uat-registry-matrix"
fi
[[ "${FAILED}" -eq 0 ]]

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
#
# Hermetic tests for nvsentinel-mongodb.sh. kubectl is a stub that serves
# fixture files; the certificates are real, from two throwaway CAs, so the
# chain checks run openssl as the lanes do.

set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for tool in jq openssl base64; do
  command -v "${tool}" >/dev/null 2>&1 || { echo "FAIL: ${tool} is not on PATH" >&2; exit 1; }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/nvsentinel-mongodb-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
fails=0
ran=0
pass() { ran=$((ran + 1)); echo "  ok   $1"; }
fail() { ran=$((ran + 1)); echo "FAIL: $1" >&2; fails=$((fails + 1)); }

# shellcheck source=tests/e2e/lib/nvsentinel-mongodb.sh
. "${SCRIPT_DIR}/nvsentinel-mongodb.sh"
NVSENTINEL_MONGODB_POLL_SECONDS=0.05

# ca <name> and leaf <name> <ca> write <name>.crt and <name>.key into WORK.
ca() {
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
    -subj "/CN=$1" -keyout "${WORK}/$1.key" -out "${WORK}/$1.crt" >/dev/null 2>&1
}
leaf() {
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$1" \
    -keyout "${WORK}/$1.key" -out "${WORK}/$1.csr" >/dev/null 2>&1 &&
    openssl x509 -req -in "${WORK}/$1.csr" -CA "${WORK}/$2.crt" -CAkey "${WORK}/$2.key" \
      -CAcreateserial -days 1 -out "${WORK}/$1.crt" >/dev/null 2>&1
}
if ! { ca ca1 && ca ca2 && leaf server1 ca1 && leaf server2 ca2 && leaf client1 ca1 && leaf client2 ca2; }; then
  echo "FAIL: could not create test certificates with openssl" >&2
  exit 1
fi

FIX="${WORK}/fixtures"

# secret <name> <cert> writes a Secret fixture whose tls.crt is <cert>.crt.
secret() {
  jq -n --arg crt "$(base64 <"${WORK}/$2.crt" | tr -d '\n')" '{data: {"tls.crt": $crt}}' >"${FIX}/secret-$1.json"
}

# setup <ca> <server> <client> <issuing> [served] writes a cluster state.
# <served> is the certificate mongod presents on its listener; it defaults to
# the server Secret's.
setup() {
  rm -rf "${FIX}" && mkdir -p "${FIX}"
  secret "${NVSENTINEL_MONGODB_CA_CERT}" "$1"
  secret "${NVSENTINEL_MONGODB_SERVER_SECRET}" "$2"
  secret "${NVSENTINEL_MONGODB_CLIENT_SECRET}" "$3"
  jq -n --arg issuing "$4" '{status: {conditions: ([{type: "Ready", status: "True"}] +
    (if $issuing == "yes" then [{type: "Issuing", status: "True"}] else [] end))}}' >"${FIX}/certificate.json"
  cp "${WORK}/${5:-$2}.crt" "${FIX}/served.pem"
  : >"${FIX}/deletes"
  rm -f "${FIX}/reissue"
  STUB_DELAY=0
}

# kubectl serves the fixtures. Each call takes STUB_DELAY seconds and fails,
# like a timed-out request, when that exceeds its --request-timeout. exec
# answers like openssl s_client with the served certificate. A delete of the
# client Secret records itself and, when FIX/reissue names a certificate, puts
# it back signed by that certificate's CA, as cert-manager would.
kubectl() {
  local verb="" kind="" name="" timeout=""
  while (($#)); do
    case "$1" in
      --) break ;;
      --context | -n | -o | -c) shift ;;
      --request-timeout=*) timeout=${1#--request-timeout=} timeout=${timeout%s} ;;
      --*) ;;
      *)
        if [[ -z ${verb} ]]; then verb=$1
        elif [[ -z ${kind} ]]; then kind=$1
        elif [[ -z ${name} ]]; then name=$1
        fi
        ;;
    esac
    shift
  done
  [[ -n ${timeout} ]] || { echo "kubectl called without --request-timeout" >&2; return 1; }
  if [[ ${STUB_DELAY} != 0 ]]; then
    if awk -v d="${STUB_DELAY}" -v t="${timeout}" 'BEGIN { exit !(d > t) }'; then
      sleep "${timeout}"
      return 1
    fi
    sleep "${STUB_DELAY}"
  fi
  case "${verb} ${kind}" in
    "get secret") cat "${FIX}/secret-${name}.json" 2>/dev/null ;;
    "get certificate") cat "${FIX}/certificate.json" ;;
    "get perconaservermongodb") printf 'ready' ;;
    "get statefulset") printf '1' ;;
    "exec ${NVSENTINEL_MONGODB_STATEFULSET}-0") printf 'CONNECTED(00000003)\n---\nServer certificate\n' && cat "${FIX}/served.pem" ;;
    "delete secret")
      echo "${name}" >>"${FIX}/deletes"
      rm -f "${FIX}/secret-${name}.json"
      [[ -s "${FIX}/reissue" ]] && secret "${name}" "$(cat "${FIX}/reissue")"
      ;;
    *) echo "unexpected kubectl call: ${verb} ${kind} ${name}" >&2; return 1 ;;
  esac
}

# run_case <name> <want-rc> <want-deletes> <want-stderr-substring> [budget]
run_case() {
  local rc deletes
  nvsentinel_mongodb_ensure_tls_chain kind-test nvsentinel $((SECONDS + ${5:-2})) 2>"${WORK}/stderr"
  rc=$?
  deletes=$(grep -c . "${FIX}/deletes")
  if [[ ${rc} -ne $2 ]]; then
    fail "$1: returned ${rc}, want $2 ($(cat "${WORK}/stderr"))"
  elif [[ ${deletes} -ne $3 ]]; then
    fail "$1: deleted the client Secret ${deletes} times, want $3"
  elif [[ -n $4 ]] && ! grep -qF -- "$4" "${WORK}/stderr"; then
    fail "$1: stderr does not mention '$4': $(cat "${WORK}/stderr")"
  else
    pass "$1"
  fi
}

setup ca2 server2 client2 no
run_case "consistent chain returns without touching anything" 0 0 ""

setup ca2 server2 client1 no
echo client2 >"${FIX}/reissue"
run_case "client from an earlier CA is re-issued once and then accepted" 0 1 "is not signed by CA"

setup ca2 server2 client1 no
echo client1 >"${FIX}/reissue"
run_case "client that stays on the earlier CA fails by the deadline, deleted once" 1 1 \
  "${NVSENTINEL_MONGODB_CLIENT_SECRET} to be re-issued"

setup ca2 server1 client2 no
run_case "server certificate from an earlier CA is waited on, not touched" 1 0 \
  "MongoDB's server certificate to be re-issued"

setup ca2 server2 client2 yes
run_case "a CA cert-manager is still issuing is waited on" 1 0 "the CA Certificate to settle"

# Percona has not yet rolled mongod onto the re-issued Secret: every Secret is
# current, but the listener still presents the earlier certificate.
setup ca2 server2 client2 no server1
run_case "mongod still serving the earlier certificate is waited on" 1 0 "MongoDB to serve a certificate"

setup ca2 server2 client2 no
if out=$(set -u; nvsentinel_mongodb_ensure_tls_chain kind-test nvsentinel $((SECONDS - 1)) 2>&1); then
  fail "an expired deadline returned 0"
elif [[ ${out} == *"unbound variable"* || ${out} != *"by the deadline"* ]]; then
  fail "an expired deadline did not report under set -u: ${out}"
else
  pass "an expired deadline reports why under set -u"
fi

# Every request takes 0.7s, so the six requests of the success path cannot
# finish inside a 1-second budget. The helper must not report success, and
# bounding each request by the time left must keep it close to the deadline.
setup ca2 server2 client2 no
STUB_DELAY=0.7
start=${SECONDS}
run_case "a slow API does not report success after the deadline" 1 0 "by the deadline" 1
elapsed=$((SECONDS - start))
if ((elapsed <= 3)); then
  pass "a slow API stops within the deadline's budget (${elapsed}s)"
else
  fail "a slow API overran a 1-second deadline by $((elapsed - 1))s"
fi
STUB_DELAY=0

echo "${ran} cases, ${fails} failed"
[[ ${fails} -eq 0 ]]

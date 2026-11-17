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
# Shared by the NVSentinel Kind E2E lanes that install nvsentinel-mongodb.
# Sourcing it defines functions only. Needs kubectl, jq, openssl and base64 on
# the host, and runs openssl, sh and timeout inside the mongod container.
#
# cert-manager can issue the datastore CA twice while the Percona operator sets
# it up (#3842). A certificate signed between the two issuances chains to the
# first CA while MongoDB serves one from the second. The usual victim is
# nvsentinel-mongodb-app-client: it waits on the operator's issuer and is
# signed the moment that issuer exists. Every certificate still reports Ready,
# so a lane's waits pass, and each NVSentinel client then fails TLS with
# "x509: certificate signed by unknown authority".

NVSENTINEL_MONGODB_CA_CERT=nvsentinel-mongodb-ca-cert
NVSENTINEL_MONGODB_SERVER_SECRET=nvsentinel-mongodb-ssl
NVSENTINEL_MONGODB_CLIENT_SECRET=nvsentinel-mongodb-app-client
NVSENTINEL_MONGODB_STATEFULSET=nvsentinel-mongodb-rs0
NVSENTINEL_MONGODB_PORT=27017
NVSENTINEL_MONGODB_POLL_SECONDS=${NVSENTINEL_MONGODB_POLL_SECONDS:-5}
NVSENTINEL_MONGODB_REQUEST_SECONDS=30
NVSENTINEL_MONGODB_PROBE_SECONDS=10

# _nvsentinel_mongodb_budget <cap> prints the whole seconds left before the
# caller's deadline, at most <cap>, and returns 1 when none are left. It reads
# _nvsentinel_mongodb_deadline, which nvsentinel_mongodb_ensure_tls_chain sets.
_nvsentinel_mongodb_budget() {
  local left=$((_nvsentinel_mongodb_deadline - SECONDS))
  ((left > 0)) || return 1
  echo $((left < $1 ? left : $1))
}

_nvsentinel_mongodb_kc() {
  local ctx=$1 ns=$2 budget
  shift 2
  budget=$(_nvsentinel_mongodb_budget "${NVSENTINEL_MONGODB_REQUEST_SECONDS}") || return 1
  kubectl --context "${ctx}" -n "${ns}" --request-timeout="${budget}s" "$@"
}

_nvsentinel_mongodb_pause() {
  local left=$((_nvsentinel_mongodb_deadline - SECONDS))
  ((left > 0)) || return 0
  if [[ ${NVSENTINEL_MONGODB_POLL_SECONDS} =~ ^[0-9]+$ ]] && ((left < NVSENTINEL_MONGODB_POLL_SECONDS)); then
    sleep "${left}"
  else
    sleep "${NVSENTINEL_MONGODB_POLL_SECONDS}"
  fi
}

# _nvsentinel_mongodb_pem <context> <namespace> <secret> <key> prints the
# decoded value, or nothing when the Secret or key is absent.
_nvsentinel_mongodb_pem() {
  _nvsentinel_mongodb_kc "$1" "$2" get secret "$3" -o json 2>/dev/null |
    jq -r --arg key "$4" '.data[$key] // empty' | base64 -d 2>/dev/null
}

_nvsentinel_mongodb_fingerprint() {
  openssl x509 -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2
}

# _nvsentinel_mongodb_signed_by <ca-pem> <cert-pem>
_nvsentinel_mongodb_signed_by() {
  openssl verify -CAfile <(printf '%s\n' "$1") <(printf '%s\n' "$2") >/dev/null 2>&1
}

# _nvsentinel_mongodb_ca_settled: the CA Certificate is Ready and cert-manager
# is not issuing it again.
_nvsentinel_mongodb_ca_settled() {
  _nvsentinel_mongodb_kc "$1" "$2" get certificate "${NVSENTINEL_MONGODB_CA_CERT}" -o json 2>/dev/null |
    jq -e '[.status.conditions[]?] |
      any(.type == "Ready" and .status == "True") and
      (any(.type == "Issuing" and .status == "True") | not)' >/dev/null
}

# _nvsentinel_mongodb_served_by <context> <namespace> <ca-pem>: the Percona
# cluster is ready and every replica-set member serves a certificate signed by
# <ca-pem>. The certificate is read from each member's own TLS listener: its
# Secret and the StatefulSet revision can both be current while mongod still
# serves one it loaded before the Secret was re-issued.
_nvsentinel_mongodb_served_by() {
  local ctx=$1 ns=$2 ca=$3 replicas i budget served
  [[ "$(_nvsentinel_mongodb_kc "${ctx}" "${ns}" get perconaservermongodb nvsentinel-mongodb \
    -o jsonpath='{.status.state}' 2>/dev/null)" == ready ]] || return 1
  replicas=$(_nvsentinel_mongodb_kc "${ctx}" "${ns}" get statefulset "${NVSENTINEL_MONGODB_STATEFULSET}" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null)
  [[ ${replicas} =~ ^[1-9][0-9]*$ ]] || return 1
  for ((i = 0; i < replicas; i++)); do
    budget=$(_nvsentinel_mongodb_budget "${NVSENTINEL_MONGODB_PROBE_SECONDS}") || return 1
    served=$(_nvsentinel_mongodb_kc "${ctx}" "${ns}" exec "${NVSENTINEL_MONGODB_STATEFULSET}-${i}" -c mongod -- \
      timeout "${budget}" sh -c "openssl s_client -connect localhost:${NVSENTINEL_MONGODB_PORT} </dev/null" 2>/dev/null |
      sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p')
    if [[ -z ${served} ]] || ! _nvsentinel_mongodb_signed_by "${ca}" "${served}"; then
      return 1
    fi
  done
}

# nvsentinel_mongodb_ensure_tls_chain <context> <namespace> <deadline>
#
# Returns 0 once the CA has settled, every MongoDB member serves a certificate
# signed by it, and the client certificate is signed by it too. A client
# certificate from an earlier CA is re-issued by deleting its Secret, once per
# CA. <deadline> is an absolute value of $SECONDS, and every Kubernetes request,
# probe and pause is bounded by what is left of it. Returns 1, naming what is
# out of step, when that does not hold by the deadline.
nvsentinel_mongodb_ensure_tls_chain() {
  local ctx=$1 ns=$2 _nvsentinel_mongodb_deadline=$3
  local ca server client ca_fp reissued_for="" waiting="the CA Certificate to settle" next
  while ((SECONDS < _nvsentinel_mongodb_deadline)); do
    next="the CA Certificate to settle"
    if _nvsentinel_mongodb_ca_settled "${ctx}" "${ns}"; then
      ca=$(_nvsentinel_mongodb_pem "${ctx}" "${ns}" "${NVSENTINEL_MONGODB_CA_CERT}" tls.crt)
      server=$(_nvsentinel_mongodb_pem "${ctx}" "${ns}" "${NVSENTINEL_MONGODB_SERVER_SECRET}" tls.crt)
      client=$(_nvsentinel_mongodb_pem "${ctx}" "${ns}" "${NVSENTINEL_MONGODB_CLIENT_SECRET}" tls.crt)
      ca_fp=$(_nvsentinel_mongodb_fingerprint <<<"${ca}")
      if [[ -z ${ca_fp} || -z ${server} || -z ${client} ]]; then
        next="the CA, server and client certificate Secrets"
      elif ! _nvsentinel_mongodb_signed_by "${ca}" "${server}"; then
        # The Percona operator re-issues the server certificates itself when
        # they trail the CA.
        next="MongoDB's server certificate to be re-issued from CA ${ca_fp}"
      elif ! _nvsentinel_mongodb_signed_by "${ca}" "${client}"; then
        next="${NVSENTINEL_MONGODB_CLIENT_SECRET} to be re-issued from CA ${ca_fp}"
        if [[ ${reissued_for} != "${ca_fp}" ]]; then
          echo "nvsentinel-mongodb: ${NVSENTINEL_MONGODB_CLIENT_SECRET} is not signed by CA ${ca_fp}," \
            "which signed MongoDB's server certificate; deleting it so cert-manager re-issues it" >&2
          if _nvsentinel_mongodb_kc "${ctx}" "${ns}" delete secret "${NVSENTINEL_MONGODB_CLIENT_SECRET}" \
            --ignore-not-found --wait=false >/dev/null; then
            reissued_for=${ca_fp}
          fi
        fi
      elif ! _nvsentinel_mongodb_served_by "${ctx}" "${ns}" "${ca}"; then
        next="MongoDB to serve a certificate signed by CA ${ca_fp}"
      elif ((SECONDS < _nvsentinel_mongodb_deadline)); then
        # cert-manager can re-issue the CA while the checks above run, so the
        # certificates may chain to one that is no longer current.
        if _nvsentinel_mongodb_ca_settled "${ctx}" "${ns}" &&
          [[ $(_nvsentinel_mongodb_pem "${ctx}" "${ns}" "${NVSENTINEL_MONGODB_CA_CERT}" tls.crt |
            _nvsentinel_mongodb_fingerprint) == "${ca_fp}" ]] &&
          ((SECONDS < _nvsentinel_mongodb_deadline)); then
          return 0
        fi
        next="the CA Certificate to settle"
      else
        waiting="checks that finish before the deadline; they passed only after it"
        break
      fi
    fi
    # A pass the deadline cut short proves nothing, so the reason reported is
    # the last one a complete pass found.
    if ((SECONDS < _nvsentinel_mongodb_deadline)); then
      waiting=${next}
    fi
    _nvsentinel_mongodb_pause
  done
  echo "nvsentinel-mongodb: TLS chain not consistent by the deadline; still waiting for ${waiting}" >&2
  return 1
}

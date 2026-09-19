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

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"
# shellcheck source=/dev/null
source ./upstream.env

# ==============================================================================
# Kubernetes connection resolution
# ==============================================================================
# Inputs, all optional:
#   KUBE_CONTEXT     context name to act on.
#   KUBECONFIG       path to a kubeconfig. Both helm and kubectl read it from
#                    the environment, so no flag is derived from it.
#   KUBECONFIG_FLAG  deprecated. A literal helm flag string, translated below.
#
# helm and kubectl spell the same connection option differently -- helm's
# --kube-context is kubectl's --context -- so a context resolves to two arrays
# rather than one shared string. Forwarding helm's spelling to kubectl aborts on
# an unknown flag; dropping it silently is worse, because the reads and the
# cluster writes would then land on whatever context the ambient kubeconfig
# names, which is the wrong-cluster write this contract exists to prevent.
#
# An option this contract does not translate is refused rather than guessed at.
#
# Every rejection below exits before the first cluster call. A connection
# option that is merely dropped is indistinguishable from one never set, and
# the fallback is the ambient context.
#
# Both arrays are always declared, but a given script calls only one of the two
# binaries, so the other is legitimately unread here.
# shellcheck disable=SC2034
HELM_CONN=()
# shellcheck disable=SC2034
KUBECTL_CONN=()

if [[ -n "${KUBECONFIG_FLAG:-}" ]]; then
  echo "WARNING: KUBECONFIG_FLAG is deprecated; export KUBE_CONTEXT (and KUBECONFIG) instead." >&2

  # Deliberate word-split: this slot holds a flag list, not a single word.
  # Globbing is suppressed around it because the same unquoted expansion also
  # performs pathname expansion, and a kubeconfig path is a value rather than a
  # pattern: one match for /tmp/kc*.yaml silently substitutes a different file,
  # and several append tokens that are then rejected as unsupported options.
  # Restored only if this shell had it enabled, so an embedding script that
  # deliberately runs with `set -f` keeps it.
  # shellcheck disable=SC2206
  _aicr_reglob=0
  [[ -o noglob ]] || { _aicr_reglob=1; set -f; }
  _aicr_argv=(${KUBECONFIG_FLAG})
  (( _aicr_reglob == 0 )) || set +f
  _aicr_i=0
  _aicr_n=${#_aicr_argv[@]}
  _aicr_ctx=""

  # Indexed rather than reslicing: `arr=("${arr[@]:2}")` expands to nothing on
  # the final pair, and bash 3.2 -- which stock macOS still ships -- errors on
  # an empty array expansion under `set -u`.
  while (( _aicr_i < _aicr_n )); do
    _aicr_tok="${_aicr_argv[_aicr_i]}"
    case "${_aicr_tok}" in
      --kube-context|--kubeconfig)
        if (( _aicr_i + 1 >= _aicr_n )); then
          echo "ERROR: KUBECONFIG_FLAG ends with ${_aicr_tok} and no value." >&2
          exit 1
        fi
        _aicr_val="${_aicr_argv[_aicr_i+1]}"
        # Another option where the value belongs means the value was omitted.
        # Accepting it would name a flag as the context or the kubeconfig path,
        # and the resulting lookup failure is not the error the operator would
        # read as "you forgot a value".
        # Names the offending option, never its argument, for the same reason
        # the catch-all below does.
        if [[ "${_aicr_val}" == --* ]]; then
          echo "ERROR: KUBECONFIG_FLAG gives ${_aicr_tok} the value" >&2
          echo "       '${_aicr_val%%=*}', which is another option rather than a value." >&2
          exit 1
        fi
        if [[ "${_aicr_tok}" == "--kube-context" ]]; then
          _aicr_ctx="${_aicr_val}"
        else
          # Exported as well as passed: bash cannot export an array, and the
          # deprecated variable is unset once translated, so a child script
          # re-running this prologue would otherwise see no kubeconfig at all
          # and fall back to the ambient one.
          KUBECONFIG="${_aicr_val}"
          export KUBECONFIG
          HELM_CONN+=(--kubeconfig "${_aicr_val}")
          KUBECTL_CONN+=(--kubeconfig "${_aicr_val}")
        fi
        _aicr_i=$(( _aicr_i + 2 ))
        ;;
      --kube-context=*|--kubeconfig=*)
        _aicr_val="${_aicr_tok#*=}"
        if [[ -z "${_aicr_val}" ]]; then
          echo "ERROR: KUBECONFIG_FLAG carries '${_aicr_tok}', whose value is empty." >&2
          exit 1
        fi
        if [[ "${_aicr_tok}" == --kube-context=* ]]; then
          _aicr_ctx="${_aicr_val}"
        else
          KUBECONFIG="${_aicr_val}"
          export KUBECONFIG
          HELM_CONN+=("${_aicr_tok}")
          KUBECTL_CONN+=("${_aicr_tok}")
        fi
        _aicr_i=$(( _aicr_i + 1 ))
        ;;
      # Names the option, never its argument. helm's --kube-token carries a
      # bearer token in the joined form, and these scripts run with their output
      # attached to the terminal and to CI logs, then get retried. kubectl
      # reports an unknown flag by name alone, so echoing the whole token here
      # would disclose what the unpatched path did not.
      #
      # The rejected options do have kubectl equivalents -- --kube-token is
      # --token, --kube-apiserver is --server -- so this is a scope boundary,
      # not an unknown mapping.
      *)
        echo "ERROR: KUBECONFIG_FLAG carries '${_aicr_tok%%=*}', which this" >&2
        echo "       contract does not support; it translates only" >&2
        echo "       --kube-context and --kubeconfig. It stops here rather than" >&2
        echo "       act on an unintended cluster. Export KUBE_CONTEXT instead." >&2
        exit 1
        ;;
    esac
  done

  if [[ -n "${_aicr_ctx}" ]]; then
    if [[ -n "${KUBE_CONTEXT:-}" && "${KUBE_CONTEXT}" != "${_aicr_ctx}" ]]; then
      echo "ERROR: KUBE_CONTEXT names '${KUBE_CONTEXT}' but KUBECONFIG_FLAG names" >&2
      echo "       '${_aicr_ctx}'. Refusing to guess which cluster to act on;" >&2
      echo "       set only KUBE_CONTEXT." >&2
      exit 1
    fi
    KUBE_CONTEXT="${_aicr_ctx}"
  fi

  # Children resolve from the normalized variables, so the deprecated spelling
  # stops here rather than being re-parsed (and re-warned) once per script.
  unset KUBECONFIG_FLAG
fi

if [[ -n "${KUBE_CONTEXT:-}" ]]; then
  export KUBE_CONTEXT
  HELM_CONN+=(--kube-context "${KUBE_CONTEXT}")
  KUBECTL_CONN+=(--context "${KUBE_CONTEXT}")
fi

# Helm 4 uses server-side apply by default; --force-conflicts lets the
# upgrade overwrite fields that operators (cert-manager, gpu-operator,
# nvsentinel, ...) own on rotated webhook cert Secrets. Helm 3 uses
# client-side apply (no field-manager conflicts) and does not recognize
# the flag, so omit it on Helm 3.
HELM_MAJOR=$(helm version --template '{{.Version}}' 2>/dev/null | sed -nE 's/^v([0-9]+)\..*/\1/p')
FORCE_CONFLICTS_FLAG=""
if [[ "${HELM_MAJOR:-0}" -ge 4 ]]; then
  FORCE_CONFLICTS_FLAG="--force-conflicts"
fi

# CHART carries the full OCI URI for OCI charts and just the chart name for
# HTTP/HTTPS charts. REPO is non-empty only for HTTP/HTTPS charts; the
# ${REPO:+--repo "${REPO}"} expansion adds --repo iff REPO is set.
# When apply-crds.sh ran, it pulled the chart and read the CRDs it applied out
# of that one file. Installing from the same file keeps both phases bound to a
# single artifact; resolving CHART/VERSION again would be a second fetch that a
# mutable tag does not promise returns the same bytes.
CHART_REF="${CHART}"
CHART_VERSION_ARGS=(--version "${VERSION}")
# Under --dry-run, apply-crds.sh above never ran, so no pull happened this
# invocation: any archive present is leftover from an earlier real deploy and
# is stale by definition, not merely unverified. A dry-run install must preview
# what the next real run will actually fetch, not bytes that run never touched.
if [[ -z "${DRY_RUN_FLAG:-}" && -f "${SCRIPT_DIR}/.aicr-chart.tgz" ]]; then
  CHART_REF="${SCRIPT_DIR}/.aicr-chart.tgz"
  CHART_VERSION_ARGS=()
  REPO=""
fi

helm upgrade --install ${FORCE_CONFLICTS_FLAG} 'gpu-operator' "${CHART_REF}" \
  ${REPO:+--repo "${REPO}"} "${CHART_VERSION_ARGS[@]}" \
  --namespace 'privileged-gpu-operator' --create-namespace \
  -f values.yaml -f cluster-values.yaml \
  ${COMPONENT_WAIT_ARGS:-} ${DRY_RUN_FLAG:-} ${HELM_CONN[@]+"${HELM_CONN[@]}"} ${HELM_DEBUG_FLAG:-}

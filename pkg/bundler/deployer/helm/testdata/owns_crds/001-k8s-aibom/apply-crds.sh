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

# Apply k8s-aibom's CRDs from its pinned chart, ahead of `helm upgrade`.
#
# Helm installs a chart's crds/ directory on first install and never touches it
# again, so a chart bump whose CRDs changed would otherwise leave the previous
# schema in place: the API server then silently prunes the new controller's
# writes to fields the old schema does not know.
#
# AICR emits this script only for components the registry marks ownsCRDs whose
# ref still points at the registry-pinned chart. That flag records an audit of
# one specific chart: that the component solely owns every CRD it ships, and
# ships none using spec.conversion.strategy: Webhook. It says nothing about a
# chart an overriding ref points at.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Bound once as literals. Recipe values are validated as path components
# (separators rejected), not as shell words, so they are never interpolated
# bare into a command or into a double-quoted string, where $(...) would
# re-expand. Every later use is a plain parameter expansion, which does not.
RELEASE='k8s-aibom'
NAMESPACE='k8s-aibom-system'
RELEASE_FILTER='^k8s-aibom$'

if ! command -v kubectl >/dev/null 2>&1; then
  echo "ERROR: kubectl is required to apply ${RELEASE} CRDs before upgrade." >&2
  exit 1
fi

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
  # shellcheck disable=SC2206
  _aicr_argv=(${KUBECONFIG_FLAG})
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

# Every helm and kubectl call below runs through run_bounded. This script runs
# inside the deploy path, where a command that never returns hangs the whole
# rollout rather than failing it: deploy.sh retries a component that exits
# non-zero but has no way to interrupt one that is still running. A wedged
# registry and a wedged apiserver both produce that, so the reads and the write
# are bounded alike.
#
# No unbounded fallback. Stock macOS ships no timeout(1), and running
# unbounded there would reintroduce exactly the hang this guards against on the
# one platform least likely to be exercised in CI. Failing closed with an
# actionable message is the safer trade: the operator can install coreutils, or
# apply the CRDs by hand with the command in the component catalog.
CRD_STEP_TIMEOUT="${AICR_CRD_STEP_TIMEOUT:-30}"
# Validate before it reaches timeout(1). GNU timeout treats 0 as "no timeout",
# so an override of 0 would silently disable the bound this script's
# fail-closed behavior depends on, and a non-numeric value would be rejected
# only at the first call. Whole seconds only: a suffixed duration would pass to
# timeout but not to the arithmetic comparison, so it is refused rather than
# half-honored.
if ! [[ "${CRD_STEP_TIMEOUT}" =~ ^[0-9]+$ ]] || (( CRD_STEP_TIMEOUT <= 0 )); then
  echo "ERROR: AICR_CRD_STEP_TIMEOUT must be a positive whole number of seconds;" >&2
  echo "       got '${CRD_STEP_TIMEOUT}'. A value of 0 disables timeout(1) entirely," >&2
  echo "       which would let a wedged helm or kubectl hang the deploy." >&2
  exit 1
fi
TIMEOUT_BIN=""
for candidate in timeout gtimeout; do
  if command -v "${candidate}" >/dev/null 2>&1; then
    TIMEOUT_BIN="${candidate}"
    break
  fi
done
if [[ -z "${TIMEOUT_BIN}" ]]; then
  echo "ERROR: neither timeout(1) nor gtimeout(1) is available, so the ${RELEASE} CRD" >&2
  echo "       step cannot be bounded and will not run unbounded inside a deploy." >&2
  echo "       Install GNU coreutils (macOS: brew install coreutils), or apply this" >&2
  echo "       chart's CRDs manually before upgrading; see the upgrade section of" >&2
  echo "       docs/user/component-catalog.md." >&2
  exit 1
fi

# -k: a process that ignores TERM still gets KILLed a few seconds later, so the
# bound holds against a wedged client rather than merely asking it to stop.
run_bounded() {
  "${TIMEOUT_BIN}" -k 5 "${CRD_STEP_TIMEOUT}" "$@" </dev/null
}

# Capture through a file, never `$(cmd)`.
#
# Command substitution blocks until the write end of the pipe closes, which is
# not the same thing as the command exiting: if the bound kills helm or kubectl
# but a grandchild (a credential helper, a retry worker) still holds stdout,
# `$(...)` waits on that grandchild and the bound buys nothing. A file has no
# such reader, so the step returns when the bounded process does.
BOUNDED_OUT="$(mktemp)"
CRD_DIR="$(mktemp -d)"
PULL_DIR=""
trap 'rm -f "${BOUNDED_OUT}"; rm -rf "${CRD_DIR}"; [[ -n "${PULL_DIR}" ]] && rm -rf "${PULL_DIR}"' EXIT
# Progress is announced before each bounded call and timed after it. deploy.sh
# captures this and prints it only when a component fails, so it costs nothing
# on a good run and names the slow call on a bad one. Without it a stalled step
# is indistinguishable from a stalled `helm upgrade` further down.
capture_bounded() {
  : >"${BOUNDED_OUT}"
  echo "${RELEASE}: crd-step: running $1 $2 (bound ${CRD_STEP_TIMEOUT}s)"
  local started=${SECONDS}
  local rc=0
  run_bounded "$@" >"${BOUNDED_OUT}" 2>&1 || rc=$?
  echo "${RELEASE}: crd-step: $1 $2 exited ${rc} after $((SECONDS - started))s"
  return ${rc}
}

# Read CRDs out of the chart archive rather than out of `helm show crds`.
#
# That command's output shape is version-dependent: Helm 4 prepends "---"
# before every CRD, Helm 3 prepends one only for `show all` and emits nothing
# between documents. Parsing it meant a separator-based filter that silently
# matched nothing on Helm 3, reported "chart ships no CRDs", and exited 0 with
# the schema left stranded, which is the defect this script exists to prevent
# appearing on the success path where nothing reports it. A chart's crds/
# directory is the same on both, one file per document, so the archive is the
# stable source.
#
# Recurses into packaged dependencies so a chart whose CRDs arrive through a
# subchart is covered, matching what `helm show crds` reported.
collect_crds() { # $1 = chart .tgz, $2 = destination directory
  # Every step is checked explicitly. This function is called from an `if !`
  # condition, which disables `set -e` inside it, so an unchecked cp or find
  # would be followed by a successful cleanup and a 0 return. Losing every file
  # that way reports "chart ships no CRDs" on a deploy that applied nothing;
  # losing some replaces an incomplete set and carries on. Both are the
  # fail-open shape this step exists to prevent.
  local work listing sub f
  work="$(mktemp -d)" || return 1
  if ! tar -xzf "$1" -C "${work}" 2>/dev/null; then
    rm -rf "${work}"
    return 1
  fi
  if ! listing="$(find "${work}" -type f -path '*/crds/*' \( -name '*.yaml' -o -name '*.yml' \))"; then
    rm -rf "${work}"
    return 1
  fi
  while IFS= read -r f; do
    [[ -z "${f}" ]] && continue
    if ! cp "${f}" "${2}/$(printf '%s' "${f#"${work}"/}" | tr '/' '_')"; then
      rm -rf "${work}"
      return 1
    fi
  done <<< "${listing}"
  # A dependency archive that cannot be read fails closed, exactly as the
  # top-level one does. Swallowing it would let a chart whose CRDs live in a
  # packaged subchart fall through to the emptiness guard and report "ships no
  # CRDs" on a deploy that applied nothing. A valid archive that happens not to
  # be a chart is not this case: it extracts fine and contributes no crds/.
  if ! listing="$(find "${work}" -type f -path '*/charts/*' -name '*.tgz')"; then
    rm -rf "${work}"
    return 1
  fi
  while IFS= read -r sub; do
    [[ -z "${sub}" ]] && continue
    if ! collect_crds "${sub}" "${2}"; then
      echo "ERROR: cannot read the packaged dependency ${sub##*/}; refusing to" >&2
      echo "       continue and report its CRDs as absent." >&2
      rm -rf "${work}"
      return 1
    fi
  done <<< "${listing}"
  rm -rf "${work}"
}

# Does a release already exist? An existing release means an upgrade, and helm
# never touches crds/ on upgrade, so the CRDs are this script's to apply.
#
# An absent release does NOT by itself mean a fresh cluster; see the retained-
# CRD check further down. Helm skips a CRD that already exists on install, and
# never deletes one on uninstall, so "no release" and "no CRDs" are different
# questions and only the second one licenses skipping.
#
# "Absent" and "cannot tell" are deliberately distinguished. An auth failure,
# an unreachable apiserver, or a broken helm must not read as a fresh install:
# the `helm upgrade` that follows can still succeed, and would then leave the
# previous CRDs in place. That is precisely the stranded-schema defect this
# script exists to prevent, so an indeterminate answer fails closed. `helm
# list` exits 0 whenever the query itself succeeded, whether or not it matched,
# which is what makes the two cases separable.
#
# The status flags are named rather than left to the default: Helm 4 lists every
# status by default but Helm 3 does not, and `--all` (which Helm 3 uses for
# that) was removed in Helm 4. These three exist in both and are exactly the
# set an upgrade would act on, so one spelling works against either binary.
if ! capture_bounded helm list --namespace "${NAMESPACE}" \
  --filter "${RELEASE_FILTER}" --short --deployed --failed --pending \
  ${HELM_CONN[@]+"${HELM_CONN[@]}"}; then
  existing="$(cat "${BOUNDED_OUT}")"
  echo "ERROR: cannot determine whether release ${RELEASE} exists; refusing to" >&2
  echo "       skip the CRD step and risk leaving the previous schema in place: ${existing}" >&2
  exit 1
fi
existing="$(cat "${BOUNDED_OUT}")"
RELEASE_EXISTS=true
if [[ -z "${existing//[[:space:]]/}" ]]; then
  RELEASE_EXISTS=false
fi


# shellcheck source=/dev/null
source ./upstream.env

# Pull once, then read the CRDs out of those exact bytes.
#
# Repository, chart, and version are coordinates, not content. Reading CRDs
# through them and letting the release resolve them again is two fetches, and a
# mutable tag does not promise both got the same artifact. Force-applying
# cluster-scoped CRDs from one artifact while the release runs a controller
# from another is precisely what the ownsCRDs audit is supposed to make
# impossible, so both phases are bound to one file here: install.sh installs
# PULLED_CHART when this script leaves it behind.
#
# Pull into a private directory, so the archive taken forward is the one this
# pull produced and nothing else.
#
# The component directory is not ours to reason about: generation does not
# clear it, and an interrupted pull or a regeneration at a different version
# can leave a tarball behind. Selecting "some .tgz in the folder" would let a
# stale artifact be renamed as the current result, and its CRDs would then be
# replaced while the release installed different bytes. An empty directory we
# just created has no such ambiguity, and exactly one archive is required so a
# surprise is an error rather than a coin flip.
PULLED_CHART="${SCRIPT_DIR}/.aicr-chart.tgz"
rm -f "${PULLED_CHART}"
PULL_DIR="$(mktemp -d)"
if ! capture_bounded helm pull "${CHART}" ${REPO:+--repo "${REPO}"} --version "${VERSION}" \
  --destination "${PULL_DIR}"; then
  echo "ERROR: cannot fetch the ${RELEASE} chart: $(cat "${BOUNDED_OUT}")" >&2
  exit 1
fi
pulled_count="$(find "${PULL_DIR}" -maxdepth 1 -type f -name '*.tgz' | wc -l | tr -d '[:space:]')"
if [[ "${pulled_count}" != "1" ]]; then
  echo "ERROR: expected exactly one chart archive from helm pull for ${RELEASE}," >&2
  echo "       found ${pulled_count}; refusing to guess which bytes to use." >&2
  exit 1
fi
mv -f "$(find "${PULL_DIR}" -maxdepth 1 -type f -name '*.tgz' -print -quit)" "${PULLED_CHART}"

if ! collect_crds "${PULLED_CHART}" "${CRD_DIR}"; then
  echo "ERROR: cannot read CRDs from the ${RELEASE} chart archive." >&2
  exit 1
fi

# An ownsCRDs component whose chart ships no CRDs is a no-op, not a failure:
# kubectl rejects an empty input, which would abort the deploy over nothing.
if ! find "${CRD_DIR}" -type f -name '*.y*ml' -print -quit | grep -q .; then
  echo "${RELEASE}: chart ships no CRDs; nothing to apply."
  exit 0
fi

# With no release, skip only once the cluster confirms none of this chart's
# CRDs are already present.
#
# Uninstall is why. Helm retains a chart's CRDs when the release is removed and
# skips any CRD that already exists on install, so uninstall followed by
# reinstall pairs a new controller with the retained old schema, and helm will
# not correct it. Treating "no release" as "fresh cluster" would skip exactly
# that case, which is the defect this script exists to prevent.
#
# `kubectl get -f -` asks about precisely the objects in the manifest, so this
# needs no name extraction; --ignore-not-found makes absence an empty result
# rather than an error. A failure here is indeterminate and fails closed, for
# the same reason the release lookup does.
if [[ "${RELEASE_EXISTS}" == "false" ]]; then
  if ! capture_bounded kubectl get -f "${CRD_DIR}" --ignore-not-found -o name \
    ${KUBECTL_CONN[@]+"${KUBECTL_CONN[@]}"}; then
    echo "ERROR: cannot determine whether ${RELEASE} CRDs are already present; refusing" >&2
    echo "       to skip and risk pairing a new controller with a retained schema: $(cat "${BOUNDED_OUT}")" >&2
    exit 1
  fi
  if ! grep -q '[^[:space:]]' "${BOUNDED_OUT}"; then
    echo "${RELEASE}: no release and no existing CRDs; helm install creates them."
    exit 0
  fi
  echo "${RELEASE}: no release but CRDs remain from a previous install; updating them."
fi

# Server-side apply under Helm's own field manager.
#
# Two things have to be true at once: the chart must be authoritative, so a
# field or spec.versions entry it removes actually disappears, and the call has
# to work on a CRD that already exists.
#
# `kubectl replace` cannot do the second. Kubernetes rejects an update whose
# object carries no metadata.resourceVersion, and kubectl forwards the manifest
# as given, so replacing a chart's raw CRD fails on precisely the upgrade this
# step exists for. Verified against a live cluster: the API returns a Conflict.
#
# Plain `kubectl apply --server-side` cannot do the first. Server-side apply
# removes an omitted field only when the applying manager owns it, and these
# CRDs are owned by Helm, so a removal would survive under the default
# `kubectl` manager.
#
# Applying as `helm` satisfies both. The apply adopts Helm's fieldset, so
# anything Helm owned and the chart no longer declares is pruned, and apply
# creates the object when it is absent. Verified on a live cluster against both
# a Helm 4 install (field manager `helm`, operation Apply) and a Helm 3 install
# (same manager, operation Update): a removed schema property disappeared in
# both cases. --force-conflicts is still required because other managers may
# hold individual fields.
while IFS= read -r doc; do
  # A whitespace-only file under crds/ is not an error, but handing it to
  # kubectl is: "no objects passed" aborts the deploy, and because the loop is
  # sorted it can do so before the real CRDs are ever reached.
  grep -q '[^[:space:]]' "${doc}" || continue
  if ! capture_bounded kubectl apply --server-side --force-conflicts \
    --field-manager=helm -f "${doc}" ${KUBECTL_CONN[@]+"${KUBECTL_CONN[@]}"}; then
    echo "ERROR: could not apply a ${RELEASE} CRD: $(cat "${BOUNDED_OUT}")" >&2
    exit 1
  fi
done < <(find "${CRD_DIR}" -type f -name '*.y*ml' | sort)

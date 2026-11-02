# shellcheck shell=bash
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

# Shared steps of the kind suites (tools/operator-skeleton,
# tools/operator-qualify), sourced after `cd` to the repository root with
# KUBECONFIG set; OPERATOR_KIND_CLUSTER names the kind cluster.

setting() { yq -r "$1" .settings.yaml; }

# preload_images makes images available on the nodes without the kubelet
# pulling them: Argo CD's redis comes from ECR Public, which throttles
# anonymous pulls. Each image is pulled on the host with retries, kept in
# OPERATOR_KIND_IMAGE_CACHE when set (a CI cache), and loaded into kind. As for
# the registry image (tools/operator-kind), loading is best effort.
preload_images() {
  local image file attempt
  for image in "$@"; do
    file=""
    if [[ -n "${OPERATOR_KIND_IMAGE_CACHE:-}" ]]; then
      file="${OPERATOR_KIND_IMAGE_CACHE}/$(tr '/:@' '___' <<<"${image}").tar"
      if [[ -f "${file}" ]]; then
        docker load --quiet --input "${file}" >/dev/null 2>&1 || true
      fi
    fi
    if ! docker image inspect "${image}" >/dev/null 2>&1; then
      for attempt in 1 2 3 4 5; do
        docker pull --quiet "${image}" >/dev/null && break
        if [[ "${attempt}" == 5 ]]; then
          echo "cannot pull ${image}" >&2
          exit 1
        fi
        sleep $((attempt * 10))
      done
    fi
    if [[ -n "${file}" && ! -f "${file}" ]]; then
      mkdir -p "${OPERATOR_KIND_IMAGE_CACHE}"
      docker save --output "${file}" "${image}"
    fi
    kind load docker-image "${image}" --name "${OPERATOR_KIND_CLUSTER:-aicr-operator}" >/dev/null 2>&1 ||
      echo "warning: could not preload ${image}; the kubelet pulls it" >&2
  done
}

# install_argocd installs the pinned Argo CD chart with its images preloaded.
install_argocd() {
  local argocd_chart
  local -a argocd=(argo-cd --repo https://argoproj.github.io/argo-helm --namespace argocd
    --set dex.enabled=false --set notifications.enabled=false --set applicationSet.replicas=0
    --set 'configs.params.server\.insecure=true')
  argocd_chart="$(setting '.testing_tools.argocd_chart')"
  local -a images
  mapfile -t images < <(helm template argocd "${argocd[@]}" --version "${argocd_chart}" |
    sed -n 's/^ *image: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' | sort -u)
  preload_images "${images[@]}"
  helm upgrade --install argocd "${argocd[@]}" --version "${argocd_chart}" \
    --create-namespace --wait --timeout 10m
}

# operator_namespace creates the operator namespace as the administrator does,
# with the restricted profile (charts/aicr-operator/README.md). An
# aicr-operator Deployment that Helm does not own may have another, immutable
# selector: it is replaced once.
operator_namespace() {
  kubectl create namespace aicr-system --dry-run=client -o yaml | kubectl apply --server-side -f - >/dev/null
  kubectl label --overwrite namespace aicr-system pod-security.kubernetes.io/enforce=restricted >/dev/null
  if [[ -z "$(kubectl -n aicr-system get deployment aicr-operator -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null)" ]]; then
    kubectl -n aicr-system delete deployment aicr-operator --ignore-not-found --wait >/dev/null
  fi
}

# release_flags sets HELM_RELEASE_FLAGS to what an administrator adds to
# `helm upgrade --install` of the chart running AICR release $1, after the
# steps docs/operator/lifecycle.md and troubleshooting.md give for AICR
# v1.0.x, whose runs relabel the discovery and validation namespaces: the
# label goes back to Helm, and an upgrade takes --force-conflicts.
# OPERATOR_ADOPT=1 also adopts what another suite installed on a reused local
# cluster; CI gives each suite its own cluster and never sets it.
release_flags() {
  local ns
  HELM_RELEASE_FLAGS=()
  if [[ "$1" == v1.0.* ]]; then
    for ns in aicr-discovery aicr-validation; do
      if kubectl get namespace "${ns}" >/dev/null 2>&1; then
        kubectl label namespace "${ns}" app.kubernetes.io/managed-by=Helm --overwrite >/dev/null
      fi
    done
    HELM_RELEASE_FLAGS+=(--force-conflicts)
  fi
  if [[ "${OPERATOR_ADOPT:-}" == 1 ]]; then
    HELM_RELEASE_FLAGS+=(--take-ownership --force-conflicts)
  fi
}

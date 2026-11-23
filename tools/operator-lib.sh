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
# tools/operator-qualify, tools/operator-e2e-commit, tools/operator-main-aicr),
# sourced after `cd` to the repository root with KUBECONFIG set;
# OPERATOR_KIND_CLUSTER names the kind cluster.

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

# import_aicr builds the aicr CLI image with ko from commit $1, or from the
# checkout when $1 is empty, using scratch directory $3, which the caller
# removes. It imports the image by digest into every node of the kind
# cluster, as a registry pull would (load_image in tools/operator-qualify):
# the operator runs AICR only pinned by digest. The image is stamped with
# version $2, the commit and the commit date, runs as the numeric user
# goreleaser pins for releases (the base's own user is a name, which the
# operator's runAsNonRoot Jobs refuse), and is named as a release would be,
# ghcr.io/nvidia/aicr:$2@<digest>, which AICR_IMPORTED_IMAGE is set to. It is
# never pushed.
import_aicr() {
  local commit="$1" version="$2" scratch="$3" repository=ghcr.io/nvidia/aicr src commit_date arch digest node
  local -a nodes=()
  while IFS= read -r node; do
    nodes+=("${node}")
  done < <(kind get nodes --name "${OPERATOR_KIND_CLUSTER:-aicr-operator}")
  if [[ ${#nodes[@]} -eq 0 ]]; then
    echo "kind cluster ${OPERATOR_KIND_CLUSTER:-aicr-operator} has no nodes; run make operator-kind-up" >&2
    return 1
  fi
  src="${PWD}"
  if [[ -n "${commit}" ]]; then
    src="$(mktemp -d "${scratch}/aicr-src.XXXXXX")"
    git archive "${commit}" | tar -x -C "${src}"
  fi
  commit="$(git rev-parse "${commit:-HEAD}^{commit}")"
  commit_date="$(TZ=UTC git show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd "${commit}")"
  arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')"
  echo '{}' >"${scratch}/config.json"
  # ko names an OCI layout by its path, which must parse as a lowercase image
  # reference: a relative path in the gitignored .operator-e2e, not one under
  # a mktemp directory. An empty docker config keeps stale local registry
  # credentials away from the anonymous base-image pull.
  rm -rf "${src}/.operator-e2e/aicr-image"
  (
    cd "${src}" || exit
    GOWORK=off DOCKER_CONFIG="${scratch}" KO_DOCKER_REPO="${repository}" \
      go run "github.com/google/ko@$(setting '.build_tools.ko')" build --bare --sbom=none --push=false \
      --ldflags=-s --ldflags=-w \
      --ldflags="-X=aicr.run/pkg/cli.version=${version}" \
      --ldflags="-X=aicr.run/pkg/cli.commit=${commit}" \
      --ldflags="-X=aicr.run/pkg/cli.date=${commit_date}" \
      --image-user="$(yq -r '.kos[] | select(.id == "aicr") | .user' .goreleaser.yaml)" \
      --platform="linux/${arch}" --oci-layout-path .operator-e2e/aicr-image ./cmd/aicr >/dev/null
  )
  digest="$(yq -r '.manifests[0].digest' "${src}/.operator-e2e/aicr-image/index.json")"
  yq -i -o=json ".manifests[0].annotations.\"org.opencontainers.image.ref.name\" = \"${repository}:${version}\"" \
    "${src}/.operator-e2e/aicr-image/index.json"
  tar -C "${src}/.operator-e2e/aicr-image" -cf "${scratch}/aicr-image.tar" .
  rm -rf "${src}/.operator-e2e/aicr-image"
  for node in "${nodes[@]}"; do
    docker exec -i "${node}" ctr -n k8s.io images import --digests --snapshotter=overlayfs - <"${scratch}/aicr-image.tar" >/dev/null
    docker exec "${node}" ctr -n k8s.io images tag --force "${repository}:${version}" "${repository}@${digest}" >/dev/null
  done
  AICR_IMPORTED_IMAGE="${repository}:${version}@${digest}"
  echo "AICR ${version} built from ${commit:0:9}: ${AICR_IMPORTED_IMAGE}"
}

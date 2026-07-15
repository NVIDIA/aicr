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

readonly EXPECTED_KEYS_JSON='["aicr","aicrd","aiperf-bench","conformance","deployment","gate","performance"]'
readonly RELEASE_TAG_PATTERN='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$'
readonly PINNED_V0170_AICR='sha256:6b2be0c1c2ebbbe4acc77445f8b6b32b7042b0352252608eef3447e5fe162570'
readonly PINNED_V0170_AICRD='sha256:1962f8340e8b2f228059b2e858e751ac76e08d33ca0fa064cc418ce4aa57b4fc'
readonly IMAGE_PAIRS=(
  'aicr=ghcr.io/nvidia/aicr'
  'aicrd=ghcr.io/nvidia/aicrd'
  'aiperf-bench=ghcr.io/nvidia/aicr-validators/aiperf-bench'
  'conformance=ghcr.io/nvidia/aicr-validators/conformance'
  'deployment=ghcr.io/nvidia/aicr-validators/deployment'
  'gate=ghcr.io/nvidia/aicr-gate'
  'performance=ghcr.io/nvidia/aicr-validators/performance'
)

AICR_NETWORK_TIMEOUT_SECONDS="${AICR_NETWORK_TIMEOUT_SECONDS:-120}"
if [[ ! "${AICR_NETWORK_TIMEOUT_SECONDS}" =~ ^[0-9]+$ ]] ||
  ((AICR_NETWORK_TIMEOUT_SECONDS < 1 || AICR_NETWORK_TIMEOUT_SECONDS > 600)); then
  echo "::error::AICR_NETWORK_TIMEOUT_SECONDS must be an integer from 1 through 600" >&2
  exit 1
fi
readonly AICR_NETWORK_TIMEOUT_SECONDS

cleanup_files=()
cleanup() {
  local path
  for path in "${cleanup_files[@]}"; do
    rm -f -- "${path}"
  done
}
trap cleanup EXIT

die() {
  echo "::error::$*" >&2
  exit 1
}

run_network() {
  timeout --foreground "${AICR_NETWORK_TIMEOUT_SECONDS}s" "$@"
}

require_single_line() {
  local name="$1" value="$2"
  if [[ -z "${value}" || "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    die "${name} must be a non-empty single-line value"
  fi
}

validate_candidate_tag() {
  require_single_line CANDIDATE_TAG "$1"
  [[ "$1" =~ ^candidate-[0-9]+-[0-9]+$ ]] || die "invalid candidate tag"
}

validate_release_tag() {
  require_single_line RELEASE_TAG "$1"
  [[ "$1" =~ ${RELEASE_TAG_PATTERN} ]] ||
    die "invalid release tag"
}

validate_stable_tag() {
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
    die "stable release requires a vMAJOR.MINOR.PATCH tag"
}

validate_release_kind() {
  local release_tag="$1" is_prerelease="$2" expected=false
  if [[ "${release_tag}" == *-* ]]; then expected=true; fi
  [[ "${is_prerelease}" == "${expected}" ]] ||
    die "IS_PRERELEASE does not match RELEASE_TAG"
}

release_prerelease_json() {
  if [[ "${RELEASE_TAG}" == *-* ]]; then
    printf 'true\n'
  else
    printf 'false\n'
  fi
}

expected_release_asset_names() {
  local version="${RELEASE_TAG#v}"
  jq -cn --arg version "${version}" '[
    "aicr_\($version)_darwin_amd64.sbom.json",
    "aicr_\($version)_darwin_amd64.tar.gz",
    "aicr_\($version)_darwin_arm64.sbom.json",
    "aicr_\($version)_darwin_arm64.tar.gz",
    "aicr_\($version)_linux_amd64.sbom.json",
    "aicr_\($version)_linux_amd64.tar.gz",
    "aicr_\($version)_linux_arm64.sbom.json",
    "aicr_\($version)_linux_arm64.tar.gz",
    "aicrd_\($version)_linux_amd64.sbom.json",
    "aicrd_\($version)_linux_arm64.sbom.json",
    "THIRD_PARTY_NOTICES.md",
    "aicr_checksums.txt",
    "recipe-catalog.sigstore.json"
  ] | sort'
}

validate_revision() {
  require_single_line GITHUB_SHA "$1"
  [[ "$1" =~ ^[0-9a-f]{40}$ ]] || die "GITHUB_SHA must be a full lowercase commit SHA"
}

validate_digest() {
  [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || die "malformed image digest"
}

validate_common_environment() {
  CANDIDATE_TAG="${CANDIDATE_TAG:-}"
  RELEASE_TAG="${RELEASE_TAG:-}"
  GITHUB_SHA="${GITHUB_SHA:-}"
  validate_candidate_tag "${CANDIDATE_TAG}"
  validate_release_tag "${RELEASE_TAG}"
  validate_revision "${GITHUB_SHA}"
  readonly CANDIDATE_TAG RELEASE_TAG GITHUB_SHA
}

require_regular_file() {
  local path="$1"
  [[ -f "${path}" && ! -L "${path}" ]] || die "required input is not a regular file: ${path}"
}

validate_digest_map() {
  local path="$1" streamed_keys
  require_regular_file "${path}"
  streamed_keys="$(
    jq -c --stream \
      'select(length == 2 and (.[0] | length) == 1) | .[0][0]' "${path}" |
      jq -cs 'sort'
  )" || die "digest map is not valid JSON"
  [[ "${streamed_keys}" == "${EXPECTED_KEYS_JSON}" ]] ||
    die "digest map must not contain duplicate or unexpected keys"
  jq -e --argjson expected "${EXPECTED_KEYS_JSON}" '
    type == "object" and
    keys == $expected and
    all(.[]; type == "string" and test("^sha256:[0-9a-f]{64}$"))
  ' "${path}" >/dev/null || die "digest map must contain exactly the seven fixed image digests"
}

atomic_json_write() {
  local destination="$1" content="$2" directory temporary
  directory="$(dirname -- "${destination}")"
  [[ -d "${directory}" ]] || die "output directory does not exist: ${directory}"
  umask 077
  temporary="$(mktemp "${destination}.tmp.XXXXXX")"
  cleanup_files+=("${temporary}")
  printf '%s\n' "${content}" >"${temporary}"
  jq -e . "${temporary}" >/dev/null || die "refusing to write invalid JSON"
  mv -f -- "${temporary}" "${destination}"
}

validate_platforms_and_labels() {
  local image="$1" digest="$2" expected_version="$3" expected_revision="$4"
  local reference manifest platform config
  reference="${image}@${digest}"
  manifest="$(run_network crane manifest "${reference}")" || die "failed to inspect ${reference}"
  jq -e '
    (.mediaType == "application/vnd.oci.image.index.v1+json" or
      .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json") and
    (.manifests | type == "array") and
    ([.manifests[] |
      select((.platform.os | type) == "string" and (.platform.architecture | type) == "string") |
      "\(.platform.os)/\(.platform.architecture)"] | sort) == ["linux/amd64", "linux/arm64"] and
    (.manifests | length) == 2
  ' <<<"${manifest}" >/dev/null ||
    die "${reference} must be an exact linux/amd64 and linux/arm64 image index"

  for platform in linux/amd64 linux/arm64; do
    config="$(run_network crane config "${reference}" --platform "${platform}")" ||
      die "failed to inspect ${reference} for ${platform}"
    jq -e --arg version "${expected_version}" --arg revision "${expected_revision}" '
      (.config.Labels["org.opencontainers.image.version"] // "") == $version and
      (.config.Labels["org.opencontainers.image.revision"] // "") == $revision
    ' <<<"${config}" >/dev/null ||
      die "${reference} ${platform} has incorrect release labels"
  done
}

resolve_candidate() {
  local image="$1" expected_digest="${2:-}" digest
  digest="$(run_network crane digest "${image}:${CANDIDATE_TAG}")" ||
    die "failed to resolve ${image}:${CANDIDATE_TAG}"
  validate_digest "${digest}"
  if [[ -n "${expected_digest}" && "${digest}" != "${expected_digest}" ]]; then
    die "candidate digest changed for ${image}"
  fi
  validate_platforms_and_labels "${image}" "${digest}" "${RELEASE_TAG}" "${GITHUB_SHA}"
  printf '%s\n' "${digest}"
}

list_tags() {
  local image="$1" tags duplicate tag
  tags="$(run_network crane ls "${image}")" || die "failed to enumerate tags for ${image}"
  if [[ -n "${tags}" ]]; then
    while IFS= read -r tag; do
      [[ "${tag}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] ||
        die "registry returned an invalid tag for ${image}"
    done <<<"${tags}"
    duplicate="$(sort <<<"${tags}" | uniq -d | head -n 1)"
    [[ -z "${duplicate}" ]] || die "registry returned duplicate tag ${duplicate} for ${image}"
  fi
  printf '%s\n' "${tags}"
}

tag_exists() {
  local tags="$1" wanted="$2"
  grep -Fqx -- "${wanted}" <<<"${tags}"
}

resolve_tag_commit() {
  local tag="$1" ref_json object_type object_sha depth
  require_single_line tag "${tag}"
  ref_json="$(run_network gh api -H 'Accept: application/vnd.github+json' \
    "repos/NVIDIA/aicr/git/ref/tags/${tag}")" || die "failed to resolve release tag ${tag}"
  object_type="$(jq -er '.object.type' <<<"${ref_json}")" || die "release tag response has no object type"
  object_sha="$(jq -er '.object.sha' <<<"${ref_json}")" || die "release tag response has no object SHA"
  depth=0
  while [[ "${object_type}" == "tag" ]]; do
    ((depth += 1))
    ((depth <= 5)) || die "release tag nesting is too deep"
    ref_json="$(run_network gh api -H 'Accept: application/vnd.github+json' \
      "repos/NVIDIA/aicr/git/tags/${object_sha}")" || die "failed to peel release tag ${tag}"
    object_type="$(jq -er '.object.type' <<<"${ref_json}")" || die "annotated tag has no object type"
    object_sha="$(jq -er '.object.sha' <<<"${ref_json}")" || die "annotated tag has no object SHA"
  done
  [[ "${object_type}" == "commit" && "${object_sha}" =~ ^[0-9a-f]{40}$ ]] ||
    die "release tag ${tag} does not resolve to a commit"
  printf '%s\n' "${object_sha}"
}

verify_current_release_source() {
  local resolved
  resolved="$(resolve_tag_commit "${RELEASE_TAG}")"
  [[ "${resolved}" == "${GITHUB_SHA}" ]] || die "release tag ${RELEASE_TAG} moved from ${GITHUB_SHA}"
}

release_collisions() {
  local releases
  releases="$(run_network gh api --paginate --slurp \
    -H 'Accept: application/vnd.github+json' \
    'repos/NVIDIA/aicr/releases?per_page=100')" || die "failed to list GitHub releases"
  jq -ce --arg release_tag "${RELEASE_TAG}" '
    def normalized:
      if type == "object" and
        ((.id | type) == "number") and .id > 0 and .id == (.id | floor) and
        ((.tag_name | type) == "string") and
        (((.name | type) == "string") or .name == null) and
        ((.draft | type) == "boolean") and
        ((.prerelease | type) == "boolean")
      then {id, tag_name, name, draft, prerelease}
      else error("malformed release")
      end;
    if type != "array" then error("malformed page list")
    else [
      .[] |
      if type == "array" then .[] else error("malformed release page") end |
      normalized |
      select(.tag_name == $release_tag or .name == $release_tag)
    ]
    end
  ' <<<"${releases}" || die "malformed GitHub release state"
}

require_exact_release() {
  local collisions="$1" expected_prerelease="$2" count release
  count="$(jq -er 'length' <<<"${collisions}")" || die "failed to count matching GitHub releases"
  case "${count}" in
    0) die "release ${RELEASE_TAG} does not exist" ;;
    1) ;;
    *) die "multiple releases collide with ${RELEASE_TAG}" ;;
  esac
  release="$(jq -ce '.[0]' <<<"${collisions}")" || die "failed to select GitHub release"
  jq -e --arg release_tag "${RELEASE_TAG}" \
    '.tag_name == $release_tag and .name == $release_tag' <<<"${release}" >/dev/null ||
    die "release name and tag must both equal ${RELEASE_TAG}"
  jq -e --argjson expected "${expected_prerelease}" \
    '.prerelease == $expected' <<<"${release}" >/dev/null ||
    die "release ${RELEASE_TAG} prerelease state does not match its tag"
  printf '%s\n' "${release}"
}

require_exact_draft_release() {
  local collisions="$1" expected_prerelease="$2" release
  release="$(require_exact_release "${collisions}" "${expected_prerelease}")"
  jq -e '.draft == true' <<<"${release}" >/dev/null ||
    die "release ${RELEASE_TAG} is already public"
  printf '%s\n' "${release}"
}

validate_release_assets() {
  local release_id="$1" mode="$2" expected pages
  [[ "${release_id}" =~ ^[1-9][0-9]*$ ]] || die "release ID is malformed"
  [[ "${mode}" == "subset" || "${mode}" == "exact" ]] || die "release asset validation mode is invalid"
  expected="$(expected_release_asset_names)" || die "failed to derive expected release assets"
  pages="$(run_network gh api --paginate --slurp \
    -H 'Accept: application/vnd.github+json' \
    "repos/NVIDIA/aicr/releases/${release_id}/assets?per_page=100")" ||
    die "failed to list release assets"
  jq -e --argjson expected "${expected}" --arg mode "${mode}" '
    def normalized:
      if type == "object" and
        ((.id | type) == "number") and .id > 0 and .id == (.id | floor) and
        ((.name | type) == "string") and (.name | length) > 0 and
        (.name | test("[\\r\\n]") | not) and
        .state == "uploaded"
      then {id, name}
      else error("malformed release asset")
      end;
    (if type != "array" then error("malformed asset page list")
     else [
       .[] |
       if type == "array" then .[] else error("malformed asset page") end |
       normalized
     ]
     end) as $assets |
    ($assets | map(.id)) as $ids |
    ($assets | map(.name) | sort) as $names |
    if ($ids | length) != ($ids | unique | length) then error("duplicate asset ID")
    elif ($names | length) != ($names | unique | length) then error("duplicate asset name")
    elif $mode == "subset" and (($names - $expected) | length) != 0 then error("unexpected asset")
    elif $mode == "exact" and $names != $expected then error("incomplete asset set")
    else true
    end
  ' <<<"${pages}" >/dev/null ||
    die "release assets are malformed or violate the expected ${mode} set"
}

release_target_command() {
  local collisions count expected_prerelease release release_id
  RELEASE_TAG="${RELEASE_TAG:-}"
  GITHUB_SHA="${GITHUB_SHA:-}"
  validate_release_tag "${RELEASE_TAG}"
  validate_revision "${GITHUB_SHA}"
  readonly RELEASE_TAG GITHUB_SHA
  require_single_line GH_TOKEN "${GH_TOKEN:-}"
  verify_current_release_source

  collisions="$(release_collisions)"
  count="$(jq -er 'length' <<<"${collisions}")" || die "failed to count matching GitHub releases"
  if [[ "${count}" == "0" ]]; then
    echo "Release target ${RELEASE_TAG}: no existing release"
    return
  fi
  expected_prerelease="$(release_prerelease_json)"
  release="$(require_exact_draft_release "${collisions}" "${expected_prerelease}")"
  release_id="$(jq -er '.id | tostring' <<<"${release}")" || die "release has no valid ID"
  validate_release_assets "${release_id}" subset
  echo "Release target ${RELEASE_TAG}: reusing existing draft ${release_id}"
}

resolve_prior_release() {
  local releases prior current object_sha
  releases="$(run_network gh api --paginate --slurp \
    -H 'Accept: application/vnd.github+json' \
    'repos/NVIDIA/aicr/releases?per_page=100')" || die "failed to list public GitHub releases"
  current="${RELEASE_TAG}"
  prior="$(jq -er --arg current "${current}" '
    def version:
      if test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")
      then
        capture("^v(?<major>0|[1-9][0-9]*)\\.(?<minor>0|[1-9][0-9]*)\\.(?<patch>0|[1-9][0-9]*)$") |
        [(.major | tonumber), (.minor | tonumber), (.patch | tonumber)]
      else error("non-SemVer public stable release")
      end;
    def normalized:
      if type == "object" and
        ((.tag_name | type) == "string") and
        ((.draft | type) == "boolean") and
        ((.prerelease | type) == "boolean")
      then {tag_name, draft, prerelease}
      else error("malformed release")
      end;
    ($current | version) as $current_version |
    (if type != "array" then error("malformed page list")
     else [
       .[] |
       if type == "array" then .[] else error("malformed release page") end |
       normalized
     ]
     end) as $releases |
    [ $releases[] |
      select(.draft == false and .prerelease == false) |
      .tag_name as $tag |
      ($tag | version) as $parsed |
      {tag: $tag, parsed: $parsed}
    ] as $stable_releases |
    if any($stable_releases[]; .parsed >= $current_version)
    then error("current or newer stable release is already public")
    else
      [$stable_releases[] | select(.parsed < $current_version)] |
      if length == 0 then error("no prior stable release") else max_by(.parsed).tag end
    end
  ' <<<"${releases}")" || die "failed to identify the immediately prior stable release"

  object_sha="$(resolve_tag_commit "${prior}")"
  printf '%s\t%s\n' "${prior}" "${object_sha}"
}

validate_prior_digest() {
  local key="$1" image="$2" digest="$3" prior_tag="$4" prior_revision="$5" pinned
  validate_digest "${digest}"
  if [[ "${prior_tag}" == "v0.17.0" && ("${key}" == "aicr" || "${key}" == "aicrd") ]]; then
    if [[ "${key}" == "aicr" ]]; then pinned="${PINNED_V0170_AICR}"; else pinned="${PINNED_V0170_AICRD}"; fi
    [[ "${digest}" == "${pinned}" ]] || die "unlabeled ${key} v0.17.0 digest does not match the pinned bootstrap"
    return
  fi
  validate_platforms_and_labels "${image}" "${digest}" "${prior_tag}" "${prior_revision}"
}

resolve_command() {
  local output="$1" pair key image digest digest_map
  [[ -n "${output}" ]] || die "resolve requires an output path"
  validate_common_environment
  digest_map='{}'
  for pair in "${IMAGE_PAIRS[@]}"; do
    key="${pair%%=*}"
    image="${pair#*=}"
    digest="$(resolve_candidate "${image}")"
    digest_map="$(jq -cS --arg key "${key}" --arg digest "${digest}" \
      '. + {($key): $digest}' <<<"${digest_map}")"
  done
  jq -e --argjson expected "${EXPECTED_KEYS_JSON}" 'keys == $expected' <<<"${digest_map}" >/dev/null ||
    die "resolver produced an incomplete digest map"
  atomic_json_write "${output}" "${digest_map}"
}

preflight_command() {
  local digest_map_path="$1" output="$2" pair key image expected_digest actual_digest tags
  local version_state latest_state prior_digest latest_digest prior_tag='' prior_revision=''
  local prior_record prior_map='{}' images_json='{}' validated is_prerelease_json
  [[ -n "${digest_map_path}" && -n "${output}" ]] || die "preflight requires input and output paths"
  validate_common_environment
  IS_PRERELEASE="${IS_PRERELEASE:-}"
  [[ "${IS_PRERELEASE}" == "true" || "${IS_PRERELEASE}" == "false" ]] ||
    die "IS_PRERELEASE must be true or false"
  validate_release_kind "${RELEASE_TAG}" "${IS_PRERELEASE}"
  require_single_line GH_TOKEN "${GH_TOKEN:-}"
  validate_digest_map "${digest_map_path}"
  verify_current_release_source
  if [[ "${IS_PRERELEASE}" == "false" ]]; then
    validate_stable_tag "${RELEASE_TAG}"
    prior_record="$(resolve_prior_release)"
    IFS=$'\t' read -r prior_tag prior_revision <<<"${prior_record}"
    require_single_line prior_tag "${prior_tag}"
    validate_revision "${prior_revision}"
    for pair in "${IMAGE_PAIRS[@]}"; do
      key="${pair%%=*}"
      image="${pair#*=}"
      tags="$(list_tags "${image}")"
      tag_exists "${tags}" "${prior_tag}" || die "${image} is missing prior release alias ${prior_tag}"
      prior_digest="$(run_network crane digest "${image}:${prior_tag}")" ||
        die "failed to resolve ${image}:${prior_tag}"
      validate_prior_digest "${key}" "${image}" "${prior_digest}" "${prior_tag}" "${prior_revision}"
      prior_map="$(jq -cS --arg key "${key}" --arg digest "${prior_digest}" \
        '. + {($key): $digest}' <<<"${prior_map}")"
    done
  fi

  for pair in "${IMAGE_PAIRS[@]}"; do
    key="${pair%%=*}"
    image="${pair#*=}"
    expected_digest="$(jq -er --arg key "${key}" '.[$key]' "${digest_map_path}")" ||
      die "digest map is missing ${key}"
    actual_digest="$(resolve_candidate "${image}" "${expected_digest}")"
    [[ "${actual_digest}" == "${expected_digest}" ]] || die "candidate digest mismatch for ${image}"
    run_network gh attestation verify "oci://${image}@${expected_digest}" \
      --repo NVIDIA/aicr \
      --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml \
      --source-ref "refs/tags/${RELEASE_TAG}" \
      --source-digest "${GITHUB_SHA}" >/dev/null || die "attestation verification failed for ${image}@${expected_digest}"

    tags="$(list_tags "${image}")"
    version_state='promote'
    if tag_exists "${tags}" "${RELEASE_TAG}"; then
      actual_digest="$(run_network crane digest "${image}:${RELEASE_TAG}")" ||
        die "failed to resolve existing ${image}:${RELEASE_TAG}"
      validate_digest "${actual_digest}"
      [[ "${actual_digest}" == "${expected_digest}" ]] || die "conflicting version alias for ${image}:${RELEASE_TAG}"
      version_state='already_candidate'
    fi

    latest_state='not_applicable'
    prior_digest=''
    if [[ "${IS_PRERELEASE}" == "false" ]]; then
      prior_digest="$(jq -er --arg key "${key}" '.[$key]' <<<"${prior_map}")" ||
        die "prior digest map is missing ${key}"
      tag_exists "${tags}" latest || die "${image} is missing latest"
      latest_digest="$(run_network crane digest "${image}:latest")" || die "failed to resolve ${image}:latest"
      validate_digest "${latest_digest}"
      if [[ "${latest_digest}" == "${expected_digest}" ]]; then
        latest_state='already_candidate'
      elif [[ "${latest_digest}" == "${prior_digest}" ]]; then
        latest_state='promote'
      else
        die "${image}:latest is neither the immediate prior release nor this candidate"
      fi
    fi

    images_json="$(jq -cS \
      --arg key "${key}" --arg image "${image}" --arg digest "${expected_digest}" \
      --arg prior_digest "${prior_digest}" --arg version_state "${version_state}" \
      --arg latest_state "${latest_state}" \
      '. + {($key): {image: $image, digest: $digest, prior_digest: $prior_digest,
        version_state: $version_state, latest_state: $latest_state}}' <<<"${images_json}")"
  done

  if [[ "${IS_PRERELEASE}" == "true" ]]; then is_prerelease_json=true; else is_prerelease_json=false; fi
  validated="$(jq -cnS \
    --arg candidate_tag "${CANDIDATE_TAG}" --arg release_tag "${RELEASE_TAG}" \
    --arg revision "${GITHUB_SHA}" --arg prior_tag "${prior_tag}" \
    --argjson is_prerelease "${is_prerelease_json}" --argjson images "${images_json}" \
    '{candidate_tag: $candidate_tag, release_tag: $release_tag, revision: $revision,
      is_prerelease: $is_prerelease, prior_tag: $prior_tag, images: $images}')"
  atomic_json_write "${output}" "${validated}"
}

verify_source_command() {
  RELEASE_TAG="${RELEASE_TAG:-}"
  GITHUB_SHA="${GITHUB_SHA:-}"
  validate_release_tag "${RELEASE_TAG}"
  validate_revision "${GITHUB_SHA}"
  readonly RELEASE_TAG GITHUB_SHA
  require_single_line GH_TOKEN "${GH_TOKEN:-}"
  verify_current_release_source
}

publish_release_command() {
  local collisions expected_prerelease release release_id payload response
  RELEASE_TAG="${RELEASE_TAG:-}"
  GITHUB_SHA="${GITHUB_SHA:-}"
  IS_PRERELEASE="${IS_PRERELEASE:-}"
  validate_release_tag "${RELEASE_TAG}"
  validate_revision "${GITHUB_SHA}"
  [[ "${IS_PRERELEASE}" == "true" || "${IS_PRERELEASE}" == "false" ]] ||
    die "IS_PRERELEASE must be true or false"
  validate_release_kind "${RELEASE_TAG}" "${IS_PRERELEASE}"
  readonly RELEASE_TAG GITHUB_SHA IS_PRERELEASE
  require_single_line GH_TOKEN "${GH_TOKEN:-}"
  verify_current_release_source

  expected_prerelease="$(release_prerelease_json)"
  collisions="$(release_collisions)"
  release="$(require_exact_release "${collisions}" "${expected_prerelease}")"
  release_id="$(jq -er '.id | tostring' <<<"${release}")" || die "release has no valid ID"
  validate_release_assets "${release_id}" exact

  if jq -e '.draft == false' <<<"${release}" >/dev/null; then
    echo "GitHub release ${RELEASE_TAG} (${release_id}) is already public with the exact validated asset set"
    return
  fi

  payload="$(jq -cn --argjson prerelease "${expected_prerelease}" \
    '{draft: false, prerelease: $prerelease}')"
  response="$(run_network gh api --method PATCH \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    --input - "repos/NVIDIA/aicr/releases/${release_id}" <<<"${payload}")" ||
    die "failed to publish exact GitHub release ${release_id}"
  jq -e --arg release_id "${release_id}" --arg release_tag "${RELEASE_TAG}" \
    --argjson prerelease "${expected_prerelease}" '
    type == "object" and
    (.id | tostring) == $release_id and
    .tag_name == $release_tag and
    .name == $release_tag and
    .draft == false and
    .prerelease == $prerelease
  ' <<<"${response}" >/dev/null || die "published release response did not preserve the validated identity"
  echo "Published GitHub release ${RELEASE_TAG} (${release_id})"
}

validate_promotion_file() {
  local path="$1" expected_prerelease
  require_regular_file "${path}"
  if [[ "${IS_PRERELEASE}" == "true" ]]; then expected_prerelease=true; else expected_prerelease=false; fi
  jq -e --argjson keys "${EXPECTED_KEYS_JSON}" --arg release "${RELEASE_TAG}" \
    --argjson prerelease "${expected_prerelease}" '
    type == "object" and
    keys == ["candidate_tag", "images", "is_prerelease", "prior_tag", "release_tag", "revision"] and
    .release_tag == $release and .is_prerelease == $prerelease and
    (.candidate_tag | test("^candidate-[0-9]+-[0-9]+$")) and
    (.revision | test("^[0-9a-f]{40}$")) and
    (.images | keys == $keys) and
    all(.images[];
      (.image | type == "string") and
      (.digest | test("^sha256:[0-9a-f]{64}$")) and
      (.prior_digest | type == "string") and
      (.version_state == "promote" or .version_state == "already_candidate") and
      (.latest_state == "promote" or .latest_state == "already_candidate" or .latest_state == "not_applicable"))
  ' "${path}" >/dev/null || die "validated promotion file has an invalid schema or release identity"
}

promote_command() {
  local validated_path="$1" pair key expected_image image digest prior_digest version_state latest_state
  local candidate_tag revision tags current_digest
  declare -A version_actions=()
  declare -A latest_actions=()
  [[ -n "${validated_path}" ]] || die "promote requires a validated map path"
  RELEASE_TAG="${RELEASE_TAG:-}"
  IS_PRERELEASE="${IS_PRERELEASE:-}"
  validate_release_tag "${RELEASE_TAG}"
  [[ "${IS_PRERELEASE}" == "true" || "${IS_PRERELEASE}" == "false" ]] ||
    die "IS_PRERELEASE must be true or false"
  validate_release_kind "${RELEASE_TAG}" "${IS_PRERELEASE}"
  if [[ "${IS_PRERELEASE}" == "false" ]]; then validate_stable_tag "${RELEASE_TAG}"; fi
  readonly RELEASE_TAG IS_PRERELEASE
  validate_promotion_file "${validated_path}"
  candidate_tag="$(jq -er '.candidate_tag' "${validated_path}")"
  revision="$(jq -er '.revision' "${validated_path}")"
  validate_candidate_tag "${candidate_tag}"
  validate_revision "${revision}"
  CANDIDATE_TAG="${candidate_tag}"
  GITHUB_SHA="${revision}"
  readonly CANDIDATE_TAG GITHUB_SHA
  require_single_line GH_TOKEN "${GH_TOKEN:-}"
  verify_current_release_source

  # Revalidate every candidate and alias state before the first mutation.
  for pair in "${IMAGE_PAIRS[@]}"; do
    key="${pair%%=*}"
    expected_image="${pair#*=}"
    image="$(jq -er --arg key "${key}" '.images[$key].image' "${validated_path}")"
    digest="$(jq -er --arg key "${key}" '.images[$key].digest' "${validated_path}")"
    prior_digest="$(jq -er --arg key "${key}" '.images[$key].prior_digest' "${validated_path}")"
    version_state="$(jq -er --arg key "${key}" '.images[$key].version_state' "${validated_path}")"
    latest_state="$(jq -er --arg key "${key}" '.images[$key].latest_state' "${validated_path}")"
    [[ "${image}" == "${expected_image}" ]] || die "validated image mapping changed for ${key}"
    resolve_candidate "${image}" "${digest}" >/dev/null
    tags="$(list_tags "${image}")"

    version_actions["${key}"]='promote'
    if tag_exists "${tags}" "${RELEASE_TAG}"; then
      current_digest="$(run_network crane digest "${image}:${RELEASE_TAG}")" ||
        die "failed to resolve ${image}:${RELEASE_TAG}"
      [[ "${current_digest}" == "${digest}" ]] || die "conflicting version alias for ${image}:${RELEASE_TAG}"
      version_actions["${key}"]='skip'
    elif [[ "${version_state}" != "promote" ]]; then
      die "validated version alias disappeared for ${image}"
    fi

    latest_actions["${key}"]='not_applicable'
    if [[ "${IS_PRERELEASE}" == "false" ]]; then
      validate_digest "${prior_digest}"
      [[ "${latest_state}" != "not_applicable" ]] || die "stable release is missing latest state for ${image}"
      tag_exists "${tags}" latest || die "${image}:latest disappeared after preflight"
      current_digest="$(run_network crane digest "${image}:latest")" || die "failed to resolve ${image}:latest"
      if [[ "${current_digest}" == "${digest}" ]]; then
        latest_actions["${key}"]='skip'
      elif [[ "${current_digest}" == "${prior_digest}" && "${latest_state}" == "promote" ]]; then
        latest_actions["${key}"]='promote'
      else
        die "${image}:latest changed after preflight"
      fi
    elif [[ "${latest_state}" != "not_applicable" ]]; then
      die "prerelease must not carry a latest promotion state"
    fi
  done

  # Phase 1: converge and verify every immutable version alias.
  for pair in "${IMAGE_PAIRS[@]}"; do
    key="${pair%%=*}"
    image="${pair#*=}"
    digest="$(jq -er --arg key "${key}" '.images[$key].digest' "${validated_path}")"
    if [[ "${version_actions[${key}]}" == "promote" ]]; then
      run_network crane tag "${image}@${digest}" "${RELEASE_TAG}" || die "failed to promote ${image}:${RELEASE_TAG}"
    fi
    current_digest="$(run_network crane digest "${image}:${RELEASE_TAG}")" ||
      die "failed to verify ${image}:${RELEASE_TAG}"
    [[ "${current_digest}" == "${digest}" ]] || die "version alias verification failed for ${image}"
  done

  if [[ "${IS_PRERELEASE}" == "false" ]]; then
    # Phase 2: only after every version alias is verified, converge stable latest aliases.
    for pair in "${IMAGE_PAIRS[@]}"; do
      key="${pair%%=*}"
      image="${pair#*=}"
      digest="$(jq -er --arg key "${key}" '.images[$key].digest' "${validated_path}")"
      if [[ "${latest_actions[${key}]}" == "promote" ]]; then
        run_network crane tag "${image}@${digest}" latest || die "failed to promote ${image}:latest"
      fi
      current_digest="$(run_network crane digest "${image}:latest")" ||
        die "failed to verify ${image}:latest"
      [[ "${current_digest}" == "${digest}" ]] || die "latest alias verification failed for ${image}"
    done
  fi
}

usage() {
  echo "usage: release-images.sh release-target | resolve <output.json> | preflight <digests.json> <validated.json> | promote <validated.json> | verify-source | publish-release" >&2
  exit 2
}

command="${1:-}"
case "${command}" in
  release-target)
    [[ $# -eq 1 ]] || usage
    release_target_command
    ;;
  resolve)
    [[ $# -eq 2 ]] || usage
    resolve_command "$2"
    ;;
  preflight)
    [[ $# -eq 3 ]] || usage
    preflight_command "$2" "$3"
    ;;
  promote)
    [[ $# -eq 2 ]] || usage
    promote_command "$2"
    ;;
  verify-source)
    [[ $# -eq 1 ]] || usage
    verify_source_command
    ;;
  publish-release)
    [[ $# -eq 1 ]] || usage
    publish_release_command
    ;;
  *)
    usage
    ;;
esac

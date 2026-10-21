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
# shellcheck source=tools/common
. "${SCRIPT_DIR}/common"
INSTALLER="${SCRIPT_DIR}/install-rc"
SCRATCH=$(mktemp -d)
trap 'rm -rf "${SCRATCH}"' EXIT

TAG=v1.2.3-rc1
case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "Unsupported test OS" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "Unsupported test architecture" >&2; exit 1 ;;
esac
ASSET="aicr_${TAG#v}_${OS}_${ARCH}.tar.gz"
ATTESTATION=aicr-attestation.sigstore.json

# An explicit tool PATH keeps an installed cosign, sudo, or aicr from affecting
# the fixtures, and prevents downloads from reaching the network.
mkdir -p "${SCRATCH}/bin" "${SCRATCH}/downloads"
for tool in bash dirname uname tar gzip shasum jq awk chmod install mkdir mktemp rm cp cmp grep sort head; do
  ln -s "$(command -v "${tool}")" "${SCRATCH}/bin/${tool}"
done
cat > "${SCRATCH}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
destination= url=
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) destination="$2"; shift 2 ;;
    https://github.com/NVIDIA/aicr/releases/download/*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[[ -n "$destination" && -n "$url" ]] || exit 22
cp "${INSTALL_RC_FIXTURES}/${url##*/}" "$destination"
STUB
chmod +x "${SCRATCH}/bin/curl"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

make_release() {
  local root="$1" with_attestation="$2" digest payload
  mkdir -p "${root}/contents" "${root}/assets"
  printf '#!/usr/bin/env bash\nprintf "aicr fixture\\n"\n' > "${root}/contents/aicr"
  chmod +x "${root}/contents/aicr"
  if [[ "${with_attestation}" == true ]]; then
    digest=$(shasum -a 256 "${root}/contents/aicr" | awk '{print $1}')
    payload=$(printf '{"subject":[{"name":"aicr","digest":{"sha256":"%s"}}]}' "${digest}" | base64 | tr -d '\n')
    printf '{"dsseEnvelope":{"payload":"%s"}}\n' "${payload}" > "${root}/contents/${ATTESTATION}"
    tar -czf "${root}/assets/${ASSET}" -C "${root}/contents" aicr "${ATTESTATION}"
  else
    tar -czf "${root}/assets/${ASSET}" -C "${root}/contents" aicr
  fi
  (cd "${root}/assets" && shasum -a 256 "${ASSET}" > aicr_checksums.txt)
}

run_installer() {
  local root="$1"
  PATH="${root}/bin:${SCRATCH}/bin" TMPDIR="${SCRATCH}/downloads" \
    INSTALL_RC_FIXTURES="${root}/assets" \
    INSTALL_RC_CONTENTS="${root}/contents" \
    INSTALL_RC_COSIGN_CALLS="${root}/cosign-calls" \
    INSTALL_RC_SUDO_CALLS="${root}/sudo-calls" \
    INSTALL_RC_MKDIR_FAILED="${root}/mkdir-failed" \
    INSTALL_RC_TARGET="${root}/install dir" \
    bash "${INSTALLER}" --tag "${TAG}" --dir "${root}/install dir" > "${root}/install.log" 2>&1
}

assert_binary() {
  local root="$1"
  if ! cmp -s "${root}/contents/aicr" "${root}/install dir/aicr"; then
    fail "${root##*/}: release binary was not installed"
  elif [[ ! -x "${root}/install dir/aicr" ]]; then
    fail "${root##*/}: installed binary is not executable"
  fi
}

assert_attestation() {
  local root="$1" digest subject
  if ! cmp -s "${root}/contents/${ATTESTATION}" "${root}/install dir/${ATTESTATION}"; then
    fail "${root##*/}: matching release attestation was not installed"
    return
  fi
  [[ ! -x "${root}/install dir/${ATTESTATION}" ]] || fail "${root##*/}: installed attestation is executable"
  digest=$(shasum -a 256 "${root}/install dir/aicr" | awk '{print $1}')
  subject=$(jq -r '.dsseEnvelope.payload' "${root}/install dir/${ATTESTATION}" \
    | decode_base64 | jq -r '.subject[0].digest.sha256')
  [[ "${subject}" == "${digest}" ]] || fail "${root##*/}: attestation does not describe the installed binary"
}

root="${SCRATCH}/fresh"
make_release "${root}" true
if run_installer "${root}"; then
  assert_binary "${root}"
  assert_attestation "${root}"
else
  fail "fresh installation failed"
  cat "${root}/install.log" >&2
fi

root="${SCRATCH}/upgrade"
make_release "${root}" true
mkdir -p "${root}/install dir"
printf 'old binary\n' > "${root}/install dir/aicr"
printf 'old attestation\n' > "${root}/install dir/${ATTESTATION}"
if run_installer "${root}"; then
  assert_binary "${root}"
  assert_attestation "${root}"
else
  fail "upgrade failed"
  cat "${root}/install.log" >&2
fi

root="${SCRATCH}/without-attestation"
make_release "${root}" false
mkdir -p "${root}/install dir"
printf 'old attestation\n' > "${root}/install dir/${ATTESTATION}"
if run_installer "${root}"; then
  assert_binary "${root}"
  [[ ! -e "${root}/install dir/${ATTESTATION}" ]] || fail "archive without attestation left stale attestation installed"
  grep -Fq "Archive does not contain ${ATTESTATION}" "${root}/install.log" || fail "missing attestation was not reported"
else
  fail "installation without an attestation failed"
  cat "${root}/install.log" >&2
fi

root="${SCRATCH}/checksum-mismatch"
make_release "${root}" true
mkdir -p "${root}/install dir"
printf 'old binary\n' > "${root}/install dir/aicr"
printf 'old attestation\n' > "${root}/install dir/${ATTESTATION}"
cp "${root}/install dir/aicr" "${root}/before-binary"
cp "${root}/install dir/${ATTESTATION}" "${root}/before-attestation"
printf '%064d  %s\n' 0 "${ASSET}" > "${root}/assets/aicr_checksums.txt"
if run_installer "${root}"; then
  fail "archive checksum mismatch was accepted"
elif ! grep -q 'Checksum mismatch' "${root}/install.log"; then
  fail "checksum mismatch failed for the wrong reason"
fi
cmp -s "${root}/before-binary" "${root}/install dir/aicr" || fail "checksum mismatch replaced the existing binary"
cmp -s "${root}/before-attestation" "${root}/install dir/${ATTESTATION}" || fail "checksum mismatch changed the existing attestation"

# These fixtures exercise invocation and warning policy, not cryptographic
# verification: the provenance payloads above deliberately contain no signature.
make_cosign() {
  local root="$1"
  mkdir -p "${root}/bin"
  cat > "${root}/bin/cosign" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == version ]]; then
  printf 'GitVersion: %s\n' "${INSTALL_RC_COSIGN_VERSION}"
  exit 0
fi
printf '%s\n' "$@" > "${INSTALL_RC_COSIGN_CALLS}"
[[ "$1" == verify-blob-attestation ]] || exit 22
shift
while [[ $# -gt 1 ]]; do
  case "$1" in
    --bundle) bundle="$2"; shift 2 ;;
    --type|--certificate-oidc-issuer|--certificate-identity-regexp) shift 2 ;;
    *) exit 22 ;;
  esac
done
cmp -s "${INSTALL_RC_CONTENTS}/aicr" "$1" || exit 22
cmp -s "${INSTALL_RC_CONTENTS}/aicr-attestation.sigstore.json" "${bundle}" || exit 22
exit "${INSTALL_RC_COSIGN_RESULT:-0}"
STUB
  chmod +x "${root}/bin/cosign"
}

for version in v3.0.4 v3.10.0 v4.0.0 v3.0.3; do
  root="${SCRATCH}/cosign-${version}"
  make_release "${root}" true
  make_cosign "${root}"
  if INSTALL_RC_COSIGN_VERSION="${version}" run_installer "${root}"; then
    assert_binary "${root}"
    assert_attestation "${root}"
    if [[ "${version}" == v3.0.3 ]]; then
      [[ ! -e "${root}/cosign-calls" ]] || fail "unsupported cosign was used for verification"
      grep -q 'older than the minimum required' "${root}/install.log" || fail "unsupported cosign was not reported"
    else
      grep -q 'Provenance verified' "${root}/install.log" || fail "supported cosign did not verify the extracted release files"
      for argument in verify-blob-attestation https://slsa.dev/provenance/v1 https://token.actions.githubusercontent.com \
        'https://github.com/NVIDIA/aicr/.github/workflows/on-tag\.yaml@refs/tags/.*'; do
        grep -Fxq -- "${argument}" "${root}/cosign-calls" || fail "cosign was not given ${argument}"
      done
    fi
  else
    fail "installation with ${version} failed"
    cat "${root}/install.log" >&2
  fi
done

root="${SCRATCH}/cosign-failure"
make_release "${root}" true
make_cosign "${root}"
if INSTALL_RC_COSIGN_VERSION=v3.0.4 INSTALL_RC_COSIGN_RESULT=1 run_installer "${root}"; then
  assert_attestation "${root}"
  grep -q 'Attestation verification failed' "${root}/install.log" || fail "cosign failure was not reported"
else
  fail "optional cosign failure prevented installation"
fi

# Force the directory-creation fallback even when the test runs as root. The
# sudo stub runs without changing privileges and records the real install/rm.
for with_attestation in true false; do
  root="${SCRATCH}/sudo-${with_attestation}"
  make_release "${root}" "${with_attestation}"
  mkdir -p "${root}/bin"
  cat > "${root}/bin/mkdir" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "-p ${INSTALL_RC_TARGET}" && ! -e "${INSTALL_RC_MKDIR_FAILED}" ]]; then
  : > "${INSTALL_RC_MKDIR_FAILED}"
  exit 1
fi
exec "${INSTALL_RC_REAL_MKDIR}" "$@"
STUB
  cat > "${root}/bin/sudo" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${INSTALL_RC_SUDO_CALLS}"
exec "$@"
STUB
  chmod +x "${root}/bin/mkdir" "${root}/bin/sudo"
  if INSTALL_RC_REAL_MKDIR="$(command -v mkdir)" run_installer "${root}"; then
    assert_binary "${root}"
    if [[ "${with_attestation}" == true ]]; then
      assert_attestation "${root}"
      grep -q '^install -m 0644 ' "${root}/sudo-calls" || fail "attestation installation bypassed sudo"
    else
      grep -q '^rm -f ' "${root}/sudo-calls" || fail "attestation removal bypassed sudo"
    fi
  else
    fail "sudo fallback failed"
    cat "${root}/install.log" >&2
  fi
done

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} RC installer assertion(s) failed" >&2
  exit 1
fi
echo "RC installer preserves matching binary/attestation pairs and rejects corrupt archives"

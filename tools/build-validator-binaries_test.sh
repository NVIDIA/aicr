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

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/bin"

cat > "${tmp}/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == env && "${2:-}" == GOARCH ]]; then
  echo amd64
  exit 0
fi
printf '%s\n' "$*" >> "${VALIDATOR_TEST_CALLS}"
EOF
chmod +x "${tmp}/bin/go"

VALIDATOR_TEST_CALLS="${tmp}/calls" \
PATH="${tmp}/bin:${PATH}" \
VALIDATOR_PHASES="deployment,conformance" \
VALIDATOR_ARCHES="amd64,arm64" \
  ./tools/build-validator-binaries

[[ "$(wc -l < "${tmp}/calls" | tr -d ' ')" == 4 ]]
grep -q -- '-o dist/validator/amd64/deployment ./validators/deployment' "${tmp}/calls"
grep -q -- '-o dist/validator/arm64/conformance ./validators/conformance' "${tmp}/calls"

if PATH="${tmp}/bin:${PATH}" VALIDATOR_ARCHES=ppc64le \
  ./tools/build-validator-binaries >/dev/null 2>&1; then
  echo "unsupported architecture unexpectedly passed" >&2
  exit 1
fi

for phase in conformance deployment performance; do
  dockerfile="validators/${phase}/Dockerfile"
  if grep -q 'COPY vendor/' "${dockerfile}"; then
    echo "${dockerfile} still copies vendor/" >&2
    exit 1
  fi
  grep -q 'ARG TARGETARCH' "${dockerfile}"
  grep -Fq "COPY dist/validator/\${TARGETARCH}/${phase} /${phase}" "${dockerfile}"
done

for caller in \
  .github/actions/aicr-build/build-validator-images.sh \
  .github/workflows/on-push.yaml \
  .github/workflows/on-tag.yaml \
  .github/workflows/uat-aws.yaml \
  .github/workflows/uat-azure.yaml \
  .github/workflows/uat-gcp.yaml \
  .github/workflows/vuln-scan-images.yaml \
  Makefile; do
  grep -q 'build-validator-binaries' "${caller}"
done

echo "validator binary build tests passed"

#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TOOL="${ROOT}/tools/normalize-go-license-urls"
TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT
# CI runners may export a token; the default-path cases below must not see it.
unset GITHUB_TOKEN

# The stub answers for github.com blob URLs and their raw.githubusercontent.com
# equivalents alike, and fails if the auth config travels with any other host.
cat > "${TMP}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${STUB_ARGV_LOG:-/dev/null}"
head_request=false
config=""
prev=""
for arg in "$@"; do
    if [[ "${prev}" == "--config" ]]; then
        config=${arg}
    fi
    prev=${arg}
    if [[ "${arg}" == "--head" ]]; then
        head_request=true
        continue
    fi
    [[ "${arg}" == https://* ]] || continue
    lookup=${arg}
    if [[ "${arg}" =~ ^https://raw\.githubusercontent\.com/([^/]+)/([^/]+)/(.+)$ ]]; then
        if [[ -z "${config}" ]] || ! grep -q '^header = "Authorization: Bearer ' "${config}"; then
            echo "raw.githubusercontent.com probed without the auth config" >&2
            exit 1
        fi
        lookup="https://github.com/${BASH_REMATCH[1]}/${BASH_REMATCH[2]}/blob/${BASH_REMATCH[3]}"
    elif [[ -n "${config}" ]]; then
        echo "auth config sent with a non-raw URL: ${arg}" >&2
        exit 1
    fi
    case "${lookup}" in
        https://cs.opensource.google/go/x/term/+/v0.34.0:LICENSE | \
        https://github.com/NVIDIA/aicr/blob/HEAD/licenses/overrides/example/LICENSE | \
        https://github.com/Azure/azure-sdk-for-go/blob/sdk/azcore/v1.20.0/sdk/azcore/LICENSE.txt | \
        https://github.com/aws/aws-sdk-go-v2/blob/config/v1.2.3/config/LICENSE.txt | \
        https://github.com/blang/semver/blob/v4.0.0/LICENSE | \
        https://github.com/cel-expr/cel-go/blob/v0.31.0/LICENSE | \
        https://github.com/example/repo/blob/nested/v1.2.3/LICENSE | \
        https://github.com/kyverno/kyverno/blob/48769d003e55/LICENSE | \
        https://github.com/root/module/blob/v1.2.3/LICENSE)
            printf '%s\t200\n' "${arg}"
            ;;
        https://github.com/throttled/*)
            printf '%s\t503\n' "${arg}"
            ;;
        *)
            printf '%s\t404\n' "${arg}"
            ;;
    esac
done
if [[ "${head_request}" != "true" ]]; then
    echo "license URL check did not use HEAD requests" >&2
    exit 1
fi
EOF
chmod +x "${TMP}/curl"

cat > "${TMP}/input.csv" <<'EOF'
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
github.com/Azure/azure-sdk-for-go/sdk/azcore,https://github.com/Azure/azure-sdk-for-go/blob/sdk/azcore/v1.20.0/sdk/azcore/LICENSE.txt,Apache-2.0
github.com/aws/aws-sdk-go-v2/config,https://github.com/aws/aws-sdk-go-v2/blob/config/v1.2.3/config/LICENSE.txt,Apache-2.0
github.com/example/repo/nested,https://github.com/example/repo/blob/nested/v1.2.3/nested/LICENSE,Apache-2.0
github.com/blang/semver/v4,https://github.com/blang/semver/blob/v4.0.0/v4/LICENSE,MIT
github.com/kyverno/kyverno/pkg/ext,https://github.com/kyverno/kyverno/blob/48769d003e55/ext/LICENSE,Apache-2.0
github.com/NVIDIA/aicr/example,https://github.com/NVIDIA/aicr/blob/HEAD/licenses/overrides/example/LICENSE,Apache-2.0
golang.org/x/term,https://cs.opensource.google/go/x/term/+/v0.34.0:LICENSE,BSD-3-Clause
github.com/google/cel-go,https://github.com/google/cel-go/blob/v0.31.0/LICENSE,Apache-2.0
EOF

CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/input.csv" > "${TMP}/actual.csv"
cat > "${TMP}/expected.csv" <<'EOF'
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
github.com/Azure/azure-sdk-for-go/sdk/azcore,https://github.com/Azure/azure-sdk-for-go/blob/sdk/azcore/v1.20.0/sdk/azcore/LICENSE.txt,Apache-2.0
github.com/aws/aws-sdk-go-v2/config,https://github.com/aws/aws-sdk-go-v2/blob/config/v1.2.3/config/LICENSE.txt,Apache-2.0
github.com/example/repo/nested,https://github.com/example/repo/blob/nested/v1.2.3/LICENSE,Apache-2.0
github.com/blang/semver/v4,https://github.com/blang/semver/blob/v4.0.0/LICENSE,MIT
github.com/kyverno/kyverno/pkg/ext,https://github.com/kyverno/kyverno/blob/48769d003e55/LICENSE,Apache-2.0
github.com/NVIDIA/aicr/example,https://github.com/NVIDIA/aicr/blob/HEAD/licenses/overrides/example/LICENSE,Apache-2.0
golang.org/x/term,https://cs.opensource.google/go/x/term/+/v0.34.0:LICENSE,BSD-3-Clause
github.com/google/cel-go,https://github.com/cel-expr/cel-go/blob/v0.31.0/LICENSE,Apache-2.0
EOF
diff -u "${TMP}/expected.csv" "${TMP}/actual.csv"

cat > "${TMP}/broken.csv" <<'EOF'
github.com/example/repo/missing,https://github.com/example/repo/blob/missing/v1.2.3/missing/LICENSE,Apache-2.0
EOF
if CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/broken.csv" > /dev/null 2>&1; then
    echo "normalizer accepted two unreachable URLs" >&2
    exit 1
fi

cat > "${TMP}/unchanged-broken.csv" <<'EOF'
example.com/missing,https://example.com/missing/LICENSE,Apache-2.0
EOF
if CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/unchanged-broken.csv" > /dev/null 2>&1; then
    echo "normalizer accepted an unchecked unreachable URL" >&2
    exit 1
fi

# Every unreachable package must be reported in one pass. Failing on the first
# one hides the rest behind another full release cycle, which is how a single
# moved repository blocked v0.21.0-rc1.
cat > "${TMP}/multi-broken.csv" <<'EOF'
example.com/first,https://example.com/first/LICENSE,MIT
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
example.com/second,https://example.com/second/LICENSE,MIT
EOF
if CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/multi-broken.csv" \
    > "${TMP}/multi.out" 2> "${TMP}/multi.err"; then
    echo "normalizer accepted two unreachable URLs" >&2
    exit 1
fi
for pkg in example.com/first example.com/second; do
    if ! grep -qF "no reachable license source URL for ${pkg}:" "${TMP}/multi.err"; then
        echo "normalizer did not report ${pkg} as unreachable" >&2
        exit 1
    fi
done
if ! grep -qF "2 package(s) have no reachable license source URL." "${TMP}/multi.err"; then
    echo "normalizer did not summarize the unreachable count" >&2
    exit 1
fi
# The reachable row between the two failures must still be emitted, so the
# failure path stays a report rather than an early abort.
if ! grep -qF "github.com/root/module," "${TMP}/multi.out"; then
    echo "normalizer dropped a reachable row while reporting failures" >&2
    exit 1
fi

# With a token, github.com blob URLs are probed through raw.githubusercontent.com
# with the token in a curl config file. The output still cites github.com, and
# the token never reaches curl's argv.
GITHUB_TOKEN=stub-token-value STUB_ARGV_LOG="${TMP}/argv.log" CURL_BIN="${TMP}/curl" \
    "${TOOL}" "${TMP}/input.csv" > "${TMP}/actual-token.csv"
diff -u "${TMP}/expected.csv" "${TMP}/actual-token.csv"
if grep -qF stub-token-value "${TMP}/argv.log"; then
    echo "normalizer passed the token on curl's command line" >&2
    exit 1
fi
if ! grep -qF https://raw.githubusercontent.com/root/module/v1.2.3/LICENSE "${TMP}/argv.log"; then
    echo "normalizer did not probe github.com URLs through raw.githubusercontent.com" >&2
    exit 1
fi
if grep -qE 'https://github\.com/[^ ]+/blob/' "${TMP}/argv.log"; then
    echo "normalizer probed a github.com blob URL anonymously despite a token" >&2
    exit 1
fi
if ! grep -qF https://cs.opensource.google/go/x/term/+/v0.34.0:LICENSE "${TMP}/argv.log"; then
    echo "normalizer dropped the non-GitHub probes in token mode" >&2
    exit 1
fi

# A rate-limited probe is reported as such, with the remedy, rather than
# reading as a dead URL.
cat > "${TMP}/throttled.csv" <<'EOF'
github.com/throttled/repo,https://github.com/throttled/repo/blob/v1.0.0/LICENSE,MIT
EOF
if CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/throttled.csv" \
    > /dev/null 2> "${TMP}/throttled.err"; then
    echo "normalizer accepted a rate-limited URL" >&2
    exit 1
fi
for msg in "rate-limited (HTTP 429/503)" "Set GITHUB_TOKEN"; do
    if ! grep -qF "${msg}" "${TMP}/throttled.err"; then
        echo "normalizer did not report rate limiting (missing: ${msg})" >&2
        exit 1
    fi
done

echo "normalize-go-license-urls tests passed"

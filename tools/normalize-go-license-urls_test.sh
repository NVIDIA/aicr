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
# equivalents alike, except that raw.githubusercontent.com rate-limits the
# rawthrottled owner, and fails if the auth config travels with any other host.
cat > "${TMP}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${STUB_ARGV_LOG:-/dev/null}"
head_request=false
config=""
prev=""
prevprev=""
rc=0
for arg in "$@"; do
    if [[ "${prev}" == "--config" ]]; then
        config=${arg}
    fi
    # The normalizer passes each URL as "--output /dev/null <url>", and real
    # curl reports a result for every one, even a string that is not a URL.
    is_url=false
    if [[ "${prevprev}" == "--output" && "${prev}" == "/dev/null" ]]; then
        is_url=true
    fi
    prevprev=${prev}
    prev=${arg}
    if [[ "${arg}" == "--head" ]]; then
        head_request=true
        continue
    fi
    "${is_url}" || continue
    lookup=${arg}
    if [[ "${arg}" =~ ^https://raw\.githubusercontent\.com/([^/]+)/([^/]+)/(.+)$ ]]; then
        if [[ -z "${config}" ]] || ! grep -q '^header = "Authorization: Bearer ' "${config}"; then
            echo "raw.githubusercontent.com probed without the auth config" >&2
            exit 1
        fi
        if [[ "${BASH_REMATCH[1]}" == "rawthrottled" ]]; then
            printf '%s\t429\t\n' "${arg}"
            continue
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
        https://github.com/flaky/repo/blob/v1.0.0/sub/LICENSE | \
        https://github.com/flaky/repo/blob/v2.0.0/LICENSE | \
        https://github.com/kyverno/kyverno/blob/48769d003e55/LICENSE | \
        https://github.com/root/module/blob/v1.2.3/LICENSE | \
        https://github.com/go-yaml/yaml/blob/v3.0.1/LICENSE | \
        https://github.com/evanphx/json-patch/blob/v4.13.0/LICENSE | \
        https://github.com/go-pseudo/pseudo/blob/0123456789ab/LICENSE | \
        https://github.com/go-pseudo3/pseudo3/blob/0123456789ab/LICENSE | \
        https://github.com/go-mdlicense/mdlicense/blob/v2.1.0/LICENSE.md | \
        https://github.com/go-nested/nested/blob/v2.0.0/LICENSE | \
        https://github.com/go-nested/nested/blob/v2.0.0/sub/LICENSE | \
        https://github.com/go-copying/copying/blob/v1.0.0/COPYING | \
        https://github.com/go-copying/copying/blob/v1.0.0/LICENSE | \
        https://github.com/rawthrottled/repo/blob/v1.0.0/sub/LICENSE | \
        https://github.com/rawthrottled/repo/blob/v1.0.0/LICENSE | \
        https://github.com/rawthrottled/repo/blob/v2.0.0/LICENSE)
            printf '%s\t200\t\n' "${arg}"
            ;;
        # Like real curl, a failed transfer is still reported, and fails the exit status.
        https://github.com/flaky/repo/blob/v1.0.0/LICENSE | \
        https://github.com/flaky/repo/blob/v2.0.0/sub/LICENSE)
            printf '%s\t000\tConnection reset by peer\n' "${arg}"
            rc=56
            ;;
        https://github.com/throttled/*)
            printf '%s\t503\t\n' "${arg}"
            ;;
        # go-licenses' placeholder when it cannot discover a license URL.
        Unknown)
            printf '%s\t000\tCould not resolve host: Unknown\n' "${arg}"
            rc=6
            ;;
        *)
            printf '%s\t404\t\n' "${arg}"
            ;;
    esac
done
if [[ "${head_request}" != "true" ]]; then
    echo "license URL check did not use HEAD requests" >&2
    exit 1
fi
exit "${rc}"
EOF
chmod +x "${TMP}/curl"

# The stub answers `go list -m` for the gopkg.in modules the cases below use,
# with a module directory under ${GO_STUB_MODCACHE}, and fails like go for any
# other module.
cat > "${TMP}/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" != "list -m -f {{if not .Replace}}{{.Version}} {{.Dir}}{{end}} "* ]]; then
    echo "unexpected go invocation: $*" >&2
    exit 2
fi
module=${*: -1}
case "${module}" in
    gopkg.in/yaml.v3) version=v3.0.1 ;;
    gopkg.in/evanphx/json-patch.v4) version=v4.13.0 ;;
    gopkg.in/pseudo.v1) version=v1.0.0-20200102030405-0123456789ab ;;
    gopkg.in/pseudo3.v1) version=v1.2.4-0.20200102030405-0123456789ab ;;
    gopkg.in/mdlicense.v2) version=v2.1.0 ;;
    gopkg.in/nested.v2) version=v2.0.0 ;;
    gopkg.in/copying.v1) version=v1.0.0 ;;
    # The format prints nothing for a replaced module.
    gopkg.in/replaced.v1) exit 0 ;;
    *) echo "go: module ${module}: not a known dependency" >&2; exit 1 ;;
esac
printf '%s %s\n' "${version}" "${GO_STUB_MODCACHE}/${module}@${version}"
EOF
chmod +x "${TMP}/go"

modcache="${TMP}/modcache"

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
# with the token in a curl config file. The output still cites github.com, the
# token never reaches curl's argv, and while raw answers every probe no
# github.com blob URL is probed.
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

# raw.githubusercontent.com can rate-limit a whole repository, with or without a
# token, while github.com still serves the same files. An inconclusive raw probe
# is repeated at the github.com URL for the same ref and path, without the token
# and two at a time, and that answer stands in for it: a 200 resolves the
# top-ranked candidate and a 404 moves on to the next one. Rows raw answered are
# left alone.
cat > "${TMP}/raw-throttled.csv" <<'EOF'
github.com/rawthrottled/repo/sub,https://github.com/rawthrottled/repo/blob/v1.0.0/sub/LICENSE,MIT
github.com/rawthrottled/repo/v2/sub,https://github.com/rawthrottled/repo/blob/v2.0.0/sub/LICENSE,MIT
github.com/blang/semver/v4,https://github.com/blang/semver/blob/v4.0.0/v4/LICENSE,MIT
EOF
if ! GITHUB_TOKEN=stub-token-value STUB_ARGV_LOG="${TMP}/raw-throttled-argv.log" CURL_BIN="${TMP}/curl" \
    "${TOOL}" "${TMP}/raw-throttled.csv" > "${TMP}/raw-throttled.out" 2> "${TMP}/raw-throttled.err"; then
    echo "normalizer failed on URLs only raw.githubusercontent.com rate-limited:" >&2
    cat "${TMP}/raw-throttled.err" >&2
    exit 1
fi
cat > "${TMP}/raw-throttled.expected" <<'EOF'
github.com/rawthrottled/repo/sub,https://github.com/rawthrottled/repo/blob/v1.0.0/sub/LICENSE,MIT
github.com/rawthrottled/repo/v2/sub,https://github.com/rawthrottled/repo/blob/v2.0.0/LICENSE,MIT
github.com/blang/semver/v4,https://github.com/blang/semver/blob/v4.0.0/LICENSE,MIT
EOF
diff -u "${TMP}/raw-throttled.expected" "${TMP}/raw-throttled.out"
recheck_argv=$(grep -F https://github.com/rawthrottled/ "${TMP}/raw-throttled-argv.log")
if [[ "${recheck_argv}" != *"--parallel-max 2 "* ]]; then
    echo "normalizer did not recheck two at a time: ${recheck_argv}" >&2
    exit 1
fi
if [[ "${recheck_argv}" == *--config* || "${recheck_argv}" == *blang/semver* ]]; then
    echo "normalizer rechecked with the token or rechecked a conclusive probe: ${recheck_argv}" >&2
    exit 1
fi

# A URL that stays inconclusive on github.com too still fails, and the report
# says both hosts were tried.
if GITHUB_TOKEN=stub-token-value CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/throttled.csv" \
    > /dev/null 2> "${TMP}/throttled-token.err"; then
    echo "normalizer accepted a URL rate-limited on both hosts" >&2
    exit 1
fi
if ! grep -qF "tried (503: after raw.githubusercontent.com answered 503): https://github.com/throttled/repo/blob/v1.0.0/LICENSE" \
    "${TMP}/throttled-token.err"; then
    echo "normalizer did not report the github.com recheck:" >&2
    cat "${TMP}/throttled-token.err" >&2
    exit 1
fi

# One failed transfer makes curl exit non-zero for the whole batch. When every
# package still resolves, that must not fail the run.
cat > "${TMP}/flaky-irrelevant.csv" <<'EOF'
github.com/flaky/repo/sub,https://github.com/flaky/repo/blob/v1.0.0/sub/LICENSE,MIT
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
EOF
if ! CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/flaky-irrelevant.csv" \
    > "${TMP}/flaky-irrelevant.out" 2> "${TMP}/flaky-irrelevant.err"; then
    echo "normalizer failed on a transfer error no package depended on:" >&2
    cat "${TMP}/flaky-irrelevant.err" >&2
    exit 1
fi
diff -u "${TMP}/flaky-irrelevant.csv" "${TMP}/flaky-irrelevant.out"

# A failed transfer on a higher-ranked candidate must not fall through to a
# lower-ranked one: the emitted URL would then depend on the network. It is
# reported with curl's error instead.
cat > "${TMP}/flaky-blocking.csv" <<'EOF'
github.com/flaky/repo/sub,https://github.com/flaky/repo/blob/v2.0.0/sub/LICENSE,MIT
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
EOF
if CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/flaky-blocking.csv" \
    > "${TMP}/flaky-blocking.out" 2> "${TMP}/flaky-blocking.err"; then
    echo "normalizer skipped past a candidate whose probe failed" >&2
    exit 1
fi
for msg in \
    "no reachable license source URL for github.com/flaky/repo/sub:" \
    "tried (000: Connection reset by peer): https://github.com/flaky/repo/blob/v2.0.0/sub/LICENSE" \
    "1 probe(s) got no HTTP response"; do
    if ! grep -qF "${msg}" "${TMP}/flaky-blocking.err"; then
        echo "normalizer did not report the failed probe (missing: ${msg})" >&2
        exit 1
    fi
done
if grep -qF "github.com/flaky/repo/sub," "${TMP}/flaky-blocking.out"; then
    echo "normalizer emitted a fallback URL for a package whose preferred probe failed" >&2
    exit 1
fi

# The same holds with a token when the github.com recheck fails too, and the
# report keeps the error from each host.
if GITHUB_TOKEN=stub-token-value CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/flaky-blocking.csv" \
    > "${TMP}/flaky-blocking-token.out" 2> "${TMP}/flaky-blocking-token.err"; then
    echo "normalizer skipped past a candidate whose probe failed on both hosts" >&2
    exit 1
fi
if ! grep -qF "tried (000: Connection reset by peer, after raw.githubusercontent.com failed: Connection reset by peer): https://github.com/flaky/repo/blob/v2.0.0/sub/LICENSE" \
    "${TMP}/flaky-blocking-token.err"; then
    echo "normalizer did not report the failed probe on each host:" >&2
    cat "${TMP}/flaky-blocking-token.err" >&2
    exit 1
fi
if grep -qF "github.com/flaky/repo/sub," "${TMP}/flaky-blocking-token.out"; then
    echo "normalizer emitted a fallback URL for a package whose preferred probe failed on both hosts" >&2
    exit 1
fi

# When gopkg.in is down, go-licenses cannot read the go-source tag that names a
# gopkg.in module's repository and emits "Unknown". The URL is derived from
# gopkg.in's fixed naming rule, the module version and the license file
# go-licenses chose (the fourth column), then probed like any other candidate.
# copying.v1 also has a reachable COPYING, which go-licenses did not choose.
m="${modcache}/gopkg.in"
cat > "${TMP}/gopkgin.csv" <<EOF
gopkg.in/yaml.v3,Unknown,MIT,${m}/yaml.v3@v3.0.1/LICENSE
gopkg.in/evanphx/json-patch.v4,Unknown,BSD-3-Clause,${m}/evanphx/json-patch.v4@v4.13.0/LICENSE
gopkg.in/pseudo.v1/sub,Unknown,MIT,${m}/pseudo.v1@v1.0.0-20200102030405-0123456789ab/LICENSE
gopkg.in/pseudo3.v1,Unknown,MIT,${m}/pseudo3.v1@v1.2.4-0.20200102030405-0123456789ab/LICENSE
gopkg.in/mdlicense.v2,Unknown,MIT,${m}/mdlicense.v2@v2.1.0/LICENSE.md
gopkg.in/nested.v2/sub,Unknown,BSD-3-Clause,${m}/nested.v2@v2.0.0/sub/LICENSE
gopkg.in/copying.v1,Unknown,MIT,${m}/copying.v1@v1.0.0/LICENSE
EOF
GO_STUB_MODCACHE="${modcache}" GO_BIN="${TMP}/go" CURL_BIN="${TMP}/curl" \
    "${TOOL}" "${TMP}/gopkgin.csv" > "${TMP}/gopkgin.out"
cat > "${TMP}/gopkgin.expected" <<'EOF'
gopkg.in/yaml.v3,https://github.com/go-yaml/yaml/blob/v3.0.1/LICENSE,MIT
gopkg.in/evanphx/json-patch.v4,https://github.com/evanphx/json-patch/blob/v4.13.0/LICENSE,BSD-3-Clause
gopkg.in/pseudo.v1/sub,https://github.com/go-pseudo/pseudo/blob/0123456789ab/LICENSE,MIT
gopkg.in/pseudo3.v1,https://github.com/go-pseudo3/pseudo3/blob/0123456789ab/LICENSE,MIT
gopkg.in/mdlicense.v2,https://github.com/go-mdlicense/mdlicense/blob/v2.1.0/LICENSE.md,MIT
gopkg.in/nested.v2/sub,https://github.com/go-nested/nested/blob/v2.0.0/sub/LICENSE,BSD-3-Clause
gopkg.in/copying.v1,https://github.com/go-copying/copying/blob/v1.0.0/LICENSE,MIT
EOF
diff -u "${TMP}/gopkgin.expected" "${TMP}/gopkgin.out"

# An "Unknown" URL is still fatal outside gopkg.in, for a gopkg.in module whose
# version go cannot resolve, for a replaced one, and when go-licenses chose no
# license file or one outside the module's copy.
for row in \
    "example.com/vanity,Unknown,MIT" \
    "gopkg.in/unresolvable.v1,Unknown,MIT,${m}/unresolvable.v1@v1.0.0/LICENSE" \
    "gopkg.in/replaced.v1,Unknown,MIT,${m}/replaced.v1@v1.0.0/LICENSE" \
    "gopkg.in/yaml.v3,Unknown,MIT,Unknown" \
    "gopkg.in/yaml.v3,Unknown,MIT,${m}/yaml.v3@v3.0.0/LICENSE" \
    "gopkg.in/yaml.v3,Unknown,MIT"; do
    printf '%s\n' "${row}" > "${TMP}/unknown.csv"
    if GO_STUB_MODCACHE="${modcache}" GO_BIN="${TMP}/go" CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/unknown.csv" \
        > /dev/null 2> "${TMP}/unknown.err"; then
        echo "normalizer accepted an Unknown license URL for ${row}" >&2
        exit 1
    fi
    if ! grep -qF "no reachable license source URL for ${row%%,*}:" "${TMP}/unknown.err"; then
        echo "normalizer did not report ${row%%,*} as unreachable" >&2
        exit 1
    fi
done

# curl failing outright, without a result per URL, is still fatal.
printf '#!/usr/bin/env bash\nexit 2\n' > "${TMP}/curl-broken"
chmod +x "${TMP}/curl-broken"
if CURL_BIN="${TMP}/curl-broken" "${TOOL}" "${TMP}/input.csv" \
    > /dev/null 2> "${TMP}/broken-curl.err"; then
    echo "normalizer accepted a curl run that reported no results" >&2
    exit 1
fi
if ! grep -qF "failed to check license source URLs (curl exit 2" "${TMP}/broken-curl.err"; then
    echo "normalizer did not report the curl failure" >&2
    exit 1
fi

echo "normalize-go-license-urls tests passed"

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

cat > "${TMP}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
for arg in "$@"; do
    case "${arg}" in
        https://github.com/aws/aws-sdk-go-v2/blob/config/v1.2.3/config/LICENSE.txt | \
        https://github.com/example/repo/blob/nested/v1.2.3/LICENSE)
            printf '%s\t200\n' "${arg}"
            ;;
        https://*)
            printf '%s\t404\n' "${arg}"
            ;;
    esac
done
EOF
chmod +x "${TMP}/curl"

cat > "${TMP}/input.csv" <<'EOF'
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
github.com/aws/aws-sdk-go-v2/config,https://github.com/aws/aws-sdk-go-v2/blob/config/v1.2.3/config/LICENSE.txt,Apache-2.0
github.com/example/repo/nested,https://github.com/example/repo/blob/nested/v1.2.3/nested/LICENSE,Apache-2.0
EOF

CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/input.csv" > "${TMP}/actual.csv"
cat > "${TMP}/expected.csv" <<'EOF'
github.com/root/module,https://github.com/root/module/blob/v1.2.3/LICENSE,MIT
github.com/aws/aws-sdk-go-v2/config,https://github.com/aws/aws-sdk-go-v2/blob/config/v1.2.3/config/LICENSE.txt,Apache-2.0
github.com/example/repo/nested,https://github.com/example/repo/blob/nested/v1.2.3/LICENSE,Apache-2.0
EOF
diff -u "${TMP}/expected.csv" "${TMP}/actual.csv"

cat > "${TMP}/broken.csv" <<'EOF'
github.com/example/repo/missing,https://github.com/example/repo/blob/missing/v1.2.3/missing/LICENSE,Apache-2.0
EOF
if CURL_BIN="${TMP}/curl" "${TOOL}" "${TMP}/broken.csv" > /dev/null 2>&1; then
    echo "normalizer accepted two unreachable URLs" >&2
    exit 1
fi

echo "normalize-go-license-urls tests passed"

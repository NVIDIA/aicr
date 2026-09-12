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

# Verify no Go tool is installed in a way that depends on the checksum database.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_TOOLS="${SCRIPT_DIR}/setup-tools"

bash -n "${SETUP_TOOLS}"

# This guard used to assert that every `go install` cleared GOFLAGS. #2667
# removed the last one, so the contract is now stricter and the guard asserts
# the stricter thing: no `go install pkg@version` at all.
#
# `go install pkg@version` resolves outside the main module. Nothing it builds
# is covered by this repo's go.sum, so every transitive dependency is
# authenticated against sum.golang.org on each run, and an outage there fails
# the install -- which is what took down tests / E2E on the v0.21.1 release
# (#2664) and still reached the qualification gate afterwards (#2667). Building
# from the main module verifies against the committed go.sum instead and never
# consults the checksum database.
#
# Anchor on start-of-line or whitespace before `go`, not on "some character then
# whitespace": the latter cannot see an unindented `go install` at column 1,
# which is exactly the shape this guard exists to catch. Requiring whitespace
# (rather than any non-`#` character) also keeps `cargo install` from matching.
#
# Comment lines are excluded so prose mentioning `go install` does not trip it.
mapfile -t install_lines < <(
    grep -nE '(^|[[:space:]])go install ' "${SETUP_TOOLS}" \
        | grep -vE '^[0-9]+:[[:space:]]*#' || true
)

if [[ "${#install_lines[@]}" -ne 0 ]]; then
    echo "FAIL: found ${#install_lines[@]} 'go install' line(s) in setup-tools." >&2
    echo "      Each one reintroduces the sum.golang.org dependency #2667 removed." >&2
    echo "      Build from the main module with build_module_tool, or install a" >&2
    echo "      checksum-pinned binary release the way the oasdiff block does." >&2
    printf '        %s\n' "${install_lines[@]}" >&2
    exit 1
fi

# Assert the module-built tools BY PACKAGE, so deleting the installs outright
# cannot make the check above pass vacuously. Counting alone is not enough: two
# `build_module_tool apidiff ...` lines satisfy a floor of 2 while go-licenses
# silently disappears and `make license-check` loses its tool.
#
# These two are here because neither publishes a binary release. Tools that do
# (addlicense, oasdiff, ctlptl, ...) are downloaded and checksum-verified
# instead, and are deliberately not listed.
REQUIRED_MODULE_TOOLS=(
    "golang.org/x/exp/cmd/apidiff"
    "github.com/google/go-licenses/v2"
)

mapfile -t module_builds < <(
    grep -nE '(^|[[:space:]])build_module_tool ' "${SETUP_TOOLS}" \
        | grep -vE '^[0-9]+:[[:space:]]*#' | grep -v 'build_module_tool()' || true
)

missing_tools=0
for pkg in "${REQUIRED_MODULE_TOOLS[@]}"; do
    # Match the package as a whole argument, so a longer path that merely
    # contains this one cannot vouch for it.
    if ! printf '%s\n' "${module_builds[@]}" \
        | grep -qE "(^|[[:space:]])${pkg//./\\.}([[:space:]]|\$)"; then
        echo "FAIL: no 'build_module_tool' call builds ${pkg}." >&2
        echo "      It has no binary release, so building it from the main module is what" >&2
        echo "      keeps its install off sum.golang.org (#2667)." >&2
        missing_tools=1
    fi
done
[[ "${missing_tools}" -eq 0 ]] || exit 1

echo "No checksum-database-dependent installs; ${#REQUIRED_MODULE_TOOLS[@]} required tools built from the main module"

if ! grep -qE 'installed_helm_version.*==.*HELM_VERSION' "${SETUP_TOOLS}"; then
    echo "FAIL: helm install block no longer compares the installed version against HELM_VERSION" >&2
    exit 1
fi
echo "Helm install block still enforces its version pin"

# The grep checks above only prove the relevant tokens exist in the script,
# not that install_helm() actually picks the right Homebrew action or
# reports a mismatch. Exercise the real decision logic in a subshell with
# fake `helm`/`brew` binaries on PATH: setup-tools' own `set -euo pipefail`
# (sourced from tools/common) must not escape into this test process, and
# each scenario needs its own PATH/env without clobbering the others.
#
# homebrew_managed: "true" makes the fake `brew list --versions helm` exit 0
#   (a Homebrew-owned keg), "false" makes it exit 1 (a manually installed
#   binary Homebrew doesn't know about).
# post_install_version: what the fake `brew upgrade|install helm` leaves
#   installed, fed back to install_helm()'s post-install version check.
run_install_helm() {
    local homebrew_managed="$1" post_install_version="$2"
    (
        set -euo pipefail
        scratch=$(mktemp -d)
        trap 'rm -rf "${scratch}"' EXIT

        fake_bin="${scratch}/bin"
        mkdir -p "${fake_bin}"
        helm_version_file="${scratch}/helm_version"
        echo "9.8.0" > "${helm_version_file}" # stale, pre-install version
        brew_call_log="${scratch}/brew_calls"
        : > "${brew_call_log}"

        cat > "${fake_bin}/helm" << EOF
#!/usr/bin/env bash
if [[ "\$1" == "version" ]]; then
    cat "${helm_version_file}" 2>/dev/null
    exit 0
fi
exit 1
EOF
        chmod +x "${fake_bin}/helm"

        cat > "${fake_bin}/brew" << EOF
#!/usr/bin/env bash
echo "\$*" >> "${brew_call_log}"
case "\$1" in
    list)
        if [[ "${homebrew_managed}" == "true" ]]; then
            echo "helm 9.8.0"
            exit 0
        fi
        exit 1
        ;;
    upgrade|install)
        echo "${post_install_version}" > "${helm_version_file}"
        exit 0
        ;;
esac
EOF
        chmod +x "${fake_bin}/brew"

        export PATH="${fake_bin}:${PATH}"
        export HELM_VERSION="9.9.9" # fake pin, independent of any real Helm release
        export UPGRADE="false"
        export AUTO_MODE="true" # skip the interactive prompt_continue read
        export SETUP_TOOLS_SOURCE_ONLY="true"
        # shellcheck source=tools/setup-tools
        source "${SETUP_TOOLS}"
        OS="darwin" # override the real-host detection sourcing just ran

        # Capture the exit code explicitly rather than relying on `set -e` to
        # halt this subshell on failure: this whole function runs as the
        # tested command of a caller's `if`, and bash ignores -e for the full
        # extent of a compound command under test that way -- including
        # nested subshells that re-enable it themselves. Without this, a
        # failing install_helm would silently fall through to the two lines
        # below and this subshell would still exit 0.
        rc=0
        install_helm || rc=$?
        echo "---BREW_CALLS---"
        cat "${brew_call_log}"
        exit "${rc}"
    )
}

output=$(run_install_helm "true" "9.9.9")
if ! printf '%s\n' "${output}" | grep -q '^upgrade helm$'; then
    echo "FAIL: a stale Homebrew-managed Helm did not run 'brew upgrade helm'" >&2
    echo "${output}" >&2
    exit 1
fi
if printf '%s\n' "${output}" | grep -q '^install helm$'; then
    echo "FAIL: a stale Homebrew-managed Helm ran 'brew install helm' instead of upgrading" >&2
    exit 1
fi
echo "Stale Homebrew-managed Helm runs 'brew upgrade helm'"

output=$(run_install_helm "false" "9.9.9")
if ! printf '%s\n' "${output}" | grep -q '^install helm$'; then
    echo "FAIL: a stale unmanaged PATH Helm did not run 'brew install helm'" >&2
    echo "${output}" >&2
    exit 1
fi
if printf '%s\n' "${output}" | grep -q '^upgrade helm$'; then
    echo "FAIL: a stale unmanaged PATH Helm ran 'brew upgrade helm', which fails on a binary Homebrew doesn't own" >&2
    exit 1
fi
echo "Stale unmanaged PATH Helm runs 'brew install helm'"

if output=$(run_install_helm "true" "9.9.8"); then
    echo "FAIL: a post-install version mismatch (got 9.9.8, pinned 9.9.9) did not fail install_helm" >&2
    echo "${output}" >&2
    exit 1
fi
if ! printf '%s\n' "${output}" | grep -q 'is pinned in .settings.yaml'; then
    echo "FAIL: a post-install version mismatch (got 9.9.8, pinned 9.9.9) did not report the pin-mismatch error" >&2
    echo "${output}" >&2
    exit 1
fi
echo "Post-install version mismatch fails install_helm and reports the pin-mismatch error"

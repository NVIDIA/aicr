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

# --- module-built tool pins (apidiff, go-licenses) --------------------------
# #2741 removed these tools' .settings.yaml keys in favour of the go.mod
# require line, but setup-tools kept reading the keys. yq exits 0 and prints
# "null" for a missing key, so the required version became the string "null":
# it never equalled an installed version, so apidiff rebuilt on every run
# while reporting success. These cover the retain/replace decision that
# regression turned into "always replace".
#
# Sourced in a subshell for the reason install_helm is: setup-tools defines
# these at top level and the source-only hook returns before the installers.
check_module_tool_pins() {
    (
        export SETUP_TOOLS_SOURCE_ONLY="true"
        # shellcheck source=tools/setup-tools
        source "${SETUP_TOOLS}"

        module_tool_up_to_date "v1.2.3" "v1.2.3" ||
            { echo "an exactly-matching installed version was not retained"; exit 1; }
        ! module_tool_up_to_date "v1.2.3" "v1.2.4" ||
            { echo "a stale installed version was retained instead of replaced"; exit 1; }
        ! module_tool_up_to_date "" "v1.2.3" ||
            { echo "an unreadable installed version was treated as up to date"; exit 1; }

        # The regression itself: yq printed "null" for the removed key, and an
        # unresolved pin must never look like a match.
        ! module_tool_up_to_date "v1.2.3" "null" ||
            { echo "the literal 'null' from a missing settings key was treated as a pin"; exit 1; }
        ! module_tool_up_to_date "" "" ||
            { echo "an unresolved required version was treated as up to date"; exit 1; }

        # Both pins must resolve from go.mod, not from the removed keys.
        for module in golang.org/x/exp github.com/google/go-licenses/v2; do
            resolved=$(go_mod_required_version "${module}" "${REPO_ROOT}/go.mod") ||
                { echo "could not resolve ${module} from go.mod"; exit 1; }
            [[ -n "${resolved}" && "${resolved}" != "null" ]] ||
                { echo "${module} resolved to '${resolved}' rather than a version"; exit 1; }
        done
    )
}

if ! reason=$(check_module_tool_pins); then
    echo "FAIL: ${reason}" >&2
    exit 1
fi
echo "Module-built tool pins: exact retained, stale/unreadable/unresolved replaced, both resolve from go.mod"

# --- checksum verification core (#2666) ------------------------------------
# verify_digest is the single comparison every binary install passes through
# before it reaches /usr/local/bin, so it has to fail closed: an empty or
# unrecognized expectation is a missing check, not a passed one. verify_sha256
# used to return 0 for "" and "SKIP", so a caller whose checksum lookup came
# back empty went on to install an unverified binary.
#
# The digests are fixed rather than computed here, so the test does not lean on
# the same shasum/sha256sum the code under test uses. Each call runs in its own
# subshell because a failed verification exits rather than returns.
DIGEST_PAYLOAD_SHA256="36e355979e671af2ffddc498d4b7e5d431b2b25bf27618beaf7a8d2d52e3d4d2"
DIGEST_PAYLOAD_SHA512="64cc424eb1d30d5546e61c31cd7deb22b733809a42715c59230e415255b5a107ce431eb310548ebe09103a74f8fdb0cc167c07c1c37a27889ab9c202eeb4585f"

check_digest_verification() {
    (
        export SETUP_TOOLS_SOURCE_ONLY="true"
        # shellcheck source=tools/setup-tools
        source "${SETUP_TOOLS}"

        scratch=$(mktemp -d)
        trap 'rm -rf "${scratch}"' EXIT
        payload="${scratch}/payload"
        printf 'aicr' > "${payload}"

        # expect <pass|fail> <label> <command...>
        expect() {
            local want="$1" label="$2"; shift 2
            local rc=0
            ( "$@" ) >/dev/null 2>&1 || rc=$?
            if [[ "${want}" == "pass" && "${rc}" -ne 0 ]]; then
                echo "${label}: expected to pass, exited ${rc}"; exit 1
            fi
            if [[ "${want}" == "fail" && "${rc}" -eq 0 ]]; then
                echo "${label}: expected to fail, but passed"; exit 1
            fi
        }

        expect pass "matching sha256"  verify_digest "${payload}" sha256 "${DIGEST_PAYLOAD_SHA256}"
        expect pass "matching sha512"  verify_digest "${payload}" sha512 "${DIGEST_PAYLOAD_SHA512}"
        expect fail "mismatched sha256" verify_digest "${payload}" sha256 "${DIGEST_PAYLOAD_SHA256/3/4}"
        expect fail "sha512 digest checked as sha256" verify_digest "${payload}" sha256 "${DIGEST_PAYLOAD_SHA512}"
        expect fail "empty expectation" verify_digest "${payload}" sha256 ""
        expect fail "unknown algorithm" verify_digest "${payload}" md5 "${DIGEST_PAYLOAD_SHA256}"

        # The fail-open this replaces, asserted on the old entry point too.
        expect pass "verify_sha256 on a match" verify_sha256 "${payload}" "${DIGEST_PAYLOAD_SHA256}"
        expect fail "verify_sha256 with an empty expectation" verify_sha256 "${payload}" ""
        expect fail "verify_sha256 with SKIP" verify_sha256 "${payload}" "SKIP"
    )
}

if ! reason=$(check_digest_verification); then
    echo "FAIL: ${reason}" >&2
    exit 1
fi
echo "Digest verification: sha256 and sha512 match, and a mismatch, empty expectation, unknown algorithm or SKIP all fail closed"

# A sidecar source names the digest algorithm and, optionally, the file suffix
# the digest is published under. The suffix is not derivable from the algorithm:
# dl.k8s.io serves kubectl's SHA-256 at `.sha256` and 404s on `.sha256sum`.
check_sidecar_specs() {
    (
        export SETUP_TOOLS_SOURCE_ONLY="true"
        # shellcheck source=tools/setup-tools
        source "${SETUP_TOOLS}"

        expect_spec() {
            local source="$1" want="$2" got
            got=$(release_sidecar_spec "${source}") || { echo "${source} was rejected"; exit 1; }
            [[ "${got}" == "${want}" ]] || { echo "${source} resolved to '${got}', want '${want}'"; exit 1; }
        }
        expect_spec sidecar:sha256         "sha256 .sha256sum"
        expect_spec sidecar:sha512         "sha512 .sha512"
        expect_spec sidecar:sha256:.sha256 "sha256 .sha256"

        if ( release_sidecar_spec sidecar:md5 ) >/dev/null 2>&1; then
            echo "an unsupported algorithm was accepted"; exit 1
        fi
    )
}

if ! reason=$(check_sidecar_specs); then
    echo "FAIL: ${reason}" >&2
    exit 1
fi
echo "Sidecar sources: default suffix per algorithm, explicit suffix honored, unsupported algorithm rejected"

# --- every binary install is checksum-verified (#2666) ----------------------
# install_release_binary will not run without a checksum source, so routing
# every install through it is what makes verification impossible to skip. This
# guard fails if an install into /usr/local/bin appears anywhere else -- the
# shape a tool takes when it is added the quick way, which is how eight installs
# ended up with no integrity check at all.
#
# Exceptions are named rather than pattern-matched, each with its reason:
#   grype    -- installs through anchore's install.sh, which is itself
#               checksum-pinned (GRYPE_INSTALL_SHA256) and verifies what it fetches.
#   yq       -- UNVERIFIED. The bootstrap tool: it is what reads .settings.yaml,
#               so it installs before any pin can be read and tracks `latest`.
#               Verifying it needs a bootstrap pin; #2939.
#   yamllint -- a pip package at a pinned version in a venv, symlinked in. pip
#               resolves it rather than a release checksum, so it is outside
#               what install_release_binary covers.
BIN_INSTALL_EXCEPTIONS=(
    "/usr/local/bin/grype"
    "/usr/local/bin/yq"
    "/usr/local/bin/yamllint"
)

helper_start=$(grep -nE '^install_release_binary\(\) *\{' "${SETUP_TOOLS}" | cut -d: -f1 || true)
if [[ -z "${helper_start}" ]]; then
    echo "FAIL: install_release_binary() is not defined in setup-tools." >&2
    exit 1
fi
helper_end=$(awk -v s="${helper_start}" 'NR > s && /^\}/ { print NR; exit }' "${SETUP_TOOLS}")

# An allowlist, not a list of forbidden verbs. The first version of this check
# enumerated the ways to write a file (mv, cp, curl, ...) and missed `sudo ln`,
# which put yamllint into /usr/local/bin unseen: a verb list fails open for
# every verb nobody anticipated. Instead, every line outside the helper that
# touches /usr/local/bin must be a named exception or a plain read -- a [[ ]]
# test or a log_* message, and never under sudo. Anything else fails.
#
# Both read patterns match the whole line, so a read is a line that is only a
# test or only a log call. Anchoring the start alone let
# `[[ -f x ]] && cp x /usr/local/bin/x` pass as a test, and an unanchored log
# match let `install ... /usr/local/bin/x || log_error "..."` pass as a message.
# [^;&|] keeps a command separator from hiding inside the test.
read_test_re='^[[:space:]]*((el)?if[[:space:]]+)?\[\[[^;&|]*\]\]([[:space:]]*;[[:space:]]*then)?[[:space:]]*$'
read_log_re='^[[:space:]]*log_(info|warning|error|success|debug)[[:space:]]+"[^"]*"[[:space:]]*$'
#
# A command substitution runs before the command it sits in, so a test or log
# line containing one can write while looking like a read:
# `log_info "$(cp x /usr/local/bin/x)"`. The only substitution setup-tools uses
# beside this path is `$(command -v <tool>)`, which is read-only; that one is
# stripped, and any other $() or backtick disqualifies the line.
safe_subst_re='\$\(command -v [[:alnum:]_-]+\)'

mapfile -t bin_refs < <(
    grep -nE '/usr/local/bin' "${SETUP_TOOLS}" | grep -vE '^[0-9]+:[[:space:]]*#' || true
)

bypasses=()
for entry in "${bin_refs[@]}"; do
    line_no="${entry%%:*}"
    code="${entry#*:}"
    if (( line_no > helper_start && line_no < helper_end )); then
        continue
    fi
    # Excepted only when every /usr/local/bin path the line names is an
    # exception, each compared whole. A substring test let `/usr/local/bin/yq`
    # vouch for `/usr/local/bin/yq-helper`, and let a trailing comment that
    # mentions an excepted path vouch for whatever the line actually installs.
    mapfile -t named_paths < <(grep -oE '/usr/local/bin/[[:alnum:]._-]+' <<< "${code}" || true)
    excepted=false
    if (( ${#named_paths[@]} > 0 )); then
        excepted=true
        for named in "${named_paths[@]}"; do
            listed=false
            for exception in "${BIN_INSTALL_EXCEPTIONS[@]}"; do
                [[ "${named}" == "${exception}" ]] && listed=true
            done
            "${listed}" || { excepted=false; break; }
        done
    fi
    "${excepted}" && continue
    unsubstituted="${code}"
    while [[ "${unsubstituted}" =~ ${safe_subst_re} ]]; do
        unsubstituted="${unsubstituted/"${BASH_REMATCH[0]}"/}"
    done
    if [[ "${code}" != *sudo* && "${unsubstituted}" != *'$('* && "${unsubstituted}" != *'`'* ]] \
        && [[ "${code}" =~ ${read_test_re} || "${code}" =~ ${read_log_re} ]]; then
        continue
    fi
    bypasses+=("${entry}")
done

if [[ "${#bypasses[@]}" -ne 0 ]]; then
    echo "FAIL: ${#bypasses[@]} line(s) write to /usr/local/bin outside install_release_binary." >&2
    echo "      Each one can land a binary with no checksum comparison. Route it through" >&2
    echo "      install_release_binary, which will not run without a checksum source, or" >&2
    echo "      add it to BIN_INSTALL_EXCEPTIONS with the reason it cannot be." >&2
    printf '        %s\n' "${bypasses[@]}" >&2
    exit 1
fi

# Assert the installs BY BINARY, for the reason the module-tool check above
# does: the bypass check alone passes vacuously if an install is deleted.
REQUIRED_RELEASE_BINARIES=(
    addlicense chainsaw cosign crane ctlptl flux git-cliff goreleaser hauler
    helm helmfile kind ko kubectl mkcert oasdiff oras syft tilt zarf
)

# Join backslash-continued lines first. The binary argument usually sits on a
# continuation line, so matching the first line of each call alone misses it --
# and passes only for the calls where the name happens to appear up front.
mapfile -t helper_calls < <(
    awk '{ if (sub(/\\$/, "")) { buf = buf $0; next } print buf $0; buf = "" }' "${SETUP_TOOLS}" \
        | grep -E '(^|[[:space:]])install_release_binary[[:space:]]' \
        | grep -vE '^[[:space:]]*#' || true
)

missing_binaries=0
for binary in "${REQUIRED_RELEASE_BINARIES[@]}"; do
    # Match the binary as the bare argument that precedes the quoted
    # description, not as any occurrence of the name. The description always
    # contains the tool's name ("ko ${KO_VERSION} for Linux"), so a looser match
    # lets it vouch for a call whose binary argument is gone. Anchoring on the
    # trailing `"` also keeps "helm" from vouching for "helmfile".
    if ! printf '%s\n' "${helper_calls[@]}" | grep -qE "[[:space:]]${binary}[[:space:]]+\""; then
        echo "FAIL: no install_release_binary call installs ${binary}." >&2
        missing_binaries=1
    fi
done
[[ "${missing_binaries}" -eq 0 ]] || exit 1

echo "Every write to /usr/local/bin is accounted for: ${#REQUIRED_RELEASE_BINARIES[@]} checksum-verified through install_release_binary, ${#BIN_INSTALL_EXCEPTIONS[@]} named exceptions"

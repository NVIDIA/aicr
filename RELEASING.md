# Release Process

This document describes when, why, and how AICR releases are made. For contribution guidelines, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Cadence

Releases follow a **bi-weekly cadence**. A new release is cut every two weeks.

| Release Type | When | Version Bump | Decision |
|-------------|------|-------------|----------|
| Regular release | Every two weeks | `patch` or `minor` | Maintainer determines bump type based on changes landed |
| Hotfix | Between regular releases, as needed | `patch` | Any maintainer can initiate for critical fixes |
| Pre-release | Before a regular release, as needed | `rc` | Any maintainer can create for testing |
| Major | Planned | `major` | Requires team agreement and advance communication |

## What Goes Into a Release

A release includes everything merged to `main` since the last tag. There is no cherry-picking or feature branching for releases — if it's on `main`, it ships.

**Before cutting a release, verify:**

- All CI checks pass on `main` (`make qualify`)
- No known regressions since the last release
- Breaking changes use `feat!:` or `fix!:` commit prefix (drives changelog and signals consumers)

## Quality Gates

Every release must pass these automated gates before artifacts are published:

- Unit tests with race detector
- golangci-lint + yamllint
- License header verification
- Vulnerability scans (Anchore in release workflows, Grype in `make scan`)
- E2E tests on Kind cluster
- Per-platform vulnerability scans of the exact candidate image digests
- SLSA Build Level 3 provenance for those same digests

Container builds initially publish only a run-unique
`candidate-<run-id>-<run-attempt>` tag. Version aliases, stable `latest`
aliases, and the public GitHub release remain unchanged until all seven
candidate digests pass their gates. Homebrew publication starts only after
the GitHub release is public. If any gate fails, the candidate tags remain
available for diagnosis but are not promoted to public aliases.

## How to Release

### Standard Release (recommended)

```bash
git checkout main
git pull origin main
make qualify          # Verify locally before releasing

make bump-patch       # v1.2.3 → v1.2.4
# or
make bump-minor       # v1.2.3 → v1.3.0
```

This validates clean state, tags the current HEAD, pushes the tag, and triggers the release pipeline. No commits are created — the tag points directly at the code.

Use `make changelog` to preview changes since the last tag. The changelog is generated for GitHub Release notes and is not committed to the repository.

### Pre-release with Promotion (recommended for important releases)

Use this workflow to validate an RC before promoting it to stable. The promotion re-tags the exact same SHA — no new commits, no re-builds.

```bash
git checkout main
git pull origin main
make qualify

# 1. Tag an RC (bumps minor version)
make bump-rc                         # v1.2.3 → v1.3.0-rc1

# 2. Validate the RC (CI runs, manual testing, etc.)

# 3a. If issues found, fix on main and cut another RC
make bump-rc                         # v1.3.0-rc1 → v1.3.0-rc2

# 3b. When satisfied, promote the RC to stable (same SHA)
make bump-promote TAG=v1.3.0-rc2    # → v1.3.0 on same commit
```

Pre-releases exercise the full build/test/scan/attest pipeline. After those
gates pass, their version aliases are promoted to the exact candidate digests,
but they do not update:

- Homebrew formula (users on `brew upgrade` are unaffected)
- Container `:latest` tags (only candidate and version aliases are written)
- Demo deployment (Cloud Run stays on latest stable)
- Site documentation (GitHub Pages stays on latest stable)

Slack notifications fire for both pre-releases and stable releases.

### Re-run Existing Release

Use **Re-run failed jobs** to recover a transient failure. Successful upstream
jobs retain the candidate tag emitted by `detect`, so promotion converges from
the same digest set. This is the required recovery path after a partial
cross-repository alias promotion. If GoReleaser left a partial exact-tag draft,
the rerun reuses it only when its name and tag both equal the release tag, its
pre-release state matches the tag, and every existing asset belongs to the
fixed 13-asset GoReleaser set. Expected assets from the partial attempt are
replaced, missing assets are uploaded, and release notes are regenerated from
the current tag. Unexpected, duplicate, or malformed assets fail closed and
require maintainer inspection instead of automatic deletion. The generated
Homebrew formula is retained for GitHub's full 30-day workflow-rerun window.

**Re-run all jobs** creates a new run attempt and therefore a new candidate
tag. Use it only before any public alias moved, or when rebuilding is
intentional. If an immutable version alias already points at a different
digest, preflight fails rather than overwriting it. If `detect` itself failed,
re-running it also creates the current attempt's new candidate tag. Once the
exact-tag GitHub release is public, the build fails closed instead of modifying
its assets; cut a new tag for any further release. Publication revalidates the
tag commit and exact 13-asset set, then publishes the validated numeric release
ID. It never resolves the draft by a mutable display name or tag at the write
step. If GitHub made that exact release public but its response was lost, a
failed-job rerun accepts it only after the same source, identity, pre-release
state, and exact asset set are revalidated; it does not publish a second time.

## Hotfix Procedure

For critical fixes between regular releases:

1. Fix on `main` first (PR, review, merge as normal)
2. Cut a patch release: `make bump-patch`
3. For patching older release lines (rare): cherry-pick from `main` onto a hotfix branch, tag manually

## Release Pipeline

```
Tag Push --> CI --> Candidate Images --> Resolve Digests --> Scan + Attest --> Promote Aliases --> Publish --> Deploy
```

The release workflow resolves one authoritative seven-image digest map. Both
architectures of each digest are scanned, and provenance plus platform-specific
SBOMs are generated before promotion. A read-only preflight checks every
candidate, attestation, existing version alias, and stable `latest` alias
before the first registry write. Stable releases also fail closed if the same
or a newer stable version is already public, even if registry aliases were
changed out of band.

Promotion first creates and verifies all seven immutable version aliases. Only
then does a stable release begin updating `latest`. Promotion across seven GHCR
repositories is not transactional, so a registry failure in the second phase
can leave a mix of immediate-prior and current-candidate `latest` aliases.
Re-running the failed jobs with the same candidate is idempotent and finishes
only the remaining aliases. The repository-global concurrency group prevents
simultaneous promotion jobs, but GitHub Actions retains at most one pending run
and may replace an older pending run; operators must confirm the surviving run
belongs to the intended release before retrying.

Candidate and per-architecture candidate tags are intentionally retained.
Automated cleanup is deferred until shared-manifest deletion behavior and
package-storage growth have a separately reviewed policy.

## Released Artifacts

### Binaries

Built via GoReleaser for multiple platforms:

| Binary | Platforms | Description |
|--------|-----------|-------------|
| `aicr` | darwin/amd64, darwin/arm64, linux/amd64, linux/arm64 | CLI tool |
| `aicrd` | linux/amd64, linux/arm64 | API server |

### Container Images

Published to GitHub Container Registry (`ghcr.io/nvidia/`):

| Image | Base | Description |
|-------|------|-------------|
| `aicr` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Pure-Go CLI/agent (driver-free GPU discovery) |
| `aicrd` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Minimal API server |
| `aicr-gate` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Bundle readiness-gate Job image (emitted by `aicr bundle --readiness-hooks`) |

Published to GitHub Container Registry (`ghcr.io/nvidia/aicr-validators/`):

| Image | Base | Description |
|-------|------|-------------|
| `deployment` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Deployment validator |
| `performance` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Performance validator |
| `conformance` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Conformance validator |
| `aiperf-bench` | `nvcr.io/nvidia/distroless/python:3.13-v4.0.8` | AIPerf benchmark runner (built from `python:3.13-slim`) |

Stable releases promote `vX.Y.Z` and `latest`; prereleases promote their
`vX.Y.Z-rcN` version tags but never `latest`. The release workflow also retains
non-promoted `candidate-<run-id>-<run-attempt>` tags in the public GHCR packages
for audit, diagnosis, and recovery.

### Supply Chain

Every release includes:

- **SLSA Build Level 3 Provenance** — verifiable image build attestations (provenance v1), generated from a reusable workflow
- **SBOM** — Software Bill of Materials (SPDX format)
- **Sigstore Signatures** — keyless signing via Fulcio + Rekor
- **Checksums** — SHA256 for all binaries
- **Third-party notices** — `THIRD_PARTY_NOTICES.md` listing every
  third-party dependency AICR redistributes and embedding the verbatim
  text of each license-bearing file shipped upstream (e.g. `LICENSE`,
  `NOTICE`) where available (generated by `make notices`; uploaded as a
  top-level GitHub release asset). It covers two surfaces: Go modules
  linked into the released binaries (collected via `go-licenses` from
  `vendor/`), and the Python packages installed into the released
  `aiperf-bench` image (collected out-of-band by `make python-licenses`,
  which needs network access to PyPI, then committed as a rendered
  fragment so `make notices` itself stays offline). The Go half
  is the union of the dependency graph across every released OS/arch
  target, generated deterministically so it is byte-identical on macOS and
  Linux; the `notices-freshness` merge-gate job fails any PR whose
  dependency changes leave the committed file stale (run `make notices`
  and commit)

## Versioning

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1` (automatically marked in GitHub)
- **Breaking changes**: Increment MAJOR version

## Verification

### Container Attestations

Verify the **digest-pinned** image that a tag currently resolves to. Tag refs
are registry-rewritable; attestations bind to digests. Requires `crane` (or
substitute `docker buildx imagetools inspect` for digest resolution).

Predicate types attach at two different levels of the image index, so the
digest you verify against depends on what you are asking for:

| Predicate | Attached to | Verify against |
|-----------|-------------|----------------|
| SLSA provenance (`slsaprovenance1`) | multi-arch index | `crane digest <image>:<tag>` |
| OpenVEX (`openvex`) | multi-arch index | `crane digest <image>:<tag>` |
| SBOM (`spdxjson`) | per-platform child manifest | `crane digest --platform <os>/<arch> <image>:<tag>` |

Asking for `spdxjson` against the index digest fails with `none of the
attestations matched the predicate type`.

```bash
set -euo pipefail
TAG=$(gh release view --repo NVIDIA/aicr --json tagName -q .tagName)
[[ -n "${TAG}" ]] || { echo "failed to resolve latest TAG" >&2; exit 1; }

# Resolve immutable digests up front so a missing image / crane failure
# aborts here (set -e) instead of being attributed to a later gh/cosign step.
AICR_INDEX=$(crane digest "ghcr.io/nvidia/aicr:${TAG}")
AICRD_INDEX=$(crane digest "ghcr.io/nvidia/aicrd:${TAG}")
GATE_INDEX=$(crane digest "ghcr.io/nvidia/aicr-gate:${TAG}")
DEPLOY_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/deployment:${TAG}")
PERF_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/performance:${TAG}")
CONF_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/conformance:${TAG}")
AIPERF_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/aiperf-bench:${TAG}")

# GitHub CLI (core images) — --source-ref binds the attestation to this tag
gh attestation verify "oci://ghcr.io/nvidia/aicr@${AICR_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicrd@${AICRD_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-gate@${GATE_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# GitHub CLI (validator images)
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/deployment@${DEPLOY_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/performance@${PERF_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/conformance@${CONF_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/aiperf-bench@${AIPERF_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Cosign — provenance and OpenVEX are on the index. Pin the workflow *and*
# the exact tag ref (same binding as --source-ref above): without
# --certificate-github-workflow-ref, the identity regexp alone would accept
# an attestation signed for any release tag on a digest this tag was
# rewritten to point at.
IDENTITY='^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$'
for predicate in slsaprovenance1 openvex; do
  cosign verify-attestation \
    --type "${predicate}" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp "${IDENTITY}" \
    --certificate-github-workflow-ref "refs/tags/${TAG}" \
    "ghcr.io/nvidia/aicr@${AICR_INDEX}" >/dev/null
done

# Cosign — the SBOM is on the per-platform child manifest
platform="linux/$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
AICR_CHILD=$(crane digest --platform "${platform}" "ghcr.io/nvidia/aicr@${AICR_INDEX}")
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "${IDENTITY}" \
  --certificate-github-workflow-ref "refs/tags/${TAG}" \
  "ghcr.io/nvidia/aicr@${AICR_CHILD}" >/dev/null
```

### Binary Checksums

`aicr_checksums.txt` lists digests for release archives (and SBOMs). Download
the archive you intend to verify **and** the checksums file into the same
directory, assert the archive is present and non-empty, then check **that**
file’s line — do not use `--ignore-missing` (it can pass with zero files
verified). On macOS, use `shasum -a 256` (built-in); on Linux, `sha256sum`
(GNU coreutils).

```bash
set -euo pipefail
TAG=$(gh release view --repo NVIDIA/aicr --json tagName -q .tagName)
[[ -n "${TAG}" ]] || { echo "failed to resolve latest TAG" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
archive="aicr_${TAG#v}_${os}_${arch}.tar.gz"

tmpdir=$(mktemp -d)
trap 'rm -rf "${tmpdir}"' EXIT
gh release download "${TAG}" -R NVIDIA/aicr -D "${tmpdir}" \
  -p "aicr_checksums.txt" \
  -p "${archive}"

cd "${tmpdir}"
[[ -s "${archive}" ]] || { echo "missing or empty archive: ${archive}" >&2; exit 1; }
[[ -s aicr_checksums.txt ]] || { echo "missing aicr_checksums.txt" >&2; exit 1; }

# Fail closed: verify only the downloaded archive line from the checksums file.
line=$(grep -F "  ${archive}" aicr_checksums.txt) || {
  echo "no checksum entry for ${archive}" >&2
  exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
  printf '%s\n' "${line}" | sha256sum -c -
elif command -v shasum >/dev/null 2>&1; then
  printf '%s\n' "${line}" | shasum -a 256 -c -
else
  echo "need sha256sum (GNU coreutils) or shasum" >&2
  exit 1
fi
```

## Demo Deployment

> **Note**: Demonstration only — not a production service. Self-host `aicrd` for production use. See [API Server Documentation](docs/contributor/api-server.md).

The `aicrd` API server demo deploys to Google Cloud Run on successful release (region: `us-west1`, auth: Workload Identity Federation). Project-specific details are managed in CI configuration.

## Troubleshooting

| Problem | Action |
|---------|--------|
| Tests fail during release | Fix on `main`, cut new tag |
| Lint errors | Run `make lint` locally before releasing |
| Image push failure | Check GHCR permissions |
| Promotion partially completed | Re-run failed jobs for the same workflow run; do not repoint aliases manually |
| Version alias conflict | Stop and verify the existing digest; the workflow intentionally refuses overwrite |
| Draft identity or asset check fails | Inspect the exact-tag draft; correct the name/tag or remove only verified stale assets, then re-run failed jobs |
| Need a full rebuild | Re-run all jobs only before public aliases move; this creates a new candidate tag |

## Prerequisites

- Repository admin access with write permissions
- Access to GitHub Actions workflows
- [git-cliff](https://git-cliff.org/) installed for `make changelog` (`make tools-setup`)

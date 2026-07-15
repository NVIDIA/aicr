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
| `aicr` | `nvcr.io/nvidia/cuda:13.1.0-runtime-ubuntu24.04` | CLI with CUDA runtime |
| `aicrd` | `gcr.io/distroless/static:nonroot` | Minimal API server |

Published to GitHub Container Registry (`ghcr.io/nvidia/aicr-validators/`):

| Image | Base | Description |
|-------|------|-------------|
| `deployment` | `gcr.io/distroless/static-debian12:nonroot` | Deployment validator |
| `performance` | `gcr.io/distroless/static-debian12:nonroot` | Performance validator |
| `conformance` | `gcr.io/distroless/static-debian12:nonroot` | Conformance validator |
| `aiperf-bench` | `python:3.12-slim` | AIPerf benchmark runner |

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
  third-party Go dependency and embedding the verbatim text of each
  license-bearing file shipped by the upstream module (e.g. `LICENSE`,
  `NOTICE`) where available (generated by `make notices` via
  `go-licenses`; uploaded as a top-level GitHub release asset). The file
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

```bash
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')

# GitHub CLI (core images)
gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify oci://ghcr.io/nvidia/aicrd:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# GitHub CLI (validator images)
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/deployment:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/performance:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/conformance:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/aiperf-bench:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Cosign
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ghcr.io/nvidia/aicr:${TAG}
```

### Binary Checksums

```bash
curl -sL "https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_checksums.txt" -o checksums.txt
sha256sum -c checksums.txt --ignore-missing
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

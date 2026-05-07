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

If any gate fails, the release pipeline stops. Fix forward on `main` and cut a new tag.

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

Pre-releases exercise the full build/test/attest pipeline but do not update:

- Homebrew formula (users on `brew upgrade` are unaffected)
- Container `:latest` tags (only version-tagged images are pushed)
- Demo deployment (Cloud Run stays on latest stable)
- Site documentation (GitHub Pages stays on latest stable)

Slack notifications fire for both pre-releases and stable releases.

### Re-run Existing Release

To rebuild artifacts from an existing tag without creating a new one: **Actions** > **On Tag Release** > **Run workflow** > enter the tag.

## Hotfix Procedure

For critical fixes between regular releases:

1. Fix on `main` first (PR, review, merge as normal)
2. Cut a patch release: `make bump-patch`
3. For patching older release lines (rare): cherry-pick from `main` onto a hotfix branch, tag manually

## Release Pipeline

```
Tag Push --> CI (tests + lint) --> Build (binaries + images) --> Attest (SBOM + provenance) --> Deploy (demo)
```

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

Tags: `latest`, `vX.Y.Z`

### Supply Chain

Every release includes:

- **SLSA Build Level 3 Provenance** — verifiable build attestations
- **SBOM** — Software Bill of Materials (SPDX format)
- **Sigstore Signatures** — keyless signing via Fulcio + Rekor
- **Checksums** — SHA256 for all binaries
- **Third-party notices** — `THIRD_PARTY_NOTICES.md` listing every
  third-party Go dependency and embedding the verbatim text of each
  license-bearing file shipped by the upstream module (e.g. `LICENSE`,
  `NOTICE`) where available (generated by `make notices` via
  `go-licenses`; uploaded as a top-level GitHub release asset)

## Versioning

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1` (automatically marked in GitHub)
- **Breaking changes**: Increment MAJOR version

## Verification

### Container Attestations

```bash
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')

# GitHub CLI (core images)
gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --owner nvidia
gh attestation verify oci://ghcr.io/nvidia/aicrd:${TAG} --owner nvidia

# GitHub CLI (validator images)
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/deployment:${TAG} --owner nvidia
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/performance:${TAG} --owner nvidia
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/conformance:${TAG} --owner nvidia
gh attestation verify oci://ghcr.io/nvidia/aicr-validators/aiperf-bench:${TAG} --owner nvidia

# Cosign
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/.*' \
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
| Need to rebuild | Use manual workflow trigger with existing tag |

## Prerequisites

- Repository admin access with write permissions
- Access to GitHub Actions workflows
- [git-cliff](https://git-cliff.org/) installed for `make changelog` (`make tools-setup`)

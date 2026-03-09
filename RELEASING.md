# Release Process

This document describes when, why, and how AICR releases are made. For contribution guidelines, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Cadence

Releases follow a **bi-weekly cadence**, aligned with sprint boundaries. A new release is cut at the conclusion of each 2-week sprint.

| Release Type | When | Version Bump | Decision |
|-------------|------|-------------|----------|
| Sprint release | End of each 2-week sprint | `patch` or `minor` | Maintainer determines bump type based on changes landed |
| Hotfix | Between sprints, as needed | `patch` | Any maintainer can initiate for critical fixes |
| Major | Planned | `major` | Requires team agreement and advance communication |

## What Goes Into a Release

A release includes everything merged to `main` since the last tag. There is no cherry-picking or feature branching for releases — if it's on `main`, it ships.

**Before cutting a release, verify:**

- All CI checks pass on `main` (`make qualify`)
- No known regressions from the current sprint
- Breaking changes use `feat!:` or `fix!:` commit prefix (drives changelog and signals consumers)

## Quality Gates

Every release must pass these automated gates before artifacts are published:

- Unit tests with race detector
- golangci-lint + yamllint
- License header verification
- Vulnerability scans (Trivy in release workflows, Grype in `make scan`)
- E2E tests on Kind cluster

If any gate fails, the release pipeline stops. Fix forward on `main` and cut a new tag.

## How to Release

### Standard Release (recommended)

```bash
git checkout main
git pull origin main
make qualify          # Verify locally before releasing

make bump-patch       # v1.2.3 -> v1.2.4
# or
make bump-minor       # v1.2.3 -> v1.3.0
```

This automatically: validates clean state, generates changelog, commits, tags, pushes, and triggers the release pipeline.

### Manual Tag

For more control:

```bash
git checkout main && git pull origin main
make qualify
git-cliff --tag v1.2.3 -o CHANGELOG.md       # Optional: generate changelog
git add CHANGELOG.md && git commit -m "chore: update CHANGELOG for v1.2.3"
git tag -a v1.2.3 -m "Release v1.2.3"
git push origin main v1.2.3
```

### Re-run Existing Release

To rebuild artifacts from an existing tag without creating a new one: **Actions** > **On Tag Release** > **Run workflow** > enter the tag.

## Hotfix Procedure

For critical fixes between sprints:

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

Tags: `latest`, `vX.Y.Z`

### Supply Chain

Every release includes:

- **SLSA Build Level 3 Provenance** — verifiable build attestations
- **SBOM** — Software Bill of Materials (SPDX format)
- **Sigstore Signatures** — keyless signing via Fulcio + Rekor
- **Checksums** — SHA256 for all binaries

## Versioning

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1`, `v1.2.3-beta1` (automatically marked in GitHub)
- **Breaking changes**: Increment MAJOR version

## Verification

### Container Attestations

```bash
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')

# GitHub CLI
gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --owner nvidia
gh attestation verify oci://ghcr.io/nvidia/aicrd:${TAG} --owner nvidia

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
- [git-cliff](https://git-cliff.org/) installed (`make tools-setup`)

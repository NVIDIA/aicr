---
title: "Release Process"
linkTitle: "Releasing"
weight: 30
description: "Release process and versioning"
---


This document outlines the release process for NVIDIA AI Cluster Runtime (AICR). For contribution guidelines, see [CONTRIBUTING.md](/docs/project/contributing/).

## Prerequisites

- Repository admin access with write permissions
- Understanding of semantic versioning (vMAJOR.MINOR.PATCH)
- Access to GitHub Actions workflows
- [git-cliff](https://git-cliff.org/) installed (run `make tools-setup` to install)

## Release Methods

### Method 1: Version Bump (Recommended)

Use Makefile targets for standard releases:

```bash
make bump-patch   # v1.2.3 → v1.2.4
make bump-minor   # v1.2.3 → v1.3.0
make bump-major   # v1.2.3 → v2.0.0
```

**What happens automatically**:

1. Validates working directory is clean with no unpushed commits
2. Calculates the new version based on bump type
3. Generates/updates `CHANGELOG.md` using [git-cliff](https://git-cliff.org/)
4. Commits the changelog update
5. Creates an annotated tag
6. Pushes both commit and tag to origin
7. Triggers release workflows (see [Workflow Pipeline](#workflow-pipeline))

**Note**: Manual edits to `CHANGELOG.md` (e.g., corrections to previous releases) are preserved. The bump process prepends new entries without overwriting existing content.

### Method 2: Manual Tag (Advanced)

For cases where you need more control over the release process:

1. **Ensure main is ready**:
   ```bash
   git checkout main
   git pull origin main
   make qualify  # All checks must pass
   ```

2. **Generate changelog manually** (optional):
   ```bash
   git-cliff --tag v1.2.3 -o CHANGELOG.md
   git add CHANGELOG.md
   git commit -m "chore: update CHANGELOG for v1.2.3"
   git push origin main
   ```

3. **Create and push a version tag**:
   ```bash
   git tag -a v1.2.3 -m "Release v1.2.3"
   git push origin v1.2.3
   ```

4. **Automatic workflows trigger** (via `on-tag.yaml`)

### Method 3: Manual Workflow Trigger

For rebuilding from existing tags or emergency releases:

1. Navigate to **Actions** → **On Tag Release**
2. Click **Run workflow**
3. Enter the existing tag (e.g., `v1.2.3`)
4. Click **Run workflow**

This is useful when you need to re-run the release pipeline without creating a new tag.

## Workflow Pipeline

```
┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
│ Tag Push │───▶│  Go CI   │───▶│  Build   │───▶│  Attest  │───▶│  Deploy  │
└──────────┘    └──────────┘    └──────────┘    └──────────┘    └──────────┘
                  tests +         binaries +      SBOM +          Demo Deploy
                  lint            images          provenance      (example)
```

## Released Components

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

Tags: `latest`, `v1.2.3`

### Supply Chain Artifacts

Every release includes:

- **SLSA Build Level 3 Provenance**: Verifiable build attestations
- **SBOM**: Software Bill of Materials (SPDX format)
- **Sigstore Signatures**: Keyless signing via Fulcio + Rekor
- **Checksums**: SHA256 checksums for all binaries

## Quality Gates

All releases must pass:

- **Unit tests**: With race detector enabled
- **Linting**: golangci-lint + yamllint
- **License headers**: All source files verified
- **Security scans**: Trivy in release workflows, Grype in `make scan`

## Verification

### Verify Container Attestations

```bash
# Get latest release tag
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')

# Verify with GitHub CLI (recommended)
gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --owner nvidia
gh attestation verify oci://ghcr.io/nvidia/aicrd:${TAG} --owner nvidia

# Verify with Cosign
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/.*' \
  ghcr.io/nvidia/aicr:${TAG}
```

### Verify Binary Checksums

```bash
# Download checksums file from GitHub Release
curl -sL "https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_checksums.txt" -o checksums.txt

# Verify downloaded binary
sha256sum -c checksums.txt --ignore-missing
```

### Pull and Test Images

```bash
# Pull container images
docker pull ghcr.io/nvidia/aicr:${TAG}
docker pull ghcr.io/nvidia/aicrd:${TAG}

# Test CLI
docker run --rm ghcr.io/nvidia/aicr:${TAG} --version

# Test API server
docker run --rm -p 8080:8080 ghcr.io/nvidia/aicrd:${TAG} &
curl http://localhost:8080/health
```

## Version Management

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1`, `v1.2.3-beta1` (automatically marked in GitHub)
- **Breaking changes**: Increment MAJOR version

## Demo Cloud Run Deployment

> **Note**: This is a **demonstration deployment** for testing and development purposes only. It is not a production service. Users should self-host the `aicrd` API server in their own infrastructure for production use. See [API Server Documentation](/docs/contributor/api-server/) for deployment guidance.

The `aicrd` API server demo is automatically deployed to Google Cloud Run on successful release:

- **Project**: configured in CI/CD deployment settings
- **Region**: `us-west1`
- **Service**: `api`
- **Authentication**: Workload Identity Federation (keyless)

This demo deployment only occurs if the build step succeeds and serves as an example of how to deploy the API server.

## Troubleshooting

### Failed Release

1. Check **Actions** → **On Tag Release** for error logs
2. Common issues:
   - Tests failing: Fix and create new tag
   - Lint errors: Run `make lint` locally first
   - Image push failures: Check GHCR permissions

### Rebuild Existing Release

Use manual workflow trigger with the existing tag. No need to delete and recreate tags.

## Emergency Hotfix Procedure

For urgent fixes:

1. **Fix in main first**:
   ```bash
   git checkout main
   git checkout -b fix/critical-issue
   # Apply fix, create PR to main, merge
   ```

2. **Create hotfix release**:
   ```bash
   git checkout main
   git pull origin main
   make bump-patch  # Generates changelog, tags, and pushes
   ```

3. **For patching older releases** (rare):
   ```bash
   git checkout v1.2.3
   git checkout -b hotfix/v1.2.4
   git cherry-pick <commit-hash-from-main>
   git-cliff --tag v1.2.4 --unreleased --prepend CHANGELOG.md
   git add CHANGELOG.md
   git commit -m "chore: update CHANGELOG for v1.2.4"
   git tag -a v1.2.4 -m "Release v1.2.4"
   git push origin hotfix/v1.2.4 v1.2.4
   ```

## Release Checklist

Before running `make bump-*`:

- [ ] All CI checks pass on main (`make qualify`)
- [ ] Working directory is clean (no uncommitted changes)
- [ ] All commits are pushed to origin
- [ ] Breaking changes documented in commit messages (use `feat!:` or `fix!:` prefix)
- [ ] Version bump type is correct (major for breaking, minor for features, patch for fixes)

After release:

- [ ] GitHub Release created with changelog
- [ ] Container images available in GHCR
- [ ] Attestations verifiable
- [ ] Demo Cloud Run deployment successful (optional)
- [ ] Announce release (if applicable)

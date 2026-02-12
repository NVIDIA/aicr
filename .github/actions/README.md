# GitHub Actions Architecture

This directory contains a modular, reusable GitHub Actions architecture optimized for separation of concerns and composability.

## Composite Actions

### Core CI/CD Actions

#### `go-ci/`
**Purpose**: Complete Go project CI pipeline (setup, test, lint)
**When to use**: Every workflow that needs to validate Go code
**Inputs**:
- `go_version` (required): Go version (e.g., "1.25")
- `golangci_lint_version` (required): golangci-lint version (e.g., "v2.6")
- `addlicense_version` (optional): addlicense version (default: "v1.1.1")
- `coverage_report` (optional): Whether to generate GitHub-native coverage report (default: "false")
- `coverage_threshold` (optional): Minimum coverage percentage required (default: "70")

**Example**:
```yaml
- uses: ./.github/actions/go-ci
  with:
    go_version: '1.25'
    golangci_lint_version: 'v2.6'
    coverage_report: 'true'
    coverage_threshold: '70'
```

#### `security-scan/`
**Purpose**: Trivy vulnerability scanning with SARIF upload  
**When to use**: Security validation in CI/CD pipelines  
**Inputs**:
- `scan_type` (optional): Scan type (default: "fs")
- `scan_ref` (optional): Scan target (default: ".")
- `severity` (optional): Severity levels (default: "HIGH,CRITICAL")
- `output_file` (optional): SARIF file name (default: "trivy-results.sarif")
- `category` (optional): GitHub Security category (default: "trivy")
- `skip_dirs` (optional): Directories to skip (default: "")

**Example**:
```yaml
- uses: ./.github/actions/security-scan
  with:
    severity: 'MEDIUM,HIGH,CRITICAL'
    category: 'trivy-fs'
```

### Development Environment Actions

#### `install-e2e-tools/`
**Purpose**: Install development and E2E testing tools using the shared `tools/setup-tools` script
**When to use**: E2E test workflows that need development tools (kubectl, kind, tilt, etc.)
**Key Features**:
- Uses `tools/setup-tools` for consistency with local development
- Caches tools based on `.versions.yaml` hash
- Same tools, same versions as local dev - no "works on my machine" issues

**Example**:
```yaml
- uses: ./.github/actions/install-e2e-tools
```

This action runs `tools/setup-tools --skip-go --skip-docker` in auto mode, which:
- Reads versions from `.versions.yaml` (single source of truth)
- Installs: helm, kubectl, kind, ctlptl, tilt, ko, grype, yamllint, golangci-lint
- Skips Go (handled by `actions/setup-go`) and Docker (pre-installed on runners)
- Uses the same installation logic as local development

#### `load-versions/`
**Purpose**: Load tool versions from `.versions.yaml` as workflow outputs
**When to use**: When you need version values in workflow steps
**Outputs**:
- `go`, `goreleaser`, `ko`, `crane`, `golangci_lint`, `yamllint`, `addlicense`
- `grype`, `kubectl`, `kind`, `ctlptl`, `tilt`, `helm`

**Example**:
```yaml
- uses: ./.github/actions/load-versions
  id: versions
- uses: actions/setup-go@7a3fe6cf4cb3a834922a1244abfce67bcef6a0c5  # v6.2.0
  with:
    go-version: ${{ steps.versions.outputs.go }}
```

### Build & Release Actions

#### `setup-build-tools/`
**Purpose**: Install container build tools (ko, syft, crane, goreleaser)  
**When to use**: When you need specific build tools without full build pipeline  
**Inputs**:
- `install_ko` (optional): Install ko (default: "false")
- `install_syft` (optional): Install syft (default: "false")
- `install_crane` (optional): Install crane (default: "false")
- `crane_version` (optional): crane version (default: "v0.20.6")
- `install_goreleaser` (optional): Install goreleaser (default: "false")

**Example**:
```yaml
- uses: ./.github/actions/setup-build-tools
  with:
    install_ko: 'true'
    install_crane: 'true'
    crane_version: 'v0.20.6'
```

#### `go-build-release/`
**Purpose**: Complete build and release pipeline (tools + auth + make release)
**When to use**: Release workflows that build and publish artifacts
**Inputs**:
- `registry` (optional): Container registry (default: "ghcr.io")

**Outputs**:
- `release_outcome`: Release step outcome (success/failure)

**Note**: Image repository paths are fully specified in `.goreleaser.yaml` under `kos.repositories`.

**Example**:
```yaml
- uses: ./.github/actions/go-build-release
  id: release
- if: steps.release.outputs.release_outcome == 'success'
  run: echo "Release succeeded"
```

### Attestation Actions

#### `ghcr-login/`
**Purpose**: Authenticate to GitHub Container Registry  
**When to use**: Before any GHCR operations (shared authentication)  
**Inputs**:
- `registry` (optional): Registry URL (default: "ghcr.io")
- `username` (optional): Username (default: github.actor)

**Example**:
```yaml
- uses: ./.github/actions/ghcr-login
```

#### `attest-image-from-tag/`
**Purpose**: Resolve digest from tag and generate SBOM + provenance  
**When to use**: Attesting images by tag (typical release workflow)  
**Inputs**:
- `image_name` (required): Full image name without tag (e.g., "ghcr.io/org/image")
- `tag` (required): Image tag (e.g., "v1.2.3")
- `crane_version` (optional): crane version (default: "v0.20.6")

**Outputs**:
- `image_digest`: Resolved sha256 digest

**Example**:
```yaml
- uses: ./.github/actions/attest-image-from-tag
  with:
    image_name: ghcr.io/${{ github.repository_owner }}/my-app
    tag: ${{ github.ref_name }}
```

#### `sbom-and-attest/`
**Purpose**: Generate SBOM and attestations for image with known digest  
**When to use**: When you already have the digest (e.g., from build output)  
**Inputs**:
- `image_name` (required): Full image name
- `image_digest` (required): sha256 digest

**Example**:
```yaml
- uses: ./.github/actions/sbom-and-attest
  with:
    image_name: ghcr.io/org/image
    image_digest: sha256:abc123...
```

### KWOK Testing Actions

#### `kwok-test/`
**Purpose**: Test recipes using KWOK simulated nodes in a shared Kind cluster
**When to use**: KWOK recipe validation in CI or manual workflow dispatch
**Inputs**:
- `recipe` (optional): Recipe name to test (empty = all testable recipes)
- `go_version` (required): Go version to install
- `kind_version` (optional): Kind version (default: "0.31.0")
- `helm_version` (optional): Helm version (default: "v4.1.0")
- `kwok_version` (optional): KWOK version (default: "v0.7.0")
- `kubectl_version` (optional): kubectl version (default: "v1.35.0")

**Key Design**: Calls `run-all-recipes.sh` — the same script used by `make kwok-test-all` locally. This ensures CI and local testing use identical code paths with a single shared cluster.

**Example**:
```yaml
- uses: ./.github/actions/kwok-test
  with:
    go_version: ${{ steps.versions.outputs.go }}
    kind_version: ${{ steps.versions.outputs.kind }}
    helm_version: ${{ steps.versions.outputs.helm }}
```

### Deployment Actions

#### `cloud-run-deploy/`
**Purpose**: Copy image from GHCR to Artifact Registry and deploy to Cloud Run
**When to use**: Cloud Run deployments from CI/CD
**Inputs**:
- `project_id` (required): GCP project ID
- `workload_identity_provider` (required): WIF provider resource name
- `service_account` (required): Service account email
- `region` (required): Cloud Run region
- `service` (required): Cloud Run service name
- `source_image` (required): Source image to copy (e.g., "ghcr.io/nvidia/eidosd:v1.0.0")
- `target_registry` (required): Target Artifact Registry path (e.g., "us-docker.pkg.dev/project/repo")
- `image_name` (optional): Image name in target registry (default: "eidosd")
- `ghcr_token` (required): GitHub token for GHCR authentication (use `github.token`)

**Flow**: GHCR → Artifact Registry → Cloud Run

**Example**:
```yaml
- uses: ./.github/actions/cloud-run-deploy
  with:
    project_id: 'eidosx'
    workload_identity_provider: 'projects/.../providers/github-actions-provider'
    service_account: 'github-actions@eidosx.iam.gserviceaccount.com'
    region: 'us-west1'
    service: 'api'
    source_image: 'ghcr.io/nvidia/eidosd:v1.0.0'
    target_registry: 'us-docker.pkg.dev/eidosx/demo'
    image_name: 'eidosd'
    ghcr_token: ${{ github.token }}
```

## Workflows

### `on-push.yaml`
**Trigger**: Push to main, PRs to main
**Purpose**: CI validation
**Jobs** (run in parallel):
1. **Unit Tests**: Go CI (setup, test, lint) + security scan
2. **Integration Tests**: Chainsaw CLI integration tests via `tools/e2e`
3. **E2E Tests**: Full end-to-end tests using Kind cluster (via `.github/actions/e2e`)

### `on-tag.yaml`
**Trigger**: Semantic version tags (v*.*.*)
**Purpose**: Build, release, attest, deploy
**Jobs**:
1. **Unit Tests** (parallel): Go CI + security scan
2. **Integration Tests** (parallel): CLI integration tests
3. **E2E Tests** (parallel): Full end-to-end tests
4. **Build and Release** (after tests): GoReleaser builds binaries and images to GHCR
5. **Attest Images** (after build): SBOM and provenance for eidos and eidosd images
6. **Deploy Demo API Server** (after attest): Copy image to Artifact Registry and deploy demo to Cloud Run (example deployment)

### `test-deploy.yaml`
**Trigger**: Manual (workflow_dispatch)
**Purpose**: Isolated testing of the deploy action
**Inputs**:
- `image_tag`: Image tag to deploy (e.g., "v0.1.5")

### `kwok-recipes.yaml`
**Trigger**: Push/PR to main (when `pkg/recipe/data/**` or `kwok/**` change), manual dispatch
**Purpose**: KWOK simulated cluster validation of recipe scheduling
**Jobs**:
1. **Test**: Calls `kwok-test` action which runs `run-all-recipes.sh` (same as `make kwok-test-all`)
2. **Summary**: Reports pass/fail

## Architecture Principles

### Separation of Concerns
- **Single Responsibility**: Each action does one thing well
- **Composability**: Actions can be combined for complex workflows
- **Testability**: Small actions are easier to test in isolation

### Reusability Layers
1. **Primitive Actions**: Low-level operations (ghcr-login, setup-build-tools)
2. **Composed Actions**: Combine primitives (attest-image-from-tag = login + crane + sbom-and-attest)
3. **Pipeline Actions**: Full workflows (go-build-release = tools + auth + release)

### Authentication Strategy
- GHCR authentication centralized in `ghcr-login` action
- All actions requiring registry access use this shared action
- Eliminates redundant login steps (was happening 3x in on-tag workflow)

### Tool Installation Strategy
- **Development tools**: Use `install-e2e-tools` which delegates to `tools/setup-tools`
  - Same script used locally and in CI - guaranteed consistency
  - Versions managed in `.versions.yaml` (single source of truth)
  - `make tools-check` works identically in both environments
- **Build tools**: Use `setup-build-tools` for selective installation of ko, syft, crane, goreleaser
- Version pinning ensures reproducibility across all environments

## Migration from Previous Architecture

### Removed Redundancies
- **Before**: 3 separate GHCR logins (attest-image-from-tag, sbom-and-attest, workflow)
- **After**: Single `ghcr-login` action reused everywhere

- **Before**: Inline Trivy scan + upload steps in workflow
- **After**: Reusable `security-scan` action

- **Before**: 4 separate tool installations in workflow (ko, syft, crane, goreleaser)
- **After**: Single `go-build-release` or selective `setup-build-tools`

### Benefits
- **Less Code**: ~40% reduction in workflow YAML
- **Better Reuse**: Actions portable to other repos/workflows
- **Clearer Intent**: Pipeline steps self-document through action names
- **Easier Testing**: Individual actions can be tested independently
- **Version Management**: Tool versions centralized in action defaults

## Adding New Workflows

### For a simple CI workflow
```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
      - uses: ./.github/actions/load-versions
        id: versions
      - uses: ./.github/actions/go-ci
        with:
          go_version: ${{ steps.versions.outputs.go }}
          golangci_lint_version: ${{ steps.versions.outputs.golangci_lint }}
          coverage_report: 'true'
      - uses: ./.github/actions/security-scan
```

### For a release workflow with attestations
```yaml
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
      - uses: ./.github/actions/load-versions
        id: versions
      - uses: ./.github/actions/go-ci
        with:
          go_version: ${{ steps.versions.outputs.go }}
          golangci_lint_version: ${{ steps.versions.outputs.golangci_lint }}
      - uses: ./.github/actions/go-build-release
        id: release
      - uses: ./.github/actions/attest-image-from-tag
        with:
          image_name: ghcr.io/nvidia/eidosd
          tag: ${{ github.ref_name }}
          crane_version: ${{ steps.versions.outputs.crane }}
```

### For custom tool combinations
```yaml
steps:
  - uses: ./.github/actions/setup-build-tools
    with:
      install_crane: 'true'
      install_ko: 'true'
  - run: |
      ko build ./cmd/my-app
      crane digest ghcr.io/org/my-app:latest
```

## Local/CI Consistency

The `install-e2e-tools` action ensures that CI uses the exact same tool installation logic as local development:

```
┌─────────────────────┐     ┌─────────────────────┐
│   Local Dev         │     │   GitHub Actions    │
│                     │     │                     │
│ make tools-setup    │     │ install-e2e-tools   │
│        │            │     │        │            │
│        ▼            │     │        ▼            │
│ tools/setup-tools   │◄───►│ tools/setup-tools   │
│        │            │     │        │            │
│        ▼            │     │        ▼            │
│  .versions.yaml     │◄───►│  .versions.yaml     │
└─────────────────────┘     └─────────────────────┘
         │                           │
         └───────────────────────────┘
                Same versions, same tools
```

This eliminates "works on my machine" issues by ensuring:
- Same tool versions (from `.versions.yaml`)
- Same installation logic (`tools/setup-tools`)
- Same verification (`make tools-check`)

## Future Enhancements

### Potential Improvements
1. **Matrix Attestation Action**: Accept arrays of images to attest N images in one step
2. **Reusable Workflow**: For full "CI → release → attest → deploy" as a callable workflow
3. **Multi-Registry Support**: Extend ghcr-login to support DockerHub, ECR, GAR, etc.
4. **Parallel Attestations**: Run attestations concurrently for faster builds
5. **Notification Action**: Slack/Discord/PagerDuty notifications for workflow events

### Cross-Repo Reusability
To use these actions in other repositories:
```yaml
- uses: NVIDIA/eidos/.github/actions/go-ci@main
  with:
    go_version: '1.25'
    golangci_lint_version: 'v2.6.2'
    coverage_report: 'true'
```

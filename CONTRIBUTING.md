# Contributing to NVIDIA Eidos

Thank you for your interest in contributing to NVIDIA Eidos! We welcome contributions from developers of all backgrounds, experience levels, and disciplines.

## Quick Start (TL;DR)

```bash
# 1. Clone and setup
git clone https://github.com/NVIDIA/eidos.git && cd eidos
make tools-setup    # Install all required tools
make tools-check    # Verify versions match .versions.yaml

# 2. Develop
make test           # Run tests with race detector
make lint           # Run linters
make build          # Build binaries
make e2e            # Run integration tests

# 3. Before submitting PR
make qualify        # Full check: test + lint + scan (REQUIRED)
git commit -s -m "chore: your message"  # -s for DCO sign-off
```

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [How Can I Contribute?](#how-can-i-contribute)
- [Design Principles](#design-principles)
- [Development Setup](#development-setup)
- [Project Architecture](#project-architecture)
- [Development Workflow](#development-workflow)
- [Pull Request Process](#pull-request-process)
- [Developer Certificate of Origin](#developer-certificate-of-origin)
- [Tips for Contributors](#tips-for-contributors)
- [Additional Resources](#additional-resources)

## Code of Conduct

This project follows NVIDIA's commitment to fostering an open and welcoming environment. Please be respectful and professional in all interactions.

## How Can I Contribute?

### Reporting Bugs

- Describe the issue clearly, including steps to reproduce
- Include relevant system information (OS, Go version, hardware)
- Attach logs or screenshots if applicable
- Check if the issue already exists before creating a new one

### Suggesting Enhancements

- Open an issue with the "enhancement" label
- Clearly describe the proposed feature and its use case
- Explain how it benefits the project and users
- Provide examples or mockups if applicable

### Improving Documentation

- Fix typos, clarify instructions, or add examples
- Update README.md for user-facing changes
- Update API documentation when endpoints change

### Contributing Code

- Fix bugs, add features, or improve performance
- Follow the development workflow outlined below
- Ensure all tests pass and code meets quality standards

## Design Principles

These principles guide all design decisions in Eidos. When faced with trade-offs, these principles take precedence.

### Local Development Equals CI

The same tools, same versions, and same validation run locally and in CI.

**What:** Tool versions are centralized in `.versions.yaml`. Both `make tools-setup` (local) and GitHub Actions use this single source of truth. `make qualify` runs the exact same checks as CI.

**Why:** "Works on my machine" is not acceptable. If a contributor can run `make qualify` locally and it passes, CI will pass. This eliminates surprise failures and reduces feedback loops.

### Adoption Comes from Idiomatic Experience

The system integrates into how users already work. We provide validated configuration, not a new operational model.

**What:** Eidos outputs standard formats (Helm values, Kubernetes manifests) that work with existing tools (kubectl, ArgoCD, Flux). Users don't need to learn "the Eidos way" of deploying.

**Why:** If adoption requires retraining users on a new workflow, our design has failed. Value comes from correctness, not from lock-in.

### Correctness Must Be Reproducible

Given the same inputs, the same system version must always produce the same result (e.g. recipe, bundle artifacts).

**What:** No hidden state, no implicit defaults, no non-deterministic behavior. A recipe/bundle/image digest generated using the same version of eidos today must be identical to one generated tomorrow.

**Why:** Reproducibility is a prerequisite for debugging, validation, and trust. If users can't reproduce a result, they can't trust it.

### Metadata Is Separate from Consumption

Validated configuration exists independent of how it is rendered, packaged, or deployed.

**What:** Recipes define *what* is correct. Bundlers and deployers determine *how* to deliver it (Helm, ArgoCD, raw manifests). The recipe doesn't change based on the deployment mechanism.

**Why:** This prevents tight coupling of correctness to a specific tool, workflow, or delivery mechanism. Users can adopt new deployment tools without re-validating their configurations.

### Recipe Specialization Requires Explicit Intent

More specific recipes are never matched unless explicitly requested. Generic intent cannot silently resolve to specialized configurations.

**What:** If a user requests a "training" recipe, they get the training configuration. The system never silently upgrades to a more specific variant (e.g., "training-distributed-horovod") without explicit opt-in.

**Why:** This prevents accidental misconfiguration and preserves user control. Surprises in infrastructure configuration are dangerous.

### Trust Requires Verifiable Provenance

Trust is established through evidence, not assertions. Every released artifact carries verifiable proof of origin and build process.

**What:** All releases include SLSA Build Level 3 provenance, SBOM attestations, and Sigstore signatures. Users can verify exactly which commit, workflow, and build produced any artifact.

**Why:** This underpins supply-chain security, compliance, and confidence. "Trust us" is not a security model.

## Development Setup

### Clone the Repository

```bash
git clone https://github.com/NVIDIA/eidos.git
cd eidos
```

### Prerequisites

Before running the setup script, ensure you have:

- **Go**: Version 1.25 or higher ([download](https://golang.org/dl/))
- **make**: For build automation (pre-installed on macOS/Linux)
- **git**: For version control

### Automated Setup (Recommended)

The project includes a setup script that installs all required development tools with versions managed centrally in `.versions.yaml`. This ensures consistency between local development and CI environments.

```bash
# Install all required tools (interactive mode)
make tools-setup

# Verify all tools are installed with correct versions
make tools-check
```

Example `make tools-check` output:

```
=== Tool Version Check ===

Tool                 Expected        Installed       Status
----                 --------        ---------       ------
go                   1.25            1.25            ✓
golangci-lint        v2.6            2.6.0           ✓
grype                v0.107.0        0.107.0         ✓
ko                   v0.18.0         0.18.0          ✓
goreleaser           v2              2.13.3          ✓
helm                 v3.17.0         v3.17.0         ✓
kind                 0.27.0          0.27.0          ✓
yamllint             1.35.0          1.35.0          ✓
kubectl              v1.32           v1.32           ✓
docker               -               24.0.7          ✓

Legend: ✓ = installed, ⚠ = version mismatch, ✗ = missing
```

### Version Management

All tool versions are centrally managed in `.versions.yaml`. This file is the single source of truth used by:
- `make tools-setup` - Local development setup
- `make tools-check` - Version verification
- GitHub Actions CI - Ensures CI uses identical versions

When updating tool versions, edit `.versions.yaml` and the changes propagate everywhere automatically.

### Alternative: Using Flox

If you prefer a fully reproducible environment without installing tools globally, you can use [Flox](https://flox.dev/):

```bash
# Install Flox (https://flox.dev/docs/install-flox/)
# Then activate the development environment
flox activate

# Optional: Enable auto-activation with direnv
direnv allow
```

Flox provides all tools in an isolated environment that won't affect your system.

### Finalize Setup

After installing tools (via either method):

```bash
# Download Go module dependencies
make tidy

# Verify everything is ready
make tools-check

# Run full qualification to ensure setup is correct
make qualify
```

## Project Architecture

### Key Components

#### CLI (`eidos`)
- **Location**: `cmd/eidos/main.go` → `pkg/cli/`
- **Framework**: [urfave/cli v3](https://github.com/urfave/cli)
- **Commands**: `snapshot`, `recipe`
- **Purpose**: User-facing tool for system snapshots and recipe generation (supports both query and snapshot modes)
- **Output**: Supports JSON, YAML, and table formats

#### API Server
- **Location**: `cmd/eidosd/main.go` → `pkg/server/`, `pkg/api/`
- **Endpoints**: 
  - `GET /v1/recipe` - Generate configuration recipes
  - `GET /health` - Liveness probe
  - `GET /ready` - Readiness probe
  - `GET /metrics` - Prometheus metrics
- **Purpose**: HTTP service for recipe generation with rate limiting and observability
- **Deployment**: http://localhost:8080

#### Collectors
- **Location**: `pkg/collector/`
- **Pattern**: Factory-based with dependency injection
- **Types**: 
  - **SystemD**: Service states (containerd, docker, kubelet)
  - **OS**: 4 subtypes - grub, sysctl, kmod, release
  - **Kubernetes**: Node info, server version, images, ClusterPolicy
  - **GPU**: Hardware info, driver version, MIG settings
- **Purpose**: Parallel collection of system configuration data
- **Context Support**: All collectors respect context cancellation

#### Recipe Engine
- **Location**: `pkg/recipe/`
- **Purpose**: Generate optimized configurations using base-plus-overlay model
- **Modes**:
  - **Query Mode**: Direct recipe generation from system parameters
  - **Snapshot Mode**: Extract query from snapshot → Build recipe → Return recommendations
- **Input**: OS, OS version, kernel, K8s service/version, GPU type, workload intent
- **Output**: Recipe with matched rules and configuration measurements
- **Data Source**: Embedded YAML configuration (`recipe/data/overlays/*.yaml` including `base.yaml`)
- **Query Extraction**: Parses K8s, OS, GPU measurements from snapshots to construct recipe queries

#### Snapshotter
- **Location**: `pkg/snapshotter/`
- **Purpose**: Orchestrate parallel collection of system measurements
- **Output**: Complete snapshot with metadata and all collector measurements
- **Usage**: CLI command, Kubernetes Job agent
- **Format**: Structured snapshot (eidos.nvidia.com/v1alpha1)

#### Bundler Framework
- **Location**: `pkg/bundler/`
- **Pattern**: Registry-based with pluggable bundler implementations
- **API**: Object-oriented with functional options (DefaultBundler.New())
- **Purpose**: Generate deployment bundles from recipes (Helm values, K8s manifests, scripts)
- **Features**:
  - Template-based generation with go:embed
  - Functional options pattern for configuration (WithBundlerTypes, WithFailFast, WithConfig, WithRegistry)
  - **Parallel execution** (all bundlers run concurrently)
  - Empty bundlerTypes = all registered bundlers (dynamic discovery)
  - Fail-fast or error collection modes
  - Prometheus metrics for observability
  - Context-aware execution with cancellation support
  - **Value overrides**: CLI `--set bundler:path.to.field=value` allows runtime customization
  - **Node scheduling**: `--system-node-selector`, `--accelerated-node-selector`, and toleration flags for workload placement
- **Extensibility**: Implement `Bundler` interface and self-register in init() to add new bundle types

### Common Make Targets

```bash
# Tools Management
make tools-check  # Check installed tools and compare versions
make tools-setup  # Install all required development tools

# Development
make tidy         # Format code and update dependencies
make build        # Build binaries for current platform
make server       # Start API server locally (debug mode)

# Testing
make test         # Run unit tests with coverage
make qualify      # Run tests, lints, and scans (full check)

# Code Quality
make lint         # Lint Go and YAML files and ensure license headers
make lint-go      # Lint Go files only
make lint-yaml    # Lint YAML files only
make license      # Add/verify license headers in source files
make scan         # Security and vulnerability scanning

# Dependency Management
make upgrade      # Upgrade all dependencies
make info         # Show project and tool versions

# Local Development Environment
make dev-env      # Create Kind cluster and start Tilt
make dev-env-clean # Stop Tilt and delete cluster
make dev-reset    # Full reset (tear down and recreate)

# Utilities
make help         # Show all available targets
make help-full    # Show targets grouped by category
```

## Development Workflow

### 1. Create a Branch

Use descriptive branch names:

```bash
# For new features
git checkout -b feat/add-gpu-collector

# For bug fixes
git checkout -b fix/snapshot-crash-on-empty-gpu

# For documentation
git checkout -b docs/update-contributing-guide
```

### 2. Make Changes

Follow these principles:
- **Small, focused commits**: Each commit should address one logical change
- **Clear commit messages**: Use imperative mood (e.g., "Add GPU collector" not "Added GPU collector")
- **Test as you go**: Write tests alongside your code
- **Document your code**: Add comments for exported functions and complex logic

### 3. Run Tests

```bash
# Run all tests
make test
```

### 5. Lint Your Code

```bash
# Run all linters
make lint
```

### 6. Test Locally

CLI: 

```bash
# Build for current platform and test
make e2e
```

API Server: 

```bash
# Start API server
make server

# Test API endpoints (in another terminal)
curl http://localhost:8080/healthz
curl "http://localhost:8080/v1/recipe?os=ubuntu&service=eks"
```

### 7. Run Security Scans

```bash
# Run vulnerability scan
make scan
```

### 8. Full Qualification

Before submitting a PR:

```bash
# Run everything above
make qualify
```

All checks must pass before PR submission.

## Pull Request Process

### Before Submitting

**1. Ensure all checks pass:**
```bash
make qualify
```

**2. Update documentation:**
- [ ] README.md for user-facing changes
- [ ] CONTRIBUTING.md for developer workflow changes
- [ ] Code comments and godoc
- [ ] docs/ for guides or playbooks

**4. Commit with DCO sign-off:**
```bash
git add .
git commit -s -m "Add network collector for system configuration

- Implement NetworkCollector interface
- Add unit tests with 80% coverage
- Update factory registration
- Document collector usage

Fixes #123"
```

**Important**: Always use `-s` flag for DCO sign-off.

### Creating the Pull Request

1. Navigate to your fork of [NVIDIA/eidos](https://github.com/NVIDIA/eidos)
2. Click "Create Pull Request"
3. Fill out the PR template:

**Title**: Clear, concise (e.g., "Add network collector" or "Fix GPU detection crash")

**Description**:
```markdown
## Summary
Brief description of changes

## Changes
- Bullet list of specific changes
- What was added/modified/removed

## Related Issues
Fixes #123

## Testing
- [ ] Unit tests added/updated
- [ ] Integration tests performed
- [ ] Manual testing on Ubuntu 24.04
- [ ] API endpoints tested

## Breaking Changes
None / Describe any breaking changes

## Checklist
- [x] All tests pass (`make test`)
- [x] Linting passes (`make lint`)
- [x] License headers present (`make license`)
- [x] Security scan passes (`make scan`)
- [x] Documentation updated
- [x] Commits are signed off (DCO)
```

### Review Process

1. **Automated Checks** (GitHub Actions `on-push` workflow):
   - ✓ Go tests with race detector
   - ✓ golangci-lint (v2.6.2)
   - ✓ Trivy security scan (MEDIUM, HIGH, CRITICAL)
   - ✓ GitHub-native code coverage tracking
   - ✓ Integration tests (CLI workflows)
   - ✓ E2E tests (Kind cluster with fake GPU)
   - Must pass before merge
2. **Maintainer Review**: A maintainer will review your code for:
   - Correctness and functionality
   - Code style and idioms
   - Test coverage and quality
   - Documentation completeness
3. **Feedback**: Address requested changes by pushing new commits
4. **Approval**: Once approved and CI passes, a maintainer will merge
5. **Celebration**: Your contribution is now part of the project! 🎉

### Addressing Feedback

```bash
# Make requested changes
vim pkg/collector/network.go

# Test changes
make test

# Commit with DCO
git commit -s -m "Address review feedback: improve error handling"

# Push to update PR
git push origin feature/your-feature-name
```

### After Merging

```bash
# Update your local repository
git checkout main
git pull upstream main

# Delete your feature branch
git branch -d feature/your-feature-name
git push origin --delete feature/your-feature-name
```

## Developer Certificate of Origin

All contributions must include a DCO sign-off to certify that you have the right to submit the contribution under the project's license.

### How to Sign Off

Add the `-s` flag to your commit:

```bash
git commit -s -m "Your commit message"
```

This adds a "Signed-off-by" line:
```
Signed-off-by: Jane Developer <jane@example.com>
```

The sign-off certifies that you agree to the DCO below.

### Developer Certificate of Origin 1.1

```
Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

### Amending Commits

If you forget to sign off, amend your commit:

```bash
git commit --amend --signoff
git push --force-with-lease origin feature/your-branch
```

## Tips for Contributors

### First-Time Contributors

**Recommended starting points:**
1. Start with issues labeled `good first issue`
2. Read existing code in the package you're modifying (required before writing)
3. Run `make tools-check` to verify your environment matches expected versions
4. Study the [Design Principles](#design-principles) section before writing code

**Before writing any code:**
```bash
# 1. Verify your setup
make tools-check

# 2. Read existing code in the target package
# Look for patterns: error handling, context usage, test structure

# 3. Run tests to understand expected behavior
go test -v ./pkg/<package>/... -run TestSpecific
```

**Common first-contribution areas:**
- Documentation improvements (typos, clarifications)
- Adding test cases to existing tests
- Improving error messages with better context
- Adding debug logging to collectors

### Writing Good Commit Messages

```
Short summary (50 chars or less)

More detailed explanation if needed. Wrap at 72 characters.
Explain the problem being solved and why this approach was chosen.

- Bullet points are fine
- Use present tense ("Add feature" not "Added feature")
- Reference issues: "Fixes #123" or "Related to #456"

Signed-off-by: Your Name <your@email.com>
```

## Additional Resources

### Project Documentation
- [README.md](README.md) - User documentation and quick start
- [docs/OVERVIEW.md](docs/OVERVIEW.md) - System overview and glossary

### External Resources
- [Go Documentation](https://golang.org/doc/)
- [Effective Go](https://golang.org/doc/effective_go.html)
- [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- [urfave/cli Documentation](https://cli.urfave.org/)

### Getting Help

- **GitHub Issues**: [Create an issue](https://github.com/NVIDIA/eidos/issues/new)
- **Discussions**: Check existing discussions and open new ones
- **Email**: For security issues, contact the maintainers privately

## Questions?

If you have questions about contributing:
- Open a GitHub issue with the "question" label
- Check existing issues for similar questions
- Review this guide and project documentation
- Look at recent merged PRs for examples

Thank you for contributing to NVIDIA Eidos! Your efforts help improve GPU-accelerated infrastructure for everyone.

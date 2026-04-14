---
title: "Contributing"

weight: 10
description: "How to contribute to AICR"
---

# Contributing

Thank you for your interest in contributing to NVIDIA AICR! We welcome contributions from developers of all backgrounds and experience levels.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [How to Contribute](#how-to-contribute)
- [Design Principles](#design-principles)
- [Pull Request Process](#pull-request-process)
- [Developer Certificate of Origin](#developer-certificate-of-origin)
- [Tips for Contributors](#tips-for-contributors)

## Code of Conduct

This project follows NVIDIA's commitment to fostering an open and welcoming environment. Please be respectful and professional in all interactions. See [CODE_OF_CONDUCT.md](/docs/project/code-of-conduct) for details.

## Getting Started

Before contributing:

1. Read the [Getting Started Guide](/docs/getting-started/) to understand the project
2. Check existing [issues](https://github.com/NVIDIA/aicr/issues) to avoid duplicates
3. Review the [security policy](/docs/project/security) for security-related contributions
4. Set up your development environment following [DEVELOPMENT.md](/docs/project/development)

## How to Contribute

### Reporting Bugs

- Use the [bug report template](https://github.com/NVIDIA/aicr/issues/new?template=bug_report.yml)
- Describe the issue clearly with steps to reproduce
- Include system information (OS, Go version, Kubernetes version)
- Attach logs or screenshots if applicable
- Check if the issue already exists before creating a new one

### Suggesting Enhancements

- Use the [feature request template](https://github.com/NVIDIA/aicr/issues/new?template=feature_request.yml)
- Clearly describe the proposed feature and its use case
- Explain how it benefits the project and users
- Provide examples or mockups if applicable

### Improving Documentation

- Fix typos, clarify instructions, or add examples
- Update README.md for user-facing changes
- Update API documentation when endpoints change
- Ensure code comments are accurate and helpful

### Contributing Code

- Fix bugs, add features, or improve performance
- Follow the development workflow in [DEVELOPMENT.md](/docs/project/development)
- Ensure all tests pass and code meets quality standards
- Write tests for new functionality

#### Go dependencies (vendor)

This project vendors Go dependencies. After changing `go.mod` or `go.sum`, run `make tidy` (which runs `go mod vendor`) and commit `go.mod`, `go.sum`, and the `vendor/` directory. CI will fail if `vendor/` is out of sync.

#### Adding Validation Constraints

AICR uses a validator framework to check cluster state against requirements. To add new validation constraints:

**Quick Start:**
```bash
# Generate all necessary files
make generate-validator ARGS="--constraint Deployment.my-app.version --phase deployment --description 'Validates my-app version'"
```

This creates three files with TODOs guiding implementation:
- Helper functions with validation logic
- Unit tests with table-driven test cases
- Integration test with automatic registration

**Next Steps:**
1. Implement the TODOs in generated files
2. Add comprehensive test cases
3. Run `make test` - registration validation ensures completeness
4. Submit PR - CI enforces all requirements

**See [Validator Development Guide](/docs/contributor/validations) for complete guide with examples, architecture overview, and troubleshooting.**

## Design Principles

These principles guide all design decisions in AICR. When faced with trade-offs, these principles take precedence.

### Local Development Equals CI

The same tools, same versions, and same validation run locally and in CI.

**What:** Tool versions are centralized in `.settings.yaml`. Both `make tools-setup` (local) and GitHub Actions use this single source of truth. `make qualify` runs the exact same checks as CI.

**Why:** "Works on my machine" is not acceptable. If a contributor can run `make qualify` locally and it passes, CI will pass. This eliminates surprise failures and reduces feedback loops.

### Adoption Comes from Idiomatic Experience

The system integrates into how users already work. We provide validated configuration, not a new operational model.

**What:** AICR outputs standard formats (Helm values, Kubernetes manifests) that work with existing tools (kubectl, Argo CD, Flux). Users don't need to learn "the AICR way" of deploying.

**Why:** If adoption requires retraining users on a new workflow, our design has failed. Value comes from correctness, not from lock-in.

### Correctness Must Be Reproducible

Given the same inputs, the same system version must always produce the same result (e.g. recipe, bundle artifacts).

**What:** No hidden state, no implicit defaults, no non-deterministic behavior. A recipe/bundle/image digest generated using the same version of aicr today must be identical to one generated tomorrow.

**Why:** Reproducibility is a prerequisite for debugging, validation, and trust. If users can't reproduce a result, they can't trust it.

### Metadata Is Separate from Consumption

Validated configuration exists independent of how it is rendered, packaged, or deployed.

**What:** Recipes define *what* is correct. Bundlers and deployers determine *how* to deliver it (Helm, Argo CD, raw manifests). The recipe doesn't change based on the deployment mechanism.

**Why:** This prevents tight coupling of correctness to a specific tool, workflow, or delivery mechanism. Users can adopt new deployment tools without re-validating their configurations.

### Recipe Specialization Requires Explicit Intent

More specific recipes are never matched unless explicitly requested. Generic intent cannot silently resolve to specialized configurations.

**What:** If a user requests a "training" recipe, they get the training configuration. The system never silently upgrades to a more specific variant (e.g., "training-distributed-horovod") without explicit opt-in.

**Why:** This prevents accidental misconfiguration and preserves user control. Surprises in infrastructure configuration are dangerous.

### Trust Requires Verifiable Provenance

Trust is established through evidence, not assertions. Every released artifact carries verifiable proof of origin and build process.

**What:** All releases include SLSA Build Level 3 provenance, SBOM attestations, and Sigstore signatures. Users can verify exactly which commit, workflow, and build produced any artifact.

**Why:** This underpins supply-chain security, compliance, and confidence. "Trust us" is not a security model.

## Pull Request Process

### Before Submitting

1. **Ensure all checks pass:**
   ```bash
   make qualify
   ```

2. **Update documentation if needed:**
   - README.md for user-facing changes
   - DEVELOPMENT.md for developer workflow changes
   - Code comments and godoc for API changes

3. **Commit with required provenance:**
   ```bash
   # External contributors (DCO sign-off required)
   git commit -s -m "feat: add network collector

   - Implement NetworkCollector interface
   - Add unit tests with 80% coverage
   - Update factory registration

   Fixes #123"

   # NVIDIA org members / automation (DCO sign-off exempt)
   git commit -S -m "feat: add network collector"
   ```

   External contributors must use `-s`. NVIDIA organization members are exempt from DCO bot sign-off checks and should use cryptographic signing (`-S`).

### Creating the Pull Request

1. Push your branch and open a PR against `main`
2. Fill out the PR template completely:
   - **Summary**: Brief description of changes
   - **Type of Change**: Bug fix, feature, breaking change, etc.
   - **Testing**: What testing was performed
   - **Checklist**: Verify all items

### Review Process

1. **Automated Checks** run via GitHub Actions:
   - Go tests with race detector
   - golangci-lint
   - YAML linting
   - Security scans (Anchore in CI, Grype in `make scan`)
   - Coverage tracking
   - E2E tests

2. **Maintainer Review** covers:
   - Correctness and functionality
   - Code style and Go idioms
   - Test coverage and quality
   - Documentation completeness

3. **Address Feedback** by pushing new commits:
   ```bash
   git commit -s -m "address review: improve error handling"   # external contributors
   # or
   git commit -S -m "address review: improve error handling"   # NVIDIA org members / automation
   git push origin your-branch
   ```

4. **Merge**: Once approved and CI passes, a maintainer will merge

### Issue and PR Lifecycle

Automated bots manage the lifecycle of issues and pull requests:

| Day | Action |
|-----|--------|
| 0 | Issue/PR opened, `needs-triage` label added to issues |
| 14 | Inactive PRs receive a reminder comment |
| 30 | Inactive PRs marked `lifecycle/stale` |
| 44 | Stale PRs auto-closed |
| 60 | Inactive issues marked `lifecycle/stale` |
| 74 | Stale issues auto-closed |
| 90+ | Closed issues/PRs locked |

**To prevent auto-close:** Add the `lifecycle/frozen` label. PRs with `do-not-merge` are also exempt.

### After Merging

```bash
# Update your local repository
git checkout main
git pull upstream main

# Delete your feature branch
git branch -d your-branch
git push origin --delete your-branch
```

## Developer Certificate of Origin

Contributions must satisfy Developer Certificate of Origin (DCO) policy. External contributors (non-NVIDIA organization members) must include a DCO sign-off on each commit. NVIDIA organization members are exempt from DCO bot sign-off checks and should use cryptographic signing (`-S`).

### How to Sign Off (External Contributors)

Add the `-s` flag to your commit:

```bash
git commit -s -m "Your commit message"
```

This adds a "Signed-off-by" line:

```
Signed-off-by: Jane Developer <jane@example.com>
```

### Configure Git for Automatic Sign-off

```bash
git config user.name "Your Name"
git config user.email "your.email@example.com"
```

### Amending Commits

If you forget to sign off:

```bash
git commit --amend --signoff
git push --force-with-lease origin your-branch
```

### NVIDIA Org Members and Automation

NVIDIA organization members are exempt from DCO bot sign-off checks (`.github/dco.yml`). Use cryptographic commit signing:

```bash
git commit -S -m "Your commit message"
```

### What You're Certifying

By signing off, you certify the Developer Certificate of Origin 1.1:

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

## Tips for Contributors

### First-Time Contributors

**Recommended starting points:**

1. Start with issues labeled `good first issue`
2. Read existing code in the package you're modifying before writing
3. Run `make tools-check` to verify your environment
4. Study the [Design Principles](#design-principles) section

**Good first contributions:**

- Documentation improvements (typos, clarifications)
- Adding test cases to existing tests
- Improving error messages with better context

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

### Code Style

- Follow existing patterns in the codebase
- Use `pkg/errors` for error handling (not `fmt.Errorf`)
- Always check `ctx.Done()` in loops and long operations
- Write table-driven tests for multiple test cases
- Use functional options for configuration

### Getting Help

- **GitHub Issues**: [Create an issue](https://github.com/NVIDIA/aicr/issues/new) with the "question" label
- **Existing Issues**: Search for similar questions first
- **Recent PRs**: Look at merged PRs for examples

## Additional Resources

- [DEVELOPMENT.md](/docs/project/development) - Development setup, architecture, and tooling
- [Getting Started Guide](/docs/getting-started/) - Project overview and quick start
- [Documentation Overview](/docs/getting-started/) - System overview and glossary
- [Architecture Documentation](/docs/contributor/) - Architecture documentation

Thank you for contributing to NVIDIA AICR! Your efforts help improve GPU-accelerated infrastructure for everyone.

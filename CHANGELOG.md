# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Features

- Add multi-phase validation system with readiness, deployment, performance, and conformance phases
- Add `--phase` flag to `eidos validate` command to run specific validation phases
- Extend recipe schema with `ValidationConfig` for phase-specific checks and constraints
- Extend `Constraint` schema with optional `severity`, `remediation`, and `unit` fields

### Documentation

- Update CLI reference with multi-phase validation documentation
- Add validation configuration guide to recipe development documentation
- Update architecture documentation with validator phase details
- Add validation phase examples to user guide

## [0.3.3] - 2026-02-04

### Tasks

- Adjust release commit message order by [@mchmarny](https://github.com/mchmarny)

## [0.3.2] - 2026-02-04

### Tasks

- Include non-conventional commits in changelog by [@mchmarny](https://github.com/mchmarny)
- Update release commit message format by [@mchmarny](https://github.com/mchmarny)

## [0.3.1] - 2026-02-04

### Features

- Add aws-efa component  by [@Kevin-Hawkins](https://github.com/Kevin-Hawkins)

### Refactor

- Use structured errors and improve test coverage by [@mchmarny](https://github.com/mchmarny)

### Tasks

- Remove daily scan from blocking prs by [@mchmarny](https://github.com/mchmarny)
- Add Claude instructions to not co-authored commits by [@mchmarny](https://github.com/mchmarny)
- Allow attribution but not co-authoring by [@mchmarny](https://github.com/mchmarny)
- Moved coauthoring into main claude doc by [@mchmarny](https://github.com/mchmarny)

## [0.3.0] - 2026-02-04

### Bug Fixes

- Add contents:read permission for coverage comment workflow  by [@dims](https://github.com/dims)
- Use /tmp paths for coverage artifacts  by [@dims](https://github.com/dims)
- Rename prometheus component to kube-prometheus-stack  by [@yuanchen8911](https://github.com/yuanchen8911)
- Remove namespaceOverride from nvidia-dra-driver-gpu values  by [@yuanchen8911](https://github.com/yuanchen8911)

### CI/CD

- Add license verification workflow  by [@dims](https://github.com/dims)
- Add license verification workflow  by [@dims](https://github.com/dims)
- Add CodeQL security analysis workflow  by [@dims](https://github.com/dims)
- Use copy-pr-bot branch pattern for PR workflows  by [@dims](https://github.com/dims)
- Trigger workflows on branch create for copy-pr-bot  by [@dims](https://github.com/dims)
- Skip workflows on forks to prevent duplicate check runs  by [@dims](https://github.com/dims)
- Match nvsentinel workflow pattern for copy-pr-bot  by [@dims](https://github.com/dims)

### Features

- Add coverage delta reporting for PRs  by [@dims](https://github.com/dims)
- Link GitHub usernames in changelog  by [@dims](https://github.com/dims)
- Add structured CLI exit codes for predictable scripting  by [@dims](https://github.com/dims)
- Add fullnameOverride to remove release prefix from deployment names  by [@yuanchen8911](https://github.com/yuanchen8911)

### Tasks

- Rename default claude file to follow convention by [@mchmarny](https://github.com/mchmarny)
- Add .claude/settings.local.json to ignore by [@mchmarny](https://github.com/mchmarny)
- Add copy-pr-bot configuration  by [@dims](https://github.com/dims)
- Refactor tools-check into standalone script  by [@mchmarny](https://github.com/mchmarny)

## [0.2.2] - 2026-02-01

### Bug Fixes

- Preserve manual changelog edits during version bump by @mchmarny

## [0.2.1] - 2026-02-01

### Bug Fixes

- Use workflow_run for PR coverage comments on fork PRs  by @dims
- Add actions:read permission for artifact download  by @dims

### Features

- Add contextcheck and depguard linters  by @dims
- Add stale issue and PR automation  by @dims
- Add Dependabot grouping for Kubernetes dependencies  by @dims
- Add automatic changelog generation with git-cliff by @mchmarny

### Tasks

- Add dims in maintainers by @mchmarny
- Add owners file by @mchmarny
- Fix code owners by @mchmarny
- Replace explicit list with a link to the maintainer team by @mchmarny
- Update code owners by @mchmarny

## [0.2.0] - 2026-01-31

### Bug Fixes

- Support private repo downloads in install script by @mchmarny
- Skip sudo when install directory is writable by @mchmarny

## [0.1.5] - 2026-01-31

### Bug Fixes

- Add GHCR authentication for image copy by @mchmarny

## [0.1.4] - 2026-01-31

### Features

- Add Artifact Registry for demo API server deployment by @mchmarny

## [0.1.3] - 2026-01-31

### Bug Fixes

- Install ko and crane from binary releases by @mchmarny

## [0.1.2] - 2026-01-31

### Bug Fixes

- Remove KO_DOCKER_REPO that conflicts with goreleaser repositories by @mchmarny

### Other

- Restore flat namespace for container images by @mchmarny

### Refactor

- Extract E2E tests into reusable composite action by @mchmarny

## [0.1.1] - 2026-01-31

### Bug Fixes

- Ko uppercase repository error and refactor on-tag workflow by @mchmarny

### Refactor

- Migrate container images to project-specific registry path by @mchmarny

## [0.1.0] - 2026-01-31

### Bug Fixes

- Correct serviceAccountName field casing in Job specs by @mchmarny
- Add actions:read permission for CodeQL telemetry by @mchmarny
- Add explicit slug to Codecov action by @mchmarny
- Make SARIF upload graceful when code scanning unavailable by @mchmarny
- Install ko from binary release instead of go install by @mchmarny
- Strip v prefix from ko version for URL construction by @mchmarny

### CI/CD

- Run test and e2e jobs concurrently by @mchmarny
- Add notice when SARIF upload is skipped by @mchmarny

### Features

- Replace Codecov with GitHub-native coverage tracking by @mchmarny
- Add Flox manifest generator from .versions.yaml by @mchmarny

### Refactor

- Integrate E2E tests into main CI workflow by @mchmarny
- Split CI into unit, integration, and e2e jobs by @mchmarny

### Tasks

- Init repo by @mchmarny
- Replace file-existence-action with hashFiles by @mchmarny
- Replace ko-build/setup-ko with go install by @mchmarny
- Remove Homebrew and update org to NVIDIA by @mchmarny
- Update settings by @mchmarny
- Remove code owners for now by @mchmarny
- Update project docs and setup by @mchmarny
- Update contributing doc by @mchmarny
- Remove badges not supported in local repos by @mchmarny

<!-- Generated by git-cliff -->

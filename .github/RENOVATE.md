<!--
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Renovate

Self-hosted Renovate keeps the project's dependencies up to date across `go.mod`, Dockerfiles, Terraform, Helm chart values, and — crucially — the tool versions pinned in [`.settings.yaml`](../.settings.yaml), which a vanilla Renovate setup cannot reach without the custom regex manager configured here. GitHub Actions / composite-action digests are owned by Dependabot (Renovate's `github-actions` manager is disabled in `renovate.json5`).

- Configuration: [`.github/renovate.json5`](renovate.json5)
- Workflow: [`.github/workflows/renovate.yaml`](workflows/renovate.yaml)
- Companion script: [`tools/update-chainsaw-checksums`](../tools/update-chainsaw-checksums)

Policy choices (schedule, cooldown, auto-merge scope, group consolidation) are documented inline in `renovate.json5`. This doc covers what's covered, how to extend coverage, and the known gotchas.

## Coverage

| Source | Manager |
|---|---|
| `go.mod` | `gomod` (groups: `kubernetes`, `golang-x`, `opencontainers`) |
| `.github/workflows/*.yaml`, `.github/actions/*/action.yml` | `github-actions` (**disabled** — Dependabot owns workflow / composite-action bumps; Renovate cannot push `.github/workflows/*` with the auto-issued `GITHUB_TOKEN`) |
| `validators/*/Dockerfile` | `dockerfile` |
| `infra/**/*.tf` | `terraform` (grouped) |
| `recipes/components/*/values.yaml` | `helm-values` (partial — see limitations) |
| `.settings.yaml` (28 tool entries) | custom regex manager (`# renovate:` annotations) |
| `.settings.yaml` `nvkind` SHA | dedicated git-refs digest customManager (`# renovate-digest:`) |
| `.settings.yaml` `chainsaw_checksums` | `postUpgradeTasks` → `tools/update-chainsaw-checksums` |
| `.go-version` (Go toolchain) | dedicated `golang-version` customManager (`go-toolchain` group) |

The `go` directive in `go.mod` is intentionally not bumped — the Go toolchain version is owned by `.go-version`. Makefile (`GOTOOLCHAIN`), the `load-versions` composite action, `install-karpenter-kwok`, and validator Dockerfiles (`--build-arg GO_VERSION`) all read from that single file.

## Adding a new pin to `.settings.yaml`

Place a `# renovate:` annotation directly above the value. The annotation **must** include `depType=<section>` naming the top-level YAML section (e.g. `build_tools`, `testing_tools`); `packageRules` use this to bundle PRs by section.

```yaml
# Plain version string (no embedded ':')
# renovate: datasource=github-releases depName=owner/repo depType=build_tools
mytool: 'v1.2.3'

# Docker image with embedded tag — captures only the tag
# renovate: datasource=docker depName=registry.example.com/path/image depType=testing
mytool_image: 'registry.example.com/path/image:1.2.3'

# YAML list item
some_list:
  # renovate: datasource=github-releases depName=owner/repo depType=build_tools
  - 'v1.2.3'
```

`depType` must come immediately after `depName` (the regex captures it as the next whitespace-separated token). Block scalars (`|` / `>`) and unquoted values are not supported — keep version pins as quoted scalars.

Optional metadata (`extractVersion`, `versioning`, `packageNames`, `registryUrls`) belongs in `renovate.json5`'s `packageRules` keyed off `matchDepNames`, not in the annotation comment.

### Tracking a git-refs SHA

For tools pinned by 40-char commit SHA (no upstream releases), use the distinct `# renovate-digest:` prefix:

```yaml
# renovate-digest: datasource=git-refs depName=mytool packageName=https://github.com/owner/repo branch=main depType=testing_tools
mytool: '1234567890abcdef1234567890abcdef12345678'
```

The distinct prefix prevents the broad regex from double-extracting it.

### Validating changes

```sh
make lint-renovate    # requires Docker; runs the same image the workflow uses
```

CI re-runs `make lint-renovate` automatically via `merge-gate.yaml` whenever `.github/renovate.json5` changes.

## Known limitations

- **AWS EFA device-plugin image** (`recipes/components/aws-efa/values.yaml`) is published only to AWS's authenticated public ECR (`602401143452.dkr.ecr.us-west-2.amazonaws.com/...`); no `public.ecr.aws` mirror. The image is in `ignoreDeps`; bumps must be coordinated manually with EKS add-on releases.
- **`recipes/components/*/values.yaml`** is partially covered. `helm-values` only auto-detects the conventional `image: { repository, tag }` shape; add `# renovate:` annotations directly in those files to extend coverage.
- **No vulnerability fast-path.** Self-hosted Renovate cannot consume GitHub vulnerability alerts (Mend-hosted feature). The weekday cron is the mitigation.
- **The Renovate runner image** (`ghcr.io/renovatebot/renovate`) is not yet auto-managed. The digest is pinned in two places — the `RENOVATE_VALIDATOR_IMAGE` variable in `Makefile` and the `renovate-version` input in the workflow — and must be bumped manually in lockstep. The custom regex doesn't yet capture `image:tag@sha256:...` shapes.

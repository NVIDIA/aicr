<!--
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Renovate

Self-hosted Renovate keeps the project's dependencies up to date across `go.mod`, Dockerfiles, Terraform, Helm chart values, and — crucially — the tool versions pinned in [`.settings.yaml`](../.settings.yaml), which a vanilla Renovate setup cannot reach without the custom regex manager configured here. GitHub Actions / composite-action digests are owned by Dependabot (Renovate's `github-actions` manager is disabled in `renovate.json5`).

- Configuration: [`.github/renovate.json5`](renovate.json5)
- Workflow: [`.github/workflows/renovate.yaml`](workflows/renovate.yaml)
- Companion scripts: [`tools/update-chainsaw-checksums`](../tools/update-chainsaw-checksums), [`tools/update-helmfile-checksums`](../tools/update-helmfile-checksums), [`tools/update-helm-diff-checksums`](../tools/update-helm-diff-checksums), [`tools/update-oras-checksums`](../tools/update-oras-checksums), [`tools/update-oasdiff-checksums`](../tools/update-oasdiff-checksums), [`tools/update-addlicense-checksums`](../tools/update-addlicense-checksums), [`tools/update-setup-envtest-checksums`](../tools/update-setup-envtest-checksums)

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
| `.settings.yaml` `helmfile_checksums` | `postUpgradeTasks` → `tools/update-helmfile-checksums` |
| `.settings.yaml` `helm_diff_checksums` | `postUpgradeTasks` → `tools/update-helm-diff-checksums` |
| `.settings.yaml` `oras_sha256_*` | `postUpgradeTasks` → `tools/update-oras-checksums` |
| `.settings.yaml` `oasdiff_sha256_linux_amd64` | `postUpgradeTasks` → `tools/update-oasdiff-checksums` |
| `.settings.yaml` `addlicense_sha256_linux_amd64` | `postUpgradeTasks` → `tools/update-addlicense-checksums` |
| `.settings.yaml` `setup_envtest_sha256_linux_amd64` | `postUpgradeTasks` → `tools/update-setup-envtest-checksums` (hashes the asset; controller-runtime publishes no `checksums.txt`) |
| `go.mod` `golang.org/x/exp` / `github.com/google/go-licenses/v2` | the only pin for tools built from this module; the `gomod` manager is re-enabled for these two `// indirect` entries and `.settings.yaml` deliberately does not repeat them — `TestToolPinsLiveOnlyInGoMod` fails if it does |
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

## Auto-merge and the render golden

`pkg/bundler/testdata/stock_render_golden.yaml` pins a digest per leaf overlay over its fully rendered output. The `kubernetes` manager's digest bumps inside `recipes/components/*/manifests/**` and the `helm-values` manager's tag bumps in `recipes/components/*/values.yaml` both change rendered bytes and move this golden — and both are configured to `automerge: true`. Left alone, such a PR goes red on an unregenerated golden and stalls its own auto-merge until a human clones the repo and runs the regeneration command by hand (this happened on PR #2773 and took `main` down when the red PR was merged anyway).

[`render-golden-refresh.yaml`](workflows/render-golden-refresh.yaml) closes half of that gap: on a PR whose branch matches `renovate/*`, it regenerates the golden and, if it drifted, commits the refresh back onto the same PR branch (via GraphQL `createCommitOnBranch`, pinned to the branch's current tip so a concurrent push during regeneration fails loudly instead of landing a stale commit). It does **not** re-fire the PR's checks: a `GITHUB_TOKEN`-authored commit fires no `pull_request` event, and dispatching `merge-gate.yaml` via `workflow_dispatch` does not substitute for that — its checks attach to the commit but never appear in the PR's status-check rollup, so they cannot satisfy the required `gate` check (confirmed against PR #2780: dispatching `merge-gate.yaml` at the refreshed head produced 27 check-runs on the commit and 0 entries in the PR's rollup). After a refresh commit, the PR's checks still point at the pre-refresh head SHA; the workflow leaves a `::notice::` and job-summary line saying a maintainer must push any commit to the branch, or close and reopen the PR, to get a PR-associated run.

This refresh deliberately does **not** run on human PRs, even ones that touch `recipes/components/**`. The golden exists so a human sees an unintended change to rendered output; a Renovate digest/tag bump's effect on the golden is arithmetic, so that narrow class is safe to automate, but auto-refreshing on a contributor's PR would silently absorb the signal the golden exists to give.

If a Renovate PR's checks are still pinned to a stale SHA after the refresh commits, do **not** try to work around it with `workflow_dispatch` — a manually re-run `merge-gate.yaml` has the same rollup problem the workflow itself hit. Do **not** assume `/ok-to-test` unblocks it either: that path (`.github/workflows/ok-to-test.yaml`) dispatches `qualification.yaml` directly, producing individual qualification check names, not `merge-gate.yaml`'s `gate` aggregate, so it has not been shown to satisfy the `gate` required check either. The fix is a PR-associated run: push any commit to the branch, or close and reopen the PR.

## Known limitations

- **AWS EFA device-plugin image** (`recipes/components/aws-efa/values.yaml`) is published only to AWS's authenticated public ECR (`602401143452.dkr.ecr.us-west-2.amazonaws.com/...`); no `public.ecr.aws` mirror. The image is in `ignoreDeps`; bumps must be coordinated manually with EKS add-on releases.
- **`recipes/components/*/values.yaml`** is partially covered. `helm-values` only auto-detects the conventional `image: { repository, tag }` shape; add `# renovate:` annotations directly in those files to extend coverage.
- **No vulnerability fast-path.** Self-hosted Renovate cannot consume GitHub vulnerability alerts (Mend-hosted feature). The weekday cron is the mitigation.
- **The Renovate runner image** (`ghcr.io/renovatebot/renovate`) is not yet auto-managed. The digest is pinned in two places — the `RENOVATE_VALIDATOR_IMAGE` variable in `Makefile` and the `renovate-version` input in the workflow — and must be bumped manually in lockstep. The custom regex doesn't yet capture `image:tag@sha256:...` shapes.

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

## Deprecation Policy

AICR freezes four public surfaces at v1
([ROADMAP §1](ROADMAP.md#1-defensible-api-stability)):
the `aicr` CLI, the REST API, the Go SDK (`pkg/client/v1`), and the bundle
layout plus artifact schemas. This section defines what counts as a breaking
change on each, the notice a removal owes, and how a deprecation reaches the
people affected by it. A change that is breaking under this table and ships
without the notice below is a release blocker, not a release note.

### What counts as breaking, per surface

| Surface | Breaking | Additive |
|---|---|---|
| **CLI** — flags, subcommands, exit codes, stdout shape | Removing or renaming a flag or subcommand; changing a default such that identical input yields different output; narrowing an accepted value set; changing what an exit code means; removing a field from `--output json`/`yaml` | New flag whose default preserves current behavior; new subcommand; new accepted enum value; new field in structured output |
| **REST** — `api/aicr/v1/server.yaml` | Removing a path or method; removing or renaming a response field; adding a required request field; narrowing a type; removing a value from a request enum | New optional request field; new response field; new path or method; new value in a response enum |
| **Go SDK** — `pkg/client/v1` | Removing or renaming an exported identifier; changing a signature; narrowing a parameter type; changing documented semantics without changing the name | New exported function, method, or type; new functional option; new field on a struct the caller does not construct positionally |
| **Bundle + schemas** — layout and artifact kinds | Removing or renaming a bundle path; removing a schema field; tightening a type; adding a required field; retiring an `apiVersion` | New optional field; new file in the bundle; new artifact kind |

Adding a value to a *response* enum is additive for the server and breaking for
a client that switches exhaustively on it, so it is announced but does not owe a
window. Adding a value to a *request* enum is always additive; removing one is
always breaking.

### Notice owed before removal

- **Before `v1.0.0`:** a minimum of **two minor releases** between the
  deprecation shipping with a working warning and the removal. At the current
  cadence that is roughly one month.
- **After `v1.0.0`:** a breaking removal on any of the four frozen surfaces
  requires the next `vMAJOR`. The deprecation may be announced at any time; the
  removal waits for the major. This is what the freeze buys and it is not
  waivable by a release manager.

Artifact `apiVersion` retirement is the one surface with a maturity-scoped
window rather than a flat one, because an alpha version never promised
stability in the first place. Its rules are below and take precedence for that
surface.

**Closing a fail-open gate is not a deprecation.** When two enforcement paths
disagree about the same document and one of them already rejected it, aligning
the permissive path to the strict one owes no notice window. The permissive
behavior was a defect, not a contract: it accepted input the project had
already decided was invalid, and continuing to honor it for two releases would
mean knowingly shipping the fail-open seam the stricter path exists to close.
Such a change must still be recorded in
[`docs/user/deprecations.md`](docs/user/deprecations.md) with its real release,
and its rationale written down in the governing ADR.

Exercised exactly once so far, in v0.21: a `RecipeMetadata` overlay with an
empty `apiVersion` was rejected by the catalog scanner but silently hydrated on
the direct-input path. ADR-022 §3 records the decision and
[#2421](https://github.com/NVIDIA/aicr/issues/2421) the analysis; no committed
artifact in the tree was affected. This clause is deliberately narrow — it does
not cover tightening validation that both paths previously accepted, which is an
ordinary breaking change and owes the full window.

### How a deprecation is announced

Every deprecation appears in all three places. One is not a substitute for
another: release notes are read once, the durable page is read later by someone
debugging, and the runtime warning reaches the user who never read either.

1. A `### Deprecations` section in the release notes for the release that
   introduces it, naming the replacement and the planned removal release. The
   heading is an h3 sibling of `### Highlights`; the release-notes generator
   (`.agents/skills/aicr-release-notes/SKILL.md`) emits it at that level.
2. An entry on the durable page at
   [`docs/user/deprecations.md`](docs/user/deprecations.md), which carries every
   active deprecation and its removal release until the removal ships.
3. A runtime warning on the affected surface, using that surface's mechanism:

   | Surface | Mechanism |
   |---|---|
   | CLI | Warning on stderr naming the replacement and the removal release. Honors `NO_COLOR` and the existing logger conventions |
   | REST | A `Deprecation` response header ([RFC 9745](https://www.rfc-editor.org/rfc/rfc9745.html)) carrying the deprecation date, a `Sunset` header ([RFC 8594](https://www.rfc-editor.org/rfc/rfc8594.html)) carrying the removal date, a `Link` with `rel="deprecation"`, and `deprecated: true` on the operation in `api/aicr/v1/server.yaml` |
   | Go SDK | A `// Deprecated:` godoc marker, which `staticcheck` surfaces to consumers automatically |
   | Bundle + schemas | The loader accepts the deprecated shape and warns, naming the file and the release that stops reading it |

### Exercising the channel before `v1.0.0`

ROADMAP [§1](ROADMAP.md#1-defensible-api-stability) requires this file to define
breaking changes and the deprecation policy for every surface; it does not
require a rehearsal. Manufacturing a deprecation to prove the channel works
would prove only that we can manufacture one.

**The channel will most likely reach `v1.0.0` mechanically tested but never
exercised on an obligation it actually owed.** That is a deliberate, accepted
position at this stage of the project, recorded here so it is a choice rather
than something discovered later.

Two candidates existed and neither turns out to be a real exercise:

- **The `/v1/*` REST family is being collapsed, not deprecated.**
  [#2112](https://github.com/NVIDIA/aicr/issues/2112) folds the profile-aware
  `/v2` contract into `/v1` and removes the `/v2` paths. With no REST consumers
  yet, that owes no notice window — it is a pre-adoption restructure. Spending
  two releases deprecating an endpoint nobody calls would buy a worse end state
  (two frozen path families instead of one) for the sake of a dry run.
- **The ADR-022 alpha migration runs warn-then-remove across v0.22 and v0.23**
  and will be the first end-to-end use of the loader-warning arm — once
  [#2416](https://github.com/NVIDIA/aicr/issues/2416) wires `deprecation.Warn`
  into the artifact loaders, which is still outstanding. Even then, alpha owes
  no window under the table above, so it demonstrates the mechanism working
  rather than the policy being honored.

What that leaves untested is the *obligation*, not the machinery. The
per-surface mechanisms have unit coverage in `pkg/deprecation` and `pkg/server`.
The REST arm — the RFC 9745 `Deprecation` and RFC 8594 `Sunset` headers and the
OpenAPI `deprecated` flag, which is what an integrator would actually consume —
has no route marked deprecated and will not be driven by a shipped deprecation.
The CLI arm has no deprecated flag or subcommand to warn about.

The first surface change that genuinely owes a window is the real exercise, and
it will most likely land after `v1.0.0`, when the notice owed is a `vMAJOR`
rather than two minors. Treat the first such change as the moment to verify the
channel end to end, and fix whatever it exposes then.

## Artifact Compatibility and Deprecation

Artifact `apiVersion` maturity is independent of the AICR release version and
is governed by [ADR-022](docs/design/022-artifact-maturity-and-deprecation.md).
Retiring an artifact version owes the following window on the AICR release
axis:

- Alpha: no deprecation window.
- Beta: readable for two releases after deprecation.
- GA: readable for the rest of the current AICR major version; removal requires
  the next `vMAJOR` release.

For beta and GA bumps, stage the new reader in Release N before switching the
emitter in Release N+1. Release notes must identify any deprecation, the last
release that reads the retiring version, and the required artifact recapture,
regeneration, or authored-header edit.

The initial alpha-to-target migration is the explicit three-release sequence in
ADR-022 §3, bound to these releases:

| Release | Reads | Emits | Tracking |
|---|---|---|---|
| v0.21 | alpha and target | alpha | [#2404](https://github.com/NVIDIA/aicr/pull/2404) |
| v0.22 | alpha and target | target | [#2416](https://github.com/NVIDIA/aicr/issues/2416) |
| v0.23 | target only | target | [#2417](https://github.com/NVIDIA/aicr/issues/2417) |

Cutting v0.22 or v0.23 means completing the corresponding issue in that release,
not after it. The consumer-facing form of this table, including the per-kind
target values, is in
[`docs/integrator/data-extension.md`](docs/integrator/data-extension.md#catalog-and-binary-compatibility).

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

**One-time check on the first RC after the notices change.**
`THIRD_PARTY_NOTICES.md` is no longer committed — `make release` regenerates it
from the tag being released and goreleaser uploads it. An RC runs the same
`on-tag.yaml` → `go-build-release` → `make release` path as a stable release
(the `Build and Release` step is not gated on `is_prerelease`), so the first RC
is where that path is proven. Confirm the release job's output matches a local
run:

Both inputs have to match what the release job used, or the comparison measures
drift rather than reproducibility. The release generated from the RC tag's tree
with the toolchain pinned in `.settings.yaml`, so pin both locally: run from a
worktree at the tag rather than the ambient checkout, and confirm the local
`go` and `go-licenses` match their pins first.

```bash
git worktree add /tmp/rc-verify vX.Y.Z-rc1
cd /tmp/rc-verify
make tools-check   # go and go_licenses must match .settings.yaml
gh release download vX.Y.Z-rc1 -p THIRD_PARTY_NOTICES.md -D /tmp/rc
make notices
diff /tmp/rc/THIRD_PARTY_NOTICES.md THIRD_PARTY_NOTICES.md
```

Identical output confirms both that the asset was attached and that generation
is host-independent, which is what the generator's fixed platform matrix and
`LC_ALL=C` sort exist to guarantee. A missing asset means the `extra_files` glob
found nothing. A diff means generation is not reproducible and the release
should not be promoted until it is understood — but check `make tools-check`
first: a `⚠` on `go` or `go_licenses` means the local toolchain, not the
generator, explains the difference.

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
| `aicr` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Pure-Go CLI/agent (driver-free GPU discovery) |
| `aicrd` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Minimal API server |
| `aicr-gate` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Bundle readiness-gate Job image (emitted by `aicr bundle --readiness-hooks`) |

Published to GitHub Container Registry (`ghcr.io/nvidia/aicr-validators/`):

| Image | Base | Description |
|-------|------|-------------|
| `deployment` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Deployment validator |
| `performance` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Performance validator |
| `conformance` | `nvcr.io/nvidia/distroless/static:v4.0.0` | Conformance validator |
| `aiperf-bench` | `nvcr.io/nvidia/distroless/python:3.13-v4.1.2` | AIPerf benchmark runner (built from `python:3.13-slim`) |

Stable releases promote `vX.Y.Z` and `latest`; prereleases promote their
`vX.Y.Z-rcN` version tags but never `latest`. The release workflow also retains
non-promoted `candidate-<run-id>-<run-attempt>` tags in the public GHCR packages
for audit, diagnosis, and recovery.

### Supply Chain

Every release includes:

- **SLSA Build Level 3 Provenance** — verifiable image build attestations (provenance v1), generated from a reusable workflow
- **SBOM** — Software Bill of Materials (SPDX for the release binaries, CycloneDX for the container images)
- **OpenVEX** — per-platform vulnerability triage, attested alongside each container image SBOM
- **Sigstore Signatures** — keyless signing via Fulcio + Rekor
- **Checksums** — SHA256 for all binaries
- **Third-party notices** — `THIRD_PARTY_NOTICES.md` listing every
  third-party dependency AICR redistributes and embedding the verbatim
  text of each license-bearing file shipped upstream (e.g. `LICENSE`,
  `NOTICE`) where available (generated by `make notices`; uploaded as a
  top-level GitHub release asset). It covers two surfaces: Go modules
  linked into the released binaries (collected via `go-licenses` from the
  Go module cache — `vendor/` until #2374 removed it, which is also why
  each row now links to a version-pinned upstream license rather than an
  in-repo path), and the Python packages installed into the released
  `aiperf-bench` image (collected out-of-band by `make python-licenses`,
  which needs network access to PyPI, then committed as a rendered
  fragment). Note that `make notices` is no longer offline either: a cold
  module cache means it fetches. The Go half
  is the union of the dependency graph across every released OS/arch
  target, generated deterministically so it is byte-identical on macOS and
  Linux. The file is not committed: `make release` depends on `make
  notices`, so it is regenerated from the tag being released and uploaded
  by goreleaser's `release.extra_files`. The `notices-generator`
  merge-gate job runs the generator on dependency changes so a break
  surfaces on that PR rather than blocking a release at tag time

## Versioning

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1` (automatically marked in GitHub)
- **Breaking changes**: Increment MAJOR version

## Verification

### Container Attestations

Verify the **digest-pinned** image that a tag currently resolves to. Tag refs
are registry-rewritable; attestations bind to digests. Requires `crane` (or
substitute `docker buildx imagetools inspect` for digest resolution).

Predicate types attach at two different levels of the image index, so the
digest you verify against depends on what you are asking for:

| Predicate | Attached to | Verify against |
|-----------|-------------|----------------|
| SLSA provenance (`slsaprovenance1`) | multi-arch index | `crane digest <image>:<tag>` |
| SBOM (`cyclonedx`) | per-platform child manifest | `crane digest --platform <os>/<arch> <image>:<tag>` |
| OpenVEX (`openvex`) | per-platform child manifest | `crane digest --platform <os>/<arch> <image>:<tag>` |

Asking for `cyclonedx` or `openvex` against the index digest fails with `none of
the attestations matched the predicate type`.

```bash
set -euo pipefail
TAG=$(gh release view --repo NVIDIA/aicr --json tagName -q .tagName)
[[ -n "${TAG}" ]] || { echo "failed to resolve latest TAG" >&2; exit 1; }

# Resolve immutable digests up front so a missing image / crane failure
# aborts here (set -e) instead of being attributed to a later gh/cosign step.
AICR_INDEX=$(crane digest "ghcr.io/nvidia/aicr:${TAG}")
AICRD_INDEX=$(crane digest "ghcr.io/nvidia/aicrd:${TAG}")
GATE_INDEX=$(crane digest "ghcr.io/nvidia/aicr-gate:${TAG}")
DEPLOY_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/deployment:${TAG}")
PERF_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/performance:${TAG}")
CONF_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/conformance:${TAG}")
AIPERF_INDEX=$(crane digest "ghcr.io/nvidia/aicr-validators/aiperf-bench:${TAG}")

# GitHub CLI (core images) — --source-ref binds the attestation to this tag
gh attestation verify "oci://ghcr.io/nvidia/aicr@${AICR_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicrd@${AICRD_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-gate@${GATE_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# GitHub CLI (validator images)
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/deployment@${DEPLOY_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/performance@${PERF_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/conformance@${CONF_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
gh attestation verify "oci://ghcr.io/nvidia/aicr-validators/aiperf-bench@${AIPERF_INDEX}" --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Cosign — only provenance is on the index. Pin the workflow *and* the exact
# tag ref (same binding as --source-ref above): without
# --certificate-github-workflow-ref, the identity regexp alone would accept
# an attestation signed for any release tag on a digest this tag was
# rewritten to point at.
IDENTITY='^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$'
cosign verify-attestation \
  --type slsaprovenance1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "${IDENTITY}" \
  --certificate-github-workflow-ref "refs/tags/${TAG}" \
  "ghcr.io/nvidia/aicr@${AICR_INDEX}" >/dev/null

# Cosign — the SBOM and the VEX are both on the per-platform child manifest
platform="linux/$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
AICR_CHILD=$(crane digest --platform "${platform}" "ghcr.io/nvidia/aicr@${AICR_INDEX}")
for predicate in cyclonedx openvex; do
  cosign verify-attestation \
    --type "${predicate}" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp "${IDENTITY}" \
    --certificate-github-workflow-ref "refs/tags/${TAG}" \
    "ghcr.io/nvidia/aicr@${AICR_CHILD}" >/dev/null
done
```

### Binary Checksums

`aicr_checksums.txt` lists digests for release archives (and SBOMs). Download
the archive you intend to verify **and** the checksums file into the same
directory, assert the archive is present and non-empty, then check **that**
file’s line — do not use `--ignore-missing` (it can pass with zero files
verified). On macOS, use `shasum -a 256` (built-in); on Linux, `sha256sum`
(GNU coreutils).

```bash
set -euo pipefail
TAG=$(gh release view --repo NVIDIA/aicr --json tagName -q .tagName)
[[ -n "${TAG}" ]] || { echo "failed to resolve latest TAG" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
archive="aicr_${TAG#v}_${os}_${arch}.tar.gz"

tmpdir=$(mktemp -d)
trap 'rm -rf "${tmpdir}"' EXIT
gh release download "${TAG}" -R NVIDIA/aicr -D "${tmpdir}" \
  -p "aicr_checksums.txt" \
  -p "${archive}"

cd "${tmpdir}"
[[ -s "${archive}" ]] || { echo "missing or empty archive: ${archive}" >&2; exit 1; }
[[ -s aicr_checksums.txt ]] || { echo "missing aicr_checksums.txt" >&2; exit 1; }

# Fail closed: verify only the downloaded archive line from the checksums file.
line=$(grep -F "  ${archive}" aicr_checksums.txt) || {
  echo "no checksum entry for ${archive}" >&2
  exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
  printf '%s\n' "${line}" | sha256sum -c -
elif command -v shasum >/dev/null 2>&1; then
  printf '%s\n' "${line}" | shasum -a 256 -c -
else
  echo "need sha256sum (GNU coreutils) or shasum" >&2
  exit 1
fi
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

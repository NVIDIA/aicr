# Release Process

This document describes when, why, and how AICR releases are made. For contribution guidelines, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Cadence

Releases follow a **bi-weekly cadence**.

| Release Type | When | Version Bump | Decision |
|-------------|------|-------------|----------|
| Regular release | Every two weeks | `patch` or `minor` | Maintainer determines bump type based on changes landed |
| Hotfix | Between regular releases, as needed | `patch` | Any maintainer can initiate for critical fixes |
| Pre-release | Before a regular release, as needed | `rc` | Any maintainer can create for testing |
| Major | Planned | `major` | Requires team agreement and advance communication |

## Supported Versions

AICR supports the latest released minor and the one before it. Both receive
security fixes, so you have a full release of overlap to upgrade in. Anything
older is end-of-life: fixes do not reach it, and the upgrade path is forward to
a supported release.

| Version | Status |
|---------|--------|
| `1.0.x` (latest released minor) | Supported: receives security fixes |
| `0.22.x` (previous minor) | Supported: receives security fixes |
| `< 0.22` | End-of-life: upgrade to a supported release |

A fix lands on `main` first and ships in the next release cut from `main`.
Reaching the previous minor also requires the manual path in
[Hotfix Procedure](#hotfix-procedure), because there is no long-lived release
branch. A fix is never backported to an end-of-life version.

Security issues are reported to NVIDIA PSIRT, not through GitHub; see
[SECURITY.md](SECURITY.md) for reporting, embargo, and CVE assignment. Both
files carry this table so each stands alone, and
`TestSupportedVersionsMatchSecurityPolicy` in `tests/releasepolicy` fails when
they disagree.

## Deprecation Policy

AICR freezes four public surfaces at v1
([ROADMAP](ROADMAP.md#api-stability)): the `aicr` CLI, the REST API, the Go SDK
(`pkg/client/v1`), and the bundle layout plus artifact schemas. A change that is
breaking under the table below and ships without the notice it owes is a
release blocker, not a release note.

### What counts as breaking, per surface

| Surface | Breaking | Additive |
|---|---|---|
| **CLI** — flags, subcommands, exit codes, stdout shape | Removing or renaming a flag or subcommand; changing a default such that identical input yields different output; narrowing an accepted value set; changing what an exit code means; removing a field from `--output json`/`yaml` | New flag whose default preserves current behavior; new subcommand; new accepted enum value; new field in structured output |
| **REST** — `api/aicr/v1/server.yaml` | Removing a path or method; removing or renaming a response field; adding a required request field; narrowing a type; removing a value from a request enum | New optional request field; new response field; new path or method; new value in a response enum |
| **Go SDK** — `pkg/client/v1` | Removing or renaming an exported identifier; changing a signature; narrowing a parameter type; changing documented semantics without changing the name | New exported function, method, or type; new functional option; new field on a struct the caller does not construct positionally |
| **Bundle + schemas** — layout and artifact kinds | Removing or renaming a bundle path; removing a schema field; tightening a type; adding a required field; retiring an `apiVersion` | New optional field; new file in the bundle; new artifact kind |

Adding a value to a *response* enum is additive for the server but breaks a
client that switches exhaustively on it, so it is announced but owes no window.
Adding a value to a *request* enum is always additive; removing one is always
breaking.

### Criteria enums name recognized values, not covered ones

The recipe criteria enums (`service`, `accelerator`, `os`, `intent`,
`platform`, in `api/aicr/v1/server.yaml` and `pkg/recipe/criteria.go`) list the
values AICR *recognizes*, not the values a recipe covers. A recognized value
with no recipe resolves to `INVALID_REQUEST` naming the gap, which is intended:
the request was well-formed, and coverage does not exist.

At v1.0.0, `os=rhel`, `os=amazonlinux`, `os=talos`, `service=metal3`, and
`platform=runai` carry no recipe. This is a recorded decision. **Do not remove
them as cleanup**: removing a request enum value is breaking on both the CLI and
REST surfaces and requires the next major. Coverage arrives by adding a recipe,
which is additive.

### Notice owed before removal

A breaking removal on any of the four frozen surfaces requires the next
`vMAJOR`. The deprecation may be announced at any time; the removal waits for
the major. A release manager cannot waive this. Artifact `apiVersion`
retirement has its own maturity-scoped window, defined in
[Artifact Compatibility and Deprecation](#artifact-compatibility-and-deprecation).

**Closing a fail-open gate is not a deprecation.** When two enforcement paths
disagree about the same document and one already rejects it, aligning the
permissive path to the strict one owes no window: the permissive behavior was a
defect, not a contract. Record the change in
[`docs/user/deprecations.md`](docs/user/deprecations.md) with its real release,
and its rationale in the governing ADR. The one precedent is v0.21, where the
direct-input path started rejecting an empty `apiVersion` that the catalog
scanner already rejected (ADR-022 §3,
[#2421](https://github.com/NVIDIA/aicr/issues/2421)). This does not cover
tightening validation that both paths previously accepted, which is an ordinary
breaking change and owes the full notice.

### How a deprecation is announced

Every deprecation appears in all three places. Release notes are read once, the
durable page is read later by someone debugging, and the runtime warning reaches
the user who read neither.

1. A `### Deprecations` section in the release notes for the release that
   introduces it, naming the replacement and the planned removal release. It is
   an h3 sibling of `### Highlights`, the level the release-notes skill
   (`.agents/skills/aicr-release-notes/SKILL.md`) emits.
2. An entry on [`docs/user/deprecations.md`](docs/user/deprecations.md), which
   carries every active deprecation and its removal release until the removal
   ships.
3. A runtime warning on the affected surface:

   | Surface | Mechanism |
   |---|---|
   | CLI | Warning on stderr naming the replacement and the removal release. Honors `NO_COLOR` and the existing logger conventions |
   | REST | A `Deprecation` response header ([RFC 9745](https://www.rfc-editor.org/rfc/rfc9745.html)) carrying the deprecation date, a `Sunset` header ([RFC 8594](https://www.rfc-editor.org/rfc/rfc8594.html)) carrying the removal date, a `Link` with `rel="deprecation"`, and `deprecated: true` on the operation in `api/aicr/v1/server.yaml` |
   | Go SDK | A `// Deprecated:` godoc marker, which `staticcheck` surfaces to consumers automatically |
   | Bundle + schemas | The loader accepts the deprecated shape and warns, naming the file and the release that stops reading it |

#### A deprecated parameter value, not a deprecated operation

The REST row above describes a deprecated operation. When only one value or
spelling of a parameter is deprecated, and the operation and the parameter
itself remain supported, the notice is scoped to that value:

- The operation is not marked `deprecated: true`, and neither is the parameter.
  Its description in `api/aicr/v1/server.yaml` names the deprecated value and
  its replacement.
- Only responses to requests that use the deprecated value carry the headers.
  The `Link` points at the value's own entry on `docs/user/deprecations.md`
  (`deprecation.Notice.Anchor`), not the top of the page.
- `Deprecation` is sent only when the notice records a deprecation date, and
  `Sunset` only once a removal date is committed. Neither header admits a
  placeholder, so a notice with no dates sends the `Link` alone.
- The CLI warning omits the removal release while none is scheduled, as the
  existing `docs/user/deprecations.md` entries do.

The first deprecation to use this form is the `deployer:<key>` value override
on `set` ([#3264](https://github.com/NVIDIA/aicr/issues/3264)).

The CLI and REST arms have unit coverage in `pkg/deprecation` and `pkg/server`.
They first carried a real deprecation with the `deployer:<key>` value override
above; no REST route is marked deprecated and no CLI flag or subcommand is
deprecated. The loader arm ran end to end in
the ADR-022 alpha migration (warn in v0.22,
[#2416](https://github.com/NVIDIA/aicr/issues/2416); reject in v1.0.0,
[#2417](https://github.com/NVIDIA/aicr/issues/2417)). Treat the first change
that owes a window as the end-to-end test of the channel, and fix what it
exposes then.

## Artifact Compatibility and Deprecation

Artifact `apiVersion` maturity is independent of the AICR release version and
is governed by [ADR-022](docs/design/022-artifact-maturity-and-deprecation.md).
Retiring an artifact version owes this window, counted in AICR releases:

- Alpha: no deprecation window.
- Beta: readable for two releases after deprecation.
- GA: readable for the rest of the current AICR major version; removal requires
  the next `vMAJOR` release.

For beta and GA bumps, ship the new reader in release N before switching the
emitter in release N+1. Release notes must name the deprecation, the last
release that reads the retiring version, and the required artifact recapture,
regeneration, or authored-header edit.

The initial alpha-to-target migration (ADR-022 §3) completed in v1.0.0:

| Release | Reads | Emits | Tracking |
|---|---|---|---|
| v0.21 | alpha and target | alpha | [#2404](https://github.com/NVIDIA/aicr/pull/2404) |
| v0.22 | alpha and target | target | [#2416](https://github.com/NVIDIA/aicr/issues/2416) |
| v1.0.0 | target only | target | [#2417](https://github.com/NVIDIA/aicr/issues/2417) |

The consumer-facing form of this table, with per-kind target values, is in
[`docs/integrator/data-extension.md`](docs/integrator/data-extension.md#catalog-and-binary-compatibility).

## What Goes Into a Release

A release ships everything merged to `main` since the last tag. There is no
cherry-picking and no release branch, with one exception: a security fix for
the previous supported minor uses the manual path in
[Hotfix Procedure](#hotfix-procedure).

**Before cutting a release:**

- All CI checks pass on `main` (`make qualify`)
- No known regressions since the last release
- Breaking changes use the `feat!:` or `fix!:` prefix (drives the changelog and signals consumers)

**After a minor release publishes:**

- Bump the supported minors and the end-of-life threshold in **both**
  [Supported Versions](#supported-versions) and `SECURITY.md`.
  `TestSupportedVersionsMatchSecurityPolicy` catches the two files disagreeing,
  but not both going stale together. Patch releases change neither.
- Move any deprecation whose removal shipped in this release from `## Active`
  to `## Removed` in [`docs/user/deprecations.md`](docs/user/deprecations.md).
  Nothing gates this. The entry stays under `## Active` until the tag lands,
  because until then no released binary behaves the new way.

## Quality Gates

Every release must pass these automated gates before artifacts are published:

- Unit tests with race detector
- golangci-lint + yamllint
- License header verification
- Vulnerability scans (Anchore in release workflows, Grype in `make scan`)
- E2E tests: hermetic Chainsaw CLI suites (`--no-cluster`) and the `aicrd` + CLI suite on a Kind cluster
- Per-platform vulnerability scans of the exact candidate image digests
- SLSA Build Level 3 provenance for those same digests

Container builds first publish only a run-unique
`candidate-<run-id>-<run-attempt>` tag. Version aliases, `latest`, and the
public GitHub release stay unchanged until all seven candidate digests pass
their gates. Homebrew publication starts only after the GitHub release is
public. If a gate fails, the candidate tags stay available for diagnosis but
are not promoted.

## How to Release

### Standard Release

```bash
git checkout main
git pull origin main
make qualify          # Verify locally before releasing

make bump-patch       # v1.2.3 → v1.2.4
# or
make bump-minor       # v1.2.3 → v1.3.0
```

`make bump-*` checks for a clean, pushed tree, computes the next version from
the latest **local** tag, tags `HEAD`, and pushes the tag, which starts the
release pipeline. No commits are created. `make bump-major`, `bump-minor`, and
`bump-patch` count from the latest **stable** tag, so a leftover RC of the next
version does not change the result; `bump-rc` counts from the latest tag of any
kind so that it can increment an existing RC. `make changelog` previews the
changes since the last stable tag; the changelog is generated for the GitHub
release and not committed.

### Release Candidates

Cut an RC to validate a release before it goes stable:

```bash
make bump-rc          # v1.2.3 → v1.3.0-rc1 (always the next minor)
# fix issues on main, then cut another as needed
make bump-rc          # v1.3.0-rc1 → v1.3.0-rc2
```

The release workflow accepts only `vX.Y.Z` and `vX.Y.Z-(alpha|beta|rc)N` tags
(a dot before `N` is allowed), the versions the AICR Operator's release builds
accept. An RC runs the full build, test, scan, and attest pipeline and gets
image tags spelled exactly as its git tag (`vX.Y.Z-rcN` or `vX.Y.Z-rc.N`), but
it does not update:

- the Homebrew formula
- container `:latest` tags
- site documentation versions (the Fern publish runs, but a pre-release
  registers no new docs version)

Slack notifications fire for both pre-releases and stable releases.

### Promoting an RC to GA

Promote the RC that was validated. The RC tag must exist locally and
`origin/main` must be current, so fetch first:

```bash
git fetch origin --tags
make bump-promote TAG=v1.3.0-rc2     # tags v1.3.0 on the RC's commit and pushes it
```

`make bump-promote` tags the RC's own commit, so GA ships exactly what the RC
validated. Anything merged to `main` after the RC waits for the next release.
The commit must be reachable from `origin/main`; otherwise the target refuses.
The GA tag runs the full pipeline and rebuilds from source; it does not reuse
the RC's images, which stay on GHCR.

Leave the RC releases and tags in place. They are history, and nothing has to
be deleted before tagging GA:

- **Release tag.** After `make bump-promote` the GA tag and its RC tag point at
  the same commit, and git version-sorts `v1.3.0-rc2` ahead of `v1.3.0`. Left
  alone, GoReleaser would take the RC as the tag being released and build the
  GA run as that RC. For a stable tag, the release workflow sets
  `GORELEASER_CURRENT_TAG` to the tag that triggered the run, so GA is always
  released as GA.
- **Release notes.** For a stable tag, the release workflow sets
  `GORELEASER_PREVIOUS_TAG` to the latest stable tag before the GA commit, so
  the GA notes always cover everything since the previous stable release. They
  do not start at an RC, whether GA is tagged on the RC's commit or on a later
  one. RC notes are unchanged: they cover the changes since the previous tag of
  any kind, so `rc2` lists what landed after `rc1`.
- **Version.** `make bump-major`, `bump-minor`, and `bump-patch` ignore RC tags,
  so a leftover `v1.3.0-rc2` does not turn `make bump-minor` into `v1.4.0`.

To ship `HEAD` instead of the RC's commit, for example because a fix landed
after the RC, tag it as an ordinary release:

```bash
make bump-minor       # v1.2.3 → v1.3.0, even with v1.3.0-rc2 present (bump-major for a new major)
```

That release includes everything merged after the RC, so it ships without RC
validation of those changes.

### Re-run Existing Release

- **Re-run failed jobs** recovers a transient failure, and is the required path
  after a partial cross-repository alias promotion. Successful upstream jobs
  keep the candidate tag emitted by `detect`, so promotion converges on the same
  digest set. If GoReleaser left a partial exact-tag draft, the rerun reuses it
  only when its name and tag both equal the release tag, its pre-release state
  matches the tag, and every asset belongs to the release's fixed asset set
  (`expected_release_asset_names` in `.github/scripts/release-images.sh`: the
  GoReleaser assets and the four `kubectl-aicr` binaries). It then replaces expected assets, uploads missing ones, and regenerates
  release notes from the current tag. Unexpected, duplicate, or malformed
  assets fail closed for maintainer inspection. The generated Homebrew formula
  is kept for GitHub's 30-day workflow-rerun window.
- **Re-run all jobs** creates a new run attempt and so a new candidate tag. Use
  it only before any public alias has moved, or when a rebuild is intended.
  Preflight fails rather than overwrite an immutable version alias that points
  at a different digest. Re-running a failed `detect` also creates a new
  candidate tag.
- Once the exact-tag GitHub release is public, the build fails closed rather
  than modify its assets; cut a new tag for any further release. Publication
  revalidates the tag commit and that exact asset set, then publishes by the
  validated numeric release ID, never by a mutable name or tag. If GitHub made
  the release public but the response was lost, a failed-job rerun accepts it
  after revalidating source, identity, pre-release state, and assets, and does
  not publish a second time.

## Hotfix Procedure

For critical fixes between regular releases:

1. Fix on `main` first (PR, review, merge as normal).
2. Cut a patch release: `make bump-patch`.
3. To reach the previous supported minor (see
   [Supported Versions](#supported-versions)), cherry-pick the fix onto a
   branch cut from that minor's latest tag and tag it manually. Step 2 only
   patches the latest minor.

**Bring the release tooling forward with the fix.** A tag push runs the
workflows from the pushed ref, so a branch cut from an older tag builds, scans,
and attests with that tag's `.github/` tree, not `main`'s. For example,
[#2729](https://github.com/NVIDIA/aicr/pull/2729) (between v0.21.1 and v0.22.0)
added SLSA provenance on each platform manifest; a hotfix cut from v0.21.1
without it publishes weaker provenance than
[SECURITY.md](SECURITY.md#supply-chain-security) promises. Cherry-pick any
`.github/workflows/**` and `.github/actions/**` change affecting build, scan, or
attestation before tagging, and check that the published attestations match a
current release.

## Release Pipeline

```
Tag Push --> CI --> Candidate Images --> Resolve Digests --> Scan + Attest --> Promote Aliases --> Publish
```

The workflow resolves one authoritative seven-image digest map, scans both
architectures of each digest, and generates provenance and per-platform SBOMs
before promotion. A read-only preflight checks every candidate, attestation,
version alias, and `latest` alias before the first registry write. Stable
releases also fail closed if the same or a newer stable version is already
public, even if registry aliases were changed out of band.

Promotion creates and verifies all seven immutable version aliases before a
stable release touches `latest`. Promotion across seven GHCR repositories is not
transactional, so a registry failure while updating `latest` can leave a mix of
prior and current aliases. Re-running the failed jobs with the same candidate is
idempotent and finishes only the remaining aliases. A repository-wide
concurrency group serializes promotion, but GitHub Actions keeps at most one
pending run and may replace an older one, so confirm the surviving run belongs
to the intended release before retrying.

Candidate and per-architecture candidate tags are intentionally retained.
Automated cleanup waits on a reviewed policy for shared-manifest deletion and
package-storage growth.

## Released Artifacts

### Binaries

Built via GoReleaser:

| Binary | Platforms | Description |
|--------|-----------|-------------|
| `aicr` | darwin/amd64, darwin/arm64, linux/amd64, linux/arm64 | CLI tool |
| `aicrd` | linux/amd64, linux/arm64 | API server |

### Container Images

Published to GitHub Container Registry for linux/amd64 and linux/arm64. Base
images are pinned in `.ko.yaml` and each validator's Dockerfile.

| Image | Description |
|-------|-------------|
| `ghcr.io/nvidia/aicr` | Pure-Go CLI/agent (driver-free GPU discovery) |
| `ghcr.io/nvidia/aicrd` | Minimal API server |
| `ghcr.io/nvidia/aicr-gate` | Bundle readiness-gate Job image (emitted by `aicr bundle --readiness-hooks`) |
| `ghcr.io/nvidia/aicr-validators/deployment` | Deployment validator |
| `ghcr.io/nvidia/aicr-validators/performance` | Performance validator |
| `ghcr.io/nvidia/aicr-validators/conformance` | Conformance validator |
| `ghcr.io/nvidia/aicr-validators/aiperf-bench` | AIPerf benchmark runner |

Stable releases promote `vX.Y.Z` and `latest`; pre-releases promote
`vX.Y.Z-rcN` but never `latest`. Non-promoted
`candidate-<run-id>-<run-attempt>` tags stay in the public packages for audit,
diagnosis, and recovery.

### AICR Operator (preview)

The [AICR Operator](docs/operator/index.md) ships at the release's version
outside the image promotion above: `operator-candidate` builds it from the tag
(`make operator-candidate`) and malware-scans the `kubectl-aicr` binaries,
`operator-scan` scans the image on both architectures, and
`operator-publish` runs only after the release integrity check. It publishes:

| Artifact | Where |
|----------|-------|
| Operator image | `ghcr.io/nvidia/aicr-operator:vX.Y.Z`, with build provenance on the index and an SPDX SBOM attestation on each platform image, in the registry |
| Helm chart | `oci://ghcr.io/nvidia/charts/aicr-operator`, version `X.Y.Z`, with build provenance in the registry |
| `kubectl-aicr` | Release assets `kubectl-aicr-<os>-<arch>` for darwin and linux on amd64 and arm64, malware-scanned and attested |

The chart defaults to the operator image by digest and to the AICR release the
operator was qualified against (`operator:` in `.settings.yaml`).
`release-check` fails closed on the operator candidate and its scans, so neither
the image aliases above nor the operator are published when they fail, and
`publish` waits for `operator-publish`. Publishing the same candidate again is
a no-op, so re-running the failed jobs converges; a tag that already names
other content is refused. The operator image takes no `latest` alias.

The first release that publishes the operator creates the `aicr-operator` and
`charts/aicr-operator` packages. If GHCR creates them private, an organization
administrator makes both public once, as for the other images, so users can
pull and verify them anonymously.

The operator's attestations are generated in `on-tag.yaml` itself, not in the
reusable `attest-images.yaml`, so they are SLSA Build Level 2. Verify them
against that workflow and the tag:

```shell
gh attestation verify "oci://ghcr.io/nvidia/charts/aicr-operator:${VERSION}" --repo NVIDIA/aicr \
  --signer-workflow NVIDIA/aicr/.github/workflows/on-tag.yaml --source-ref "refs/tags/v${VERSION}"
```

### Supply Chain

Every release includes, for the AICR binaries and the seven images above:

- **SLSA Build Level 3 Provenance**: verifiable image build attestations (provenance v1), generated from a reusable workflow
- **SBOM**: SPDX for the release binaries, CycloneDX for the container images
- **OpenVEX**: per-platform vulnerability triage, attested alongside each container image SBOM
- **Sigstore Signatures**: keyless signing via Fulcio + Rekor
- **Checksums**: SHA256 for all binaries
- **Third-party notices**: `THIRD_PARTY_NOTICES.md`, a top-level release
  asset listing every third-party dependency AICR redistributes, with the
  verbatim text of its license files. It covers the Go modules linked into the
  release binaries (the union across every released OS/arch target,
  byte-identical on macOS and Linux) and the Python packages in the
  `aiperf-bench` image (a committed fragment refreshed by
  `make python-licenses`). The file is not committed: `make release` runs
  `make notices` to regenerate it from the tag. That needs network access, and
  `GITHUB_TOKEN` keeps its license-URL probes from being rate-limited. The
  `notices-generator` merge-gate job runs it on dependency changes, so a break
  shows up on the PR rather than at tag time.

The AICR Operator preview's artifacts carry less: Level 2 provenance on the
image index, the chart and each `kubectl-aicr` binary, an SPDX SBOM attestation
on each operator platform image, no OpenVEX, and no `aicr_checksums.txt` entry
for the plugin, whose attestations bind its digests
([AICR Operator (preview)](#aicr-operator-preview)).

## Versioning

- **Semantic versioning**: `vMAJOR.MINOR.PATCH`
- **Pre-releases**: `v1.2.3-rc1` (automatically marked in GitHub)
- **Breaking changes**: Increment MAJOR version

## Verification

### Container Attestations

Verify the **digest-pinned** image a tag currently resolves to: tags can be
rewritten in the registry, attestations bind to digests. Requires `crane` (or
`docker buildx imagetools inspect` for digest resolution).

Predicates attach at two levels of the image index, so the digest to verify
against depends on the predicate:

| Predicate | Attached to | Verify against |
|-----------|-------------|----------------|
| SLSA provenance (`slsaprovenance1`) | multi-arch index **and** each per-platform child manifest | `crane digest <image>:<tag>`, or `crane digest --platform <os>/<arch> <image>:<tag>` |
| SBOM (`cyclonedx`) | per-platform child manifest | `crane digest --platform <os>/<arch> <image>:<tag>` |
| OpenVEX (`openvex`) | per-platform child manifest | `crane digest --platform <os>/<arch> <image>:<tag>` |

Asking for `cyclonedx` or `openvex` against the index digest fails with `none of
the attestations matched the predicate type`.

```bash
set -euo pipefail
TAG=$(gh release view --repo NVIDIA/aicr --json tagName -q .tagName)
[[ -n "${TAG}" ]] || { echo "failed to resolve latest TAG" >&2; exit 1; }

# GitHub CLI: --source-ref binds the attestation to this tag
for image in aicr aicrd aicr-gate \
  aicr-validators/deployment aicr-validators/performance \
  aicr-validators/conformance aicr-validators/aiperf-bench; do
  digest=$(crane digest "ghcr.io/nvidia/${image}:${TAG}")
  gh attestation verify "oci://ghcr.io/nvidia/${image}@${digest}" \
    --repo NVIDIA/aicr \
    --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml \
    --source-ref "refs/tags/${TAG}"
done

# Cosign: provenance is the only predicate on the index, which is what an
# admission policy can reach. --certificate-github-workflow-ref pins the exact
# tag; the identity regexp alone would accept an attestation signed for any
# release tag on a digest this tag was rewritten to point at.
AICR_INDEX=$(crane digest "ghcr.io/nvidia/aicr:${TAG}")
IDENTITY='^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$'
cosign verify-attestation \
  --type slsaprovenance1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "${IDENTITY}" \
  --certificate-github-workflow-ref "refs/tags/${TAG}" \
  "ghcr.io/nvidia/aicr@${AICR_INDEX}" >/dev/null

# Cosign: the SBOM and the VEX are on the per-platform child manifest
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
file's line. Do not use `--ignore-missing`: it can pass with zero files
verified. On macOS use `shasum -a 256` (built in); on Linux, `sha256sum` (GNU
coreutils).

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

## Troubleshooting

| Problem | Action |
|---------|--------|
| Tests fail during release | Fix on `main`, cut new tag |
| Lint errors | Run `make lint` locally before releasing |
| Image push failure | Check GHCR permissions |
| GA release notes list only changes since an RC | The `Pin the release tag and the previous stable tag` step in the `build-ko` job should have set `GORELEASER_PREVIOUS_TAG`; check that it ran. It is skipped for pre-releases, and sets no previous tag when no earlier stable tag exists. See [Promoting an RC to GA](#promoting-an-rc-to-ga) |
| A GA run builds or publishes as its RC (RC version in the artifacts, or a conflict with the existing RC release) | The same step should have set `GORELEASER_CURRENT_TAG` to the GA tag; check its log. Without it, GoReleaser picks the RC when both tags are on one commit |
| `make bump-major`, `bump-minor`, or `bump-patch` computes an unexpected version | They count from the latest stable tag reachable from `HEAD` in your local clone. Run `git fetch origin --tags`, then `git describe --tags --abbrev=0 --match='v[0-9]*' --exclude='*-*'` to see the base |
| Promotion partially completed | Re-run failed jobs for the same workflow run; do not repoint aliases manually |
| Version alias conflict | Stop and verify the existing digest; the workflow intentionally refuses overwrite |
| Draft identity or asset check fails | Inspect the exact-tag draft; correct the name/tag or remove only verified stale assets, then re-run failed jobs |
| Need a full rebuild | Re-run all jobs only before public aliases move; this creates a new candidate tag |

## Prerequisites

- Repository admin access with write permissions
- Access to GitHub Actions workflows
- [git-cliff](https://git-cliff.org/) installed for `make changelog` (`make tools-setup`)

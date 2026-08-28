# ADR-023: Remove vendor/ and Resolve Dependencies Through the Go Proxy

## Status

**Accepted** — 2026-08-25. Implements [#2374](https://github.com/NVIDIA/aicr/issues/2374).

Sequenced after [#2372](https://github.com/NVIDIA/aicr/issues/2372) (adopt the
DGXC Go proxy) and [#2375](https://github.com/NVIDIA/aicr/issues/2375) (move it
from `routed` to `enforced`), both closed, and after
[#2384](https://github.com/NVIDIA/aicr/pull/2384), which made
`THIRD_PARTY_NOTICES.md` complete and changed its coverage gate to match on
module identity rather than path prefix — the property that lets notices
generation survive the source moving from `vendor/` to the module cache.

## Decision Summary

AICR stops committing `vendor/` (8,853 files, ~99 MB of a 176 MB repository) and
resolves Go dependencies through the module proxy instead. Release builds resolve
through the DGXC Artifactory proxy in `enforced` mode; everything else uses the
public proxy.

## Context

`vendor/` was 56% of the repository. The three usual arguments for vendoring do
not survive contact with this repo:

**Reviewable — not in practice.** A representative dependency bump touched 401
files for +34,657 / −8,796 lines. A diff that size is approved, not read.

**Verifiable — false.** Go validates `vendor/modules.txt` against `go.mod`, but
it never hashes vendored files against `go.sum`. Appending a line to a file under
`vendor/` and running `GOFLAGS=-mod=vendor go build ./cmd/aicr` exits 0 — the
tampering compiles in silently. The only thing catching a modified `vendor/` was
CI's `go mod vendor && git diff --exit-code vendor/`, which itself requires a
network fetch. The integrity property depended on the round trip that vendoring
was supposed to remove.

**Hermetic — true, and the only real one.** Addressed under Consequences.

There was also a claim the repo could not make honestly. `.goreleaser.yaml`
pinned `GOFLAGS=-mod=vendor` on both binaries and set `gomod: proxy: false`, so a
release build contacted no proxy at all. "Release dependencies flow through
Artifactory" was aspirational. Removing `vendor/` is what makes it literal.

## Decision

1. Delete `vendor/` and drop all 93 `-mod=vendor` pins across 50 files.
2. Drop the `go mod vendor` + `git diff` check from `go-test`. Keep
   `go mod tidy -diff` — it answers a different question (are the manifests
   correct for the source?) that the vendor check never covered, because
   `go mod vendor` regenerates from `go.mod` and so treats it as input rather
   than subject.
3. Route every Go release build through Artifactory in `enforced` mode: the
   GoReleaser/ko binaries, all three Go validator images, and the gate image.
4. Retire `.github/workflows/dgxc-goproxy-probe.yaml`. It existed to prove a
   property about a proxy nothing built from; now a failed build is the check.
   Only its GOPROXY assertion carries over — that one catches what a build
   cannot, namely the action silently no-opping or a revert to `routed`, either
   of which looks healthy while routing nothing.
5. Add a credential-free pre-merge check for dependency bumps
   (`dependency-resolvability.yaml`).

### Ordering constraints that are load-bearing

**The proxy must be configured before the first `go` command that touches
application dependencies.** In `build-ko` that is the `go run ./cmd/aicr` inside
`generate-slsa-predicate`, not the goreleaser build. Configure it later and that
earlier command warms the module cache from the public proxy; the release build
then finds everything cached and fetches nothing, and a green run proves nothing
about coverage. The release jobs therefore disable `setup-go` module-cache
restoration and use a job-private `GOMODCACHE`; the measured fetch must start
cold.

**The credential is short-lived, so the graph is pulled immediately.** The
OIDC-minted Artifactory token lasts roughly 15 minutes against a 30-minute job
that installs cosign, generates a SLSA predicate, downloads six release tools and
cross-builds six binaries with retrying attestation hooks. A `go mod download`
placed directly after the mint removes the timing dependency entirely: every
later `go` command in the job resolves from a warm cache and needs no credential.
The one straggler — the `go install` of `go-licenses`, a separate module the warm
cache does not cover — is handled by re-minting before it. That is free: GitHub's
OIDC request credential is valid for the life of the job, so the exchange can be
repeated without new permissions or a stored secret.

**Container builds cannot reach this proxy at all.** `setup-dgxc-goproxy`
exports `GOPROXY`/`GOSUMDB`/`GOAUTH` to `GITHUB_ENV` and writes its credential to
`$RUNNER_TEMP`; neither crosses into a BuildKit stage, and the credential is a
bearer token, so `--build-arg` is not an option (build args are recorded in image
history). A builder stage would therefore resolve from the public proxy while
every assertion around it looked healthy. The gate and three Go validator
Dockerfiles accordingly no longer compile anything: their binaries are built on
the runner, where the proxy is configured, and the Dockerfiles only package the
result. That makes coverage true by construction and, as a side effect, removes
network egress from those image builds.

## Consequences

### Gained

- Repository drops ~56%. Dependency diffs stop drowning review signal.
- Dependabot and Renovate PRs become two-file diffs; the `make tidy` re-vendor
  step and the vendor-sync check both disappear (~48 commits of friction per
  quarter).
- No `vendor/` merge conflicts on concurrent dependency changes.
- Every module is now verified against `go.sum` and `sum.golang.org` on every
  build — cryptographic verification against a public transparency log, with no
  CI step to forget and nobody reading 34k-line diffs.

### Lost

**An Artifactory outage can stop a release.** Under `enforced`, a 401/403/429/5xx
stops rather than falling through; `,direct` is a *not-found* fallback, not an
outage fallback. Three things bound this: `edge.urm.nvidia.com` is
CDN-distributed; every AICR dependency is a public module, so Artifactory is a
cache rather than a sole source and a one-line change points back at the public
proxy; anything shorter is transient. It delays a release; it cannot prevent one.

**Offline development needs a warm module cache.** Already true for anyone who
has run `make tidy`.

**`make notices` is no longer offline.** It collects from the module cache rather
than a committed tree, so a cold cache means it fetches. Two knock-on effects,
both handled: license files copied out of the module cache arrive read-only
(0444/0555), so the merge step restores `u+w` on its own scratch trees before
merging platforms; and every row in `THIRD_PARTY_NOTICES.md` now links to a
version-pinned upstream license instead of an in-repo `blob/HEAD/vendor/...`
path, which is both necessary (the old paths would 404) and an improvement (the
old ones were pinned to `HEAD`, not to a version).

**Weaker pre-merge signal on dependency bumps.** This is the sharpest edge and is
mitigated rather than solved. Dependabot PRs run with a read-only token, no
secrets, and no `id-token: write`, so they cannot mint the OIDC credential the
proxy needs — the same class of restriction fork PRs have. A check that talks to
Artifactory therefore cannot run on the PRs that change dependencies most often
(Dependabot authored 23 of 48 vendor-touching commits in 90 days).

`dependency-resolvability.yaml` closes most of the gap without a credential: it
asserts that every module version newly added to `go.sum` resolves on the public
proxy and is present in the public checksum database. Because `dgxc-go-virtual`
is a *virtual* repository that re-fetches from upstream on a miss, a version that
resolves publicly is one Artifactory can serve on demand.

It is deliberately not equivalent. Two cases still reach `main` unflagged: a
module Artifactory deliberately withholds (pulled for a security reason — where
not serving it is the intended behavior), and Artifactory being down or evicting
mid-flight. Neither is detectable without a credential. The strong check still
exists, it just runs later: the release build resolves in `enforced` mode, so an
unserved module fails there loudly and by name.

The rejected alternative was giving Dependabot a static Artifactory token via
Dependabot secrets. It would give a real pre-merge signal, at the cost of
reintroducing exactly the managed long-lived credential the OIDC design exists to
avoid.

### Explicitly not consequences

- **Rebuilding old releases from git alone.** Go modules are immutable and the
  public mirror is append-only, so old versions stay fetchable. Signed artifacts,
  provenance, and SBOMs are published besides; verification does not depend on
  re-deriving the build from source.
- **Artifactory eviction.** A virtual repo re-fetches from upstream on a miss, so
  eviction is not loss. When eviction is deliberate — a module pulled for
  security — not re-downloading it is the point.

# Supply Chain Verification

This guide is for integrators wiring AICR artifact verification into CI
pipelines, clusters, and audit tooling. It collects the full command
walkthroughs for verifying build provenance, SBOMs, and image/bundle
attestations, plus admission-policy enforcement and offline/air-gapped
verification.

For a quick trust overview and how to report a vulnerability, see the
top-level [`SECURITY.md`](../../SECURITY.md).

Everything below drives verification from the shell. To do the same
from Go — verifying bundles, evidence, the recipe catalog, and the
`aicr` binary itself — see
[Verifying artifacts](./go-library.md#verifying-artifacts) in the Go
library guide. The two paths run the same verification code, so a CI
gate written either way reaches the same verdict.

## Prerequisites and Setup

Verification uses [Cosign](https://docs.sigstore.dev/cosign/system_config/installation/),
the [GitHub CLI](https://cli.github.com/) (`gh`), `crane` (recommended; `docker inspect` resolves a digest only after a local pull),
`jq`, [ORAS](https://oras.land/docs/installation) (only for retrieving the
third-party source archive), and — for in-cluster enforcement — `kubectl`.
Binary and bundle verification (`aicr verify`) need only the `aicr` binary.

**Cosign version.** AICR release signatures (the binary attestation and the
signed recipe catalog) are recorded in **Rekor v2** as of the v2 cutover (see the
release notes for the exact version). Verifying those bundles **with Cosign**
requires **Cosign v3.0.1+**: older Cosign cannot parse a Rekor v2
inclusion proof or its RFC3161 timestamp. `aicr verify` needs only the `aicr`
binary and verifies both v1 and v2 transparently, so the Cosign floor does not
apply to it. Releases published before the cutover are in Rekor v1 and verify
with any recent Cosign. The verification *commands* are identical either way: a
bundle self-describes which log it is in, so nothing in your workflow changes
beyond the Cosign version. For why AICR signs to Rekor v2 and how the signing
path works, see [Rekor v2 Signing](../contributor/rekor-v2-signing.md).

Export the following variables once; the rest of this guide reuses them.
Tags are mutable and can be repointed to a different image, so resolve the
tag to an immutable `@sha256:` digest and verify against the digest.

```shell
# Latest release tag
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
export VERSION=${TAG#v}  # strip leading 'v' for release filenames

# CLI image
export IMAGE="ghcr.io/nvidia/aicr"
export DIGEST=$(crane digest "${IMAGE}:${TAG}")   # crane required; `docker inspect` only resolves a digest after `docker pull`
export IMAGE_DIGEST="${IMAGE}@${DIGEST}"
export IMAGE_SBOM="$IMAGE:sha256-$(echo "$DIGEST" | cut -d: -f2).sbom"

# API server image
export IMAGE_API="ghcr.io/nvidia/aicrd"
export DIGEST_API=$(crane digest "${IMAGE_API}:${TAG}")
export IMAGE_API_DIGEST="${IMAGE_API}@${DIGEST_API}"
```

**Authentication** (if the registry requires it):

```shell
docker login ghcr.io
```

## Unified Metadata Retrieval

Release metadata AICR publishes for a container image is attached to that image
as an OCI referrer. The first three kinds below are signed in-toto attestations.
The source archive is a plain OCI artifact rather than an attestation, but it is
Sigstore-signed like the rest, so it is verified with `cosign verify` instead of
`cosign verify-attestation`. There is one retrieval command per kind, and each
has a fixed subject:

| Metadata | Predicate type | Subject | Retrieved with |
|----------|----------------|---------|----------------|
| SLSA build provenance | `https://slsa.dev/provenance/v1` | multi-platform index digest | `gh attestation verify` |
| CycloneDX SBOM | `https://cyclonedx.org/bom` | per-platform manifest digest | `cosign verify-attestation --type cyclonedx` |
| OpenVEX | `https://openvex.dev/ns` | per-platform manifest digest | `cosign verify-attestation --type openvex` |
| Third-party source (`aiperf-bench` only) | `application/vnd.nvidia.aicr.source.v1+tar` | multi-platform index digest | `oras pull` — see [Third-Party Source Code](#third-party-source-code) |

The subjects differ because the claims differ. Provenance describes the build
that produced the whole release image, so it belongs on the index. The SBOM and
the VEX each describe exactly one root filesystem, so both are attached to that
platform's own manifest digest; querying the index digest for either returns
nothing. That also matches where they are looked for: a consumer that resolved
`linux/amd64` enumerates referrers on the child manifest, and evidence on the
index is invisible from there.

The two per-platform documents are deliberately in different formats. A
referrer descriptor carries only `digest`, `mediaType`, `size`, `artifactType`,
and `annotations`, with no name, so two CycloneDX predicates on one manifest
would be indistinguishable in a listing without fetching every payload.
Publishing the VEX as OpenVEX gives the two documents distinct predicate types,
so the listing is self-describing.

Read the two documents for what they each claim. The SBOM is an inventory of
what is in the image. The VEX is a record of the CVEs AICR has triaged and
dismissed, so an **empty VEX means "no exceptions asserted", not "no
vulnerabilities"**, and most images ship an empty one. See
[Using the published VEX document](#using-the-published-vex-document).

Resolve the digests first. `crane digest` without `--platform` returns the index
digest; with `--platform` it returns that platform's child manifest digest.
Resolve the platform digests **from the pinned index**, not from the tag: a tag
repointed between the two lookups would yield a platform digest belonging to a
different index than the one the provenance and VEX are verified against, and
a wildcard signer pattern would still accept that other release's validly
signed SBOM. This is the same resolution the
[`attest-image-from-tag`](https://github.com/NVIDIA/aicr/blob/main/.github/actions/attest-image-from-tag/action.yml)
action performs at publication time.

```shell
export IMAGE="ghcr.io/nvidia/aicr"
export DIGEST=$(crane digest "${IMAGE}:${TAG}")
export DIGEST_AMD64=$(crane digest --platform linux/amd64 "${IMAGE}@${DIGEST}")
export DIGEST_ARM64=$(crane digest --platform linux/arm64 "${IMAGE}@${DIGEST}")

export AICR_ISSUER="https://token.actions.githubusercontent.com"
# Exact identity, qualified by the release tag. A `refs/tags/.+` regexp would
# accept any AICR release's signature, which proves only that some NVIDIA/aicr
# release workflow signed the artifact, not that ${TAG} did.
export AICR_SIGNER="https://github.com/NVIDIA/aicr/.github/workflows/attest-images.yaml@refs/tags/${TAG}"
```

Then one command per kind:

```shell
# Extract under pipefail, and only write a predicate file once the whole
# pipeline has succeeded. Both halves are needed: `jq` exits 0 on empty input,
# so without pipefail a failed `cosign verify-attestation` still ends the
# pipeline with status 0, and the shell truncates a `>` target before cosign
# ever runs, so a plain redirect would leave a zero-length file that reads as a
# successful extraction.
set -o pipefail

# 1. Build provenance (index digest)
gh attestation verify "oci://${IMAGE}@${DIGEST}" \
  --repo NVIDIA/aicr \
  --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml \
  --source-ref "refs/tags/${TAG}" \
  --bundle-from-oci

# Both documents below are per-platform, so name the files per-platform too.
# Re-running these for linux/arm64 with a fixed name would overwrite the amd64
# evidence, or leave arm64 evidence under a name that claims to be amd64.
PLATFORM=linux-amd64
DIGEST_PLATFORM="${DIGEST_AMD64}"

# 2. CycloneDX SBOM for one platform (per-platform manifest digest)
sbom=$(cosign verify-attestation --type cyclonedx \
  --certificate-oidc-issuer "${AICR_ISSUER}" \
  --certificate-identity "${AICR_SIGNER}" \
  "${IMAGE}@${DIGEST_PLATFORM}" \
  | jq -r '.payload' | base64 -d | jq '.predicate') \
  && printf '%s\n' "${sbom}" > "sbom-${PLATFORM}.cdx.json"

# 3. OpenVEX vulnerability triage (same per-platform manifest digest)
openvex=$(cosign verify-attestation --type openvex \
  --certificate-oidc-issuer "${AICR_ISSUER}" \
  --certificate-identity "${AICR_SIGNER}" \
  "${IMAGE}@${DIGEST_PLATFORM}" \
  | jq -r '.payload' | base64 -d | jq '.predicate') \
  && printf '%s\n' "${openvex}" > "aicr-openvex-${PLATFORM}.json"
```

Each of the seven released images (`aicr`, `aicrd`, `aicr-gate`, and the four
`aicr-validators/*` images) carries the same three kinds; substitute the image
name in `IMAGE` and re-resolve the digests. Commands 2 and 3 retrieve the
`linux/amd64` evidence; repeat them with `PLATFORM=linux-arm64` and
`DIGEST_PLATFORM="${DIGEST_ARM64}"` for the other platform, which carries its
own SBOM and its own digest-bound VEX under its own filenames.

**Why `--bundle-from-oci`.** `gh attestation verify` fetches bundles from
GitHub's attestations API unless told otherwise, so the default form would
succeed even if the registry referrer push had silently failed. `--bundle-from-oci`
makes it read the same referrer Cosign reads in commands 2 and 3, which is what
makes this a single registry-backed retrieval path. Verifying through the API
instead is a legitimate fallback when the registry is unreachable or
unauthenticated; the sections below that omit the flag are that API-based
alternative, and they are labeled as such where they appear.

**Cosign version.** Commands 2 and 3 are Sigstore bundles published through the
OCI referrers path, which requires **Cosign v3.0.1+**; the same floor the
Rekor v2 note above sets. Command 1 uses the GitHub CLI and does not involve
Cosign, so the floor does not apply to it. GHCR does not implement the OCI 1.1
`/v2/{name}/referrers/{digest}` endpoint, so clients fall back to the
specification's referrers *tag* schema (a `sha256-{hex}` tag holding an index of
referrers). Cosign and ORAS do this transparently, which is why the commands
above are the documented path rather than a raw referrers API call.

## Third-Party Source Code

Source for every third-party open-source component AICR adds on top of its base
images is published, regardless of license. Where it lives depends on how the
component reaches the image.

| Image | Third-party components | Where the source is |
|-------|------------------------|---------------------|
| `aicr`, `aicrd`, `aicr-gate`, `aicr-validators/{conformance,deployment,performance}` | Go modules compiled into the binary | The public Go module proxy, at the exact versions pinned in the `go.mod`/`go.sum` of the GitHub release source archive for the matching tag. Modules are immutable and `sum.golang.org` is append-only, so those coordinates stay fetchable and each one's content is verifiable against the `go.sum` entry. |
| `aicr-validators/aiperf-bench` | Python packages installed from wheels | An OCI referrer on the image, retrieved with the commands below. |
| all | base image contents | Provided by NVIDIA with `nvcr.io/nvidia/distroless/*` under that image's own approval. |

License texts for all of the above are in `THIRD_PARTY_NOTICES.md`, published as
an asset on each release. That file discharges attribution; this section covers
source availability, which is a separate obligation.

### Retrieving the aiperf-bench source

The archive is attached as an OCI referrer, so it is **not** fetched by
`docker pull` and will not appear in the image's layers. Retrieve it explicitly:

```bash
IMAGE="ghcr.io/nvidia/aicr-validators/aiperf-bench:v0.19.0"

# 1. Resolve the image to its digest and list what is attached to it.
DIGEST=$(crane digest "${IMAGE}")
oras discover --format tree "${IMAGE%:*}@${DIGEST}"

# 2. Select the source referrer. Fail loudly on anything but exactly one:
#    artifactType is not a unique key, and anyone with push access to the
#    repository can attach another referrer carrying the same type. Taking the
#    first match would silently pick theirs.
mapfile -t MATCHES < <(oras discover --format json "${IMAGE%:*}@${DIGEST}" \
  | jq -r '.referrers[]
           | select(.artifactType == "application/vnd.nvidia.aicr.source.v1+tar")
           | .digest')
if [ "${#MATCHES[@]}" -ne 1 ]; then
  echo "expected exactly 1 source referrer, found ${#MATCHES[@]}" >&2
  exit 1
fi
SOURCE="${MATCHES[0]}"

# 3. Verify the signature before trusting the bytes. Pin the exact signing
#    workflow and tag; a regexp over the repository would accept a signature
#    from any workflow in it.
cosign verify \
  --certificate-oidc-issuer "${AICR_ISSUER}" \
  --certificate-identity \
    "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/${TAG}" \
  "${IMAGE%:*}@${SOURCE}"

# 4. Pull the verified archive.
oras pull -o ./aiperf-source "${IMAGE%:*}@${SOURCE}"

# 5. The archive holds one sdist per installed package, plus a README.
tar tzf ./aiperf-source/aiperf-bench-python-source.tar.gz | head
```

The archive contains the source distribution of every Python package installed
into the image **that publishes one upstream**, at the exact version installed.
One package is intentionally absent: `aiperf` itself publishes no source
distribution to PyPI. It is an
NVIDIA package and its source is at [ai-dynamo/aiperf](https://github.com/ai-dynamo/aiperf); the
archive's `README.txt` records this too.

Only `aiperf-bench` carries this referrer. The other six images need none: their
dependencies are Go modules, named exactly by the `go.sum` in the release source
archive and fetchable from the public module proxy at those coordinates.

**Scope of the correspondence.** The archive is resolved from the same
`requirements.txt` the image installs, so both derive from one input. Two gaps
remain and are tracked rather than claimed away:

* `pip` and `wheel` are provided by `python -m venv` rather than declared in
  `requirements.txt`, so they ship in the image without source in this archive.
* The image and the archive resolve the ranged dependencies independently: the
  image when `build-docker` runs `pip install`, the archive later in
  `attach-source`. A transitive release landing between the two resolves the
  archive newer than the image, so the closures can differ by that window.

Correspondence is therefore by shared input, not by identical resolved closure.
An exact guarantee would require resolving the archive from the built image's
own `pip freeze`, or pinning both to a shared lockfile.

The second gap used to be unbounded. The image build layer-cached its
resolution, so a cached build could replay a months-old closure
([#2086](https://github.com/NVIDIA/aicr/issues/2086)); it now resolves fresh on
every build, narrowing the divergence to the build-to-attach window.

A referrer binds to one image digest, so resolve the tag to a digest first as
shown above rather than assuming a tag keeps the same attachment across builds.

### Using the published VEX document

The OpenVEX document records, per CVE, why AICR is not affected: a machine
readable `status` and `justification` plus a human-readable impact statement
with the reachability evidence behind the call. Feeding it to your scanner
suppresses exactly the findings AICR's own release scan suppresses, so a
downstream gate does not re-flag CVEs that have already been triaged.

Every statement in the published document names one product,
`pkg:oci/<image>@sha256:<platform-manifest-digest>`, so the claim binds to the
exact manifest it was made about. The committed
[`.openvex.json`](https://github.com/NVIDIA/aicr/blob/main/.openvex.json) is the
reviewed source of truth and carries bare `pkg:oci/<image>` products; what the
release publishes is a digest-bound projection of it, generated at publication
time because the digest does not exist before the image is built. Statuses,
justifications, impact statements and subcomponents are published as curated:
nothing is translated or dropped.

Exactly one VEX document exists per platform manifest. A retriage replaces the
current document rather than adding a revision, so a referrers listing never
requires a consumer to decide which of several VEX documents is current.

Every released image carries a VEX document, including the ones with nothing to
say. Where AICR has triaged no CVE for an image, its document verifies normally
and its `statements` array is empty. Most released images are in that state at
any given time. Which ones is a property of the release you are verifying, not
of this page: read the document you retrieved rather than relying on a count
here, and treat
[`.openvex.json`](https://github.com/NVIDIA/aicr/blob/main/.openvex.json) at the
release tag as the reviewed record of what was claimed.

**An empty VEX means "no exceptions asserted", not "no vulnerabilities".** It is
a statement about what AICR has said, not about what a scanner would find. Do
not read one as a clean bill of health, and do not read the absence of a
statement about a particular CVE as a claim that the image is unaffected by it:
absence means AICR has not spoken to that CVE, nothing more. The VEX is a filter
you apply to a scan, never a substitute for running one. What an empty document
does buy you is that it is distinguishable from a *missing* attestation, which
would mean the evidence never reached the registry.

```shell
# Grype: apply the VEX document to a scan of the same platform manifest
grype "${IMAGE}@${DIGEST_AMD64}" --vex aicr-openvex.json --only-fixed --fail-on high

# Trivy: same document, same effect
trivy image --vex aicr-openvex.json "${IMAGE}@${DIGEST_AMD64}"
```

Statements apply only to products whose PURL matches, so passing the document to
a scan of an unrelated image (or of the other platform) is a no-op rather than
a blanket suppression. Treat
the document as evidence, not as an instruction: read the `impact_statement` for
any CVE your policy cares about and decide whether AICR's reasoning holds for
your deployment before adopting the suppression.

### Why provenance uses a different signer

Image provenance is produced by `actions/attest-build-provenance` from the
reusable [`attest-images.yaml`](https://github.com/NVIDIA/aicr/blob/main/.github/workflows/attest-images.yaml)
workflow, while the SBOM and VEX attestations are produced by `cosign attest` in
the same job. Unifying all three onto `cosign attest` would make the signer
identity uniform but would drop the image provenance from SLSA Build Level 3 to
Level 2: the Level 3 isolation property comes from GitHub's attestation service
recording the *reusable workflow* as the builder identity, which a caller
workflow cannot forge. AICR keeps the GitHub attestation flow for provenance for
that reason. All three land on the same image as OCI referrers regardless, so the
split affects which verifier you reach for, not where the metadata lives.

## Verifying Build Provenance (SLSA)

AICR produces SLSA build provenance through GitHub Actions: builds are
defined as code and provenance is service-generated (signed by GitHub's
OIDC-authenticated attestation service via `actions/attest-build-provenance`)
rather than self-asserted, then logged to the public Rekor transparency log.

> **Note on the SLSA Build Level.** GitHub's attestation service yields Build
> Level 2 by default; Build Level 3 additionally requires build **isolation**
> via a dedicated reusable workflow. AICR generates image attestations from the
> reusable [`attest-images.yaml`](https://github.com/NVIDIA/aicr/blob/main/.github/workflows/attest-images.yaml)
> workflow, so the image provenance is **Build Level 3** — its signer identity is
> that reusable workflow, which the caller cannot tamper with. Verify it by
> pinning `--signer-workflow .../attest-images.yaml` (below). CLI binaries are
> signed with `cosign attest-blob` from the release job and remain Build Level 2.

**Method 1: GitHub CLI**

These commands omit `--bundle-from-oci`, so they are the **API-based
alternative**: `gh` fetches the bundle from GitHub's attestations API rather
than from the registry referrer. Add `--bundle-from-oci` to verify the referrer
itself, as [Unified Metadata Retrieval](#unified-metadata-retrieval) does.

```shell
# Verify provenance exists and is valid (using digest)
gh attestation verify oci://${IMAGE_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Output shows:
# ✓ Verification succeeded!
#
# Attestations:
#   • Build provenance (SLSA v1.0)
# (the CycloneDX SBOM is a separate Cosign attestation — see Verifying the SBOM)
```

**Method 2: Extract and inspect provenance**

```shell
# Get full provenance data (using digest)
gh attestation verify oci://${IMAGE_DIGEST} \
  --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}" \
  --format json | jq '.[] | select(.verificationResult.statement.predicateType | contains("slsa"))'

# Key fields in provenance:
# - buildDefinition.buildType: GitHub Actions workflow type
# - runDetails.builder.id: Workflow file and commit
# - buildDefinition.externalParameters.workflow: Workflow path and ref
# - buildDefinition.resolvedDependencies: Source code commit SHA
# - runDetails.metadata.invocationId: GitHub run ID
```

The signed certificate binds the artifact to its source repository,
commit SHA, workflow, and run. A representative slice:

```json
{
  "verificationResult": {
    "signature": {
      "certificate": {
        "subjectAlternativeName": "https://github.com/NVIDIA/aicr/.github/workflows/attest-images.yaml@refs/tags/v0.8.12",
        "issuer": "https://token.actions.githubusercontent.com",
        "githubWorkflowName": "on_tag",
        "githubWorkflowRepository": "NVIDIA/aicr",
        "githubWorkflowRef": "refs/tags/v0.8.12",
        "sourceRepositoryURI": "https://github.com/NVIDIA/aicr",
        "sourceRepositoryDigest": "ba6cbbe8b1a8fc8b72bb18454c10a3ba31d94a2e",
        "runnerEnvironment": "github-hosted",
        "runInvocationURI": "https://github.com/NVIDIA/aicr/actions/runs/20642050863/attempts/1"
      }
    }
  }
}
```

### Build process transparency

All AICR releases are built using GitHub Actions with full transparency:

1. **Source Code** — Public GitHub repository
2. **Build & Attest Workflows** — `.github/workflows/on-tag.yaml` builds and calls the reusable `.github/workflows/attest-images.yaml`, which signs the image attestations (both version controlled)
3. **Build Logs** — Public GitHub Actions run logs
4. **Attestations** — Signed and stored in the public transparency log (Rekor)
5. **Artifacts** — Published to GitHub Releases and GHCR

**View build history:**

```shell
# List all releases with attestations
gh api repos/NVIDIA/aicr/releases | \
  jq -r '.[] | "\(.tag_name): \(.html_url)"'

# View specific build logs
gh run list --repo NVIDIA/aicr --workflow=on-tag.yaml
gh run view 20642050863 --repo NVIDIA/aicr --log
```

**Verify in the transparency log (Rekor):**

```shell
# Search Rekor for attestations
rekor-cli search --sha "${DIGEST#sha256:}"

# Get entry details
rekor-cli get --uuid <entry-uuid>
```

## Verifying the SBOM

AICR provides SBOMs in two places and two formats. Binary SBOMs ship as
separate GoReleaser artifacts alongside the CLI binaries, in **SPDX v2.3 JSON**.
Container image SBOMs are attached as Cosign attestations, in **CycloneDX 1.6
JSON**, one per platform manifest. Both are generated by Syft/Anchore.

**Migrating from the SPDX container SBOM attestation.** The container image
SBOM has had three shapes, and an already-released image keeps whichever one it
was published with. Verifying across a boundary means matching both the
predicate type and the subject to the release:

| Releases | Predicate type | Subject | Retrieved from |
|----------|----------------|---------|----------------|
| Through v0.18.x | `--type spdxjson` | multi-platform **index** digest | legacy `.att` tag, not the referrers path |
| v0.19.0 through the release before this change | `--type spdxjson` | **per-platform** manifest digest | OCI referrer ([#1957](https://github.com/NVIDIA/aicr/issues/1957)) |
| This change onward | `--type cyclonedx` | **per-platform** manifest digest | OCI referrer |

So the current change alters the predicate type only; the subject moved one
release earlier, in v0.19.0. Querying a v0.18.x image on a platform digest, or a
v0.19.0+ image on the index digest, reports valid evidence as missing.

To migrate a current verification, change `--type spdxjson` to
`--type cyclonedx` in any `cosign verify-attestation` call or admission policy,
and move field queries from the SPDX shape (`.packages[]`, `.versionInfo`,
`.licenseDeclared`, `.creationInfo.created`) to the CycloneDX shape
(`.components[]`, `.version`, `.licenses[]`, `.metadata.timestamp`); see
[SBOM format](#sbom-format) for both. The binary SBOM assets are unaffected and
stay SPDX.

### Binary SBOM (CLI)

```shell
# Detect OS and architecture
export OS=$(uname -s | tr '[:upper:]' '[:lower:]')
export ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

# Download the versioned archive from GitHub releases and extract the binary.
# GoReleaser ships aicr_<version>_<os>_<arch>.tar.gz; ${VERSION} is the tag
# without its leading "v" (e.g. TAG=v0.8.12 -> VERSION=0.8.12).
curl -LO https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_${VERSION}_${OS}_${ARCH}.tar.gz
tar -xzf aicr_${VERSION}_${OS}_${ARCH}.tar.gz
chmod +x aicr

# Download SBOM (separate file)
curl -LO https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_${VERSION}_${OS}_${ARCH}.sbom.json

# View SBOM
cat aicr_${VERSION}_${OS}_${ARCH}.sbom.json
```

Each binary SBOM ships with its own Sigstore bundle,
`aicr_${VERSION}_${OS}_${ARCH}.sbom.json.sigstore.json`, carrying the release
run's SLSA provenance over the SBOM document itself. Download it alongside the
SBOM and verify that NVIDIA CI produced that exact document:

```shell
curl -LO https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_${VERSION}_${OS}_${ARCH}.sbom.json.sigstore.json

cosign verify-blob-attestation \
  --bundle aicr_${VERSION}_${OS}_${ARCH}.sbom.json.sigstore.json \
  --type https://slsa.dev/provenance/v1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.+$' \
  aicr_${VERSION}_${OS}_${ARCH}.sbom.json
```

Binary metadata stays on the GitHub release rather than getting its own OCI
home: the binaries are distributed as release assets, so a Sigstore bundle
downloaded next to the asset it covers keeps the artifact and its evidence on a
single retrieval path. Image metadata lives in the registry for the same reason.

### Container image SBOM

Container image SBOMs are attached **per platform**: each platform's CycloneDX
document is an attestation on that platform's manifest digest, not on the
multi-platform index digest. Resolve the platform digest with
`crane digest --platform` (see [Unified Metadata Retrieval](#unified-metadata-retrieval))
before verifying.

```shell
# pipefail plus the deferred write, for the reason given under
# Unified Metadata Retrieval: a failed verification must not leave an empty
# sbom.json behind.
set -o pipefail

# Resolve from the pinned index (${DIGEST_API}), never from the mutable tag.
export DIGEST_API_AMD64=$(crane digest --platform linux/amd64 "${IMAGE_API}@${DIGEST_API}")

# Method 1: Using Cosign (extracts attestation) - uses the per-platform digest
sbom=$(cosign verify-attestation \
  --type cyclonedx \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE_API}@${DIGEST_API_AMD64} | \
  jq -r '.payload' | base64 -d | jq '.predicate') \
  && printf '%s\n' "${sbom}" > sbom.json

# Method 2: GitHub CLI (build provenance only; the CycloneDX SBOM needs Method 1's Cosign flow).
# Without --bundle-from-oci this reads GitHub's attestations API, not the registry referrer.
gh attestation verify oci://${IMAGE_API_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}" --format json
```

### SBOM format

Binary SBOMs are SPDX v2.3 JSON. A representative package entry (the full
document lists every Go module and its transitive dependencies, licenses, and
package URLs):

```json
{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "name": "aicr",
  "creationInfo": {
    "creators": ["Organization: Anchore, Inc", "Tool: syft-1.38.2"]
  },
  "packages": [
    {
      "name": "github.com/NVIDIA/aicr",
      "versionInfo": "v0.8.12",
      "externalRefs": [
        {
          "referenceType": "purl",
          "referenceLocator": "pkg:golang/github.com/NVIDIA/aicr@v0.8.12"
        }
      ]
    }
  ]
}
```

Container image SBOMs are CycloneDX 1.6 JSON. The same information lives under
different keys, so queries written against one format do not run against the
other:

```json
{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "tools": {"components": [{"name": "syft", "version": "1.45.1", "type": "application"}]},
    "component": {"type": "container", "name": "ghcr.io/nvidia/aicr"}
  },
  "components": [
    {
      "type": "library",
      "name": "github.com/NVIDIA/aicr",
      "version": "v0.8.12",
      "purl": "pkg:golang/github.com/NVIDIA/aicr@v0.8.12",
      "licenses": [{"license": {"id": "Apache-2.0"}}]
    }
  ]
}
```

The document carries no `vulnerabilities` key. Scan results are deliberately not
published: anyone holding the image can derive them, and they go stale the
moment a vulnerability database updates, whereas the image does not change. The
one thing that cannot be derived, the triage judgment about whether a finding is
reachable, is published as the OpenVEX document instead.

### SBOM use cases

Against a container image SBOM (CycloneDX):

```shell
# Vulnerability scanning — feed the SBOM to Grype, Anchore, or Snyk
grype sbom:./sbom.json

# License compliance — list declared licenses
jq -r '.components[] | select(.licenses != null) | "\(.name) \(.version) \(.licenses[0].license.id // .licenses[0].license.name)"' sbom.json

# Dependency tracking — search for a specific component
jq '.components[] | select(.name | contains("vulnerable-lib"))' sbom.json

# Audit trail — the SBOM timestamp proves when components were included
jq '.metadata.timestamp' sbom.json
```

Against a binary SBOM (SPDX), the same queries read `.packages[]`,
`.licenseDeclared`, `.versionInfo`, and `.creationInfo.created`.

## Verifying Image and Bundle Attestations

### Container image attestations

**Method 1: GitHub CLI (recommended)**

Also the API-based path: append `--bundle-from-oci` to each command to verify
the registry referrer instead of GitHub's attestations API.

```shell
# Verify using digest (preferred - no warnings)
gh attestation verify oci://${IMAGE_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Verify the aicrd image
gh attestation verify oci://${IMAGE_API_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Note: You can still use tags, but tools may show warnings about mutability
# gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
```

**Method 2: Cosign (SBOM and VEX attestations)**

```shell
# pipefail plus the deferred write, for the reason given under
# Unified Metadata Retrieval: a failed verification must not leave an empty
# predicate file behind.
set -o pipefail

# Verify the SBOM attestation on a per-platform manifest digest,
# resolved from the pinned index rather than from the mutable tag.
export DIGEST_AMD64=$(crane digest --platform linux/amd64 "${IMAGE}@${DIGEST}")
cosign verify-attestation \
  --type cyclonedx \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE}@${DIGEST_AMD64}

# Extract and view the SBOM predicate
cosign verify-attestation \
  --type cyclonedx \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE}@${DIGEST_AMD64} | jq -r '.payload' | base64 -d | jq '.predicate'

# Verify the OpenVEX attestation on the same per-platform manifest digest
openvex=$(cosign verify-attestation \
  --type openvex \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE}@${DIGEST_AMD64} | jq -r '.payload' | base64 -d | jq '.predicate') \
  && printf '%s\n' "${openvex}" > aicr-openvex.json
```

### CLI binary attestation

CLI binary releases are attested with SLSA Build Provenance v1 using Cosign
keyless signing via GitHub Actions OIDC. Each release archive (`.tar.gz`)
contains the `aicr` binary and an `aicr-attestation.sigstore.json` Sigstore
bundle. The attestation is logged to the public
[Rekor](https://rekor.sigstore.dev/) transparency log and can be verified
offline.

```shell
cosign verify-blob-attestation \
  --bundle aicr-attestation.sigstore.json \
  --type https://slsa.dev/provenance/v1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.+$' \
  aicr
```

The install script (`./install`) runs this verification automatically when
Cosign is available. The `Build Attested Binaries` workflow
(`.github/workflows/build-attested.yaml`) can be triggered manually from the
Actions tab to produce attested binaries from any branch without cutting a
release.

### Bundle attestation

When `aicr bundle` runs with `--attest`, it signs the bundle using Sigstore
keyless OIDC, binding the bundle creator's identity to the generated
closed-world inventory and the binary that produced it (via
`resolvedDependencies`). Attestation is opt-in; bundles are unsigned by
default. The bundle output includes
`attestation/bundle-attestation.sigstore.json` (SLSA Build Provenance v1 for
the bundle) and `attestation/aicr-attestation.sigstore.json` (the binary
provenance chain).

Bundle verification is closed-world: `checksums.txt` defines the regular
payload, and any unlisted or non-regular entry fails verification. See
[Artifact Verification](../user/artifact-verification.md#what-can-be-verified)
for the exact metadata exceptions, ordering rules, publication guarantees, and
legacy-bundle behavior.

```shell
aicr verify ./my-bundle
```

This performs full closed-world verification: it validates every manifest
digest and rejects any additional filesystem entry, then verifies the bundle
attestation against the Sigstore trusted root and the binary attestation
provenance chain (identity pinned to NVIDIA CI). Manifest parsing is
order-independent, but reordering an already signed `checksums.txt` changes
the signed bytes and invalidates its existing attestation. Enforce a minimum
trust level:

```shell
aicr verify ./my-bundle --min-trust-level verified
```

For full CLI flag documentation, see the
[CLI Reference](../user/cli-reference.md#aicr-verify) (`aicr verify`,
`aicr bundle --attest`, `aicr trust update`). For a hands-on walkthrough,
see the [Bundle Attestation Demo](../../demos/bundle-attestation.md).

## Enforcing with Admission Policies

You can enforce provenance verification at deployment time with a Kubernetes
admission controller. AICR's images carry **GitHub Artifact Attestations**,
which are **Sigstore bundles** — so the admission policy must verify the
Sigstore *bundle* format (not the legacy Cosign signature format):

Pin every policy to AICR's release identity:

- **issuer:** `https://token.actions.githubusercontent.com`
- **subject:** `https://github.com/NVIDIA/aicr/.github/workflows/attest-images.yaml@refs/tags/*` (the reusable attestation workflow that signs image provenance/SBOMs; narrow to the release pattern rather than trusting every workflow/ref)

### Kyverno

> **Not verified against AICR images — use Sigstore Policy Controller (below).**
> Kyverno verifies Sigstore bundles with `type: SigstoreBundle` (v1.18+; see
> Kyverno's
> [Verifying Sigstore Bundles](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/sigstore/#verifying-sigstore-bundles)
> guide). In testing on GKE 1.35 with Kyverno **v1.18.1**, a `SigstoreBundle`
> `verifyImages` rule pinned to AICR's release identity could **not** verify
> AICR's GitHub Artifact Attestation — it failed with `no matching signatures
> found`, even though `cosign verify-attestation` and the Policy Controller
> policy below verify the same Sigstore-bundle (`v0.3`) referrer on the image's
> index digest. Until that gap is understood, enforce AICR images with the
> Sigstore Policy Controller policy below. Tracking:
> [#1537](https://github.com/NVIDIA/aicr/issues/1537).

### Kyverno and per-platform evidence

The caveat above concerns AICR's **GitHub Artifact Attestation** on the index
digest. The results below are a separate measurement: `cosign attest` output on
**platform manifests**, read with `type: SigstoreBundle`. Different producer,
different subject, and both results hold.

Measured on kind with Kubernetes v1.34.0, Kyverno v1.19.0 and cosign v3.0.6.

A deployment manifest names a tag, not a digest. Kyverno's `mutateDigest` is on
by default and resolves that tag to the **index** digest, which is where
provenance lives and where the SBOM and the VEX do not. Four policies against
one image:

| Policy | Subject it evaluates | Result |
|--------|----------------------|--------|
| provenance only | index digest | admitted |
| SBOM only | index digest | denied |
| SBOM + VEX + provenance | platform manifest | denied |
| provenance only | platform manifest | denied |

No single rule spans both digests. So for a normal tag-naming workload, SLSA
provenance is enforceable at admission and the CycloneDX SBOM and the OpenVEX
document are not. This follows from where the evidence is attached, not from how
the policy is written, so a better policy will not recover it.

The completeness property does work. Listing several `attestations[]` entries
requires all of them, in both `ClusterPolicy` and `ImageValidatingPolicy`:
deleting the VEX referrer and re-running a SBOM + VEX policy denies in both
engines. It is only the cross-digest span that fails.

Three ways to live with this, none of them free:

* **Attach every predicate type to whichever digest you gate on.** This makes
  one rule sufficient, at the cost of honesty in the SBOM: a single index-level
  SBOM cannot describe two different root filesystems.
* **Split the gate.** Enforce provenance at admission, and the SBOM and VEX at
  build or promotion time where the platform digest is already known. This keeps
  every document truthful but moves part of the check off the admission path.
* **Pin platform digests in pod specs.** One rule then covers everything, and
  you give up multi-arch scheduling.

### Kyverno attestation payload limits

Kyverno loads the whole attestation payload into the policy evaluation context
*before* any condition runs, bounded by `maxContextSize` in Kyverno's ConfigMap.
The default is 2 MiB and exists as a denial-of-service control:

```text
context size limit exceeded: 5216263 bytes exceeds limit of 2097152 bytes
```

This is not about condition complexity. A condition reading only `bomFormat`
fails exactly as a condition walking 9,003 components does, because neither has
run yet. Measured with a synthetically padded SBOM: 1.2 MB admitted, 5.5 MB
denied, and the same 5.5 MB admitted after setting `maxContextSize: 32Mi`.

AICR's own image SBOMs are well under the default, but real application-image
SBOMs pass 2 MiB routinely, and the error names neither the predicate nor the
condition that triggered it. Treat raising `maxContextSize` as a prerequisite
for deploying SBOM-reading policies rather than as tuning to reach for after a
failure.

### Sigstore Policy Controller

Sigstore-bundle support requires **v0.13.0+** and `signatureFormat: bundle`;
see the
[Sigstore bundle format](https://docs.sigstore.dev/policy-controller/overview/#sigstore-bundle-format)
docs. Enforcement only runs in namespaces labeled
`policy.sigstore.dev/include=true`.

```yaml
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: aicr-require-provenance
spec:
  images:
    - glob: "ghcr.io/nvidia/aicr**"
  authorities:
    - name: aicr-release
      signatureFormat: bundle
      keyless:
        url: https://fulcio.sigstore.dev
        identities:
          - issuer: https://token.actions.githubusercontent.com
            subjectRegExp: '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$'
      ctlog:
        url: https://rekor.sigstore.dev
      attestations:
        - name: slsa-provenance
          predicateType: https://slsa.dev/provenance/v1
```

Save the `ClusterImagePolicy` above as `clusterimagepolicy.yaml`, apply it,
create and label a target namespace, then confirm enforcement (`DIGEST` is
resolved in **Prerequisites and Setup** above):

```shell
kubectl apply -f clusterimagepolicy.yaml
kubectl -n cosign-system rollout status deploy/policy-controller-webhook
export NAMESPACE=aicr-policy-test
kubectl create namespace "$NAMESPACE"
kubectl label namespace "$NAMESPACE" policy.sigstore.dev/include=true
sleep 15   # let the webhook ingest the new policy

# Positive: a signed AICR image (pinned by digest) is admitted
kubectl -n "$NAMESPACE" run aicr-signed \
  --image="ghcr.io/nvidia/aicr@${DIGEST}" --restart=Never \
  --command -- /ko-app/aicr --version
```

For a coherent **negative** test the image must match the policy `glob`
(`ghcr.io/nvidia/aicr**`) yet be unsigned — a non-matching image is simply
ignored by the policy. Push an unsigned image under a path you control whose
name the glob matches (or temporarily widen the glob to it), then confirm the
admission webhook rejects it.

> **Validation status.** The Policy Controller `ClusterImagePolicy` above is
> cluster-validated against Policy Controller **v0.13.1** on GKE 1.35: a signed
> AICR image is admitted and a wrong-identity pin is rejected (note that
> `signatureFormat` and `ctlog` are *per-authority* fields). The Kyverno
> `SigstoreBundle` path was cluster-tested (v1.18.1) and **failed** to verify
> AICR's bundle attestation (`no matching signatures found`) — see the Kyverno
> note above; tracked in [#1537](https://github.com/NVIDIA/aicr/issues/1537).

## Gating Deployment on Verification

`aicr verify` is the deploy-time gate: run it against the bundle directory
before anything installs that bundle or publishes it for a controller to pull.
Because `checksums.txt` is a closed-world inventory of every payload file,
verifying it transitively covers the whole bundle; see
[Artifact Verification](../user/artifact-verification.md#what-can-be-verified).

Where that gate belongs depends on whether the deployer pushes or pulls, so
each one is covered separately below. Every gate is marked either *advisory* (a
pipeline step that an operator holding cluster credentials can bypass) or
*binding* (the cluster itself refuses to proceed).

### Gating the helm and helmfile deployers

The default `helm` deployer and `helmfile` are push-based: the same pipeline
step that verifies is the one that installs, so verification and use cannot
drift apart.

```shell
cd bundles
aicr verify . --min-trust-level verified
chmod +x deploy.sh
./deploy.sh
```

`aicr verify` exits non-zero on any failure, so `&&` chaining or `set -e` is
enough to stop the install. Prefer an explicit `--min-trust-level verified`
over the default `max`, which resolves to the highest level *that particular
bundle* can reach and therefore passes an unsigned bundle. Add
`--require-creator` to pin who built it and `--cli-version-constraint` to floor
the `aicr` version that produced it; `--format json` emits a machine-readable
result for a pipeline that needs to branch. For `helmfile`, run the same
`aicr verify .` before `helmfile apply`. See [Automation](automation.md) for
the full four-stage pipeline and its GitLab, CircleCI, and Terraform
equivalents.

**These gates are advisory.** They run in your pipeline, on the machine that
holds the kubeconfig. Anyone who can run `helm install` directly, or who can
edit the pipeline definition, bypasses them. Protect them the way you protect
any pipeline: branch protection on the workflow definition, and cluster
credentials that only the pipeline holds.

### Gating Argo CD

Argo CD is pull-based. `aicr bundle --deployer argocd --repo <git-url>` writes
`app-of-apps.yaml` plus per-component `NNN-<component>/application.yaml`, you
commit them, and the cluster's Argo CD reconciles the repository on its own
schedule. A CI step therefore gates **what enters the repository**, not what
the cluster applies. Verify before you commit:

```shell
aicr bundle --recipe recipe.yaml --deployer argocd --attest \
  --repo https://github.com/my-org/my-gitops-repo.git \
  --output ./bundles
aicr verify ./bundles --min-trust-level verified
cp -r ./bundles/* gitops-repo/
(cd gitops-repo && git add . && git commit -S -m "Update GPU stack" && git push)
```

Verify the bundle directory, never the GitOps repository. Verification is
closed-world and rejects any filesystem entry not listed in `checksums.txt`, so
running it against a repository that holds anything else fails.

For a **binding** control, have the pipeline sign the commit and have Argo CD
refuse unsigned revisions. `AppProject.spec.signatureKeys` lists the GnuPG key
IDs allowed to sign; Argo CD then refuses to sync any revision that is unsigned
or signed by a key outside that list:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: gpu-stack
  namespace: argocd
spec:
  signatureKeys:
    - keyID: 4AEE18F83AFDEB23
  sourceRepos:
    - https://github.com/my-org/my-gitops-repo.git
```

Read the trust chain carefully. The cluster enforces "this revision was signed
by CI," and CI signs only after `aicr verify` passed. The cluster is not
verifying the AICR attestation; it is trusting your pipeline's identity to have
checked it. Argo CD's GnuPG verification applies to **Git** sources only, not
to Helm repositories, and it disables `argocd app sync --local`. See Argo CD's
[GnuPG signature verification](https://argo-cd.readthedocs.io/en/stable/user-guide/gpg-verification/).

### Gating Flux

Flux is likewise pull-based, in two source shapes.

**Git source (default).** `aicr bundle --deployer flux --output ./flux-bundle`
writes a root `kustomization.yaml`, a `sources/` directory, per-component
`helmrelease.yaml` files, and a `README.md` carrying the entry-point
`GitRepository` and `Kustomization` you apply to the cluster. Verify the bundle
directory before copying it to the repository root, exactly as for Argo CD.
Bind the reconcile by signing the commit in CI and adding `spec.verify` to that
entry-point `GitRepository`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: aicr-stack
  namespace: flux-system
spec:
  url: https://github.com/my-org/my-gitops-repo.git
  ref:
    branch: main
  interval: 10m
  verify:
    mode: HEAD
    secretRef:
      name: pgp-public-keys
```

`mode: HEAD` verifies the commit at the checked-out HEAD; `Tag` and
`TagAndHEAD` are the other accepted values. The referenced Secret holds the
trusted public keys (`.asc` for PGP, `.sshpub` for SSH). See Flux's
[GitRepository verification](https://fluxcd.io/flux/components/source/gitrepositories/).

**OCI source.** With `--output oci://...`, AICR pushes the bundle as an OCI
artifact and generates `ArtifactGenerator` CRs that reference an
`OCIRepository` **you** deploy (named by `--flux-oci-source-name`, default
`aicr-bundle`, in `--flux-namespace`, default `flux-system`). Because you own
that `OCIRepository`, you can require a Cosign signature on the artifact before
Flux will reconcile anything from it:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: aicr-bundle
  namespace: flux-system
spec:
  interval: 10m
  url: oci://ghcr.io/my-org/aicr-bundle
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: "^https://token\\.actions\\.githubusercontent\\.com$"
        subject: "^https://github\\.com/my-org/my-gitops-repo/\\.github/workflows/deploy\\.yaml@refs/heads/main$"
```

That Cosign signature is **not** AICR's bundle attestation, and AICR does not
produce it: your pipeline countersigns the pushed manifest after `aicr verify`
passes. An OCI push also materializes the same inventory in `./bundle` relative
to the working directory, which is the copy to verify, and `--image-refs`
captures the published digest so the countersignature pins the exact manifest:

```shell
aicr bundle --recipe recipe.yaml --deployer flux --attest \
  --output oci://ghcr.io/my-org/aicr-bundle:v1.0.0 \
  --image-refs ./published-digest.txt
aicr verify ./bundle --min-trust-level verified
cosign sign "ghcr.io/my-org/aicr-bundle@$(cat ./published-digest.txt)"
```

This one is binding: source-controller will not produce an artifact from an
`OCIRepository` whose signature fails to verify, so no `HelmRelease` downstream
of it reconciles. As with Argo CD, what the cluster enforces is your pipeline's
countersignature, not the AICR attestation. Flux OCI mode also has its own
prerequisites (Flux v2.7+ with source-watcher and the `ExternalArtifact`
feature gate); see
[Flux OCI Mode](../user/cli-reference.md#flux-oci-mode) and Flux's
[OCIRepository verification](https://fluxcd.io/flux/components/source/ocirepositories/).

### Gates across trust environments

Only the trust material handed to `aicr verify` changes between environments;
the gate itself is the same command in the same place.

| Environment | Signing | Deploy-time gate |
|-------------|---------|------------------|
| Public Sigstore | `--attest` | `aicr verify <dir> --min-trust-level verified` |
| Private Sigstore | `--attest --fulcio-url <url> --rekor-url <url>` | `aicr verify <dir> --trust-root ./trusted_root.json --require-creator ci@myorg.example.com` |
| KMS key | `--attest --signing-key <kms-uri>` | `aicr verify <dir> --key <kms-uri>` (or a PEM exported with `cosign public-key`) |
| Air-gapped | `--attest --signing-key <kms-uri> --tlog-upload=false` | `aicr verify <dir> --key ./bundle-signer.pub --insecure-ignore-tlog` |

`--trust-root` is additive to AICR's built-in public-good root, so one command
verifies both org-signed and NVIDIA-signed bundles.
`--insecure-ignore-tlog` requires `--key`, and a local PEM key keeps the verify
fully offline where a KMS URI still resolves remotely. Full details for each
shape are in
[Artifact Verification](../user/artifact-verification.md#kms-key-verification).

The binding half is unchanged across all four rows, because it enforces your
pipeline's own signature rather than AICR's. Air-gapped sites should note that
Flux's `spec.verify` keyless form depends on public Sigstore: use a key-based
Cosign signature with `spec.verify.secretRef` instead of `matchOIDCIdentity`.

## Offline and Air-Gapped Verification

Container image verification uses GitHub's attestation API
(`gh attestation verify`) because images are already fetched from a
registry — an inherently online context. Binary and bundle verification
uses `sigstore-go` with a local trusted root instead. Verification is a
read operation that may run frequently — in CI pipelines, in clusters
verifying deployed bundles, or by audit tools — and must not be coupled to
external API availability or rate limits. Cryptographic security is
identical in both cases; the Rekor inclusion proof is embedded in every
`.sigstore.json` bundle and verified locally.

### Trusted root management

Bundle verification uses a Sigstore trusted root (CA certificates and Rekor
public keys) to validate attestation signatures offline.

**Three layers of trust resolution (in priority order):**

1. **TUF cache** (`~/.sigstore/root/`) — updated by `aicr trust update`
2. **Embedded TUF root** — compiled into the binary, used to bootstrap
3. **TUF update** — `aicr trust update` contacts the Sigstore TUF CDN

Verification itself never contacts the network — it uses the cache or the
embedded root. The install script runs `aicr trust update` automatically
after installation.

```shell
aicr trust update
```

Run this when Sigstore rotates their keys (a few times per year) or if
verification reports a stale root.

## Monitoring Your Signing Identity

Everything above is consumer-side: it proves that an artifact you already hold
came from the identity you expect. It cannot tell you that somebody else signed
something *as you*. That is the producer-side question, and the transparency log
is the only place it can be answered. An entry under your signing identity that
you did not produce may indicate that the identity was used without you, and it
is the only signal that will tell you.

The gap is sharpest for keyless signing, which leaves no local trace. The Fulcio
certificate is short-lived, there is no private key on disk whose use you could
audit, and the signing event is invisible once the process exits. The Rekor entry
is the only durable record, so watching the log is the only way to notice misuse.
Sigstore reaches the same conclusion: its
[threat model](https://docs.sigstore.dev/about/threat-model/) treats identity and
consistency monitoring as the detection control for a compromised signing
identity, and its [Rekor documentation](https://docs.sigstore.dev/logging/overview/)
tells artifact owners to monitor the log for their own identity.
AICR runs exactly this check against its own release signing identity in
[`.github/workflows/rekor-monitor.yaml`](https://github.com/NVIDIA/aicr/blob/main/.github/workflows/rekor-monitor.yaml);
this section is how to run it against yours.

Every AICR signing path uploads to a transparency log by default:
`aicr bundle --attest`, `aicr validate --emit-attestation --push`,
`aicr evidence publish`, and `aicr evidence sign`. The one signing mode that
uploads nothing is `--tlog-upload=false` (KMS-only air-gapped signing). With no
log entry there is nothing to monitor.

### What your signature records

Configure the monitor from what your own entries actually carry, rather than
guessing. Read it off a bundle you signed:

```shell
# Fulcio certificate from a keyless-signed bundle. The SAN is under
# "X509v3 Subject Alternative Name"; the OIDC issuer is extension
# 1.3.6.1.4.1.57264.1.8.
jq -r '.verificationMaterial.certificate.rawBytes' \
  ./my-bundle/attestation/bundle-attestation.sigstore.json \
  | base64 -d | openssl x509 -inform DER -noout -text
```

| Signing mode | What the log entry identifies you by | Log |
|--------------|--------------------------------------|-----|
| Interactive keyless (`aicr bundle --attest`, browser or `--oidc-device-flow`) | Fulcio certificate: your email address in the SAN, plus the OIDC issuer that authenticated you, which is `https://oauth2.sigstore.dev/auth` for the default browser flow | public-good Rekor v2 |
| Non-interactive keyless (ambient CI OIDC, or `--identity-token`) | Fulcio certificate: the workload identity URL in the SAN; under GitHub Actions the issuer is `https://token.actions.githubusercontent.com` | public-good Rekor v2 |
| Keyless evidence signing (`aicr validate --emit-attestation --push`, `aicr evidence publish`, `aicr evidence sign`) | as keyless above; these commands take no key and no log override | public-good Rekor v2 |
| KMS key (`aicr bundle --signing-key <kms-uri>`) | a public key, not a certificate: no SAN and no issuer to match, only the key itself | public-good Rekor v2 |
| Private Fulcio alone (`aicr bundle --fulcio-url`) | as keyless above, but the certificate is issued by your own CA | public-good Rekor v2 |
| Private Fulcio plus private log (`aicr bundle --fulcio-url` with `--rekor-url`) | as keyless above, but the certificate is issued by your own CA | your private Rekor v1 |
| Rekor v1 opt-out (`aicr bundle --rekor-url https://rekor.sigstore.dev`) | as keyless above | public-good Rekor v1 |
| Air-gapped KMS (`aicr bundle --tlog-upload=false`) | nothing is uploaded | none |

The flags in the first column are `aicr bundle` flags. `--signing-key`,
`--fulcio-url`, `--rekor-url`, `--signing-config`, and `--tlog-upload` are
registered on that command only; the evidence-signing paths
(`aicr validate --emit-attestation --push`, `aicr evidence publish`,
`aicr evidence sign`) are keyless-only and always write to the default Rekor v2,
with no key and no Rekor v1 override.

Note the third column carefully. Rekor v2 is the default for every keyless and
KMS signing path in the CLI, and only `--rekor-url` and `--signing-config` change
the log. `--fulcio-url` selects the certificate authority, not the log, so a
private CA on its own still publishes into the public-good Rekor v2. Pair it with
`--rekor-url` to keep the entry inside your own infrastructure. See
[Rekor v2 Signing](../contributor/rekor-v2-signing.md). Several of these rows
have no working monitor today; the coverage gaps at the end of this section say
which and why.

### Monitoring the public-good Rekor v2

**The upstream [`sigstore/rekor-monitor`](https://github.com/sigstore/rekor-monitor)
reusable workflow cannot monitor this log today.** It resolves which Rekor to
read from Sigstore's *default* signing config, which lists only Rekor v1;
Sigstore's [Rekor evolution post](https://blog.sigstore.dev/rekor-evolution/)
states that the public-good instance will continue using Rekor v1 as the default
log for the foreseeable future. The workflow's inputs are `file_issue`,
`artifact_retention_days`, `once`, `config`, and `url`; none of them selects a
different signing config, and pointing `url` at a v2 shard falls through to its
v1 client and fails.

AICR hit that same wall and wrote `tools/rekor-monitor` for it. The tool resolves
the v2 shard from the same signing config AICR signs against, then delegates the
security-critical verification to upstream's library packages. It takes the
watched identity as flags, so it monitors your identity as readily as AICR's:

```shell
# Pin the tool you audit by commit SHA. Cloning the default branch means the
# monitor can change under you between runs; bump this deliberately after
# review. `git clone --branch` resolves only branch and tag names, so the
# checkout is a separate step.
git clone https://github.com/NVIDIA/aicr && cd aicr
git checkout --detach d4f7bef460dc8d1ef7ea0334a6935c0038de88e4

# Allowlist the tags a real release actually signed, so a legitimate release
# does not alert. Derive it from your release workflow's completed run history,
# never from `git tag --list`: an attacker-pushed or never-signed tag present in
# the repository would be pre-suppressed. The workflow example below shows the
# query; run it once ahead of the monitor.
go run ./tools/rekor-monitor \
  --file checkpoint_v2.txt \
  --cert-subject '^https://github\.com/myorg/myrepo/\.github/workflows/release\.yaml@refs/tags/.*$' \
  --cert-issuer '^https://token\.actions\.githubusercontent\.com$' \
  --known-tags-file known-tags.txt
```

The two halves of the identity must come off the same certificate. Under the
GitHub Actions issuer the SAN is the workflow identity URL shown above, never an
email address; an email SAN belongs with the interactive IdP issuer
(`^https://oauth2\.sigstore\.dev/auth$` for the default browser flow). Subject
and issuer are AND-ed, so an incoherent pair matches nothing at all and the
monitor reports clean forever while watching an identity that cannot exist.

`--cert-subject` and `--cert-issuer` are regexes matched against the
certificate's SAN and OIDC issuer extension; anchor them, or a lookalike
identity matches. `--file` is the cursor: the first run baselines at the current
tree head and scans nothing, and every later run scans only the window added
since. The tool writes two companions alongside it. `<file>.scan` holds how far
the current window has been scanned, so a large backlog is caught up across
several bounded runs; it must survive between runs alongside the checkpoint, or
the window is rescanned from the start. `<file>.stall` holds only the catch-up
convergence history that feeds the `degraded` classification, so losing it costs
stall detection, not scan progress. `--timeout` bounds a single pass. Exit `0` is clean,
`1` is a security finding (`tamper` or `identity`), and `3` is a non-security
failure (`operational` for Sigstore, Rekor, or network trouble; `degraded` when
the log is outpacing the per-run scan). Every completed run prints a matching
`CLASSIFICATION=` line to branch on. Full flag and exit-code reference:
[`tools/rekor-monitor/README.md`](https://github.com/NVIDIA/aicr/blob/main/tools/rekor-monitor/README.md).

Know the one limitation before you rely on this. On an identity match the tool
deliberately holds the cursor before the matching chunk rather than advancing
past it, because advancing would let the next clean window auto-close the alert
without anyone acknowledging it. The consequence is that the same finding is
re-detected and re-alerted on every subsequent run until a maintainer triages
it. The only built-in suppression is `--known-tags-file`, and it works by
matching a release tag inside the certificate SAN, so it applies to a
tag-bearing release identity and not to an email or a generic CI identity.
The tool is therefore turnkey for an identity that signs rarely enough to triage
each hit one at a time, and for a tag-bearing release identity via
`--known-tags-file`. For an identity that signs continuously it is not: its
first legitimate signature after the baseline leaves the monitor permanently
alerting, and closing that out needs an acknowledgment mechanism the tool does
not have yet.

Both examples here watch a tag-bearing release identity, so both pass
`--known-tags-file`. Two residual gaps come with that suppression. An attacker
who re-signs an *existing* release tag is suppressed, because that tag is
legitimately on the allowlist. And the allowlist keys on a completed release run
rather than on proof that the run's signing step succeeded, since signing happens
mid-run and a release that flaked afterwards still produced a real entry. Closing
both tightly needs a per-tag entry-count or provenance check, tracked in
[#1887](https://github.com/NVIDIA/aicr/issues/1887).

On a schedule, in your own repository:

```yaml
name: Signer identity monitor
on:
  schedule:
    - cron: "17 * * * *"
  workflow_dispatch:
    inputs:
      bootstrap:
        description: "Start without a prior checkpoint, baselining at the current log head. Only for the very first run or a deliberate re-arm."
        type: boolean
        default: false
permissions: {}
concurrency:
  group: signer-identity-monitor
  cancel-in-progress: false
env:
  CERT_SUBJECT: '^https://github\.com/myorg/myrepo/\.github/workflows/release\.yaml@refs/tags/.*$'
  CERT_ISSUER: '^https://token\.actions\.githubusercontent\.com$'
  # Must name the same workflow as CERT_SUBJECT: the SAN is
  # <this-workflow>@refs/tags/<tag>, so this workflow's run history is the
  # authoritative record of which tags a real release signed. Change both together.
  RELEASE_WORKFLOW_FILE: release.yaml
jobs:
  monitor:
    runs-on: ubuntu-latest
    timeout-minutes: 90
    permissions:
      contents: read
      actions: read   # prior checkpoint artifact + release workflow run history
    steps:
      - uses: actions/checkout@v7
        with:
          repository: NVIDIA/aicr
          # Pin the tool you audit by commit SHA. A scheduled security job should
          # not float on another project's default branch, and a tag is mutable;
          # bump this deliberately after review.
          ref: d4f7bef460dc8d1ef7ea0334a6935c0038de88e4
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod

      - name: Fetch previous checkpoint
        env:
          GH_TOKEN: ${{ github.token }}
          DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
          ALLOW_BOOTSTRAP: ${{ github.event_name == 'workflow_dispatch' && inputs.bootstrap }}
        run: |
          set -euo pipefail
          # The by-name artifact query is repo-wide: it also returns same-named
          # artifacts uploaded by a fork pull_request run, whose GITHUB_TOKEN can
          # upload under any name. A poisoned checkpoint (a genuine one copied
          # from the live log head) passes the consistency proof while collapsing
          # the identity-scan window to empty, silently skipping entries. So
          # accept only artifacts produced by this repository on its default
          # branch: head_repository_id == repository_id rejects every fork run,
          # and the head_branch match limits it to the scheduled runs.
          id="$(gh api --paginate \
            "repos/${GITHUB_REPOSITORY}/actions/artifacts?name=rekor-v2-checkpoint&per_page=100" \
            | jq -rs --arg branch "${DEFAULT_BRANCH}" '[.[].artifacts[]
                | select(.expired == false)
                | select(.workflow_run.head_branch == $branch)
                | select(.workflow_run.head_repository_id == .workflow_run.repository_id)]
                | sort_by(.created_at) | last | .id // empty')"
          if [ -n "${id}" ]; then
            gh api "repos/${GITHUB_REPOSITORY}/actions/artifacts/${id}/zip" > checkpoint.zip
            exit 0
          fi
          # No usable cursor: never uploaded, or expired past retention-days.
          # Baselining here would silently skip every entry added since the last
          # good run, so bootstrapping has to be asked for. A scheduled run fails
          # as an operational error instead.
          if [ "${ALLOW_BOOTSTRAP}" = "true" ]; then
            echo "Bootstrap requested: starting without a checkpoint."
            exit 0
          fi
          echo "::error::No usable checkpoint artifact from this repository on ${DEFAULT_BRANCH}."
          echo "Re-arm deliberately with a workflow_dispatch run and bootstrap=true."
          exit 1

      - name: Fetch signed release tags
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail
          # The suppression allowlist must come from an authoritative record of
          # what actually signed, NOT from `git tag --list`: an attacker-pushed or
          # never-signed tag sitting in the repository would be pre-suppressed.
          # The SAN is <workflow>@refs/tags/<tag>, so a tag-push run of
          # RELEASE_WORKFLOW_FILE is the proof that a real release signed <tag>.
          # Run history also persists after a tag or release is deleted, so an
          # ephemeral release candidate stays recognised. head_branch is the tag
          # for a tag-push run. Require status=completed but accept any
          # conclusion: signing happens mid-run, so a release that flaked in a
          # later step still produced a legitimate entry.
          gh api --paginate \
            "repos/${GITHUB_REPOSITORY}/actions/workflows/${RELEASE_WORKFLOW_FILE}/runs?event=push&status=completed&per_page=100" \
            > runs.json
          jq -rs '[.[].workflow_runs[] | select(.head_branch != null) | .head_branch] | unique | .[]' \
            runs.json > known-tags.txt
          echo "Allowlisted $(wc -l < known-tags.txt) signed release tags."

      - name: Scan for our signing identity
        run: |
          # Without --known-tags-file every legitimate release alerts, and the
          # tool holds the cursor on a match, so the first real release after
          # baseline would suspend the scan until triaged.
          go run ./tools/rekor-monitor \
            --file checkpoint_v2.txt \
            --restore-zip checkpoint.zip \
            --cert-subject "${CERT_SUBJECT}" \
            --cert-issuer "${CERT_ISSUER}" \
            --known-tags-file known-tags.txt

      - uses: actions/upload-artifact@v7
        if: ${{ !cancelled() }}
        with:
          name: rekor-v2-checkpoint
          # The cursor plus its .scan and .stall companions. All three must
          # travel together so the next run resumes an in-progress catch-up.
          path: |
            checkpoint_v2.txt
            checkpoint_v2.txt.scan
            checkpoint_v2.txt.stall
          # The cursor has to outlast the longest gap between *successful* runs,
          # which is a much wider window than the cron interval: an expired
          # artifact is a lost cursor. The ceiling is the repository's artifact
          # retention maximum, 90 days on a public repository.
          retention-days: 30
          if-no-files-found: ignore
```

The `ref:` above and the `git checkout --detach` in the shell example are
already full-length commit SHAs, because the source they pull in *is* the
monitor you are trusting and a tag is mutable. The `uses:` action references are
left as readable version tags so the example stays legible; pin those by SHA too
in production, the same argument the digest-pinning advice earlier in this guide
makes for images.

Two further hardening steps from AICR's own workflow are worth copying before
you rely on this. It branches notifications on the `CLASSIFICATION=` value, so
that Sigstore or GitHub-API flakiness produces a quiet, self-healing failure
rather than paging like a security event. And it wraps every checkpoint API call
in a retry helper
([`.github/scripts/gh-api-retry.sh`](https://github.com/NVIDIA/aicr/blob/main/.github/scripts/gh-api-retry.sh)),
which recovers 5xx and 429 responses and publishes each response through
`OUTFILE.part` so a failed attempt's error body can never be mistaken for a
checkpoint. The example above is left plain because it already fails closed.

### Monitoring Rekor v1 and a private Rekor

If you opted signing out to Rekor v1 with `aicr bundle --rekor-url`, be aware
that neither of the two obvious cases has a turnkey monitor today. Both are
coverage gaps, not recipes.

**The public-good Rekor v1 gives no sustained coverage.** Identity monitoring is
a linear scan of every entry added since the last checkpoint, because Rekor's
index cannot be queried by certificate SAN. On the v1 firehose that scan runs
roughly fifty times slower than the log grows, so a bounded CI job can never
catch up. AICR measured exactly this before moving its own monitoring to v2 (see
the rationale in
[`.github/workflows/rekor-monitor.yaml`](https://github.com/NVIDIA/aicr/blob/main/.github/workflows/rekor-monitor.yaml)).
The upstream workflow will still complete its consistency check and record a
baseline, but the identity search behind it falls further behind on every run.

**A private Rekor v1 fails before it starts.** Upstream resolves the checkpoint
signing key by matching the log's key ID against the Rekor logs listed in
Sigstore's TUF-distributed trusted root, which does not contain a private log's
key: `GetLogVerifier` returns `couldn't find matching log instance` and the run
aborts. The upstream binary does accept `--tuf-repository` and `--tuf-root-path`
to point at your own trust material, but the reusable workflow's inputs are
`file_issue`, `artifact_retention_days`, `once`, `config`, and `url` only, so
there is no way to pass them through it. Running the binary directly against a
TUF repository that serves your log's key is the path here, not the reusable
workflow.

What the upstream workflow does still illustrate is the configuration format,
which is worth reading even if you end up driving the binary yourself. Sigstore's
[walkthrough of rekor-monitor](https://blog.sigstore.dev/using-rekor-monitor/)
covers the same ground in more depth, including the split between its consistency
check and its identity search, and the key-fingerprint mode below.

Read the block below for the shape of `monitoredValues` only. It is not a
turnkey recipe: against the public-good Rekor v1 the identity search cannot keep
up, and it carries no `url` because pointing it at a private log fails at
verifier setup for the reason given above.

```yaml
name: Signer identity monitor (Rekor v1)
on:
  schedule:
    - cron: "0 * * * *"
permissions: {}
jobs:
  monitor:
    permissions:
      contents: read
      issues: write
      # Required: upstream's detect-workflow job mints an OIDC token to resolve
      # which reusable repository and ref it is running from. Because that token
      # is minted as YOUR repository, the third-party workflow below must be
      # pinned by full-length commit SHA, not a mutable @main.
      id-token: write
    uses: sigstore/rekor-monitor/.github/workflows/reusable_monitoring.yml@aa97a44631e9ae0f2b262a77ee5ea8edbb11e26c
    with:
      file_issue: true
      artifact_retention_days: 14
      # No `url`: it defaults to the public-good Rekor v1, and a private log
      # cannot be reached through this workflow at all. Drive the binary
      # directly with --tuf-repository / --tuf-root-path instead.
      config: |
        monitoredValues:
          certIdentities:
            - certSubject: ^ci@myorg\.example\.com$
              issuers:
                - ^https://keycloak\.internal\.example\.com/realms/myorg$
```

`issuers` is matched against the certificate's OIDC issuer extension
(`1.3.6.1.4.1.57264.1.8`), which records the identity provider that
authenticated the signer. It is not the Fulcio CA that issued the certificate,
so a private Fulcio URL here matches nothing: put your own IdP's issuer URL in
it, as above. Upstream compiles both `certSubject` and each entry of `issuers`
as Go regular expressions and requires both to match, so anchor them for the
same reason you anchor the flags above, and keep the pair coherent the same way.

A KMS-signed entry carries no certificate, so there is no subject or issuer to
match. Watch the key instead, via `fingerprints`, which is the hex-encoded
SHA-256 of the DER-encoded public key:

```shell
cosign public-key --key gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k \
  | openssl pkey -pubin -outform DER \
  | openssl dgst -sha256 -hex | awk '{print $NF}'
```

Then substitute that digest for the `config:` block in the workflow above:

```yaml
      config: |
        monitoredValues:
          fingerprints:
            - <hex digest from the command above>
```

**The remaining coverage gaps, stated plainly.** Alongside the two Rekor v1
cases above, three more signing modes have no turnkey monitor:

- A **KMS key signing to the default Rekor v2**: `tools/rekor-monitor` exposes
  only the certificate identity flags, even though the upstream library it
  builds on does match v2 entries by key fingerprint.
- A **private Rekor v2** (reached with `aicr bundle --signing-config`): out of
  reach for both tools, because `tools/rekor-monitor` resolves its shards from
  Sigstore's TUF-distributed v2 signing config and takes no override.
- A **private Fulcio signing into the public-good Rekor v2**, the
  "Private Fulcio alone" row of the table (`aicr bundle --fulcio-url` with no
  `--rekor-url`): also unmonitorable by either tool, and this one fails quietly.
  `tools/rekor-monitor` materializes its trusted CA roots from Sigstore's public
  TUF trusted root and offers no flag to add your own, so when the upstream
  identity search reaches an entry whose certificate chain does not validate
  against those roots, it writes a note to stderr and skips the entry. That is
  neither a match nor a failure, so entries under your private CA never reach
  the SAN and issuer comparison and the monitor stays green. Upstream's binary
  does take `--ca-roots` and `--ca-intermediates`, but its reusable workflow
  exposes no input for them and cannot read Rekor v2 in the first place.

In every case, treat `tools/rekor-monitor` as a small reference implementation
to adapt, or sign those artifacts to a target one of the two monitors already
covers.

### Responding to a hit

1. **Rule yourself out first.** Cross-check the entry's log index and integrated
   timestamp against your own CI run history and any local signing you did. AICR
   automates this for its release identity by correlating matches against its
   release workflow's run history; for a personal identity the equivalent is
   asking whether you signed anything at that moment.
2. **Confirm the match is really your identity.** Re-read the entry's certificate
   SAN and OIDC issuer, or its key fingerprint, against what you configured. An
   unanchored regex is the usual cause of a false positive.
3. **Treat an unexplained hit as compromise of the identity, not of the
   artifact.** What "contain it" means depends on the signing mode:
   - *Interactive keyless, an email in the SAN.* The OIDC account is what was
     taken. Rotate its credentials, revoke active sessions and tokens, and read
     the identity provider's own audit log.
   - *CI workload identity, a workflow URL in the SAN.* There is no account
     session to rotate here. The SAN names a workflow inside a repository, so
     containment means treating that repository as compromised: audit and narrow
     the workflow permissions, rotate every secret and deployment token those
     workflows can reach, review the environments and their protection rules,
     and look through the named workflow's run history for a run nobody
     triggered.
   - *KMS key.* Rotate the key and revoke the principals allowed to sign with
     it.
4. **Re-verify what you already published, but do not mistake what that proves.**
   The command depends on what you signed:
   - Keyless bundles: `aicr verify ./my-bundle --require-creator <identity>`.
   - KMS-signed bundles: there is no certificate creator to require, so verify
     against the key itself with `aicr verify ./my-bundle --key <kms-uri-or-pem>`.
   - Recipe evidence: `aicr evidence verify <evidence-bundle>`, pinning the
     signer with `--expected-issuer` and `--expected-identity-regexp`.
   - Anything signed against a private Sigstore: add
     `--trust-root ./trusted_root.json` to `aicr verify`.

   None of these separates yours from the attacker's inside the matched set,
   because a compromised identity satisfies the check exactly as well as you do.
   Establish legitimacy from a record the attacker does not control: compare each
   artifact's digest and provenance against your own build and release history.
   Quarantine anything you cannot account for there.
5. **Expect the entry to be permanent.** Nothing can be removed from a
   transparency log, so the response is rotation plus a public statement of which
   entries are legitimate, never takedown.

## References

- [GitHub Artifact Attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
- [SLSA Framework](https://slsa.dev/)
- [GitHub Actions SLSA Generation](https://github.com/slsa-framework/slsa-github-generator)
- [SPDX Specification](https://spdx.dev/)
- [CycloneDX Specification](https://cyclonedx.org/specification/overview/)
- [OpenVEX Specification](https://github.com/openvex/spec)
- [Sigstore Cosign](https://docs.sigstore.dev/cosign/signing/overview/)
- [Sigstore Policy Controller](https://docs.sigstore.dev/policy-controller/overview/)
- [Kyverno Image Verification](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/overview/)
- [Sigstore Rekor](https://docs.sigstore.dev/logging/overview/), on why artifact owners should monitor the log for their own identity
- [Sigstore Threat Model](https://docs.sigstore.dev/about/threat-model/), which positions identity and consistency monitoring as the detection control for a compromised signer
- [Using rekor-monitor to Scan Your Transparency Logs](https://blog.sigstore.dev/using-rekor-monitor/), Sigstore's walkthrough of consistency checking, identity search, and key-fingerprint monitoring
- [sigstore/rekor-monitor](https://github.com/sigstore/rekor-monitor), the upstream tool and its reusable workflow inputs
- [sigstore.dev and Rekor evolution](https://blog.sigstore.dev/rekor-evolution/), Sigstore's current statement on Rekor v1 remaining the public-good default and v2 being opt-in

# Supply Chain Verification

This guide is for integrators wiring AICR artifact verification into CI
pipelines, clusters, and audit tooling. It collects the full command
walkthroughs for verifying build provenance, SBOMs, and image/bundle
attestations, plus admission-policy enforcement and offline/air-gapped
verification.

For a quick trust overview and how to report a vulnerability, see the
top-level [`SECURITY.md`](../../SECURITY.md).

## Prerequisites and Setup

Verification uses [Cosign](https://docs.sigstore.dev/cosign/system_config/installation/),
the [GitHub CLI](https://cli.github.com/) (`gh`), `crane` (recommended; `docker inspect` resolves a digest only after a local pull),
`jq`, and — for in-cluster enforcement — `kubectl`. Binary and bundle
verification (`aicr verify`) need only the `aicr` binary.

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

```shell
# Verify provenance exists and is valid (using digest)
gh attestation verify oci://${IMAGE_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Output shows:
# ✓ Verification succeeded!
#
# Attestations:
#   • Build provenance (SLSA v1.0)
# (the SPDX SBOM is a separate Cosign attestation — see Verifying the SBOM)
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

AICR provides **SBOMs in SPDX v2.3 JSON format**: binary SBOMs as separate GoReleaser
artifacts (generated alongside CLI binaries) and container image SBOMs attached
as Cosign attestations (generated by Syft/Anchore).

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

### Container image SBOM

```shell
# Method 1: Using Cosign (extracts attestation) - uses digest to avoid warnings
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE_API_DIGEST} | \
  jq -r '.payload' | base64 -d | jq '.predicate' > sbom.json

# Method 2: GitHub CLI (build provenance only; the SPDX SBOM needs Method 1's Cosign flow)
gh attestation verify oci://${IMAGE_API_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}" --format json
```

### SBOM format

Both binary and container SBOMs are SPDX v2.3 JSON. A representative
package entry (the full document lists every Go module and its transitive
dependencies, licenses, and package URLs):

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

### SBOM use cases

```shell
# Vulnerability scanning — feed the SBOM to Grype, Anchore, or Snyk
grype sbom:./sbom.json

# License compliance — list declared licenses
jq -r '.packages[] | select(.licenseDeclared != "NOASSERTION") | "\(.name) \(.versionInfo) \(.licenseDeclared)"' sbom.json

# Dependency tracking — search for a specific component
jq '.packages[] | select(.name | contains("vulnerable-lib"))' sbom.json

# Audit trail — the SBOM timestamp proves when components were included
jq '.creationInfo.created' sbom.json
```

## Verifying Image and Bundle Attestations

### Container image attestations

**Method 1: GitHub CLI (recommended)**

```shell
# Verify using digest (preferred - no warnings)
gh attestation verify oci://${IMAGE_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Verify the aicrd image
gh attestation verify oci://${IMAGE_API_DIGEST} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"

# Note: You can still use tags, but tools may show warnings about mutability
# gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --repo NVIDIA/aicr --signer-workflow NVIDIA/aicr/.github/workflows/attest-images.yaml --source-ref "refs/tags/${TAG}"
```

**Method 2: Cosign (SBOM attestations)**

```shell
# Verify SBOM attestation using digest (preferred - avoids warnings)
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE_DIGEST}

# Extract and view the SBOM predicate
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/NVIDIA/aicr/\.github/workflows/attest-images\.yaml@refs/tags/.+$' \
  ${IMAGE_DIGEST} | jq -r '.payload' | base64 -d | jq '.predicate'
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
keyless OIDC, binding the bundle creator's identity to the generated payload
files listed in `checksums.txt`, including `recipe.yaml` in Helm bundles, and the binary that produced it (via
`resolvedDependencies`). Attestation is opt-in; bundles are unsigned by
default. The bundle output includes `bundle-attestation.sigstore.json`
(SLSA Build Provenance v1 for the bundle) and a copy of the binary's
`aicr-attestation.sigstore.json` (provenance chain).

```shell
aicr verify ./my-bundle
```

This verifies checksums against `checksums.txt`, the bundle attestation
against the Sigstore trusted root, and the binary attestation provenance
chain (identity pinned to NVIDIA CI). Enforce a minimum trust level:

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

## References

- [GitHub Artifact Attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
- [SLSA Framework](https://slsa.dev/)
- [GitHub Actions SLSA Generation](https://github.com/slsa-framework/slsa-github-generator)
- [SPDX Specification](https://spdx.dev/)
- [Sigstore Cosign](https://docs.sigstore.dev/cosign/signing/overview/)
- [Sigstore Policy Controller](https://docs.sigstore.dev/policy-controller/overview/)
- [Kyverno Image Verification](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/overview/)

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
GOFLAGS="-mod=vendor" go run ./tools/rekor-monitor \
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
          GOFLAGS="-mod=vendor" go run ./tools/rekor-monitor \
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
- [Sigstore Cosign](https://docs.sigstore.dev/cosign/signing/overview/)
- [Sigstore Policy Controller](https://docs.sigstore.dev/policy-controller/overview/)
- [Kyverno Image Verification](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/overview/)
- [Sigstore Rekor](https://docs.sigstore.dev/logging/overview/), on why artifact owners should monitor the log for their own identity
- [Sigstore Threat Model](https://docs.sigstore.dev/about/threat-model/), which positions identity and consistency monitoring as the detection control for a compromised signer
- [Using rekor-monitor to Scan Your Transparency Logs](https://blog.sigstore.dev/using-rekor-monitor/), Sigstore's walkthrough of consistency checking, identity search, and key-fingerprint monitoring
- [sigstore/rekor-monitor](https://github.com/sigstore/rekor-monitor), the upstream tool and its reusable workflow inputs
- [sigstore.dev and Rekor evolution](https://blog.sigstore.dev/rekor-evolution/), Sigstore's current statement on Rekor v1 remaining the public-good default and v2 being opt-in

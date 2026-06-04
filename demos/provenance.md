# Build Provenance Demo

Every released AICR binary and image carries a signed SLSA Build Provenance v1
attestation and an SPDX SBOM, produced by NVIDIA CI on tag and recorded in the
public Rekor transparency log. This demo walks the consumer-side verification
chain — what a downstream operator runs to confirm an artifact came from NVIDIA,
from a known commit, and contains a known software bill of materials.

The producer side runs in `.github/workflows/on-tag.yaml`; nothing here needs
NVIDIA credentials or repo access. The companion script is
[`provenance-demo.sh`](provenance-demo.sh); the slide deck is
[`provenance-demo-slides.html`](provenance-demo-slides.html).

This walkthrough covers:

1. Resolve the latest release tag to an immutable digest.
2. Verify the SLSA Provenance v1 attestation with `gh attestation verify`.
3. Verify the SPDX SBOM attestation with `cosign verify-attestation`.
4. Use the SBOM for vulnerability scanning and license compliance.
5. Audit the signature in the Rekor transparency log.
6. Enforce verification at admission time with Sigstore Policy Controller or Kyverno.

## Prerequisites

| Tool | Why |
|------|-----|
| `curl`, `jq` | Resolve the latest release tag from the GitHub API |
| [`crane`](https://github.com/google/go-containerregistry/tree/main/cmd/crane) | Resolve mutable tags to immutable digests without pulling layers |
| [`gh`](https://cli.github.com/) | Verifies attestations against NVIDIA's identity (`gh attestation verify`) |
| [`cosign`](https://github.com/sigstore/cosign) | Verifies SBOM attestations with an explicit signer policy |
| `grype` *(optional)* | SBOM-based vulnerability scan |
| `rekor-cli` *(optional)* | Transparency-log lookup |

Bootstrap the Sigstore trusted root once on the verifier's machine:

```shell
aicr trust update
```

## 1. Resolve the latest tag to a digest

A registry tag is mutable — anyone with push rights can move it. A digest is a
content hash, so resolving once and pinning everything else to the digest gives
every check the same anchor.

```shell
TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
IMAGE=ghcr.io/nvidia/aicr
DIGEST=$(crane digest "${IMAGE}:${TAG}")
IMAGE_DIGEST="${IMAGE}@${DIGEST}"
echo "$IMAGE_DIGEST"
# ghcr.io/nvidia/aicr@sha256:f0c1...
```

The `aicrd` server image is published in lockstep, but its digest is
independent — resolve it separately:

```shell
IMAGE_AICRD=ghcr.io/nvidia/aicrd
DIGEST_AICRD=$(crane digest "${IMAGE_AICRD}:${TAG}")
```

## 2. Verify the image (SLSA Provenance v1)

`gh attestation verify` is the simplest path: it pulls the attestation from
GHCR, validates the Sigstore signature against the Fulcio cert chain and a
Rekor inclusion proof, and enforces that the signer is a workflow owned by
`--owner`.

```shell
gh attestation verify "oci://${IMAGE_DIGEST}" --owner NVIDIA
```

Expected output:

```text
Loaded digest sha256:f0c1... for oci://ghcr.io/nvidia/aicr@sha256:f0c1...
Loaded 1 attestation from GitHub API
✓ Verification succeeded!

The following policy criteria will be enforced:
  - OIDC Issuer must match: https://token.actions.githubusercontent.com
  - Source Repository Owner URI must match: https://github.com/NVIDIA
  - Predicate type must match: https://slsa.dev/provenance/v1
```

Same for `aicrd`:

```shell
gh attestation verify "oci://${IMAGE_AICRD}@${DIGEST_AICRD}" --owner NVIDIA
```

### Why pin to the digest

A tag-only verify (`oci://ghcr.io/nvidia/aicr:vX.Y.Z`) resolves to whatever
digest the tag points at *now*. If the tag is repointed between resolve and
verify, the two see different artifacts. Always pass `@sha256:...`.

## 3. Verify the SPDX SBOM attestation

Different predicate, same trust model. `cosign verify-attestation` enforces an
explicit signer policy (issuer + identity regex) and writes the verified DSSE
envelope to disk:

```shell
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/.*' \
  "${IMAGE_DIGEST}" \
  --output-file predicate.json
```

The output file is a DSSE envelope; the inner `predicate` is the SPDX SBOM:

```shell
jq -r '.payload' predicate.json | base64 -d | jq '.predicate' > sbom.json
```

### Alternative: binary SBOM from the release page

Released binaries carry their SBOM as a separate release asset (not an OCI
attestation). Fetch it directly:

```shell
VERSION=${TAG#v}                                 # strip the 'v' prefix
gh release download "$TAG" \
  --repo NVIDIA/aicr \
  --pattern "aicr_${VERSION}_linux_arm64.sbom.json" \
  --clobber \
  --output sbom.json
```

## 4. SBOM use cases

The SBOM is the same SPDX JSON whether sourced from the OCI attestation or the
release page, so downstream tools work either way.

**Vulnerability scan:**

```shell
grype sbom:./sbom.json
```

Feed the same SBOM to Anchore or Snyk; the format is portable.

**License compliance:**

```shell
jq -r '.packages[]
  | select(.licenseDeclared != "NOASSERTION")
  | "\(.name) \(.versionInfo) \(.licenseDeclared)"' sbom.json
```

**Dependency search (e.g. checking exposure to a named CVE):**

```shell
jq '.packages[] | select(.name | contains("vulnerable-lib"))' sbom.json
```

**Audit trail:**

```shell
jq -r '.creationInfo.created' sbom.json
```

## 5. Audit via Rekor

Rekor is the public, append-only transparency log of every Sigstore signature
ever made. Searching by content digest finds every attestation made against
that artifact — no NVIDIA contact required.

```shell
rekor-cli search --sha "${DIGEST#sha256:}"
# Found matching entries (listed by UUID):
# 24296fb24b8ad77a...

rekor-cli get --uuid 24296fb24b8ad77a... --format json | jq '.body'
```

The Rekor entry is independently witnessed by third parties, so a removed or
tampered attestation in GHCR doesn't erase the historical record.

For build history, the GitHub Actions run logs are public:

```shell
gh run list --repo NVIDIA/aicr --workflow=on-tag.yaml
gh run view <run-id> --repo NVIDIA/aicr --log
```

## 6. In-cluster verification

Verification at `docker pull` time is opt-in per consumer. To make it a
property of the cluster, enforce it with an admission controller. Two
production-grade options:

### Option 1: Sigstore Policy Controller

Install:

```shell
kubectl apply -f https://github.com/sigstore/policy-controller/releases/download/v0.10.0/release.yaml
```

Policy:

```yaml
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: aicr-images-require-attestation
spec:
  images:
  - glob: "ghcr.io/nvidia/aicr*"
  authorities:
  - keyless:
      url: https://fulcio.sigstore.dev
      identities:
      - issuerRegExp: ".*\\.github\\.com.*"
        subjectRegExp: "https://github.com/NVIDIA/aicr/.*"
    attestations:
    - name: build-provenance
      predicateType: https://slsa.dev/provenance/v1
      policy:
        type: cue
        data: |
          predicate: buildDefinition: buildType: "https://actions.github.io/buildtypes/workflow/v1"
```

### Option 2: Kyverno

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: verify-aicr-attestations
spec:
  validationFailureAction: Enforce
  rules:
  - name: verify-attestation
    match:
      any:
      - resources:
          kinds: [Pod]
    verifyImages:
    - imageReferences:
      - "ghcr.io/nvidia/aicr*"
      attestations:
      - predicateType: https://slsa.dev/provenance/v1
        attestors:
        - entries:
          - keyless:
              issuer: https://token.actions.githubusercontent.com
              subject: https://github.com/NVIDIA/aicr/.github/workflows/*
```

### Test enforcement

Allowed (attested image):

```shell
kubectl run aicr-ok --image="${IMAGE}:${TAG}"
```

Rejected (unsigned image):

```shell
kubectl run no-attestation --image=nginx:latest
# Error from server: image verification failed: no matching attestations found
```

## Troubleshooting

**"sigstore verification failed — trusted root may be stale"** — run `aicr trust update`.

**`gh attestation verify` returns "no attestations found"** — the artifact predates
the GitHub-attestation rollout (initially shipped in CI mid-2024) or the digest is
wrong. Verify with `cosign verify-attestation` instead, or confirm the digest with
`crane digest`.

**`cosign verify-attestation` returns "no matching attestations"** — the identity
regex is too strict, or the attestation lives on a different artifact (the
attestation is anchored to the OCI manifest digest, and multi-arch images have a
manifest list above the per-arch manifest). Try `cosign tree "${IMAGE_DIGEST}"`
to enumerate what's actually attached.

## Links

* [`provenance-demo.sh`](provenance-demo.sh) — runnable version of this walkthrough
* [`provenance-demo-slides.html`](provenance-demo-slides.html) — slide deck
* [`bundle-attestation.md`](bundle-attestation.md) — bundle attestation (parallel demo)
* [`evidence.md`](evidence.md) — recipe evidence (parallel demo)
* [SECURITY.md](../SECURITY.md) — trust model overview
* [SLSA v1.0](https://slsa.dev/spec/v1.0/) — provenance specification

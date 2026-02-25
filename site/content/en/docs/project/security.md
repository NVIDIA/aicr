---
title: "Security"
linkTitle: "Security"
weight: 60
description: "Security policy and vulnerability reporting"
---


NVIDIA is dedicated to the security and trust of our software products and services, including all source code repositories.

**Please do not report security vulnerabilities through GitHub.**

## Reporting Security Vulnerabilities

To report a potential security vulnerability in any NVIDIA product:

- **Web**: [Security Vulnerability Submission Form](https://www.nvidia.com/object/submit-security-vulnerability.html)
- **Email**: psirt@nvidia.com
  - Use [NVIDIA PGP Key](https://www.nvidia.com/en-us/security/pgp-key) for secure communication

**Include in your report**:
- Product/Driver name and version
- Type of vulnerability (code execution, denial of service, buffer overflow, etc.)
- Steps to reproduce
- Proof-of-concept or exploit code
- Potential impact and exploitation method

NVIDIA offers acknowledgement for externally reported security issues under our coordinated vulnerability disclosure policy. Visit [PSIRT Policies](https://www.nvidia.com/en-us/security/psirt-policies/) for details.

## Product Security Resources

For all security-related concerns: https://www.nvidia.com/en-us/security

## Supply Chain Security

AICR (AICR) provides supply chain security artifacts for all container images:

- **SBOM Attestation**: Complete inventory of packages, libraries, and components in SPDX format
- **SLSA Build Provenance**: Verifiable build information (how and where images were created)

### Container Image Attestations

All container images published from tagged releases include **multiple layers of attestations**, providing comprehensive supply chain security:

1. **Build Provenance** – SLSA attestations signed using GitHub's OIDC identity
2. **SBOM Attestations** – SPDX v2.3 JSON format signed with Cosign
3. **Binary SBOMs** – SPDX v2.3 JSON format embedded in CLI binaries via GoReleaser

#### Attestation Types

**Build Provenance (SLSA)**
- Complete record of the build environment, tools, and process
- Source repository URL and exact commit SHA
- GitHub Actions workflow that produced the artifact
- Build parameters and environment variables
- Cryptographically signed using Sigstore keyless signing
- SLSA Build Level 3 compliant

**SBOM Attestations**
- Complete inventory of packages, libraries, and dependencies
- SPDX v2.3 JSON format (industry standard)
- Attached to container images as attestations
- Signed with Cosign using keyless signing (Fulcio + Rekor)
- Enables vulnerability scanning and license compliance

#### Verify Image Attestations

**Setup - Get Latest Release Tag and Resolve Digest:**

```shell
# Get the latest release tag
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
echo "Using tag: $TAG"

# Resolve tag to immutable digest (requires crane or docker)
export IMAGE="ghcr.io/nvidia/aicr"
export DIGEST=$(crane digest "${IMAGE}:${TAG}" 2>/dev/null || docker inspect "${IMAGE}:${TAG}" --format='{{index .RepoDigests 0}}' | cut -d'@' -f2)
echo "Resolved digest: $DIGEST"

# Use digest for verification (recommended - immutable reference)
export IMAGE_DIGEST="${IMAGE}@${DIGEST}"
```

**Why use digests?** Tags are mutable and can be changed to point to different images. Digests are immutable SHA256 hashes that uniquely identify an image, providing stronger security guarantees.

**Method 1: GitHub CLI (Recommended)**

```shell
# Verify using digest (preferred - no warnings)
gh attestation verify oci://${IMAGE_DIGEST} --owner nvidia

# Verify the aicrd image
export IMAGE_API="ghcr.io/nvidia/aicrd"
export DIGEST_API=$(crane digest "${IMAGE_API}:${TAG}")
gh attestation verify oci://${IMAGE_API}@${DIGEST_API} --owner nvidia

# Note: You can still use tags, but tools may show warnings about mutability
# gh attestation verify oci://ghcr.io/nvidia/aicr:${TAG} --owner nvidia
```

**Method 2: Cosign (SBOM Attestations)**

```shell
# Verify SBOM attestation using digest (preferred - avoids warnings)
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/.*' \
  ${IMAGE_DIGEST}

# Extract and view SBOM
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/.*' \
  ${IMAGE_DIGEST} | jq -r '.payload' | base64 -d | jq '.predicate'
```

**Method 3: Policy Enforcement (Kubernetes)**

See [In-Cluster Verification](#in-cluster-verification) section below for automated admission policies.

#### What's Included in Attestations

**Build Provenance Contains:**
- Build trigger (tag push event)
- Builder identity (GitHub Actions runner)
- Source repository and commit SHA
- Build workflow path and run ID
- Build parameters and environment
- Dependencies used during build
- Timestamp and build duration

**SBOM Contains:**
- All Go module dependencies with versions
- Transitive dependencies (full dependency tree)
- Package licenses (SPDX identifiers)
- Package URLs (purl) for each component
- Container base image layers
- System packages from base image

For more information:
- [GitHub Artifact Attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
- [SLSA Framework](https://slsa.dev/)
- [SPDX Specification](https://spdx.dev/)
- [Sigstore Cosign](https://docs.sigstore.dev/cosign/overview/)

### CLI Binary Attestation

CLI binary releases are attested with SLSA Build Provenance v1 using Cosign keyless
signing via GitHub Actions OIDC. Each release archive (`.tar.gz`) contains:

- `aicr` — the binary
- `aicr-attestation.sigstore.json` — SLSA Build Provenance v1 attestation (Sigstore bundle format)

The attestation cryptographically proves which repository, commit, and workflow produced
the binary. It is logged to the public [Rekor](https://rekor.sigstore.dev/) transparency
log and can be verified offline.

**Verify a binary attestation:**

```shell
cosign verify-blob-attestation \
  --bundle aicr-attestation.sigstore.json \
  --type https://slsa.dev/provenance/v1 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/on-tag\.yaml@refs/tags/.*' \
  aicr
```

The install script (`./install`) performs this verification automatically when
[Cosign](https://docs.sigstore.dev/cosign/system_config/installation/) is available.

**On-demand attested builds:** The `Build Attested Binaries` workflow
(`.github/workflows/build-attested.yaml`) can be triggered manually from the
Actions tab to produce attested binaries from any branch without cutting a release.

### Setup

Export variables for the image you want to verify:

```shell
# Get latest release tag
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')

export IMAGE="ghcr.io/nvidia/aicr"
export IMAGE_TAG="$IMAGE:$TAG"

# Get digest for the tag (requires crane or docker)
export DIGEST=$(crane digest "$IMAGE_TAG" 2>/dev/null || docker inspect "$IMAGE_TAG" --format='{{index .RepoDigests 0}}' | cut -d'@' -f2)
export IMAGE_DIGEST="$IMAGE@$DIGEST"
export IMAGE_SBOM="$IMAGE:sha256-$(echo "$DIGEST" | cut -d: -f2).sbom"
```

**Authentication** (if needed):
```shell
docker login ghcr.io
```

### Software Bill of Materials (SBOM)

AICR provides **SBOMs in SPDX v2.3 JSON format** for comprehensive supply chain visibility:

1. **Binary SBOMs** – Embedded in CLI binaries (SPDX v2.3 JSON)
2. **Container Image SBOMs** – Attached as attestations (SPDX v2.3 JSON)

#### Binary SBOMs (CLI)

Generated by GoReleaser during release builds, embedded directly in binaries.

**Access Binary SBOM:**

```shell
# Get latest release tag
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
export VERSION=${TAG#v}  # Remove 'v' prefix for filenames

# Detect OS and architecture
export OS=$(uname -s | tr '[:upper:]' '[:lower:]')
export ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

# Download binary from GitHub releases
curl -LO https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_${TAG}_${OS}_${ARCH}
chmod +x aicr_${TAG}_${OS}_${ARCH}

# Download SBOM (separate file)
curl -LO https://github.com/NVIDIA/aicr/releases/download/${TAG}/aicr_${VERSION}_${OS}_${ARCH}.sbom.json

# View SBOM
cat aicr_${VERSION}_${OS}_${ARCH}.sbom.json
```

**Binary SBOM Format** (SPDX v2.3):

```json
{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "aicr",
  "documentNamespace": "https://anchore.com/syft/file/aicr-610e106b-2614-434c-bfe6-941863de47ff",
  "creationInfo": {
    "licenseListVersion": "3.27",
    "creators": [
      "Organization: Anchore, Inc",
      "Tool: syft-1.38.2"
    ],
    "created": "2026-01-01T16:52:12Z"
  },
  "packages": [
    {
      "name": "github.com/NVIDIA/aicr",
      "SPDXID": "SPDXRef-Package-go-module-github.com-NVIDIA-aicr-f06a66ba03567417",
      "versionInfo": "v0.8.12",  // Example version - actual version varies by release
      "supplier": "NOASSERTION",
      "downloadLocation": "NOASSERTION",
      "filesAnalyzed": false,
      "sourceInfo": "acquired package info from go module information: /aicr",
      "licenseConcluded": "NOASSERTION",
      "licenseDeclared": "NOASSERTION",
      "copyrightText": "NOASSERTION",
      "externalRefs": [
        {
          "referenceCategory": "SECURITY",
          "referenceType": "cpe23Type",
          "referenceLocator": "cpe:2.3:a:NVIDIA:aicr:v0.8.12:*:*:*:*:*:*:*"  // Example CPE
        },
        {
          "referenceCategory": "SECURITY",
          "referenceType": "cpe23Type",
          "referenceLocator": "cpe:2.3:a:NVIDIA:cloud_native_stack:v0.8.12:*:*:*:*:*:*:*"  // Example CPE
        },
        {
          "referenceCategory": "PACKAGE-MANAGER",
          "referenceType": "purl",
          "referenceLocator": "pkg:golang/github.com/NVIDIA/aicr@v0.8.12"  // Example purl
        }
      ]
    },
```

#### Container Image SBOMs (API Server & Agent)

Generated by Syft/Anchore, attached as Cosign attestations in SPDX v2.3 JSON format.

**Extract SBOM from Container Image:**

```shell
# Get latest release tag and resolve digest
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
export IMAGE="ghcr.io/nvidia/aicrd"
export DIGEST=$(crane digest "${IMAGE}:${TAG}")
export IMAGE_DIGEST="${IMAGE}@${DIGEST}"

# Method 1: Using Cosign (extracts attestation) - uses digest to avoid warnings
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/aicr/.github/workflows/.*' \
  ${IMAGE_DIGEST} | \
  jq -r '.payload' | base64 -d | jq '.predicate' > sbom.json

# Method 2: Using GitHub CLI (shows all attestations)
gh attestation verify oci://${IMAGE_DIGEST} --owner nvidia --format json
```

**Container SBOM Format** (SPDX v2.3 JSON):

```json
{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "aicr",
  "documentNamespace": "https://anchore.com/syft/file/aicr-610e106b-2614-434c-bfe6-941863de47ff",
  "creationInfo": {
    "licenseListVersion": "3.27",
    "creators": [
      "Organization: Anchore, Inc",
      "Tool: syft-1.38.2"
    ],
    "created": "2026-01-01T16:52:12Z"
  },
  "packages": [
    {
      "name": "github.com/NVIDIA/aicr",
      "SPDXID": "SPDXRef-Package-go-module-github.com-NVIDIA-aicr-f06a66ba03567417",
      "versionInfo": "v0.8.12",  // Example version - actual version varies by release
      ...
```

**SBOM Use Cases:**

1. **Vulnerability Scanning** – Feed SBOM to Grype, Trivy, or Snyk
   ```shell
   grype sbom:./sbom.json
   ```

2. **License Compliance** – Analyze licensing obligations
   ```shell
   jq -r '.packages[] | select(.licenseDeclared != "NOASSERTION") | "\(.name) \(.versionInfo) \(.licenseDeclared)"' sbom.json
   ```

3. **Dependency Tracking** – Monitor for supply chain risks
   ```shell
   jq '.packages[] | select(.name | contains("vulnerable-lib"))' sbom.json
   ```

4. **Audit Trail** – Maintain records for compliance
   ```shell
   # SBOM timestamp proves when components were included
   jq '.creationInfo.created' sbom.json
   ```
### SLSA Build Provenance

SLSA (Supply chain Levels for Software Artifacts) provides verifiable information about how images were built. AICR achieves **SLSA Build Level 3** through GitHub Actions OIDC integration.

#### What is SLSA?

SLSA is a security framework that protects against supply chain attacks by ensuring:
- **Source integrity** – Code comes from expected repository
- **Build integrity** – Build process is secure and reproducible
- **Provenance** – Complete record of how artifacts were created
- **Auditability** – Cryptographically signed evidence

#### SLSA Level 3 Requirements (Achieved)

- **Build as Code** – GitHub Actions workflows define reproducible builds  
- **Provenance Available** – Attestations generated for all releases  
- **Provenance Authenticated** – Signed using Sigstore keyless signing  
- **Service Generated** – GitHub Actions generates provenance (not self-asserted)  
- **Non-falsifiable** – Strong authentication of identity (OIDC)  
- **Dependencies Complete** – Full dependency graph in SBOM  

#### Verify SLSA Provenance

**Method 1: GitHub CLI**

```shell
# Get latest release tag and resolve digest
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
export IMAGE="ghcr.io/nvidia/aicr"
export DIGEST=$(crane digest "${IMAGE}:${TAG}")
export IMAGE_DIGEST="${IMAGE}@${DIGEST}"

# Verify provenance exists and is valid (using digest)
gh attestation verify oci://${IMAGE_DIGEST} --owner nvidia

# Output shows:
# ✓ Verification succeeded!
# 
# Attestations:
#   • Build provenance (SLSA v1.0)
#   • SBOM (SPDX)
```

**Method 2: Extract and Inspect Provenance**

```shell
# Get full provenance data (using digest)
gh attestation verify oci://${IMAGE_DIGEST} \
  --owner nvidia \
  --format json | jq '.[] | select(.verificationResult.statement.predicateType | contains("slsa"))'

# Key fields in provenance:
# - buildDefinition.buildType: GitHub Actions workflow type
# - runDetails.builder.id: Workflow file and commit
# - buildDefinition.externalParameters.workflow: Workflow path and ref
# - buildDefinition.resolvedDependencies: Source code commit SHA
# - runDetails.metadata.invocationId: GitHub run ID
```

**Example Provenance Data:**

```json
...
  "verificationResult": {
    "mediaType": "application/vnd.dev.sigstore.verificationresult+json;version=0.1",
    "signature": {
      "certificate": {
        "certificateIssuer": "CN=sigstore-intermediate,O=sigstore.dev",
        "subjectAlternativeName": "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v0.8.12",
        "issuer": "https://token.actions.githubusercontent.com",
        "githubWorkflowTrigger": "push",
        "githubWorkflowSHA": "ba6cbbe8b1a8fc8b72bb18454c10a3ba31d94a2e",
        "githubWorkflowName": "on_tag",
        "githubWorkflowRepository": "NVIDIA/aicr",
        "githubWorkflowRef": "refs/tags/v0.8.12",
        "buildSignerURI": "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v0.8.12",
        "buildSignerDigest": "ba6cbbe8b1a8fc8b72bb18454c10a3ba31d94a2e",
        "runnerEnvironment": "github-hosted",
        "sourceRepositoryURI": "https://github.com/NVIDIA/aicr",
        "sourceRepositoryDigest": "ba6cbbe8b1a8fc8b72bb18454c10a3ba31d94a2e",
        "sourceRepositoryRef": "refs/tags/v0.8.12",
        "sourceRepositoryIdentifier": "1095163471",
        "sourceRepositoryOwnerURI": "https://github.com/NVIDIA",
        "sourceRepositoryOwnerIdentifier": "1728152",
        "buildConfigURI": "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v0.8.12",
        "buildConfigDigest": "ba6cbbe8b1a8fc8b72bb18454c10a3ba31d94a2e",
        "buildTrigger": "push",
        "runInvocationURI": "https://github.com/NVIDIA/aicr/actions/runs/20642050863/attempts/1",
        "sourceRepositoryVisibilityAtSigning": "public"
      }
    },
...
```

#### In-Cluster Verification

Enforce provenance verification at deployment time using Kubernetes admission controllers.

**Option 1: Sigstore Policy Controller**

```shell
# Install Policy Controller
kubectl apply -f https://github.com/sigstore/policy-controller/releases/download/v0.10.0/release.yaml

# Create ClusterImagePolicy to enforce provenance
cat <<EOF | kubectl apply -f -
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
      - issuerRegExp: ".*\.github\.com.*"
        subjectRegExp: "https://github.com/NVIDIA/aicr/.*"
    attestations:
    - name: build-provenance
      predicateType: https://slsa.dev/provenance/v1
      policy:
        type: cue
        data: |
          predicate: buildDefinition: buildType: "https://actions.github.io/buildtypes/workflow/v1"
EOF
```

**Option 2: Kyverno Policy**

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
          kinds:
          - Pod
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

**Test Policy Enforcement:**

```shell
# Get latest release tag
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')

# This should succeed (image with valid attestation)
kubectl run test-valid --image=ghcr.io/nvidia/aicr:${TAG}

# This should fail (unsigned image)
kubectl run test-invalid --image=nginx:latest
# Error: image verification failed: no matching attestations found
```

#### Build Process Transparency

All AICR releases are built using GitHub Actions with full transparency:

1. **Source Code** – Public GitHub repository
2. **Build Workflow** – `.github/workflows/on-tag.yaml` (version controlled)
3. **Build Logs** – Public GitHub Actions run logs
4. **Attestations** – Signed and stored in public transparency log (Rekor)
5. **Artifacts** – Published to GitHub Releases and GHCR

**View Build History:**

```shell
# List all releases with attestations
gh api repos/NVIDIA/aicr/releases | \
  jq -r '.[] | "\(.tag_name): \(.html_url)"'

# View specific build logs
gh run list --repo NVIDIA/aicr --workflow=on-tag.yaml
gh run view 20642050863 --repo NVIDIA/aicr --log
```

**Verify in Transparency Log (Rekor):**

```shell
# Search Rekor for attestations
rekor-cli search --artifact ghcr.io/nvidia/aicr:v0.8.12

# Get entry details
rekor-cli get --uuid <entry-uuid>
```

For more information:
- [SLSA Framework Documentation](https://slsa.dev/)
- [GitHub Actions SLSA Generation](https://github.com/slsa-framework/slsa-github-generator)
- [Sigstore Policy Controller](https://docs.sigstore.dev/policy-controller/overview/)
- [Kyverno Image Verification](https://kyverno.io/docs/writing-policies/verify-images/)

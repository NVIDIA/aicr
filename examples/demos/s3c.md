# Software Supply Chain Security Demo

Demonstration of supply chain security artifacts provided by Eidos.

![software supply chain security](images/s3c.png)

## Overview

Eidos (Eidos) provides supply chain security artifacts:

- **SBOM Attestation**: Complete inventory of packages, libraries, and components in SPDX format
- **SLSA Build Provenance**: Verifiable build information (how and where images were created)
- **Keyless Signing**: Artifacts signed using Sigstore (Fulcio CA + Rekor Transparency Log)

## Image Attestations

**Build Provenance (SLSA L3)**
- Complete record of the build environment, tools, and process
- Source repository URL and exact commit SHA
- GitHub Actions workflow that produced the artifact
- Build parameters and environment variables
- Cryptographically signed using Sigstore keyless signing

Get latest release tag:

```shell
TAG=$(curl -s https://api.github.com/repos/NVIDIA/eidos/releases/latest | jq -r '.tag_name')
echo "Using tag: $TAG"
```
Resolve tag to immutable digest:

```shell
IMAGE="ghcr.io/nvidia/eidos"
DIGEST=$(crane digest "${IMAGE}:${TAG}")
echo "Resolved digest: $DIGEST"
IMAGE_DIGEST="${IMAGE}@${DIGEST}"
```

> Tags are mutable and can be changed to point to different images. Digests are immutable SHA256 hashes that uniquely identify an image, providing stronger security guarantees.

**Method 1: GitHub CLI (Recommended)**

Verify using digest:

```shell
gh attestation verify oci://${IMAGE_DIGEST} --owner NVIDIA
```

Verify the eidosd image:

```shell
IMAGE_API="ghcr.io/nvidia/eidosd"
DIGEST_API=$(crane digest "${IMAGE_API}:${TAG}")
gh attestation verify oci://${IMAGE_API}@${DIGEST_API} --owner NVIDIA
```

**Method 2: Cosign (SBOM Attestations)**

Verify SBOM attestation using digest:

```shell
cosign verify-attestation \
  --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp 'https://github.com/NVIDIA/eidos/.github/workflows/.*' \
  ${IMAGE_DIGEST} > predicate.json
```

## SBOM

**SBOM Attestations (SPDX v2.3 JSON for Binary & Images)**
- Complete inventory of packages, libraries, and dependencies
- Attached to container images as attestations
- Signed with Cosign using keyless signing (Fulcio + Rekor)
- Enables vulnerability scanning and license compliance

**Access Binary SBOM:**

Get latest release tag:

```shell
VERSION=${TAG#v}  # Remove 'v' prefix for filenames
echo "Using version: $VERSION"
```

Download SBOM:
```shell
curl -Ls https://github.com/NVIDIA/eidos/releases/download/${TAG}/eidos_${VERSION}_linux_arm64.sbom.json -o sbom.json
```

View SBOM
```shell
cat sbom.json | jq .
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
   jq '.creationInfo.created' sbom.json
   ```

### In-Cluster Verification

Enforce provenance verification at deployment time using Kubernetes admission controllers.

**Option 1: Sigstore Policy Controller**

Install Policy Controller:

```shell
kubectl apply -f https://github.com/sigstore/policy-controller/releases/download/v0.10.0/release.yaml
```
Create ClusterImagePolicy to enforce provenance:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: eidos-images-require-attestation
spec:
  images:
  - glob: "ghcr.io/nvidia/eidos*"
  authorities:
  - keyless:
      url: https://fulcio.sigstore.dev
      identities:
      - issuerRegExp: ".*\.github\.com.*"
        subjectRegExp: "https://github.com/NVIDIA/eidos/.*"
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
  name: verify-eidos-attestations
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
      - "ghcr.io/nvidia/eidos*"
      attestations:
      - predicateType: https://slsa.dev/provenance/v1
        attestors:
        - entries:
          - keyless:
              issuer: https://token.actions.githubusercontent.com
              subject: https://github.com/NVIDIA/eidos/.github/workflows/*
```

**Test Policy Enforcement:**

Get latest release tag:

```shell
TAG=$(curl -s https://api.github.com/repos/NVIDIA/eidos/releases/latest | jq -r '.tag_name')
```

This should succeed (image with valid attestation):

```shell
kubectl run test-valid --image=ghcr.io/nvidia/eidos:${TAG}
```
This should fail (unsigned image):

```shell
kubectl run test-invalid --image=nginx:latest
```

> Error: image verification failed: no matching attestations found

#### Build Process Transparency

All Eidos releases are built using GitHub Actions with full transparency:

1. **Source Code** – Public GitHub repository
2. **Build Workflow** – `.github/workflows/on-tag.yaml` (version controlled)
3. **Build Logs** – Public GitHub Actions run logs
4. **Attestations** – Signed and stored in public transparency log (Rekor)
5. **Artifacts** – Published to GitHub Releases and GHCR

**View Build History:**

List all releases with attestations:

```shell
gh api repos/NVIDIA/eidos/releases | \
  jq -r '.[] | "\(.tag_name): \(.html_url)"'
```

View specific build logs:

```shell
gh run list --repo NVIDIA/eidos --workflow=on-tag.yaml
gh run view 21076668418 --repo NVIDIA/eidos --log
```

**Verify in Transparency Log (Rekor):**

Search Rekor for attestations:

```shell
rekor-cli search --sha $(crane digest ghcr.io/nvidia/eidos:${TAG})
```

Get entry details:

```shell
rekor-cli get --uuid <entry-uuid>
```

## Links

* [Security](https://github.com/NVIDIA/eidos/blob/main/SECURITY.md)
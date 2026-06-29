# Security

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

## Acknowledgement and Response

Reports submitted through the channels above are received by NVIDIA PSIRT.
PSIRT acknowledges receipt of each report, coordinates with the reporter
throughout the investigation, and provides progress updates as the issue moves
toward remediation. The expectations for acknowledgement, ongoing
communication, and reporter credit are defined in the
[PSIRT Policies](https://www.nvidia.com/en-us/security/psirt-policies/), which
set the response timeline for AICR. If you do not receive an acknowledgement,
re-send your report to psirt@nvidia.com rather than opening a public issue.

## Embargo, Coordinated Disclosure, and CVEs

Confirmed vulnerabilities are handled under embargo. NVIDIA PSIRT manages the
coordinated disclosure timeline: the issue is kept confidential while a fix is
prepared, affected downstream consumers are notified where appropriate, and a
security advisory is published when the embargo lifts. NVIDIA is a CVE Numbering
Authority (CNA) and assigns CVE identifiers for resolved issues that require
user action, so downstream users can track and patch them through standard
vulnerability databases.

AICR maintainers support this process by developing fixes privately and holding
public discussion of an issue until PSIRT lifts the embargo. Please keep any
reported vulnerability confidential until then.

## Out of Scope

The following generally do **not** qualify as security vulnerabilities in AICR.
Reviewing this list before reporting helps PSIRT focus on real issues:

- Findings that require physical access to a node or control-plane host.
- Social engineering, phishing, or attacks that depend on an already-compromised
  operator credential or workstation.
- Theoretical weaknesses with no demonstrated, practical exploit path.
- Vulnerabilities in upstream components that AICR only deploys (for example the
  GPU Operator, Network Operator, or other Helm chart sub-images) — report those
  to the owning project. We will help coordinate when an AICR default is involved.
- Missing cluster hardening that is the operator's responsibility, such as
  network policy, secrets management, or RBAC tightening covered by the
  deployment documentation.

When in doubt, report it — PSIRT would rather triage an out-of-scope report than
miss a real one.

## Supported Versions

AICR is pre-1.0 and ships from a single active release line. Only the latest
released minor receives security fixes; earlier minors are end-of-life and
should be upgraded.

| Version | Supported |
|---------|-----------|
| `0.15.x` (latest released minor) | ✅ Receives security fixes |
| `< 0.15` | ❌ End-of-life — upgrade to the latest release |

Security fixes ship in a new patch or minor release rather than as backports to
end-of-life versions. When AICR reaches 1.0 this policy will be revised and a
longer support window published here.

## Product Security Resources

For all security-related concerns: https://www.nvidia.com/en-us/security

## Supply Chain Security

AICR (AI Cluster Runtime) provides supply chain security artifacts for all container images:

- **SBOM Attestation**: Complete inventory of packages, libraries, and components in SPDX format
- **SLSA Build Provenance**: Verifiable build information (how and where images were created)

### Deployed Image Inventory and Pinning Policy

Beyond AICR's own runtime images, every cluster deployed by AICR pulls a set
of upstream container images selected by the recipe and the Helm charts it
references. The complete list — chart-by-chart and image-by-image — is
published as a versioned doc artifact and refreshed weekly as upstream
charts evolve:

- [`docs/user/container-images.md`](docs/user/container-images.md) — human
  readable summary of every component, chart, and image AICR can deploy.
- A machine-readable [CycloneDX 1.6][cyclonedx] BOM is produced by `make
  bom` and published as a release asset. Tooling that consumes SBOMs
  (Trivy, Grype, Cosign attestation, in-toto) should prefer the JSON; the
  Markdown is its companion.

**Pinning policy** ([ADR-006][adr-006]) defines AICR's three-layer
contract:

1. **Chart versions** are pinned for every Helm component, with no
   exceptions. `recipes/registry.yaml` MUST declare `defaultVersion`;
   `make bom BOM_STRICT=1` enforces this in CI.
2. **Explicit image overrides** that AICR pins in-tree (in
   `recipes/components/<name>/values.yaml` or embedded manifests) carry
   `@sha256:` digests in addition to tags. Renovate handles digest
   rotation as upstream rebuilds the same tag.
3. **Chart-default sub-images** (the ones the upstream chart pulls
   without AICR overriding) ship at whatever the chart resolves at the
   pinned chart version. Reproducibility for these images is delivered by
   admission-time digest verification rather than per-sub-image overrides
   (see the [supply-chain epic][epic-739] for the roadmap).

**Upstream attestation coverage.** AICR's own runtime images and the
[NVSentinel][nvsentinel] images deployed by AICR ship with full
keyless cosign signatures, SLSA build provenance, and SBOM attestations
verifiable from the public Sigstore Rekor transparency log. Other
NVIDIA-owned images that AICR deploys today (gpu-operator,
network-operator, k8s-dra-driver-gpu, nodewright/skyhook) ship legacy
key-based cosign signatures but do not yet ship keyless signatures,
SLSA provenance, or SBOM attestations. Issues requesting parity have
been filed with each upstream and are tracked under the
[supply-chain epic][epic-739] (Stage 3).

[cyclonedx]: https://cyclonedx.org/specification/overview/
[adr-006]: docs/design/006-image-pinning-policy.md
[epic-739]: https://github.com/NVIDIA/aicr/issues/739
[nvsentinel]: https://github.com/NVIDIA/nvsentinel

### Trust Guarantees

All AICR artifacts published from tagged releases carry cryptographically
verifiable supply chain guarantees. The table below summarizes what exists
and how it is signed:

| Guarantee | Artifact | Format / Standard | Signing |
|-----------|----------|-------------------|---------|
| Build provenance | Container images | SLSA Build Level 3 (provenance v1) | Sigstore keyless via GitHub Actions OIDC |
| Build provenance | CLI binaries | SLSA Build Provenance v1 (Sigstore bundle) | Cosign keyless, logged to public Rekor |
| Signed SBOM | Container images | SPDX v2.3 JSON attestation | Cosign keyless (Fulcio + Rekor) |
| Embedded SBOM | CLI binaries | SPDX v2.3 JSON (via GoReleaser) | Released alongside attested binary |
| Bundle attestation | `aicr bundle` output | SLSA Build Provenance v1 | Sigstore keyless OIDC (opt-in `--attest`) |
| Recipe / bundle validity | `aicr verify` trust levels | `verified` / `attested` / `unverified` / `unknown` | Checksums + Sigstore trusted root |

`aicr verify` reports one of four trust levels for a bundle: `verified`
(full chain verified, binary identity pinned to NVIDIA CI), `attested`
(chain verified but binary attestation missing or external data used),
`unverified` (checksums valid, `--attest` was not used), and `unknown`
(missing checksums or attestation files).

### Verify an Artifact (Happy Path)

Verify the latest released CLI image against its immutable digest:

```shell
# Resolve the latest release tag to an immutable digest
export TAG=$(curl -s https://api.github.com/repos/NVIDIA/aicr/releases/latest | jq -r '.tag_name')
export IMAGE="ghcr.io/nvidia/aicr"
export DIGEST=$(crane digest "${IMAGE}:${TAG}")

# Verify build provenance and SBOM attestations
gh attestation verify "oci://${IMAGE}@${DIGEST}" --owner nvidia
# ✓ Verification succeeded!
#   • Build provenance (SLSA v1.0)
#   • SBOM (SPDX)
```

Verify a generated deployment bundle, enforcing the highest trust level:

```shell
aicr verify ./my-bundle --min-trust-level verified
```

### Deep-Dive: Verifying Artifacts

For the full operational reference — resolving digests, inspecting SLSA
provenance and SBOM contents, verifying CLI binary and bundle attestations,
enforcing verification in clusters with Kyverno or the Sigstore Policy
Controller, and offline/air-gapped verification with a local trusted root —
see [Supply Chain Verification](docs/integrator/supply-chain-verification.md).

For full CLI flag documentation, see the
[CLI Reference](docs/user/cli-reference.md#aicr-verify) (`aicr verify`,
`aicr bundle --attest`, `aicr trust update`). For a hands-on walkthrough,
see the [Bundle Attestation Demo](demos/bundle-attestation.md).

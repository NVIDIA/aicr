# AICR Bundle Attestation Demo

## Prerequisites

* Installed `aicr` from a release archive (includes binary attestation)
* Or: attested binary from `Build Attested Binaries` workflow

## Trust Setup

Bootstrap the Sigstore trusted root (the install script does this automatically,
but for completeness):

```shell
aicr trust update
```

## Generate Recipe

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --output recipe.yaml
```

## Create Attested Bundle

Default (no attestation):

```shell
aicr bundle \
  --recipe recipe.yaml \
  --output ./my-bundle
```

With attestation (opens browser for OIDC authentication):

```shell
aicr bundle \
  --recipe recipe.yaml \
  --output ./my-bundle \
  --attest
```

GitHub Actions (OIDC token detected automatically with `--attest`):

```shell
aicr bundle \
  --recipe recipe.yaml \
  --output ./my-bundle \
  --attest
```

## Verify Bundle

Auto-detect maximum trust level:

```shell
aicr verify ./my-bundle
```

Expected output (release binary):

```
  Checksums verified (12 files)
  Bundle attested by: jdoe@company.com
  Binary built by: https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0
  Identity pinned to NVIDIA CI
  Trust level: verified

Bundle verification: PASSED
```

## Policy Enforcement

Require minimum trust level:

```shell
aicr verify ./my-bundle --min-trust-level verified
aicr verify ./my-bundle --min-trust-level attested
```

Require specific creator:

```shell
aicr verify ./my-bundle --require-creator jdoe@company.com
```

Require a minimum CLI version (bare version defaults to >= semantics):

```shell
aicr verify ./my-bundle --cli-version-constraint 1.0.0
aicr verify ./my-bundle --cli-version-constraint ">= 1.0.0"
aicr verify ./my-bundle --cli-version-constraint "== 1.0.0"
```

JSON output (for CI pipelines):

```shell
aicr verify ./my-bundle --format json
```

## Trust Levels

| Level | Meaning |
|-------|---------|
| **verified** | Full chain: checksums + bundle attestation + binary attestation pinned to NVIDIA CI |
| **attested** | Chain verified but external data used, or binary attestation incomplete |
| **unverified** | Checksums valid, no attestation (`--attest`) |
| **unknown** | Missing or invalid checksums |

## Bundle Structure

```
my-bundle/
  checksums.txt                        # SHA256 of all content files
  recipe.yaml                          # Resolved recipe
  deploy.sh                            # Automation script
  README.md                            # Deployment guide
  attestation/
    bundle-attestation.sigstore.json   # SLSA Build Provenance v1
    aicr-attestation.sigstore.json     # Binary SLSA provenance
  <component>/
    values.yaml
    README.md
```

## Links

* [Security Model](../SECURITY.md)

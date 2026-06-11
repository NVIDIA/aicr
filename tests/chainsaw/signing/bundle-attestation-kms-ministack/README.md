# KMS MiniStack E2E Tests

End-to-end (E2E) test suite for KMS-backed bundle signing and verification
(`aicr bundle --attest --signing-key awskms://...`), exercised against
[MiniStack](https://ministack.org) so the full provider-resolution → sign →
verify round-trip runs in CI with no real AWS credentials and no paid license.

## Why this exists

PR #1205 added `--signing-key <kms-uri>` to `aicr bundle --attest`. Its unit
tests stub the KMS at the `kmsSigner` seam (a local ECDSA signer), so the
real provider path (`kms.Get("awskms://...")` → `SignerVerifier.PublicKey` →
`SignMessage`) is never exercised automatically. This suite closes that gap
by signing and verifying against a real (emulated) AWS KMS endpoint.

## What is MiniStack

[MiniStack](https://ministack.org) is an open-source AWS API emulator shipped
as a single Docker image (`ministackorg/ministack`). It serves AWS services on
one port (`4566`) and accepts any credentials, so the test needs no real AWS
account. Its KMS supports the asymmetric ECDSA P-256 (`ECC_NIST_P256`,
`SIGN_VERIFY`) keys that cosign's `awskms://` signer requires.

We use it instead of LocalStack because recent LocalStack images require a paid
license token to start, which does not suit an OSS CI gate. The image is pinned
in `.settings.yaml` (`testing_tools.ministack_image`) and resolved via the
`load-versions` action, never floated to `:latest`.

## TLS and mkcert

sigstore's `awskms` client hardcodes `https://` for the KMS endpoint, so the
emulator must serve TLS with a certificate the Go AWS SDK trusts. MiniStack
serves TLS when started with `USE_SSL=1`, using a mounted cert. We generate a
`localhost` cert with [`mkcert`](https://github.com/FiloSottile/mkcert) and run
`mkcert -install` so its CA lands in the system trust store (Keychain on macOS,
`ca-certificates` on Linux); the Go SDK then accepts `https://localhost:4566`.

Three distinct trust planes are involved:

- **`aicr` (Go)** trusts the cert via the **system trust store** (the mkcert CA).
- **The `aws` CLI (Python/botocore)** ignores the system store and uses its own
  bundle, so provisioning calls set **`AWS_CA_BUNDLE`** to the mkcert root CA.
- **Binary-attestation verification** uses the **public** Sigstore trust root,
  which is left untouched (mkcert appends its CA rather than replacing roots).

mkcert is pinned in `.settings.yaml` (`testing_tools.mkcert`). On macOS with
Colima, the cert directory must live under `$HOME` (Colima shares it into the
VM); a `/tmp` path is not visible inside the container.

## What is tested

| Step | What it verifies |
|------|-----------------|
| `bundle-with-kms-signing` | `aicr bundle --attest --signing-key awskms://...` succeeds |
| `bundle-has-attestation-file` | `bundle-attestation.sigstore.json` exists; **no Fulcio cert** (public-key path, not keyless) |
| `verify-with-kms-key` | `aicr verify --key awskms://...` passes; `checksumsPassed` + `bundleAttested` both true |
| `verify-with-pem-public-key` | Same bundle verifies against the DER-exported PEM (confirms PEM path in verifier) |
| `verify-min-trust-attested` | Bundle meets `--min-trust-level attested` |
| `verify-min-trust-verified` | Bundle + attested binary meets `--min-trust-level verified` (skipped if `AICR_ATTESTED != true`) |
| `verify-tamper-detection` | Mutating `deploy.sh` causes `checksumsPassed: false` |
| `verify-wrong-key-rejected` | A second KMS key cannot verify a bundle signed by the first |

The test is gated by the label `requires: ministack` and is skipped in normal
`chainsaw test --no-cluster` runs. Activate with `--selector 'requires=ministack'`.

## KMS URI format

```text
awskms://<host:port>/<arn>
```

For MiniStack: `awskms://localhost:4566/arn:aws:kms:us-east-1:000000000000:key/<id>`

The sigstore AWS KMS client interprets the authority (`host:port`) as a
custom endpoint (and prepends `https://`), replacing the default AWS regional
endpoint. The `region` is read from `AWS_DEFAULT_REGION`; no `?region=` query
param is needed.

## Prerequisites

| Tool | Install |
|------|---------|
| Docker | https://docs.docker.com/get-docker/ |
| `aws` CLI | `pip install "awscli==<version from .settings.yaml>"` |
| `mkcert` | `make tools-setup` (then `mkcert -install` once, needs sudo) |
| `chainsaw` | `make tools-setup` |
| `goreleaser` | `make tools-setup` |
| `openssl` | System package (usually pre-installed) |

## Running locally

```bash
./tests/chainsaw/signing/bundle-attestation-kms-ministack/run.sh
```

The script issues a `localhost` cert with mkcert (running `mkcert -install`,
which may prompt for sudo the first time), starts MiniStack with `USE_SSL=1`,
provisions an ECDSA P-256 key, builds the `aicr` binary, runs the chainsaw
suite (or smoke checks; see the note below), and removes the container on exit
(success or failure). Override the image or port with `MINISTACK_IMAGE` /
`MINISTACK_PORT`; pass `DEBUG=true` for verbose output.

> **The `--attest` steps need an attested binary.** `aicr bundle --attest`
> refuses to run unless the `aicr` binary carries a cryptographic attestation;
> a plain `goreleaser --snapshot` build has none. When no attested binary is
> available, `run.sh` enters **smoke mode**: it validates the MiniStack/KMS
> plumbing (container, key provisioning, PEM export, recipe + bundle) and exits
> `0` with a clear message, rather than running the `--attest` chainsaw suite
> and failing. Two ways to get a full run:
>
> - **CI (self-contained):** push the branch and trigger the
>   `kms-ministack-e2e.yaml` workflow (`workflow_dispatch`). It builds and
>   attests the binary with its own workflow identity, then sets
>   `AICR_IDENTITY_REGEXP` to match.
> - **Local:** produce an attested binary first (run `build-attested.yaml` via
>   `gh workflow run` and download the `aicr-attested-binaries` artifact), then
>   run with `AICR_BIN=<binary>` and `AICR_IDENTITY_REGEXP` set to the attesting
>   workflow identity (`...workflows/build-attested\.yaml@.*`). With both set,
>   `run.sh` runs the full sign/verify suite.

Override any step with environment variables. For example, point the suite at a
pre-running MiniStack and a pre-provisioned key:

```bash
MINISTACK_ENDPOINT=https://localhost:4566 \
AWS_CA_BUNDLE="$(mkcert -CAROOT)/rootCA.pem" \
KMS_KEY_ARN=arn:aws:kms:us-east-1:000000000000:key/<id> \
AICR_BIN=/path/to/aicr \
  chainsaw test \
    --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/signing/bundle-attestation-kms-ministack/ \
    --selector 'requires=ministack'
```

## CI

Workflow: `.github/workflows/kms-ministack-e2e.yaml`

Triggers (current behavior): push to `main`, `pull_request` against `main` (same-repo
only; fork PRs are skipped because they lack the OIDC needed to attest the binary),
and `workflow_dispatch`. MiniStack runs as a plain `docker run` step (not a
`services:` container, which would start before the `load-versions` step that
resolves the pinned image). The workflow:

- Installs the pinned `mkcert`, runs `mkcert -install`, issues a `localhost`
  cert, and exports `AWS_CA_BUNDLE`, then starts MiniStack with `USE_SSL=1`.
- Generates an SLSA predicate and builds an attested binary (public Sigstore),
  so `verify-min-trust-verified` can exercise the full `verified` trust level.
- Sets `AICR_ATTESTED=true` when the attestation file is present; the chainsaw
  step self-skips it when absent (e.g. forked PRs without OIDC).

## Environment variables

| Variable | Set by | Purpose |
|----------|--------|---------|
| `MINISTACK_ENDPOINT` | CI / `run.sh` | MiniStack base URL (`https://localhost:4566`) |
| `MINISTACK_CERT_DIR` | CI / `run.sh` | Dir holding the mkcert `cert.pem`/`key.pem` (mounted into the container) |
| `AWS_CA_BUNDLE` | CI / `run.sh` | mkcert root CA, so the `aws` CLI trusts MiniStack's TLS |
| `KMS_KEY_ARN` | CI / `run.sh` | ARN of the pre-provisioned signing key |
| `AWS_ACCESS_KEY_ID` | CI / `run.sh` | Any value; MiniStack does not validate |
| `AWS_SECRET_ACCESS_KEY` | CI / `run.sh` | Any value; MiniStack does not validate |
| `AWS_DEFAULT_REGION` | CI / `run.sh` | Region (`us-east-1`) |
| `AICR_BIN` | CI / `run.sh` | Path to the built `aicr` binary |
| `AICR_ATTESTED` | CI detect step | `true` when binary attestation is present |
| `AICR_IDENTITY_REGEXP` | CI detect step | Workflow identity regexp for binary-attestation verification |

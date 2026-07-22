# Vault (OpenBAO) KMS E2E Tests

End-to-end (E2E) test suite for HashiCorp Vault-backed bundle signing and
verification (`aicr bundle --attest --signing-key hashivault://...`), exercised
against [OpenBAO](https://openbao.org) so the full provider-resolution → sign →
verify round-trip runs in CI against a real Transit secrets engine.

## Why this exists

Issue #1577 added `hashivault://` as a supported KMS scheme for
`aicr bundle --signing-key` and `aicr verify --key`. Unit tests cover URI
classification and confirm the provider registers, but they do not sign or
verify through a real Vault server. This suite closes that gap by driving the
`kms.Get("hashivault://...")` → `SignerVerifier.PublicKey` → `SignMessage` path
against a live (dev-mode) Transit engine.

## What is OpenBAO

[OpenBAO](https://openbao.org) is the Linux Foundation Apache-2.0 fork of
HashiCorp Vault, shipped as a single Docker image (`openbao/openbao`). Its
Transit secrets engine is API-identical to Vault's, so the sigstore
`hashivault://` signer/verifier drives it unchanged — the same code path serves
a production HashiCorp Vault. We use OpenBAO for the test container because its
Apache-2.0 license is consistent with this project's dependency policy.

Dev mode (`server -dev`) starts a single in-memory node that auto-initializes
and unseals, serves plain **HTTP** on port `8200`, and sets a known root token.
No TLS setup is needed (unlike the awskms MiniStack suite, whose signer
hardcodes `https://`). The image is pinned in `.settings.yaml`
(`testing_tools.openbao_image`) and resolved via the `load-versions` action,
never floated to `:latest`.

## What is tested

| Step | What it verifies |
|------|-----------------|
| `bundle-with-kms-signing` | `aicr bundle --attest --signing-key hashivault://...` succeeds |
| `bundle-has-attestation-file` | `bundle-attestation.sigstore.json` exists; **no Fulcio cert** (public-key path, not keyless) |
| `verify-with-kms-key` | `aicr verify --key hashivault://...` passes; `checksumsPassed` + `bundleAttested` both true |
| `verify-with-pem-public-key` | Same bundle verifies against the Transit-exported PEM (confirms PEM path in verifier) |
| `verify-min-trust-attested` | Bundle meets `--min-trust-level attested` |
| `verify-min-trust-verified` | Bundle + attested binary meets `--min-trust-level verified` (skipped if `AICR_ATTESTED != true`) |
| `verify-tamper-detection` | Mutating `deploy.sh` causes `checksumsPassed: false` |
| `verify-wrong-key-rejected` | A second Transit key cannot verify a bundle signed by the first |

The test is gated by the label `requires: openbao` and is skipped in normal
`chainsaw test --no-cluster` runs. Activate with `--selector 'requires=openbao'`.

## KMS URI format

```text
hashivault://<transit-key-name>
```

For this suite: `hashivault://aicr`. The sigstore hashivault provider reads the
server address and token from the `VAULT_ADDR` / `VAULT_TOKEN` environment
variables (`BAO_ADDR` / `BAO_TOKEN` are honored too); the URI carries only the
Transit key name. The Transit mount path defaults to `transit/` (override with
`TRANSIT_SECRET_ENGINE_PATH`). The signing key is `ecdsa-p256`, which signs over
the SHA-256 digest the bundler uses.

## Prerequisites

| Tool | Install | When |
|------|---------|------|
| Docker | https://docs.docker.com/get-docker/ | always |
| `curl` | System package (usually pre-installed) | always |
| `python3` | System package (extracts the PEM from the Transit read response) | always |
| `yq` | `make tools-setup` | always (reads the pinned OpenBAO image from `.settings.yaml`) |
| `goreleaser` | `make tools-setup` | only when building the binary (skipped if `AICR_BIN` is provided) |
| `chainsaw` | `make tools-setup` | only for the full `--attest` suite (skipped in smoke mode) |

No AWS CLI, TLS certificate, or `mkcert` is required (contrast the MiniStack
suite): dev-mode OpenBAO serves HTTP and is provisioned entirely over its HTTP
API with `curl`.

## Running locally

```bash
./tests/chainsaw/signing/bundle-attestation-vault/run.sh
```

The script starts OpenBAO in dev mode, enables Transit, provisions an
`ecdsa-p256` key, builds the `aicr` binary, runs the chainsaw suite (or smoke
checks; see the note below), and removes the container on exit (success or
failure). Override the image, port, token, or key name with `OPENBAO_IMAGE` /
`OPENBAO_PORT` / `VAULT_TOKEN` / `VAULT_KMS_KEY`; pass `DEBUG=true` for verbose
output.

> **The `--attest` steps need an attested binary.** `aicr bundle --attest`
> refuses to run unless the `aicr` binary carries a cryptographic attestation;
> a plain `goreleaser --snapshot` build has none. When no attested binary is
> available, `run.sh` enters **smoke mode**: it validates the Vault/Transit
> plumbing (container, engine, key provisioning, PEM export, recipe + bundle)
> and exits `0` with a clear message, rather than running the `--attest`
> chainsaw suite and failing. Two ways to get a full run:
>
> - **CI (self-contained):** push the branch and trigger the
>   `vault-kms-e2e.yaml` workflow (`workflow_dispatch`). It builds and attests
>   the binary with its own workflow identity, then sets `AICR_IDENTITY_REGEXP`
>   to match.
> - **Local:** produce an attested binary first (run `build-attested.yaml` via
>   `gh workflow run` and download the `aicr-attested-binaries` artifact), then
>   run with `AICR_BIN=<binary>` and `AICR_IDENTITY_REGEXP` set to the attesting
>   workflow identity (`...workflows/build-attested\.yaml@.*`). With both set,
>   `run.sh` runs the full sign/verify suite.

Override any step with environment variables. For example, point the suite at a
pre-running Vault/OpenBAO and a pre-provisioned key:

```bash
VAULT_ADDR=http://127.0.0.1:8200 \
VAULT_TOKEN=root \
VAULT_KMS_KEY=aicr \
AICR_BIN=/path/to/aicr \
  chainsaw test \
    --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/signing/bundle-attestation-vault/ \
    --selector 'requires=openbao'
```

## CI

Workflow: `.github/workflows/vault-kms-e2e.yaml`

Triggers: push to `main`, `pull_request` against `main` (same-repo only; fork
PRs are skipped because they lack the OIDC needed to attest the binary), and
`workflow_dispatch`. OpenBAO runs as a plain `docker run` step (not a
`services:` container, which would start before the `load-versions` step that
resolves the pinned image). The workflow:

- Starts OpenBAO in dev mode over HTTP, enables Transit, and provisions an
  `ecdsa-p256` signing key via the HTTP API.
- Generates an SLSA predicate and builds an attested binary (public Sigstore),
  so `verify-min-trust-verified` can exercise the full `verified` trust level.
- Sets `AICR_ATTESTED=true` when the attestation file is present; the chainsaw
  step self-skips it when absent (e.g. forked PRs without OIDC).

## Environment variables

| Variable | Set by | Purpose |
|----------|--------|---------|
| `VAULT_ADDR` | CI / `run.sh` | Vault/OpenBAO base URL (`http://127.0.0.1:8200`) |
| `VAULT_TOKEN` | CI / `run.sh` | Auth token (dev-mode root token) |
| `VAULT_KMS_KEY` | CI / `run.sh` | Transit key name (`aicr`); the URI is `hashivault://<key>` |
| `AICR_BIN` | CI / `run.sh` | Path to the built `aicr` binary |
| `AICR_ATTESTED` | CI detect step | `true` when binary attestation is present |
| `AICR_IDENTITY_REGEXP` | CI detect step | Workflow identity regexp for binary-attestation verification |

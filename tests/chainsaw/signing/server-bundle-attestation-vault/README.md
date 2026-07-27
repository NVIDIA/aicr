# Server (aicrd) Vault (OpenBAO) KMS bundle-attestation E2E

End-to-end (E2E) test suite for **server-side** bundle signing over HTTP: the
aicrd `POST /v1/bundle?attest=true` surface (#1150) driven against
[OpenBAO](https://openbao.org) with a HashiCorp Vault Transit signing key. It is
the server companion of
[`bundle-attestation-vault`](../bundle-attestation-vault/), which exercises the
same `hashivault://` path through the `aicr` CLI.

## Why this exists

Issue [#1150](https://github.com/NVIDIA/aicr/issues/1150) added server-side
attestation to `POST /v1/bundle`: with a signing identity configured entirely
from the server's own environment (a KMS key or a private-Sigstore keyless
identity), a request opts into signing with `?attest=true`, and aicrd signs the
bundle **as itself** — no human at a browser, no request-supplied identity
material. Unit tests
(`pkg/server/bundle_handler_test.go`, `pkg/server/signing_test.go`) cover the
mode selection, the `attest=true` wiring, and the "not configured" rejection,
but they inject a fake attester and never sign or verify through a real Vault
server over HTTP. This suite closes that gap: it starts the real aicrd server
with `AICR_SIGNING_KEY=hashivault://aicr` and drives the
sign (`POST /v1/bundle?attest=true`) → verify (`aicr verify --key`) round-trip
against a live (dev-mode) Transit engine.

## Structure: `run.sh` only (no `chainsaw-test.yaml`)

Unlike the CLI suite (which uses chainsaw to invoke `aicr bundle`/`aicr verify`
as discrete steps), this suite is a single `run.sh`. The subject under test is a
**long-running background process**, and its lifecycle — start aicrd, poll
`/health`, fail fast (dumping logs) if it dies at startup, `curl` the endpoint,
`aicr verify` the result, then kill the server — does not map cleanly onto
chainsaw's per-step script model, which has no notion of a process that must
outlive one step and be torn down after another. Keeping process ownership, the
startup fail-fast log dump, and the cleanup trap (which kills the server **and**
removes the OpenBAO container) in one script is clearer and is the single
artifact both local runs and CI invoke. The design note in the task brief
explicitly allows this: "a well-structured `run.sh` that does
start-server/curl/verify is acceptable and likely cleaner."

## What is OpenBAO

[OpenBAO](https://openbao.org) is the Linux Foundation Apache-2.0 fork of
HashiCorp Vault, shipped as a single Docker image (`openbao/openbao`). Its
Transit secrets engine is API-identical to Vault's, so the sigstore
`hashivault://` signer/verifier drives it unchanged — the same code path serves
a production HashiCorp Vault. We use OpenBAO for the test container because its
Apache-2.0 license is consistent with this project's dependency policy.

Dev mode (`server -dev`) starts a single in-memory node that auto-initializes
and unseals, serves plain **HTTP** on port `8200`, and sets a known root token.
No TLS setup is needed. The image is pinned in `.settings.yaml`
(`testing_tools.openbao_image`) and resolved via the `load-versions` action,
never floated to `:latest`.

## What is tested

### Full mode (attested aicrd binary present)

| Check | What it verifies |
|-------|-----------------|
| server startup | aicrd starts with `AICR_SIGNING_KEY=hashivault://aicr`, verifying its own embedded binary attestation, and reports `/health` ready |
| `POST /v1/bundle?attest=true` | Server signs the bundle as itself and returns a zip (HTTP 200) |
| bundle contents | `attestation/bundle-attestation.sigstore.json` **and** the embedded `attestation/aicr-attestation.sigstore.json` (tool provenance) are present |
| `aicr verify --key hashivault://aicr --insecure-ignore-tlog` | `checksumsPassed: true` **and** `bundleAttested: true` |

`--insecure-ignore-tlog` is used because the server signs with
`AICR_TLOG_UPLOAD=false` (KMS / Mode A, no Rekor upload), so verification is the
offline/air-gapped key-based path.

### Smoke mode (no attested aicrd binary — e.g. a local snapshot)

The server-side `--attest` path requires the server to verify its **own** binary
attestation at startup, which a plain `goreleaser --snapshot` build does not
carry. So without an attested binary the suite validates the plumbing and exits
`0`:

| Check | What it verifies |
|-------|-----------------|
| OpenBAO + Transit | container up, ECDSA P-256 key `aicr` provisioned, PEM exported |
| server startup | aicrd starts with signing **disabled** and reports `/health` ready |
| `POST /v1/bundle?attest=true` | rejected with **HTTP 400** "not configured for attestation" |
| `POST /v1/bundle` (unsigned) | returns a valid zip (HTTP 200) carrying `checksums.txt` |

## KMS URI format

```text
hashivault://<transit-key-name>
```

For this suite: `hashivault://aicr`. The sigstore hashivault provider (inside
aicrd) reads the server address and token from the `VAULT_ADDR` / `VAULT_TOKEN`
environment variables (`BAO_ADDR` / `BAO_TOKEN` are honored too); the URI carries
only the Transit key name. The signing key is `ecdsa-p256`, which signs over the
SHA-256 digest the bundler uses.

> **`VAULT_NAMESPACE` gotcha.** A stray `VAULT_NAMESPACE` (inherited from a shell
> or CI secret) makes the hashivault client target an Enterprise namespace path
> that dev-mode OpenBAO does not serve, breaking every Transit call. `run.sh`
> `unset`s it unconditionally, and the CI workflow does not set it.

## Prerequisites

| Tool | Install | When |
|------|---------|------|
| Docker | https://docs.docker.com/get-docker/ | always (runs OpenBAO) |
| `curl` | System package (usually pre-installed) | always (Vault HTTP API + aicrd) |
| `python3` | System package | always (Transit PEM, verify-JSON parse, unzip, free-port pick) |
| `yq` | `make tools-setup` | always (reads the pinned OpenBAO image from `.settings.yaml`) |
| `goreleaser` | `make tools-setup` | only when building binaries (skipped if `AICRD_BIN` + `AICR_BIN` are provided) |

## Running locally

```bash
./tests/chainsaw/signing/server-bundle-attestation-vault/run.sh
```

The script starts OpenBAO in dev mode, enables Transit, provisions an
`ecdsa-p256` key, builds (or locates) the `aicrd` server and `aicr` CLI binaries,
starts aicrd in the background, runs the full sign/verify round-trip (or the
smoke checks; see below), and on exit kills the server and removes the container.
Override the image, port, token, or key with `OPENBAO_IMAGE` / `OPENBAO_PORT` /
`VAULT_TOKEN` / `VAULT_KMS_KEY`, and the server port with `PORT`; pass
`DEBUG=true` for verbose server logs.

`aicrd` is a **linux-only** goreleaser target, so a local `goreleaser --snapshot`
build produces it only on a Linux host. On macOS, build the two binaries with
`go build` and point the runner at them:

```bash
go build -o /tmp/aicrd ./cmd/aicrd
go build -o /tmp/aicr  ./cmd/aicr
AICRD_BIN=/tmp/aicrd AICR_BIN=/tmp/aicr \
  ./tests/chainsaw/signing/server-bundle-attestation-vault/run.sh   # smoke mode
```

> **The `--attest` path needs an attested aicrd binary.** The server verifies its
> own binary attestation at startup (embedded into every attested bundle as tool
> provenance), and a plain snapshot build carries none. When no attested binary
> is available, `run.sh` enters **smoke mode** and exits `0` after validating the
> plumbing. For a full run, run the `server-kms-e2e.yaml` workflow (which attests
> the aicrd binary with its own workflow identity), or point `AICRD_BIN` at a
> CI-attested binary with its sibling `aicrd-attestation.sigstore.json`.

## CI

Workflow: `.github/workflows/server-kms-e2e.yaml`

Triggers: push to `main`, `pull_request` against `main` (same-repo only; fork PRs
are skipped because they lack the OIDC needed to attest the binary), and
`workflow_dispatch`. The workflow builds the **attested** `aicr` + `aicrd`
binaries (goreleaser + the cosign `attest-blob` hook, fed by
`generate-slsa-predicate` which sets both `SLSA_PREDICATE` and
`AICR_SIGNING_CONFIG`), exports `AICRD_BIN`/`AICR_BIN`, then runs the suite's
`run.sh`. `run.sh` owns the OpenBAO container and the aicrd server lifecycle, so
the same script runs locally and in CI with no inlined copy to drift.

## Environment variables

| Variable | Set by | Purpose |
|----------|--------|---------|
| `VAULT_ADDR` | CI / `run.sh` | Vault/OpenBAO base URL (`http://127.0.0.1:8200`) |
| `VAULT_TOKEN` | CI / `run.sh` | Auth token (dev-mode root token) |
| `VAULT_KMS_KEY` | CI / `run.sh` | Transit key name (`aicr`); the URI is `hashivault://<key>` |
| `AICRD_BIN` | CI / `run.sh` | Path to the built `aicrd` server binary |
| `AICR_BIN` | CI / `run.sh` | Path to the built `aicr` CLI binary (recipe generation + verify) |
| `PORT` | `run.sh` | aicrd listen port (a free ephemeral port when unset) |
| `AICR_SIGNING_KEY` | `run.sh` (full) | `hashivault://aicr`; selects server KMS signing |
| `AICR_TLOG_UPLOAD` | `run.sh` (full) | `false`; KMS signing with no Rekor upload (verify uses `--insecure-ignore-tlog`) |
| `AICR_BINARY_ATTESTATION_FILE` | `run.sh` (full) | Path to the sibling `aicrd-attestation.sigstore.json` the server embeds |

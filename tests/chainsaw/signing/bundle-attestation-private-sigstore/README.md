# Private Sigstore E2E Tests

End-to-end test suite for private/self-hosted Sigstore signing
(`aicr bundle --attest --fulcio-url ... --rekor-url ...`), exercised against a
local Sigstore stack deployed from the
[sigstore Helm charts](https://github.com/sigstore/helm-charts) (the `scaffold`
umbrella chart: Fulcio + Rekor + CT log + Trillian) so the keyless sign → log
round-trip runs with no public-good Sigstore and no external network.

## Why this exists

PR #408 added `--fulcio-url` and `--rekor-url` to `aicr bundle --attest` for
enterprises running a private Sigstore instance. Its unit tests stub at the
`KeylessIdentity` seam; there is no automated test that actually issues a Fulcio
certificate and writes a Rekor inclusion proof against a real (non-public)
instance. This suite closes that gap.

## Why Helm (not sigstore/scaffolding's setup scripts)

`sigstore/scaffolding`'s `setup-kind.sh` deploys the components as Knative
Services exposed through MetalLB + Kourier + sslip.io. Those LoadBalancer IPs
live inside the Kind Docker network, which is **not reachable from a macOS host**
(Docker Desktop / Colima run Kind inside a VM). The sigstore `scaffold` Helm
chart instead deploys plain `Deployment` + `ClusterIP` `Service` objects, so the
stack is reachable identically on macOS and Linux through `kubectl port-forward`
to localhost. No MetalLB, no Knative, no sslip.io, no `/etc/hosts`, no Colima
`--network-address`, no `sudo` (beyond mkcert's one-time CA install).

Two supporting pieces make the keyless flow work; both live next to this README:

- `scaffold-values.yaml` — Helm overrides: trust the in-cluster Kubernetes
  ServiceAccount (SA) OpenID Connect (OIDC) issuer, and replace Trillian's amd64-only, EOL MySQL 5.7
  image with the multi-arch official `mysql` image (so the stack runs
  arm64-native on Apple Silicon instead of being OOM-killed under emulation).
- `oidc-discovery-rbac.yaml` — grants anonymous read access to the cluster's
  OIDC discovery endpoints, which Fulcio fetches when building the verifier for
  the SA issuer.

`run.sh` additionally sets `SSL_CERT_FILE` on the Fulcio Deployment to the
mounted in-cluster CA so Fulcio trusts the apiserver serving cert during OIDC
discovery, which the chart does not expose as a value.

## HTTPS and OIDC

`aicr bundle` requires absolute `https://` signing endpoints, but
`kubectl port-forward` is plain HTTP. A tiny stdlib reverse proxy (`tlsproxy/`)
terminates TLS on localhost using an [mkcert](https://github.com/FiloSottile/mkcert)
certificate the host trust store accepts, and forwards to the port-forward.

The OIDC identity token is minted with `kubectl create token default --audience
sigstore`; Fulcio is configured to trust that in-cluster ServiceAccount issuer.
No Dex, no browser flow, no GitHub OIDC for the bundle signing.

## What is tested

| Step | What it verifies |
|------|-----------------|
| `create-attested-bundle-private-sigstore` | `aicr bundle --attest --fulcio-url $FULCIO_URL --rekor-url $REKOR_URL` succeeds |
| `attested-bundle-has-files` | `bundle-attestation.sigstore.json`, `checksums.txt`, and `recipe.yaml` all present |
| `attestation-bundle-has-fulcio-certificate` | Sigstore bundle contains a Fulcio certificate (keyless path, not public-key path) |
| `attestation-bundle-has-tlog-entry` | Bundle contains a Rekor transparency log entry (`tlogEntries`) |
| `verify-bundle-checksums` | `aicr verify` (no `--trust-root`) reports `"checksumsPassed": true` |
| `assemble-private-trust-root` | Builds a `trusted_root.json` for the local stack from the Fulcio chain, Rekor key, and CTLog key (`cosign trusted-root create`) |
| `verify-private-trust-root` | `aicr verify --trust-root` reports `bundleAttested: true` (bundle attestation validated against the private Fulcio/Rekor) |
| `verify-public-root-rejects-private-bundle` | Negative control: WITHOUT `--trust-root`, `bundleAttested` is `false` |

The test is gated by the label `requires: private-sigstore` and is skipped in
normal `chainsaw test --no-cluster` runs. Activate with
`--selector 'requires=private-sigstore'`.

## Verify trust level: `--trust-root`

`aicr verify --trust-root` (issue #1153) verifies the bundle attestation against
a self-hosted Sigstore by taking a `trusted_root.json` that is **additive** to
AICR's built-in public-good root. This suite exercises it end-to-end:

- `assemble-private-trust-root` builds the `trusted_root.json` for the local
  stack with `cosign trusted-root create`, combining the Fulcio CA chain
  (`/api/v1/rootCert`), the Rekor public key (`/api/v1/log/publicKey`), and the
  CTLog (CTFE) public key. The CTLog key is mandatory: aicr does not disable
  Signed Certificate Timestamp checks, so sigstore-go rejects the private Fulcio
  certificate without it. It is the one trust input not served over HTTP, so
  `run.sh` extracts it from the in-cluster `ctlog-public-key` secret (namespace
  `ctlog-system`, data key `public`; see `secrets.ctlog` in the scaffold chart)
  and passes the path to chainsaw as `AICR_CTLOG_PUBKEY`. Fulcio and Rekor are
  fetched by the chainsaw step itself over the plain-HTTP localhost
  port-forwards (`FULCIO_PF_PORT` / `REKOR_PF_PORT`).
- `verify-private-trust-root` asserts `bundleAttested: true` and a trust level
  of at least `attested`. The bundle-attestation step uses a permissive identity
  matcher, so `--trust-root` alone validates the private Fulcio cert chain and
  the private Rekor inclusion proof. The binary-attestation half still verifies
  against public-good Sigstore (pinned by `AICR_IDENTITY_REGEXP`); the step
  additionally asserts `binaryAttested: true` to confirm the full chain succeeds,
  without making the exact top-level trust level the gating condition.
- `verify-public-root-rejects-private-bundle` is the negative control: with the
  same bundle and binary but no `--trust-root`, `bundleAttested` is `false`,
  proving the flag is what admits the private chain.

The `verify-bundle-checksums` step is retained to keep the checksum assertion
(independent of attestation trust) covered.

## Prerequisites

| Tool | Install |
|------|---------|
| `kind` | `make tools-setup` |
| `kubectl` | `make tools-setup` |
| `helm` | `make tools-setup` |
| `chainsaw` | `make tools-setup` |
| `cosign` | `brew install cosign` (macOS) / package manager (Linux) |
| `mkcert` | `brew install mkcert` (macOS) / package manager (Linux) |
| `go`, `yq` | `make tools-setup` |
| `docker` | Docker Desktop / Colima |

## Running locally

```bash
AICR_BIN=/path/to/attested/aicr \
  ./tests/chainsaw/signing/bundle-attestation-private-sigstore/run.sh
```

`run.sh` creates a Kind cluster, `helm install`s the `scaffold` chart, configures
Fulcio's OIDC trust, port-forwards Fulcio/Rekor, fronts them with the localhost
TLS proxy, extracts the CTLog public key from the in-cluster secret, mints an SA
OIDC token, runs the chainsaw suite, and tears everything down on exit. Pass
`KEEP_CLUSTER=true` to leave the cluster running between runs.

`aicr bundle --attest` requires an NVIDIA-CI-attested binary (a co-located
`<binary>-attestation.sigstore.json`, verified against public Sigstore). Provide
one via `AICR_BIN` (e.g. the `build-attested.yaml` artifact); `AICR_IDENTITY_REGEXP`
defaults to the `build-attested.yaml` identity. A plain `goreleaser --snapshot`
build is not attested and stops at the bundle step.

To run chainsaw directly against an already-provisioned stack:

```bash
FULCIO_URL=https://127.0.0.1:8443 \
REKOR_URL=https://127.0.0.1:8444 \
OIDC_TOKEN=<token> \
AICR_BIN=/path/to/aicr \
AICR_IDENTITY_REGEXP='https://github.com/NVIDIA/aicr/\.github/workflows/build-attested\.yaml@.*' \
AICR_CTLOG_PUBKEY=/path/to/ctfe.pub \
FULCIO_PF_PORT=8080 \
REKOR_PF_PORT=8081 \
  chainsaw test \
    --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/signing/bundle-attestation-private-sigstore/ \
    --selector 'requires=private-sigstore'
```

The trust-root steps additionally need `AICR_CTLOG_PUBKEY` (CTLog public key
PEM, e.g. `kubectl -n ctlog-system get secret ctlog-public-key -o
jsonpath='{.data.public}' | base64 -d`) and the plain-HTTP Fulcio/Rekor
port-forward ports (`FULCIO_PF_PORT` / `REKOR_PF_PORT`). `run.sh` sets all of
these for you.

## CI

Workflow: `.github/workflows/sigstore-scaffolding-e2e.yaml`

It currently runs on `push` to `main`, on `pull_request` targeting `main` (for
matching path changes), and on `workflow_dispatch`. Fork PRs are skipped (a
read-only token cannot mint the OIDC needed to attest the binary). The job
builds and attests the binary, then invokes `run.sh` — the same harness used
locally — so CI and local runs are identical.

## Environment variables

| Variable | Set by | Purpose |
|----------|--------|---------|
| `FULCIO_URL` | `run.sh` / workflow | https URL of the local Fulcio CA (via the TLS proxy) |
| `REKOR_URL` | `run.sh` / workflow | https URL of the local Rekor transparency log (via the TLS proxy) |
| `OIDC_TOKEN` | `run.sh` / workflow | SA identity token Fulcio accepts (`kubectl create token`) |
| `AICR_BIN` | caller / workflow | Path to the built (attested) `aicr` binary |
| `AICR_IDENTITY_REGEXP` | caller / workflow | `--certificate-identity-regexp` for the binary attestation |
| `AICR_CTLOG_PUBKEY` | `run.sh` / workflow | Path to the CTLog (CTFE) public key PEM, extracted from the in-cluster `ctlog-public-key` secret |
| `FULCIO_PF_PORT` | `run.sh` / workflow | localhost plain-HTTP port-forward for Fulcio (trust-root assembly fetches the CA chain here) |
| `REKOR_PF_PORT` | `run.sh` / workflow | localhost plain-HTTP port-forward for Rekor (trust-root assembly fetches the public key here) |

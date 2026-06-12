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
| `verify-bundle-checksums` | `aicr verify` reports `"checksumsPassed": true` |

The test is gated by the label `requires: private-sigstore` and is skipped in
normal `chainsaw test --no-cluster` runs. Activate with
`--selector 'requires=private-sigstore'`.

## Known limitation: verify trust level

`aicr verify` does not yet accept `--rekor-url` or a custom trust root, so its
sigstore attestation check runs against the **public** trust root and cannot
validate a bundle recorded in the private Rekor (it fails log inclusion and
exits non-zero). The `verify-bundle-checksums` step therefore asserts only the
checksum result, which is independent of attestation trust. Full
transparency-log proof verification against the local Rekor is deferred to after
issue #1153 (private trust root) lands; see issue #1215 for Phase 2 scope.

## Prerequisites

| Tool | Install |
|------|---------|
| `kind` | `make tools-setup` |
| `kubectl` | `make tools-setup` |
| `helm` | `make tools-setup` |
| `chainsaw` | `make tools-setup` |
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
TLS proxy, mints an SA OIDC token, runs the chainsaw suite, and tears everything
down on exit. Pass `KEEP_CLUSTER=true` to leave the cluster running between runs.

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
  chainsaw test \
    --no-cluster \
    --config tests/chainsaw/chainsaw-config.yaml \
    --test-dir tests/chainsaw/signing/bundle-attestation-private-sigstore/ \
    --selector 'requires=private-sigstore'
```

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

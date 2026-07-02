# Private & Enterprise Signing and Verification

[`bundle-attestation.md`](bundle-attestation.md) signs against the **public-good** Sigstore (`fulcio.sigstore.dev` / `rekor.sigstore.dev`) using an interactive OIDC sign-in. Many enterprises cannot use that path: the hosts are air-gapped or behind a VPN that blocks Sigstore egress, there is no browser for OIDC, or policy requires signing keys to live in the organization's own KMS.

AICR supports three private modes, all on the same `aicr bundle --attest` / `aicr verify` surface:

1. **Self-hosted Sigstore** — keyless signing against *your own* Fulcio + Rekor (`--fulcio-url` / `--rekor-url`), verified with `aicr verify --trust-root`.
2. **KMS-backed signing** — keyed signing with no OIDC at all (`--signing-key awskms://…`), verified with `aicr verify --key`.
3. **Headless OIDC** — keyless signing without a browser, for CI and bastions (`--oidc-device-flow` or a pre-fetched `--identity-token`).

**Two ways to run this:**

- **Validate locally (no infrastructure of your own).** The runner stands up a throwaway Sigstore store and runs the full sign + `verify --trust-root` flow for you, end to end. Running it *is* the test. Start at [Bring up a private Sigstore store](#bring-up-a-private-sigstore-store).
- **Against your own private Sigstore (enterprise).** You already operate Fulcio / Rekor / CTLog; follow the numbered steps below with your endpoints.

You cannot demo or test private signing without a private store to sign against, so standing one up is the first thing the runnable demo does.

The numbered walkthrough (the enterprise / own-stack path) covers:

1. The attested-binary prerequisite (the one thing that stays public).
2. Sign against a self-hosted Sigstore.
3. Assemble a private trusted root.
4. Verify against the private root (`--trust-root`).
5. Negative control: without `--trust-root` it fails closed.
6. KMS-backed signing and `verify --key`.
7. Headless OIDC for CI.

## Prerequisites

* **An attested `aicr` binary.** `aicr bundle --attest` verifies the binary's *own* attestation against **public** Sigstore before it will sign anything, so even a fully private signing flow needs a release-archive binary (which ships the `aicr-attestation.sigstore.json` sidecar) plus a matching `--certificate-identity-regexp`. A local `go build` is not attested and stops at the bundle step. This is deliberate: it anchors "which CLI built this" to NVIDIA CI regardless of where the bundle itself is signed.
* **A private Sigstore store** (Fulcio + Rekor + CTLog + Trillian) for modes 1 and 3 — your own, or the throwaway one the runner stands up (see [Bring up a private Sigstore store](#bring-up-a-private-sigstore-store)).
* **`cosign`, `curl`, `yq`** to assemble and inspect the private trusted root.
* For mode 2: a reachable KMS key (`awskms://` | `gcpkms://` | `azurekms://`).

Throughout, `$FULCIO_URL`, `$REKOR_URL`, and `$CTFE_URL` are your stack's endpoints (Fulcio CA, Rekor log, and CTLog/CTFE respectively — some deployments co-locate CTFE with Fulcio, but treat it as a distinct service), and `$IDENTITY` is the `--certificate-identity-regexp` matching your binary's attestation (for an RC built by `on-tag.yaml`: `https://github.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*`).

## Bring up a private Sigstore store

You cannot demo or test private signing without a private Sigstore to sign against, so the repo ships a runner that stands one up for you: [`tests/chainsaw/signing/bundle-attestation-private-sigstore/run.sh`](../tests/chainsaw/signing/bundle-attestation-private-sigstore/run.sh). It creates a Kind cluster, deploys a complete throwaway store (Fulcio CA, Rekor transparency log, CTLog, Trillian via the upstream `sigstore/scaffold` Helm chart), fronts them with an mkcert TLS proxy so `aicr`'s `https://` signing endpoints resolve, mints an OIDC token from an in-cluster ServiceAccount, then runs the full sign + `verify --trust-root` flow and tears the cluster down. Standing this up by hand is involved, so `run.sh` is the maintained way to do it.

Running it end to end is exactly what validates the private-signing surface — for this use case **the demo is the test**.

Prerequisites: `kind`, `kubectl`, `helm`, `mkcert`, `chainsaw`, `cosign`, `go`, `yq`, `docker`, and `gh` (to download the release binary).

From the repo root, this is the whole thing — download the attested release binary, then run the store-up + sign + verify flow (substitute the release or RC tag for `v0.16.0`):

```shell
# 1. Download the attested release binary (it ships its aicr-attestation.sigstore.json sidecar)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
gh release download v0.16.0 -R NVIDIA/aicr -p "aicr_*_${OS}_${ARCH}.tar.gz"
tar xzf aicr_*_${OS}_${ARCH}.tar.gz          # -> ./aicr + aicr-attestation.sigstore.json
./aicr trust update                          # cache the public Sigstore root (needed by the binary-attestation gate)

# 2. Bring up the private store on Kind and run the full sign + verify flow
AICR_BIN="$PWD/aicr" \
KEEP_CLUSTER="true" \
AICR_IDENTITY_REGEXP='https://github.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*' \
  tests/chainsaw/signing/bundle-attestation-private-sigstore/run.sh
```

A green run ends with `✓ Private Sigstore E2E: PASSED`; the chainsaw `verify-private-trust-root` step asserts `bundleAttested: true` against the private root.

`run.sh` is driven entirely by environment variables:

- `AICR_BIN` (required) — path to an **attested** `aicr` binary; `--attest` refuses an unattested local build, which is why step 1 downloads the release archive. If unset, the runner builds an unattested snapshot and the sign step fails.
- `AICR_IDENTITY_REGEXP` — `--certificate-identity-regexp` for that binary's own attestation. The default targets the `build-attested` workflow, so override it for a release/RC binary (built by `on-tag`) exactly as above.
- `KEEP_CLUSTER=true` — leave the Kind cluster up after the run instead of deleting it (for inspection).
- `SIGSTORE_E2E_CLUSTER` — Kind cluster name (default `aicr-sigstore-e2e`); `SCAFFOLD_CHART_VERSION` / `KIND_NODE_IMAGE` pin the stack versions; `FULCIO_TLS_PORT` / `REKOR_TLS_PORT` / `FULCIO_PF_PORT` / `REKOR_PF_PORT` relocate host ports if they clash. See the script header for the full list.

To inspect the store after a `KEEP_CLUSTER=true` run, then clean up:

```shell
kubectl --context kind-aicr-sigstore-e2e -n trillian-system get pods   # slow trillian-mysql is the usual stall
kind delete cluster --name aicr-sigstore-e2e                            # clean up when done
```

KMS variant (keyed signing, no OIDC) against a local AWS emulator, reusing the same downloaded binary:

```shell
AICR_BIN="$PWD/aicr" \
AICR_IDENTITY_REGEXP='https://github.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*' \
  tests/chainsaw/signing/bundle-attestation-kms-ministack/run.sh
```

The numbered steps below break this same flow into its legs. Run them against your own enterprise Fulcio / Rekor (you already know those endpoints), or against the local throwaway store by bringing it up with `KEEP_CLUSTER=true` and exposing its endpoints yourself. The runner tears down its port-forwards and TLS proxy on exit, so for hand-driving you re-create what it does internally — `aicr` requires `https://` signing endpoints, so a TLS proxy fronts the plain-HTTP port-forwards:

```shell
KCTX=kind-aicr-sigstore-e2e

# Plain-HTTP port-forwards to Fulcio and Rekor
kubectl --context $KCTX -n fulcio-system port-forward svc/fulcio-server 8080:80 &
kubectl --context $KCTX -n rekor-system  port-forward svc/rekor-server  8081:80 &

# Front them with the committed TLS proxy so https:// endpoints resolve
go build -o /tmp/tlsproxy ./tests/chainsaw/signing/bundle-attestation-private-sigstore/tlsproxy/
mkcert -install
mkcert -cert-file /tmp/localhost.pem -key-file /tmp/localhost-key.pem localhost 127.0.0.1 ::1
/tmp/tlsproxy /tmp/localhost.pem /tmp/localhost-key.pem "8443=http://localhost:8080" "8444=http://localhost:8081" &

# The endpoints and inputs the numbered steps use
export FULCIO_URL=https://127.0.0.1:8443
export REKOR_URL=https://127.0.0.1:8444
export CTFE_URL=$FULCIO_URL                    # the scaffold stack co-locates CTFE with Fulcio
export IDENTITY='https://github.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*'
export OIDC_TOKEN=$(kubectl --context $KCTX create token default --audience sigstore --duration=20m)
```

## 1. The attested-binary gate

Run the numbered commands with the **attested release binary** you downloaded above, not a system `aicr` — older builds (before `--fulcio-url` landed) reject these flags with `flag provided but not defined`. Put it first on `$PATH`, then confirm the version and that it carries the attestation sidecar:

```shell
export PATH="$PWD:$PATH"          # the ./aicr you extracted (beside aicr-attestation.sigstore.json)
aicr version                      # expect 0.16.x, not an older system build
ls "$(command -v aicr)"-attestation.sigstore.json
# present -> --attest will run; absent -> use a release-archive binary
```

> The bundle-signing endpoints (`--fulcio-url`/`--rekor-url`) are fully private, but verifying *this* sidecar reaches public Sigstore roots once. On an air-gapped host, run `aicr trust update` from a connected machine first so the root is cached.

## 2. Sign against a self-hosted Sigstore

`--fulcio-url` / `--rekor-url` redirect keyless signing to your stack. Both must be absolute `https://` URLs with no embedded credentials. Pass the OIDC token your stack's Fulcio trusts (e.g. minted by your IdP, or a Kubernetes ServiceAccount token) via `COSIGN_IDENTITY_TOKEN` — prefer the env var over the `--identity-token` flag, which exposes the bearer token in `ps` / `/proc/<pid>/cmdline` on the shared hosts this mode targets (see step 7):

```shell
# a bundle is built from a recipe — generate one first (offline, no signing)
aicr recipe --service eks --accelerator gb200 --os ubuntu --intent training -o recipe.yaml

COSIGN_IDENTITY_TOKEN="$OIDC_TOKEN" \
aicr bundle --recipe recipe.yaml --output ./bundle --attest \
  --fulcio-url "$FULCIO_URL" \
  --rekor-url "$REKOR_URL"
```

The bundle attestation's Fulcio cert is now issued by *your* CA and logged to *your* Rekor. AICR's built-in public-good root knows nothing about it, so verifying it requires telling AICR about your stack (next two steps).

## 3. Assemble a private trusted root

`aicr verify --trust-root` takes a `trusted_root.json` describing your Fulcio CA, Rekor key, and CTLog (CTFE) key. Build it with `cosign trusted-root create`:

```shell
# Fetch the trust material from your stack
curl -fsS "$FULCIO_URL/api/v1/rootCert"     -o fulcio-chain.pem
curl -fsS "$REKOR_URL/api/v1/log/publicKey" -o rekor.pub
# CTLog (CTFE) public key — MANDATORY (aicr does not disable Signed Certificate
# Timestamp checks, so the private Fulcio cert fails verification without it).
# Local store: extract it from the cluster. Your own stack: get it from your CTLog.
kubectl --context kind-aicr-sigstore-e2e -n ctlog-system get secret ctlog-public-key \
  -o jsonpath='{.data.public}' | base64 -d > ctfe.pub

cosign trusted-root create \
  --no-default-fulcio --no-default-rekor --no-default-ctfe --no-default-tsa \
  --fulcio="url=$FULCIO_URL,certificate-chain=fulcio-chain.pem,start-time=1970-01-01T00:00:00Z" \
  --rekor="url=$REKOR_URL,public-key=rekor.pub,start-time=1970-01-01T00:00:00Z" \
  --ctfe="url=$CTFE_URL,public-key=ctfe.pub,start-time=1970-01-01T00:00:00Z" \
  --out trusted_root.json
```

`--no-default-*` drops the public-good services so the file describes only your stack. Sanity-check it carries all three:

```shell
yq -p json -e '.certificateAuthorities | length > 0' trusted_root.json >/dev/null && \
yq -p json -e '.tlogs | length > 0'                  trusted_root.json >/dev/null && \
yq -p json -e '.ctlogs | length > 0'                 trusted_root.json >/dev/null && \
echo "trusted root OK"
```

> `start-time=` is **required** on the rekor and ctfe tlog specs (cosign errors with `missing or empty required key 'start-time'` without it). It anchors the lower bound of the validity window; `1970-01-01T00:00:00Z` (the unix epoch) widens it so freshly issued local/test certs fall inside. Since `--trust-root` is additive to the public-good root, a production trusted root left at the epoch value accepts trust material regardless of issuance date — in production set the CA/log's real inception timestamp.

## 4. Verify against the private root

```shell
aicr verify ./bundle \
  --trust-root ./trusted_root.json \
  --certificate-identity-regexp "$IDENTITY" \
  && echo "✓ worked: verified against the private trust root" \
  || echo "✗ FAILED: expected the private chain to verify"
```

`--trust-root` is **additive**: it is unioned with AICR's built-in public-good root, so NVIDIA-signed bundles and your privately-signed bundles both verify with the same command. It composes with `--key` (step 6) and `--certificate-identity-regexp`. Expected result: the bundle attestation verifies against your private Fulcio chain and Rekor inclusion proof (`bundleAttested: true`), reaching at least `attested`, and `verified` once the binary attestation + identity pin also line up.

## 5. Negative control

Drop `--trust-root` and the same private bundle must fail to verify its attestation — proof that the private root is what does the work:

```shell
# This SHOULD fail — the private bundle does not verify against the public-good
# root — so a non-zero exit is the passing outcome here:
aicr verify ./bundle --certificate-identity-regexp "$IDENTITY" \
  && echo "✗ UNEXPECTED: verified without --trust-root" \
  || echo "✓ worked: fails closed without --trust-root (as designed)"
```

## 6. KMS-backed signing (no OIDC)

Same store, different signer. For CI/CD with no OIDC and a policy that keys live in your KMS, sign with `--signing-key` instead of keyless. It is mutually exclusive with `--identity-token`, `--oidc-device-flow`, and `--fulcio-url` (there is no Fulcio cert: the attestation carries the public key directly). Crucially, it **logs to the same private Rekor** you already stood up (`--rekor-url "$REKOR_URL"`) and verifies against the **same `trusted_root.json` from step 3** — no public Rekor, no second stack.

KMS needs a signing key. For local testing, MiniStack emulates AWS KMS; bring it up with a TLS cert the Go client trusts (reusing the mkcert CA), create a key, and derive its URI:

```shell
# dummy creds for the emulator (clear any real/expired AWS session first)
unset AWS_SESSION_TOKEN AWS_SECURITY_TOKEN AWS_PROFILE
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1

CERT=$HOME/.aicr-ministack-tls; mkdir -p "$CERT"
mkcert -install
mkcert -cert-file "$CERT/cert.pem" -key-file "$CERT/key.pem" localhost 127.0.0.1 ::1
# the Python aws CLI ignores the system trust store, so point it at the mkcert CA
# (else: SSL CERTIFICATE_VERIFY_FAILED / unable to get local issuer certificate)
export AWS_CA_BUNDLE="$(mkcert -CAROOT)/rootCA.pem"

docker run -d --rm --name aicr-kms -p 4566:4566 -e USE_SSL=1 \
  -e MINISTACK_SSL_CERT=/certs/cert.pem -e MINISTACK_SSL_KEY=/certs/key.pem \
  -v "$CERT:/certs:ro" ministackorg/ministack:1.3.61
until aws kms list-keys --endpoint-url https://localhost:4566 >/dev/null 2>&1; do sleep 2; done   # wait for KMS

ARN=$(aws kms create-key --endpoint-url https://localhost:4566 \
  --key-spec ECC_NIST_P256 --key-usage SIGN_VERIFY \
  --query 'KeyMetadata.Arn' --output text)
KMS_URI="awskms://localhost:4566/$ARN"   # endpoint-qualified; a real AWS alias is awskms:///alias/<name>
```

Sign with the KMS key, logging to the **local** Rekor, then verify with `--key` against the **step-3 trust root** (which already contains that Rekor):

```shell
aicr bundle --recipe recipe.yaml --output ./bundle-kms --attest \
  --signing-key "$KMS_URI" \
  --rekor-url "$REKOR_URL"

aicr verify ./bundle-kms --key "$KMS_URI" --trust-root ./trusted_root.json \
  && echo "✓ worked: KMS-signed, private-Rekor-logged bundle verified" \
  || echo "✗ FAILED"

docker rm -f aicr-kms          # tear down the KMS emulator when done
```

`--key` coexists with `--certificate-identity-regexp` (which still pins the separate binary attestation) and with `--trust-root`. It also accepts a local PEM public-key file (`--key ./signing-pub.pem`) instead of the KMS URI.

> A self-contained CI variant, [`bundle-attestation-kms-ministack/run.sh`](../tests/chainsaw/signing/bundle-attestation-kms-ministack/run.sh), stands up MiniStack and runs the whole thing — but it logs to the **public** Rekor, so it needs egress to `rekor.sigstore.dev` (it fails `connection reset by peer` on a Sigstore-blocking VPN). The steps above are the air-gapped, single-store path.

## 7. Headless OIDC for CI

When you want keyless signing but have no browser:

```shell
# OAuth 2.0 device authorization grant — prints a code to enter on another device
aicr bundle --recipe recipe.yaml --output ./bundle --attest --oidc-device-flow

# or a token your pipeline already fetched (prefer the env var on shared hosts:
# flag values show up in `ps` / /proc/<pid>/cmdline)
COSIGN_IDENTITY_TOKEN="$TOKEN" aicr bundle --recipe recipe.yaml --output ./bundle --attest
```

Both combine with `--fulcio-url`/`--rekor-url` to target a private store.

## Troubleshooting

**"SCT verification failed" / cert not trusted** — the CTLog (CTFE) public key is missing from `trusted_root.json`. AICR does not disable Signed Certificate Timestamp checks; add the `--ctfe` entry (step 3).

**`verify --trust-root` still fails after a CA rotation** — your stack reissued its Fulcio CA or Rekor key. Re-fetch the material and rebuild `trusted_root.json`.

**"binary attestation not found"** — the binary is not a release build. Use a release-archive `aicr` and set `--certificate-identity-regexp` to match (step 1).

**`--fulcio-url` rejected** — endpoints must be absolute `https://` with no embedded credentials. Front a plain-HTTP local stack with a TLS proxy (the Kind runner does this with `mkcert`).

**KMS sign/verify mismatch** — `--signing-key` and `verify --key` must reference the same key; a local PEM passed to `--key` must be the public half of the KMS key used to sign.

## Links

* [`bundle-attestation.md`](bundle-attestation.md) — the public-good signing path this extends
* [`tests/chainsaw/signing/bundle-attestation-private-sigstore/`](../tests/chainsaw/signing/bundle-attestation-private-sigstore) — automated private-Sigstore suite + `run.sh`
* [`tests/chainsaw/signing/bundle-attestation-kms-ministack/`](../tests/chainsaw/signing/bundle-attestation-kms-ministack) — automated KMS suite + `run.sh`
* [CLI reference: `aicr verify`](../docs/user/cli-reference.md#aicr-verify) and [`aicr bundle`](../docs/user/cli-reference.md#aicr-bundle) — full flag documentation
* [`docs/user/artifact-verification.md`](../docs/user/artifact-verification.md) — trust levels, KMS-key and private-Sigstore verification reference

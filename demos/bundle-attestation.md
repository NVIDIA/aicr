# Bundle Attestation Demo

Bundle attestation provides cryptographic proof of **who** created a deployment
bundle and **which AICR CLI** built it. When `aicr bundle --attest` runs, the
CLI signs the bundle's `checksums.txt` (which inventories the deployment
payload, including `recipe.yaml`; the attestation files are verified separately)
using [Sigstore](https://www.sigstore.dev/) and
generates SLSA Build Provenance v1 metadata. Anyone can later verify the
bundle with `aicr verify` to confirm:

* The generated payload files listed in `checksums.txt`, including `recipe.yaml`, haven't been tampered with.
* It was created by a trusted identity.
* It was built by an attested NVIDIA-CI-released AICR CLI.

The companion script is [`bundle-attestation-demo.sh`](bundle-attestation-demo.sh);
the slide deck is [`bundle-attestation-demo-slides.html`](bundle-attestation-demo-slides.html).

This walkthrough covers:

1. Bootstrap the Sigstore trusted root.
2. Generate a recipe.
3. Create an attested bundle.
4. Inspect the bundle layout.
5. Verify with default auto-detection.
6. Enforce minimum trust level / creator / CLI version policy.
7. JSON output for CI pipelines.
8. Tamper demo.

## Prerequisites

* `aicr` from a release archive (the release binary itself carries a binary
  attestation, which is what makes the `verified` trust level reachable).
* For signing: a working OIDC source.
  * **GitHub Actions:** ambient OIDC is detected automatically — nothing extra.
  * **Local:** a browser window opens for keyless OIDC sign-in.
* Sigstore egress: `fulcio.sigstore.dev` and `rekor.sigstore.dev` must be
  reachable from the signing host. Both are commonly blocked on corporate
  VPNs — if so, sign from a host with public internet egress.

## 1. Trust setup (optional)

`aicr verify` falls back to the embedded Sigstore trusted root, so verification
works offline out of the box. Run this only to refresh a stale cached root:

```shell
aicr trust update   # optional; idempotent
```

This pulls the current Sigstore TUF root used to verify Fulcio cert chains.
You only need it if `aicr verify` reports "trusted root may be stale" (the
embedded root otherwise suffices).

## 2. Generate a recipe

A bundle is built *from* a recipe, so the recipe is the producer's input:

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --output recipe.yaml
```

## 3. Create an attested bundle

**Default (no attestation):**

```shell
aicr bundle --recipe recipe.yaml --output ./my-bundle
```

Produces a bundle whose maximum reachable trust level is `unverified` —
`checksums.txt` is generated, but nothing is signed.

**With attestation (opens browser for OIDC sign-in):**

```shell
aicr bundle --recipe recipe.yaml --output ./my-bundle --attest
```

**In GitHub Actions** the OIDC token is detected automatically; no browser. To
make CI bundles `verified`, run them from a workflow with `id-token: write`
permissions.

## 4. Bundle layout

```text
my-bundle/
├── checksums.txt                          # SHA256 of every generated payload file
├── recipe.yaml                            # canonical post-resolution recipe
├── deploy.sh                              # automation script
├── README.md                              # deployment guide
├── 001-<component>/                       # per-component folder (NNN-prefixed)
│   ├── install.sh
│   ├── values.yaml
│   └── cluster-values.yaml
└── attestation/
    ├── bundle-attestation.sigstore.json   # SLSA Provenance v1 — signs checksums.txt
    └── aicr-attestation.sigstore.json     # binary SLSA attestation (copied in)
```

The two attestations together form the chain that makes `verified` reachable:

* **`bundle-attestation.sigstore.json`** — Sigstore Bundle (DSSE + Fulcio cert
  + Rekor inclusion proof). Its in-toto subject is the SHA256 of
  `checksums.txt`, so signing this one file transitively pins every generated
  payload file that `checksums.txt` lists, including `recipe.yaml`. The signer
  identity is the creator's OIDC identity.
* **`aicr-attestation.sigstore.json`** — the SLSA Build Provenance attestation
  *of the AICR CLI binary that produced the bundle*, copied in at bundle time.
  Its signer identity is NVIDIA CI (`https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@...`).

Inspect the predicate:

```shell
jq '.dsseEnvelope.payload | @base64d | fromjson
    | { subject, predicateType, predicate: .predicate }' \
   my-bundle/attestation/bundle-attestation.sigstore.json
```

## 5. Verify the bundle

**Auto-detect maximum trust level:**

```shell
aicr verify ./my-bundle
```

Expected output (release binary, signed bundle):

```text
✓ Checksums verified (12 files)
✓ Bundle attested by: jdoe@company.com
✓ Binary built by: https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0
✓ Identity pinned to NVIDIA CI
  Trust level: verified

Bundle verification: PASSED
```

Five gates run, top to bottom:

1. **Checksums** — every generated payload file listed in `checksums.txt`, including `recipe.yaml`, is hashed and compared.
2. **Bundle signature** — the Sigstore Bundle is verified against the trusted root.
3. **Bundle predicate** — the in-toto subject is checked against the actual `checksums.txt` digest.
4. **Binary attestation chain** — `aicr-attestation.sigstore.json` is verified and its subject is checked against the CLI binary digest claimed in the bundle predicate.
5. **Identity pin** — the binary attestation's signer is pinned to NVIDIA CI workflows.

Any gate failing short-circuits to a lower trust level (see table below).

## 6. Policy enforcement

### Require minimum trust level

```shell
aicr verify ./my-bundle --min-trust-level verified
aicr verify ./my-bundle --min-trust-level attested
```

| Level | Name | Criteria |
|-------|------|----------|
| **4** | `verified` | Checksums + bundle attestation + binary attestation pinned to NVIDIA CI |
| **3** | `attested` | Bundle attestation verified; binary attestation missing/unverified, or external data used. A *failed* binary attestation also reports attested but exits nonzero (#1550) |
| **2** | `unverified` | Checksums valid, `--attest` was not used |
| **1** | `unknown` | Missing/invalid `checksums.txt`, or bundle attestation fails verification |

Pick the floor per environment. Production bundles must be `verified`; an
emergency hotfix bundle built off-CI might only be required to reach
`attested`.

### Require a specific creator

```shell
aicr verify ./my-bundle --require-creator jdoe@company.com
```

Identity is the OIDC subject — typically a user email or a workflow ref.

### Require a CLI version range

```shell
aicr verify ./my-bundle --cli-version-constraint 1.0.0           # bare = ">= 1.0.0"
aicr verify ./my-bundle --cli-version-constraint ">= 1.0.0"
aicr verify ./my-bundle --cli-version-constraint "== 1.0.0"
```

The CLI version (`toolVersion`) is read from the *bundle* attestation's
predicate, so it is available once the bundle reaches `attested` (it does not
require the binary attestation).

## 7. JSON output (CI path)

```shell
aicr verify ./my-bundle --format json | jq '{ trustLevel, bundleCreator, toolVersion }'
```

```json
{
  "trustLevel": "verified",
  "bundleCreator": "jdoe@company.com",
  "toolVersion": "1.0.0"
}
```

Branching in a pipeline:

```shell
# Capture the JSON and the verify exit code separately: a binary-attestation
# that *fails* verification still reports trustLevel=attested but exits nonzero.
if out=$(aicr verify ./my-bundle --format json); then rc=0; else rc=$?; fi
trust=$(printf '%s' "$out" | jq -r .trustLevel)
if [ "$rc" -ne 0 ]; then echo "fail — verify exited $rc" ; exit 1 ; fi
case "$trust" in
  verified) echo "ok — deploying" ;;
  attested) echo "warn — escalating" ; notify-slack ;;
  *)        echo "fail — refusing"  ; exit 1 ;;
esac
```

## 8. Tamper demo

The signed manifest hash pins every file listed in `checksums.txt`. Mutating
a listed file breaks verification:

```shell
# Component dirs are numbered NNN-<component>/; tamper the first one's values
# rather than guessing the index, so this works for any generated recipe:
f=$(ls my-bundle/[0-9]*-*/values.yaml | head -1)
echo '# tampered' >> "$f"
aicr verify ./my-bundle
# Expected (exact file and digests vary by recipe):
# ✗ Checksums failed: 1 file mismatch
#     <NNN-component>/values.yaml — sha256 mismatch (got 3f9a…, want 7b21…)
#   Trust level: unknown
# Bundle verification: FAILED (non-zero exit)
```

Editing `checksums.txt` to match the new hash defeats the checksum gate but
breaks the bundle signature gate — the signed subject is the digest of
`checksums.txt` itself, which now doesn't match the signed value.

## Troubleshooting

**"sigstore verification failed — trusted root may be stale"** — Sigstore
rotates its TUF roots periodically. Run `aicr trust update`.

**"trust level: attested (expected: verified)"** — the bundle reaches
`attested` but not `verified`. Common causes: the AICR binary used to build
the bundle was not a release binary, so it carries no binary attestation.
Build with a release binary so the chain can reach `verified`.

**Browser doesn't open on a remote shell** — pass
`--oidc-device-flow` to use device-flow OIDC, or run the
bundle step on a workstation with a browser and copy the bundle to the
remote.

**Signing hangs from a corporate network** — Fulcio (`fulcio.sigstore.dev`)
or Rekor (`rekor.sigstore.dev`) is likely blocked. Sign from a host with
public egress, then copy the bundle.

## Links

* [`bundle-attestation-demo.sh`](bundle-attestation-demo.sh) — runnable version of this walkthrough
* [`bundle-attestation-demo-slides.html`](bundle-attestation-demo-slides.html) — slide deck
* [`provenance.md`](provenance.md) — binary & image attestation (parallel demo)
* [`evidence.md`](evidence.md) — recipe evidence (parallel demo)
* [CLI reference: `aicr verify`](../docs/user/cli-reference.md#aicr-verify) — full flag documentation
* [SECURITY.md](../SECURITY.md) — trust model overview

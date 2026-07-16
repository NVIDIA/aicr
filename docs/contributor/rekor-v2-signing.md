# Rekor v2 Signing

AICR signs its keyless attestations to **Rekor v2** by default. This page
explains what Rekor v2 is, why AICR moved to it, and how the signing path is
wired, so contributors can reason about the transparency-log behavior and its
verification and monitoring consequences.

## What Rekor v2 is

Rekor is Sigstore's signature transparency log: the append-only, publicly
auditable record that every keyless signature is entered into, so that a
signature cannot be produced and then hidden. Rekor v2 is a redesign of that log
with a few properties that matter here:

- **Tile-based.** The log is served as immutable, content-addressed tiles (the
  C2SP tlog-tiles layout, backed by Trillian-Tessera) rather than a live
  database queried entry-by-entry. Tiles are static files, cheap to host on a
  CDN and cheap to read in bulk.
- **Yearly shards.** Each year gets a fresh log shard at
  `log<year>-<rev>.rekor.sigstore.dev` (for example `log2025-1`). A shard is
  bounded in size, and old shards are frozen and archived as static tiles. This
  is the core of v2's "cheaper to run" thesis, and it keeps consistency proofs
  and monitoring bounded.
- **Integrated witnessing.** v2 folds witnessing into the log, strengthening the
  append-only guarantee.
- **No inline timestamp.** A v2 entry does not carry a signed entry timestamp
  (the "SET" that v1 provided). A bundle needs trusted time from a separate
  RFC3161 **timestamp authority (TSA)** instead. This is why AICR attaches a TSA
  when signing to v2 and fails closed if a v2 target has no TSA.
- **No search index.** v2 removed v1's `/api/v1/index/retrieve` search. Finding
  entries is delegated to clients reading tiles.

For the migration status and Sigstore's own guidance, see the
[Rekor v2 GA post](https://blog.sigstore.dev/rekor-v2-ga/) and the
[Rekor evolution post](https://blog.sigstore.dev/rekor-evolution/).

## Why AICR moved to Rekor v2

The motivating driver is **release-identity monitoring**. AICR wants to watch the
public-good log for any entry made under its release-signing identity that a
release did not produce, since such an entry signals OIDC or key compromise. On
Rekor v1 that is not feasible: v1 has no way to query the index by certificate
SAN, and AICR's release identity is keyless (an ephemeral Fulcio certificate, no
email, no fixed public key), so the only way to find AICR's entries is a linear
walk of the entire public-good firehose. Measured, that scan runs roughly 50x
slower than the log grows, so it can never keep up in a bounded job. The analysis
and the pivot are tracked in [#1623](https://github.com/NVIDIA/aicr/issues/1623).

Rekor v2 makes monitoring feasible: tiles are read in bulk (256-entry bundles),
so a scan runs about two orders of magnitude faster than the v1 per-entry fetch,
and a single worker keeps up with the ecosystem's write rate. A yearly shard is
also small and bounded rather than billions of entries. Signing to v2 is the
prerequisite for that monitoring, tracked in
[#1650](https://github.com/NVIDIA/aicr/issues/1650), under the closed
supply-chain epic [#1149](https://github.com/NVIDIA/aicr/issues/1149).

Beyond monitoring, v2 is Sigstore's forward direction (cheaper to operate,
stronger append-only guarantees), and AICR is pre-1.0, so adopting it early is a
deliberate, low-cost choice.

Note that the public-good Sigstore default still points other clients at Rekor
v1 for now, so AICR opts into v2 explicitly by consuming the v2 signing config
(below) rather than waiting for the ecosystem default to flip.

## How AICR signs to v2

### Signing config from TUF, not hardcoded

Signing endpoints (which Fulcio, which Rekor, which TSA) come from a Sigstore
**SigningConfig**. Sigstore distributes a v2 SigningConfig as a TUF target
(`signing_config_rekor_v2.v0.2.json`) that lists the current v2 shard and a TSA,
each with a validity window. AICR fetches that target through the TUF client
(`pkg/trust`), so shard rotation is handled by Sigstore and nothing is
hardcoded. `aicr trust update` refreshes both the trusted root (verification
material) and this signing config (sign-side endpoints); signing also resolves
the config from the local cache and falls back to a bounded network fetch on a
cold cache, so a signer works without a prior explicit update.

Selection of the active Rekor and TSA from the config uses sigstore-go's
`root.SelectServices`, which prefers the highest supported API version and never
mixes versions. Because the v2 config lists both a v2 and a v1 Rekor, this
selects v2; the v1 entry is only a fallback for clients that support v1 only.

### The default is set at the CLI layer

Rekor v2 is the default for keyless signing, but the default is computed in the
CLI commands, not in the library's zero-value options. This keeps library,
server, and test callers of `attestation.ResolveOptions` on their existing v1
behavior, so the flip is contained to the signing commands and does not
surprise embedders. All three keyless signing paths default to v2 consistently:

- `aicr bundle --attest`
- `aicr recipe sign-catalog`
- evidence signing (`aicr validate --emit-attestation --push`,
  `aicr evidence publish`, `aicr evidence sign`)

The single `attestation.SignOptionsFromResolve` mapper carries the signing-target
fields (Fulcio, Rekor, signing config) from `ResolveOptions` into `SignOptions`
for every path, so a new field cannot be dropped by one caller and silently sign
to the wrong log.

### Opting out to Rekor v1 or a custom config

Signing target selection has this precedence: a `--signing-config` file, then the
TUF v2 config (default), then a Rekor v1 URL.

- `--rekor-url <url>` signs to **Rekor v1** at that URL, for a private instance
  or the public-good v1 URL. Existing users who set `--rekor-url` or
  `spec.bundle.rekorURL` are therefore unaffected by the default flip.
- `--signing-config <file>` signs with a custom SigningConfig JSON, for an edited
  config or a private v2 instance.
- KMS signing (`--signing-key`) follows the same rules: it signs to Rekor v2 by
  default and honors the same `--rekor-url` / `--signing-config` opt-outs. The
  `KMSAttester` builds its transparency policy through the same
  `transparencyForOptions` path as keyless signing, so both stay in lockstep. A
  KMS v2 entry carries public-key verification material (no Fulcio certificate).
- `--tlog-upload=false` (KMS only) skips the Rekor upload entirely for fully
  offline / air-gapped signing (#409). It sets `ResolveOptions.DisableTLogUpload`,
  which wires `NewKMSAttester(..., WithoutTransparencyLog())`; the attester's
  `HasRekorEntry()` then reports false and `Attest` attaches no transparency log.
  The flag is rejected on the keyless path, because keyless OIDC signing needs
  Fulcio and Rekor network access, and is mutually exclusive with `--rekor-url`
  and `--signing-config`: those select a transparency-log target, which cannot be
  reconciled with writing no entry (the no-tlog policy would silently override
  them, so it fails closed instead). Verify such a bundle with
  `aicr verify --key <uri> --insecure-ignore-tlog`.

`--rekor-url` and `--signing-config` are mutually exclusive, so an operator's
private Rekor is never silently overridden by the v2 default.

### Fail-closed rules

- A v2 selection with no timestamp authority is rejected: a v2 bundle carries no
  inline timestamp, so it would have no trusted time and could not be verified.
- In the release pipeline, if signing is enabled (`SLSA_PREDICATE` set) but the
  signing config path is unset, the goreleaser hooks error rather than let cosign
  silently fall back to Rekor v1.

### Release wiring

At release time the `generate-slsa-predicate` action runs
`aicr trust update --emit-signing-config` to materialize the v2 SigningConfig to
a file and exports its path as `AICR_SIGNING_CONFIG` (co-located with
`SLSA_PREDICATE` so every signing workflow inherits it). The goreleaser hooks pass
that file to both `cosign attest-blob --signing-config` (the binary attestation)
and `aicr recipe sign-catalog --signing-config` (the recipe catalog), so both
land in the same v2 log. `cosign` cannot fetch the TUF-distributed v2 config
itself (`cosign signing-config create` only builds one from hardcoded URLs), so
`aicr` supplies it.

**The signer must be Cosign >= v3.1.0** (pinned in `.settings.yaml` and passed to
`cosign-installer` via `cosign-release` in every signing workflow). Only v3.1.0+
logs a DSSE attestation to Rekor v2 as a `hashedrekord` entry over the envelope's
pre-auth encoding (PAE); older Cosign writes the legacy `dsse` entry type, which
no released sigstore-go can verify. The `bundle-headless-oidc-ci` chainsaw guard
only catches a regression on the `bundle --attest` path, so keep the pin in step
with the installer default (which lags well behind v3.1.0). See #1650.

## Verification

The consumer verification commands do not change. A bundle self-describes which
log it is in (the entry, its inclusion proof, and the checkpoint origin are all
embedded), and the public-good trusted root already carries the v2 log's key and
a TSA, so `aicr verify` and `cosign verify-blob-attestation` verify a v2 bundle
with the same flags as a v1 one.

`aicr verify` needs only the `aicr` binary and handles v1 and v2 transparently
(it embeds a v2-capable sigstore-go). The **Cosign v3.0.1+** floor applies only to
the `cosign verify-blob-attestation` path: older Cosign cannot parse a v2 tiles
inclusion proof or its RFC3161 timestamp. Releases published before the v2
cutover remain in Rekor v1 and verify with any recent Cosign.

## Operational notes

- **Shard rotation** is Sigstore's responsibility: when a new yearly shard is
  deployed, Sigstore updates the TUF signing-config target, and AICR picks it up
  on the next `trust update` or on-demand fetch. Do not hardcode a shard URL.
- **Stale-cache downgrade window.** `root.SelectServices` applies each service's
  validity window at sign time. If a cached signing config's v2 shard ages past
  its `validFor.end` before the cache refreshes, selection falls back to the
  still-valid v1 entry — a silent Rekor v1 downgrade. The window is bounded: the
  TUF client refreshes the cache when its timestamp metadata expires, so the
  worst case is that expiry interval, and a v1 selection under the v2 default is
  logged as a warning (see `NewSigningConfigPolicyFromTUF`). Run `aicr trust
  update` to refresh eagerly; the `bundle-headless-oidc-ci` shard assertion
  guards the CI path but not individual end users.
- **A private or local Rekor v2 test stack** would need both a `rekor-tiles`
  server and a `timestamp-authority`, driven via `--signing-config`. The existing
  private-Sigstore chainsaw test exercises the Rekor v1 path via `--rekor-url` and
  is unaffected.
- **`AICR_SIGNING_CONFIG` binds as `--signing-config` process-wide.** Because
  `generate-slsa-predicate` exports it into the job environment, any later
  `aicr … --rekor-url …` in the same job auto-picks it up as `--signing-config`
  and fails closed with the mutual-exclusivity error (the two select different
  logs). A caller combining a private `--rekor-url` with an inherited config env
  must `unset AICR_SIGNING_CONFIG` first — as the private-Sigstore chainsaw test
  does. The error message names both env vars so the collision is diagnosable.
- The `bundle-headless-oidc-ci` chainsaw test asserts that a default signature
  lands in a v2 shard, guarding against a silent revert to v1.

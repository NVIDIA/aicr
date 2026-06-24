# Publishing Recipe Evidence

Recipe evidence is a signed bundle that proves a recipe's validators passed
on hardware matching its `criteria`. Producing it has two legs:

1. **Validate + push (network-light).** Runs where the cluster lives —
   often a corporate VPN. Captures a snapshot, runs the validators, builds
   the bundle, and pushes it to an OCI registry.
2. **Sign (Fulcio-bound).** Signs the pushed bundle with keyless OIDC
   (Sigstore Fulcio + Rekor) and records the signature in the committed
   pointer.

The two legs need different network access, and the signing leg is the one
contributors most often can't complete locally.

## The Fulcio connectivity problem

Keyless signing reaches `fulcio.sigstore.dev` and `rekor.sigstore.dev`.
**Corporate VPNs and some home networks block TLS to these hosts** — the
connection is rejected at the IP level upstream, not by AICR. The symptom
is a hang or a TLS/connection-reset error during the sign step, even though
`aicr validate` and the registry push succeed.

This is not an AICR bug and AICR cannot work around it; the block is on the
path to Sigstore's public-good infrastructure. Contributors have reported
needing a phone hotspot to sign from a laptop.

## Recommended path: split the legs, sign in CI

The fix is to keep the network-light leg local and move only the
Fulcio-bound leg to GitHub Actions, where Sigstore is reachable. The signer
identity becomes your fork's GitHub Actions OIDC identity rather than a
personal one.

### 1. Validate and push an unsigned bundle (local, on VPN)

```shell
aicr snapshot -o snapshot.yaml
aicr validate \
  -r recipes/overlays/<slug>.yaml \
  -s snapshot.yaml \
  --emit-attestation ./out \
  --push ghcr.io/<your-fork-owner>/aicr-evidence \
  --no-sign
cp ./out/pointer.yaml recipes/evidence/<slug>.yaml
```

`--no-sign` skips all OIDC/Fulcio/Rekor work, so this runs even where
Sigstore egress is blocked. It pushes the content-addressed bundle and
writes a pointer with an empty `signer` block. See
[`aicr validate`](../user/cli-reference.md#aicr-validate) and
[`aicr evidence publish`](../user/cli-reference.md#aicr-evidence-publish)
(which also accepts `--no-sign` if you push as a separate step).

> **Make the registry package public.** The signing step (and the
> repository's evidence gate) pull the bundle back to sign and verify it.
> If your fork's `aicr-evidence` package is private, the pull fails with an
> HTTP **403** and the gate reports `registry-forbidden`. On GHCR, set the
> `aicr-evidence` package visibility to **public** under your account's
> *Packages* settings. Any OCI-1.1 registry works; it just has to be
> readable.

### 2. Commit the unsigned pointer and push your branch

```shell
git add recipes/evidence/<slug>.yaml
# Use -s (DCO sign-off) — required for all contributors. NVIDIA org members
# additionally use -S (cryptographic signing); external contributors use -s
# alone. See CONTRIBUTING.md.
git commit -s -m "evidence: <slug> (unsigned; sign in CI)"
git push
```

`aicr evidence verify` reports an unsigned pointer as a non-failing
**pending signature** state, so an in-flight PR is not flagged as broken.

### 3. Sign in your fork via GitHub Actions

Run the **Recipe Evidence: Sign** workflow
(`.github/workflows/evidence-publish.yaml`) from your fork's *Actions* tab
(it is `workflow_dispatch` — no inputs). The filename says *publish* because it
anticipates a future auto-publish leg (see the workflow header); today it only
signs. The workflow:

- discovers every pointer in `recipes/evidence/*.yaml` with an empty
  `signer` (i.e. unsigned),
- signs the bundle each one already references using the runner's ambient
  OIDC token ([`aicr evidence sign`](../user/cli-reference.md#aicr-evidence-sign)),
- patches the pointer's `signer` block in place and commits it back to the
  branch.

It is a clean no-op when there are no unsigned pointers, and it fails with a
clear message if it cannot pull a bundle (the public-package requirement
above). Pull the commit it pushes (`git pull`) — your PR now carries a pointer
whose `signer` block is filled in (the *bundle* is signed; the commit-back
itself is a normal, unsigned GitHub Actions commit, which the eventual
squash-merge re-signs under the repo's policy).

> The workflow declares `id-token: write` in its own `permissions:` block —
> it is not a default. Your fork must have GitHub Actions enabled and not
> restrict workflow OIDC token issuance (the default fork setting allows it).
>
> Avoid pushing other commits to the branch while the workflow runs: it
> commits the patched pointers back, and a concurrent push would cause a
> non-fast-forward — re-dispatch after `git pull` if that happens.

## Fallback: split the legs locally

If you have a host with Sigstore egress (a jump box, CI runner, or
hotspot), you can run the signing leg there instead of in Actions:

```shell
# On VPN, where the cluster is: produce an unsigned bundle.
aicr validate -r recipes/overlays/<slug>.yaml -s snapshot.yaml --emit-attestation ./out

# Off VPN, where Sigstore is reachable: sign, push, and write the pointer.
aicr evidence publish ./out --push ghcr.io/<your-fork-owner>/aicr-evidence
cp ./out/pointer.yaml recipes/evidence/<slug>.yaml
```

The signed artifact is content-addressed, so the result is identical
regardless of which host ran which leg.

## See also

- [ADR-007: Verifiable Recipe Test Evidence](../design/007-recipe-evidence.md) — trust model and bundle format.
- [`aicr evidence` CLI reference](../user/cli-reference.md#aicr-evidence-sign) — `sign`, `publish`, `verify`.

# Publishing Recipe Evidence

Recipe evidence is a signed bundle that records a recipe's validator results
and binds them to a signer's identity (it attests the recorded result, not that
a specific cluster existed). Producing it has two legs:

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
cp ./out/pointer.yaml recipes/evidence/<recipe>.yaml
```

`--no-sign` skips all OIDC/Fulcio/Rekor work, so this runs even where
Sigstore egress is blocked. It pushes the content-addressed bundle and
writes a pointer with an empty `signer` block.

Commit it **flat** at `recipes/evidence/<recipe>.yaml` (where `<recipe>` is
the pointer's `recipe:` field). This is the *only* committable location for
an unsigned pointer: the nested per-source path
`recipes/evidence/<recipe>/<src>/<digest>.yaml` includes a `<src>`
segment derived from the **signer**, which a `--no-sign` pointer does not yet
have. The signing leg below relocates the pointer to that nested path once it
is signed. See
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

> **Grant the fork's Actions write access to the package.** Signing in CI
> *attaches* the signature as an OCI referrer — a registry **write**, separate
> from the public-read above. Two GHCR prerequisites the signing leg needs:
> 1. The package's **Actions access** must include your fork repo with the
>    **Write** role (GHCR package → *Settings* → *Manage Actions access* → add
>    your fork). Without it the attach fails with HTTP **403** even though
>    `packages: write` is granted in the workflow.
> 2. A package first pushed from a local `aicr validate --push` may be linked
>    to **NVIDIA/aicr** (the chart/repo it was built against) rather than your
>    fork. Re-link it to your fork (GHCR package → *Settings* → *Change
>    repository*) so your fork's Actions token is authorized. The first push
>    from the fork's own CI links it correctly.

### 2. Commit the unsigned pointer and push your branch

```shell
git add recipes/evidence/<recipe>.yaml
# Every commit from every contributor must be both signed off (-s, DCO) and
# cryptographically signed (-S). See CONTRIBUTING.md.
git commit -s -S -m "evidence: <recipe> (unsigned; sign in CI)"
git push
```

`aicr evidence verify` reports an unsigned pointer as a non-failing
**pending signature** state, so an in-flight PR is not flagged as broken.

### 3. Sign in your fork via GitHub Actions

The **Recipe Evidence: Sign** workflow (`.github/workflows/evidence-publish.yaml`)
runs two ways in your fork:

- **Automatically** when you push a change under `recipes/evidence/**` to any
  branch other than your default — so step 2's push usually triggers it for you.
- **Manually** from your fork's *Actions* tab (`workflow_dispatch`, no inputs) —
  use this to re-run after making the registry package public, or if the auto-run
  didn't fire.

The auto-trigger is fork-only (it never runs on the canonical repo) and skips its
own signing commit, so it can't loop. The workflow:

- discovers every flat pointer in `recipes/evidence/*.yaml` with an empty
  `signer` (i.e. unsigned),
- signs the bundle each one already references using the runner's ambient
  OIDC token and **relocates** the now-signed pointer to its canonical
  per-source path `recipes/evidence/<recipe>/<src>/<digest>.yaml`
  ([`aicr evidence sign --relocate`](../user/cli-reference.md#aicr-evidence-sign)),
- commits the move back to the branch.

It is a clean no-op when there are no flat pointers, and it fails with a
clear message if it cannot pull a bundle (the public-package requirement
above). Pull the commit it pushes (`git pull`) — your PR now carries a
**signed, nested** pointer (the flat pending file is gone). The *bundle* is
signed; the commit-back is created through GitHub's `createCommitOnBranch`
API, so GitHub applies its web-flow signature and it shows **Verified** (the
DCO sign-off is preserved). The evidence trust anchor remains the Sigstore
bundle signature, verified by `aicr evidence verify` — independent of the
git commit's signature status.

> **Run this leg before merge.** The blocking per-source contract gate
> requires a **signed, nested** pointer; it rejects a flat pending pointer
> still sitting at the evidence root. That is deliberate — an unsigned pointer
> must not merge to `main`, where the fork-only sign workflow would never run
> to complete it. So a PR whose pointer is still flat stays red until this leg
> signs and relocates it (and you `git pull` the commit-back). Local tooling
> can validate the transient flat state with
> `evidence-pointercheck --allow-pending`, but the merge gate does not set it.

> The workflow declares `id-token: write` in its own `permissions:` block —
> it is not a default. Your fork must have GitHub Actions enabled and not
> restrict workflow OIDC token issuance (the default fork setting allows it).
>
> Avoid pushing other commits to the branch while the workflow runs: it
> commits the patched pointers back, and a concurrent push would cause a
> non-fast-forward — re-dispatch after `git pull` if that happens.

### 4. Add your signer to the allowlist

A signed, nested pointer is **not** sufficient on its own. The contract gate
also requires the pointer's claimed signer to be listed in
[`recipes/evidence/allowlist.yaml`](../../recipes/evidence/allowlist.yaml) as a
`community` or `partner` entry; an unlisted signer is rejected (it would only
ever count as "reported", never corroborating). Your fork's GitHub Actions OIDC
identity is a **new signer** that the existing entries do not cover, so you must
add it in the same PR.

The entry is keyed by the one-way `source` slug — the exact `<src>` directory
the signing leg just created under `recipes/evidence/<recipe>/<src>/`. Add it
under the `community` class (no cleartext identity; an optional `label` may
carry a non-PII handle):

```yaml
# recipes/evidence/allowlist.yaml
community:
  - issuer: https://token.actions.githubusercontent.com  # required
    source: <src>          # the <src> directory segment from step 3
    label: <your-gh-handle> # optional, non-PII
```

See the header of `recipes/evidence/allowlist.yaml` and
[artifact verification](../user/artifact-verification.md) for the anti-sybil
rules; maintainer review of this entry is the trust gate.

## Fallback: split the legs locally

If you have a host with Sigstore egress (a jump box, CI runner, or
hotspot), you can run the signing leg there instead of in Actions:

```shell
# On VPN, where the cluster is: produce an unsigned bundle.
aicr validate -r recipes/overlays/<slug>.yaml -s snapshot.yaml --emit-attestation ./out

# Off VPN, where Sigstore is reachable: sign, push, and write the pointer.
aicr evidence publish ./out --push ghcr.io/<your-fork-owner>/aicr-evidence
```

`aicr evidence publish` signs the pointer, so commit it directly at its
**canonical per-source path**, not flat. The command logs the exact
destination (`copyTo=…`) — copy the pointer there:

```shell
# The path is recipes/evidence/<recipe>/<src>/<digest>.yaml; <src> is
# derived from your signer identity. Use the copyTo path the command printed.
mkdir -p recipes/evidence/<recipe>/<src>
cp ./out/pointer.yaml recipes/evidence/<recipe>/<src>/<digest>.yaml
```

The flat commit-then-CI-sign flow above is only for the unsigned case: a
signed pointer already has the signer the `<src>` segment derives from, so
it goes straight to its nested path and needs no relocation. The *bundle* is
content-addressed, so its bytes (and `bundle.digest`) are identical regardless
of which host ran which leg — but the committed *pointer path* still depends on
the signer: `<src>` is derived from your signing identity, so a different
signer lands the pointer under a different `<src>` directory.

## See also

- [ADR-007: Verifiable Recipe Test Evidence](../design/007-recipe-evidence.md) — trust model and bundle format.
- [`aicr evidence` CLI reference](../user/cli-reference.md#aicr-evidence-sign) — `sign`, `publish`, `verify`.

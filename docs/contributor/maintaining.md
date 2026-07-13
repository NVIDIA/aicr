# Maintaining AICR

Runbook for AICR maintainers. Two surfaces:

- **Releases** — cadence, tag flow, supply-chain verification.
- **Recipe contributions** — reviewing PRs against `recipes/` paths,
  including the forthcoming evidence-backed flow from ADR-007.

For end-user release verification, see
[RELEASING.md](https://github.com/NVIDIA/aicr/blob/main/RELEASING.md).
For contribution mechanics (DCO, CI, signing), see
[CONTRIBUTING.md](https://github.com/NVIDIA/aicr/blob/main/CONTRIBUTING.md).

## Cutting a Release

The full release procedure lives in
[RELEASING.md](https://github.com/NVIDIA/aicr/blob/main/RELEASING.md).
The short form:

| Step | Command | Notes |
|------|---------|-------|
| 1. Pre-flight | `make qualify` on `main` | Must pass. Tests + lint + e2e + scan. |
| 2. Bump | `make bump-patch` (or `bump-minor`/`bump-rc`) | Tags HEAD and pushes the tag. To promote a pre-release to stable on the same SHA, use `make bump-promote TAG=<rc-tag>` (e.g. `TAG=v1.3.0-rc2`). |
| 3. Push | `git push origin <tag>` (done by the bump target) | Triggers the `On Tag Release` (`on-tag.yaml`) workflow. |
| 4. Verify | `gh release view <tag>` + `cosign verify-attestation ...` | See RELEASING.md §Verification. |
| 5. Demo | Cloud Run deploy auto-triggers on tag push | Inspect `aicrd.demo` health. |

Bi-weekly cadence; hotfix between cycles when a fix is critical.

### Common Release Breakages

**`goreleaser` fails with auth conflict.** `goreleaser` panics if both
`GITLAB_TOKEN` and `GITHUB_TOKEN` are set. Always `unset GITLAB_TOKEN`
before `make build`, `make qualify`, `make e2e`, or any release tooling
that wraps goreleaser. Local-shell hazard; CI is unaffected.

**Tag exists but workflow did not trigger.** Delete the local tag and
re-push from a fresh shell. If the workflow ran but failed, fix on
`main` and re-tag — never amend a published tag.

**Attestation verification fails for users.** Confirm the GitHub
attestation predicate type matches `https://slsa.dev/provenance/v1`
and that the user's `gh` is recent enough (`gh attestation verify` is
v2.49+). RELEASING.md §Container Attestations has both `gh` and
`cosign` flows.

**Cloud Run demo deploy fails after tag push.** Check the demo deploy
job (`deploy.yaml`, called from `on-tag.yaml`); the most common cause is GitHub Container
Registry (GHCR) pull
failure during the first 60s after tag publish. Re-run the workflow.

## Release Supply-Chain Monitoring

The `Rekor Monitor` workflow (`.github/workflows/rekor-monitor.yaml`) runs
hourly and calls the upstream `sigstore/rekor-monitor` reusable workflow. It
watches the **Rekor v2** transparency log (where AICR release signing writes
since [#1650](https://github.com/NVIDIA/aicr/issues/1650)) for two things, both
in one job: that the log stays append-only (consistency), and that no entry
appears under AICR's release signing identity that a release did not produce
(identity). On either failure it opens an issue.

This protects the trust root every AICR consumer depends on: the release
binaries, the signed recipe catalog, and the container images all chain to that
one identity. When the workflow files an issue, follow the triage steps in the
workflow file's header comment; an unrecognized identity hit should be treated
as potential OIDC/key compromise.

### Why v2, and why identity monitoring is feasible now

Identity monitoring is a linear scan of every entry added to the log since the
last checkpoint, because Rekor's index cannot be queried by certificate SAN and
AICR's keyless release identity has no email or fixed public key to search on.
On the Rekor **v1** firehose that scan runs roughly 50x slower than the log
grows, so it can never keep up inside a bounded CI job: the earlier v1
identity config timed out on every run and never completed a single scan
([#1623](https://github.com/NVIDIA/aicr/issues/1623)). Rekor **v2** is
tile-based: bulk 256-entry reads let a single worker outpace the log, so the
identity scan is a cheap job that always finishes. This is why the whole design
is a single unbounded scan again rather than sharded paging.

### Shard selection (automatic, no manual re-tune)

The monitor selects Rekor v2 by pointing its `url` at a v2 shard listed in the
Sigstore `SigningConfig`; a match on a v2 service switches it to v2, after which
it auto-discovers the **full** shard set from TUF and refreshes it every run.

The shard URL is **not hardcoded**. The `resolve-v2-shard` job computes it at run
time from the same TUF-distributed signing config that release signing resolves,
then feeds it to the monitor:

```bash
# Same command the resolve-v2-shard job runs (works from a fresh checkout).
go run ./cmd/aicr trust update --emit-signing-config signing-config.json
jq -er '[.rekorTlogUrls[] | select(.majorApiVersion == 2)] | sort_by(.validFor.start) | last | .url' signing-config.json
```

Because it reads the signing config directly, the monitor provably watches where
releases actually write, and yearly shard rotation (`log2025-1` -> `log2026-1`
-> ...) needs no change to this workflow. If the resolve job ever fails (for
example the TUF CDN is unreachable), the monitor job is skipped for that run and
retries on the next hourly tick.

### First run after switching from v1: reset the checkpoint

The `checkpoint` artifact persists a **v1** checkpoint from the prior config; a
v2 run cannot parse it and will fail. After merging a change that moves this
workflow to v2, delete the stale artifact once so the first v2 run establishes a
fresh v2 baseline (it saves the current v2 tree head and scans forward from
there):

```bash
gh api "repos/NVIDIA/aicr/actions/artifacts?name=checkpoint" \
  --jq '.artifacts[].id' \
  | xargs -I{} gh api -X DELETE "repos/NVIDIA/aicr/actions/artifacts/{}"
```

The first v2 run then watches forward from the current head; historical entries
predating the baseline are covered by release-time verification (the `aicr
verify` path), not by this monitor.

## Reviewing Recipe Contributions

A recipe PR touches `recipes/overlays/`, `recipes/mixins/`,
`recipes/components/`, or `recipes/registry.yaml`. Three concerns:

1. **The recipe parses and resolves.** Covered by `make qualify` and
   the recipe unit tests; trust CI here.
2. **The BOM stays in sync.** `make bom-docs` must have been run; the
   `docs/user/container-images.md` change must be present in the PR
   when a chart pin or values file changed. See
   [recipe.md](recipe.md#bom-regeneration).
3. **The configuration is correct on the target hardware.** This is
   the hard one — maintainers cannot run a contributor's GB200 recipe
   on an H100. ADR-007 closes that gap with bundled evidence.

The forthcoming evidence flow is documented below as future state.
Until ADR-007 PR-D lands, recipe acceptance still relies on author
attestation + maintainer judgement.

## Evidence-Backed Review (Future State per ADR-007)

> **Status (partially landed).** `recipes/evidence/` now exists: the
> per-source pointer tree (`#1347` Option A / `#1401`) shipped, and two
> signed nested pointers are committed today
> (`h100-gke-cos-training`, `gb200-eks-ubuntu-training`), each under
> `recipes/evidence/<recipe>/<src>/<digest>.yaml`. Two gates run on
> `recipes/evidence/**`: the **blocking** *Evidence Pointer Contract*
> (`tools/evidence-pointercheck`) rejects any committed pointer that lacks a
> signer claim, lives at a flat path, sits under the wrong signer directory,
> or whose *claimed* signer is not allowlisted — a structural check on the
> pointer's signer fields, not cryptographic signature verification (#1535);
> and the **warning-only**
> recipe-evidence verify gate (signature/integrity against OCI). Cryptographic
> trust is enforced **after merge, at ingest** (`evidence-ingest.yaml`), which
> verifies the signature pinned to the claimed signer before any result is
> counted (#1535). (This ingest verification is implemented but **currently fails closed** — the GP2 loader cannot yet parse the canonical `identityPattern`/`source` allowlist; tracked in [#1505](https://github.com/NVIDIA/aicr/issues/1505).) The ADR-007 `spec.maintainers` work (PR-D) is still future
> state. Treat
> proposed-only items below as design contract, not operational guide.

The motivating constraint: maintainers cannot independently re-run a
contributor's validator on hardware they don't have. The evidence
bundle is the trust artifact that lets a maintainer accept a recipe
they cannot reproduce.

### Reviewing a Recipe PR You Can't Run

Use this checklist on any PR that touches `recipes/overlays/**`,
`recipes/mixins/**`, `recipes/components/**`, or `recipes/registry.yaml`.
Items 1, 2, and 5 are validated automatically by the `recipe-evidence`
check; items 3–4 and 6–8 are maintainer judgement calls. The sticky comment
renders only Recipe / Source / Pointer / Verify / Digest-match columns — it
does not surface the signer identity or OCI ref, so review those from the
committed pointer file and the PR description.

1. **Pointer file present.** At least one per-source pointer file under
   `recipes/evidence/<recipe>/<src>/<bundle-digest>.yaml` — one immutable
   file per signed run — exists for every touched overlay. The CI gate is
   warning-only: when a recipe change has no matching pointer it flags the
   gap in the sticky comment but does not block merge.
2. **`recipe-evidence` check is green.** This warning-only OCI check runs
   `aicr evidence verify` per pointer; exit 0 means the bundle verified
   (predicate/schema parse, manifest-inventory hash binding, and — when the
   bundle is signed — signature + claimed-signer cross-check) **or** is a
   valid *pending* (unsigned) pointer. It does not by itself prove the signer
   is a trusted identity: the blocking on-disk pointer-contract gate is
   structural (it checks the *claimed* signer against the allowlist, not a
   cryptographic signature — see
   [#1535](https://github.com/NVIDIA/aicr/issues/1535)). A structured `exit: 1` (in `--format json`) requires explicit disposition
   (see [Exit-1 Review Process](#exit-1-review-process)); `exit: 2` is a hard
   fail. Both collapse to OS exit code 2, so distinguish them by reading
   `.exit` from `aicr evidence verify --format json`.
3. **Signer identity is acceptable.** Open the committed pointer file under
   `recipes/evidence/<recipe>/<src>/` and review its `signer` block. See
   [Signer Identity Trust Patterns](#signer-identity-trust-patterns).
4. **Bundle Open Container Initiative (OCI) ref matches PR description.** The PR template
   has no dedicated evidence section, so contributors paste the `bundle.oci` field
   into the PR description (see the recipe-development guide); confirm the
   pointer's `bundle.oci` matches the ref pasted in the PR description.
5. **Manifest inventory hash matches.** The shipped verifier binds
   `manifest.json` to the predicate's manifest digest and verifies every
   bundle file and phase-report digest against it. (The semantic
   material-slice / JCS subject-digest binding is proposed in ADR-007 but
   not yet implemented — today's canonicalization hashes the normalized
   full recipe, not a material slice.)
6. **Test environment is plausible.** The PR template captures cloud,
   accelerator, OS, Kubernetes version, and cluster size. A GB200
   recipe attested from a single-node Minikube is a red flag.
7. **BOM reflects the recipe's image set.** Spot-check the CycloneDX
   BOM in the bundle against `docs/user/container-images.md` for the
   touched components. Drift indicates the contributor's `aicr
   validate` ran against a different recipe than the one in the PR.
8. **Recipe changes are scoped.** A new accelerator overlay should not
   touch unrelated overlays or component values.

### Signer Identity Trust Patterns

`aicr evidence verify` records the OIDC issuer and identity from the
cosign keyless certificate but does not classify it. Three patterns
cover most contributions in V1.

| Pattern | Issuer | Identity | Treatment |
|---------|--------|----------|-----------|
| **NVIDIA employee** | `token.actions.githubusercontent.com` or `accounts.google.com` | GitHub user in `NVIDIA` org, or `@nvidia.com` Google | Accept on identity |
| **Unknown fork** | GitHub Actions or public OIDC | New GitHub user | Confirm cosign identity == PR author; mismatch warrants a comment |
| **Corporate tenant** | `login.microsoftonline.com/<tenant>/v2.0` or workspace Google | Tenant user | Note issuer; the tenant is the trust anchor |

V1 deliberately ships without a formal trust-tier policy (see ADR-007
§"What V1 does not ship"). When a pattern recurs often enough to
warrant filtering, the tier-policy work pulls in.

### Exit-1 Review Process

A structured `exit: 1` (the `.exit` field from `aicr evidence verify --format
json`; the process itself exits with OS code 2) means the bundle verified
cleanly (signature, predicate/schema,
manifest-inventory hash, signer cross-check) but one or more validator
phases reported failures. Common causes: a conformance check failed on the
contributor's hardware, a performance threshold was not met, an
optional check requires a feature the contributor's cluster does not
have.

A structured `exit: 1` is **not** the same as evidence/exempt: `exit: 1` means
"evidence was produced and shows a partial failure"; exempt means
"no evidence was produced."

**Workflow:**

1. Contributor declares exit-1 intent in the PR description (the PR
   template has no dedicated evidence section), with a reason.
2. If acceptable, apply `evidence/known-failure` label (not yet created — future state) and merge.
3. If not, request changes. Typical resolutions: narrow the recipe
   criteria so the failing check is not selected, fix the underlying
   constraint, or attest against a different cluster where the check
   passes.

**Acceptable** reasons cluster into: optional check not applicable to
this hardware; performance ceiling is hardware-limited; validator
under active rework. **Unacceptable**: "test was flaky, please merge"
or any reason that asks the maintainer to extend trust beyond what
the evidence shows.

### evidence/exempt Bypass Policy

> **Future state.** The `evidence/known-failure` and `evidence/exempt` labels
> are not yet created, and the recipe-evidence check does not yet implement the
> exemption bypass. This section describes the intended process, not current
> operational behavior.

The `evidence/exempt` label bypasses the recipe-evidence check
entirely. It exists for PRs that modify files under `recipes/` for
non-recipe reasons.

**Appropriate uses:**

- Mechanical refactors (file renames, comment-only changes, license
  header sweeps).
- Self-bootstrapping changes that wire up the evidence pipeline
  itself.
- Documentation edits that touch `recipes/` paths but no recipe
  semantics.

**Inappropriate uses:**

- "I don't have the hardware right now, please merge." Maintainers
  MUST NOT apply the label to skip an inconvenient evidence check.
- Recipe value changes (image versions, constraint thresholds,
  overlay merge behavior).

A PR carrying `evidence/exempt` must include a sentence in the
description explaining why the bypass is appropriate. The label is
queryable via `is:pr label:evidence/exempt` for audit.

### 6-Month Audit Runbook

Quarterly or semi-annually, walk the merged-recipe history to confirm
that what merged is still verifiable:

```bash
# Enumerate recently-touched pointers
git log --since='6 months ago' --diff-filter=AM \
  --name-only --pretty=format: \
  -- recipes/evidence/ ':(exclude)recipes/evidence/allowlist.yaml' | sort -u

# For each, re-verify against the current OCI artifact (POINTER = one path
# from the list above)
POINTER="recipes/evidence/<recipe>/<src>/<digest>.yaml"
aicr evidence verify "$POINTER"
```

Exit 0 confirms the bundle is still fetchable and the signature still
chains. If the OCI registry has been deleted the bytes are gone, so
`aicr evidence verify` (and `cosign verify-attestation`, which also pulls the
artifact) can no longer run. The only remaining record is the Rekor
transparency log: search it by the bundle digest recorded in the pointer to
confirm the entry existed and who signed it (it cannot recover the bytes).

```bash
# pull the digest out of the pointer and strip the algorithm prefix
DIGEST=$(yq -r '.attestations[0].bundle.digest' "$POINTER")
UUID=$(rekor-cli search --sha "${DIGEST#sha256:}" --format json | jq -er '.UUIDs[0]')
rekor-cli get --uuid "$UUID"
```

Pointers older than 24 months are past the V1 re-cert age cutoff (see
ADR-007 §"What V1 does not ship"). File an issue asking the
contributor (or a replacement) to re-attest.

### `maintainers:` Block Routing (Post PR-D)

ADR-007 PR-D adds an optional `maintainers:` block to recipe
metadata. It is a **routing surface**, not a merge-authority surface:
it provides a durable contact for re-cert prompts and lets the audit
runbook file re-cert issues. It does not confer merge authority and
does not replace the signer identity on the bundle.

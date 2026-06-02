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
| 2. Bump | `./tools/release patch` (or `minor`/`rc`/`promote`) | Creates the signed tag locally. |
| 3. Push | `git push origin <tag>` | Triggers `release.yaml` workflow. |
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

**Cloud Run demo deploy fails after tag push.** Check
`deploy-demo.yaml` workflow; the most common cause is GitHub Container
Registry (GHCR) pull
failure during the first 60s after tag publish. Re-run the workflow.

## Reviewing Recipe Contributions

A recipe PR touches `recipes/overlays/`, `recipes/mixins/`,
`recipes/components/`, or `recipes/registry.yaml`. Three concerns:

1. **The recipe parses and resolves.** Covered by `make qualify` and
   the recipe unit tests; trust CI here.
2. **The BOM stays in sync.** `make bom-docs` must have been run; the
   `docs/user/container-images.md` change must be present in the PR
   when a chart pin or values file changed. See
   [recipe.md](recipe.md#bom-regeneration-rule).
3. **The configuration is correct on the target hardware.** This is
   the hard one — maintainers cannot run a contributor's GB200 recipe
   on an H100. ADR-007 closes that gap with bundled evidence.

The forthcoming evidence flow is documented below as future state.
Until ADR-007 PR-D lands, recipe acceptance still relies on author
attestation + maintainer judgement.

## Evidence-Backed Review (Future State per ADR-007)

> **Status:** ADR-007 PR-D has not landed. `recipes/evidence/` does
> not exist yet; `aicr evidence verify` ships but the CI gate is
> warning-only. The runbook below describes the target state after
> PR-D. Use it as the design contract, not an operational guide.

The motivating constraint: maintainers cannot independently re-run a
contributor's validator on hardware they don't have. The evidence
bundle is the trust artifact that lets a maintainer accept a recipe
they cannot reproduce.

### Reviewing a Recipe PR You Can't Run

Use this checklist on any PR that touches `recipes/overlays/**`,
`recipes/mixins/**`, `recipes/components/**`, or `recipes/registry.yaml`.
Items 1–5 are validated automatically by the `recipe-evidence` check;
items 6–8 are maintainer judgement calls.

1. **Pointer file present.** `recipes/evidence/<recipe>.yaml` exists
   for every touched overlay. The CI gate fails closed when a recipe
   change has no matching pointer.
2. **`recipe-evidence` check is green.** Exit 0 means the bundle
   signature, schema, inventory, fingerprint match, constraint replay,
   and BOM cross-reference all passed. Exit 1 requires explicit
   disposition (see [Exit-1 Review Process](#exit-1-review-process)).
   Exit 2 is a hard fail.
3. **Signer identity is acceptable.** Open the sticky comment, find
   the recipe's `<details>` section, and review the signer block. See
   [Signer Identity Trust Patterns](#signer-identity-trust-patterns).
4. **Bundle Open Container Initiative (OCI) ref matches PR description.** The recipe PR template
   asks the contributor to paste the `bundle.oci` field; confirm the
   sticky comment shows the same ref.
5. **Material slice digest matches.** Verifier step 6a recomputes
   `sha256(JCS(material-slice(post-resolution recipe)))` and confirms
   it matches the attestation's subject digest.
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

Exit 1 means the bundle verified cleanly (signature, schema,
inventory, fingerprint) but one or more validator phases reported
failures. Common causes: a conformance check failed on the
contributor's hardware, a performance threshold was not met, an
optional check requires a feature the contributor's cluster does not
have.

Exit 1 is **not** the same as evidence/exempt. Exit 1 means
"evidence was produced and shows a partial failure"; exempt means
"no evidence was produced."

**Workflow:**

1. Contributor declares exit-1 intent in the PR template's "Evidence
   disposition" section, with a reason.
2. If acceptable, apply `evidence/known-failure` label and merge.
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
  -- recipes/evidence/ | sort -u

# For each, re-verify against the current OCI artifact
aicr evidence verify recipes/evidence/<recipe>.yaml
```

Exit 0 confirms the bundle is still fetchable and the signature still
chains. If the OCI registry has been deleted, fall back to Rekor:

```bash
cosign verify-attestation --type=recipe-evidence/v1 <bundle-oci-ref>
```

A passing Rekor verify confirms the bundle existed and was signed by
the recorded identity, even if the bytes are no longer fetchable.

Pointers older than 24 months are past the V1 re-cert age cutoff (see
ADR-007 §"What V1 does not ship"). File an issue asking the
contributor (or a replacement) to re-attest.

### `maintainers:` Block Routing (Post PR-D)

ADR-007 PR-D adds an optional `maintainers:` block to recipe
metadata. It is a **routing surface**, not a merge-authority surface:
it provides a durable contact for re-cert prompts and lets the audit
runbook file re-cert issues. It does not confer merge authority and
does not replace the signer identity on the bundle.

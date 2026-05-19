# Maintaining Recipe Contributions

Runbook for AICR maintainers reviewing recipe contributions backed by
the evidence bundles described in
[ADR-007](https://github.com/NVIDIA/aicr/blob/main/docs/design/007-recipe-evidence.md).
This page assumes the recipe-evidence CI gate is configured for the
repository; contributor-facing setup steps live in
[CONTRIBUTING.md](https://github.com/NVIDIA/aicr/blob/main/CONTRIBUTING.md).

The motivating constraint: maintainers cannot independently re-run a
contributor's validator on hardware they don't have. The evidence
bundle is the trust artifact that lets a maintainer accept a recipe
they cannot reproduce.

## Reviewing a Recipe PR You Can't Run

Use the checklist below on any PR that touches `recipes/overlays/**`,
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
4. **Bundle OCI ref matches PR description.** The recipe PR template
   asks the contributor to paste the `bundle.oci` field; confirm the
   sticky comment shows the same ref.
5. **Material slice digest matches.** Verifier step 6a recomputes
   `sha256(JCS(material-slice(post-resolution recipe)))` and confirms
   it matches the attestation's subject digest. CI fails closed on
   mismatch; if green, the bundle was generated against the recipe at
   this PR's tree.
6. **Test environment is plausible.** The PR template captures cloud,
   accelerator, OS, Kubernetes version, and cluster size. A GB200
   recipe attested from a single-node Minikube is a red flag worth
   asking about; an H100 recipe attested from a small but real EKS
   cluster is normal.
7. **BOM reflects the recipe's image set.** Spot-check the CycloneDX
   BOM in the bundle against `docs/user/container-images.md` for the
   touched components. Drift here is rare but indicates the
   contributor's `aicr validate` ran against a different recipe than
   the one in the PR.
8. **Recipe changes are scoped.** A new accelerator overlay should
   not touch unrelated overlays or component values. Scope creep
   beyond the contributor's tested environment is a request-changes
   condition even when the evidence check is green.

## Signer Identity Trust Patterns

`aicr evidence verify` records the OIDC issuer and identity from the
cosign keyless certificate but does not classify it. Maintainers
classify at review time. Three patterns cover most contributions in V1.

### NVIDIA-Employee Contribution

**Shape:** OIDC issuer is `https://token.actions.githubusercontent.com`
(GitHub Actions) or `https://accounts.google.com` (workstation `cosign
login`); identity is a GitHub user belonging to the `NVIDIA` org or a
`@nvidia.com` Google identity.

**Treatment:** Accept on identity. The signer is reachable through
internal channels for re-cert prompts and signer-identity disputes.

### Unknown Fork Contribution

**Shape:** OIDC issuer is GitHub Actions or a public OIDC provider;
identity is a GitHub user with no prior AICR history.

**Treatment:** Confirm the cosign identity is the same person as the
PR author. The expected match is the GitHub user appearing in both
the cosign certificate's `Subject` and the PR's
`pull_request.user.login`. Mismatch is not a hard reject but warrants
a comment asking the contributor to clarify.

### Corporate Tenant Contribution

**Shape:** OIDC issuer is a corporate identity provider
(`https://login.microsoftonline.com/<tenant>/v2.0`,
`https://accounts.google.com` with a workspace domain). Identity is a
user in that tenant.

**Treatment:** Note which issuer signed the bundle. The corporate
tenant is the trust anchor; the contributor's organization vouches
for the identity at certificate-issuance time. V1 records the
issuer; no tier policy filters on it.

V1 deliberately ships without a formal trust-tier policy (see
ADR-007 §"What V1 does not ship"). When a contribution pattern
recurs often enough to warrant filtering, the tier-policy work
pulls in.

## Exit-1 Review Process

Exit 1 means the bundle verified cleanly (signature, schema,
inventory, fingerprint) but one or more validator phases reported
failures. Common causes: a conformance check failed on the
contributor's hardware, a performance threshold was not met, an
optional check requires a feature the contributor's cluster does not
have.

Exit 1 is **not** the same as evidence/exempt. Exit 1 means
"evidence was produced and shows a partial failure"; exempt means
"no evidence was produced."

### Workflow

1. The contributor declares exit-1 intent in the recipe PR template's
   "Evidence disposition" section, with a reason.
2. If the reason is acceptable, the maintainer applies the
   `evidence/known-failure` label and merges.
3. If the reason is not acceptable, request changes. Typical
   resolutions: narrow the recipe criteria so the failing check is
   not selected, fix the underlying constraint, or attest against
   a different cluster where the check passes.

### What "Acceptable" Looks Like

Acceptable reasons for exit-1 generally fall into three buckets:

- **Optional check not applicable to this hardware.** The contributor
  has explained why; the recipe's criteria intentionally do not
  promise the failing capability.
- **Performance ceiling is hardware-limited.** A small cluster cannot
  hit a performance threshold tuned for production-sized fabrics; the
  recipe is correct but the contributor's test bed is the bottleneck.
- **Validator under active rework.** A known issue is tracked; the
  failure is documented and not a recipe defect.

Unacceptable reasons: "test was flaky, please merge anyway,"
"validator failed but it works in production," or any reason that
asks the maintainer to extend trust beyond what the evidence shows.

## evidence/exempt Bypass Policy

The `evidence/exempt` label bypasses the recipe-evidence check
entirely. It exists for PRs that modify files under `recipes/` for
non-recipe reasons.

**Appropriate uses:**

- Mechanical refactors (file renames, comment-only changes, license
  header sweeps).
- Self-bootstrapping changes that wire up the evidence pipeline
  itself (the gate cannot gate its own bootstrapping PRs).
- Documentation edits that touch `recipes/` paths but no recipe
  semantics.

**Inappropriate uses:**

- "I don't have the hardware right now, please merge." That's what
  evidence/exempt looks like in misuse — the contributor needs to
  either get evidence from somewhere, or wait. Maintainers MUST NOT
  apply the label to skip an inconvenient evidence check.
- Recipe value changes (image versions, constraint thresholds,
  overlay merge behavior) — these always need a fresh bundle.

**Required justification.** A PR carrying `evidence/exempt` must
include a sentence in the PR description explaining why the bypass
is appropriate. The label is logged in the PR event history and is
queryable via `is:pr label:evidence/exempt` for audit.

## 6-Month Audit Runbook

Quarterly or semi-annually, walk the merged-recipe history to confirm
that what merged is still verifiable. The walk:

1. **Enumerate recently-touched pointers.**

   ```bash
   git log --since='6 months ago' --diff-filter=AM \
     --name-only --pretty=format: \
     -- recipes/evidence/ | sort -u
   ```

2. **For each pointer, find the latest rev.**

   ```bash
   git log -1 --format='%H %s' -- recipes/evidence/<recipe>.yaml
   ```

3. **Re-verify against the current OCI artifact.**

   ```bash
   aicr evidence verify recipes/evidence/<recipe>.yaml
   ```

   Exit 0 confirms the bundle is still fetchable and the signature
   still chains.

4. **Rekor-only fallback when bundle bytes are gone.** If the OCI
   registry has been deleted or made private, the Rekor entry from
   the original signing still exists. Run:

   ```bash
   cosign verify-attestation --type=recipe-evidence/v1 \
     <bundle-oci-ref-from-pointer>
   ```

   A passing verify against Rekor confirms the bundle existed and
   was signed by the recorded identity, even if the bytes are no
   longer fetchable. Add a comment to the recipe noting the bundle
   is Rekor-only; consider mirroring to a maintained registry.

5. **Flag stale pointers.** Any pointer whose latest commit is older
   than 24 months is past the V1-documented re-cert age cutoff (see
   ADR-007 §"What V1 does not ship"). V1 has no automated cutoff;
   the bot lands when the first pointer ages past 12 months. For
   now, file an issue per stale recipe asking the contributor (or a
   replacement) to re-attest.

## `maintainers:` Block Routing (Post PR-D)

ADR-007 PR-D adds an optional `maintainers:` block to recipe
metadata. The block is a **routing surface**, not a merge-authority
surface.

**What it does:**

- Provides a durable contact for re-cert prompts and advisory
  revocations.
- Lets the recipe-evidence check ping the listed maintainer when the
  PR author is the listed maintainer (auto-ping).
- Gives the audit runbook above a deterministic place to file
  re-cert issues.

**What it does not do:**

- Confer merge authority. Recipe changes still flow through the
  normal AICR review process; the listed maintainer is one reviewer
  among many.
- Replace the signer identity. The cosign identity on the bundle is
  still the trust anchor; `maintainers:` is metadata for humans.

When PR-D lands, this section gets a worked example. Until then,
the block is unused.

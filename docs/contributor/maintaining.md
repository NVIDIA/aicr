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

### SDK API Compatibility

`make api-diff` compares the exported `pkg/client/v1` surface to the latest
stable release tag. It runs through `make qualify` and the qualification
workflow; additive changes are reported, while removals and incompatible type
changes fail the gate.

Local runs require the repository-pinned `apidiff` and `yq`; install them with
`make tools-setup`. They also require full tag history and a stable release tag
reachable from `HEAD`. The gate checks out that baseline in a temporary
detached worktree, so it leaves the current working tree unchanged but adds
checkout and filesystem I/O cost to `make qualify`.

The gate compares declarations exported by `pkg/client/v1`. Because `apidiff`
does not recursively compare an external named type reached through an alias,
the gate derives the repository-local named-type closure exposed by
`BundleConfig`, `BundleAttester`, `BundleArtifact`, `OIDCResolveOptions`, and
`CriteriaRegistry` from both the release baseline and current source. It then
compares only that baseline/current closure, including nested fields and method
signatures; unrelated exports in the evolving target packages remain filtered
out. Closure derivation and an out-of-sync transparent-alias root list both fail
the gate closed. Generic target aliases are supported only when they forward
every type parameter unchanged with identical constraints. Concrete,
transformed, and narrowed instantiations fail before `apidiff` because its
package-level report cannot distinguish one instantiation from the generic
origin.

To acknowledge an intentional break, first run `make api-diff`. Add a
baseline-scoped entry to `pkg/client/v1/api-diff-exceptions.yaml` containing the
reported baseline plus non-empty `issue`, `summary`, and `rationale` fields.
Exactly one acknowledgement entry is allowed per baseline. Copy every reported
incompatible line that begins with `- ` into `incompatible_changes`, omitting
the `- ` prefix. The `incompatible_changes` list must exactly and completely
match the command output; omissions and extras both fail the gate.

The acknowledgement authorizes a break only for the active baseline, so keep it
in place through the release that ships that break — it is what keeps the gate
green until the release tag lands. Once the tag advances the baseline, the entry
is obsolete and should be pruned. The gate is deliberately asymmetric about
this. When the diff against the new baseline is clean there is nothing for a
stale entry to authorize, so the gate only warns that the entry is prunable and
still exits successfully; this is what lets the release pipeline and open pull
requests stay green while the cleanup lands. When the diff is *not* clean, a
stale entry is a hard failure: an acknowledgement scoped to an older baseline
must never be accepted for a break against the current one. Prune the entry in a
follow-up change after the release; Git history retains the release-notes record
of the breaking change.

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
hourly and runs our own monitor, `tools/rekor-monitor`, against the **Rekor v2**
transparency log (where AICR release signing writes since
[#1650](https://github.com/NVIDIA/aicr/issues/1650)). In one job it checks two
things: that the log stays append-only (consistency), and that no entry appears
under AICR's release signing identity that a release did not produce (identity).

The monitor classifies every failure and the workflow branches on it, so infra
flakiness never pages like a security event. A tamper (consistency break) or
identity failure opens a security tracking issue that mentions the maintainers
and posts a Slack page. An operational failure (Sigstore/Rekor/TUF/GitHub-API
trouble) pages no one: a single red hourly job with no issue is a transient blip
that self-heals, and only after three consecutive failed runs does a calm
`area/ci` "degraded" issue open. That same degraded issue also covers a `degraded`
classification, which is different: the identity catch-up is not converging (the
log outpacing the bounded per-run scan). There the monitor completed every pass
and will not self-heal, so its issue body gives a concrete remediation (more scan
budget per run, or triage a held finding) rather than "wait for upstream". The job
still goes red on any failure, and a later clean run closes both the security and
degraded issues.

This protects the trust root every AICR consumer depends on: the release
binaries, the signed recipe catalog, and the container images all chain to that
one identity. When the workflow files a security issue, follow the triage steps
in the workflow file's header comment; an unrecognized identity hit should be
treated as potential OIDC/key compromise.

### Known-release correlation (why a release no longer pages)

The identity scan matches every entry under the release SAN, which includes
every legitimate release: each real release signs an entry under exactly that
identity, so a naive scan would page on every tag. To separate real releases
from an attacker, the workflow fetches the tags a real release actually signed
and passes them to the tool via `--known-tags-file`. The tool then suppresses
any identity match whose certificate SAN carries an `@refs/tags/<tag>` that is a
known signed tag, so only an entry for a tag **no release signed** alerts.

The correlation source is the **`on-tag.yaml` (release signing workflow) run
history** (`RELEASE_WORKFLOW_FILE`), not the repo's current tags or releases.
The SAN is `on-tag.yaml@refs/tags/<tag>`, so a tag-push run of that workflow is
the authoritative proof that a real release signed `<tag>`. Crucially, run
history **persists after a tag or release is deleted**, whereas `/tags` and
`/releases` do not: an ephemeral release candidate (`vX.Y.Z-rc1`, whose tag and
GitHub Release are cleaned up once the final ships) would otherwise reappear as
an unexplained identity hit even though it was a genuine signing. That exact
false positive is what [#1902](https://github.com/NVIDIA/aicr/issues/1902)
caught.

Runs count in any lifecycle state and with any conclusion, so the query filters
on neither. GitHub mints the SAN only while a run of the workflow is executing
at `refs/tags/<tag>`, which makes the run's *existence* at that ref the proof;
its lifecycle state is not part of the identity. Filtering on
`status=completed` hid a release for the ~32 minutes it takes to run even
though its signing jobs write entries ~20 minutes in, which is exactly where
the hourly scan lands: that is the false positive
[#2153](https://github.com/NVIDIA/aicr/issues/2153) caught on `v0.19.0`. It also
let a re-run of a job that signs nothing (a flaky post-publish deploy) revoke a
tag's allowlist entry by flipping the run back to `in_progress`. Widening to
in-flight runs does not weaken the control: under the old gate, anyone able to
push a tag could already allowlist it by waiting for the run to finish.

The correlation fetch reads the workflow name from the
`RELEASE_WORKFLOW_FILE` env var; `CERT_SUBJECT` is a
separate literal regex that names the same workflow, kept in sync by convention
(both sit in the env block with a "change both together" note). They are not
auto-derived from one value: building the anchored, regex-escaped `CERT_SUBJECT`
from the var in shell would be more error-prone than the drift it prevents, and
a mismatch fails safe anyway (the runs query 404s, surfaced as operational, not
a false page).

The suppression is fail-closed: an unknown tag or a malformed SAN still alerts,
an *empty* allowlist file disables suppression (so every match alerts), and a
*missing* allowlist file is a hard error that fails the run (exit 2, surfaced as
operational). A broken correlation input can never silently silence a real hit.

Two residual gaps are accepted, both requiring the attacker to also subvert the
signing path (not just forge a log entry). First, an attacker who re-signs an
*existing* release tag is suppressed, because that tag is on the allowlist.
Second, the allowlist keys on the existence of an on-tag run at
`refs/tags/<tag>` in **any state and of any conclusion**, not on proof that the
sign step itself ran: signing happens mid-run, so a real release whose later
step flakes still concludes `failure` (e.g. `v0.18.0` itself) and must stay on
the allowlist, which means gating on `conclusion == success` is not viable (it
would re-create the [#1902](https://github.com/NVIDIA/aicr/issues/1902) false
positive on genuine releases). A run that failed *before* the sign step
therefore still allowlists its tag. Closing both tightly needs a
per-signing-step or per-tag entry-count / provenance check (did the sign step
succeed; how many entries a known tag is expected to have), tracked as a
follow-up in
[#1887](https://github.com/NVIDIA/aicr/issues/1887).

### Why v2, and why identity monitoring is feasible now

Identity monitoring is a linear scan of every entry added to the log since the
last checkpoint, because Rekor's index cannot be queried by certificate SAN and
AICR's keyless release identity has no email or fixed public key to search on.
On the Rekor **v1** firehose that scan runs roughly 50x slower than the log
grows, so it can never keep up inside a bounded CI job: the earlier v1
identity config timed out on every run and never completed a single scan
([#1623](https://github.com/NVIDIA/aicr/issues/1623)). Rekor **v2** is
tile-based: bulk 256-entry reads let a single worker outpace the log, so the
identity scan is a cheap job that always finishes.

### Why our own tool, not the upstream reusable workflow

The upstream `sigstore/rekor-monitor` reusable workflow selects its Rekor API
version and discovers shards from Sigstore's **default** signing config,
`signing_config.v0.2.json`. That config lists only Rekor v1 and, per Sigstore's
[rekor-evolution](https://blog.sigstore.dev/rekor-evolution/) plan, keeps v1 as
the ecosystem default "for the foreseeable future". AICR opted into v2 **early**
via a separate TUF target, `signing_config_rekor_v2.v0.2.json` (see `pkg/trust`),
which the upstream tool never reads and exposes no flag to select. So pointing
it at a v2 shard URL just falls through to v1 and fails.

`tools/rekor-monitor` closes exactly that gap: it reads the v2 signing config
AICR actually signs against (`trust.ResolveSigningConfig`) and then reuses the
upstream rekor-monitor **library** packages for the security-critical work (tile
consistency proofs and identity search), so we do not reimplement
transparency-log verification. To inspect the current v2 shard the way the tool
resolves it:

```bash
go run ./cmd/aicr trust update --emit-signing-config signing-config.json
jq -er '[.rekorTlogUrls[] | select(.majorApiVersion == 2)] | sort_by(.validFor.start) | last | .url' signing-config.json
```

When Sigstore makes v2 the ecosystem default, `signing_config.v0.2.json` will
list the v2 shards, the upstream reusable workflow can monitor v2 directly, and
this tool can be retired. Until then upstream exposes no flag to point the
monitor at a non-default signing config (it always reads
`signing_config.v0.2.json`); a feature request for that would let early v2
adopters drop this tool.

### Checkpoint and first run

The monitor persists its cursor as the `rekor-v2-checkpoint` artifact between
runs (a deliberately fresh name, so the stale v1 `checkpoint` artifact from the
earlier design is simply ignored, no migration). The **first** run has no prior
checkpoint, so it establishes a baseline at the current v2 tree head and skips
the identity scan; every run after that scans only the newly-added window.
Entries predating the baseline are covered by release-time verification (the
`aicr verify` path), not by this monitor.

### Catching up across runs (large backlog windows)

The identity scan is linear in the window size, so a window that has not
advanced for a while (a multi-hour Sigstore/TUF outage, or a finding that
deliberately holds the cursor) can grow past what one run can scan inside the
pass deadline. Rather than re-scan (and time out on) the whole window every run,
the scan is **resumable**: each run scans (in `scanChunkSize` chunks) whatever
fits before a **soft time budget** expires -- it stops once the pass deadline is
within `scanBudgetHeadroom` -- and persists how far it got in a `<checkpoint>.scan`
companion carried in the same artifact. The time budget is the primary bound, so
catch-up adapts to scan speed (a slow run covers fewer entries, a fast run more)
and never overruns the deadline; `maxScanEntriesPerRun` is only an outer safety
ceiling on a single run. On a same-shard window the signed checkpoint advances
(and the companion resets) only once the scan reaches head; the other advance
paths (first-run baseline, an empty window, and a shard rotation) re-baseline and
are reported as such. So a large backlog is caught up over several hourly runs
while each run stays within budget, with no coverage gap for a same-shard window.
A partial catch-up run is a clean (exit 0) pass; its log line reads `catching up,
N entr(y/ies) remaining`. A finding halts catch-up at that chunk (it is
re-detected until triaged), so a partial-clean same-shard pass never coexists with
an open finding alert; the one exception is a shard rotation, which re-baselines
past any held finding (rare, yearly) and reports the abandoned prior-shard count.
This is what unblocks a backlog like the ~1.2M-entry window that followed the
[#1902](https://github.com/NVIDIA/aicr/issues/1902) correlation fix, where the
earlier single-pass scan timed out every run and never advanced.

A catch-up is only healthy if it converges. Each partial pass records its
`remaining` count in a second `<checkpoint>.stall` companion; if `remaining`
fails to decrease for `maxCatchUpStallRuns` consecutive passes (the log is growing
faster than the per-run scan), the run returns a `degraded` classification instead
of `clean`. That is a non-security failure, so it never pages like a compromise,
but it makes the run go red and — after the workflow's usual consecutive-failure
streak — opens the low-urgency degraded issue. The stall trend resets on any
checkpoint advance, so once catch-up resumes (or the log growth slows) the monitor
returns to `clean` on its own. This closes the gap where a permanently-behind
catch-up would otherwise report green indefinitely.

### Recovering a wedged checkpoint artifact

The cursor and its `.scan`/`.stall` companions travel in one GitHub artifact
(`rekor-v2-checkpoint`). A corrupt companion is self-healing on most paths (an
advance rewrites it), but a malformed `.scan` on the identity-scan path fails the
pass, and the `if: !cancelled()` upload re-publishes the bad artifact, so the next
run re-reads it: a wedge that only a human can clear. Symptom: consecutive
`operational` runs whose logs show a scan-progress parse error (`failed to parse
scan-progress file` / `scan progress exceeds window end`), not an upstream outage.

To recover, delete the poisoned artifact so the next run re-baselines from head:

```bash
# find the latest rekor-v2-checkpoint artifact from a main run
gh api "repos/NVIDIA/aicr/actions/artifacts?name=rekor-v2-checkpoint&per_page=100" \
  --jq '[.artifacts[] | select(.workflow_run.head_branch == "main")] | sort_by(.created_at) | last | {id, created_at}'
# delete it (replace <id>)
gh api -X DELETE "repos/NVIDIA/aicr/actions/artifacts/<id>"
```

Coverage cost: re-baselining skips identity-scanning the window between the last
good checkpoint and the current head (consistency is unaffected). That gap is the
same one a first run has, and is acceptable for recovery; note it if the skipped
window is large.

### Shard rotation (and what the operator sees)

Shard rotation (`log2025-1` -> `log2026-1` -> ...) needs no config change here:
the tool reads the live shard set from the signing config every run. It does,
however, leave a small, **intentionally visible** identity-scan gap that a
maintainer should recognize in the run logs:

- **On the first pass after rotation**, the previous checkpoint is on the old
  shard and the current one is on the new shard (different logs), so there is no
  meaningful cross-shard window. The monitor **re-baselines on the new shard**
  and logs `shard rotation detected: ... re-baselining ...`. Entries appended to
  the old shard just before rotation, and new-shard entries before the
  re-baseline, are not identity-scanned this pass (the vendored `IdentitySearch`
  only reads the latest shard).
- **If the new shard is still empty** when the monitor first sees it, the
  size-0 checkpoint is not persisted (`WriteCheckpointRekorV2` skips size-0
  writes), so the pass collapses to a normal first run and logs `baseline
  established at tree size N (first run; identity scan skipped)` once the shard
  has entries. Those `[0, N-1]` entries are the standard forward-looking
  first-run gap.

In both cases the un-scanned entries are covered by **release-time verification**
(`aicr verify` runs against each release's own bundle), so this is a
monitoring-coverage gap, not a verification gap. A follow-up may add a one-time
new-shard backfill; until then, treat a rotation log line as a prompt to
spot-check releases made around the rotation boundary.

### Daily release re-verification

`Release Re-Verification` (`.github/workflows/release-reverify.yaml`) runs daily
and answers the question the Rekor monitor cannot: did the release we published
actually **ship** the artifacts a consumer needs to verify it? The monitor proves
the log is sound; it says nothing about a signing-side upload that silently
failed or was skipped, which leaves a published release whose provenance cannot
be reconstructed while nothing in the log is wrong
([#1461](https://github.com/NVIDIA/aicr/issues/1461)).

It also declares `workflow_dispatch`, which matters operationally: GitHub
disables scheduled workflows after 60 days of repository inactivity, so a manual
dispatch is how a maintainer re-arms the schedule. It is also the way to get an
on-demand run right after cutting a release, rather than waiting for the next
day's cron to be the first thing that looks at the new artifacts.

Each run resolves the latest non-draft, non-prerelease release (never a hardcoded
tag; the resolved tag is written to the job summary) and runs the **shipped**
verification commands against it, exactly as
[`supply-chain-verification.md`](../integrator/supply-chain-verification.md)
documents them:

- the `linux/amd64` archive against `aicr_checksums.txt`, then its
  `aicr-attestation.sigstore.json` SLSA provenance bundle via
  `cosign verify-blob-attestation` pinned to `on-tag.yaml@refs/tags/<exact tag>`.
  This is delegated wholesale to the `install-aicr-release` composite that UAT
  release cells already use, so there is one hardened implementation of the
  binary check, not two. When that composite fails, the classifier re-runs
  **both** of its sub-checks (the checksum and the provenance) rather than
  presuming which one broke: the attestation binds the *binary* while
  `aicr_checksums.txt` covers the *archive*, so a manifest that stopped matching
  an otherwise-valid archive would otherwise report "transient" every day while
  every consumer following the documented checksum flow fails every time;
- `recipe-catalog.sigstore.json` (a loose asset, deliberately outside
  `aicr_checksums.txt`) via `aicr recipe verify-catalog`, run with the released
  binary just verified — the only check that proves the *shipped* binary's
  embedded `registry.yaml` and `validators/catalog.yaml` still digest to what the
  release signed;
- the `linux/amd64` SPDX SBOM every release must publish for each binary in
  `EXPECTED_SBOM_BINARIES`, and each one's sibling `.sigstore.json` bundle. The
  expected set is derived from the **tag**, mirroring
  `expected_release_asset_names()` in `.github/scripts/release-images.sh`, never
  from the release's own inventory: derived from the release, only a zero-SBOM
  release would be a finding, and deleting one of the two would leave the check
  verifying the survivor and reporting clean. Gated on `SBOM_SIGNING_FLOOR`
  (`v0.18.0`): releases at or before it predate SBOM signing
  ([#1957](https://github.com/NVIDIA/aicr/issues/1957)) and legitimately ship
  unsigned SBOMs, while every later release must ship a bundle per SBOM.

Classification reuses the monitor's vocabulary and exit codes so both workflows
triage identically: `clean` (0), `tamper` (1, security), `operational` (3). A
`tamper` finding opens a security issue **and** posts a Slack alert through the
same `SLACK_SERVICE` webhook rekor-monitor uses, so an incident-grade finding
does not depend on one channel. Operational failures page no one, and only three
consecutive failed scheduled runs open a calm `area/ci` degraded issue. Only a
`failure` conclusion counts toward that streak: a canceled or timed-out run says
nothing about upstream health.

**The alert is tag-scoped, and that is a deliberate divergence from
rekor-monitor.** The issue title carries the resolved tag, and a clean run only
closes the alert for the tag it actually verified. The job checks the latest
release, so a clean run on `vX+1` proves nothing about `vX`; closing `vX`'s issue
would silently resolve a live finding on a release that is never re-checked
again. The degraded issue has no such scoping, because it tracks the checker's
own health rather than a release, so any clean run clears it. rekor-monitor's
alert is release-agnostic (it tracks the log), which is why a clean run genuinely
clears it there.

**`tamper` is asserted only on positive evidence**, never as a fallback, so an
outage cannot masquerade as a missing entry. "Missing" means an asset name is
absent from the release's own asset inventory, or an attestation is absent from
an archive that already downloaded and checksummed. Neither is reachable from a
failed network call: a failed read aborts the step before any comparison, and an
inventory file that cannot be read at all demotes and stops rather than reporting
every asset as missing. A failed *cryptographic* check is promoted to `tamper`
only when every demotion test declines it:

- the command was not killed (`timeout` exit 124, or 137 from a SIGKILL or the
  OOM killer);
- it produced a non-empty, readable log: `grep` declines to match an empty file
  and exits 2 on an unreadable one, either of which would otherwise sail straight
  through the pattern guard;
- every Sigstore liveness probe answered. Both the TUF CDN and Fulcio are
  probed and **all** must respond, so a Fulcio-only outage demotes too. Rekor v2
  shard hostnames rotate, so no fixed shard is probed;
- the captured output carries no transport or outage signature.

All of these are demote-only, so the worst case is a real finding reported as
operational: still a red job, still a degraded issue after three days, and it
re-fires the next day. Never the reverse. The cost of that bias is that a probe
URL which broke permanently would silently disable paging, so each probe failure
is logged by name. Any failure *before* the classifier runs leaves the
classification empty, which the gates treat as operational by construction.

One fault in the step fails in the **opposite** direction and is guarded
separately: if `sort -V` cannot order the tag against the SBOM signing floor, an
unguarded comparison would read as "at or before the floor" and skip every SBOM
check while logging that it did so on purpose. That is silent under-verification
rather than a false page, and it is demoted explicitly.

Two things are deliberately out of scope, both tracked as follow-ups:

- **Container-image OCI referrer attestations** (SBOM / OpenVEX / SLSA
  provenance, [#1982](https://github.com/NVIDIA/aicr/issues/1982)). Those live in
  ghcr.io's referrer store — a different system with a different retention and GC
  model from GitHub Releases — and re-verifying seven images times five
  attestations each (provenance on the index, plus a CycloneDX SBOM and an
  OpenVEX document on each of the two platform manifests) would add roughly
  thirty-five registry round-trips per run, multiplying operational noise
  against the one signal this job exists to keep crisp. The images are already pulled and scanned weekly by
  `vuln-scan-images.yaml`. A registry-side sibling check must use
  `gh attestation verify --bundle-from-oci`, otherwise it reads GitHub's
  attestations API and proves nothing about the registry copy's retrievability.
- **Rekor entry liveness by log index.** `cosign verify-blob-attestation`
  verifies a self-contained bundle: the inclusion proof and RFC3161 timestamp
  travel inside it and are checked against the live Sigstore trust root. A pass
  proves the bundle is retrievable and cryptographically sound, not that Rekor
  would still serve that entry by index.

Triage is in the workflow file's header comment. In short, a security issue names
the exact artifact:

- **Absent asset**: a signing or upload step silently skipped. Re-upload and
  re-sign the asset, or re-cut the release.
- **Present asset that fails verification**: read the **verifier's reported
  failure reason** before concluding anything. `cosign verify-blob-attestation`
  fails on a certificate-identity or predicate-type mismatch just as it does on a
  digest mismatch, so an asset re-signed under a different workflow identity, with
  its bytes fully intact, produces the same red as tampering. A digest mismatch
  means the published bytes are not what the release signed and is an incident;
  an identity or predicate mismatch is a signing-path problem, and the remediation
  is different.

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
   `.exit` from `aicr evidence verify --format json`. A structured `exit: 3` is **not** a verdict on
   the bundle — verification never reached one. `failureCause.class:
   transient` (OS exit code 5) means the bundle was not readable (dead mount,
   unreachable registry); re-run the check. `failureCause.class: canceled`
   (OS exit code 9) means the run was deliberately aborted. In neither case
   should the contributor be asked to change anything.
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

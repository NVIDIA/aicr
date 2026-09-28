---
name: aicr-managing-openvex
description: |
  Use when adding, updating, or removing CVE/GHSA suppressions in
  `.openvex.json` — the OpenVEX document consumed by the weekly image
  vulnerability scan workflow. Triggers on "VEX", "OpenVEX",
  ".openvex.json", "suppress CVE", "ignore CVE", "vulnerability
  suppression", "aiperf-bench CVE", or any request to act on findings
  reported by `Weekly Image Vulnerability Scan` for the aiperf-bench
  image. Keeps the file current: adds reachability-evidenced statements
  for new HIGH+ findings, drops statements that no longer apply
  (dependency upgraded past the fix, advisory recalled, package
  removed), and verifies suppressions actually land in the JSON output.
  Reads `vex-state.yaml` beside this skill at the start of every
  invocation and updates it at the end: it carries the deferred
  verifications, known negatives, and deliberate exclusions that one
  invocation has to hand the next.
---

# Managing `.openvex.json`

`.openvex.json` carries per-CVE reachability evidence used to suppress
vulnerability findings in the aiperf-bench container image. The file is
consumed by the `Weekly Image Vulnerability Scan` workflow
(`.github/workflows/vuln-scan-images.yaml`) via the `vex:` input on
`anchore/scan-action@v7.4.0`, which passes it to grype as `--vex
.openvex.json`.

It is also the source for the OpenVEX attestation each release publishes on
every platform manifest. `.github/actions/sbom-and-attest` validates this file,
then runs `tools/openvex-bind` to rewrite each statement's bare
`pkg:oci/<image>` products to `pkg:oci/<image>@sha256:<platform-digest>` and
signs that projection. Edit this file only: the published document is
generated, never committed. Three consequences for edits here: a statement whose
product PURL does not name a released image is silently dropped from every
projection; statuses, justifications and impact statements are published
verbatim to a public registry, signed, on every released image; and
document-level `tooling` is not published at all, because the projection
replaces it with an identifier of the generator (invariant 5).

This skill exists because the file has *non-obvious* invariants — most
notably the product-PURL matching rule — and getting them wrong silently
no-ops every statement in the document.

## When to use

- A `Weekly Image Vulnerability Scan` run reports HIGH+ CVE(s) on the
  aiperf-bench image and a maintainer needs to add a suppression after
  verifying reachability.
- A maintainer bumps the aiperf pin (`AIPERF_VERSION` in
  `validators/performance/aiperf-bench.Dockerfile`) or its dependency
  pins, fixing a CVE that was previously suppressed → the entry must be
  removed.
- A maintainer audits the file before a release to drop stale entries.
- The scan workflow shows non-zero HIGH+ counts but VEX is "supposed to
  cover them" — typically a PURL or vulnerability-ID mismatch.

## Non-negotiable invariants

These are the rules that, when violated, cause silent suppression
failures. Verify each one before claiming a statement is correctly
applied.

### 1. `products[].purl` must equal the grype image PURL

Grype derives the OCI image PURL from the **registry repository
basename**, not from `org.opencontainers.image.title`. For the aiperf-bench
image:

- CI scans `ghcr.io/nvidia/aicr-validators/aiperf-bench:<tag>` →
  grype PURL `pkg:oci/aiperf-bench`.
- A local build tagged `aicr-aiperf-bench:test` (matching the title
  label) → grype PURL `pkg:oci/aicr-aiperf-bench`.

Every statement in this repo therefore carries **both** product entries:

```json
"products": [
  { "@id": "pkg:oci/aicr-aiperf-bench", "identifiers": { "purl": "pkg:oci/aicr-aiperf-bench" } },
  { "@id": "pkg:oci/aiperf-bench",      "identifiers": { "purl": "pkg:oci/aiperf-bench" } }
]
```

If you add a statement, include both. If you rename the image or add a
new image to the VEX scope, derive the new PURL by repeating the local
reproduction below and checking `.source.target.userInput` against the
generated PURL — do not guess from labels.

### 2. `vulnerability.name` must equal grype's primary ID

Grype emits a single primary ID per match (the `.vulnerability.id` field
of `.matches[]`). For ecosystem advisories with both a GHSA and a CVE,
the primary ID is usually the **GHSA**; the CVE shows up only as a
`relatedVulnerabilities[].id` alias. OpenVEX matching is by exact name —
a CVE in the VEX file will not match a GHSA primary ID even though they
describe the same advisory.

Use the ID that appears in the `HIGH+:` line of the scan artifact /
Slack notification (which prints `<pkg> <primary-id> (<aliases>)`), or
extract it directly from the JSON:

```bash
jq -r '.matches[] | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
       | "\(.artifact.name) \(.vulnerability.id) (\(.relatedVulnerabilities|map(.id)|join(",")))"' \
  <(grype <image> --only-fixed -c .grype.yaml --vex .openvex.json -o json)
```

### 3. Justifications must use the OpenVEX v0.2.0 enum

Allowed values for `not_affected` status:

- `component_not_present` — package isn't in the image at all.
- `vulnerable_code_not_present` — package is in the image but the
  specific vulnerable symbol/file/build is absent (e.g., conditionally
  compiled out, removed in the shipped version).
- `vulnerable_code_not_in_execute_path` — code exists but the workload
  never invokes it.
- `vulnerable_code_cannot_be_controlled_by_adversary` — code is
  reachable but inputs are not attacker-influenced.
- `inline_mitigations_already_exist` — runtime hardening (seccomp,
  caps drop, etc.) blocks the trigger.

`vulnerable_code_not_in_execute_path` is the most common choice for
this image; `vulnerable_code_not_present` is used when the symbol is
conditionally compiled out (e.g., Windows-only APIs in a Linux glibc).

### 4. `impact_statement` must cite concrete evidence

Every statement requires a substantive `impact_statement` — not a
hand-wave. Reviewers and downstream consumers (auditors, customers
reading SBOMs) read this. Cite at least one of:

- Specific grep against aiperf source that returns zero hits, with the
  pattern shown (e.g., `grep -rn -E '^(import|from) (gzip|lzma|bz2)'`).
- Specific file path in the image / aiperf source that proves a
  feature is gated off (e.g., `aiperf/plot/dashboard/server.py` is only
  reached via the `aiperf plot` subcommand).
- Specific Dockerfile clauses that establish the hardening claim
  (USER, capabilities, base-image choice).
- Upstream advisory text that limits the trigger to a config we don't
  use.

See existing statements for the expected density; CI does not enforce
this but reviewers will.

### 5. Document-level fields are short metadata, not prose

OpenVEX v0.2.0 defines exactly nine document-level fields: `@context`,
`@id`, `author`, `role`, `timestamp`, `last_updated`, `version`,
`tooling` and `statements`. Each is short, structured metadata -- an IRI,
an author or role string, an RFC 3339 timestamp, an integer, a generator
identifier -- and `statements` is the only one that carries substance.
**None of them is a free-form prose field, and there is no notes or
changelog field.**

So anything not scoped to a single CVE has no home in the document at
all: what a revision changed and how it was verified goes in that
revision's PR description, and work that is not finished yet goes in
`vex-state.yaml` (see **Carried state** below).

`tooling` is where this went wrong. Each revision appended its own
rationale to it because it was the only free-form string available, and
it reached 8,010 bytes. Since v0.21.0 every release signs it onto all
seven images, six of which project to zero statements, so the field was
publishing one image's dependency triage as a claim about the others
(NVIDIA/aicr#2706). Two mechanisms now stop a repeat, and both are
load-bearing rather than advisory:

- `tools/openvex-bind` replaces `tooling` with a fixed identifier of
  itself in every projection, so a committed value cannot be published.
- `.github/actions/sbom-and-attest/openvex-guard.sh` rejects any
  document-level string over 256 bytes, in both source and projection
  mode. A `tooling` that regrows fails the release, and
  `TestReleaseOpenVEXValidation` in `tests/releasepolicy` fails the PR
  first.

Per-statement `version` and `last_updated` are deliberately unused too.
`version` numbers a statement's own revisions, not the document revision
it was introduced at, so either reading is a field no consumer reads and
the next editor has to remember. `last_updated` would always equal the
document `timestamp`, because the stale audit below re-verifies every
statement on every edit.

## Where rationale goes

| Kind of rationale | Destination |
|---|---|
| Why this CVE cannot be exploited, and the evidence for it | that statement's `impact_statement` (with `vulnerability.description` for the advisory summary) |
| What this revision changed, why, what it retired, and how it was verified | the PR description |
| A check that can only run later, a negative result nobody should repeat, a decision not to act | `vex-state.yaml` beside this skill |
| Anything else | nowhere: if it is worth keeping it is one of the three above |

The PR description is the record of a revision, not a copy of one: it is
reviewable, linkable, carries the scan-run references, and needs no
maintenance afterwards. Do not restate it in the document, and do not
open a changelog file for it.

`vex-state.yaml` is the opposite thing, and the distinction is what keeps
both small. It holds *unfinished* work, not history, and every entry
states what would invalidate it so it can be deleted when that happens.
The next section is its contract.

## Carried state (`vex-state.yaml`)

Some of this work does not finish inside one session. A statement's real
confirmation is a CI run that has not happened yet; a bump you checked and
found unavailable will be worth re-checking only when upstream moves; a
decision *not* to suppress something is invisible to whoever reads the
document next. None of that fits in a VEX statement, and writing it into
the document is what grew `tooling` to 8,010 bytes.

`.agents/skills/aicr-managing-openvex/vex-state.yaml` carries it instead.
**Read it first, update it last**, on every invocation:

1. **At the start**, work through `deferred_verifications`. For each,
   run the check it names. If it passes, delete the entry and say so in
   the PR description. If it fails, that failure is the finding you are
   now triaging, ahead of whatever brought you here.
2. **Before triaging a new finding**, check `known_negatives`. If the
   bump you are about to evaluate is already recorded as unavailable and
   nothing named in `invalidated_by` has happened, do not re-derive it.
3. **Before deleting or adding a statement**, check
   `deliberate_exclusions`. A finding that is deliberately unsuppressed
   must stay unsuppressed until its `invalidated_by` condition fires.
4. **At the end**, add entries for anything you could not finish: a check
   that needs a later run, a negative you would hate to redo, a decision
   not to act. Every entry names what would invalidate it. If you cannot
   name that, the note belongs in the PR description instead.

The lifecycle is the whole point. Entries are deleted when they resolve,
not rewritten as history: a file that only ever grows is the failure this
replaced. Evidence caveats ride on the entry they qualify. A deletion
validated against a scan of the new base rather than against rebuilt
bytes is a weaker claim than it looks, so it is recorded on the deferred
verification that will settle it, and both go when the scan confirms.

## Local reproduction (canonical)

The only way to be certain a statement applies is to run the same
grype invocation CI runs and confirm the finding moves from `.matches[]`
to `.ignoredMatches[]`. The recipe:

```bash
# 1. Build the image locally with the title label the workflow sets
docker buildx build \
  --load \
  --platform linux/amd64 \
  -f validators/performance/aiperf-bench.Dockerfile \
  -t aicr-aiperf-bench:test \
  --label "org.opencontainers.image.title=aicr-aiperf-bench" \
  .

# 2. Install the exact grype version the workflow pins
#    (lives in GrypeVersion.js of anchore/scan-action@v7.4.0)
GRYPE_VERSION=v0.110.0  # cross-check with .github/workflows/vuln-scan-images.yaml
gh release download "${GRYPE_VERSION}" --repo anchore/grype \
  --pattern "grype_*_darwin_arm64.tar.gz" -O /tmp/grype.tgz
tar -xzf /tmp/grype.tgz -C /tmp grype && mv /tmp/grype /tmp/grype-vex

# 3. Reproduce the CI scan flags exactly
/tmp/grype-vex aicr-aiperf-bench:test \
  --fail-on high --only-fixed --vex .openvex.json -c .grype.yaml \
  -o json --file /tmp/scan.json

# 4. Inspect what survived (these MUST be empty for a passing scan)
jq '[.matches[] | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
     | {id: .vulnerability.id, pkg: .artifact.name}]' /tmp/scan.json

# 5. Confirm the suppression landed. NOTE: ignoredMatches entries carry
#    `vulnerability` at the top level (NOT under `.match`) in grype 0.110.
jq '[.ignoredMatches[]? | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
     | {id: .vulnerability.id, rules: .appliedIgnoreRules}]' /tmp/scan.json
```

A new statement is correct **only** when step 4 returns `[]` for the
vulnerability it targets and step 5 lists it under `appliedIgnoreRules`
with `namespace = "vex"`.

### Shortcut: scan CI's exact images instead of building

The scan workflow builds and pushes every image with tag
`scan-<full-head-sha>` before scanning. Those tags stay on GHCR, so you
can scan the **exact bytes CI scanned** — all seven matrix images, not
just aiperf-bench — without a local docker build:

```bash
SHA=$(gh run list -R NVIDIA/aicr --workflow vuln-scan-images.yaml \
      --limit 1 --json headSha --jq '.[0].headSha')
/tmp/grype-vex "ghcr.io/nvidia/aicr-validators/aiperf-bench:scan-${SHA}" \
  --only-fixed --vex .openvex.json -c .grype.yaml -o json --file /tmp/scan.json
```

Use this for triage and the stale audit (it covers `aicr-gate` and
`aicr`, which have no local Dockerfile build path). Use the docker-build
recipe above only when validating a Dockerfile change before it is
pushed. Caveat: a local grype DB newer than this morning's CI run can
surface advisories CI hasn't seen yet — treat those as *incoming*
findings, not discrepancies.

## Triage a new finding from the scan workflow

The weekly scan (Thursdays, 06:00 UTC) emits HIGH+ identifiers in the
per-image artifact and
Slack notification:

```
aiperf-bench: 0 critical, 2 high, 6 medium, 0 low, 0 negligible (10 VEX-suppressed)
  HIGH+: pillow GHSA-pwv6-vv43-88gr (CVE-2026-42311), pillow GHSA-whj4-6x5x-4v2j (CVE-2026-40192)
```

Before the per-ID work, clear `vex-state.yaml`: resolve any outstanding
`deferred_verifications` and re-read the `known_negatives` and
`deliberate_exclusions` that bear on this image.

For each ID:

1. **Check upstream first.** Read the GHSA / NVD page. If a fix has
   shipped in a version reachable from aiperf's pins, the right action
   is usually *not* a VEX entry — it's bumping the aiperf pin so the
   fix lands and the finding disappears. Bump
   `AIPERF_VERSION` in `validators/performance/aiperf-bench.Dockerfile`,
   verify with the local repro above, and skip the rest of this section.
2. **If a bump isn't feasible**, prove non-reachability. The work that
   must be visible in `impact_statement`:
   - Identify the vulnerable function / file in upstream source.
   - Check whether aiperf imports it (`grep -rn` patterns).
   - Check whether the workload (`aiperf profile <text-llm>` invoked
     by `validators/performance/inference_perf_constraint.go`) reaches
     the code path even transitively.
   - Note any base-image constraint (e.g., `python:3.13-slim` is
     Debian trixie / glibc / Linux only, so Windows-only and
     glibc-only-on-certain-locales conditions are inert).
3. **Author the statement** with both PURLs (see invariant 1), the
   correct primary ID (see invariant 2), a v0.2.0 justification (see
   invariant 3), and concrete evidence (see invariant 4).
4. **Reproduce locally**, confirm step-4 returns `[]` for the new ID.
5. **Run the stale audit** (next section) — every edit to the file MUST
   include it, so dead statements never accumulate alongside new ones.
6. **Commit and dispatch the workflow** to confirm CI matches local.
   Run `gh workflow run "Weekly Image Vulnerability Scan" --repo
   NVIDIA/aicr --ref main`, watch with `gh run watch <id> --exit-status`,
   inspect the aiperf-bench scan-result artifact.
7. **Record what you could not confirm.** Some checks are only possible
   after merge: the scan that runs against the rebuilt image, or the
   attestation the next release publishes. Do not assert them in prose
   anywhere; add a `deferred_verifications` entry to `vex-state.yaml`
   naming the check and how to run it, so the next invocation resolves
   and deletes it.

## Stale audit (MANDATORY on every edit)

Statements rot: dependencies get upgraded past fixes, advisories get
withdrawn, components leave the image. A stale statement is invisible —
it applies to nothing, silently — so the audit runs on **every** change
to the file, not just before releases. (The audit that introduced this
rule found 12 dead statements out of 70.)

Scan every image the document has product entries for (currently
`aiperf-bench` alone; the `aicr-gate` and `aicr` statements were retired
at revisions 12 and earlier) using the `scan-<sha>` shortcut above, then
diff declared statements against applied rules:

```bash
# Applied: unique vuln IDs suppressed via the vex namespace
jq -r '[.ignoredMatches[]? | select((.appliedIgnoreRules//[]) | any(.namespace=="vex"))
        | .vulnerability.id] | unique[]' /tmp/scan-<image>.json | sort > /tmp/applied.txt

# Declared: statement names scoped to that image's product PURL
jq -r '.statements[] | select([.products[]["@id"]] | any(test("<image>")))
       | .vulnerability.name' .openvex.json | sort > /tmp/declared.txt

comm -23 /tmp/declared.txt /tmp/applied.txt   # stale candidates
```

For each candidate, classify before deleting — three distinct cases:

1. **Gone entirely** (`grep <id> /tmp/scan-<image>.json` → 0 hits,
   including aliases): the finding no longer exists (package upgraded
   past the fix, advisory withdrawn). **Delete.**
2. **Present but ignored by `fix-state: wont-fix`** (appears in
   `.ignoredMatches[]` with `appliedIgnoreRules[].namespace == ""`):
   `--only-fixed` already hides it, so the VEX statement never applies.
   **Delete** — do not keep it "just in case": if the distro ships a
   fix, the weekly image rebuild absorbs it automatically, and until it
   does the finding must surface rather than be pre-suppressed (a fix
   that becomes reachable means bump, not VEX).
3. **Present in `.matches[]` under a different primary ID** (a CVE
   statement while grype emits the GHSA, or vice versa): NOT stale — a
   name mismatch. Fix `vulnerability.name` per invariant 2.

After deleting, re-run the scans for **all** covered images and confirm
the vex-suppressed counts still match the latest CI run for the
statements that remain (deleting must be count-neutral).

The workflow counts suppressed **rows**, not statements: a statement
whose CVE matches two packages counts twice. Revision 14's 18 statements
produced 25 rows (7 openssl CVEs on both `libssl3t64` and
`openssl-provider-fips`, plus 11 single-row pillow/aiohttp GHSAs);
revision 15's 11 statements produce 11. Predict the row count before
reading the scan, or a correct run looks like a discrepancy.

Bump the document `version` and refresh `timestamp` in the same edit, and
write what changed, what it retired, and how you verified it into the PR
description (invariant 5). That is the record: nothing about the revision
goes into the document itself.

## Anti-patterns

- **Using `pkg:oci/<image-title>` when CI scans `pkg:oci/<repo-basename>`.**
  The label has no effect on grype's image PURL. Always include the
  registry-basename form.
- **Using a CVE ID in `vulnerability.name` when grype emits a GHSA primary.**
  The two names are NOT interchangeable for OpenVEX matching.
- **Suppressing a CVE the dependency upgrade would have fixed.** VEX is
  for findings that *cannot* be remediated by upgrading; if the fixed
  version is reachable, bump the pin instead.
- **Boilerplate `impact_statement` ("not exploitable", "low risk").**
  Cite the specific code path, file, or upstream language that supports
  the claim. Reviewers will reject thin justifications.
- **Forgetting to refresh `timestamp` and `version` at the document
  level when materially changing statements.** Bump
  `version` on each substantive edit and update `timestamp` (or
  `Reviewed:` notes) so downstream consumers can detect drift.
- **Adding a statement without local reproduction.** A statement that
  fails to apply is invisible — there is no warning, no failure, no log
  line. The only signal is that the CVE keeps appearing in scans. Always
  run the local repro before committing.
- **Appending this revision's narrative to `tooling`, or to any other
  document-level field.** `tooling` names what generated the document;
  it is not a changelog. Appending to it is how it reached 8,010 bytes of
  prose that every release signs onto every image. Per-CVE reasoning goes
  in the statement, unfinished work in `vex-state.yaml`, and everything
  else in the PR description. The guard now rejects the attempt, but the
  point is that there was always a better place for it.
- **Dropping an evidence caveat because the statement it qualified is
  being deleted.** "Validated against the base image, not a rebuild" is a
  limitation on the claim. Record it on the `vex-state.yaml` entry for
  the scan that will settle it, and in the PR description that performs
  the deletion.
- **Leaving a resolved entry in `vex-state.yaml` "for the record".** A
  confirmed verification is finished; the record is the PR that confirmed
  it. Entries that are never removed are exactly how `tooling` grew.

## Quick reference

- Workflow: `.github/workflows/vuln-scan-images.yaml`
- VEX document: `.openvex.json`
- Carried state (deferred checks, known negatives, exclusions):
  `.agents/skills/aicr-managing-openvex/vex-state.yaml`
- Release publication: `.github/actions/sbom-and-attest/action.yml` +
  `tools/openvex-bind` (digest binding, `tooling` normalization) +
  `.github/actions/sbom-and-attest/openvex-guard.sh` (contract and size
  bound)
- Grype config (excludes for source scans only): `.grype.yaml`
- Image source: `validators/performance/aiperf-bench.Dockerfile`
- aiperf pin: `AIPERF_VERSION` ARG in that Dockerfile
- Grype version pin (read from scan-action): `GrypeVersion.js` at the
  pinned scan-action SHA in the workflow
- Workflow output format (per image, in scan-N artifact):
  ```
  <short-name>: N critical, N high, N medium, N low, N negligible (N VEX-suppressed)
    HIGH+: <pkg> <primary-id> (<aliases>), ...
  ```

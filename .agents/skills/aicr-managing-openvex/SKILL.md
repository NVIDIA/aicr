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
generated, never committed. Two consequences for edits here: a statement whose
product PURL does not name a released image is silently dropped from every
projection, and statuses, justifications and impact statements are published
verbatim to a public registry.

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

## Stale audit (MANDATORY on every edit)

Statements rot: dependencies get upgraded past fixes, advisories get
withdrawn, components leave the image. A stale statement is invisible —
it applies to nothing, silently — so the audit runs on **every** change
to the file, not just before releases. (The audit that introduced this
rule found 12 dead statements out of 70.)

Scan every image the document has product entries for (currently
`aiperf-bench`, `aicr-gate`, `aicr`) using the `scan-<sha>` shortcut
above, then diff declared statements against applied rules:

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

Bump the document `version` and refresh `timestamp` in the same edit.

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

## Quick reference

- Workflow: `.github/workflows/vuln-scan-images.yaml`
- VEX document: `.openvex.json`
- Release publication: `.github/actions/sbom-and-attest/action.yml` +
  `tools/openvex-bind` (digest binding)
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

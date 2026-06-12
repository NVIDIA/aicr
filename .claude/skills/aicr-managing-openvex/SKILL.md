---
name: aicr-managing-openvex
description: |
  Use when adding, updating, or removing CVE/GHSA suppressions in
  `.openvex.json` — the OpenVEX document consumed by the daily image
  vulnerability scan workflow. Triggers on "VEX", "OpenVEX",
  ".openvex.json", "suppress CVE", "ignore CVE", "vulnerability
  suppression", "aiperf-bench CVE", or any request to act on findings
  reported by `Daily Image Vulnerability Scan` for the aiperf-bench
  image. Keeps the file current: adds reachability-evidenced statements
  for new HIGH+ findings, drops statements that no longer apply
  (dependency upgraded past the fix, advisory recalled, package
  removed), and verifies suppressions actually land in the JSON output.
---

# Managing `.openvex.json`

`.openvex.json` carries per-CVE reachability evidence used to suppress
vulnerability findings in the aiperf-bench container image. The file is
consumed by the `Daily Image Vulnerability Scan` workflow
(`.github/workflows/vuln-scan-images.yaml`) via the `vex:` input on
`anchore/scan-action@v7.4.0`, which passes it to grype as `--vex
.openvex.json`.

This skill exists because the file has *non-obvious* invariants — most
notably the product-PURL matching rule — and getting them wrong silently
no-ops every statement in the document.

## When to use

- A `Daily Image Vulnerability Scan` run reports HIGH+ CVE(s) on the
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

# 5. Confirm the suppression landed
jq '[.ignoredMatches[]? | select(.match.vulnerability.severity == "High" or .match.vulnerability.severity == "Critical")
     | {id: .match.vulnerability.id, rules: .appliedIgnoreRules}]' /tmp/scan.json
```

A new statement is correct **only** when step 4 returns `[]` for the
vulnerability it targets and step 5 lists it under `appliedIgnoreRules`
with `namespace = "vex"`.

## Triage a new finding from the scan workflow

The daily scan emits HIGH+ identifiers in the per-image artifact and
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
5. **Commit and dispatch the workflow** to confirm CI matches local.
   Run `gh workflow run "Daily Image Vulnerability Scan" --repo
   NVIDIA/aicr --ref main`, watch with `gh run watch <id> --exit-status`,
   inspect the aiperf-bench scan-result artifact.

## Drop a stale statement

Statements rot. Drop them when any of these hold:

- The package has been upgraded past the fixed version. Grype's
  `--only-fixed` means the finding disappears, so the VEX statement is
  dead weight. Verify with: `jq '.statements[] | select(.vulnerability.name ==
  "<id>") | .impact_statement'` and cross-check the pinned dependency
  version in the image.
- Upstream withdrew or downgraded the advisory.
- The component was removed from the image (e.g., base image swap).

To check which statements are still *applied* in the latest scan,
diff the VEX statement names against the `appliedIgnoreRules` in
`.ignoredMatches[]` of a fresh scan JSON. A statement that produces
zero applied rules across a full scan is a candidate for removal —
but confirm it's not because of a PURL/ID typo before deleting.

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

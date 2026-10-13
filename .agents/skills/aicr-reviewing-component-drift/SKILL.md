---
name: aicr-reviewing-component-drift
description: |
  Use when reviewing the weekly AICR component drift report — the Slack digest
  and drift-report.json artifact produced by Registry Drift Report
  (registry-drift.yaml) listing which recipes/registry.yaml chart pins have
  moved upstream. Triggers on "review this week's drift", "component drift",
  "/aicr-reviewing-component-drift", "should we bump <component>", "what changed
  in <chart>", a pasted drift digest from Slack, or release prep that needs to
  know which chart pins are safe to advance. Gathers values-path
  compatibility, lockstep-family, image/BOM, CRD/upgrade-record, upstream
  release-note, and Kubernetes-compatibility evidence per component, then
  writes a ranked take/hold/defer recommendation. Recommends only — it never
  edits the registry, files an issue, or opens a PR.
---

# AICR Component Drift Review

Turns a weekly drift report into a decision. The workflow
(<https://github.com/NVIDIA/aicr/actions/workflows/registry-drift.yaml>) answers
"what moved"; this skill answers "what should we take, and what does taking it
cost". An AICR component bump is never a version-string edit, which is exactly
why the workflow does not open PRs.

## When to Use

- The weekly drift digest landed in Slack and someone asks what to do about it
- Release prep needs to know which chart pins can safely advance
- Someone asks whether a specific component should be bumped

Do NOT use this skill to perform a bump. It produces a recommendation; the edit,
the `make bom-docs` run, and the PR are separate, human-initiated work.

## Step 1 — Read the state file

Read `drift-state.yaml` beside this file before anything else. It records what
was reviewed at which candidate set and why anything was held, so a component
whose candidates have not changed since last week is confirmed, not re-derived.
Update it in Step 5.

**Reuse a verdict only after comparing the whole candidate set** — `current`,
`latest`, and every entry in `alternatives[]` and `predating[]` — against what
the entry records. A chart in `withheld[]` has no `latest`; its set is
`current` plus `predating[]`.
A verdict keyed on the version pair alone goes stale silently, because a new
candidate can appear while `current` and `latest` both stay put: `kai-scheduler`
sat at `v0.16.9 -> v0.20.1` across two runs while a `patch v0.16.10` arrived
beside it, and v0.16.10 is the one worth taking. If any candidate was added,
removed, or changed, re-derive.

An entry with no recorded `candidates` predates that field. Treat it as changed
and re-derive it; do not read the absence as "nothing new".

## Step 2 — Resolve the input

In order of preference:

1. An explicit run, downloaded into a scratch directory rather than the
   working tree (`gh run download` extracts in place; without `--dir` the two
   report files land as untracked files in this checkout and collide with
   themselves on a second run):
   ```bash
   dir=$(mktemp -d "${TMPDIR:-/tmp}/aicr-drift.XXXXXX")
   gh run download <id> -R NVIDIA/aicr -n drift-report --dir "$dir"
   ```
   Read `drift-report.json` and `drift-report-raw.json` from `"$dir"`.
2. The latest run:
   `gh run list -R NVIDIA/aicr --workflow=registry-drift.yaml --status=success --event schedule --limit 1 --json databaseId`
   then the same scratch-directory download as above (`mktemp -d` + `gh run
   download <id> -R NVIDIA/aicr -n drift-report --dir "$dir"`).

   The default is deliberately the weekly scheduled run on the default
   branch — `registry-drift.yaml` also runs on `workflow_dispatch` and, for a
   same-repo PR touching the drift surface, on `pull_request`; every
   successful path uploads the same `drift-report` artifact name, so an
   unfiltered "latest successful run" could just as easily be a PR-validation
   run and put the review against unmerged changes. `--event schedule`
   excludes both. Reviewing a manual or PR-validation report is still
   supported — pass `--run <id>` explicitly (option 1) rather than relying on
   the default.
3. A local `drift-report.json` path the user provides
4. A pasted Slack digest — it carries counts and a `report artifact` link, no
   component names, so take the run ID out of that link and download the
   artifact per option 1. If the artifact has expired, re-derive the drift
   set from `recipes/registry.yaml` and the upstream registry (`helm show
   chart` for HTTP repos, `crane ls` for `oci://`). That re-derivation is
   unfiltered: it does not apply Renovate's `minimumReleaseAge` (3 days,
   `.github/renovate.json5`), `internalChecksFilter: "strict"`, or the
   report's publish-date screen. It can surface a release younger than the
   cooldown, one Renovate's strict filter would reject, or a frozen line
   numbered above the maintained one (the highest `crane ls` tag for
   `kai-scheduler` is the retracted `v0.20.1`). Use it only to identify which
   components to look at —
   never to justify a bump on its own; the artifact remains the authoritative
   source for whether a version is actually eligible.

Confirm `schemaVersion` is `2`. A higher number means this skill is stale —
read `tools/drift-report/report.go` before trusting the field names. A `1` is
an artifact from before the publish-date screen: its `latest` can be a
retracted or frozen line, so verify every candidate's release date by hand.

Report `unresolved[]` to the user before reviewing anything: those pins are
unknown, not current, and a persistent entry is a broken datasource worth
fixing ahead of any bump.

**Read `alternatives[]` on every row that has one.** `latest` is the largest
step on offer, not the only one and not necessarily the one to take. A row
carrying `alternatives` has a smaller step available — usually the one worth
recommending, since it crosses fewer upstream changes. Name both in the verdict
and say which you are recommending; a verdict that discusses only `latest` when
the row offered a minor beside a major has reviewed the wrong upgrade (#2791).
The field is absent when only one candidate was offered, the common case.

**Read `predating[]` and `withheld[]`.** A registry can carry a frozen or
retracted line numbered above the maintained one, and Renovate offers the
highest number. That is how `kai-scheduler`'s retracted `v0.20.1` and
`agentgateway`'s kgateway-lineage `v2.2.1` were proposed for weeks (#2791). The
tool walks each update type from the highest version down. It withholds
versions published more than a day before the current pin (`currentPublished`)
until it reaches one that is not, offers that one, and drops lower versions of
that type unexamined. A withheld version stays visible in the row's
`predating[]`; a chart whose every candidate was withheld moves out of
`drift[]` into `withheld[]` and has no `latest`. Publish order is a signal, not
a verdict: a maintained parallel line is withheld too when its newest release
is older than the pin, as when the pin is itself a recent patch on an older
line. Never recommend a withheld version without confirming upstream still
maintains its line (`gh release list`, the release being marked Latest, no
retraction), and say in the verdict that you did.

**Treat `unscreened` as unchecked.** It is set when an offered version could
not be checked against the pin: the pin itself has no publish date (then
nothing was withheld), or an offered version has none. Causes are a failed
registry lookup, an OCI chart without a usable
`org.opencontainers.image.created` annotation, or a Helm entry Renovate left
undated. An unscreened version is offered exactly as before the screen
existed, and lower lines of its type were not examined. Check the dates
yourself (`crane manifest <image>:<tag>` for `oci://`, `helm search repo -l`
or the repo index for Helm) and look for a maintained line below it
(`crane ls`) before recommending one. `summary.unscreened` counts these rows,
and the digest names the count when it is not zero; a sudden jump usually means
a registry refused the lookups. A row without `unscreened` had every offered
version checked.

## Step 3 — Gather evidence per component

Ordered by how often each is what actually bites.

1. **Values-path compatibility.** Pull both chart versions and diff their
   values, then verify every path AICR depends on still exists:
   - keys set in `recipes/components/<name>/values.yaml`
   - `values:` blocks in `recipes/overlays/*.yaml` and `recipes/mixins/*.yaml`
     that target the component
   - the component's `nodeScheduling.nodeSelectorPaths` and `tolerationPaths`
     in `recipes/registry.yaml`

   The third is the silent one: a renamed `master.nodeSelector` leaves AICR
   rendering valid YAML that no longer schedules anything correctly, and no
   current test catches it. Treat a missing path as a blocking finding.

2. **Lockstep families.** These move together; never recommend a partial bump:
   - `slinky-slurm-operator-crds`, `slinky-slurm-operator`, `slinky-slurm`
   - `mariadb-operator-crds`, `mariadb-operator`, `slurm-accounting-mariadb`
   - `agentgateway-crds`, `agentgateway`
   - `prometheus-operator-crds`, `kube-prometheus-stack`, keyed on
     `appVersion`, not chart version (the two chart sequences are unrelated).
     A lagging CRDs pin also breaks `nvsentinel` and
     `k8s-ephemeral-storage-metrics`, which create resources defined by those CRDs.
     `TestPrometheusOperatorAppVersionLockstep` enforces this.
   - `prometheus-adapter` / `prometheus-adapter-ocp`,
     `nvidia-dra-driver-gpu` / `nvidia-dra-driver-gpu-ocp`, and
     `k8s-nim-operator` / `k8s-nim-operator-ocp` — these three `-ocp` twins
     carry a `defaultRepository` identical to their base, so `BuildReport`
     already collapses each pair into one report row; name both in the
     verdict anyway. The other four `-ocp` components (`gpu-operator-ocp`,
     `network-operator-ocp`, `nfd-ocp`, `cert-manager-ocp`) have
     `defaultRepository: ""` and are manifest-only — `BuildReport` drops them
     before `Tracked++`, so they never appear in the report at all (they
     surface in `untrackedComponents` instead). Do not assume a base
     component's row already accounts for one of these four; there is no
     shared row to collapse into.

3. **Image and BOM delta.** Render old against new and diff the image list.
   `make bom-docs` is the repo's renderer; a new image means new vulnerability
   surface, and a new registry host means `tools/registry-inventory`'s allowlist
   needs extending too.

4. **CRD and upgrade-record impact.** For CRD-bearing charts (kueue,
   gatekeeper, mariadb, slinky, prometheus-operator-crds, nvsentinel), check
   whether the transition needs an ADR-021 record: see `pkg/upgrade` and
   `tools/check-upgrade-records`.

5. **Upstream breaking changes.** Where the chart maps to a GitHub project,
   read the releases between the two versions
   (`gh release list -R <owner>/<repo>`), looking for removed flags, renamed
   values, and required migration steps.

6. **Kubernetes compatibility and blast radius.** Compare the new chart's
   `kubeVersion` against the `K8s.server.version` constraints of the overlays
   that pull the component, and check whether
   `recipes/checks/<name>/health-check.yaml` exists and whether its assertions
   still hold against the new chart's resource names.

## Step 4 — Write the recommendation

Write Markdown to a temp file (`"$TMPDIR"/aicr-drift-review-<date>.md`) and
summarize it in chat. Rank components by (blocking findings, then update type,
then age of the pin). Per component:

- **Verdict:** take / hold / defer, one line of why. When the row carries
  `alternatives` or `predating`, name which version the verdict is about — a
  bare "take" is ambiguous once more than one is on offer, and a `withheld[]`
  row has no `latest` to default to
- **Evidence:** the findings from Step 3 that produced the verdict, with the
  specific values path, image, or release note that matters
- **Cost:** what a bump would require — a values-file edit, a new upgrade
  record, a BOM refresh, a health-check update, an allowlist entry

Do not edit the registry, file issues, or open PRs. Ask before doing anything
outward-facing.

## Step 5 — Update the state file

Record every component reviewed: the version pair, the full candidate set the
verdict was formed against, which candidate the verdict recommends, the date,
and for a hold or defer the condition that would change it. Next week's digest
repeats the same components by design — this file is what keeps the review from
repeating with it.

`candidates` is what Step 1 compares against, so record it verbatim from the
report rather than summarizing: an entry that omits a candidate makes next
week's run confirm a verdict that never considered it. Record a `predating[]`
entry with a `predating` prefix (`predating major v2.2.1`) so a withheld version
is never read back as one that was offered. `recommends` is only meaningful
when the row had alternatives or predating versions; omit it otherwise, since
the single candidate is already named by `to`. A `withheld[]` row has no `latest`, so omit `to` and set
`recommends` to the predating version the verdict names, if any.

## Gotchas

- `crane ls` on `ghcr.io` works in the sandbox; NGC (`nvcr.io`, and
  `helm.ngc.nvidia.com` over `curl`) and some TLS paths need
  `dangerouslyDisableSandbox: true`. Try sandboxed first.
- OCI chart repositories list cosign signature and attestation tags alongside
  real chart tags. Ignore anything that is not a version.
- `recipes/overlays/aks.yaml` pins `kube-prometheus-stack` to `83.7.0`
  deliberately (#700, declared in `versionPinExemptions` in
  `pkg/recipe/version_pin_guard_test.go`). A `kube-prometheus-stack` bump has to
  reckon with that exemption; do not propose removing it casually.
- `aws-efa`'s chart is tracked but its device-plugin image is in Renovate's
  `ignoreDeps` and must be coordinated with EKS add-on releases.

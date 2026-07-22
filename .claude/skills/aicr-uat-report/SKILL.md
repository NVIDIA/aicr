---
name: aicr-uat-report
description: |
  Use when reporting on UAT health across services and GPU targets — which
  service (EKS/GKE/AKS) x GPU (H100/GB200) x intent combinations are passing
  or failing in the UAT Run workflow (uat-run.yaml). Triggers on "UAT report",
  "/aicr-uat-report", "which UAT combos are failing", "UAT pass rate", or
  RC/release-candidate validation prep that needs the combinations to test
  manually. Runs the bundled uat_report.py (read-only gh calls), classifies
  failures as product vs infra signal, and prints a summary table plus an
  RC validation priority list.
---

# AICR UAT Report

Reports how the UAT Run workflow
(<https://github.com/NVIDIA/aicr/actions/workflows/uat-run.yaml>) performed
over a lookback window, aggregated by service x GPU x intent, with each
failure classified as product signal (real test failure) or infra noise.
The output feeds the release process: combinations failing on `main` are
the ones to test more closely during RC validation.

## When to Use

- User asks for a UAT report, UAT pass rates, or failing UAT combinations
- User invokes `/aicr-uat-report` (optionally with a number of days)
- Release prep needs the list of service/GPU combos to validate manually

Do NOT use this skill to re-run, dispatch, or debug individual UAT runs —
it only reads run history and reports.

## Inputs

- **days** (optional, default **3**): lookback window. If the user says
  "over the last week", pass `--days 7`. Do not ask — default to 3 when
  unspecified.
- Only runs against `main` (empty `aicr_version` input) are reported by
  default; that is the release-process signal. Add `--all-versions` only
  if the user explicitly asks to compare against release-tag runs.

## How the Data Works

The workflow's `run-name` encodes everything needed — no per-job digging:
`UAT <reservation> <intent> @ <version|main>[ #dispatch_key]`.
Reservation names are `<cloud>-<gpu>` rows from
`infra/uat/reservations.yaml` (e.g. `aws-h100`, `gcp-h100`, `azure-h100`,
`aws-gb200`), and cloud maps to service: aws=EKS, gcp=GKE, azure=AKS,
kind=Kind (self-hosted nvkind lane). Runs before 2026-07-21 used a title
without the intent word; the script derives intent from the nightly
dispatch-key cell index (version-outer/intent-inner, training first) and
marks those rows "(intent derived)".

## Procedure

### Step 1 — Run the report script

```bash
python3 .claude/skills/aicr-uat-report/uat_report.py --days 3
```

It prints, per version, a Markdown table (`Service | GPU | Intent | Pass |
Failures`) and a "Failure detail" section listing each failing run's
timestamp, URL, and the failed job/step names. It is read-only
(`gh run list` / `gh run view`). If `gh` is not authenticated, stop and
tell the user to run `gh auth status`.

### Step 2 — Classify each failure

Map the failed step name to a failure nature. This drives the RC
priority ranking, so classify every failure:

| Failed step contains | Nature | Product signal? |
|---|---|---|
| `UAT - validate`, `UAT - prep`, CUJ/test phase names | Real test failure | YES |
| `Bringup Infra`, provision/actuator steps | Infra bring-up failure | no |
| `Buildx`, `Build and push`, image/GHCR steps | CI/image flake | no |
| `Validate inputs`, `helmfile apply`, install steps | Setup/install issue | maybe — recurring = investigate |

A retry that went green the same night (same combo, later timestamp,
success) downgrades the earlier failure to a flake.

### Step 3 — Render the report

Produce exactly two artifacts, in this order (see Output Format
Reference). Sort the table worst-first: lowest pass ratio at the top;
bold the Service/GPU/Intent cells of rows with product-signal failures.
Include run URLs as links for at least the most recent failure of each
failing combo.

### Step 4 — Write the RC validation input

A numbered priority list derived from the table:

1. Combos with **consistent test-phase failures** (0/N or repeated
   validate-phase failures) — top manual-validation priority.
2. Combos whose **most recent** failure is test-phase (even if earlier
   ones were infra) — deserve a close look.
3. Combos with **infra/CI-only** failures — noisy, not product signal;
   note them but rank low.
4. Reservations in **bring-up** (e.g. GB200 while `nightly-intents: []`,
   kind lane) — expected churn, call out separately.
5. **Green combos** — state them explicitly as lowest priority; a clean
   bill is information too.

## Output Format Reference

```markdown
## UAT report against `main` (<start>–<end>, N runs)

| Service | GPU | Intent | Pass | Failure nature |
|---|---|---|---|---|
| **AKS** | **H100** | **training** | **0/4** | Real test failures — every run fails at "UAT - validate (all phases)" ([latest](<url>)) |
| EKS | H100 | training | 2/5 | Infra only — 2x bring-up, 1x Buildx CI flake; no test-phase failures |
| GKE | H100 | training | 4/4 | Green |

## RC validation input

1. **<Service>/<GPU>/<intent> is the clear red flag** — <pass ratio,
   failure signature, latest run link, whether it also fails on release
   tags (env issue) or only main (regression candidate)>.
2. ...
N. **<green combos> are solidly green** — lowest manual-testing priority.
```

Keep failure-nature cells to one sentence; detail beyond that belongs in
the RC list, not the table.

## Failure Modes

- **`gh run list` returns nothing** — window may predate retention or the
  workflow was renamed; say so rather than reporting "all green".
- **Unparsed titles in the Notes line** — the `run-name` format in
  `uat-run.yaml` changed; read the workflow's current format string and
  update `NEW_TITLE`/`OLD_TITLE` in `uat_report.py` in the same PR.
- **Unknown reservation (e.g. new cloud)** — the script falls back to the
  uppercased cloud token as the service name; cross-check new rows against
  `infra/uat/reservations.yaml`.
- **A combo has very few runs** (e.g. 0/1) — flag low sample size instead
  of declaring it broken.

## What This Skill Does NOT Do

- Does not dispatch, re-run, or cancel workflow runs
- Does not read job logs or diagnose root causes (it reports failed step
  names only — deeper debugging is a follow-up task)
- Does not modify reservations, workflows, or any in-repo file

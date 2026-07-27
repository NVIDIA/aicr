---
name: aicr-uat-report
description: |
  Use when reporting on UAT health across services and GPU targets — which
  service (EKS/GKE/AKS) x GPU (H100/GB200) x intent combinations are passing
  or failing in the UAT Run workflow (uat-run.yaml). Triggers on "UAT report",
  "/aicr-uat-report", "which UAT combos are failing", "UAT pass rate",
  "download the UAT debug bundle", "why did the UAT run fail", or
  RC/release-candidate validation prep that needs the combinations to test
  manually. Runs the bundled uat_report.py, classifies failures as product
  vs infra signal, prints a summary table plus an RC validation priority
  list, and can download the per-run cluster debug bundles for triage.
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
- User asks why a UAT run failed, or for the debug bundle behind a failure

Do NOT use this skill to re-run, dispatch, or cancel UAT runs.

## Inputs

- **days** (optional, default **3**): lookback window. If the user says
  "over the last week", pass `--days 7`. Do not ask — default to 3 when
  unspecified.
- Only runs against `main` (empty `aicr_version` input) are reported by
  default; that is the release-process signal. Add `--all-versions` only
  if the user explicitly asks to compare against release-tag runs.
- **debug bundles** (optional): pass `--download-debug <dir>` when the user
  asks why something failed, or when Step 2 classifies a failure as product
  signal. Skip it for a plain pass-rate report — bundles are tens of MB.

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

### Debug bundles

Each per-cloud workflow (`uat-aws.yaml`, `uat-gcp.yaml`, `uat-azure.yaml`,
`uat-kind.yaml`) runs `tests/uat/<cloud>/run debug` on failure — before
teardown, while the cluster is still up — and uploads
`uat-<cloud>[-<intent>]-debug-<run_id>` with **30-day retention**. Reusable
workflows inherit the caller's `run_id`, so the artifact hangs off the
`uat-run.yaml` run the report already lists.

The upload is gated on `failure() && steps.prep.outcome != 'skipped'`, so
**no bundle exists** when the cloud job died in bring-up or image build, or
when the failure was in a downstream job (evidence ingest) while the cluster
job passed. Absence is itself a classification signal, not an error.

What to open, in triage order (`if-no-files-found: ignore`, so any entry can
be missing; `cluster-debug/` prefix omitted below):

| Open | When / what it answers |
|---|---|
| `MANIFEST.yaml` | Always first — runId, config, resolved recipe + criteria, and `failingChecks` lifted from `report.json` |
| `report.json` | Full validator results; absent if the run died before validate |
| `train-logs/**`, `serve-logs/**` | A CUJ check failed (NCCL, inference-perf) |
| `cr-skyhooks.yaml`, `node-reboot-fingerprint.txt`, `readiness-gate.log` | Readiness-gate or tuning-race failure — Skyhook `status.status`, taints, bootID/kernel |
| `pods-notready.txt`, `events.txt` | Scheduling, eviction, OOM |
| `logs-<namespace>.txt` | The operator owning the failing resource |
| `nodes*`, other `cr-*.yaml`, `ns-*.txt` | Broader node and operator state |
| `snapshot.yaml`, `recipe.yaml`, `dry-run.json` | What was collected / resolved / deployed |
| `evidence-result.json`, `evidence/pointer.yaml` | Signed-evidence emit outcome |

## Procedure

### Step 1 — Run the report script

```bash
python3 .agents/skills/aicr-uat-report/uat_report.py --days 3
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
| `UAT - validate`, `UAT - prep`, CUJ/test phase names | Real test failure | YES — but the validate step also emits signed evidence, so confirm against `report.json` (Step 2b) before ranking |
| `Bringup Infra`, provision/actuator steps | Infra bring-up failure | no |
| `Buildx`, `Build and push`, image/GHCR steps | CI/image flake | no |
| `Validate inputs`, `helmfile apply`, install steps | Setup/install issue | maybe — recurring = investigate |

A retry that went green the same night (same combo, later timestamp,
success) downgrades the earlier failure to a flake.

### Step 2b — Pull debug bundles (only for product-signal failures)

Skip this step entirely for a routine pass-rate report. Run it when the
user asks *why* something failed, or when Step 2 found a test-phase
failure worth root-causing:

```bash
python3 .agents/skills/aicr-uat-report/uat_report.py --days 3 \
  --download-debug /tmp/uat-debug --max-downloads 3
```

Prefer `--run <id>` (repeatable) over raising `--max-downloads`: usually only
the latest failure per failing combo is worth reading.

Bundles land in `<dir>/<service>-<gpu>-<intent>-<run_id>/`, each with a
printed digest — MANIFEST head, failing checks from `report.json`, and a
one-line contents summary. Read that summary for *presence*, not file names:
a missing `evidence/` or `report.json` says the run died before that stage,
which is often the whole diagnosis. Then open files per the Debug bundles
table above.

Cite `file_path:line` and the run ID for every finding. Leave the download
directory in place — the user may want to keep digging.

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
- **"no debug artifact"** — expected for bring-up/Buildx/ingest failures
  (see Debug bundles). Report it as corroborating the infra classification;
  do not present it as a tooling problem.
- **"debug artifact expired"** — the window exceeds the 30-day retention.
  Nothing to recover; note it and work from failed step names.
- **`cluster-debug/` missing or thin** — the collector is best-effort and
  its cloud credentials can expire on a long failure. Say the bundle is
  incomplete; do not read it as the cluster being healthy.
- **Failing step name disagrees with `report.json`** — trust the bundle.
  `UAT - validate (all phases) + emit signed evidence` covers two concerns,
  so N/N passing checks under a failed step means the evidence leg failed,
  not the product. Reclassify to infra before ranking it in Step 4.

## What This Skill Does NOT Do

- Does not dispatch, re-run, or cancel workflow runs
- Does not download raw job logs (`gh run view --log`); it reports failed
  step names and, on request, the uploaded cluster debug bundles
- Does not modify reservations, workflows, or any in-repo file — the only
  writes are downloaded artifacts under the `--download-debug` directory

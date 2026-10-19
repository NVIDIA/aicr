#!/usr/bin/env python3
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Aggregate UAT Run workflow results by service x GPU x intent.

Fetches runs of .github/workflows/uat-run.yaml via `gh`, keeps the runs
that exercised `main` (aicr_version empty), and prints a Markdown summary
table plus per-failure detail (failed job/step + run URL) for the agent to
classify and narrate. Reporting is read-only (`gh run list` / `gh run view`).

With --download-debug DIR it additionally fetches the `uat-<cloud>-debug-<run_id>`
artifact for failing runs into DIR/<service>-<gpu>-<intent>-<run_id>/ and
prints a triage digest (cluster-debug/MANIFEST.yaml, report.json failing
checks, and the readiness gate's last failed-validator block). That is the
only mode that writes anything, and only under DIR.

Usage: uat_report.py [--days N] [--repo OWNER/REPO] [--all-versions]
                     [--download-debug DIR] [--max-downloads N] [--run ID]
"""

import argparse
import json
import os
import re
import subprocess
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone

WORKFLOW = "uat-run.yaml"

# Per-cloud workflows upload the failure bundle as
# `uat-<cloud>[-<intent>]-debug-<run_id>`. Reusable workflows share the caller's
# run_id, so the artifact hangs off the uat-run.yaml run we already listed.
DEBUG_ARTIFACT_HINT = "debug"

# `retention-days: 30` on every "Upload failure debug" step.
DEBUG_RETENTION_DAYS = 30

# How much of MANIFEST.yaml to echo in the triage digest — enough to cover
# generatedAt/runId/config/recipe/criteria plus the failingChecks block.
MANIFEST_DIGEST_LINES = 40

# Reservation names are <cloud>-<gpu> (infra/uat/reservations.yaml);
# cloud maps to the managed-Kubernetes service users know it by.
SERVICE = {"aws": "EKS", "gcp": "GKE", "azure": "AKS", "kind": "Kind"}

# Current title: "UAT <reservation> <intent> @ <version|main>[ #key]"
NEW_TITLE = re.compile(r"^UAT (\S+) (training|inference) @ (\S+)")
# Pre-2026-07-21 title lacked the intent: "UAT <reservation> @ <version>[ #key]"
OLD_TITLE = re.compile(r"^UAT (\S+) @ (\S+?)(?:\s+#\S+-(\d+))?$")

# Step-name prefixes of the per-cloud workflows. Runs that predate the split
# (#2630) have no readiness step: their "UAT - install (...)" step ran both the
# apply and the readiness gate, so a failure there cannot be attributed to
# either from the step name alone.
INSTALL_STEP = "UAT - install"
READINESS_STEP = "UAT - readiness gate"

# Title of the annotation the readiness phase emits on failure; its message is
# "<comma-separated failing validators>: <short reason>".
READINESS_ANNOTATION_TITLE = "UAT readiness gate failed"

# The per-attempt block in readiness-gate.log holding the non-passed tests'
# name, message, and stdout (incl. "Failed resources:"). Attempts are headed by
# "===== attempt N ...", which also ends the block.
FAILED_VALIDATOR_BLOCK = re.compile(r"^--- failed validator output \(attempt (\d+)\) ---$")
GATE_ATTEMPT_HEADER = "===== attempt "
GATE_DIGEST_LINES = 30


def gh_json(args):
    out = subprocess.run(
        ["gh"] + args, capture_output=True, text=True, timeout=120, check=True
    ).stdout
    return json.loads(out)


def parse_title(title):
    """Return (reservation, intent, version, intent_derived) or None."""
    m = NEW_TITLE.match(title)
    if m:
        res, intent, ver = m.groups()
        return res, intent, ver, False
    m = OLD_TITLE.match(title)
    if not m:
        return None
    res, ver, cell = m.groups()
    # Nightly batch cells run version-outer/intent-inner with training first,
    # so odd cell index = training, even = inference. Manual dispatches
    # (no dispatch key) defaulted to training.
    intent = "training" if cell is None or int(cell) % 2 == 1 else "inference"
    return res, intent, ver, True


def run_id_of(run_url):
    return run_url.rstrip("/").rsplit("/", 1)[-1]


def step_nature(step, job_steps):
    """Return a classification tag for a failed install/readiness step, else None.

    `job_steps` is every step name in the job; a readiness step anywhere in it
    marks the post-#2630 layout, where install is apply-only.
    """
    if step.startswith(READINESS_STEP):
        return "readiness gate: product signal"
    if step.startswith(INSTALL_STEP):
        if any(s.startswith(READINESS_STEP) for s in job_steps):
            return "apply only: maybe infra"
        return "legacy apply+readiness: ambiguous, read readiness-gate.log"
    return None


def readiness_annotation(repo, job_id):
    """Return the failing-validators message the readiness phase annotated, or None."""
    try:
        anns = gh_json(["api", f"repos/{repo}/check-runs/{job_id}/annotations"])
    except (subprocess.SubprocessError, json.JSONDecodeError):
        return None
    for ann in anns if isinstance(anns, list) else []:
        if ann.get("title") == READINESS_ANNOTATION_TITLE:
            return (ann.get("message") or "").strip() or None
    return None


def failed_steps(repo, run_url):
    run_id = run_id_of(run_url)
    try:
        data = gh_json(["run", "view", run_id, "-R", repo, "--json", "jobs"])
    except (subprocess.SubprocessError, json.JSONDecodeError):
        return "job details unavailable"
    parts = []
    for job in data.get("jobs", []):
        if job.get("conclusion") != "failure":
            continue
        job_steps = [s.get("name", "") for s in job.get("steps", [])]
        steps = []
        for s in job.get("steps", []):
            if s.get("conclusion") != "failure":
                continue
            name = s["name"]
            nature = step_nature(name, job_steps)
            if nature:
                name += f" [{nature}]"
            if name.startswith(READINESS_STEP) and job.get("databaseId"):
                msg = readiness_annotation(repo, job["databaseId"])
                if msg:
                    name += f" (failing validators: {msg})"
            steps.append(name)
        parts.append(f"{job['name']} — {', '.join(steps) or 'no failed step recorded'}")
    return "; ".join(parts) or "no failed job recorded"


def debug_artifacts(repo, run_id):
    """Return every debug artifact on the run, expired ones included.

    The caller filters for expiry so it can distinguish "never uploaded"
    (infra failure before prep) from "aged out of retention" — different
    classifications that a silent filter here would collapse into one.
    """
    arts = gh_json(
        ["api", f"repos/{repo}/actions/runs/{run_id}/artifacts", "--jq", ".artifacts"]
    )
    return [a for a in arts if DEBUG_ARTIFACT_HINT in a.get("name", "")]


def slug(*parts):
    """Build a filesystem-safe directory name from run metadata.

    Reservation names reach us through the workflow's run-name, and the title
    regexes accept any non-whitespace token, so `svc`/`gpu` are not guaranteed
    to be well-formed. Collapsing everything outside [a-z0-9-] removes both
    separators and dot runs, so no component can traverse out of the download
    directory.
    """
    raw = "-".join(str(p) for p in parts).lower()
    return re.sub(r"[^a-z0-9-]+", "-", raw).strip("-") or "unknown"


def gate_failure_block(path):
    """Return the last attempt's failed-validator block from readiness-gate.log.

    The last attempt is the one the gate gave up on, so its block names the
    validators that never converged. Returns [] when the log predates the
    block (#2630) or the gate never failed an attempt.
    """
    block, inside = [], False
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            for raw in fh:
                line = raw.rstrip("\n")
                if FAILED_VALIDATOR_BLOCK.match(line):
                    block, inside = [line], True
                elif line.startswith(GATE_ATTEMPT_HEADER):
                    inside = False
                elif inside:
                    block.append(line)
    except OSError:
        return []
    while block and not block[-1].strip():
        block.pop()
    return block


def triage_digest(dest):
    """Summarize a downloaded bundle: manifest head + failing checks + inventory."""
    lines = []
    manifest = os.path.join(dest, "cluster-debug", "MANIFEST.yaml")
    if os.path.isfile(manifest):
        with open(manifest, encoding="utf-8", errors="replace") as fh:
            head = [next(fh, "").rstrip("\n") for _ in range(MANIFEST_DIGEST_LINES)]
        lines.append("  MANIFEST.yaml:")
        lines += [f"    {ln}" for ln in head if ln]
    else:
        lines.append(
            "  no cluster-debug/MANIFEST.yaml — the collector did not run "
            "(failure before prep, or kubectl unavailable)"
        )
    report = os.path.join(dest, "report.json")
    if os.path.isfile(report):
        try:
            with open(report, encoding="utf-8") as fh:
                # `or {}` / `or []` rather than .get(k, default): a truncated
                # report can carry an explicit null, which a default does not
                # catch. Nothing here may raise — triage_digest is called
                # per-run, so one bad file must not abort the whole download.
                tests = (json.load(fh).get("results") or {}).get("tests") or []
            bad = [t for t in tests if t.get("status") in ("failed", "other")]
            lines.append(
                f"  report.json: {len(bad)}/{len(tests)} checks failing"
                + (
                    ": " + ", ".join(f"{t.get('name')}({t.get('status')})" for t in bad)
                    if bad
                    else ""
                )
            )
        except (OSError, ValueError, AttributeError, TypeError):
            lines.append("  report.json present but unparseable")
    gate_log = os.path.join(dest, "cluster-debug", "readiness-gate.log")
    if os.path.isfile(gate_log):
        block = gate_failure_block(gate_log)
        if block:
            lines.append("  readiness-gate.log, last failed attempt:")
            lines += [f"    {ln}" for ln in block[:GATE_DIGEST_LINES]]
            if len(block) > GATE_DIGEST_LINES:
                lines.append(f"    ... ({len(block) - GATE_DIGEST_LINES} more lines in the file)")
    # Presence, not a full listing: which top-level artifacts the run produced
    # (is there a train-logs/? did evidence emit?) is the actionable part.
    # cluster-debug/ collapses to a count — SKILL.md's reading order says which
    # of its files to open, so naming all 40 here is noise.
    contents = sorted(os.listdir(dest)) if os.path.isdir(dest) else []
    cluster = os.path.join(dest, "cluster-debug")
    if os.path.isdir(cluster):
        count = len(os.listdir(cluster))
        contents = [c for c in contents if c != "cluster-debug"]
        contents.append(f"cluster-debug/ ({count} files)")
    if contents:
        lines.append(f"  contents: {', '.join(contents)}")
    return lines


def download_debug(repo, failures, outdir, limit):
    """Fetch debug bundles for `failures` (newest first) into `outdir`.

    Returns nothing; prints a per-run status block. An absent artifact is a
    signal, not an error: the upload step is gated on
    `failure() && steps.prep.outcome != 'skipped'`, so a run that died in
    bring-up or image build has no bundle by construction.
    """
    print("## Debug bundles\n")
    selected = failures[:limit]
    if len(failures) > len(selected):
        print(
            f"Downloading {len(selected)} of {len(failures)} failing run(s) "
            f"(newest first; raise --max-downloads to include the rest).\n"
        )
    for f in selected:
        run_id = run_id_of(f["url"])
        label = f"{f['svc']}/{f['gpu']}/{f['intent']} {f['ts']}Z run {run_id}"
        dest = os.path.join(
            outdir, slug(f["svc"], f["gpu"], f["intent"], run_id)
        )
        try:
            arts = debug_artifacts(repo, run_id)
        except (subprocess.SubprocessError, json.JSONDecodeError) as err:
            print(f"- {label}: could not list artifacts ({err})")
            continue
        if not arts:
            print(
                f"- {label}: no debug artifact — either the cloud job never "
                "reached `UAT - prep` (bring-up / image build), or the failure "
                "was in a downstream job (evidence ingest) while the cluster "
                "job passed. Both are infra/CI, not product signal."
            )
            continue
        live = [a for a in arts if not a.get("expired")]
        if not live:
            print(
                f"- {label}: debug artifact expired "
                f"(retention is {DEBUG_RETENTION_DAYS}d)"
            )
            continue
        for art in live:
            # One run normally carries one bundle, but a caller that fans out
            # several cloud jobs under a single run_id would produce more.
            # Unpacking those into a shared directory would silently merge two
            # clusters' state and make the digest describe neither, so give
            # each artifact its own subdirectory once there is more than one.
            art_dest = dest if len(live) == 1 else os.path.join(dest, slug(art["name"]))
            mb = art.get("size_in_bytes", 0) / 1e6
            os.makedirs(art_dest, exist_ok=True)
            try:
                subprocess.run(
                    ["gh", "run", "download", run_id, "-R", repo,
                     "-n", art["name"], "-D", art_dest],
                    capture_output=True, text=True, timeout=600, check=True,
                )
            except subprocess.SubprocessError as err:
                stderr = (getattr(err, "stderr", "") or "").strip()
                print(
                    f"- {label}: download of {art['name']} failed "
                    f"({stderr or err})"
                )
                continue
            print(f"- {label}: {art['name']} ({mb:.1f} MB) -> {art_dest}")
            print("\n".join(triage_digest(art_dest)))
    print()


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--days", type=int, default=3, help="lookback window (default 3)")
    ap.add_argument("--repo", default="NVIDIA/aicr")
    ap.add_argument(
        "--all-versions",
        action="store_true",
        help="also report release-version runs (default: main only)",
    )
    ap.add_argument(
        "--download-debug",
        metavar="DIR",
        help="download the failure debug bundle of each failing run into DIR",
    )
    ap.add_argument(
        "--max-downloads",
        type=int,
        default=5,
        help="cap on bundles fetched by --download-debug (default 5, newest first)",
    )
    ap.add_argument(
        "--run",
        action="append",
        default=[],
        metavar="ID",
        help="restrict --download-debug to these run IDs (repeatable)",
    )
    opts = ap.parse_args()

    cutoff = (datetime.now(timezone.utc) - timedelta(days=opts.days)).strftime(
        "%Y-%m-%dT%H:%M:%S"
    )
    runs = gh_json(
        [
            "run", "list",
            "--workflow", WORKFLOW,
            "-R", opts.repo,
            "--created", f">={cutoff}",
            "--limit", "500",
            "--json", "displayTitle,conclusion,status,createdAt,url",
        ]
    )

    agg = defaultdict(lambda: defaultdict(list))
    in_progress = excluded_versions = 0
    unparsed = []
    for r in runs:
        parsed = parse_title(r["displayTitle"])
        if not parsed:
            unparsed.append(r["displayTitle"])
            continue
        res, intent, ver, derived = parsed
        if r["conclusion"] in (None, ""):
            in_progress += 1
            continue
        if ver != "main" and not opts.all_versions:
            excluded_versions += 1
            continue
        cloud, _, gpu = res.partition("-")
        key = (SERVICE.get(cloud, cloud.upper()), gpu.upper(), intent)
        agg[ver][key].append(
            (r["createdAt"][:16], r["conclusion"], r["url"], derived)
        )

    failures = []
    print(f"# UAT runs on {opts.repo}, last {opts.days} days (since {cutoff}Z)\n")
    for ver in sorted(agg, key=lambda v: (v != "main", v)):
        print(f"## Version: {ver}\n")
        print("| Service | GPU | Intent | Pass | Failures |")
        print("|---|---|---|---|---|")
        details = []
        for key in sorted(agg[ver]):
            svc, gpu, intent = key
            rows = sorted(agg[ver][key])
            ok = sum(1 for _, c, _, _ in rows if c == "success")
            fails = [(ts, c, url, d) for ts, c, url, d in rows if c != "success"]
            print(f"| {svc} | {gpu} | {intent} | {ok}/{len(rows)} | {len(fails)} |")
            for ts, concl, url, derived in fails:
                note = " (intent derived from dispatch-key cell)" if derived else ""
                failures.append(
                    {"ts": ts, "url": url, "svc": svc, "gpu": gpu, "intent": intent}
                )
                details.append(
                    f"- {svc}/{gpu}/{intent} {ts}Z {concl.upper()}{note}\n"
                    f"  {url}\n"
                    f"  failed: {failed_steps(opts.repo, url)}"
                )
        print()
        if details:
            print("### Failure detail\n")
            print("\n".join(details))
            print()

    if opts.download_debug:
        wanted = failures
        if opts.run:
            wanted = [f for f in failures if run_id_of(f["url"]) in set(opts.run)]
            missing = set(opts.run) - {run_id_of(f["url"]) for f in wanted}
            if missing:
                print(
                    f"Note: run ID(s) {sorted(missing)} are not failing runs in "
                    "this window; widen --days or drop --run.\n"
                )
        wanted.sort(key=lambda f: f["ts"], reverse=True)
        if wanted:
            download_debug(opts.repo, wanted, opts.download_debug, opts.max_downloads)
        else:
            print("## Debug bundles\n\nNo failing runs to download.\n")

    notes = []
    if opts.days > DEBUG_RETENTION_DAYS and opts.download_debug:
        notes.append(
            f"window exceeds the {DEBUG_RETENTION_DAYS}d debug-artifact "
            "retention; older bundles are gone"
        )
    if not opts.all_versions and excluded_versions:
        notes.append(
            f"{excluded_versions} release-version run(s) excluded "
            "(re-run with --all-versions to include)"
        )
    if in_progress:
        notes.append(f"{in_progress} run(s) still in progress, excluded from counts")
    if unparsed:
        notes.append(f"unparsed titles: {unparsed}")
    if notes:
        print("Notes: " + "; ".join(notes))
    if not agg:
        print("No completed runs found in the window.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Aggregate UAT Run workflow results by service x GPU x intent.

Fetches runs of .github/workflows/uat-run.yaml via `gh`, keeps the runs
that exercised `main` (aicr_version empty), and prints a Markdown summary
table plus per-failure detail (failed job/step + run URL) for the agent to
classify and narrate. Read-only: only `gh run list` / `gh run view`.

Usage: uat_report.py [--days N] [--repo OWNER/REPO] [--all-versions]
"""

import argparse
import json
import re
import subprocess
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone

WORKFLOW = "uat-run.yaml"

# Reservation names are <cloud>-<gpu> (infra/uat/reservations.yaml);
# cloud maps to the managed-Kubernetes service users know it by.
SERVICE = {"aws": "EKS", "gcp": "GKE", "azure": "AKS", "kind": "Kind"}

# Current title: "UAT <reservation> <intent> @ <version|main>[ #key]"
NEW_TITLE = re.compile(r"^UAT (\S+) (training|inference) @ (\S+)")
# Pre-2026-07-21 title lacked the intent: "UAT <reservation> @ <version>[ #key]"
OLD_TITLE = re.compile(r"^UAT (\S+) @ (\S+?)(?:\s+#\S+-(\d+))?$")


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


def failed_steps(repo, run_url):
    run_id = run_url.rstrip("/").rsplit("/", 1)[-1]
    try:
        data = gh_json(["run", "view", run_id, "-R", repo, "--json", "jobs"])
    except (subprocess.SubprocessError, json.JSONDecodeError):
        return "job details unavailable"
    parts = []
    for job in data.get("jobs", []):
        if job.get("conclusion") != "failure":
            continue
        steps = [
            s["name"] for s in job.get("steps", []) if s.get("conclusion") == "failure"
        ]
        parts.append(f"{job['name']} — {', '.join(steps) or 'no failed step recorded'}")
    return "; ".join(parts) or "no failed job recorded"


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--days", type=int, default=3, help="lookback window (default 3)")
    ap.add_argument("--repo", default="NVIDIA/aicr")
    ap.add_argument(
        "--all-versions",
        action="store_true",
        help="also report release-version runs (default: main only)",
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

    notes = []
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

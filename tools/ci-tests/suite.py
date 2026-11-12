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
"""Partition the unit suite, summarize streamed Go events, and verify coverage."""

import argparse
from collections import Counter
import json
from pathlib import Path
import sys


MODULE = "github.com/NVIDIA/aicr/"
SHARDS = ("recipe", "a", "b")
# Median package seconds from 11 successful Linux race/coverage runs, 2026-11-12.
# These are balancing weights, not predictions of concurrent command wall time.
SECONDS = {
    "pkg/client/v1": 222.8, "pkg/cli": 206.6, "pkg/operator/controller": 200.5,
    "pkg/bundler": 182.4, "pkg/chainsaw": 99.7, "tests/operator/chart": 57.8,
    "validators/conformance": 39.3, "tools/crd-inventory": 34.9, "tools/coverage": 30.8,
    "pkg/server": 28.5, "pkg/validator/job": 26.7, "tests/releasepolicy": 26.5,
    "validators/performance": 23.6, "pkg/bundler/deployer/localformat": 20.9,
}


def packages(path):
    """Read a nonempty, duplicate-free package list."""
    result = Path(path).read_text().splitlines()
    if not result or len(result) != len(set(result)) or any(not p or p.strip() != p for p in result):
        raise ValueError(f"invalid package list: {path}")
    return result


def partition(all_packages):
    """Keep recipe alone and greedily balance the remainder with stable tie breaks."""
    result = {shard: [] for shard in SHARDS}
    loads = {"a": 0, "b": 0}
    for package in sorted(all_packages, key=lambda p: (-SECONDS.get(p.removeprefix(MODULE), 1), p)):
        if package == MODULE + "pkg/recipe":
            result["recipe"].append(package)
            continue
        shard = min(loads, key=lambda s: (loads[s], s))
        result[shard].append(package)
        loads[shard] += SECONDS.get(package.removeprefix(MODULE), 1)
    verify_manifest(all_packages, result)
    return result


def verify_manifest(all_packages, manifest):
    """Reject missing, duplicate, unexpected, or empty shards and packages."""
    if set(manifest) != set(SHARDS) or any(not manifest[s] for s in SHARDS):
        raise ValueError("expected three nonempty shards: recipe, a, b")
    actual = Counter(p for shard in SHARDS for p in manifest[shard])
    if actual != Counter(all_packages):
        raise ValueError(f"shard package mismatch: {actual - Counter(all_packages)}, "
                         f"missing: {Counter(all_packages) - actual}")
    if manifest["recipe"] != [MODULE + "pkg/recipe"]:
        raise ValueError("recipe must run alone")


def stream(destination):
    """Render Go output as it arrives and retain only terminal timing events."""
    rows = []
    try:
        for line in sys.stdin:
            event = json.loads(line)
            if event.get("Action") == "output":
                print(event.get("Output", ""), end="", flush=True)
            if event.get("Action") in ("pass", "fail", "skip"):
                rows.append({key: event.get(key, "" if key != "Elapsed" else 0)
                             for key in ("Package", "Test", "Action", "Elapsed")})
    finally:
        Path(destination).write_text(json.dumps(rows, separators=(",", ":")) + "\n")
        failed = Counter("tests" if row["Test"] else "packages" for row in rows if row["Action"] == "fail")
        print(f"Go failures: {failed['packages']} packages, {failed['tests']} tests", flush=True)


def merge(root, all_packages, output):
    """Require successful complete shard evidence and merge atomic block counts."""
    manifest = {shard: packages(root / shard / "packages.txt") for shard in SHARDS}
    verify_manifest(all_packages, manifest)
    blocks = {}
    for shard in SHARDS:
        directory = root / shard
        timings = json.loads((directory / "timings.json").read_text())
        completed = [row["Package"] for row in timings if not row["Test"]]
        if Counter(completed) != Counter(manifest[shard]) or any(row["Action"] == "fail" for row in timings):
            raise ValueError(f"incomplete or failed test results: {shard}")
        lines = (directory / "coverage.full.out").read_text().splitlines()
        if not lines or lines[0] != "mode: atomic" or len(lines) == 1:
            raise ValueError(f"missing atomic coverage: {shard}")
        for line in lines[1:]:
            location, statements, count = line.split()
            statements, count = int(statements), int(count)
            if statements < 0 or count < 0:
                raise ValueError(f"negative coverage: {line}")
            previous = blocks.get(location, (statements, 0))
            if previous[0] != statements:
                raise ValueError(f"conflicting statement counts: {location}")
            blocks[location] = (statements, previous[1] + count)
    with Path(output).open("w") as profile:
        profile.write("mode: atomic\n")
        for location, (statements, count) in sorted(blocks.items()):
            if not location.startswith(MODULE + "validators/"):
                profile.write(f"{location} {statements} {count}\n")


def main():
    """Dispatch CI commands; package enumeration is shared with make test."""
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    manifest = commands.add_parser("manifest")
    manifest.add_argument("packages")
    manifest.add_argument("directory", type=Path)
    summary = commands.add_parser("stream")
    summary.add_argument("destination")
    coverage = commands.add_parser("merge")
    coverage.add_argument("packages")
    coverage.add_argument("directory", type=Path)
    coverage.add_argument("output")
    args = parser.parse_args()
    if args.command == "manifest":
        for shard, entries in partition(packages(args.packages)).items():
            directory = args.directory / shard
            directory.mkdir(parents=True, exist_ok=True)
            (directory / "packages.txt").write_text("\n".join(entries) + "\n")
    elif args.command == "stream":
        stream(args.destination)
    else:
        merge(args.directory, packages(args.packages), args.output)


if __name__ == "__main__":
    main()

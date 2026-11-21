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
"""Report which video episodes need fact review between two stable release tags."""

import argparse
import json
from pathlib import Path, PurePosixPath
import re
import subprocess

VIDEOS = Path(__file__).resolve().parent
REPO = VIDEOS.parent.parent
STABLE_TAG = re.compile(r"v(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*)){2}")
INDEX_ID = re.compile(r"^- \[\*\*(\d{2}) · ", re.MULTILINE)
FEATURE_PATHS = ("pkg/", "cmd/", "api/", "recipes/", "validators/", "charts/", "docs/design/")


def git(*args: str) -> bytes:
    """Run a bounded read-only Git operation in the source checkout."""
    return subprocess.run(
        ["git", *args], cwd=REPO, capture_output=True, check=True, timeout=30
    ).stdout


def commit(tag: str) -> str:
    """Resolve an exact stable tag to its commit, including annotated tags."""
    if not STABLE_TAG.fullmatch(tag):
        raise ValueError(f"expected a stable release tag such as v1.3.0, got {tag!r}")
    return git("rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}").decode("ascii").strip()


def matches(path: str, watch: str) -> bool:
    """Match an exact file or a directory ending in a slash."""
    return path.startswith(watch) if watch.endswith("/") else path == watch


def read_watch() -> dict[str, list[str]]:
    """Load explicit review paths and require coverage of every episode in the index."""
    raw = json.loads((VIDEOS / "review-paths.json").read_text(encoding="utf-8"))
    ids = INDEX_ID.findall((VIDEOS / "README.md").read_text(encoding="utf-8"))
    if not ids or len(ids) != len(set(ids)):
        raise ValueError("the video index must contain unique numbered episodes")
    if not isinstance(raw, dict) or set(raw) != set(ids):
        raise ValueError("review-paths.json must have one entry for every numbered episode")
    for episode, paths in raw.items():
        if not isinstance(paths, list) or not paths:
            raise ValueError(f"episode {episode} needs at least one review path")
        for value in paths:
            if not isinstance(value, str) or not value.strip():
                raise ValueError(f"episode {episode} has an empty review path")
            path = PurePosixPath(value)
            if path.is_absolute() or ".." in path.parts or value.rstrip("/") != path.as_posix() or value == ".":
                raise ValueError(f"episode {episode} has an invalid review path: {value!r}")
    return raw


def prepare(base: str, release: str) -> dict:
    """Return a deterministic review queue and unmapped feature changes."""
    base_commit, release_commit = commit(base), commit(release)
    changes = sorted(
        path.decode("utf-8", errors="surrogateescape")
        for path in git("diff", "--name-only", "--no-renames", "-z", base_commit, release_commit, "--").split(b"\0")
        if path
    )
    watched = read_watch()
    mapped = set()
    episodes = {}
    for episode, watches in sorted(watched.items()):
        hits = [path for path in changes if any(matches(path, watch) for watch in watches)]
        mapped.update(hits)
        episodes[episode] = {"review": bool(hits), "changedPaths": hits}
    return {
        "base": {"tag": base, "commit": base_commit},
        "release": {"tag": release, "commit": release_commit},
        "episodes": episodes,
        "unmappedFeatureChanges": [
            path for path in changes if path.startswith(FEATURE_PATHS) and path not in mapped
        ],
    }


def main() -> None:
    """Print the release review queue for local inspection."""
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("base", help="previous stable release tag present in this checkout")
    cli.add_argument("release", help="new stable release tag present in this checkout")
    args = cli.parse_args()
    try:
        plan = prepare(args.base, args.release)
    except (OSError, ValueError, subprocess.SubprocessError) as err:
        cli.error(str(err))
    print(json.dumps(plan, indent=2))


if __name__ == "__main__":
    main()

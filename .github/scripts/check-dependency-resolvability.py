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
"""Assert that module versions newly added to go.sum resolve publicly.

Companion to .github/workflows/dependency-resolvability.yaml, which explains why
this exists and what it deliberately does not prove. In short: Dependabot and
fork PRs cannot mint the OIDC credential the DGXC proxy needs, so the pre-merge
check available to them has to be unauthenticated. Both endpoints used here are.

Usage:  check-dependency-resolvability.py <base-sha>
"""

from __future__ import annotations

import email.utils
import fnmatch
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

PROXY = "https://proxy.golang.org"
SUMDB = "https://sum.golang.org"
TIMEOUT = 30
RETRIES = 3
BACKOFF_BASE_SECONDS = 1.0
MAX_RETRY_AFTER_SECONDS = 30.0
MAX_RESPONSE_BYTES = 1024 * 1024
RETRYABLE_STATUS = {408, 425, 429, 500, 502, 503, 504}

# `module version[/go.mod] h1:base64=`. Keep both hashes: the checksum database
# lookup returns the exact go.sum lines, so presence alone is not enough.
GOSUM_LINE = re.compile(r"^(\S+)\s+(\S+?)(/go\.mod)?\s+(h1:\S+)$")


def escape_proxy(value: str) -> str:
    """Apply Go's ASCII-only module proxy case encoding.

    Proxies serve case-insensitive filesystems, so Go escapes each uppercase
    letter as '!' + its lowercase form. Skipping this is the classic reason a
    perfectly valid module 404s: github.com/Azure/... must be requested as
    github.com/!azure/...
    """
    return "".join(f"!{c.lower()}" if "A" <= c <= "Z" else c for c in value)


def parse_go_sum(blob: str) -> dict[tuple[str, str], set[str]]:
    """Group canonical go.sum lines by module coordinate."""
    out: dict[tuple[str, str], set[str]] = {}
    for line in blob.splitlines():
        match = GOSUM_LINE.match(line)
        if not match:
            continue
        module, version, suffix, checksum = match.groups()
        out.setdefault((module, version), set()).add(
            f"{module} {version}{suffix or ''} {checksum}"
        )
    return out


def added_modules(base_sha: str) -> set[tuple[str, str, frozenset[str]]]:
    """Return coordinates with new go.sum lines and all current expected hashes."""

    def read(ref: str) -> dict[tuple[str, str], set[str]]:
        try:
            blob = subprocess.run(
                ["git", "show", f"{ref}:go.sum"],
                capture_output=True, text=True, check=True,
            ).stdout
        except subprocess.CalledProcessError:
            # No go.sum at that ref (new repo, or the file was just added).
            return {}
        return parse_go_sum(blob)

    head = read("HEAD")
    base = read(base_sha)
    return {
        (module, version, frozenset(sums))
        for (module, version), sums in head.items()
        if sums - base.get((module, version), set())
    }


def retry_delay(headers: object | None, attempt: int) -> float:
    """Return a bounded Retry-After delay or an exponential fallback."""
    value = headers.get("Retry-After") if headers is not None else None
    if value:
        try:
            delay = float(value)
        except ValueError:
            try:
                retry_at = email.utils.parsedate_to_datetime(value)
                if retry_at.tzinfo is None:
                    retry_at = retry_at.replace(tzinfo=timezone.utc)
                delay = (retry_at - datetime.now(timezone.utc)).total_seconds()
            except (TypeError, ValueError, OverflowError):
                delay = BACKOFF_BASE_SECONDS * (2**attempt)
        return min(max(delay, 0.0), MAX_RETRY_AFTER_SECONDS)
    return min(BACKOFF_BASE_SECONDS * (2**attempt), MAX_RETRY_AFTER_SECONDS)


def http_get(url: str) -> tuple[bytes | None, str]:
    """GET a bounded response, retrying only transient failures."""
    last = "unknown"
    for attempt in range(RETRIES):
        headers = None
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "aicr-dep-check"})
            with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
                if resp.status == 200:
                    body = resp.read(MAX_RESPONSE_BYTES + 1)
                    if len(body) > MAX_RESPONSE_BYTES:
                        return None, "response exceeds size limit"
                    return body, "200"
                last = f"HTTP {resp.status}"
                if resp.status not in RETRYABLE_STATUS:
                    return None, last
                headers = resp.headers
        except urllib.error.HTTPError as e:
            # 404/410 are verdicts, not transient — do not retry them.
            if e.code in (404, 410):
                return None, f"HTTP {e.code}"
            last = f"HTTP {e.code}"
            if e.code not in RETRYABLE_STATUS:
                return None, last
            headers = e.headers
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            last = type(e).__name__
        if attempt < RETRIES - 1:
            time.sleep(retry_delay(headers, attempt))
    return None, last


def matches_prefix_patterns(patterns: str, target: str) -> bool:
    """Match Go's comma-separated GOPRIVATE/GONOSUMDB prefix patterns."""
    for pattern in patterns.split(","):
        pattern = pattern.rstrip("/")
        if not pattern:
            continue
        prefix = "/".join(target.split("/")[: pattern.count("/") + 1])
        if fnmatch.fnmatchcase(prefix, pattern):
            return True
    return False


def check(mod_ver: tuple[str, str, frozenset[str]]) -> tuple[str, str, list[str]]:
    module, version, expected_sums = mod_ver
    escaped_module = escape_proxy(module)
    escaped_version = escape_proxy(version)
    problems: list[str] = []

    body, detail = http_get(f"{PROXY}/{escaped_module}/@v/{escaped_version}.info")
    if body is None:
        problems.append(f"not resolvable on the public proxy ({detail})")

    # GONOSUMDB defaults to GOPRIVATE in Go. The public proxy check remains
    # useful for a publicly mirrored coordinate, but an explicit checksum
    # exemption must not be treated as absence from sum.golang.org.
    no_sumdb = os.environ.get("GONOSUMDB") or os.environ.get("GOPRIVATE", "")
    if not matches_prefix_patterns(no_sumdb, module):
        body, detail = http_get(f"{SUMDB}/lookup/{escaped_module}@{escaped_version}")
        if body is None:
            problems.append(f"absent from the public checksum database ({detail})")
        else:
            recorded = set(body.decode("utf-8", errors="replace").splitlines())
            missing = sorted(expected_sums - recorded)
            if missing:
                problems.append(
                    "checksum database disagrees with go.sum (missing: "
                    + "; ".join(missing)
                    + ")"
                )

    return module, version, problems


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    base_sha = sys.argv[1]

    pending = sorted(added_modules(base_sha), key=lambda item: item[:2])
    if not pending:
        print("No module versions added to go.sum; nothing to verify.")
        return 0

    print(f"Verifying {len(pending)} newly added module version(s) against "
          f"{PROXY} and {SUMDB}\n")

    # Bounded: these are public shared services and this is not a load test.
    with ThreadPoolExecutor(max_workers=8) as pool:
        results = list(pool.map(check, pending))

    failed = [r for r in results if r[2]]
    for module, version, problems in results:
        mark = "FAIL" if problems else "ok  "
        print(f"  {mark}  {module}@{version}")
        for p in problems:
            print(f"          {p}")

    if failed:
        print(f"\n::error::{len(failed)} newly added module version(s) do not "
              f"resolve publicly. A dependency that cannot be fetched from the "
              f"public proxy cannot be mirrored by dgxc-go-virtual either, so "
              f"this would break the release build after merge.")
        return 1

    print(f"\nOK: all {len(pending)} added module version(s) resolve publicly "
          f"and match the checksum database.")
    return 0


if __name__ == "__main__":
    sys.exit(main())

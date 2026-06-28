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

"""Auto-generate under_investigation OpenVEX draft statements for new BOM scan findings.

Reads all Grype JSON outputs from bom-scan-results/, checks them against existing
statements in .openvex.json, and emits new under_investigation statements for any
(vuln-id, product-purl) pair not already covered.

Each scan artifact directory contains:
  <short-name>.ref  — raw image reference used to scan (e.g. nvcr.io/nvidia/foo:v1.2.3)
  <short-name>.json — Grype JSON output for that image

The script is idempotent: re-runs will not generate duplicate statements because
each new (vuln-id, product-purl) is checked against the existing .openvex.json.

Exit codes:
  0 — success (0 or more new statements generated)
  1 — fatal error (I/O failure or malformed input)
"""

import glob
import json
import os
import sys
from datetime import datetime, timezone


_MEDIUM_PLUS = {"Critical", "High", "Medium"}


def oci_purl(image_ref: str) -> str:
    """Compute an OCI package URL for a container image reference.

    Follows the purl-spec OCI type definition:
      pkg:oci/<name>@<digest>?repository_url=<registry/namespace/name>&tag=<tag>

    When no digest is present (tag-only reference), the digest qualifier is omitted.
    """
    ref = image_ref.strip()

    # Separate digest (@sha256:...) from the rest
    digest = ""
    if "@" in ref:
        ref, digest = ref.rsplit("@", 1)

    # Separate tag from registry/repository path
    tag = ""
    last_segment = ref.split("/")[-1]
    if ":" in last_segment:
        cut = ref.rfind(":")
        tag = ref[cut + 1:]
        ref = ref[:cut]

    # Image name is the final path component (no registry prefix, no tag/digest)
    name = ref.split("/")[-1]

    qualifiers = [f"repository_url={ref}"]
    if tag:
        qualifiers.append(f"tag={tag}")

    purl = f"pkg:oci/{name}"
    if digest:
        purl += f"@{digest}"
    purl += "?" + "&".join(qualifiers)

    return purl


def load_existing_keys(openvex_path: str) -> set:
    """Return the set of (vuln-name, product-purl) pairs already in .openvex.json.

    Keying by (vuln, product-purl) ensures statements for the same CVE on
    different images are treated as distinct entries.
    """
    keys: set = set()
    try:
        with open(openvex_path) as fh:
            doc = json.load(fh)
    except FileNotFoundError:
        return keys
    except json.JSONDecodeError as exc:
        print(f"Warning: could not parse {openvex_path}: {exc}", file=sys.stderr)
        return keys

    for stmt in doc.get("statements", []):
        vuln_name = stmt.get("vulnerability", {}).get("name", "")
        for product in stmt.get("products", []):
            # Accept both @id and identifiers.purl as the canonical product key
            product_id = (
                product.get("identifiers", {}).get("purl", "")
                or product.get("@id", "")
            )
            if vuln_name and product_id:
                keys.add((vuln_name, product_id))

    return keys


def collect_scan_pairs(results_dir: str) -> list:
    """Return [(image_ref, json_path)] pairs from the artifact download directory."""
    pairs = []
    for ref_file in glob.glob(f"{results_dir}/**/*.ref", recursive=True):
        json_file = ref_file[: -len(".ref")] + ".json"
        if not os.path.exists(json_file):
            print(
                f"Warning: .ref without matching .json — {ref_file}", file=sys.stderr
            )
            continue
        with open(ref_file) as fh:
            image_ref = fh.read().strip()
        if image_ref:
            pairs.append((image_ref, json_file))
    return pairs


def build_new_statements(
    scan_pairs: list,
    existing_keys: set,
) -> list:
    """Return a list of new OpenVEX under_investigation statement dicts."""
    new_stmts: list = []
    seen_keys: set = set()

    for image_ref, json_path in scan_pairs:
        product_purl = oci_purl(image_ref)

        try:
            with open(json_path) as fh:
                grype_data = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            print(f"Warning: could not read {json_path}: {exc}", file=sys.stderr)
            continue

        for match in grype_data.get("matches", []):
            severity = match.get("vulnerability", {}).get("severity", "")
            if severity not in _MEDIUM_PLUS:
                continue

            vuln_id = match["vulnerability"].get("id", "")
            if not vuln_id:
                continue

            key = (vuln_id, product_purl)
            if key in existing_keys or key in seen_keys:
                continue
            seen_keys.add(key)

            # Prefer the upstream description; fall back to a terse placeholder.
            vuln_desc = (
                match["vulnerability"].get("description")
                or f"{severity} severity; see upstream advisory for details."
            )

            new_stmts.append(
                {
                    "vulnerability": {
                        "name": vuln_id,
                        "description": vuln_desc,
                    },
                    "products": [
                        {
                            "@id": product_purl,
                            "identifiers": {"purl": product_purl},
                        }
                    ],
                    "status": "under_investigation",
                    "impact_statement": (
                        f"Auto-generated draft ({severity}) for {image_ref}. "
                        "Triage required: inspect source reachability, review "
                        "upstream advisory, and confirm whether a patch exists. "
                        "Update status to not_affected/affected/fixed with a "
                        "justification and evidence before resolving. "
                        "Do NOT merge without human review."
                    ),
                }
            )

    return new_stmts


def update_openvex(openvex_path: str, new_stmts: list, now_iso: str) -> None:
    """Append new_stmts to .openvex.json, incrementing version and timestamp."""
    with open(openvex_path) as fh:
        doc = json.load(fh)

    doc["version"] = doc.get("version", 0) + 1
    doc["timestamp"] = now_iso

    existing_tooling = doc.get("tooling", "")
    suffix = (
        f"auto-generated under_investigation drafts {now_iso}"
        " — human triage required before resolving"
    )
    doc["tooling"] = (existing_tooling + "; " + suffix) if existing_tooling else suffix

    doc["statements"] = doc.get("statements", []) + new_stmts

    with open(openvex_path, "w") as fh:
        json.dump(doc, fh, indent=2)
        fh.write("\n")


def main() -> int:
    openvex_path = ".openvex.json"
    results_dir = "bom-scan-results"

    existing_keys = load_existing_keys(openvex_path)
    scan_pairs = collect_scan_pairs(results_dir)

    if not scan_pairs:
        print("No scan result pairs found in bom-scan-results/; nothing to draft.")
        _write_output("new_count", "0")
        return 0

    new_stmts = build_new_statements(scan_pairs, existing_keys)
    count = len(new_stmts)
    _write_output("new_count", str(count))

    if count == 0:
        print("No new BOM findings require under_investigation VEX drafts.")
        return 0

    print(f"Generating {count} new under_investigation VEX draft statement(s).")

    now_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    try:
        update_openvex(openvex_path, new_stmts, now_iso)
    except (OSError, json.JSONDecodeError) as exc:
        print(f"Error updating {openvex_path}: {exc}", file=sys.stderr)
        return 1

    print(f"Updated {openvex_path} with {count} new statement(s).")
    return 0


def _write_output(key: str, value: str) -> None:
    """Append key=value to $GITHUB_OUTPUT if set."""
    github_output = os.environ.get("GITHUB_OUTPUT", "")
    if github_output:
        with open(github_output, "a") as fh:
            fh.write(f"{key}={value}\n")


if __name__ == "__main__":
    sys.exit(main())
    
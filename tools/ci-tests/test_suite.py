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
"""Exercise the shard and coverage integrity boundaries without running the suite."""

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import suite


class SuiteTest(unittest.TestCase):
    """Check fail-closed manifests, completion evidence, and coverage accounting."""

    def setUp(self):
        """Build three small successful shard artifacts."""
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.packages = [suite.MODULE + name for name in ("pkg/recipe", "pkg/errors", "pkg/new")]
        self.manifest = suite.partition(self.packages)
        for shard, entries in self.manifest.items():
            directory = self.root / shard
            directory.mkdir()
            (directory / "packages.txt").write_text("\n".join(entries) + "\n")
            (directory / "timings.json").write_text(json.dumps([
                {"Package": package, "Test": "", "Action": "pass", "Elapsed": 1} for package in entries
            ]))
            (directory / "coverage.full.out").write_text(
                f"mode: atomic\n{entries[0]}/source.go:1.1,2.2 2 1\n"
            )

    def test_partition(self):
        """New packages are assigned automatically and ordering is deterministic."""
        self.assertEqual(self.manifest, suite.partition(list(reversed(self.packages))))
        suite.verify_manifest(self.packages, self.manifest)
        self.assertEqual(self.manifest["recipe"], [suite.MODULE + "pkg/recipe"])

    def test_invalid_manifest(self):
        """Every missing, extra, duplicate, or empty shard must fail."""
        for kind in ("missing", "extra", "duplicate", "empty"):
            with self.subTest(kind=kind):
                manifest = {key: list(value) for key, value in self.manifest.items()}
                if kind == "missing":
                    del manifest["a"]
                elif kind == "extra":
                    manifest["a"].append(suite.MODULE + "unexpected")
                elif kind == "duplicate":
                    manifest["a"].extend(manifest["b"])
                else:
                    manifest["a"] = []
                with self.assertRaises(ValueError):
                    suite.verify_manifest(self.packages, manifest)

    def test_merge(self):
        """Deduplicate block statements, sum atomic counts, and exclude validators."""
        for shard in suite.SHARDS:
            with (self.root / shard / "coverage.full.out").open("a") as profile:
                profile.write(suite.MODULE + "shared/file.go:1.1,2.2 3 2\n")
                profile.write(suite.MODULE + "validators/example/file.go:1.1,2.2 8 0\n")
        output = self.root / "coverage.out"
        suite.merge(self.root, self.packages, output)
        result = output.read_text()
        self.assertEqual(result.count("mode: atomic"), 1)
        self.assertIn("shared/file.go:1.1,2.2 3 6\n", result)
        self.assertNotIn("validators/", result)

    def test_missing_artifacts(self):
        """Each required artifact is independently mandatory."""
        for filename in ("packages.txt", "timings.json", "coverage.full.out"):
            with self.subTest(filename=filename):
                path = self.root / "a" / filename
                data = path.read_text()
                path.unlink()
                with self.assertRaises(FileNotFoundError):
                    suite.merge(self.root, self.packages, self.root / "out")
                path.write_text(data)

    def test_incomplete_or_failed(self):
        """Reject truncated streams, duplicate completions, and failed tests."""
        path = self.root / "a" / "timings.json"
        original = json.loads(path.read_text())
        for rows in ([], original * 2, [dict(original[0], Action="fail")],
                     original + [dict(original[0], Test="TestFailure", Action="fail")]):
            with self.subTest(rows=rows):
                path.write_text(json.dumps(rows))
                with self.assertRaises(ValueError):
                    suite.merge(self.root, self.packages, self.root / "out")

    def test_invalid_profile(self):
        """Reject wrong modes, empty profiles, corrupt counts, and conflicting blocks."""
        path = self.root / "a" / "coverage.full.out"
        for profile in ("", "mode: atomic\n", "mode: set\nx 1 1\n", "mode: atomic\nx 1 -1\n",
                        "mode: atomic\nx 1 1\nx 2 1\n"):
            with self.subTest(profile=profile):
                path.write_text(profile)
                with self.assertRaises(ValueError):
                    suite.merge(self.root, self.packages, self.root / "out")

    def test_stream(self):
        """Failure output stays readable and both test and package timings survive."""
        events = [
            {"Action": "output", "Output": "assertion failed\n"},
            {"Action": "fail", "Package": "example", "Test": "TestOne", "Elapsed": 1.2},
            {"Action": "fail", "Package": "example", "Elapsed": 1.3},
        ]
        output = io.StringIO()
        destination = self.root / "summary.json"
        with patch("sys.stdin", io.StringIO("\n".join(json.dumps(e) for e in events))):
            with contextlib.redirect_stdout(output):
                suite.stream(destination)
        self.assertIn("assertion failed\n", output.getvalue())
        self.assertIn("1 packages, 1 tests", output.getvalue())
        self.assertEqual(len(json.loads(destination.read_text())), 2)


if __name__ == "__main__":
    unittest.main()

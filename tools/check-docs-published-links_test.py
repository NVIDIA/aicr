# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for the published documentation link checker."""

from __future__ import annotations

import importlib.machinery
import importlib.util
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("check-docs-published-links")
LOADER = importlib.machinery.SourceFileLoader("check_docs_published_links", str(MODULE_PATH))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class PublishedDocsLinkTest(unittest.TestCase):
    def test_relative_target_resolves_against_source_page(self) -> None:
        self.assertEqual(
            MODULE.relative_target("docs/user/guide.md", "../contributor/index.md#architecture"),
            "docs/contributor/index.md",
        )

    def test_github_blob_target_extracts_repository_path(self) -> None:
        self.assertEqual(
            MODULE.github_blob_target("https://github.com/NVIDIA/aicr/blob/main/README.md"),
            "README.md",
        )

    def test_fenced_code_is_not_treated_as_a_link(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            page = Path(directory) / "page.md"
            page.write_text("````\n[not a link](missing.md)\n````\n[real](real.md)\n", encoding="utf-8")
            self.assertEqual(MODULE.markdown_links(page), ["real.md"])


if __name__ == "__main__":
    unittest.main()

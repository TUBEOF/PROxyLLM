#!/usr/bin/env python3
"""Negative self-tests for the matrix gates.

The gate itself runs in CI; these controls prove it rejects the mistakes it
exists for: a missing test, a pending cell without a reason, a second claim on
an already-claimed test, and an extra claiming a runner.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import check_manifest
import check_cli_manifest


class CheckCellTests(unittest.TestCase):
    def test_duplicate_extra_claim_is_rejected(self) -> None:
        claimed: set[str] = set()
        tests = {"TestA"}
        first = {"id": "first", "status": "green", "test": "TestA"}
        second = {"id": "second", "status": "green", "test": "TestA"}
        self.assertEqual(check_manifest.check_cell(first, tests, claimed), [])
        self.assertIn("TestA", claimed)
        self.assertTrue(
            check_manifest.check_cell(second, tests, claimed),
            "a second claim on the same test must fail",
        )

    def test_missing_test_is_rejected(self) -> None:
        problems = check_manifest.check_cell(
            {"id": "x", "status": "green", "test": "TestMissing"}, set(), set()
        )
        self.assertTrue(problems)

    def test_pending_requires_reason(self) -> None:
        problems = check_manifest.check_cell({"id": "x", "status": "pending"}, set(), set())
        self.assertTrue(problems)

    def test_runner_name_cannot_be_claimed(self) -> None:
        problems = check_manifest.check_cell(
            {"id": "x", "status": "green", "test": "TestMatrix_TextPairs"},
            {"TestMatrix_TextPairs"},
            {"TestMatrix_TextPairs"},
        )
        self.assertTrue(problems)


class CheckCLIManifestTests(unittest.TestCase):
    def manifest(self) -> dict:
        return {
            "schema": 1,
            "clients": ["codex"],
            "scenarios": ["answer"],
            "shards": {"basic": ["answer"]},
            "cells": [{"client": "codex", "scenario": "answer", "status": "green"}],
        }

    def test_valid_manifest_passes(self) -> None:
        problems = check_cli_manifest.validate(self.manifest(), {"answer"}, "shard: basic")
        self.assertEqual(problems, [])

    def test_missing_pair_is_rejected(self) -> None:
        manifest = self.manifest()
        manifest["cells"] = []
        self.assertTrue(check_cli_manifest.validate(manifest, {"answer"}, "shard: basic"))

    def test_skipped_without_reason_is_rejected(self) -> None:
        manifest = self.manifest()
        manifest["cells"][0] = {"client": "codex", "scenario": "answer", "status": "skipped"}
        self.assertTrue(check_cli_manifest.validate(manifest, {"answer"}, "shard: basic"))

    def test_green_scenario_without_function_is_rejected(self) -> None:
        self.assertTrue(check_cli_manifest.validate(self.manifest(), set(), "shard: basic"))

    def test_green_scenario_without_shard_is_rejected(self) -> None:
        manifest = self.manifest()
        manifest["shards"] = {}
        self.assertTrue(check_cli_manifest.validate(manifest, {"answer"}, "shard: basic"))

    def test_shard_missing_from_workflow_is_rejected(self) -> None:
        self.assertTrue(check_cli_manifest.validate(self.manifest(), {"answer"}, ""))


if __name__ == "__main__":
    unittest.main()

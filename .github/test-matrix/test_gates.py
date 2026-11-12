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


if __name__ == "__main__":
    unittest.main()

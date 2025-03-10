"""
Simplified POSIX only version of internal `diagnostics_test_utils.py` used to run the tests locally
TODO unify integration testing code across the public and private repositories
"""

import json
import pathlib
import subprocess
import os
import difflib
import sys


def _normalize_actual(test_dir, database_dir):
    proc = subprocess.run(['codeql', 'database', 'export-diagnostics', '--format', 'raw', '--', database_dir],
                          stdout=subprocess.PIPE, universal_newlines=True, check=True, text=True)
    data = proc.stdout.replace(str(test_dir.absolute()), "<test-root-directory>")
    data = json.loads(data)
    data[:] = [e for e in data if not e["source"]["id"].startswith("cli/")]
    for e in data:
        e.pop("timestamp")
    return _normalize_json(data)


def _normalize_expected(test_dir):
    with open(test_dir / "diagnostics.expected") as expected:
        text = expected.read()
    return _normalize_json(_load_concatenated_json(text))


def _load_concatenated_json(text):
    text = text.lstrip()
    entries = []
    decoder = json.JSONDecoder()
    while text:
        obj, index = decoder.raw_decode(text)
        entries.append(obj)
        text = text[index:].lstrip()
    return entries


def _normalize_json(data):
    entries = [json.dumps(e, sort_keys=True, indent=2) for e in data]
    entries.sort()
    entries.append("")
    return "\n".join(entries)


def check_diagnostics(test_dir=".", test_db="db"):
    test_dir = pathlib.Path(test_dir)
    test_db = pathlib.Path(test_db)
    actual = _normalize_actual(test_dir, test_db)
    if os.environ.get("CODEQL_INTEGRATION_TEST_LEARN") == "true":
        with open(test_dir / "diagnostics.expected", "w") as expected:
            expected.write(actual)
        return
    expected = _normalize_expected(test_dir)
    if actual != expected:
        with open(test_dir / "diagnostics.actual", "w") as actual_out:
            actual_out.write(actual)
        actual = actual.splitlines(keepends=True)
        expected = expected.splitlines(keepends=True)
        print("".join(difflib.unified_diff(actual, expected, fromfile="diagnostics.actual", tofile="diagnostics.expected")), file=sys.stderr)
        sys.exit(1)

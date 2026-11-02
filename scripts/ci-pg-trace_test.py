"""CLI regression cases for the Postgres connection evidence extractor."""

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "ci-pg-trace.py"
MODULE = "github.com/PerpetualSoftware/pad"


def event(package, output="PAD_PG_CONNECTED helper=probe\n", action="output"):
    return {"Action": action, "Package": package, "Test": "TestProbe", "Output": output}


class TraceTest(unittest.TestCase):
    def test_cli(self):
        cases = [
            ("empty", [], None, 0, [], []),
            ("sort and deduplicate", [event(MODULE + "/internal/store"),
                                      event(MODULE + "/cmd/pad"),
                                      event(MODULE + "/internal/store")],
             None, 0, [MODULE + "/cmd/pad", MODULE + "/internal/store"], []),
            ("only output events with packages",
             [event("ignored", action="pass"), event("ignored", "ordinary log\n"),
              {"Action": "output", "Output": "PAD_PG_CONNECTED helper=probe"}],
             None, 0, [], []),
            ("relative and import paths",
             [event(MODULE + "/cmd/pad"), event(MODULE + "/internal/store")],
             "# candidate\n\n./cmd/pad/\n" + MODULE + "/internal/store\n",
             0, [MODULE + "/cmd/pad", MODULE + "/internal/store"], []),
            ("unexpected and missing",
             [event(MODULE + "/internal/server"), event(MODULE + "/cmd/pad")],
             "./internal/store\n",
             1, [MODULE + "/cmd/pad", MODULE + "/internal/server"],
             ["unexpected: " + MODULE + "/cmd/pad",
              "unexpected: " + MODULE + "/internal/server",
              "warning: expected package never connected: " + MODULE + "/internal/store"]),
            ("missing is only a warning", [], "./internal/store\n", 0, [],
             ["warning: expected package never connected: " + MODULE + "/internal/store"]),
            ("package output without a test",
             [{"Action": "output", "Package": MODULE + "/internal/store",
               "Output": "PAD_PG_CONNECTED helper=probe\n"}], "./internal/store\n",
             0, [MODULE + "/internal/store"], []),
        ]
        for name, events, expected, code, packages, diagnostics in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                args = [sys.executable, str(SCRIPT)]
                if expected is not None:
                    expect = Path(tmp) / "expected.txt"
                    expect.write_text(expected)
                    args += ["--expect", str(expect)]
                result = subprocess.run(
                    args, input="".join(json.dumps(row) + "\n" for row in events),
                    text=True, capture_output=True, cwd=tmp, check=False,
                )
                self.assertEqual(result.returncode, code, result.stderr)
                self.assertEqual(result.stdout.splitlines(), packages)
                self.assertEqual(result.stderr.splitlines(), diagnostics)


class HumanOutputTest(unittest.TestCase):
    def test_output_can_be_mirrored_without_losing_trace(self):
        row = event(MODULE + '/internal/store', 'ordinary test output\n')
        result = subprocess.run([sys.executable, str(SCRIPT), '--show-output'],
                                input=json.dumps(row) + '\n', text=True,
                                capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'ordinary test output\n')


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Print packages with successful test Postgres connections from go test -json.

Usage: PAD_TEST_PG_TRACE=1 go test -json ... | python3 scripts/ci-pg-trace.py \
    --expect scripts/ci-pg-packages.txt

With --expect, the connecting packages must EQUAL the list. A package that
connected but is not listed, a listed package that never connected, or no
connection at all exits 1, so the guard cannot pass vacuously when the trace
marker stops being emitted. Without --expect it only prints the packages.

This command does not check the test run's exit status. Preserve that
separately (for example, with shell pipefail).
"""

import argparse
import json
from pathlib import Path
import posixpath
import sys


def normalize_package(package, module):
    if package == "." or package.startswith("./"):
        package = module + "/" + package
    return posixpath.normpath(package)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--expect", type=Path, help="expected package list")
    parser.add_argument("--show-output", action="store_true",
                        help="also stream human-readable go test Output events")
    args = parser.parse_args()

    connected = set()
    try:
        for number, line in enumerate(sys.stdin, 1):
            if not line.strip():
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                parser.error(f"invalid JSON on stdin line {number}")
            if args.show_output and event.get("Action") == "output":
                print(event.get("Output", ""), end="", flush=True)
            if (event.get("Action") == "output"
                    and "PAD_PG_CONNECTED" in event.get("Output", "")
                    and event.get("Package")):
                connected.add(event["Package"])

        expected = None
        if args.expect is not None:
            go_mod = Path(__file__).resolve().parent.parent / "go.mod"
            module = next(line.split()[1] for line in go_mod.read_text().splitlines()
                          if line.startswith("module "))
            expected = {normalize_package(line.strip(), module)
                        for line in args.expect.read_text().splitlines()
                        if line.strip() and not line.lstrip().startswith("#")}
    except OSError as err:
        parser.error(str(err))

    for package in sorted(connected):
        print(package)
    if expected is None:
        return 0
    unexpected = connected - expected
    missing = expected - connected
    for package in sorted(unexpected):
        print(f"unexpected: {package}", file=sys.stderr)
    for package in sorted(missing):
        print(f"expected package never connected: {package}", file=sys.stderr)
    if not connected:
        print("no PAD_PG_CONNECTED markers: the trace saw no connection at all", file=sys.stderr)
    return 1 if unexpected or missing or not connected else 0


if __name__ == "__main__":
    sys.exit(main())

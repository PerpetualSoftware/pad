#!/usr/bin/env python3
"""Print packages with successful test Postgres connections from go test -json.

Usage: PAD_TEST_PG_TRACE=1 go test -json ... | python3 scripts/ci-pg-trace.py \
    --expect scripts/ci-pg-packages.txt

The candidate list is evidence only; this command does not check the test run's
exit status. Preserve that separately (for example, with shell pipefail).
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
    if expected is not None:
        for package in sorted(connected - expected):
            print(f"unexpected: {package}", file=sys.stderr)
        for package in sorted(expected - connected):
            print(f"warning: expected package never connected: {package}", file=sys.stderr)
        if connected - expected:
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Print a `go test -run` regex naming the tests of one package that touch a
PostgreSQL store, for `make test-pg` (TASK-4402).

`testServer` is SQLite even under `make test-pg`, so most of internal/server's
tests would only run the SQLite suite a second time. A test touches PostgreSQL
if its body, or the body of a function it calls (any depth, by name), contains
one of the seed tokens below. CI's PostgreSQL jobs do not use this: they set
PAD_TEST_POSTGRES_URL themselves and run the whole package.

usage: pg-test-run.py <package dir>   (prints ^(TestA|TestB|...)$ ; nothing and exit 1 if none)
"""
import glob
import re
import sys

SEEDS = ["bothBackends", "pg=%t", "testServerPostgres", "NewPostgres", "PAD_TEST_POSTGRES_URL", "testStorePostgres"]
FUNC = re.compile(r"^func (?:\([^)]*\) )?(\w+)\(", re.M)


def functions(path):
    src = open(path, encoding="utf-8", errors="replace").read()
    heads = list(FUNC.finditer(src))
    for i, m in enumerate(heads):
        end = src.find("\n}\n", m.end())
        yield m.group(1), src[m.start():end if end >= 0 else len(src)]


def touches(body, helpers):
    """A seed matches as a PREFIX (NewPostgres also matches NewPostgresBounded);
    a helper function name found by the fixpoint matches exactly."""
    seeds = r"(?<![\w])(?:%s)" % "|".join(map(re.escape, SEEDS))
    if re.search(seeds, body):
        return True
    return bool(helpers) and re.search(r"(?<![\w])(?:%s)(?![\w])" % "|".join(map(re.escape, helpers)), body) is not None


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    funcs = {}
    for path in glob.glob(sys.argv[1].rstrip("/") + "/*.go"):
        for name, body in functions(path):
            funcs.setdefault(name, []).append(body)
    tokens = set()  # helper names found below, matched exactly
    changed = True
    while changed:
        changed = False
        for name, bodies in funcs.items():
            if name in tokens or name.startswith("Test"):
                continue
            if any(touches(b, tokens) for b in bodies):
                tokens.add(name)
                changed = True
    
    tests = sorted(n for n, bs in funcs.items() if n.startswith("Test") and any(touches(b, tokens) for b in bs))
    if not tests:
        sys.exit(1)
    print("^(" + "|".join(tests) + ")$")


main()

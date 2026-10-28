# Provenance

Vendored from github.com/teambition/rrule-go, version v1.8.2, upstream commit
429348ca4477691b08edb0200b7f837edd32414c (module proxy v1.8.2.info, Origin.Hash).
License: MIT (LICENSE, copied verbatim from the module).

## Why it is vendored

Its Next() is unbounded on a no-match stretch (TASK-4100 H1): UNTIL is checked
only against produced candidates, so a rule that matches rarely or never spins
inside one Next() until year 9999. The caller cannot interrupt it. Upstream has
been unmaintained since 2023-01. The conformance suite in internal/schedule is
the contract. Do not "upgrade" this back to the module.

The differences from upstream marked TASK-4100 in rrule.go are:

- The per-iterator step budget: the `budget` field, SetBudget and Exhausted
  (TASK-4100 H1).
- The `steps` counter, which resets at every produced candidate so the budget
  bounds a no-match stretch (TASK-4100 r2).
- The `stall` position, StallPos and spend(adv), which record a conservative
  lower bound on every occurrence an exhausted iterator did not yield
  (TASK-4100 r3).

rruleset.go, str.go, util.go and LICENSE are byte-identical to upstream: their
sha256 matches the table below. rrule.go is the only modified file.

Lint: .golangci.yml excludes one upstream finding, ineffassign on `lno1wkst = 0`
in rrule.go (upstream's own dead store), rather than editing upstream code. Every
other linter still runs on this package, including our delta (TASK-4100 lint ruling).

## File count

The brief for this work said 3 library files; there are 4 (rrule.go, rruleset.go,
str.go, util.go). Tests, README, Makefile and example are not vendored.

## sha256 of the originals (module cache, v1.8.2)

```
3af19c18cb8ca1fde2082eb35847c20eced3ecdecae1c359e6bb591a8e568fda  rrule.go
89f1c9ca8eee08fefbeb388c3d1b7605ef6db0f4f665a74e4c208e6b97edcad8  rruleset.go
0dc2d364239a55d67956fc34c7e45cd84c0a3f5f69e033d83047362b629c7d96  str.go
d95c565b665cf0a9c92851c1b32dd3390ca5fb350e8553cf55e48323f1bc0173  util.go
2c5aa8001e37c2ba4488e1c809772c9372623f2eaa0605b9abd8863cf7a3470f  LICENSE
```

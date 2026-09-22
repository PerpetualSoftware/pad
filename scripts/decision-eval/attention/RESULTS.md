# Attention threshold eval — TASK-3137 (2026-09-22)

Re-run of the day-73 decision-detector eval (eval2.py section D) on the SAME
140 items: 70 human-tasks as positives, and the day-73 run's 70 randomly drawn
tasks as negatives. Model jev-1.13.0, question `needs_human_decision` verbatim.
One call per item per mode, 0 errors. Per-item JSON is kept locally, not
committed.

| mode | state sent | AUC | P / R at 0.5 | P / R at 0.6 | P / R at 0.7 | P / R at 0.8 |
|---|---|---|---|---|---|---|
| day-73 (recorded) | title + body | 0.939 | — | — | — | — |
| titlebody (control, today) | title + body | 0.935 | 0.945 / 0.743 | 0.930 / 0.571 | 0.944 / 0.486 | 0.958 / 0.329 |
| production | `BuildItemState` | 0.854 | 0.897 / 0.371 | 0.833 / 0.214 | 0.750 / 0.129 | 0.750 / 0.043 |
| nostatus (ablation) | production − fields.status | 0.887 | 0.881 / 0.529 | 0.889 / 0.457 | 0.885 / 0.329 | 0.778 / 0.100 |
| nostatus-notrail (ablation) | production − status − trail | 0.935 | 0.927 / 0.729 | 0.936 / 0.629 | 0.949 / 0.529 | 1.000 / 0.371 |
| **r1, replay at last open moment (DECIDES)** | `BuildItemState` over the item as of just before its first logged terminal transition | 0.885 | 0.850 / 0.750 | 0.846 / 0.647 | 0.825 / 0.485 | 0.833 / 0.294 |
| r0, replay at filing | `BuildItemState` over the oldest version, initial status, no trail | 0.931 | 0.926 / 0.735 | 0.935 / 0.632 | 0.971 / 0.485 | 1.000 / 0.368 |

The r0 and r1 rows cover 138 items: 68 positives and 70 negatives. Two positives are
excluded from each: in r1, items closed with no logged terminal transition; in
r0, items created terminal. The spec and rule were pre-registered on TASK-3137,
checkpoint 3, before any replay call.

## Outcome (lead-ruled rule: highest t with P >= 90% and R >= 60% on r1)

**No threshold passes on r1**: precision never reaches 90%. Per the ruling, this
is a finding about the question set, and the constant is NOT changed. r0 would
pass at 0.6 (P 0.935 / R 0.632), but it does not decide: production asks
throughout an item's open life, not only at filing.

What the r1 false positives are: 6 of the 9 at t=0.5 are negatives that RISE
by more than 0.2 from r0 to r1, all with a trail, and 5 of them are in-progress.
At its last open moment an ordinary task often IS waiting on a human or
someone external. TASK-1717's last comment before closing is "leaving
in-progress until the shared E2E device matrix", a device test only a person
can run. TASK-1668 was "paused before merge — Codex CLI hit its usage limit",
an external wait. The labels say which collection an item was FILED in, and
they cannot see such transient waits. Setting a threshold for the dashboard
needs present-tense labels at the replayed moment, not collection membership.

## Reading (the first four modes)

- **No model drift.** The control reproduces day-73 (0.935 vs 0.939).
- **The production gap is the labels, not the question.** 68 of the 70
  positives, and 50 of the 70 negatives, are now `status: done`. The production
  state carries the present status and the resolution trail, and the model
  answers what it is asked: a finished item is not waiting on anyone NOW. The
  ablation was pre-registered on TASK-3137. Removing status and trail recovers
  AUC fully, 0.935, and removing status alone recovers it part way, 0.887.
- **So this set cannot validate a threshold for production.** The labels say
  "was filed as a human-task", which is historical; the `attention` set asks
  only about OPEN items and asks about the present. The open subset is 22
  items, only 2 of them positive.
- The pre-registered choice rule (precision >= 90% and recall >= 60% on the
  production mode) is met by no threshold, so `AttentionThreshold` is NOT
  changed by this run.
- Probe items HT-1177 and HT-544 score 0.70 and 0.76 on production, but both
  are done, and production never asks about them. TASK-3118's 0.71 probe
  measured the same confound.

## Reproduce

    go run ./scripts/decision-eval/attention -dump <dir> -mode <mode> -out run.json
    go run ./scripts/decision-eval/attention -dump <dir> -out /dev/null -dry   # instrument check

`<dir>` holds `population.json` (ref, truth, day73_p, from the day-73
results-decision.json) and `items/<ref>.json` + `items/<ref>.comments.json`
from `pad item show|comments --format json`. `-dry` builds every state without
calling the provider, and must reproduce the day-73 AUC of 0.939 from the
recorded probabilities.

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

## TASK-3957 and TASK-3921 (2026-10-19)

The same 140 items and labels, with the dump rebuilt from the day-73
results-decision.json and the live items. `-dry` reproduces 0.939, and every
run had 0 errors. r1 excludes the same 2 positives as above (closed with no
logged terminal transition). Model `jev-1.13.0`. Only needs_human_decision is
scored. Each run prints the question keys it sends (`questions per call=`).

- TASK-3957: the ONE-question call after TASK-3915 (`blocked` and
  `waiting_on_external` retired), run at e432d0f5b.
- TASK-3921: the FOUR-question call, adding the staged effort, effort_medium
  and high_stakes.
- `titlebody` is unchanged: it sends the day-73 pair (needs_human_decision +
  `blocked`) and remains the model-drift control.
- `titlebody-live` (new) sends the live set on the same {title, body} state, so
  it measures the call shape without the production state. The TASK-3921 gate
  is needs_human_decision within .02 of 0.939 there.

| mode | questions per call | AUC | P / R at 0.5 | P / R at 0.7 |
|---|---|---|---|---|
| titlebody (drift control) | 2 (day-73 pair) | 0.934 | 0.927 / 0.729 | 0.919 / 0.486 |
| production, TASK-3957 | 1 | 0.871 | 0.917 / 0.314 | 0.818 / 0.129 |
| r1, TASK-3957 | 1 | 0.853 | 0.794 / 0.735 | 0.791 / 0.500 |
| titlebody-live, TASK-3921 (gate) | 4 | 0.936 | 0.944 / 0.729 | 0.944 / 0.486 |
| production, TASK-3921 | 4 | 0.865 | 0.923 / 0.343 | 0.800 / 0.114 |
| r1, TASK-3921 | 4 | 0.856 | 0.810 / 0.750 | 0.775 / 0.456 |

Reading:

- No model drift: the control gives 0.934, against 0.935 on 09-22.
- The gate passes. titlebody-live gives 0.936, 0.003 below 0.939, and is not
  measurably different from the two-question control.
- Adding the three staged questions does not move needs_human_decision on the
  production or replayed state: production went 0.871 to 0.865 and r1 went
  0.853 to 0.856.
- r1 is 0.03 below its 09-22 figure (0.885, three questions) under both the one-
  and four-question calls, so it predates this change. These runs cannot say
  whether the TASK-3915 call shape or another runner change since 09-22
  opened it. Production compares only within this table, because its
  states are live and have moved since 09-22.

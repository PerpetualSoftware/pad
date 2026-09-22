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

## Reading

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

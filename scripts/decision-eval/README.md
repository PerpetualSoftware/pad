# Decision-provider evals

The scripts behind PLAN-3114's go/no-go decision. They measure the decision
provider (typesafe.ai Jev) against human-set labels in the docapp workspace,
so a model pin change (`internal/decision.DefaultModel`) can be measured
against a recorded baseline instead of argued about.

| script | what it asks | ground truth |
|---|---|---|
| `eval.py` | **A.** field recovery: severity/component (bugs), priority/effort (tasks), impact (ideas), as Choice questions. **B.** collection classification: bug, task or idea, with the collection hidden. | the human-set field / the real collection |
| `eval2.py` | **C.** CONVE-4 (conventional commits) as a Noul over 200 real commit subjects plus up to 60 mutated negatives. **D.** `needs_human_decision` / `blocked` as Nouls over human-tasks (positives) vs 70 random tasks (negatives). | a regex (C); the collection the item lives in (D) |
| `attention/` | TASK-3137's replay of D through the production state builder; see its `RESULTS.md`. | as D |

Both Python scripts call `https://api.typesafe.ai/v1/systemone` directly
(standard library only) and send item titles and bodies there, which is the
same disclosure the product makes when the provider is enabled.

## Running

```bash
export TYPESAFE_API_KEY=...          # read from the environment, never printed
cd scripts/decision-eval
# 1. regenerate the corpora (below); they must sit NEXT TO the scripts
# 2.
python3 eval.py --dry-run            # builds the jobs and prints one request; no API call
python3 eval.py --model jev-1.13.0   # A + B; writes results-<model>.jsonl
python3 eval2.py                     # C + D; pinned to jev-1.13.0; writes results-decision.json
```

The scripts read **`TYPESAFE_API_KEY`**, the vendor's name. Pad itself reads
**`PAD_TYPESAFE_API_KEY`** (see the README's *Decision provider* section).
They are deliberately separate, so an eval run never borrows a server's
credentials by accident.

`eval.py` defaults to `--model jev-latest`, which is what the day-73 A/B run
used. `jev-latest` moves without notice, so pass the pinned model to compare
like with like. `eval2.py` has no `--dry-run`: importing it reads the key and
calls the API.

## Regenerating the corpora

The corpora are exports of the docapp workspace and are **never committed**
(`.gitignore`). From a checkout linked to the docapp workspace:

```bash
pad item list bugs        --all --full --limit 1000 --format json > bugs.json
pad item list tasks       --all --full --limit 1000 --format json > tasks.json
pad item list ideas       --all --full --limit 1000 --format json > ideas.json
pad item list human-tasks --all --full --limit 1000 --format json > human-tasks.json
pad collection list --format json                                 > collections.json
git log --format=%s -n 400                                        > commits.txt
```

`--all` is required, because the day-73 corpora include done and closed items,
and `--full` supplies the bodies. **A regenerated corpus is today's workspace,
not day 73's**: at day 73 there were 557 bugs, 1000 tasks (capped by
`--limit`), 433 ideas, 70 human-tasks and 400 commit subjects, and the
workspace has grown since. The random draws are seeded (`73`), but over
different populations, so compare RATES, not item-level answers.

## Day-73 baseline (2026-09-19)

A, B and D were recomputed from the saved result files with the scripts' own
summary logic, and match the figures recorded on PLAN-3114. C's results were
not saved; its figures are from PLAN-3114's trail.

**A. field recovery** (`jev-latest`, 360 items; exact = the human's value, adjacent = within one level)

| field | n | exact | adjacent | conf >= 0.8 correct | majority baseline |
|---|---|---|---|---|---|
| bugs.severity | 114 | 42.1% | 86.8% | 15 / 27 | 43.0% (low) |
| bugs.component | 112 | 42.0% | 42.0% | 41 / 83 | 21.4% (web) |
| tasks.priority | 120 | 56.7% | 95.8% | 24 / 36 | 45.0% (medium) |
| tasks.effort | 33 | 51.5% | 97.0% | 4 / 6 | 45.5% (m) |
| ideas.impact | 120 | 56.7% | 92.5% | 26 / 41 | 47.5% (medium) |

**B. collection classification** (`jev-latest`): 278 / 360 = 77.2% overall; 217 / 250 = 86.8% where confidence >= 0.8.

**C. CONVE-4 Noul** (`jev-1.13.0`, 260 subjects): AUC 0.971; at threshold 0.9, precision 98.8% and recall 92.3%.

**D. `needs_human_decision` Noul** (`jev-1.13.0`, 70 + 70 items, title + body)

| AUC | P / R at 0.5 | P / R at 0.7 | P / R at 0.9 |
|---|---|---|---|
| 0.939 | 93.0% / 75.7% | 94.4% / 48.6% | 100.0% / 21.4% |

What those numbers led to is on PLAN-3114: subjective scales are not worth
automating (tranche 2 chips only at >= 0.8); objective Nouls are. TASK-3137
re-ran D through the production state builder.

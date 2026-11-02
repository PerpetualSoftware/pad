# Decision-provider evals

These scripts measure the decision provider (typesafe.ai Jev) against
human-set labels in Pad's own project workspace, so a model pin change
(`internal/decision.DefaultModel`) can be measured against a recorded
baseline (see Baseline below) instead of argued about.

| script | what it asks | ground truth |
|---|---|---|
| `eval.py` | **A.** field recovery: severity/component (bugs), priority/effort (tasks), impact (ideas), as Choice questions. **B.** collection classification: bug, task or idea, with the collection hidden. | the human-set field / the real collection |
| `eval2.py` | **C.** CONVE-4 (conventional commits) as a Noul over 200 real commit subjects plus up to 60 mutated negatives. **D.** `needs_human_decision` / `blocked` as Nouls over human-tasks (positives) vs 70 random tasks (negatives). | a regex (C); the collection the item lives in (D) |
| `attention/` | TASK-3137's replay of D through the production state builder; usage is in its package comment (`main.go`), results are on DOC-4261. | as D |

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

`eval.py` defaults to `--model jev-latest`, which is what the 2026-09-19
baseline run used. `jev-latest` moves without notice, so pass the pinned model to compare
like with like. `eval2.py` has no `--dry-run`: its code runs at the top level,
so running it at all reads the key and makes the API calls.

## Regenerating the corpora

The corpora are exports of Pad's own project workspace and are **never
committed** (`.gitignore`). From a checkout linked to that workspace:

```bash
pad item list bugs        --all --full --limit 1000 --format json > bugs.json
pad item list tasks       --all --full --limit 1000 --format json > tasks.json
pad item list ideas       --all --full --limit 1000 --format json > ideas.json
pad item list human-tasks --all --full --limit 1000 --format json > human-tasks.json
pad collection list --format json                                 > collections.json
git log --format=%s -n 400                                        > commits.txt
```

`--all` is required, because the baseline corpora include done and closed
items, and `--full` supplies the bodies. **A regenerated corpus is today's
workspace, not the baseline's**: on 2026-09-19 there were 557 bugs, 1000 tasks (capped by
`--limit`), 433 ideas, 70 human-tasks and 400 commit subjects, and the
workspace has grown since. The random draws are seeded (`73`), but over
different populations, so compare RATES, not item-level answers.

## Baseline

The 2026-09-19 baseline figures for A to D, and what they led to, are on
PLAN-3114. DOC-4261 holds a copy of them with the attention-threshold runs
(TASK-3137, TASK-3957, TASK-3921).

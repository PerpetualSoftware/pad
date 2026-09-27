#!/usr/bin/env python3
"""Eval 2: objective questions.

C. Executable convention — CONVE-4 (conventional commit format) as a Noul over
   real commit subjects; ground truth by regex; plus mutated negatives.
D. Decision detector — "waiting on a decision only a human can make" as a Noul
   over docapp human-tasks (positives) vs tasks (negatives).
"""
import json, os, random, re, sys, time, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed

HERE = os.path.dirname(os.path.abspath(__file__))
API = "https://api.typesafe.ai/v1/systemone"
KEY = os.environ["TYPESAFE_API_KEY"]
MODEL = "jev-1.13.0"
CC_RE = re.compile(r'^(feat|fix|docs|chore|refactor|test|ci|build|perf|style|revert)(\([^)]+\))?!?: \S')

CONVE4 = "Use conventional commit format for all commit messages: feat:, fix:, docs:, refactor:, test:, chore:. Include scope when relevant, e.g. feat(api): add user endpoint"


def call(state, questions, retries=5):
    body = json.dumps({"model": MODEL, "state": state, "questions": questions}).encode()
    req = urllib.request.Request(API, data=body, method="POST", headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"})
    delay = 1.0
    for attempt in range(retries):
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            if e.code in (429, 529) and attempt < retries - 1:
                time.sleep(delay); delay *= 2; continue
            raise RuntimeError(f"HTTP {e.code}: {e.read().decode(errors='replace')[:200]}")
    raise RuntimeError("retries exhausted")


def run(jobs, workers=8):
    out = []
    with ThreadPoolExecutor(workers) as ex:
        futs = {ex.submit(call, st, q): meta for meta, st, q in jobs}
        for f in as_completed(futs):
            meta = futs[f]
            try:
                out.append((meta, f.result()))
            except Exception as e:
                print("  error", meta.get("id"), e, file=sys.stderr)
    return out


def auc(pos, neg):
    # probability a random positive scores above a random negative
    wins = sum((p > n) + 0.5 * (p == n) for p in pos for n in neg)
    return wins / (len(pos) * len(neg))


def report(name, rows, thresholds=(0.5, 0.7, 0.9)):
    pos = [p for ok, p in rows if ok]; neg = [p for ok, p in rows if not ok]
    print(f"\n{name}: n={len(rows)} (pos={len(pos)} neg={len(neg)}) AUC={auc(pos, neg):.3f}  mean p: pos={sum(pos)/len(pos):.2f} neg={sum(neg)/len(neg):.2f}")
    for t in thresholds:
        tp = sum(p >= t for p in pos); fp = sum(p >= t for p in neg)
        fn = len(pos) - tp; tn = len(neg) - fp
        prec = tp / max(1, tp + fp); rec = tp / max(1, len(pos))
        print(f"  threshold {t:.1f}: precision={prec:.1%} recall={rec:.1%}  (tp={tp} fp={fp} fn={fn} tn={tn})")


random.seed(73)

# ---- C: conventional commits ----
subjects = [s.strip() for s in open(os.path.join(HERE, "commits.txt")) if s.strip()]
real = subjects[:200]
def mutate(s):
    m = CC_RE.match(s)
    if not m: return None
    rest = s[m.end() - 1:].strip()
    kind = random.choice(["drop-type", "space-colon", "caps-type", "no-colon", "bad-type"])
    if kind == "drop-type": return rest[0].upper() + rest[1:]
    if kind == "space-colon": return s.replace(": ", " : ", 1)
    if kind == "caps-type": return s[0].upper() + s[1:]
    if kind == "no-colon": return s.replace(": ", " ", 1)
    if kind == "bad-type": return "update" + s[s.index(":"):]
mutants = [m for m in (mutate(s) for s in subjects[200:320]) if m][:60]
q_cc = {"follows": {"type": "noul",
                    "instructions": "The commit message subject line in `state.commit_subject` follows this convention: " + CONVE4,
                    "criteria": {"true": "the subject starts with one of the listed types (optionally with a (scope) and !), immediately followed by a colon and a space, then a description",
                                 "false": "the subject is missing the type prefix, uses a type not in the list, or the type/colon/space shape is wrong"}}}
jobs = [({"id": f"real{i}", "truth": bool(CC_RE.match(s)), "subject": s}, {"commit_subject": s}, q_cc) for i, s in enumerate(real)]
jobs += [({"id": f"mut{i}", "truth": bool(CC_RE.match(s)), "subject": s}, {"commit_subject": s}, q_cc) for i, s in enumerate(mutants)]
t0 = time.time(); res = run(jobs); dt = time.time() - t0
rows = [(m["truth"], r["answers"]["follows"]["noul"]) for m, r in res]
report(f"C. CONVE-4 conventional-commit Noul ({len(res)} calls in {dt:.0f}s)", rows)
bad = sorted(((abs(m["truth"] - r["answers"]["follows"]["noul"]), m["truth"], r["answers"]["follows"]["noul"], m["subject"]) for m, r in res), reverse=True)[:6]
print("  worst disagreements (truth, p, subject):")
for _, t, p, s in bad: print(f"    {t!s:5} {p:.2f} {s[:90]}")

# ---- D: decision detector ----
def load(name):
    items = json.load(open(os.path.join(HERE, name)))
    out = []
    for it in items:
        f = it.get("fields") or {}
        if isinstance(f, str):
            try: f = json.loads(f)
            except Exception: f = {}
        out.append({"ref": it["ref"], "title": it["title"], "body": (it.get("content") or "")[:5000], "status": f.get("status")})
    return out
ht = load("human-tasks.json")
tasks = load("tasks.json"); random.shuffle(tasks); tasks = tasks[:70]
q_dec = {"needs_human_decision": {"type": "noul",
                                  "instructions": "This work item is waiting on a judgment, approval, credential, or decision that only a human owner can supply, rather than on engineering work an AI agent could do unattended.",
                                  "criteria": {"true": "the next step requires a human's decision, approval, sign-off, credentials, money, an external relationship, or hardware the agent cannot reach",
                                               "false": "the next step is implementable by an engineer or AI agent from the description alone"}},
         "blocked": {"type": "noul", "instructions": "The item's text says the work is currently blocked on something outside the item itself.",
                     "criteria": {"true": "an explicit dependency, wait, or blocker is named", "false": "no blocker is stated; the work can start"}}}
jobs = [({"id": it["ref"], "truth": True}, {"title": it["title"], "body": it["body"]}, q_dec) for it in ht]
jobs += [({"id": it["ref"], "truth": False}, {"title": it["title"], "body": it["body"]}, q_dec) for it in tasks]
t0 = time.time(); res = run(jobs); dt = time.time() - t0
rows = [(m["truth"], r["answers"]["needs_human_decision"]["noul"]) for m, r in res]
report(f"D. decision-wearing-a-task-costume Noul ({len(res)} calls in {dt:.0f}s)", rows)
print("  lowest-scoring human-tasks (misses):")
for m, r in sorted(((m, r) for m, r in res if m["truth"]), key=lambda x: x[1]["answers"]["needs_human_decision"]["noul"])[:5]:
    print(f"    {m['id']} p={r['answers']['needs_human_decision']['noul']:.2f}")
print("  highest-scoring tasks (false alarms):")
for m, r in sorted(((m, r) for m, r in res if not m["truth"]), key=lambda x: -x[1]["answers"]["needs_human_decision"]["noul"])[:5]:
    print(f"    {m['id']} p={r['answers']['needs_human_decision']['noul']:.2f}")
json.dump([(m, r) for m, r in res], open(os.path.join(HERE, "results-decision.json"), "w"))

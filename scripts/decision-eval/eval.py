#!/usr/bin/env python3
"""Jev triage eval against docapp's human-labelled items.

Reads TYPESAFE_API_KEY from the environment (never printed). Input: the
bugs/tasks/ideas JSON dumps in this directory. Output: per-item JSONL and a
summary. Two tasks:

  A. field recovery — for items where a human set the field, ask a Choice
     over the schema's options and measure agreement vs confidence.
  B. collection classification — hide the collection, ask bug/task/idea.

Usage: eval.py [--n-per-collection N] [--model jev-latest] [--dry-run]
"""
import argparse, json, os, random, sys, time, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed
from collections import Counter, defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
API = "https://api.typesafe.ai/v1/systemone"
MAX_BODY = 6000  # chars of content; keep state focused (docs: irrelevant state degrades)

COMPONENTS = ["web", "server", "cli", "store", "mcp", "backend", "frontend", "docs", "ci", "other"]

FIELD_QUESTIONS = {
    "bugs": {
        "severity": ("How severe is this bug for users of a self-hosted project tracker used by developers and AI agents?",
                     {"low": "cosmetic, or a workaround is obvious and cheap",
                      "medium": "a real defect a user will hit in normal use; workaround exists or impact is contained",
                      "high": "breaks a core flow, data shown wrong, or no reasonable workaround",
                      "critical": "data loss or corruption, security exposure, or the product is unusable"}),
        "component": ("Which part of the codebase does the fix for this bug primarily live in?",
                      {"web": "the SvelteKit web UI (components, pages, editor)",
                       "server": "the Go HTTP API handlers and server wiring",
                       "cli": "the `pad` command-line tool and its client",
                       "store": "the database layer, migrations, SQL",
                       "mcp": "the MCP tool catalog and dispatch for agents",
                       "backend": "Go code not clearly server, store or mcp",
                       "frontend": "web code described as frontend rather than a specific component",
                       "docs": "documentation, README, instructions text",
                       "ci": "GitHub Actions, build, release tooling",
                       "other": "none of the above"}),
    },
    "tasks": {
        "priority": ("How urgent is this task relative to other work on the same product?",
                     {"low": "nice to have; no one is waiting on it",
                      "medium": "should be done in the normal course of work",
                      "high": "blocks other work, a user, or a release; do soon",
                      "critical": "drop everything; production or data is at risk"}),
        "effort": ("How much implementation effort does this task take for an experienced engineer or agent?",
                   {"xs": "minutes: a one-line or one-file change",
                    "s": "an hour or two: a small contained change with a test",
                    "m": "half a day to a day: several files, tests, some design",
                    "l": "several days: cross-cutting, migrations or new surfaces",
                    "xl": "a week or more: a project, not a task"}),
    },
    "ideas": {
        "impact": ("If built, how much would this idea change the product for its users?",
                   {"low": "marginal polish or a niche convenience",
                    "medium": "a clear improvement many users would notice",
                    "high": "changes what the product can do or who it is for"}),
    },
}

COLLECTION_Q = ("Which kind of work item is this?",
                {"bug": "a defect: something that exists and behaves wrongly",
                 "task": "a unit of work to do, with a defined outcome, not primarily a defect report",
                 "idea": "a proposal or possibility to explore; not yet committed work"})


def load(coll):
    with open(os.path.join(HERE, f"{coll}.json")) as f:
        items = json.load(f)
    out = []
    for it in items:
        fields = it.get("fields") or {}
        if isinstance(fields, str):
            try:
                fields = json.loads(fields)
            except Exception:
                fields = {}
        out.append({"ref": it["ref"], "title": it["title"], "content": (it.get("content") or "")[:MAX_BODY],
                    "fields": fields, "collection": coll})
    return out


def call(key, model, state, questions, retries=5):
    body = json.dumps({"model": model, "state": state, "questions": questions}).encode()
    req = urllib.request.Request(API, data=body, method="POST", headers={
        "Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    delay = 1.0
    for attempt in range(retries):
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                data = json.load(r)
                data["_latency_ms"] = int((time.time() - t0) * 1000)
                return data
        except urllib.error.HTTPError as e:
            if e.code in (429, 529) and attempt < retries - 1:
                time.sleep(delay); delay *= 2; continue
            detail = e.read().decode(errors="replace")[:300]
            raise RuntimeError(f"HTTP {e.code}: {detail}")
    raise RuntimeError("retries exhausted")


def choice_q(instructions, criteria):
    return {"type": "choice", "instructions": instructions, "criteria": criteria}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--n-per-collection", type=int, default=120)
    ap.add_argument("--model", default="jev-latest")
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--seed", type=int, default=73)
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    key = os.environ.get("TYPESAFE_API_KEY")
    if not key and not a.dry_run:
        print("TYPESAFE_API_KEY not in environment", file=sys.stderr); sys.exit(2)

    random.seed(a.seed)
    jobs = []
    for coll, fq in FIELD_QUESTIONS.items():
        items = load(coll)
        labelled = [it for it in items if any(it["fields"].get(k) for k in fq)]
        random.shuffle(labelled)
        for it in labelled[: a.n_per_collection]:
            questions = {k: choice_q(*fq[k]) for k in fq}
            questions["collection"] = choice_q(*COLLECTION_Q)
            state = {"title": it["title"], "body": it["content"]}
            jobs.append((it, questions, state))

    print(f"{len(jobs)} items queued ({', '.join(f'{c}:{sum(1 for j in jobs if j[0]['collection']==c)}' for c in FIELD_QUESTIONS)})")
    if a.dry_run:
        print(json.dumps({"model": a.model, "state": jobs[0][2], "questions": jobs[0][1]}, indent=1)[:1500]); return

    results, errors, usage_in, lat = [], [], 0, []
    out_path = os.path.join(HERE, f"results-{a.model}.jsonl")
    with open(out_path, "w") as out, ThreadPoolExecutor(a.workers) as ex:
        futs = {ex.submit(call, key, a.model, st, q): it for it, q, st in jobs}
        for n, f in enumerate(as_completed(futs), 1):
            it = futs[f]
            try:
                r = f.result()
            except Exception as e:
                errors.append((it["ref"], str(e))); continue
            usage_in += r.get("usage", {}).get("input_tokens", 0); lat.append(r["_latency_ms"])
            rec = {"ref": it["ref"], "collection": it["collection"], "human": it["fields"], "answers": r["answers"],
                   "latency_ms": r["_latency_ms"], "usage": r.get("usage")}
            results.append(rec); out.write(json.dumps(rec) + "\n")
            if n % 50 == 0: print(f"  {n}/{len(jobs)}", file=sys.stderr)

    # ---- summary ----
    print(f"\nmodel={results[0]['answers'] and results and (r.get('model') or a.model)} items={len(results)} errors={len(errors)} "
          f"input_tokens={usage_in} est_cost=${usage_in/1e6*0.042:.4f} "
          f"latency p50={sorted(lat)[len(lat)//2]}ms p90={sorted(lat)[int(len(lat)*0.9)]}ms")
    for e in errors[:5]: print("  error", e)

    print("\nTask B — collection classification (human collection is ground truth)")
    cm = Counter(); conf_ok, conf_bad = [], []
    for rec in results:
        truth = {"bugs": "bug", "tasks": "task", "ideas": "idea"}[rec["collection"]]
        ans = rec["answers"]["collection"]; cm[(truth, ans["choice"])] += 1
        (conf_ok if ans["choice"] == truth else conf_bad).append(ans["confidence"])
    total = sum(cm.values()); correct = sum(v for (t, p), v in cm.items() if t == p)
    print(f"  accuracy {correct}/{total} = {correct/total:.1%}   mean conf when right {sum(conf_ok)/max(1,len(conf_ok)):.2f}, when wrong {sum(conf_bad)/max(1,len(conf_bad)):.2f}")
    for t in ("bug", "task", "idea"):
        row = {p: cm[(t, p)] for p in ("bug", "task", "idea")}
        print(f"  truth={t:5s} -> {row}")

    print("\nTask A — field recovery (only items where a human set the field)")
    for coll, fq in FIELD_QUESTIONS.items():
        for k in fq:
            rows = [(rec["human"].get(k), rec["answers"][k]) for rec in results if rec["collection"] == coll and rec["human"].get(k)]
            if not rows: continue
            exact = sum(1 for h, ans in rows if ans["choice"] == h)
            # adjacent = off by one level on ordered scales
            order = list(fq[k][1].keys())
            adj = sum(1 for h, ans in rows if h in order and ans["choice"] in order and abs(order.index(h) - order.index(ans["choice"])) <= 1)
            hi = [(h, ans) for h, ans in rows if ans["confidence"] >= 0.5]
            hi_exact = sum(1 for h, ans in hi if ans["choice"] == h)
            vhi = [(h, ans) for h, ans in rows if ans["confidence"] >= 0.8]
            vhi_exact = sum(1 for h, ans in vhi if ans["choice"] == h)
            base = Counter(h for h, _ in rows).most_common(1)[0]
            print(f"  {coll}.{k:10s} n={len(rows):3d} exact={exact/len(rows):.1%} adjacent={adj/len(rows):.1%} | "
                  f"conf>=0.5: {len(hi)} items, exact={hi_exact/max(1,len(hi)):.1%} | conf>=0.8: {len(vhi)} items, exact={vhi_exact/max(1,len(vhi)):.1%} | "
                  f"majority-baseline={base[1]/len(rows):.1%} ({base[0]})")
            conf = Counter()
            for h, ans in rows: conf[(h, ans["choice"])] += 1
            top = sorted(((h, p, v) for (h, p), v in conf.items() if h != p), key=lambda x: -x[2])[:3]
            if top: print(f"      top confusions (human->jev): " + ", ".join(f"{h}->{p}:{v}" for h, p, v in top))
    print(f"\nper-item results: {out_path}")


if __name__ == "__main__":
    main()

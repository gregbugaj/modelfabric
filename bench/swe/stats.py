#!/usr/bin/env python3
"""Per-task and aggregate stats from mini-swe-agent trajectories.

Usage: stats.py RUN_DIR [EVAL_REPORT.json]
Call latency is the gap between the previous message's timestamp (the prompt
being ready) and the assistant message's timestamp (the response back).
"""
import glob, json, os, statistics, sys

run = sys.argv[1]
resolved = None
if len(sys.argv) > 2:
    rep = json.load(open(sys.argv[2]))
    resolved = set(rep.get("resolved_ids", []))
    graded = set(rep.get("completed_ids", []))
else:
    graded = set()

def pct(v, p):
    v = sorted(v); return v[max(0, min(len(v)-1, int(round(p/100*len(v)+.5))-1))] if v else None

rows, calls = [], []
for f in sorted(glob.glob(os.path.join(run, "*/*.traj.json"))):
    d = json.load(open(f)); msgs = d["messages"]
    prompt = cached = out = turns = maxp = 0; prev_ts = None; lat = []
    for m in msgs:
        ex = m.get("extra") or {}
        if m.get("role") == "assistant":
            turns += 1
            u = (ex.get("response") or {}).get("usage") or {}
            p = u.get("prompt_tokens", 0); c = ((u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0)
            o = u.get("completion_tokens", 0)
            prompt += p; cached += c; out += o; maxp = max(maxp, p + o)
            if prev_ts and ex.get("timestamp"):
                l = ex["timestamp"] - prev_ts; lat.append(l)
                calls.append({"p": p, "c": c, "o": o, "lat": l})
        if ex.get("timestamp"):
            prev_ts = ex["timestamp"]
    iid = d["instance_id"]
    rows.append({"id": iid, "exit": d["info"].get("exit_status"), "turns": turns,
                 "prompt": prompt, "cached": cached, "out": out,
                 "max_ctx": maxp,
                 "model_s": sum(lat),
                 "resolved": (iid in resolved) if iid in graded else None})

print(f"{'instance':42} {'exit':12} {'turns':>5} {'ctx max':>8} {'cache%':>6} {'out tok':>8} {'model s':>8} res")
for r in rows:
    print(f"{r['id']:42} {str(r['exit'])[:12]:12} {r['turns']:5} {r['max_ctx']:8} "
          f"{100*r['cached']/max(r['prompt'],1):6.1f} {r['out']:8} {r['model_s']:8.0f} "
          f"{'' if r['resolved'] is None else ('✓' if r['resolved'] else '✗')}")
P = sum(r["prompt"] for r in rows); C = sum(r["cached"] for r in rows); O = sum(r["out"] for r in rows)
lats = [c["lat"] for c in calls]
summary = {
    "tasks": len(rows),
    "submitted": sum(1 for r in rows if r["exit"] == "Submitted"),
    "graded": sum(1 for r in rows if r["resolved"] is not None),
    "resolved": sum(1 for r in rows if r["resolved"]),
    "turns_total": sum(r["turns"] for r in rows),
    "turns_mean": round(statistics.mean([r["turns"] for r in rows]), 1) if rows else None,
    "prompt_tokens": P, "cached_tokens": C, "cache_hit_pct": round(100*C/max(P,1), 1),
    "output_tokens": O,
    "call_latency_s_p50": round(pct(lats, 50), 1) if lats else None,
    "call_latency_s_p95": round(pct(lats, 95), 1) if lats else None,
    "prefill_computed_tokens": P - C,
}
print(json.dumps(summary, indent=1))

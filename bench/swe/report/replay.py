#!/usr/bin/env python3
"""Generate measurement data for the router comparison page.

Usage:
  replay.py --arm KEY=LABEL ... --run KEY=RUN_DIR ... [--note KEY=RUN_DIR:TEXT]
            --out site/data/routing.json

Each RUN_DIR must contain calls.jsonl and calls.meta.json from aiperf-calls.py.
Repeated --run values for an arm retain their supplied order. The first arm
is drawn last, above the others. Add runs by saving engine logs, processing
them with aiperf-calls.py and supplying additional --run arguments.
"""
import argparse
import json
import math
import os


def percentile(sorted_values, q):
    return sorted_values[min(int(len(sorted_values) * q), len(sorted_values) - 1)]


def one_run(run_dir):
    with open(os.path.join(run_dir, "calls.jsonl")) as f:
        rows = [json.loads(line) for line in f]
    with open(os.path.join(run_dir, "calls.meta.json")) as f:
        meta = json.load(f)
    rows.sort(key=lambda r: r["sent"])
    latency = sorted(r["end"] - r["sent"] for r in rows)
    nodes, last, moves, after_move = {}, {}, 0, 0
    for r in rows:
        read = r["prompt"] - r["cached"]
        n = nodes.setdefault(r["node"], {"calls": 0, "read": 0, "cached": 0})
        n["calls"] += 1
        n["read"] += read
        n["cached"] += r["cached"]
        if r["task"] in last and last[r["task"]] != r["node"]:
            moves += 1
            after_move += read
        last[r["task"]] = r["node"]
    read = sum(n["read"] for n in nodes.values())
    cached = sum(n["cached"] for n in nodes.values())
    for name, n in nodes.items():
        n["hit"] = round(100 * n["cached"] / max(n["cached"] + n["read"], 1), 1)
        n["dropped"] = meta["dropped"].get(name, 0)
    return {
        "run": os.path.basename(run_dir.rstrip("/")),
        "calls": len(rows),
        "wall_min": round((max(r["end"] for r in rows) - rows[0]["sent"]) / 60, 1),
        "read": read,
        "prompt": read + cached,
        "cache_pct": round(100 * cached / max(read + cached, 1), 1),
        "moves": moves,
        "read_after_move": after_move,
        "dropped": sum(meta["dropped"].values()),
        "p50": round(percentile(latency, 0.50), 1),
        "p90": round(percentile(latency, 0.90), 1),
        "p99": round(percentile(latency, 0.99), 1),
        "max": round(latency[-1], 1),
        "nodes": nodes,
    }, latency


def curve(latency, points=120):
    """Latency read at percentiles from the median to the slowest call, spaced
    evenly in nines, as the page's chart draws them."""
    latency = sorted(latency)
    n = len(latency)
    lo, hi = -math.log10(0.5), -math.log10(1 / n)
    out = []
    for i in range(points + 1):
        t = lo + (hi - lo) * i / points
        pc = 100 * (1 - 10 ** -t)
        out.append([round(pc, 4), round(latency[min(int(n * pc / 100), n - 1)], 1)])
    return out


def pairs(values):
    out = []
    for v in values or []:
        k, _, rest = v.partition("=")
        out.append((k, rest))
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--arm", action="append", required=True, metavar="KEY=LABEL")
    ap.add_argument("--run", action="append", required=True, metavar="KEY=RUN_DIR")
    ap.add_argument("--extra", action="append", metavar="NAME=RUN_DIR",
                    help="a run that belongs to no arm, for a side experiment")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    arms = [{"key": k, "name": label} for k, label in pairs(a.arm)]
    runs, pooled = [], {arm["key"]: [] for arm in arms}
    for key, run_dir in pairs(a.run):
        if key not in pooled:
            ap.error(f"--run {key}=... names an arm no --arm declares")
        summary, latency = one_run(run_dir)
        summary["arm"] = key
        summary["n"] = sum(1 for r in runs if r["arm"] == key) + 1
        runs.append(summary)
        pooled[key] += latency
    extra = {}
    for name, run_dir in pairs(a.extra):
        extra[name], _ = one_run(run_dir)

    data = {
        "arms": arms,
        "runs": runs,
        "pct": {k: curve(v) for k, v in pooled.items() if v},
        "calls": {k: len(v) for k, v in pooled.items()},
        "extra": extra,
    }
    with open(a.out, "w") as f:
        json.dump(data, f, indent=1)
        f.write("\n")
    for r in runs:
        print(f"{r['arm']:8s} {r['run']:18s} {r['wall_min']:5.1f} min  read {r['read']:9,d}  moves {r['moves']:3d}  dropped {r['dropped']:3d}")


if __name__ == "__main__":
    main()

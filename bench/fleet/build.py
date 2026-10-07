#!/usr/bin/env python3
"""Builds the homepage's per-machine speed table from `mfsh bench` reports.

Usage:
  bench/fleet/build.py REPORT.json... [--out site/data/fleet-bench.json]

Pass one single-node report per machine, from one sitting:

  mfsh bench -model qwen/qwen3.8-27b -node xpredator
  mfsh bench -model qwen/qwen3.8-27b -node minion
  mfsh bench -model qwen/qwen3.8-27b -node helion

Reports live in ~/.local/share/modelfabric/bench/. The homepage renders
whatever this writes and nothing else, so the table cannot drift from the
measurements, the same rule bench/swe/report/build.py follows for the SWE page.

It refuses reports that do not belong in one table: a different model or
quantization, a different ModelFabric build, the same machine twice, or a
fleet (cluster) report. The saved reports when this was written mixed a 0.6B
on two machines with a 27B on the third; a table built from those would have
compared nothing.
"""
import argparse, json, re, sys

# The columns the homepage shows. A machine missing one of these sizes shows a
# gap rather than an interpolated number.
SIZES = [1024, 8192, 32768]


def die(msg):
    sys.exit(f"build.py: {msg}")


def gpu_of(machine, runtime):
    # What the report says, never a guess. Linux reports carry the GPU name;
    # a Mac report carries none, but darwin/arm64 is Apple silicon and the
    # runtime says Metal.
    if machine.get("gpu"):
        return machine["gpu"].replace("NVIDIA ", "").replace("GeForce ", "")
    if machine.get("platform") == "darwin/arm64":
        return "Apple silicon (Metal)" if "metal" in runtime else "Apple silicon"
    return "unknown"


def node_row(path, r):
    if "holders" in r:
        die(f"{path} is a fleet report; pass one single-node report per machine")
    machine, engine = r.get("machine") or {}, r.get("engine") or {}
    runtime = engine.get("runtime", "")
    sizes = {}
    for s in r.get("single") or []:
        m = re.match(r"pp(\d+)/", s.get("test", ""))
        if m:
            sizes[int(m.group(1))] = {
                "pp_tps": round(s["pp_tps"]),
                "tg_tps": round(s["tg_tps"], 1),
                "ttft_ms": round(s["ttft_ms"]),
            }
    return {
        "node": r["node"],
        "gpu": gpu_of(machine, runtime),
        "os": machine.get("os", ""),
        "runtime": runtime,
        "sizes": {str(k): sizes[k] for k in SIZES if k in sizes},
        "measured": r.get("at", "")[:10],
        "_model": r.get("model"),
        "_quant": engine.get("quant"),
        "_build": machine.get("modelfabric", "unknown"),
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("reports", nargs="+")
    ap.add_argument("--out", default="site/data/fleet-bench.json")
    a = ap.parse_args()

    rows = [node_row(p, json.load(open(p))) for p in a.reports]
    for key, what in (("_model", "model"), ("_quant", "quantization"), ("_build", "ModelFabric build")):
        seen = {r[key] for r in rows}
        if len(seen) > 1:
            die(f"reports disagree on {what}: {', '.join(sorted(map(str, seen)))}")
    nodes = [r["node"] for r in rows]
    if len(set(nodes)) != len(nodes):
        die(f"a machine appears twice: {', '.join(nodes)}")

    first = rows[0]
    out = {
        "model": first["_model"],
        "quant": first["_quant"],
        "build": first["_build"],
        "measured": max(r["measured"] for r in rows),
        "sizes": SIZES,
        "command": f"mfsh bench -model {first['_model']} -node <machine>",
        # Fastest prefill first: the order a reader compares machines in.
        "nodes": sorted(
            ({k: v for k, v in r.items() if not k.startswith("_")} for r in rows),
            key=lambda n: -max((s["pp_tps"] for s in n["sizes"].values()), default=0),
        ),
    }
    with open(a.out, "w") as f:
        json.dump(out, f, indent=2)
        f.write("\n")
    print(f"wrote {a.out}: {len(rows)} machines, {out['model']} {out['quant']}, build {out['build']}")


if __name__ == "__main__":
    main()

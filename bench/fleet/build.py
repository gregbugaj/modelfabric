#!/usr/bin/env python3
"""Build the homepage's per-machine speed table from mfsh bench reports.

Usage:
  bench/fleet/build.py REPORT.json... [--out site/data/fleet-bench.json]

Supply one single-node report per machine from the same benchmark session.
Reports are stored in ~/.local/share/modelfabric/bench/.
Reject mixed models, quantizations or ModelFabric builds, duplicate machines,
and cluster reports. The homepage renders the generated measurements.
"""
import argparse, json, re, sys

# The columns the homepage shows. A machine missing one of these sizes shows a
# gap rather than an interpolated number.
SIZES = [1024, 8192, 32768]


def die(msg):
    sys.exit(f"build.py: {msg}")


def gpu_of(machine, runtime):
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

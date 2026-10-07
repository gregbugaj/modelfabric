#!/usr/bin/env python3
"""Read, save and check per-engine lifetime token counters.

    counters.py ADDR show                 print the table
    counters.py ADDR save FILE            print it and write FILE and FILE.json
    counters.py ADDR check [MAX]          exit 1 if any engine is above MAX
    counters.py ADDR fingerprint FILE     record serving engines
    counters.py ADDR verify FILE          exit 1 if engine identities changed

ADDR is the control node's management URL. Save counters before reload resets
them, then verify they are near zero so failed reloads cannot carry prior-run
work into the next measurement. Allow startup probes to produce small counts.
Compare engine instance IDs before and after each run to detect restarts,
disappearances or replacements that would change the measured fleet.
"""
import json
import sys
import urllib.request

# Allow startup probes on fresh engines, but reject token counts suggesting prior benchmark traffic.
DEFAULT_MAX = 50_000


def read(addr):
    url = addr.rstrip("/") + "/z/mesh"
    with urllib.request.urlopen(url, timeout=10) as r:
        mesh = json.load(r)
    rows = []
    for node in [mesh.get("self")] + (mesh.get("peers") or []):
        if not node:
            continue
        for inst in node.get("instances") or []:
            rows.append(dict(
                node=node.get("node", "?"), instance=inst.get("id", "?"),
                model=inst.get("model", ""), state=inst.get("state", ""),
                prompt=inst.get("prompt_tokens") or 0,
                cached=inst.get("cached_tokens") or 0,
                output=inst.get("output_tokens") or 0,
                inflight=inst.get("inflight") or 0,
                prefill_tok_s=inst.get("prefill_tok_s") or 0,
                decode_tok_s=inst.get("decode_tok_s") or 0,
            ))
    return sorted(rows, key=lambda r: (r["node"], r["instance"]))


def table(rows):
    total = sum(r["prompt"] for r in rows)
    out = ["  %-12s %12s %6s %13s %9s %12s  %s" %
           ("node", "prefilled", "share", "from cache", "cache hit", "generated", "now")]
    for r in rows:
        p, c = r["prompt"], r["cached"]
        # tokens_predicted_total updates only on completion. Include live inflight
        # counts so engines still generating their first response do not appear idle.
        busy = ""
        if r["inflight"] > 0:
            busy = "%d in flight" % r["inflight"]
            if r["output"] == 0:
                busy += ", first reply not finished"
        out.append("  %-12s %12s %5.1f%% %13s %8.1f%% %12s  %s" % (
            r["node"], f"{p:,}", 100 * p / total if total else 0, f"{c:,}",
            100 * c / (c + p) if (c + p) else 0, f"{r['output']:,}", busy))
    out.append("  %-12s %12s" % ("TOTAL", f"{total:,}"))
    return "\n".join(out)


def main():
    if len(sys.argv) < 3:
        print(__doc__.strip().split("\n\n")[1], file=sys.stderr)
        return 2
    addr, action = sys.argv[1], sys.argv[2]

    try:
        rows = read(addr)
    except Exception as e:
        # Only check mode is a blocking guard; display failures must not abort a run.
        print(f"counters.py: could not read {addr}: {e}", file=sys.stderr)
        return 1 if action == "check" else 0

    if not rows:
        print("counters.py: no engines are loaded anywhere in the mesh", file=sys.stderr)
        return 1 if action == "check" else 0

    if action == "check":
        limit = int(sys.argv[3]) if len(sys.argv) > 3 else DEFAULT_MAX
        over = [r for r in rows if r["prompt"] > limit]
        if over:
            print(f"counters.py: {len(over)} engine(s) still carry a previous run's "
                  f"counters (over {limit:,} prompt tokens):", file=sys.stderr)
            for r in over:
                print(f"  {r['node']:12} {r['prompt']:>12,} prompt tokens "
                      f"({r['instance']})", file=sys.stderr)
            print("  The engine did not restart. Reload it before running, or the "
                  "per-node attribution for this mode is false.", file=sys.stderr)
            return 1
        print(f"✓ engine counters reset ({max(r['prompt'] for r in rows):,} "
              f"prompt tokens at most, limit {limit:,})")
        return 0

    if action in ("fingerprint", "verify"):
        if len(sys.argv) < 4:
            print(f"counters.py: {action} needs a file", file=sys.stderr)
            return 2
        now = sorted(f"{r['node']}/{r['instance']}" for r in rows)
        if action == "fingerprint":
            with open(sys.argv[3], "w") as f:
                f.write("\n".join(now) + "\n")
            print(f"✓ {len(now)} engine(s) serving: " + ", ".join(r["node"] for r in rows))
            return 0
        try:
            with open(sys.argv[3]) as f:
                before = sorted(x for x in f.read().split() if x)
        except OSError as e:
            print(f"counters.py: no fingerprint to verify against: {e}", file=sys.stderr)
            return 1
        if before == now:
            print(f"✓ the same {len(now)} engine(s) served the whole run")
            return 0
        gone, added = sorted(set(before) - set(now)), sorted(set(now) - set(before))
        print("counters.py: the fleet changed while the run was in flight — "
              "this mode measured more than one fleet and is not comparable.", file=sys.stderr)
        for g in gone:
            print(f"  gone:  {g}", file=sys.stderr)
        for a in added:
            print(f"  new:   {a}  (an engine that restarts mid-run also resets its counters)", file=sys.stderr)
        return 1

    text = table(rows)
    print(text)
    if action == "save":
        if len(sys.argv) < 4:
            print("counters.py: save needs a file", file=sys.stderr)
            return 2
        with open(sys.argv[3], "w") as f:
            f.write(text + "\n")
        with open(sys.argv[3] + ".json", "w") as f:
            json.dump(rows, f, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Say which node served each request of an AIPerf replay.

Usage:
  aiperf-calls.py RUN_DIR [OUT.jsonl] [--bodies]

RUN_DIR is a replay written by aiperf-run.sh, with each node's engine log for
that run saved as RUN_DIR/engine-logs/NODE.log.

A response names the engine's build and nothing else. That told the nodes
apart while each ran a different llama.cpp; on 2026-10-06 two of them were put
on the same build, and the summary then counted a conversation crossing
between them as staying put: 26 moves reported where the engine logs show 53.

The engine's own log does say. Every request it serves is logged with the
tokens it read, the time that took to the hundredth of a millisecond, and the
slot's token count when it finished, and the response carries the same three
figures. One line per request is written, in the order they were sent:

  task, turn, sent, end   the conversation and when (unix seconds)
  node                    the node whose log holds this request, "" if none
  prompt, cached, out     tokens: whole prompt, served from cache, answer
  slot, how               the slot it ran in and how the engine chose it:
                          "lcp" for the slot holding its prompt, "lru" otherwise
  body                    the request as sent, with --bodies (for routesim -calls)

and beside it OUT.meta.json with, per node, how many conversations the engine
dropped from its RAM cache to make room.
"""
import json
import os
import re
import sys

SELECTED = re.compile(r"get_availabl: id\s+(\d+) .*selected slot by (LCP similarity|LRU)")
LAUNCHED = re.compile(r"launch_slot_: id\s+(\d+) \| task (\d+)")
READ = re.compile(r"task (\d+) \| prompt eval time =\s+([\d.]+) ms /\s+(\d+) tokens")
RELEASED = re.compile(r"task (\d+) \| stop processing: n_tokens = (\d+)")
DROPPED = re.compile(r"making room for prompt cache entry, removing oldest entry")


def engine_log(path):
    """The requests one engine served, and how many conversations it dropped."""
    tasks, order, dropped, chosen = {}, [], 0, None
    with open(path, errors="replace") as f:
        for line in f:
            if m := SELECTED.search(line):
                chosen = (int(m.group(1)), "lcp" if m.group(2).startswith("LCP") else "lru")
            elif m := LAUNCHED.search(line):
                slot, task = int(m.group(1)), int(m.group(2))
                how = chosen[1] if chosen and chosen[0] == slot else ""
                tasks[task] = {"slot": slot, "how": how}
                order.append(task)
                chosen = None
            elif m := READ.search(line):
                if (t := tasks.get(int(m.group(1)))) is not None:
                    t["read"], t["read_ms"] = int(m.group(3)), float(m.group(2))
            elif m := RELEASED.search(line):
                if (t := tasks.get(int(m.group(1)))) is not None:
                    t["tokens"] = int(m.group(2))
            elif DROPPED.search(line):
                dropped += 1
    served = [tasks[t] for t in order if "read" in tasks[t] and "tokens" in tasks[t]]
    return served, dropped


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    bodies = "--bodies" in sys.argv
    if not args:
        sys.exit(__doc__)
    run = args[0].rstrip("/")
    out = args[1] if len(args) > 1 else os.path.join(run, "calls.jsonl")
    logs = os.path.join(run, "engine-logs")
    if not os.path.isdir(logs):
        sys.exit(f"{logs} is missing: save each node's engine log for this run there as NODE.log")

    by_read, dropped = {}, {}
    for name in sorted(os.listdir(logs)):
        if not name.endswith(".log"):
            continue
        node = name[: -len(".log")]
        served, dropped[node] = engine_log(os.path.join(logs, name))
        for s in served:
            by_read.setdefault(s["read"], []).append((node, s))

    rows, unmatched = [], 0
    with open(os.path.join(run, "profile_export_raw.jsonl")) as f:
        for line in f:
            x = json.loads(line)
            md = x["metadata"]
            try:
                r = json.loads(x["responses"][0]["text"])
                t, u = r["timings"], r["usage"]
            except (KeyError, IndexError, TypeError, ValueError):
                continue  # a failed request: no engine answered it
            read, cached = t.get("prompt_n") or 0, t.get("cache_n") or 0
            answer = u.get("completion_tokens") or 0
            # The slot's count at release is one short of prompt plus answer.
            hits = [
                h for h in by_read.get(read, [])
                if not h[1].get("taken")
                and abs(h[1]["read_ms"] - (t.get("prompt_ms") or 0)) < 0.06
                and h[1]["tokens"] == read + cached + answer - 1
            ]
            node, slot, how = "", None, ""
            if len(hits) == 1:
                node, served = hits[0]
                served["taken"] = True
                slot, how = served["slot"], served["how"]
            else:
                unmatched += 1
            row = {
                "task": md["conversation_id"], "turn": md["turn_index"],
                "sent": md["request_start_ns"] / 1e9, "end": md["request_end_ns"] / 1e9,
                "node": node, "prompt": read + cached, "cached": cached, "out": answer,
                "slot": slot, "how": how,
            }
            if bodies:
                row["body"] = json.dumps(x["payload"], separators=(",", ":"))
            rows.append(row)

    rows.sort(key=lambda r: r["sent"])
    with open(out, "w") as f:
        for row in rows:
            f.write(json.dumps(row) + "\n")
    with open(out[: -len(".jsonl")] + ".meta.json" if out.endswith(".jsonl") else out + ".meta.json", "w") as f:
        json.dump({"run": os.path.basename(run), "calls": len(rows), "unmatched": unmatched, "dropped": dropped}, f, indent=1)
    print(f"{os.path.basename(run)}: {len(rows)} calls, {unmatched} not found in any engine log, dropped {dropped}")
    if unmatched:
        sys.exit(1)


if __name__ == "__main__":
    main()

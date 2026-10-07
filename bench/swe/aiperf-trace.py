#!/usr/bin/env python3
"""Turn a recorded SWE run into a trace NVIDIA AIPerf can replay.

Usage:
  aiperf-trace.py RECORDED_RUN_DIR OUT.jsonl [--model M] [--max-prompt N]
                  [--max-output N] [--tasks N] [--turns N] [--exact-output]

A live agent run is a poor instrument for comparing routers, because the
workload depends on the router: the agent builds each prompt from the model's
last answer, so every run is a different set of conversations. Across four
runs of the same 20 tasks on 2026-10-05 and 06, one task took 33, 77, 83 and
100 calls and whole runs ranged from 749 to 831. A router could not be told
from the draw it was dealt.

A replay sends the recorded requests instead: the same prompts, of the same
sizes, in the same order, for every router. AIPerf already does that
(mooncake-trace, multi-turn sessions: each turn waits for the one before and
then the recorded delay), so this only writes its input. One line per model
call:

  session_id  the task, so a conversation's turns stay in order
  payload     the request exactly as it is sent: the recorded messages, the
              agent's one tool, max_tokens set to the recorded answer's
              length, temperature 0
  delay       milliseconds the agent's command took before this call

With --exact-output each payload also carries ignore_eos, llama.cpp's "do not
stop early", so an answer is the recorded length and not merely no longer.

What a replay cannot say is whether a task was solved; routing does not change
that. And one thing differs from a live run, for every router alike: after a
turn the engine's slot holds the answer it just wrote, the next request
carries the recorded answer, which is different text, so each turn reads the
recorded answer as new prompt.
"""
import argparse
import glob
import json
import os
import sys

# The agent's one tool, so the chat template renders the recorded tool calls
# the way it did live. The recording keeps the calls, not this definition.
BASH_TOOL = [{
    "type": "function",
    "function": {
        "name": "bash",
        "description": "Execute a bash command",
        "parameters": {
            "type": "object",
            "properties": {"command": {"type": "string", "description": "The bash command to execute"}},
            "required": ["command"],
        },
    },
}]


def turns_of(msgs):
    """Each model call of one recorded conversation: what was sent, how long
    the answer was, and how long the agent then took before asking again."""
    out = []
    for i, m in enumerate(msgs):
        extra = m.get("extra") or {}
        usage = (extra.get("response") or {}).get("usage") or {}
        if m.get("role") != "assistant" or not usage.get("prompt_tokens"):
            continue
        hist = []
        for h in msgs[:i]:
            if h.get("role") not in ("system", "user", "assistant", "tool"):
                continue
            e = {"role": h["role"], "content": h.get("content") or ""}
            if h["role"] == "assistant":
                if h.get("tool_calls"):
                    e["tool_calls"] = h["tool_calls"]
                if h.get("reasoning_content"):
                    e["reasoning_content"] = h["reasoning_content"]
            if h["role"] == "tool":
                e["tool_call_id"] = h.get("tool_call_id", "x")
            hist.append(e)
        answered = extra.get("timestamp") or 0
        last = answered
        for n in msgs[i + 1:]:
            if n.get("role") == "assistant":
                break
            last = max(last, (n.get("extra") or {}).get("timestamp") or 0)
        out.append({
            "messages": hist,
            "out": max(int(usage.get("completion_tokens") or 1), 1),
            "prompt": int(usage["prompt_tokens"]),
            "pause": max(last - answered, 0.0),
        })
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("run", help="a recorded run directory holding */*.traj.json")
    ap.add_argument("out", help="the trace file to write")
    ap.add_argument("--model", default="qwen/qwen3.8-27b")
    ap.add_argument("--tasks", type=int, default=0, help="only the first N conversations")
    ap.add_argument("--turns", type=int, default=0, help="only the first N turns of each")
    ap.add_argument("--max-prompt", type=int, default=0,
                    help="end each conversation before prompt plus answer passes this many recorded tokens")
    ap.add_argument("--max-output", type=int, default=0,
                    help="cap every answer at this many tokens, for a quick run")
    ap.add_argument("--exact-output", action="store_true",
                    help="add ignore_eos, so each answer is exactly its recorded length (llama.cpp)")
    args = ap.parse_args()

    paths = sorted(glob.glob(os.path.join(args.run, "*", "*.traj.json")))
    if not paths:
        sys.exit(f"no trajectories under {args.run}")
    if args.tasks:
        paths = paths[:args.tasks]
    calls = sessions = written = 0
    largest = 0
    with open(args.out, "w") as f:
        for path in paths:
            traj = json.load(open(path))
            turns = turns_of(traj["messages"])
            if args.turns:
                turns = turns[:args.turns]
            if args.max_prompt:
                # A conversation longer than the smallest engine's context
                # fails there and succeeds elsewhere, and a failure is fast:
                # the router that sent it to the small engine would finish
                # sooner for having failed. Ending every conversation at a
                # size all engines can hold keeps the script the same work
                # wherever it lands.
                turns = [t for t in turns if t["prompt"] + t["out"] <= args.max_prompt]
            if not turns:
                continue
            sessions += 1
            pause = 0.0
            for t in turns:
                # Writing is most of a run's time and the part routing does
                # not change: 440,000 tokens at 20 to 80 a second made one arm
                # 40 minutes. Capping the answers leaves every prompt, and so
                # everything about where a conversation is cached, as it was,
                # and an arm takes minutes. What a capped run does not show is
                # how a long read slows the engine's other writers, since
                # there is little writing left to slow; use the full trace for
                # numbers that are quoted.
                out = min(t["out"], args.max_output) if args.max_output else t["out"]
                payload = {"model": args.model, "messages": t["messages"], "tools": BASH_TOOL,
                           "max_tokens": out, "temperature": 0, "seed": 1}
                if args.exact_output:
                    payload["ignore_eos"] = True
                row = {"session_id": traj.get("instance_id") or os.path.basename(path), "payload": payload}
                if pause > 0:
                    row["delay"] = round(pause * 1000)
                f.write(json.dumps(row) + "\n")
                pause = t["pause"]
                calls += 1
                written += out
                largest = max(largest, t["prompt"])
    print(f"{args.out}: {sessions} conversations, {calls} calls, {written:,} tokens to write, "
          f"largest prompt {largest:,} tokens")


if __name__ == "__main__":
    main()

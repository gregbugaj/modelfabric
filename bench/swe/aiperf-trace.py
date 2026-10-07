#!/usr/bin/env python3
"""Convert a recorded SWE run into a trace NVIDIA AIPerf can replay.

Usage:
  aiperf-trace.py RECORDED_RUN_DIR OUT.jsonl [--model M] [--max-prompt N]
                  [--max-output N] [--tasks N] [--turns N] [--exact-output]

Replay fixes prompts, order and recorded delays across routers, avoiding
workload differences caused by live agent responses. Each model-call line has:

  session_id  task identifier, preserving conversation order
  payload     recorded messages and tool definition, temperature 0,
              max_tokens set to the recorded response length
  delay       agent execution delay before this call, in milliseconds

--exact-output adds ignore_eos so generation reaches the recorded length.
Replay does not measure task success. Each turn carries the recorded prior
answer, which may differ from the newly generated answer cached by the
engine and therefore requires additional prefill.
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
                # Limit conversations to the smallest engine context so routing to a smaller engine cannot reduce runtime by failing early.
                turns = [t for t in turns if t["prompt"] + t["out"] <= args.max_prompt]
            if not turns:
                continue
            sessions += 1
            pause = 0.0
            for t in turns:
                # Capping answers reduces replay time while retaining prompts and cache state.
                # It also reduces decode/prefill contention; use full traces for reported measurements.
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

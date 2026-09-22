#!/usr/bin/env python3
"""Replay SWE trajectories against one engine, interleaving conversations.

Usage: replay-interleaved.py ENGINE_URL SECONDS RUN_DIR
Four workers interleave different conversations so slots keep switching,
which exercises llama-server's prompt-cache save/restore. Note: this does NOT
reproduce context-checkpoint growth (slots keep resetting); use
replay-sequential.py for that.
"""
import glob, json, random, sys, threading, time, urllib.request

# Checked before anything is sent: a bad SECONDS used to surface as a
# ValueError, and a missing argument as an IndexError, after setup had begun.
if len(sys.argv) < 4:
    sys.exit("Usage: replay-interleaved.py ENGINE_URL SECONDS RUN_DIR")
URL, RUN = sys.argv[1], sys.argv[3]
try:
    SECONDS = float(sys.argv[2])
except ValueError:
    sys.exit(f"SECONDS must be a number, got {sys.argv[2]!r}")
if SECONDS <= 0:
    sys.exit(f"SECONDS must be greater than 0, got {SECONDS}")
convs = []
for f in sorted(glob.glob(RUN + "/*/*.traj.json")):
    msgs = json.load(open(f))["messages"]
    prompts = []
    for i, m in enumerate(msgs):
        if m.get("role") == "assistant" and i > 0:
            hist = []
            for h in msgs[:i]:
                if h["role"] not in ("system", "user", "assistant", "tool"):
                    continue
                e = {"role": h["role"], "content": h.get("content") or ""}
                if h["role"] == "assistant" and h.get("tool_calls"):
                    e["tool_calls"] = h["tool_calls"]
                if h["role"] == "tool":
                    e["tool_call_id"] = h.get("tool_call_id", "x")
                hist.append(e)
            prompts.append(hist)
    convs.append(prompts)
rng = random.Random(1)
end = time.time() + SECONDS
done = [0]

def worker():
    while time.time() < end:
        c = rng.choice(convs)
        # jump into the middle so long contexts come quickly
        for hist in c[len(c) // 2:: max(1, len(c) // 8)]:
            if time.time() >= end:
                return
            body = {"model": "x", "messages": hist, "max_tokens": 8}
            req = urllib.request.Request(URL + "/v1/chat/completions", json.dumps(body).encode(),
                                         {"Content-Type": "application/json"})
            try:
                urllib.request.urlopen(req, timeout=600).read()
                done[0] += 1
            except Exception as e:
                print("err", str(e)[:120], flush=True)

ts = [threading.Thread(target=worker) for _ in range(4)]
[t.start() for t in ts]; [t.join() for t in ts]
print("requests", done[0])

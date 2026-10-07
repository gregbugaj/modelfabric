"""Replay one SWE trajectory sequentially against one engine.

Usage: replay-sequential.py TRAJ_JSON TURNS [ENGINE_URL]
Growing one conversation in a slot reproduces context-checkpoint growth.
Load with -arg -v to log checkpoint creation and size.
"""
import json, sys, urllib.request
# Arguments are checked before the first request goes out: TURNS was parsed
# only inside the loop, so a typo sent a request, waited for generation, and
# then failed with a ValueError.
if len(sys.argv) < 3:
    sys.exit(__doc__.strip().splitlines()[2])
try:
    TURNS = int(sys.argv[2])
except ValueError:
    sys.exit(f"TURNS must be a whole number, got {sys.argv[2]!r}")
if TURNS < 1:
    sys.exit(f"TURNS must be at least 1, got {TURNS}")
URL = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:18000"
msgs = json.load(open(sys.argv[1]))["messages"]
n = 0
for i, m in enumerate(msgs):
    if m.get("role") != "assistant" or i == 0:
        continue
    hist = []
    for h in msgs[:i]:
        e = {"role": h["role"], "content": h.get("content") or ""}
        if h["role"] == "assistant" and h.get("tool_calls"): e["tool_calls"] = h["tool_calls"]
        if h["role"] == "tool": e["tool_call_id"] = h.get("tool_call_id", "x")
        if h["role"] in ("system", "user", "assistant", "tool"): hist.append(e)
    body = {"model": "x", "messages": hist, "max_tokens": 64}
    urllib.request.urlopen(urllib.request.Request(URL + "/v1/chat/completions", json.dumps(body).encode(), {"Content-Type": "application/json"}), timeout=600).read()
    n += 1
    if n >= TURNS: break
print("turns", n)

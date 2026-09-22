#!/bin/bash
# Which hop between the client and the engine fails to stop a generation that
# nobody is waiting for any more?
#
#   bench/hops/abandon.sh [engine|shim|front|public] [close|hold]
#
# A request that outlives its client is not a curiosity: one of them held a
# single-slot engine for ninety minutes, produced about 130,000 tokens nobody
# read, and stopped a benchmark at 22 of 24 with two idle machines beside it.
# The chain it crossed has its own timeouts and buffering at every hop, and
# only one of them has to swallow the cancellation:
#
#   aider -> nginx -> ModelFabric public :1235 -> ModelFabric router
#         -> [llm-d Envoy] -> ModelFabric shim :18001 -> llama-server :18000
#
# So this abandons a request deliberately at one layer at a time and watches
# whether the engine stops. Whichever layer keeps it running is the answer.
#
# Two ways to abandon, because they are not the same failure:
#
#   close  the client closes the socket, as a well-behaved timeout does.
#          Every hop should propagate this, and a hop that does not is a bug
#          with an obvious fix.
#   hold   the client stops reading but leaves the socket open, which is what
#          a library timeout often does. Nothing downstream can see it except
#          as an absence of reads, so this is the case that actually bit.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
MFSH=${MFSH:-./mfsh}
MODEL=${MODEL:-qwen/qwen3.8-27b}
WHERE=${1:-engine}
HOW=${2:-close}
# Long enough that the engine is unambiguously still working when the client
# leaves, and capped so a forgotten run cannot become the thing it studies.
MAX_TOKENS=${MAX_TOKENS:-3000}
WATCH=${WATCH:-45}

# The node whose engine will serve this, and how to watch it. Everything is
# read from the mesh rather than assumed: the port an engine listens on is
# assigned, not fixed.
read -r NODE ENGINE_URL SHIM_URL <<EOF
$(curl -s http://127.0.0.1:1234/z/mesh | python3 -c '
import json, sys
d = json.load(sys.stdin)
for i in d["self"].get("instances", []):
    a = i.get("address") or "127.0.0.1"
    port = i["port"]
    shim = i.get("metrics_port") or port
    print(d["self"]["node"], "http://%s:%d" % (a, port), "http://%s:%d" % (a, shim))
    break
')
EOF

[ -n "${NODE:-}" ] || { echo "no local engine in the mesh; load a model first" >&2; exit 1; }

case "$WHERE" in
  engine)  TARGET="$ENGINE_URL/v1/chat/completions"; KEY="" ;;
  shim)    TARGET="$SHIM_URL/v1/chat/completions"; KEY="" ;;
  front)   TARGET="http://127.0.0.1:1234/v1/chat/completions"; KEY=$("$MFSH" key 2>/dev/null) ;;
  public)  TARGET="${PUBLIC_BASE:?set PUBLIC_BASE, e.g. https://example.com/v1}/chat/completions"
           KEY=${PUBLIC_KEY:?set PUBLIC_KEY} ;;
  *) echo "usage: $0 [engine|shim|front|public] [close|hold]" >&2; exit 1 ;;
esac

# stream:false on purpose. That is what the benchmark's client sends, and it
# matters: with no streaming the client receives nothing at all until the
# answer is complete, so a client timeout always lands mid-generation.
BODY=$(python3 - "$MODEL" "$MAX_TOKENS" <<'PY'
import json,sys
print(json.dumps({
  "model": sys.argv[1],
  "stream": False,
  "max_tokens": int(sys.argv[2]),
  "messages": [{"role": "user", "content":
    "Write the numbers from 1 to 900, one per line, with no commentary."}],
}))
PY
)

echo "abandoning at: $WHERE ($HOW)"
echo "target:        $TARGET"
echo "engine:        $NODE"
echo

engineBusy() { # inflight, decode rate and KV occupancy, live rather than on completion
  curl -s http://127.0.0.1:1234/z/mesh | python3 -c '
import json, sys
d = json.load(sys.stdin)
for i in d["self"].get("instances", []):
    print("inflight=%s decode=%6.1f kv=%.3f" % (i.get("inflight"), i.get("decode_tok_s", 0), i.get("kv_usage", 0)))
    break
'
}

echo "before:   $(engineBusy)"

case "$HOW" in
  close)
    # curl gives up and closes, which is what a timeout ought to look like.
    curl -s -m 6 -o /dev/null -H "Content-Type: application/json" \
      ${KEY:+-H "Authorization: Bearer $KEY"} -d "$BODY" "$TARGET" 2>/dev/null
    echo "client:   closed the socket after 6s"
    ;;
  hold)
    # Sends the request, reads nothing, and keeps the socket open: the case a
    # library timeout produces, and the one no hop can detect directly.
    python3 - "$TARGET" "$KEY" "$BODY" <<'PY' &
import socket, ssl, sys, time
from urllib.parse import urlparse
u = urlparse(sys.argv[1]); key, body = sys.argv[2], sys.argv[3].encode()
port = u.port or (443 if u.scheme == "https" else 80)
s = socket.create_connection((u.hostname, port), timeout=10)
if u.scheme == "https":
    s = ssl.create_default_context().wrap_socket(s, server_hostname=u.hostname)
req = [f"POST {u.path} HTTP/1.1", f"Host: {u.hostname}",
       "Content-Type: application/json", f"Content-Length: {len(body)}",
       "Connection: keep-alive"]
if key:
    req.append(f"Authorization: Bearer {key}")
s.sendall(("\r\n".join(req) + "\r\n\r\n").encode() + body)
# Never read. Hold the socket open until killed, which is the whole point.
time.sleep(3600)
PY
    HOLDER=$!
    trap 'kill $HOLDER 2>/dev/null' EXIT
    sleep 6
    echo "client:   sent the request, reads nothing, socket still open"
    ;;
esac

# Then watch. If the engine is still decoding well after the client is gone,
# this hop did not propagate the abandonment.
for i in $(seq $((WATCH / 5))); do
  sleep 5
  echo "+$((i * 5))s:     $(engineBusy)"
done

echo
echo "Reading this: inflight back to 0 and decode falling to 0 means the hop"
echo "propagated the abandonment. Still decoding means it did not, and the"
echo "engine is generating an answer that no longer has anywhere to go."

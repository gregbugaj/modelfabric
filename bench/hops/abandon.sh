#!/bin/bash
# Check cancellation propagation at each client-to-engine hop.
#
#   bench/hops/abandon.sh [engine|shim|front|public] [close|hold]
#
# Path: client -> nginx -> public API -> router -> optional Envoy -> shim -> engine.
# close closes the socket; each hop should propagate cancellation.
# hold stops reading with the socket open; detecting it requires a progress timeout.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
MFSH=${MFSH:-./mfsh}
MODEL=${MODEL:-qwen/qwen3.8-27b}
WHERE=${1:-engine}
HOW=${2:-close}
# Cap generation while keeping it long enough to remain active when the client disconnects.
MAX_TOKENS=${MAX_TOKENS:-3000}
WATCH=${WATCH:-45}

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

# Use stream:false to match the benchmark: the client receives no text until generation completes.
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
    curl -s -m 6 -o /dev/null -H "Content-Type: application/json" \
      ${KEY:+-H "Authorization: Bearer $KEY"} -d "$BODY" "$TARGET" 2>/dev/null
    echo "client:   closed the socket after 6s"
    ;;
  hold)
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
# Keep the socket open without reading until terminated.
time.sleep(3600)
PY
    HOLDER=$!
    trap 'kill $HOLDER 2>/dev/null' EXIT
    sleep 6
    echo "client:   sent the request, reads nothing, socket still open"
    ;;
esac

# Continued decoding after disconnect indicates cancellation was not propagated.
for i in $(seq $((WATCH / 5))); do
  sleep 5
  echo "+$((i * 5))s:     $(engineBusy)"
done

echo
echo "Reading this: inflight back to 0 and decode falling to 0 means the hop"
echo "propagated the abandonment. Still decoding means it did not, and the"
echo "engine is generating an answer that no longer has anywhere to go."

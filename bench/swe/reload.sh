#!/bin/bash
# Reloads the model on this node and every peer, so a run starts with cold
# caches, then checks each engine really got the context asked for.
set -euo pipefail
source "$(dirname "$0")/env.sh"
# Engines are loaded on the machines that have GPUs, never on the entrypoint.
# sequence.sh exports MFSH_ADDR so llm-d control reaches the node requests
# arrive at; inheriting it here would send `mfsh load` to a node whose role is
# entrypoint and which holds no models at all.
unset MFSH_ADDR
"$MFSH" unload --all >/dev/null || true
for peer in $PEERS; do
  # Quote MODEL and each LOAD_ARGS word for the remote shell. Preserve tilde
  # expansion in the trusted PEER_MFSH path; quoting it would make ~ literal.
  peer_args=$(loadArgsFor "$peer")
  remote_cmd=$(printf '%s unload --all >/dev/null; %s load %q' "$PEER_MFSH" "$PEER_MFSH" "$MODEL")
  for arg in $peer_args; do remote_cmd+=$(printf ' %q' "$arg"); done
  remote_cmd+=$(printf " >/dev/null && echo %q loaded" "$peer")
  echo "  $peer: $peer_args"
  # Allow 600 seconds for cold model loading, including slower storage and empty page caches.
  timeout 600 tailscale ssh "$peer" "$remote_cmd" \
    || { echo "$peer did not reload (Tailscale SSH may need a browser approval)" >&2; exit 1; }
done
local_args=$(loadArgsFor "$("$MFSH" status --json 2>/dev/null | python3 -c '
import json, sys
try: print((json.load(sys.stdin).get("self") or {}).get("node") or "")
except Exception: print("")
' 2>/dev/null)")
echo "  this node: $local_args"
"$MFSH" load $MODEL $local_args >/dev/null && echo "local loaded"
"$MFSH" endpoints | "$BENCH_DIR/check-engines.py"
